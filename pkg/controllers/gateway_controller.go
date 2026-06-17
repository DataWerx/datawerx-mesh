package controllers

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	networkingv1alpha1 "github.com/DataWerx/datawerx-mesh/pkg/apis/networking/v1alpha1"
	"github.com/DataWerx/datawerx-mesh/pkg/gateway"
	"github.com/DataWerx/datawerx-mesh/pkg/topology"
)

// gatewayProfileManagedBy marks the access-profile ConfigMap as authored by the
// gateway role, mirroring the label the topology syncer stamps on MeshPeers.
const gatewayProfileManagedBy = "datawerx-remote-gateway"

// GatewayDataPlane installs the remote-access masquerade so client-sourced
// traffic forwarded into the mesh returns via the gateway. Depending on the
// interface, not the concrete *nat.Manager, keeps the reconciler unit-testable.
// It mirrors the other data-plane seams in this package.
type GatewayDataPlane interface {
	SyncGatewayMasq(clientCIDRs, destCIDRs []string) error
}

// GatewayReconciler runs on the remote-access gateway. On every MeshPeer change
// it recomputes the full set of mesh destinations a remote client can reach,
// (1) programs the masquerade for client→mesh traffic and (2) republishes the
// access-profile ConfigMap that a thin client consumes to configure its routes
// and DNS.
//
// It performs a full-state sync (declarative, drift-proof): correctness comes
// from reconciling against the complete current MeshPeer set rather than a
// single delta, so the request is ignored. This keeps the gateway additive and
// on-strateg. DataWerx rides the client's existing overlay and only programs
// the multi-cluster service layer on top.
type GatewayReconciler struct {
	client.Client
	Scheme    *runtime.Scheme
	DataPlane GatewayDataPlane

	// ClientCIDRs are the overlay source ranges remote clients connect from
	// (e.g. the Tailscale/CGNAT or corporate-VPN range). Used as the masquerade
	// source scope; required for the gateway to do anything.
	ClientCIDRs []string
	// GatewayEndpoints are the overlay-reachable addresses advertised to clients.
	GatewayEndpoints []string
	// ClusterSetCIDRs are the ClusterSetIP VIP ranges - IPv4 and optionally IPv6.
	ClusterSetCIDRs []string
	// LocalCIDRs are this cluster's own pod/service ranges - reachable directly.
	LocalCIDRs []string
	// DNS describes how clients reach clusterset.local resolution.
	DNS gateway.DNSConfig
	// ProfileNamespace is where the access-profile ConfigMap is published;
	// defaults to gateway.DefaultProfileNamespace when empty.
	ProfileNamespace string
	// NoNAT, when true, disables the client masquerade so pods see the remote
	// client's real source IP per-client NetworkPolicy works. It requires the
	// premium return-route component on every node to steer client-bound replies
	// back to this gateway; without it, return traffic black-holes. Default false
	// MASQUERADE — works with no extra components.
	NoNAT bool
}

// +kubebuilder:rbac:groups=networking.datawerx.io,resources=meshpeers,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update;patch

// Reconcile recomputes the reachable mesh, applies the masquerade, and publishes
// the access profile. The request is ignored; correctness comes from the
// complete current object set.
func (r *GatewayReconciler) Reconcile(ctx context.Context, _ ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var peers networkingv1alpha1.MeshPeerList
	if err := r.List(ctx, &peers); err != nil {
		return ctrl.Result{}, fmt.Errorf("listing MeshPeers: %w", err)
	}

	// The set a client can reach: this cluster's own ranges plus every peer's
	// directly-routed, non-overlapping remote ranges. Overlapping ranges are
	// served via the remap NETMAP and deliberately excluded here, exactly as the
	// masquerade-exemption reconciler excludes them.
	mesh := r.reachableMesh(peers.Items)

	// Masquerade destinations include the ClusterSetIP ranges too: a client
	// hitting a VIP is DNAT'd to a backend in `mesh`, but including the VIP range
	// is harmless and future-proofs direct-VIP cases.
	dest := dedupeUnion(r.ClusterSetCIDRs, mesh)
	// In no-NAT mode we program no client masquerade, preserving the client's
	// real source IP into the cluster; return routing is handled by the premium
	// per-node component instead.
	masqClients := r.ClientCIDRs
	if r.NoNAT {
		masqClients = nil
	}
	if err := r.DataPlane.SyncGatewayMasq(masqClients, dest); err != nil {
		return ctrl.Result{}, fmt.Errorf("syncing gateway masquerade: %w", err)
	}

	if err := r.publishProfile(ctx, mesh); err != nil {
		return ctrl.Result{}, err
	}

	logger.V(1).Info("remote-access gateway reconciled",
		"clientCIDRs", len(r.ClientCIDRs), "destCIDRs", len(dest), "meshCIDRs", len(mesh))
	return ctrl.Result{}, nil
}

// reachableMesh returns the de-duplicated union of this cluster's local CIDRs
// and every programmable peer's directly-routed remote CIDRs.
func (r *GatewayReconciler) reachableMesh(peers []networkingv1alpha1.MeshPeer) []string {
	seen := map[string]struct{}{}
	var out []string
	add := func(cidrs []string) {
		for _, c := range cidrs {
			if c == "" {
				continue
			}
			if _, ok := seen[c]; ok {
				continue
			}
			seen[c] = struct{}{}
			out = append(out, c)
		}
	}

	add(r.LocalCIDRs)
	for i := range peers {
		p := &peers[i]
		if p.Spec.PublicKey == "" {
			continue // not programmable, so not reachable over the mesh
		}
		routable, _ := topology.Partition(p.Spec.AllCIDRs(), r.LocalCIDRs)
		add(routable)
	}
	return out
}

// publishProfile upserts the access-profile ConfigMap so a thin client can
// configure itself. The profile's slices are sorted, so an unchanged topology
// produces byte-identical data and CreateOrUpdate is a no-op.
func (r *GatewayReconciler) publishProfile(ctx context.Context, mesh []string) error {
	profile := gateway.BuildAccessProfile(r.GatewayEndpoints, r.ClusterSetCIDRs, mesh, r.DNS)
	data, err := gateway.ProfileConfigMapData(profile)
	if err != nil {
		return err
	}

	cm := &corev1.ConfigMap{}
	cm.Name = gateway.ProfileConfigMapName
	cm.Namespace = r.profileNamespace()
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, cm, func() error {
		if cm.Labels == nil {
			cm.Labels = map[string]string{}
		}
		cm.Labels["app.kubernetes.io/managed-by"] = gatewayProfileManagedBy
		cm.Data = data
		return nil
	}); err != nil {
		return fmt.Errorf("publishing access-profile ConfigMap %s/%s: %w", cm.Namespace, cm.Name, err)
	}
	return nil
}

func (r *GatewayReconciler) profileNamespace() string {
	if r.ProfileNamespace != "" {
		return r.ProfileNamespace
	}
	return gateway.DefaultProfileNamespace
}

// dedupeUnion returns the de-duplicated union of two CIDR slices, preserving
// no particular order - the data plane re-sorts per family. Empty entries are
// dropped.
func dedupeUnion(a, b []string) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, s := range append(append([]string(nil), a...), b...) {
		if s == "" {
			continue
		}
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}

// SetupWithManager registers the reconciler. Any MeshPeer change re-triggers the
// full-state gateway sync.
func (r *GatewayReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&networkingv1alpha1.MeshPeer{}).
		Named("remote-gateway").
		Complete(r)
}
