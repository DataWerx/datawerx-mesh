package controllers

import (
	"context"
	"fmt"
	"strings"

	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	mcsv1alpha1 "github.com/datawerx/datawerx/pkg/apis/multicluster/v1alpha1"
	networkingv1alpha1 "github.com/datawerx/datawerx/pkg/apis/networking/v1alpha1"
	"github.com/datawerx/datawerx/pkg/dns"
)

// DefaultClusterSetCIDR is the IPv4 range from which virtual ClusterSetIPs are
// allocated. 241.0.0.0/8 sits in the reserved Class E space, so it will not
// collide with real cluster pod/service ranges.
const DefaultClusterSetCIDR = "241.0.0.0/8"

// ServiceImportReconciler builds cluster-local ServiceImport objects from the
// EndpointExports published by every cluster in the mesh. It is the import half
// of the MCS pipeline and is tier-agnostic: it only reads EndpointExport
// objects, regardless of whether those were written by the local export
// controller (free/open tier), mirrored by GitOps, or materialized by the premium SaaS
// syncer.
//
// All cross-cluster aggregation and the consistent ClusterSetIP allocation are
// delegated to the pure functions in pkg/dns, so the reconciler stays a thin
// shell (the repo-wide design rule).
type ServiceImportReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// ClusterSetCIDR is the IPv4 range virtual ClusterSetIPs are drawn from.
	// Defaults to DefaultClusterSetCIDR when empty.
	ClusterSetCIDR string
	// ClusterSetCIDR6, when set, is the IPv6 range from which a second
	// ClusterSetIP is allocated for services that have IPv6 backends (dual
	// stack). Empty disables IPv6 ClusterSetIP allocation.
	ClusterSetCIDR6 string
}

// +kubebuilder:rbac:groups=networking.datawerx.io,resources=endpointexports,verbs=get;list;watch
// +kubebuilder:rbac:groups=multicluster.x-k8s.io,resources=serviceimports,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=multicluster.x-k8s.io,resources=serviceimports/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=discovery.k8s.io,resources=endpointslices,verbs=get;list;watch;create;update;patch;delete

// Reconcile rebuilds the ServiceImport for a single service identity
// from all EndpointExports that target it.
func (r *ServiceImportReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("serviceimport", req.NamespacedName)
	key := dns.ServiceKey{Namespace: req.Namespace, Name: req.Name}

	// Read every EndpointExport in the cluster (local + mirrored/materialized).
	var exportList networkingv1alpha1.EndpointExportList
	if err := r.List(ctx, &exportList); err != nil {
		return ctrl.Result{}, fmt.Errorf("listing EndpointExports: %w", err)
	}

	exports := make([]dns.ExportedService, 0, len(exportList.Items))
	for i := range exportList.Items {
		exports = append(exports, endpointExportToExported(&exportList.Items[i]))
	}

	grouped, keys := dns.GroupExports(exports)

	// No cluster exports this service any more: ensure the ServiceImport and any
	// mirrored EndpointSlices are gone.
	endpoints, ok := grouped[key]
	if !ok || len(endpoints) == 0 {
		if err := r.reconcileMirrorSlices(ctx, key, nil); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, r.deleteServiceImport(ctx, key)
	}

	plan := dns.PlanServiceImport(endpoints)
	if plan.HasConflicts() {
		// Conflicts are surfaced on ServiceExport status in the exporting
		// cluster; here we proceed with the resolved plan and record the issue.
		logger.Info("service import has cross-cluster conflicts", "conflicts", plan.Conflicts)
	}

	vips, err := r.allocateVIPs(key, plan, keys, grouped)
	if err != nil {
		return ctrl.Result{}, err
	}

	if err := r.applyServiceImport(ctx, key, plan, vips); err != nil {
		return ctrl.Result{}, err
	}

	// Mirror the cross-cluster endpoints into the consuming cluster's
	// discovery.k8s.io API for a headless import (KEP-1645). A ClusterSetIP
	// import is realized via the virtual IP and node-local DNAT instead, so it
	// carries no mirrored slices.
	var desired []dns.ExportedEndpoint
	if plan.Type == mcsv1alpha1.Headless {
		desired = endpoints
	}
	return ctrl.Result{}, r.reconcileMirrorSlices(ctx, key, desired)
}

