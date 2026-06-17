package controllers

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
	"github.com/datawerx/datawerx/pkg/topology"
)

// Condition reasons reported on a ServiceExport's Valid condition.
const (
	// reasonExported indicates the referenced Service exists and the export is
	// accepted.
	reasonExported = "Exported"
	// reasonServiceNotFound indicates no Service matches the ServiceExport's
	// name/namespace, so there is nothing to export.
	reasonServiceNotFound = "ServiceNotFound"
)

// serviceExportFinalizer guarantees the controller can withdraw the published
// EndpointExport before the ServiceExport disappears. We use a finalizer rather
// than owner references because EndpointExports are mirrored to other clusters
// where an owner reference would dangle and trigger erroneous GC.
const serviceExportFinalizer = "networking.datawerx.io/serviceexport-cleanup"

// ServiceExportReconciler validates ServiceExport objects against their
// referenced Service and reports readiness via the Valid status condition.
//
// Per the MCS API, a ServiceExport has no spec. It refers to the Service with
// the same name and namespace. This controller validates that reference,
// surfaces the Valid condition, and publishes the cluster's contribution as an
// EndpointExport (the broker-less wire format). EndpointExports are mirrored
// between clusters (GitOps in the free tier, the SaaS syncer in premium) and
// consumed by the ServiceImport controller — which therefore stays entirely
// tier-agnostic.
type ServiceExportReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// ClusterID identifies this cluster in the mesh; it stamps the exported
	// endpoint so remote clusters can attribute and de-duplicate contributions.
	ClusterID string
}

// +kubebuilder:rbac:groups=multicluster.x-k8s.io,resources=serviceexports,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=multicluster.x-k8s.io,resources=serviceexports/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=multicluster.x-k8s.io,resources=serviceexports/finalizers,verbs=update
// +kubebuilder:rbac:groups=networking.datawerx.io,resources=endpointexports,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch
// +kubebuilder:rbac:groups=discovery.k8s.io,resources=endpointslices,verbs=get;list;watch

// Reconcile validates a ServiceExport, publishes/withdraws its EndpointExport,
// and updates its Valid condition.
func (r *ServiceExportReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var export mcsv1alpha1.ServiceExport
	if err := r.Get(ctx, req.NamespacedName, &export); err != nil {
		if apierrors.IsNotFound(err) {
			// The export is gone; the import side reacts to its disappearance.
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("fetching ServiceExport: %w", err)
	}

	// Finalizer-driven withdrawal: while the object is being deleted, remove the
	// published EndpointExport before letting the ServiceExport go.
	if !export.GetDeletionTimestamp().IsZero() {
		return r.reconcileDeletion(ctx, &export)
	}

	// Register the finalizer before publishing anything.
	if !controllerutil.ContainsFinalizer(&export, serviceExportFinalizer) {
		controllerutil.AddFinalizer(&export, serviceExportFinalizer)
		if err := r.Update(ctx, &export); err != nil {
			return ctrl.Result{}, fmt.Errorf("adding finalizer: %w", err)
		}
		return ctrl.Result{}, nil
	}

	return r.reconcileExport(ctx, req, &export)
}

// reconcileDeletion withdraws this cluster's EndpointExport and then drops the
// finalizer, allowing the ServiceExport to be removed.
func (r *ServiceExportReconciler) reconcileDeletion(ctx context.Context, export *mcsv1alpha1.ServiceExport) (ctrl.Result, error) {
	if controllerutil.ContainsFinalizer(export, serviceExportFinalizer) {
		if err := r.withdrawEndpointExport(ctx, export.Namespace, export.Name); err != nil {
			return ctrl.Result{}, err
		}
		controllerutil.RemoveFinalizer(export, serviceExportFinalizer)
		if err := r.Update(ctx, export); err != nil {
			return ctrl.Result{}, fmt.Errorf("removing finalizer: %w", err)
		}
	}
	return ctrl.Result{}, nil
}

// reconcileExport validates the referenced Service and publishes the EndpointExport, or when the
// Service is gone, withdraws.. Then updates the Valid condition.
func (r *ServiceExportReconciler) reconcileExport(ctx context.Context, req ctrl.Request, export *mcsv1alpha1.ServiceExport) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("serviceexport", req.NamespacedName)

	// The referenced Service shares the export's name and namespace.
	var svc corev1.Service
	switch err := r.Get(ctx, req.NamespacedName, &svc); {
	case apierrors.IsNotFound(err):
		// Nothing to export: withdraw any stale publication and report invalid.
		if err := r.withdrawEndpointExport(ctx, export.Namespace, export.Name); err != nil {
			return ctrl.Result{}, err
		}
		return r.setValidCondition(ctx, export, false, reasonServiceNotFound,
			"no Service with this name exists in the namespace")
	case err != nil:
		return ctrl.Result{}, fmt.Errorf("fetching referenced Service: %w", err)
	}

	endpoint := dns.BuildExportedEndpoint(r.ClusterID, &svc)
	// Headless services carry no ClusterIP; their cross-cluster addresses are
	// the backing pod IPs, gathered from the local EndpointSlices so remote
	// clusters can resolve the name to real endpoints.
	if dns.ServiceIsHeadless(&svc) {
		ips, err := r.headlessEndpointIPs(ctx, &svc)
		if err != nil {
			return ctrl.Result{}, err
		}
		endpoint.IPs = ips
	}
	// The ServiceExport's creation time is the MCS "oldest export wins" key,
	// carried on the EndpointExport so every importing cluster resolves
	// type/port conflicts identically.
	if err := r.publishEndpointExport(ctx, &svc, endpoint, export.CreationTimestamp.Unix()); err != nil {
		return ctrl.Result{}, err
	}
	logger.V(1).Info("service exported", "type", endpoint.Type, "ports", len(endpoint.Ports), "endpoints", len(endpoint.IPs))

	msg := fmt.Sprintf("exported as %s with %d port(s)", endpoint.Type, len(endpoint.Ports))
	return r.setValidCondition(ctx, export, true, reasonExported, msg)
}

