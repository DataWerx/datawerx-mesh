package controllers

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	networkingv1alpha1 "github.com/datawerx/datawerx/pkg/apis/networking/v1alpha1"
	"github.com/datawerx/datawerx/pkg/topology"
)

// MasqExemptDataPlane installs the masquerade-exemption rules so cross-cluster
// pod traffic egressing the mesh keeps its real source. Depending on the
// interface (not the concrete *nat.Manager) keeps the reconciler unit-testable.
type MasqExemptDataPlane interface {
	SyncMeshNoMasq(local, remote []string) error
}

// MasqExemptReconciler keeps the node's masquerade-exemption rules in sync with
// the set of remote mesh CIDRs we route. Without it, the node's own masquerade
// rules, the CNI's or kind-masq-agent in CI, rewrite the source of traffic
// leaving the mesh device toward a remote cluster. Because the mesh device has
// no address, the masqueraded source then falls outside the peer's WireGuard
// AllowedIPs and the far end drops it. Exempting local→remote traffic preserves
// the real pod source end to end.
//
// It performs a full-state sync making it declarative and drift-proof. Any MeshPeer
// change recomputes the union of directly-routed remote CIDRs and hands it, with this
// cluster's local CIDRs, to the data plane. Overlapping remapped ranges are
// deliberately excluded.  In overlap mode the source is presented via the NETMAP
// in the remap chain instead, so exempting it here would pre-empt that.
type MasqExemptReconciler struct {
	client.Client
	Scheme    *runtime.Scheme
	DataPlane MasqExemptDataPlane

	LocalCIDRs []string
}

// +kubebuilder:rbac:groups=networking.datawerx.io,resources=meshpeers,verbs=get;list;watch

// Reconcile recomputes and applies the full masquerade-exemption set. The
// request is ignored; correctness comes from the complete current object set.
func (r *MasqExemptReconciler) Reconcile(ctx context.Context, _ ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var peers networkingv1alpha1.MeshPeerList
	if err := r.List(ctx, &peers); err != nil {
		return ctrl.Result{}, fmt.Errorf("listing MeshPeers: %w", err)
	}

	remote := r.routedRemoteCIDRs(peers.Items)
	if err := r.DataPlane.SyncMeshNoMasq(r.LocalCIDRs, remote); err != nil {
		return ctrl.Result{}, fmt.Errorf("syncing masquerade exemption: %w", err)
	}
	logger.V(1).Info("masquerade exemption reconciled", "localCIDRs", len(r.LocalCIDRs), "remoteCIDRs", len(remote))
	return ctrl.Result{}, nil
}

// routedRemoteCIDRs returns the de-duplicated union, across every programmable
// peer, of the remote CIDRs we route the non-overlapping ones directly. These
// are exactly the destinations whose return path the host would otherwise
// masquerade.
func (r *MasqExemptReconciler) routedRemoteCIDRs(peers []networkingv1alpha1.MeshPeer) []string {
	seen := map[string]struct{}{}
	var out []string
	for i := range peers {
		p := &peers[i]
		if p.Spec.PublicKey == "" {
			continue
		}
		routable, _ := topology.Partition(p.Spec.AllCIDRs(), r.LocalCIDRs)
		for _, c := range routable {
			if _, ok := seen[c]; ok {
				continue
			}
			seen[c] = struct{}{}
			out = append(out, c)
		}
	}
	return out
}

// SetupWithManager registers the reconciler. Any MeshPeer change re-triggers the
// full-state masquerade-exemption sync.
func (r *MasqExemptReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&networkingv1alpha1.MeshPeer{}).
		Named("masqexempt").
		Complete(r)
}