// allocateVIPs derives the virtual ClusterSetIP(s) for key. Allocation is a pure
// function of the full key set, so every cluster derives the same IP(s). A
// dual-stack service gets a v4 VIP and (when a v6 pool is set) a v6 VIP;
// non-ClusterSetIP services get none.
func (r *ServiceImportReconciler) allocateVIPs(
	key dns.ServiceKey,
	plan dns.ImportPlan,
	keys []dns.ServiceKey,
	grouped map[dns.ServiceKey][]dns.ExportedEndpoint,
) ([]string, error) {
	if plan.Type != mcsv1alpha1.ClusterSetIP {
		return nil, nil
	}

	var vips []string
	v4, err := dns.AllocateClusterSetIPs(r.cidr(), keys)
	if err != nil {
		return nil, fmt.Errorf("allocating IPv4 ClusterSetIP: %w", err)
	}
	if ip := v4[key]; ip != "" {
		vips = append(vips, ip)
	}

	if r.ClusterSetCIDR6 == "" {
		return vips, nil
	}
	v6Keys := keysWithIPv6Backends(grouped)
	if _, ok := v6Keys[key]; !ok {
		return vips, nil
	}
	v6, err := dns.AllocateClusterSetIPs(r.ClusterSetCIDR6, sortedKeySet(v6Keys))
	if err != nil {
		return nil, fmt.Errorf("allocating IPv6 ClusterSetIP: %w", err)
	}
	if ip := v6[key]; ip != "" {
		vips = append(vips, ip)
	}
	return vips, nil
}

// keysWithIPv6Backends returns the set of service keys that have at least one
// IPv6 backend, so an IPv6 ClusterSetIP is allocated only for those.
func keysWithIPv6Backends(grouped map[dns.ServiceKey][]dns.ExportedEndpoint) map[dns.ServiceKey]struct{} {
	out := map[dns.ServiceKey]struct{}{}
	for k, eps := range grouped {
		for _, ep := range eps {
			for _, ip := range ep.IPs {
				if strings.Contains(ip, ":") {
					out[k] = struct{}{}
				}
			}
		}
	}
	return out
}

func sortedKeySet(m map[dns.ServiceKey]struct{}) []dns.ServiceKey {
	out := make([]dns.ServiceKey, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return dns.SortServiceKeys(out)
}

// applyServiceImport creates or updates the ServiceImport for key to match the
// computed plan, then reconciles its status.
func (r *ServiceImportReconciler) applyServiceImport(
	ctx context.Context,
	key dns.ServiceKey,
	plan dns.ImportPlan,
	clusterSetIPs []string,
) error {
	si := &mcsv1alpha1.ServiceImport{}
	si.Namespace = key.Namespace
	si.Name = key.Name

	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, si, func() error {
		si.Spec.Type = plan.Type
		si.Spec.Ports = plan.Ports
		if plan.Type == mcsv1alpha1.ClusterSetIP && len(clusterSetIPs) > 0 {
			si.Spec.IPs = clusterSetIPs
		} else {
			si.Spec.IPs = nil
		}
		return nil
	}); err != nil {
		return fmt.Errorf("applying ServiceImport %s: %w", key, err)
	}

	// Status carries the contributing clusters. It lives on the status
	// subresource, so it is written separately from the spec above.
	desired := make([]mcsv1alpha1.ClusterStatus, 0, len(plan.Clusters))
	for _, c := range plan.Clusters {
		desired = append(desired, mcsv1alpha1.ClusterStatus{Cluster: c})
	}
	if clusterStatusEqual(si.Status.Clusters, desired) {
		return nil
	}
	base := si.DeepCopy()
	si.Status.Clusters = desired
	if err := r.Status().Patch(ctx, si, client.MergeFrom(base)); err != nil {
		return fmt.Errorf("updating ServiceImport %s status: %w", key, err)
	}
	return nil
}

// deleteServiceImport removes the ServiceImport for key if it exists.
func (r *ServiceImportReconciler) deleteServiceImport(ctx context.Context, key dns.ServiceKey) error {
	si := &mcsv1alpha1.ServiceImport{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: key.Namespace, Name: key.Name}, si); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("getting ServiceImport %s for deletion: %w", key, err)
	}
	if err := r.Delete(ctx, si); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("deleting ServiceImport %s: %w", key, err)
	}
	return nil
}

