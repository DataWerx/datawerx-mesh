package controllers_test

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	mcsv1alpha1 "github.com/datawerx/datawerx/pkg/apis/multicluster/v1alpha1"
	networkingv1alpha1 "github.com/datawerx/datawerx/pkg/apis/networking/v1alpha1"
	"github.com/datawerx/datawerx/pkg/controllers"
)

// exportFinalizer mirrors the unexported finalizer the controller installs;
// pre-seeding it lets a single reconcile proceed straight to validation/publish
// instead of spending a pass installing the finalizer.
const exportFinalizer = "networking.datawerx.io/serviceexport-cleanup"

func newExportReconciler(t *testing.T, objs ...client.Object) (*controllers.ServiceExportReconciler, client.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("core scheme: %v", err)
	}
	if err := mcsv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("mcs scheme: %v", err)
	}
	if err := networkingv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("networking scheme: %v", err)
	}
	fc := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		WithStatusSubresource(&mcsv1alpha1.ServiceExport{}).
		Build()
	return &controllers.ServiceExportReconciler{Client: fc, Scheme: scheme, ClusterID: "c1"}, fc
}

func exportWithFinalizer(name, ns string) *mcsv1alpha1.ServiceExport {
	return &mcsv1alpha1.ServiceExport{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Finalizers: []string{exportFinalizer}},
	}
}

func reconcileExport(t *testing.T, r *controllers.ServiceExportReconciler, ns, name string) {
	t.Helper()
	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: ns, Name: name},
	})
	if err != nil {
		t.Fatalf("Reconcile(%s/%s): %v", ns, name, err)
	}
}

func validCondition(t *testing.T, c client.Client, ns, name string) *metav1.Condition {
	t.Helper()
	var se mcsv1alpha1.ServiceExport
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: name}, &se); err != nil {
		t.Fatalf("get serviceexport: %v", err)
	}
	return apimeta.FindStatusCondition(se.Status.Conditions, mcsv1alpha1.ServiceExportValid)
}

func TestServiceExport_ValidWhenServiceExists(t *testing.T) {
	export := exportWithFinalizer("payments", "prod")
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "payments", Namespace: "prod"},
		Spec: corev1.ServiceSpec{
			ClusterIP: "10.96.0.10",
			Ports:     []corev1.ServicePort{{Name: "http", Port: 80, Protocol: corev1.ProtocolTCP}},
		},
	}
	r, c := newExportReconciler(t, export, svc)

	reconcileExport(t, r, "prod", "payments")

	cond := validCondition(t, c, "prod", "payments")
	if cond == nil {
		t.Fatal("expected Valid condition to be set")
	}
	if cond.Status != metav1.ConditionTrue {
		t.Errorf("Valid = %s, want True", cond.Status)
	}
	if cond.Reason != "Exported" {
		t.Errorf("reason = %s, want Exported", cond.Reason)
	}
}

func TestServiceExport_InvalidWhenServiceMissing(t *testing.T) {
	export := exportWithFinalizer("ghost", "prod")
	r, c := newExportReconciler(t, export)

	reconcileExport(t, r, "prod", "ghost")

	cond := validCondition(t, c, "prod", "ghost")
	if cond == nil {
		t.Fatal("expected Valid condition to be set")
	}
	if cond.Status != metav1.ConditionFalse {
		t.Errorf("Valid = %s, want False", cond.Status)
	}
	if cond.Reason != "ServiceNotFound" {
		t.Errorf("reason = %s, want ServiceNotFound", cond.Reason)
	}
}

