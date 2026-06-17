package controllers_test

import (
	"context"
	"net"
	"testing"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	mcsv1alpha1 "github.com/DataWerx/datawerx-mesh/pkg/apis/multicluster/v1alpha1"
	networkingv1alpha1 "github.com/DataWerx/datawerx-mesh/pkg/apis/networking/v1alpha1"
	"github.com/DataWerx/datawerx-mesh/pkg/controllers"
	"github.com/DataWerx/datawerx-mesh/pkg/dns"
)

func newImportReconciler(t *testing.T, objs ...client.Object) (*controllers.ServiceImportReconciler, client.Client) {
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
		WithStatusSubresource(&mcsv1alpha1.ServiceImport{}).
		Build()
	return &controllers.ServiceImportReconciler{Client: fc, Scheme: scheme}, fc
}

func reconcileImport(t *testing.T, r *controllers.ServiceImportReconciler, ns, name string) {
	t.Helper()
	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: ns, Name: name},
	}); err != nil {
		t.Fatalf("Reconcile(%s/%s): %v", ns, name, err)
	}
}

func endpointExport(name, clusterID, ns, svc string, typ mcsv1alpha1.ServiceImportType, ips []string, ports ...mcsv1alpha1.ServicePort) *networkingv1alpha1.EndpointExport {
	return &networkingv1alpha1.EndpointExport{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: networkingv1alpha1.EndpointExportSpec{
			ClusterID:        clusterID,
			ServiceNamespace: ns,
			ServiceName:      svc,
			Type:             typ,
			IPs:              ips,
			Ports:            ports,
		},
	}
}

func getImport(t *testing.T, c client.Client, ns, name string) (*mcsv1alpha1.ServiceImport, bool) {
	t.Helper()
	var si mcsv1alpha1.ServiceImport
	err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: name}, &si)
	if err != nil {
		return nil, false
	}
	return &si, true
}

func TestServiceImport_AggregatesTwoClusters(t *testing.T) {
	httpPort := mcsv1alpha1.ServicePort{Name: "http", Port: 80, Protocol: corev1.ProtocolTCP}
	a := endpointExport("a-payments", "a", "prod", "payments", mcsv1alpha1.ClusterSetIP, []string{"10.96.0.10"}, httpPort)
	b := endpointExport("b-payments", "b", "prod", "payments", mcsv1alpha1.ClusterSetIP, []string{"10.97.0.10"}, httpPort)

	r, c := newImportReconciler(t, a, b)
	reconcileImport(t, r, "prod", "payments")

	si, ok := getImport(t, c, "prod", "payments")
	if !ok {
		t.Fatal("expected ServiceImport to be created")
	}
	if si.Spec.Type != mcsv1alpha1.ClusterSetIP {
		t.Errorf("type = %s, want ClusterSetIP", si.Spec.Type)
	}
	if len(si.Spec.IPs) != 1 {
		t.Fatalf("expected 1 ClusterSetIP, got %v", si.Spec.IPs)
	}
	if ip := net.ParseIP(si.Spec.IPs[0]); ip == nil || !is241(ip) {
		t.Errorf("ClusterSetIP %q not in 241.0.0.0/8", si.Spec.IPs[0])
	}
	if len(si.Spec.Ports) != 1 || si.Spec.Ports[0].Port != 80 {
		t.Errorf("ports = %v, want single 80", si.Spec.Ports)
	}
	if len(si.Status.Clusters) != 2 {
		t.Errorf("expected 2 contributing clusters, got %v", si.Status.Clusters)
	}
}

func TestServiceImport_Headless(t *testing.T) {
	a := endpointExport("a-db", "a", "data", "db", mcsv1alpha1.Headless, nil,
		mcsv1alpha1.ServicePort{Port: 5432, Protocol: corev1.ProtocolTCP})
	r, c := newImportReconciler(t, a)
	reconcileImport(t, r, "data", "db")

	si, ok := getImport(t, c, "data", "db")
	if !ok {
		t.Fatal("expected ServiceImport")
	}
	if si.Spec.Type != mcsv1alpha1.Headless {
		t.Errorf("type = %s, want Headless", si.Spec.Type)
	}
	if len(si.Spec.IPs) != 0 {
		t.Errorf("headless import must have no ClusterSetIP, got %v", si.Spec.IPs)
	}
}

func TestServiceImport_DeletedWhenNoExports(t *testing.T) {
	// A pre-existing ServiceImport with no backing EndpointExports must be removed.
	stale := &mcsv1alpha1.ServiceImport{
		ObjectMeta: metav1.ObjectMeta{Name: "gone", Namespace: "prod"},
		Spec:       mcsv1alpha1.ServiceImportSpec{Type: mcsv1alpha1.ClusterSetIP},
	}
	r, c := newImportReconciler(t, stale)
	reconcileImport(t, r, "prod", "gone")

	if _, ok := getImport(t, c, "prod", "gone"); ok {
		t.Error("expected stale ServiceImport to be deleted")
	}
}

