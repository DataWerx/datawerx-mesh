package controllers

import (
	"context"
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	networkingv1alpha1 "github.com/datawerx/datawerx/pkg/apis/networking/v1alpha1"
	"github.com/datawerx/datawerx/pkg/meshfw"
)

// FirewallDataPlane programs the compiled mesh firewall. Depending on the
// interface, (not the concrete *meshfw.Manager, keeps the reconciler unit
// testable without root/iptables.
type FirewallDataPlane interface {
	SyncFirewall(rs meshfw.Ruleset) error
}

// MeshNetworkPolicyReconciler compiles the full set of MeshNetworkPolicies into
// a single firewall ruleset and programs it on the WireGuard ingress path. It
// is a full-state, declarative reconciler: any policy or MeshPeer change
// recomputes the whole ruleset (the union of all policies), so the data plane
// can never drift from the declared intent.
//
// Source cluster IDs in policies are resolved to CIDRs from the live MeshPeer
// set, so a policy can name "cluster-b" without the author knowing its ranges.
type MeshNetworkPolicyReconciler struct {
	client.Client
	Scheme    *runtime.Scheme
	DataPlane FirewallDataPlane
}

// +kubebuilder:rbac:groups=networking.datawerx.io,resources=meshnetworkpolicies,verbs=get;list;watch
// +kubebuilder:rbac:groups=networking.datawerx.io,resources=meshnetworkpolicies/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=networking.datawerx.io,resources=meshpeers,verbs=get;list;watch

// Reconcile recompiles and applies the full mesh firewall. The request object
// is used only to patch that policy's status; correctness of the data plane
// comes from the complete current policy + peer set.
func (r *MeshNetworkPolicyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var policies networkingv1alpha1.MeshNetworkPolicyList
	if err := r.List(ctx, &policies); err != nil {
		return ctrl.Result{}, fmt.Errorf("listing MeshNetworkPolicies: %w", err)
	}
	var peers networkingv1alpha1.MeshPeerList
	if err := r.List(ctx, &peers); err != nil {
		return ctrl.Result{}, fmt.Errorf("listing MeshPeers: %w", err)
	}

	clusterCIDRs := map[string][]string{}
	for i := range peers.Items {
		p := &peers.Items[i]
		if p.Spec.ClusterID == "" {
			continue
		}
		clusterCIDRs[p.Spec.ClusterID] = append(clusterCIDRs[p.Spec.ClusterID], p.Spec.AllCIDRs()...)
	}

	rs := meshfw.BuildFirewall(toPolicies(policies.Items), clusterCIDRs)

	if err := r.DataPlane.SyncFirewall(rs); err != nil {
		r.patchAll(ctx, policies.Items, networkingv1alpha1.MeshNetworkPolicyPhaseError, "programming firewall: "+err.Error())
		return ctrl.Result{}, fmt.Errorf("syncing mesh firewall: %w", err)
	}

	msg := ""
	if len(rs.Skipped) > 0 {
		msg = "skipped non-IPv4 inputs (IPv4 data plane): " + strings.Join(rs.Skipped, ", ")
	}
	r.patchAll(ctx, policies.Items, networkingv1alpha1.MeshNetworkPolicyPhaseReady, msg)

	logger.V(1).Info("mesh firewall reconciled", "policies", len(policies.Items), "rules", len(rs.Rules), "skipped", len(rs.Skipped))
	return ctrl.Result{}, nil
}

// patchAll updates the status of every policy to the given phase/message,
// patching only those that actually changed to avoid reconcile churn.
func (r *MeshNetworkPolicyReconciler) patchAll(ctx context.Context, items []networkingv1alpha1.MeshNetworkPolicy, phase networkingv1alpha1.MeshNetworkPolicyPhase, msg string) {
	logger := log.FromContext(ctx)
	for i := range items {
		p := &items[i]
		if p.Status.Phase == phase && p.Status.Message == msg && p.Status.ObservedGeneration == p.Generation {
			continue
		}
		base := p.DeepCopy()
		p.Status.Phase = phase
		p.Status.Message = msg
		p.Status.ObservedGeneration = p.Generation
		if err := r.Status().Patch(ctx, p, client.MergeFrom(base)); err != nil {
			logger.Error(err, "patching MeshNetworkPolicy status", "name", p.Name)
		}
	}
}

// toPolicies projects the CRD wire format into the pure planner inputs.
func toPolicies(items []networkingv1alpha1.MeshNetworkPolicy) []meshfw.Policy {
	out := make([]meshfw.Policy, 0, len(items))
	for i := range items {
		p := &items[i]
		pol := meshfw.Policy{Name: p.Name, Destinations: p.Spec.Destinations}
		for _, ing := range p.Spec.Ingress {
			rule := meshfw.IngressRule{}
			for _, sel := range ing.From {
				rule.From = append(rule.From, meshfw.PeerSelector{ClusterIDs: sel.ClusterIDs, CIDRs: sel.CIDRs})
			}
			for _, port := range ing.Ports {
				rule.Ports = append(rule.Ports, meshfw.Port{Protocol: port.Protocol, Port: port.Port})
			}
			pol.Ingress = append(pol.Ingress, rule)
		}
		out = append(out, pol)
	}
	return out
}

// SetupWithManager registers the reconciler. Both MeshNetworkPolicy and MeshPeer
// changes re-trigger the full-state firewall compile. Peers resolve source
// CIDRs.
func (r *MeshNetworkPolicyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&networkingv1alpha1.MeshNetworkPolicy{}).
		Watches(&networkingv1alpha1.MeshPeer{}, enqueueAllPolicies(mgr.GetClient())).
		Named("meshnetworkpolicy").
		Complete(r)
}

// enqueueAllPolicies maps any MeshPeer event to reconcile requests for every
// MeshNetworkPolicy, since a peer change can alter the CIDRs a ClusterID
// selector resolves to. The full-state reconcile makes a single request
// sufficient, but enqueuing all keeps each policy's status fresh.
func enqueueAllPolicies(c client.Client) handler.EventHandler {
	return handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, _ client.Object) []reconcile.Request {
		var list networkingv1alpha1.MeshNetworkPolicyList
		if err := c.List(ctx, &list); err != nil {
			return nil
		}
		reqs := make([]reconcile.Request, 0, len(list.Items))
		for i := range list.Items {
			reqs = append(reqs, reconcile.Request{NamespacedName: types.NamespacedName{Name: list.Items[i].Name}})
		}
		return reqs
	})
}