func TestServiceExport_HeadlessReported(t *testing.T) {
	export := exportWithFinalizer("db", "data")
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "data"},
		Spec:       corev1.ServiceSpec{ClusterIP: corev1.ClusterIPNone, Ports: []corev1.ServicePort{{Port: 5432}}},
	}
	r, c := newExportReconciler(t, export, svc)

	reconcileExport(t, r, "data", "db")

	cond := validCondition(t, c, "data", "db")
	if cond == nil || cond.Status != metav1.ConditionTrue {
		t.Fatalf("expected Valid=True, got %#v", cond)
	}
	if want := "Headless"; !contains(cond.Message, want) {
		t.Errorf("message %q should mention %q", cond.Message, want)
	}
}

func TestServiceExport_NoExportIsNoOp(t *testing.T) {
	// Only a Service exists, no ServiceExport — reconcile must be a clean no-op.
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "lonely", Namespace: "prod"}}
	r, _ := newExportReconciler(t, svc)
	reconcileExport(t, r, "prod", "lonely") // must not error
}

func TestServiceExport_IdempotentStatus(t *testing.T) {
	export := exportWithFinalizer("payments", "prod")
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "payments", Namespace: "prod"},
		Spec:       corev1.ServiceSpec{ClusterIP: "10.96.0.10", Ports: []corev1.ServicePort{{Port: 80}}},
	}
	r, c := newExportReconciler(t, export, svc)

	reconcileExport(t, r, "prod", "payments")
	first := validCondition(t, c, "prod", "payments")
	reconcileExport(t, r, "prod", "payments")
	second := validCondition(t, c, "prod", "payments")

	// LastTransitionTime must not move when nothing changed.
	if !first.LastTransitionTime.Equal(&second.LastTransitionTime) {
		t.Error("LastTransitionTime changed on a no-op reconcile (status was rewritten)")
	}
}

func getEndpointExport(t *testing.T, c client.Client, ns, name string) (*networkingv1alpha1.EndpointExport, bool) {
	t.Helper()
	var ee networkingv1alpha1.EndpointExport
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: name}, &ee); err != nil {
		return nil, false
	}
	return &ee, true
}

func TestServiceExport_PublishesEndpointExport(t *testing.T) {
	export := exportWithFinalizer("payments", "prod")
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "payments", Namespace: "prod"},
		Spec: corev1.ServiceSpec{
			ClusterIP: "10.96.0.10",
			Ports:     []corev1.ServicePort{{Name: "http", Port: 80, Protocol: corev1.ProtocolTCP}},
		},
	}
	r, c := newExportReconciler(t, export, svc)

	reconcileExport(t, r, "prod", "payments")

	ee, ok := getEndpointExport(t, c, "prod", "c1-payments")
	if !ok {
		t.Fatal("expected EndpointExport c1-payments to be published")
	}
	if ee.Spec.ClusterID != "c1" || ee.Spec.ServiceName != "payments" {
		t.Errorf("unexpected EndpointExport spec: %#v", ee.Spec)
	}
	if ee.Spec.Type != mcsv1alpha1.ClusterSetIP || len(ee.Spec.IPs) != 1 || ee.Spec.IPs[0] != "10.96.0.10" {
		t.Errorf("unexpected endpoint type/IPs: %#v", ee.Spec)
	}
}

func TestServiceExport_WithdrawsWhenServiceMissing(t *testing.T) {
	export := exportWithFinalizer("payments", "prod")
	stale := &networkingv1alpha1.EndpointExport{
		ObjectMeta: metav1.ObjectMeta{Name: "c1-payments", Namespace: "prod"},
		Spec:       networkingv1alpha1.EndpointExportSpec{ClusterID: "c1", ServiceNamespace: "prod", ServiceName: "payments"},
	}
	r, c := newExportReconciler(t, export, stale)

	reconcileExport(t, r, "prod", "payments") // no Service exists

	if _, ok := getEndpointExport(t, c, "prod", "c1-payments"); ok {
		t.Error("expected stale EndpointExport to be withdrawn when Service is missing")
	}
	cond := validCondition(t, c, "prod", "payments")
	if cond == nil || cond.Status != metav1.ConditionFalse {
		t.Errorf("expected Valid=False, got %#v", cond)
	}
}

