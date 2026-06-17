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

	mcsv1alpha1 "github.com/datawerx/datawerx/pkg/apis/multicluster/v1alpha1"
	networkingv1alpha1 "github.com/datawerx/datawerx/pkg/apis/networking/v1alpha1"
	dwxmetrics "github.com/datawerx/datawerx/pkg/metrics"
	"github.com/datawerx/datawerx/pkg/nat"
)

// ServiceNATDataPlane programs the ClusterSetIP DNAT/load-balancing rules. The
// reconciler depends on this interface so it is unit-testable with a fake which
// mirrors the WireGuard PeerDataPlane split.
type ServiceNATDataPlane interface {
	SyncClusterSetNAT(services []nat.ServiceDNAT) error
}

// ServiceNATReconciler keeps the ClusterSetIP DNAT data plane in sync with the
// imported services. It performs a full-state sync  any ServiceImport or
// EndpointExport change recomputes the entire desired rule set from current
// cluster state and hands it to the data plane.
type ServiceNATReconciler struct {
	client.Client
	Scheme    *runtime.Scheme
	DataPlane ServiceNATDataPlane
}

// +kubebuilder:rbac:groups=multicluster.x-k8s.io,resources=serviceimports,verbs=get;list;watch
// +kubebuilder:rbac:groups=networking.datawerx.io,resources=endpointexports,verbs=get;list;watch

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

	services := BuildServiceDNAT(imports.Items, exports.Items)
	if err := r.DataPlane.SyncClusterSetNAT(services); err != nil {
		dwxmetrics.NATSyncs.WithLabelValues("error").Inc()
		return ctrl.Result{}, fmt.Errorf("syncing ClusterSetIP NAT: %w", err)
	}
	dwxmetrics.NATSyncs.WithLabelValues("success").Inc()
	logger.V(1).Info("clusterset NAT reconciled", "services", len(services))
	return ctrl.Result{}, nil
}

// BuildServiceDNAT is the pure mapping from cluster state to the NAT data
// plane's input: each ClusterSetIP ServiceImport with an allocated VIP
// becomes a ServiceDNAT whose backends are the reachable service IPs published
// by every exporting cluster via ClusterSetIP EndpointExports. It is exported
// so it can be unit-tested directly.
func BuildServiceDNAT(imports []mcsv1alpha1.ServiceImport, exports []networkingv1alpha1.EndpointExport) []nat.ServiceDNAT {
	backends := map[string][]string{}
	for i := range exports {
		e := &exports[i]
		if e.Spec.Type != mcsv1alpha1.ClusterSetIP {
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

// SetupWithManager wires the reconciler. It reconciles on ServiceImport changes
// and on EndpointExport changes.  It's mapped to a single sentinel request since the
// sync is global, so backend churn re-programs the DNAT rules.
func (r *ServiceNATReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&mcsv1alpha1.ServiceImport{}).
		Watches(&networkingv1alpha1.EndpointExport{}, handler.EnqueueRequestsFromMapFunc(natSyncRequest)).
		Named("servicenat").
		Complete(r)
}

// natSyncRequest collapses any EndpointExport event to a single reconcile
// request, because the data-plane sync is full-state.
func natSyncRequest(_ context.Context, _ client.Object) []reconcile.Request {
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: "clusterset-nat-sync"}}}
}
