package controllers_test

import (
	"context"
	"sort"
	"sync"
	"testing"

	corev1 "k8s.io/api/core/v1"
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
	"github.com/datawerx/datawerx/pkg/nat"
)

type fakeNATDataPlane struct {
	mu   sync.Mutex
	last []nat.ServiceDNAT
	err  error
}

func (f *fakeNATDataPlane) SyncClusterSetNAT(services []nat.ServiceDNAT) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.last = append([]nat.ServiceDNAT(nil), services...)
	return nil
}

func (f *fakeNATDataPlane) synced() []nat.ServiceDNAT {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.last
}

func newNATReconciler(t *testing.T, dp controllers.ServiceNATDataPlane, objs ...client.Object) *controllers.ServiceNATReconciler {
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
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	return &controllers.ServiceNATReconciler{Client: fc, Scheme: scheme, DataPlane: dp}
}

func clusterSetImport(ns, name, vip string, port int32) *mcsv1alpha1.ServiceImport {
	return &mcsv1alpha1.ServiceImport{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: mcsv1alpha1.ServiceImportSpec{
			Type:  mcsv1alpha1.ClusterSetIP,
			IPs:   []string{vip},
			Ports: []mcsv1alpha1.ServicePort{{Port: port, Protocol: corev1.ProtocolTCP}},
		},
	}
}

func clusterSetExport(name, cluster, ns, svc, ip string) *networkingv1alpha1.EndpointExport {
	return &networkingv1alpha1.EndpointExport{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: networkingv1alpha1.EndpointExportSpec{
			ClusterID: cluster, ServiceNamespace: ns, ServiceName: svc,
			Type: mcsv1alpha1.ClusterSetIP, IPs: []string{ip},
		},
	}
}

func TestServiceNAT_BuildsBackendsFromExports(t *testing.T) {
	si := clusterSetImport("prod", "payments", "241.0.0.5", 80)
	a := clusterSetExport("a-payments", "a", "prod", "payments", "10.96.0.10")
	b := clusterSetExport("b-payments", "b", "prod", "payments", "10.97.0.10")

	dp := &fakeNATDataPlane{}
	r := newNATReconciler(t, dp, si, a, b)

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "prod", Name: "payments"}}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	got := dp.synced()
	if len(got) != 1 {
		t.Fatalf("expected 1 ServiceDNAT, got %d: %#v", len(got), got)
	}
	s := got[0]
	if s.VIP != "241.0.0.5" {
		t.Errorf("VIP = %s, want 241.0.0.5", s.VIP)
	}
	sort.Strings(s.Backends)
	if len(s.Backends) != 2 || s.Backends[0] != "10.96.0.10" || s.Backends[1] != "10.97.0.10" {
		t.Errorf("backends = %v, want both cluster IPs", s.Backends)
	}
	if len(s.Ports) != 1 || s.Ports[0].Port != 80 || s.Ports[0].Protocol != "tcp" {
		t.Errorf("ports = %#v, want 80/tcp", s.Ports)
	}
}

func TestServiceNAT_HeadlessAndBackendlessSkipped(t *testing.T) {
	// Headless import: no DNAT.
	headless := &mcsv1alpha1.ServiceImport{
		ObjectMeta: metav1.ObjectMeta{Namespace: "data", Name: "db"},
		Spec:       mcsv1alpha1.ServiceImportSpec{Type: mcsv1alpha1.Headless},
	}
	// ClusterSetIP import with no matching exports: no backends -> skipped.
	noBackends := clusterSetImport("prod", "lonely", "241.0.0.9", 443)

	dp := &fakeNATDataPlane{}
	r := newNATReconciler(t, dp, headless, noBackends)

	if _, err := r.Reconcile(context.Background(), ctrl.Request{}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if got := dp.synced(); len(got) != 0 {
		t.Errorf("expected no ServiceDNAT, got %#v", got)
	}
}

func TestServiceNAT_SyncErrorSurfaces(t *testing.T) {
	si := clusterSetImport("prod", "payments", "241.0.0.5", 80)
	ex := clusterSetExport("a-payments", "a", "prod", "payments", "10.96.0.10")
	dp := &fakeNATDataPlane{err: context.DeadlineExceeded}
	r := newNATReconciler(t, dp, si, ex)

	if _, err := r.Reconcile(context.Background(), ctrl.Request{}); err == nil {
		t.Fatal("expected reconcile error when data plane sync fails")
	}
}

func TestBuildServiceDNAT_Pure(t *testing.T) {
	imports := []mcsv1alpha1.ServiceImport{*clusterSetImport("prod", "payments", "241.0.0.5", 80)}
	exports := []networkingv1alpha1.EndpointExport{*clusterSetExport("a-payments", "a", "prod", "payments", "10.96.0.10")}
	out := controllers.BuildServiceDNAT(imports, exports)
	if len(out) != 1 || out[0].VIP != "241.0.0.5" || len(out[0].Backends) != 1 {
		t.Fatalf("unexpected ServiceDNAT: %#v", out)
	}
}