// publishEndpointExport creates or updates this cluster's EndpointExport for the
// given Service so other clusters can import it.
func (r *ServiceExportReconciler) publishEndpointExport(ctx context.Context, svc *corev1.Service, endpoint dns.ExportedEndpoint, exportedAtUnix int64) error {
	ee := &networkingv1alpha1.EndpointExport{}
	ee.Namespace = svc.Namespace
	ee.Name = endpointExportName(r.ClusterID, svc.Name)

	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, ee, func() error {
		ee.Spec.ClusterID = r.ClusterID
		ee.Spec.ServiceNamespace = svc.Namespace
		ee.Spec.ServiceName = svc.Name
		ee.Spec.Type = endpoint.Type
		ee.Spec.ExportedAtUnix = exportedAtUnix
		ee.Spec.Ports = endpoint.Ports
		ee.Spec.IPs = endpoint.IPs
		return nil
	}); err != nil {
		return fmt.Errorf("publishing EndpointExport for %s/%s: %w", svc.Namespace, svc.Name, err)
	}
	return nil
}

// withdrawEndpointExport deletes this cluster's EndpointExport for the named
// service if it exists.
func (r *ServiceExportReconciler) withdrawEndpointExport(ctx context.Context, namespace, serviceName string) error {
	ee := &networkingv1alpha1.EndpointExport{}
	ee.Namespace = namespace
	ee.Name = endpointExportName(r.ClusterID, serviceName)
	if err := r.Delete(ctx, ee); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("withdrawing EndpointExport for %s/%s: %w", namespace, serviceName, err)
	}
	return nil
}

// headlessEndpointIPs lists the local EndpointSlices backing the service and
// returns the addresses eligible to serve traffic.
func (r *ServiceExportReconciler) headlessEndpointIPs(ctx context.Context, svc *corev1.Service) ([]string, error) {
	var slices discoveryv1.EndpointSliceList
	if err := r.List(ctx, &slices,
		client.InNamespace(svc.Namespace),
		client.MatchingLabels{discoveryv1.LabelServiceName: svc.Name},
	); err != nil {
		return nil, fmt.Errorf("listing EndpointSlices for %s/%s: %w", svc.Namespace, svc.Name, err)
	}
	return dns.ReadyEndpointIPs(slices.Items), nil
}

// endpointExportName is the deterministic name for a cluster's EndpointExport of
// a service: "<cluster>-<service>", so multiple clusters' exports of the same
// service coexist after mirroring.
func endpointExportName(clusterID, serviceName string) string {
	prefix := topology.SanitizeName(clusterID)
	return prefix + "-" + serviceName
}

// setValidCondition writes the Valid condition only when it actually changes,
// avoiding needless status writes and reconcile churn.
func (r *ServiceExportReconciler) setValidCondition(
	ctx context.Context,
	export *mcsv1alpha1.ServiceExport,
	ok bool,
	reason, message string,
) (ctrl.Result, error) {
	status := metav1.ConditionTrue
	if !ok {
		status = metav1.ConditionFalse
	}

	desired := metav1.Condition{
		Type:               mcsv1alpha1.ServiceExportValid,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: export.Generation,
	}

	if existing := apimeta.FindStatusCondition(export.Status.Conditions, mcsv1alpha1.ServiceExportValid); existing != nil &&
		existing.Status == desired.Status &&
		existing.Reason == desired.Reason &&
		existing.Message == desired.Message &&
		existing.ObservedGeneration == desired.ObservedGeneration {
		return ctrl.Result{}, nil
	}

	base := export.DeepCopy()
	apimeta.SetStatusCondition(&export.Status.Conditions, desired)
	if err := r.Status().Patch(ctx, export, client.MergeFrom(base)); err != nil {
		return ctrl.Result{}, fmt.Errorf("updating ServiceExport status: %w", err)
	}
	return ctrl.Result{}, nil
}

// SetupWithManager registers the controller and a watch on Services so that
// creating, mutating, or deleting a Service re-reconciles the matching
// ServiceExport - as in the same name/namespace). The enqueued request
// is a no-op when no such ServiceExport exists.
func (r *ServiceExportReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&mcsv1alpha1.ServiceExport{}).
		Watches(&corev1.Service{}, handler.EnqueueRequestsFromMapFunc(serviceToExportRequest)).
		Watches(&discoveryv1.EndpointSlice{}, handler.EnqueueRequestsFromMapFunc(endpointSliceToExportRequest)).
		Named("serviceexport").
		Complete(r)
}

// endpointSliceToExportRequest maps an EndpointSlice event to the reconcile
// request for the ServiceExport of its owning service (from the standard
// kubernetes.io/service-name label), so headless endpoint changes re-publish.
func endpointSliceToExportRequest(_ context.Context, obj client.Object) []reconcile.Request {
	svcName := obj.GetLabels()[discoveryv1.LabelServiceName]
	if svcName == "" {
		return nil
	}
	return []reconcile.Request{{
		NamespacedName: types.NamespacedName{Namespace: obj.GetNamespace(), Name: svcName},
	}}
}

// serviceToExportRequest maps a Service event to the reconcile request for the
// like-named ServiceExport.
func serviceToExportRequest(_ context.Context, obj client.Object) []reconcile.Request {
	return []reconcile.Request{{
		NamespacedName: types.NamespacedName{Namespace: obj.GetNamespace(), Name: obj.GetName()},
	}}
}