func TestServiceImport_ConsistentIPAcrossClusters(t *testing.T) {
	// The same export set, reconciled by two independent reconcilers simulating
	// two clusters, must produce the same ClusterSetIP.
	mk := func() (*controllers.ServiceImportReconciler, client.Client) {
		a := endpointExport("a-payments", "a", "prod", "payments", mcsv1alpha1.ClusterSetIP, []string{"10.96.0.10"})
		other := endpointExport("a-orders", "a", "prod", "orders", mcsv1alpha1.ClusterSetIP, []string{"10.96.0.11"})
		return newImportReconciler(t, a, other)
	}
	r1, c1 := mk()
	r2, c2 := mk()
	reconcileImport(t, r1, "prod", "payments")
	reconcileImport(t, r2, "prod", "payments")

	si1, _ := getImport(t, c1, "prod", "payments")
	si2, _ := getImport(t, c2, "prod", "payments")
	if si1.Spec.IPs[0] != si2.Spec.IPs[0] {
		t.Errorf("ClusterSetIP differs across clusters: %s vs %s", si1.Spec.IPs[0], si2.Spec.IPs[0])
	}
}

func is241(ip net.IP) bool {
	v4 := ip.To4()
	return v4 != nil && v4[0] == 241
}

func TestServiceImport_DualStack(t *testing.T) {
	// A service whose backends span both families must get a v4 ClusterSetIP and,
	// when an IPv6 pool is configured, an additional v6 ClusterSetIP.
	httpPort := mcsv1alpha1.ServicePort{Name: "http", Port: 80, Protocol: corev1.ProtocolTCP}
	a := endpointExport("a-web", "a", "prod", "web", mcsv1alpha1.ClusterSetIP, []string{"10.96.0.10"}, httpPort)
	b := endpointExport("b-web", "b", "prod", "web", mcsv1alpha1.ClusterSetIP, []string{"fd00:96::10"}, httpPort)

	r, c := newImportReconciler(t, a, b)
	r.ClusterSetCIDR6 = "fd00:cafe::/64"
	reconcileImport(t, r, "prod", "web")

	si, ok := getImport(t, c, "prod", "web")
	if !ok {
		t.Fatal("expected ServiceImport to be created")
	}
	if len(si.Spec.IPs) != 2 {
		t.Fatalf("expected v4 + v6 ClusterSetIPs, got %v", si.Spec.IPs)
	}
	var sawV4, sawV6 bool
	_, v6net, _ := net.ParseCIDR("fd00:cafe::/64")
	for _, s := range si.Spec.IPs {
		ip := net.ParseIP(s)
		if ip == nil {
			t.Fatalf("unparseable ClusterSetIP %q", s)
		}
		if ip.To4() != nil {
			if !is241(ip) {
				t.Errorf("v4 ClusterSetIP %q not in 241.0.0.0/8", s)
			}
			sawV4 = true
			continue
		}
		if !v6net.Contains(ip) {
			t.Errorf("v6 ClusterSetIP %q not in fd00:cafe::/64", s)
		}
		sawV6 = true
	}
	if !sawV4 || !sawV6 {
		t.Errorf("expected one IP per family, got %v", si.Spec.IPs)
	}
}

func TestServiceImport_NoIPv6PoolSkipsV6(t *testing.T) {
	// Without a configured v6 pool, an IPv6-backed service still gets only a v4 VIP.
	a := endpointExport("a-web", "a", "prod", "web", mcsv1alpha1.ClusterSetIP, []string{"fd00:96::10"})
	r, c := newImportReconciler(t, a) // ClusterSetCIDR6 unset
	reconcileImport(t, r, "prod", "web")

	si, ok := getImport(t, c, "prod", "web")
	if !ok {
		t.Fatal("expected ServiceImport")
	}
	if len(si.Spec.IPs) != 1 {
		t.Fatalf("expected single v4 ClusterSetIP, got %v", si.Spec.IPs)
	}
	if ip := net.ParseIP(si.Spec.IPs[0]); ip == nil || ip.To4() == nil {
		t.Errorf("expected v4 ClusterSetIP, got %q", si.Spec.IPs[0])
	}
}

// listMirrorSlices returns the DataWerx-mirrored EndpointSlices for a service.
func listMirrorSlices(t *testing.T, c client.Client, ns, svc string) []discoveryv1.EndpointSlice {
	t.Helper()
	var list discoveryv1.EndpointSliceList
	if err := c.List(context.Background(), &list,
		client.InNamespace(ns),
		client.MatchingLabels{
			mcsv1alpha1.LabelServiceName: svc,
			discoveryv1.LabelManagedBy:   dns.MirrorManagedBy,
		},
	); err != nil {
		t.Fatalf("listing mirrored slices: %v", err)
	}
	return list.Items
}

