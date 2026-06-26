package controllers

import (
	"context"
	"fmt"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	mcsv1alpha1 "github.com/DataWerx/datawerx-mesh/pkg/apis/multicluster/v1alpha1"
	networkingv1alpha1 "github.com/DataWerx/datawerx-mesh/pkg/apis/networking/v1alpha1"
	dwxmetrics "github.com/DataWerx/datawerx-mesh/pkg/metrics"
	"github.com/DataWerx/datawerx-mesh/pkg/nat"
)

// ServiceNATDataPlane programs the ClusterSetIP DNAT/load-balancing rules. The
// reconciler depends on this interface so it is unit-testable with a fake which
// mirrors the WireGuard PeerDataPlane split.
type ServiceNATDataPlane interface {
	SyncClusterSetNAT(services []nat.ServiceDNAT) error
}

// DefaultFailoverStaleSeconds is how long since a peer's last liveness signal
// (a successful probe, or a WireGuard handshake) before its exported backends
// are dropped from the load balancer so traffic fails over to the survivors. It
// matches verify.StaleHandshakeSeconds, kept as a local constant so the
// data-plane reconciler doesn't depend on the read-surface package.
const DefaultFailoverStaleSeconds int64 = 300

// failoverPollInterval is how often the reconciler re-evaluates peer liveness
// when health-gated failover is enabled. Staleness is time-based, but a peer
// that goes silent stops emitting watch events, so a steady requeue is what
// actually lets a dead cluster's backends be evicted.
const failoverPollInterval = 30 * time.Second

// ServiceNATReconciler keeps the ClusterSetIP DNAT data plane in sync with the
// imported services. It performs a full-state sync. Any ServiceImport or
// EndpointExport change recomputes the entire desired rule set from current
// cluster state and hands it to the data plane.
type ServiceNATReconciler struct {
	client.Client
	Scheme    *runtime.Scheme
	DataPlane ServiceNATDataPlane

	// FailoverStaleSeconds, when > 0, enables health-gated load-balancing:
	// backends exported by a meshed cluster whose tunnel is observably down
	// (see clusterDown) are dropped from the DNAT set, so traffic fails over to
	// the remaining exporters. 0 (the default) keeps every exporter's backends —
	// liveness is reported but not enforced. Enabled by DataWerx_LB_FAILOVER.
	FailoverStaleSeconds int64

	// Now returns the current Unix epoch seconds; injectable for tests. When nil
	// it defaults to time.Now().Unix().
	Now func() int64
}

// now returns the current Unix time in seconds, honoring the injectable clock.
func (r *ServiceNATReconciler) now() int64 {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now().Unix()
}

// +kubebuilder:rbac:groups=multicluster.x-k8s.io,resources=serviceimports,verbs=get;list;watch
// +kubebuilder:rbac:groups=networking.datawerx.io,resources=endpointexports,verbs=get;list;watch
// +kubebuilder:rbac:groups=networking.datawerx.io,resources=meshpeers,verbs=get;list;watch

// Reconcile recomputes and applies the full ClusterSetIP DNAT state. The
// request is intentionally ignored — correctness comes from reconciling against
// the complete current object set rather than a single delta.
func (r *ServiceNATReconciler) Reconcile(ctx context.Context, _ ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var imports mcsv1alpha1.ServiceImportList
	if err := r.List(ctx, &imports); err != nil {
		return ctrl.Result{}, fmt.Errorf("listing ServiceImports: %w", err)
	}
	var exports networkingv1alpha1.EndpointExportList
	if err := r.List(ctx, &exports); err != nil {
		return ctrl.Result{}, fmt.Errorf("listing EndpointExports: %w", err)
	}

	// Health-gated failover: when enabled, drop backends exported by clusters
	// whose tunnel is observably down so the VIP load-balances over the
	// survivors. Disabled (down == nil) leaves every exporter in the set.
	var down map[string]bool
	if r.FailoverStaleSeconds > 0 {
		var peers networkingv1alpha1.MeshPeerList
		if err := r.List(ctx, &peers); err != nil {
			return ctrl.Result{}, fmt.Errorf("listing MeshPeers: %w", err)
		}
		down = DownClusters(peers.Items, r.now(), r.FailoverStaleSeconds)
	}

	services := BuildServiceDNAT(imports.Items, exports.Items, down)
	if err := r.DataPlane.SyncClusterSetNAT(services); err != nil {
		dwxmetrics.NATSyncs.WithLabelValues("error").Inc()
		return ctrl.Result{}, fmt.Errorf("syncing ClusterSetIP NAT: %w", err)
	}
	dwxmetrics.NATSyncs.WithLabelValues("success").Inc()
	logger.V(1).Info("clusterset NAT reconciled", "services", len(services), "downClusters", len(down))

	// When failover is on, re-evaluate on a steady cadence: a cluster that goes
	// silent stops producing watch events, so only a requeue can detect that its
	// liveness has aged past the threshold.
	if r.FailoverStaleSeconds > 0 {
		return ctrl.Result{RequeueAfter: failoverPollInterval}, nil
	}
	return ctrl.Result{}, nil
}

