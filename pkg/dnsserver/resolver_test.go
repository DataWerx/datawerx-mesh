package dnsserver_test

import (
	"reflect"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	mcsv1alpha1 "github.com/DataWerx/datawerx-mesh/pkg/apis/multicluster/v1alpha1"
	networkingv1alpha1 "github.com/DataWerx/datawerx-mesh/pkg/apis/networking/v1alpha1"
	"github.com/DataWerx/datawerx-mesh/pkg/dnsserver"
)

func newFakeReader(t *testing.T, objs ...client.Object) client.Reader {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := mcsv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("mcs scheme: %v", err)
	}
	if err := networkingv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("networking scheme: %v", err)
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
}

func TestCachedResolver_ClusterSetIP(t *testing.T) {
	si := &mcsv1alpha1.ServiceImport{
		ObjectMeta: metav1.ObjectMeta{Name: "payments", Namespace: "prod"},
		Spec:       mcsv1alpha1.ServiceImportSpec{Type: mcsv1alpha1.ClusterSetIP, IPs: []string{"241.0.0.5"}},
	}
	r := &dnsserver.CachedResolver{Reader: newFakeReader(t, si)}

	got, ok, err := r.LookupClusterSet("prod", "payments")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok {
		t.Fatal("expected found")
	}
	if got.Type != mcsv1alpha1.ClusterSetIP || !reflect.DeepEqual(got.IPs, []string{"241.0.0.5"}) {
		t.Errorf("got %#v", got)
	}
}

func TestCachedResolver_HeadlessUnionsEndpointExports(t *testing.T) {
	si := &mcsv1alpha1.ServiceImport{
		ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "data"},
		Spec:       mcsv1alpha1.ServiceImportSpec{Type: mcsv1alpha1.Headless},
	}
	eeA := &networkingv1alpha1.EndpointExport{
		ObjectMeta: metav1.ObjectMeta{Name: "a-db", Namespace: "data"},
		Spec: networkingv1alpha1.EndpointExportSpec{
			ClusterID: "a", ServiceNamespace: "data", ServiceName: "db",
			Type: mcsv1alpha1.Headless, IPs: []string{"10.0.1.5", "10.0.1.6"},
		},
	}
	eeB := &networkingv1alpha1.EndpointExport{
		ObjectMeta: metav1.ObjectMeta{Name: "b-db", Namespace: "data"},
		Spec: networkingv1alpha1.EndpointExportSpec{
			ClusterID: "b", ServiceNamespace: "data", ServiceName: "db",
			Type: mcsv1alpha1.Headless, IPs: []string{"10.1.2.7", "10.0.1.5"}, // overlap deduped
		},
	}
	// An unrelated export in the same namespace must be ignored.
	other := &networkingv1alpha1.EndpointExport{
		ObjectMeta: metav1.ObjectMeta{Name: "a-cache", Namespace: "data"},
		Spec: networkingv1alpha1.EndpointExportSpec{
			ClusterID: "a", ServiceNamespace: "data", ServiceName: "cache",
			Type: mcsv1alpha1.Headless, IPs: []string{"10.9.9.9"},
		},
	}
	r := &dnsserver.CachedResolver{Reader: newFakeReader(t, si, eeA, eeB, other)}

	got, ok, err := r.LookupClusterSet("data", "db")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok {
		t.Fatal("expected found")
	}
	want := []string{"10.0.1.5", "10.0.1.6", "10.1.2.7"}
	if !reflect.DeepEqual(got.IPs, want) {
		t.Errorf("headless IPs = %v, want %v", got.IPs, want)
	}
}

func TestCachedResolver_NotImported(t *testing.T) {
	r := &dnsserver.CachedResolver{Reader: newFakeReader(t)}
	_, ok, err := r.LookupClusterSet("prod", "ghost")
	if err != nil {
		t.Fatalf("a genuinely absent service must report found=false with NO error, got %v", err)
	}
	if ok {
		t.Error("expected not found for unimported service")
	}
}