func TestServiceImport_MirrorsHeadlessEndpointSlices(t *testing.T) {
	httpPort := mcsv1alpha1.ServicePort{Name: "http", Port: 80, Protocol: corev1.ProtocolTCP}
	a := endpointExport("a-web", "a", "prod", "web", mcsv1alpha1.Headless, []string{"10.1.0.1", "10.1.0.2"}, httpPort)
	b := endpointExport("b-web", "b", "prod", "web", mcsv1alpha1.Headless, []string{"10.2.0.1"}, httpPort)

	r, c := newImportReconciler(t, a, b)
	reconcileImport(t, r, "prod", "web")

	slices := listMirrorSlices(t, c, "prod", "web")
	if len(slices) != 2 {
		t.Fatalf("expected one mirrored slice per source cluster, got %d", len(slices))
	}
	bySource := map[string]discoveryv1.EndpointSlice{}
	addrs := map[string]bool{}
	for _, s := range slices {
		bySource[s.Labels[mcsv1alpha1.LabelSourceCluster]] = s
		for _, ep := range s.Endpoints {
			addrs[ep.Addresses[0]] = true
		}
	}
	if _, ok := bySource["a"]; !ok {
		t.Error("missing mirrored slice for source cluster a")
	}
	if _, ok := bySource["b"]; !ok {
		t.Error("missing mirrored slice for source cluster b")
	}
	for _, want := range []string{"10.1.0.1", "10.1.0.2", "10.2.0.1"} {
		if !addrs[want] {
			t.Errorf("mirrored endpoints missing %s", want)
		}
	}
}

func TestServiceImport_ClusterSetIPHasNoMirrorSlices(t *testing.T) {
	a := endpointExport("a-pay", "a", "prod", "pay", mcsv1alpha1.ClusterSetIP, []string{"10.96.0.10"},
		mcsv1alpha1.ServicePort{Name: "http", Port: 80, Protocol: corev1.ProtocolTCP})

	r, c := newImportReconciler(t, a)
	reconcileImport(t, r, "prod", "pay")

	if slices := listMirrorSlices(t, c, "prod", "pay"); len(slices) != 0 {
		t.Errorf("a ClusterSetIP import must not mirror EndpointSlices, got %d", len(slices))
	}
}

func TestServiceImport_PrunesMirrorSlicesOnWithdrawal(t *testing.T) {
	a := endpointExport("a-web", "a", "prod", "web", mcsv1alpha1.Headless, []string{"10.1.0.1"})
	r, c := newImportReconciler(t, a)
	reconcileImport(t, r, "prod", "web")
	if slices := listMirrorSlices(t, c, "prod", "web"); len(slices) != 1 {
		t.Fatalf("setup: expected 1 mirrored slice, got %d", len(slices))
	}

	// All exporters withdraw; the next reconcile must prune the mirrored slices.
	if err := c.Delete(context.Background(), a); err != nil {
		t.Fatalf("deleting export: %v", err)
	}
	reconcileImport(t, r, "prod", "web")
	if slices := listMirrorSlices(t, c, "prod", "web"); len(slices) != 0 {
		t.Errorf("withdrawal must prune mirrored slices, got %d", len(slices))
	}
}

func TestServiceImport_PrunesStaleSourceClusterSlice(t *testing.T) {
	a := endpointExport("a-web", "a", "prod", "web", mcsv1alpha1.Headless, []string{"10.1.0.1"})
	b := endpointExport("b-web", "b", "prod", "web", mcsv1alpha1.Headless, []string{"10.2.0.1"})
	r, c := newImportReconciler(t, a, b)
	reconcileImport(t, r, "prod", "web")
	if slices := listMirrorSlices(t, c, "prod", "web"); len(slices) != 2 {
		t.Fatalf("setup: expected 2 mirrored slices, got %d", len(slices))
	}

	// Cluster b stops exporting; only a's slice should remain.
	if err := c.Delete(context.Background(), b); err != nil {
		t.Fatalf("deleting export b: %v", err)
	}
	reconcileImport(t, r, "prod", "web")
	slices := listMirrorSlices(t, c, "prod", "web")
	if len(slices) != 1 {
		t.Fatalf("expected 1 slice after b withdraws, got %d", len(slices))
	}
	if got := slices[0].Labels[mcsv1alpha1.LabelSourceCluster]; got != "a" {
		t.Errorf("remaining slice should be a's, got source %q", got)
	}
}