func TestServiceExport_InstallsFinalizerFirst(t *testing.T) {
	// Without a pre-seeded finalizer, the first reconcile only installs it.
	export := &mcsv1alpha1.ServiceExport{ObjectMeta: metav1.ObjectMeta{Name: "payments", Namespace: "prod"}}
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "payments", Namespace: "prod"}, Spec: corev1.ServiceSpec{ClusterIP: "10.96.0.10"}}
	r, c := newExportReconciler(t, export, svc)

	reconcileExport(t, r, "prod", "payments")

	var se mcsv1alpha1.ServiceExport
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "prod", Name: "payments"}, &se); err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(se.Finalizers) == 0 || se.Finalizers[0] != exportFinalizer {
		t.Fatalf("expected finalizer installed, got %v", se.Finalizers)
	}
	if _, ok := getEndpointExport(t, c, "prod", "c1-payments"); ok {
		t.Error("EndpointExport should not be published before the finalizer pass completes")
	}
}

func TestServiceExport_DeletionWithdrawsAndClearsFinalizer(t *testing.T) {
	now := metav1.Now()
	export := &mcsv1alpha1.ServiceExport{
		ObjectMeta: metav1.ObjectMeta{
			Name: "payments", Namespace: "prod",
			Finalizers:        []string{exportFinalizer},
			DeletionTimestamp: &now,
		},
	}
	ee := &networkingv1alpha1.EndpointExport{
		ObjectMeta: metav1.ObjectMeta{Name: "c1-payments", Namespace: "prod"},
		Spec:       networkingv1alpha1.EndpointExportSpec{ClusterID: "c1", ServiceNamespace: "prod", ServiceName: "payments"},
	}
	r, c := newExportReconciler(t, export, ee)

	reconcileExport(t, r, "prod", "payments")

	if _, ok := getEndpointExport(t, c, "prod", "c1-payments"); ok {
		t.Error("expected EndpointExport withdrawn on ServiceExport deletion")
	}
	var se mcsv1alpha1.ServiceExport
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "prod", Name: "payments"}, &se); err == nil {
		t.Errorf("expected ServiceExport gone after finalizer removal, still present with %v", se.Finalizers)
	}
}

func TestServiceExport_HeadlessPublishesEndpointIPs(t *testing.T) {
	export := exportWithFinalizer("db", "data")
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "data"},
		Spec:       corev1.ServiceSpec{ClusterIP: corev1.ClusterIPNone, Ports: []corev1.ServicePort{{Port: 5432}}},
	}
	ready := true
	es := &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "db-abc",
			Namespace: "data",
			Labels:    map[string]string{discoveryv1.LabelServiceName: "db"},
		},
		AddressType: discoveryv1.AddressTypeIPv4,
		Endpoints: []discoveryv1.Endpoint{
			{Addresses: []string{"10.0.1.5"}, Conditions: discoveryv1.EndpointConditions{Ready: &ready}},
			{Addresses: []string{"10.0.1.6"}, Conditions: discoveryv1.EndpointConditions{Ready: &ready}},
		},
	}
	r, c := newExportReconciler(t, export, svc, es)

	reconcileExport(t, r, "data", "db")

	ee, ok := getEndpointExport(t, c, "data", "c1-db")
	if !ok {
		t.Fatal("expected headless EndpointExport to be published")
	}
	if ee.Spec.Type != mcsv1alpha1.Headless {
		t.Errorf("type = %s, want Headless", ee.Spec.Type)
	}
	if len(ee.Spec.IPs) != 2 || ee.Spec.IPs[0] != "10.0.1.5" || ee.Spec.IPs[1] != "10.0.1.6" {
		t.Errorf("endpoint IPs = %v, want [10.0.1.5 10.0.1.6]", ee.Spec.IPs)
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (haystack == needle || indexOf(haystack, needle) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
