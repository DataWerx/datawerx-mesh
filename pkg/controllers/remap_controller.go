package controllers

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	networkingv1alpha1 "github.com/DataWerx/datawerx-mesh/pkg/apis/networking/v1alpha1"
	dwxmetrics "github.com/DataWerx/datawerx-mesh/pkg/metrics"
	"github.com/DataWerx/datawerx-mesh/pkg/nat"
	"github.com/DataWerx/datawerx-mesh/pkg/topology"
)

// RemapDataPlane programs the overlapping-CIDR NETMAP rules. Depending on the
// interface, not the concrete *nat.Manager, keeps the reconciler unit-testable.
type RemapDataPlane interface {
	SyncRemap(entries []nat.RemapEntry) error
}

// RemapReconciler keeps the local NETMAP rules in sync with the set of
// overlapping ranges across all MeshPeers. It performs a full-state sync.
// Any MeshPeer change recomputes the union of local
// real⇄virtual pairs and hands it to the data plane.
//
// It is only registered when overlap remap is enabled i.e. a remap pool is set.
type RemapReconciler struct {
	client.Client
	Scheme    *runtime.Scheme
	DataPlane RemapDataPlane

	ClusterID  string
	RemapPool  string
	LocalCIDRs []string
}

// +kubebuilder:rbac:groups=networking.datawerx.io,resources=meshpeers,verbs=get;list;watch

// Reconcile recomputes and applies the full set of local NETMAP entries. The
// request is ignored.  Correctness comes from the complete current object set.
func (r *RemapReconciler) Reconcile(ctx context.Context, _ ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var peers networkingv1alpha1.MeshPeerList
	if err := r.List(ctx, &peers); err != nil {
		return ctrl.Result{}, fmt.Errorf("listing MeshPeers: %w", err)
	}

	entries, err := r.buildEntries(peers.Items)
	if err != nil {
		return ctrl.Result{}, err
	}
	if err := r.DataPlane.SyncRemap(entries); err != nil {
		dwxmetrics.RemapSyncs.WithLabelValues("error").Inc()
		return ctrl.Result{}, fmt.Errorf("syncing overlap NETMAP: %w", err)
	}
	dwxmetrics.RemapSyncs.WithLabelValues("success").Inc()
	dwxmetrics.RemapEntries.Set(float64(len(entries)))
	logger.V(1).Info("overlap remap reconciled", "entries", len(entries))
	return ctrl.Result{}, nil
}

// buildEntries computes the de-duplicated union of local real⇄virtual pairs
// across every peer - a local range that overlaps any peer needs its NETMAP.
//
// PlanRemap guarantees a collision-free virtual allocation WITHIN a single
// peer's plan, but VirtualCIDR is a per-CIDR hash into a finite pool, so two
// distinct local reals that overlap DIFFERENT peers — computed in separate
// PlanRemap calls — can still hash to the same virtual range. That would make
// the inbound NETMAP (-d Virtual -> Real) ambiguous. This is the only place with
// the global view, so we detect such cross-peer collisions here and fail safe
// rather than program a corrupt mapping.
func (r *RemapReconciler) buildEntries(peers []networkingv1alpha1.MeshPeer) ([]nat.RemapEntry, error) {
	seen := map[nat.RemapEntry]struct{}{}

	// owner records, globally across all peers, which distinct source claimed each
	// virtual range. A virtual claimed by two different sources is a collision:
	//   - local⇄local: two local reals -> one virtual makes the inbound NETMAP
	//     (-d Virtual -> Real) ambiguous;
	//   - route⇄route: two peers route the same virtual into the mesh device,
	//     so the host route / AllowedIP for it can only belong to one of them;
	//   - local⇄route: a PREROUTING NETMAP for a local virtual would shadow a
	//     routed peer virtual of the same value.
	// VirtualCIDR is collision-free only within a single PlanRemap call, so these
	// cross-call collisions can only be caught here, with the full peer set in
	// view. We fail safe (error -> retry, nothing programmed) rather than install
	// a corrupt mapping.
	owner := map[string]string{}
	claim := func(virtual, source string) error {
		if prev, ok := owner[virtual]; ok && prev != source {
			return fmt.Errorf("overlap remap collision in pool %s: %s and %s both map to virtual %s; "+
				"enlarge DataWerx_REMAP_CIDR or remove the overlapping ranges", r.RemapPool, prev, source, virtual)
		}
		owner[virtual] = source
		return nil
	}

	var out []nat.RemapEntry
	for i := range peers {
		p := &peers[i]
		if p.Spec.PublicKey == "" {
			continue
		}
		_, conflicts := topology.Partition(p.Spec.AllCIDRs(), r.LocalCIDRs)
		// Dangerous ranges (default route, loopback, etc.) are withheld by
		// Partition but must never be NAT-remapped either — drop them so they are
		// purely rejected, not translated into the mesh.
		conflicts = excludeDangerous(conflicts)
		if len(conflicts) == 0 {
			continue
		}
		rp, err := topology.PlanRemap(r.RemapPool, r.ClusterID, r.LocalCIDRs, p.Spec.ClusterID, conflicts)
		if err != nil {
			return nil, fmt.Errorf("planning remap for peer %s: %w", p.Name, err)
		}
		// The virtual ranges this peer's traffic is routed under (one source per
		// peer: the peer owns all of its routed virtuals).
		for _, v := range rp.RouteVirtual {
			if err := claim(v, "peer "+p.Spec.ClusterID+" route"); err != nil {
				return nil, err
			}
		}
		// The local real⇄virtual NETMAP pairs this peer's overlap requires.
		for _, lr := range rp.Locals {
			if err := claim(lr.Virtual, "local "+lr.Real); err != nil {
				return nil, err
			}
			e := nat.RemapEntry{Real: lr.Real, Virtual: lr.Virtual}
			if _, ok := seen[e]; ok {
				continue
			}
			seen[e] = struct{}{}
			out = append(out, e)
		}
	}
	return out, nil
}

// excludeDangerous drops dangerous prefixes (default route, loopback, etc.) from
// a CIDR set so they are never NAT-remapped — only rejected.
func excludeDangerous(cidrs []string) []string {
	dangerous := map[string]struct{}{}
	for _, c := range topology.DangerousCIDRs(cidrs) {
		dangerous[c] = struct{}{}
	}
	var out []string
	for _, c := range cidrs {
		if _, bad := dangerous[c]; !bad {
			out = append(out, c)
		}
	}
	return out
}

// SetupWithManager registers the reconciler. Any MeshPeer change re-triggers the
// full-state NETMAP sync.
func (r *RemapReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&networkingv1alpha1.MeshPeer{}).
		Named("remap").
		Complete(r)
}