// reconcileMirrorSlices converges the mirrored EndpointSlices for key toward the
// desired set computed from endpoints (nil for a non-headless or withdrawn
// import, which prunes all of them). It owns only the slices it labels as
// DataWerx-mirrored, so it never disturbs a cluster's native EndpointSlices.
func (r *ServiceImportReconciler) reconcileMirrorSlices(ctx context.Context, key dns.ServiceKey, endpoints []dns.ExportedEndpoint) error {
	var desired []discoveryv1.EndpointSlice
	if len(endpoints) > 0 {
		desired = dns.PlanEndpointSlices(key.Name, key.Namespace, endpoints)
	}

	var existing discoveryv1.EndpointSliceList
	if err := r.List(ctx, &existing,
		client.InNamespace(key.Namespace),
		client.MatchingLabels{
			mcsv1alpha1.LabelServiceName: key.Name,
			discoveryv1.LabelManagedBy:   dns.MirrorManagedBy,
		},
	); err != nil {
		return fmt.Errorf("listing mirrored EndpointSlices for %s: %w", key, err)
	}

	keep := make(map[string]bool, len(desired))
	for i := range desired {
		want := &desired[i]
		keep[want.Name] = true
		if err := r.applyMirrorSlice(ctx, want); err != nil {
			return err
		}
	}

	for i := range existing.Items {
		slice := &existing.Items[i]
		if keep[slice.Name] {
			continue
		}
		if err := r.Delete(ctx, slice); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("pruning mirrored EndpointSlice %s/%s: %w", slice.Namespace, slice.Name, err)
		}
	}
	return nil
}

// applyMirrorSlice creates or updates one mirrored slice to match want. The
// address type is immutable, but the deterministic name encodes the family, so a
// given name always carries the same type and the update never conflicts.
func (r *ServiceImportReconciler) applyMirrorSlice(ctx context.Context, want *discoveryv1.EndpointSlice) error {
	slice := &discoveryv1.EndpointSlice{}
	slice.Namespace = want.Namespace
	slice.Name = want.Name
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, slice, func() error {
		slice.Labels = want.Labels
		slice.AddressType = want.AddressType
		slice.Endpoints = want.Endpoints
		slice.Ports = want.Ports
		return nil
	}); err != nil {
		return fmt.Errorf("applying mirrored EndpointSlice %s/%s: %w", want.Namespace, want.Name, err)
	}
	return nil
}

func (r *ServiceImportReconciler) cidr() string {
	if r.ClusterSetCIDR == "" {
		return DefaultClusterSetCIDR
	}
	return r.ClusterSetCIDR
}

// SetupWithManager registers the controller. It owns ServiceImport objects and
// watches EndpointExports, mapping each export event to the service identity it
// contributes to.
func (r *ServiceImportReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&mcsv1alpha1.ServiceImport{}).
		Watches(&networkingv1alpha1.EndpointExport{}, handler.EnqueueRequestsFromMapFunc(endpointExportToServiceRequest)).
		Named("serviceimport").
		Complete(r)
}

// endpointExportToServiceRequest maps an EndpointExport event to the reconcile
// request for the service it contributes to. The deleted object is still
// delivered here, so withdrawal of the last export triggers cleanup.
func endpointExportToServiceRequest(_ context.Context, obj client.Object) []reconcile.Request {
	ee, ok := obj.(*networkingv1alpha1.EndpointExport)
	if !ok {
		return nil
	}
	return []reconcile.Request{{
		NamespacedName: types.NamespacedName{
			Namespace: ee.Spec.ServiceNamespace,
			Name:      ee.Spec.ServiceName,
		},
	}}
}

// endpointExportToExported converts the CRD wire format into the pure transport
// type consumed by pkg/dns.
func endpointExportToExported(ee *networkingv1alpha1.EndpointExport) dns.ExportedService {
	return dns.ExportedService{
		Namespace: ee.Spec.ServiceNamespace,
		Name:      ee.Spec.ServiceName,
		Endpoint: dns.ExportedEndpoint{
			Cluster:    ee.Spec.ClusterID,
			Type:       ee.Spec.Type,
			Ports:      ee.Spec.Ports,
			IPs:        ee.Spec.IPs,
			ExportTime: ee.Spec.ExportedAtUnix,
		},
	}
}

func clusterStatusEqual(a, b []mcsv1alpha1.ClusterStatus) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Cluster != b[i].Cluster {
			return false
		}
	}
	return true
}