// BuildServiceDNAT is the pure mapping from cluster state to the NAT data
// plane's input: each ClusterSetIP ServiceImport with an allocated VIP
// becomes a ServiceDNAT whose backends are the reachable service IPs published
// by every exporting cluster via ClusterSetIP EndpointExports. It is exported
// so it can be unit-tested directly.
//
// downClusters, when non-empty, names exporting clusters whose tunnel is
// observably down (health-gated failover); their backends are dropped so the
// VIP load-balances over the survivors. A service all of whose exporters are
// down ends up with no backends and is skipped — its VIP programs no DNAT,
// which fails the connection fast rather than black-holing it on a dead peer. A
// nil/empty map keeps every exporter (the default, failover disabled).
func BuildServiceDNAT(imports []mcsv1alpha1.ServiceImport, exports []networkingv1alpha1.EndpointExport, downClusters map[string]bool) []nat.ServiceDNAT {
	backends := map[string][]string{}
	for i := range exports {
		e := &exports[i]
		if e.Spec.Type != mcsv1alpha1.ClusterSetIP {
			continue
		}
		if downClusters[e.Spec.ClusterID] {
			continue
		}
		key := e.Spec.ServiceNamespace + "/" + e.Spec.ServiceName
		backends[key] = append(backends[key], e.Spec.IPs...)
	}

	var out []nat.ServiceDNAT
	for i := range imports {
		si := &imports[i]
		if si.Spec.Type != mcsv1alpha1.ClusterSetIP || len(si.Spec.IPs) == 0 {
			continue
		}
		key := si.Namespace + "/" + si.Name
		b := backends[key]
		if len(b) == 0 {
			continue
		}
		ports := make([]nat.PortDNAT, 0, len(si.Spec.Ports))
		for _, p := range si.Spec.Ports {
			ports = append(ports, nat.PortDNAT{Protocol: strings.ToLower(string(p.Protocol)), Port: p.Port})
		}
		out = append(out, nat.ServiceDNAT{
			Namespace: si.Namespace,
			Name:      si.Name,
			VIP:       si.Spec.IPs[0],
			Ports:     ports,
			Backends:  b,
		})
	}
	return out
}

// DownClusters returns the set of cluster IDs whose peers are observably down,
// per clusterDown, keyed by Spec.ClusterID. now/staleAfter are Unix seconds. A
// nil result (no peers, or none down) means "drop nothing" — failover fails
// open. The local cluster has no MeshPeer of its own, so its exports are never
// in this set and are always kept.
func DownClusters(peers []networkingv1alpha1.MeshPeer, now, staleAfter int64) map[string]bool {
	var down map[string]bool
	for i := range peers {
		p := &peers[i]
		if p.Spec.ClusterID == "" {
			continue
		}
		if clusterDown(&p.Status, now, staleAfter) {
			if down == nil {
				down = map[string]bool{}
			}
			down[p.Spec.ClusterID] = true
		}
	}
	return down
}

// clusterDown reports whether a meshed peer's tunnel is observably not passing
// traffic, so its exported backends should be evicted from the load balancer.
// It mirrors the liveness preference the read surfaces use (pkg/verify, pkg/slo):
//
//   - Phase=Error: the agent could not program the peer — down.
//   - When the active prober has recently run for this peer, the probe is
//     authoritative: down unless a probe succeeded within staleAfter.
//   - Otherwise fall back to the handshake: down only once a handshake was seen
//     and has since aged past staleAfter.
//
// A peer that has neither handshaked nor been probed (still coming up) is left
// alone — we fail open rather than black-hole a peer that may be converging.
func clusterDown(st *networkingv1alpha1.MeshPeerStatus, now, staleAfter int64) bool {
	if st.Phase == networkingv1alpha1.MeshPeerPhaseError {
		return true
	}
	// Active prober is the authoritative signal when it has recently run.
	if now > 0 && st.LastProbeAttempt > 0 && now-st.LastProbeAttempt <= staleAfter {
		return !(st.LastProbeTime > 0 && now-st.LastProbeTime <= staleAfter)
	}
	// Fall back to the WireGuard handshake; never-handshaked peers are not down.
	if st.LastHandshakeTime > 0 {
		return now-st.LastHandshakeTime > staleAfter
	}
	return false
}

// SetupWithManager wires the reconciler. It reconciles on ServiceImport changes
// and on EndpointExport changes. It's mapped to a single sentinel request since
// the sync is global, so backend churn re-programs the DNAT rules. With
// health-gated failover on, MeshPeer changes are watched too so a peer's
// liveness transition promptly re-evaluates the backend set.
func (r *ServiceNATReconciler) SetupWithManager(mgr ctrl.Manager) error {
	b := ctrl.NewControllerManagedBy(mgr).
		For(&mcsv1alpha1.ServiceImport{}).
		Watches(&networkingv1alpha1.EndpointExport{}, handler.EnqueueRequestsFromMapFunc(natSyncRequest))
	if r.FailoverStaleSeconds > 0 {
		b = b.Watches(&networkingv1alpha1.MeshPeer{}, handler.EnqueueRequestsFromMapFunc(natSyncRequest))
	}
	return b.Named("servicenat").Complete(r)
}

// natSyncRequest collapses any EndpointExport event to a single reconcile
// request, because the data-plane sync is full-state.
func natSyncRequest(_ context.Context, _ client.Object) []reconcile.Request {
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: "clusterset-nat-sync"}}}
}
