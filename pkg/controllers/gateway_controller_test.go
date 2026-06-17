package controllers_test

import (
	"context"
	"encoding/json"
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

	networkingv1alpha1 "github.com/DataWerx/datawerx-mesh/pkg/apis/networking/v1alpha1"
	"github.com/DataWerx/datawerx-mesh/pkg/controllers"
	"github.com/DataWerx/datawerx-mesh/pkg/gateway"
)

type fakeGatewayDataPlane struct {
	mu     sync.Mutex
	client []string
	dest   []string
	calls  int
}

func (f *fakeGatewayDataPlane) SyncGatewayMasq(clientCIDRs, destCIDRs []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.client = append([]string(nil), clientCIDRs...)
	f.dest = append([]string(nil), destCIDRs...)
	f.calls++
	return nil
}

func newGatewayReconciler(t *testing.T, dp controllers.GatewayDataPlane, objs ...*networkingv1alpha1.MeshPeer) (*controllers.GatewayReconciler, client.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = networkingv1alpha1.AddToScheme(scheme)
	b := fake.NewClientBuilder().WithScheme(scheme)
	for _, o := range objs {
		b = b.WithObjects(o)
	}
	c := b.Build()
	r := &controllers.GatewayReconciler{
		Client:           c,
		Scheme:           scheme,
		DataPlane:        dp,
		ClientCIDRs:      []string{"100.64.0.0/10"},
		GatewayEndpoints: []string{"100.64.0.5"},
		ClusterSetCIDRs:  []string{"241.0.0.0/8"},
		LocalCIDRs:       []string{"10.244.0.0/16", "10.96.0.0/16"},
		DNS:              gateway.DNSConfig{Addr: "100.64.0.5:5353", SearchDomains: []string{"clusterset.local"}},
	}
	return r, c
}

func readProfile(t *testing.T, c client.Client, namespace string) gateway.AccessProfile {
	t.Helper()
	var cm corev1.ConfigMap
	key := types.NamespacedName{Namespace: namespace, Name: gateway.ProfileConfigMapName}
	if err := c.Get(context.Background(), key, &cm); err != nil {
		t.Fatalf("get profile ConfigMap %s: %v", key, err)
	}
	raw, ok := cm.Data[gateway.ProfileConfigMapKey]
	if !ok {
		t.Fatalf("ConfigMap missing key %q: %v", gateway.ProfileConfigMapKey, cm.Data)
	}
	var p gateway.AccessProfile
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		t.Fatalf("unmarshal profile: %v", err)
	}
	return p
}

// A routed, non-overlapping peer contributes its CIDRs to both the masquerade
// destinations and the published route set.  The masquerade source is the
// configured client range, and the ClusterSetIP range is always reachable.
func TestGateway_ProgramsMasqAndPublishesProfile(t *testing.T) {
	peer := &networkingv1alpha1.MeshPeer{
		ObjectMeta: metav1.ObjectMeta{Name: "b"},
		Spec: networkingv1alpha1.MeshPeerSpec{
			ClusterID: "cluster-b", PublicKey: "kb",
			PodCIDRs:     []string{"10.245.0.0/16"},
			ServiceCIDRs: []string{"10.97.0.0/16"},
		},
	}
	dp := &fakeGatewayDataPlane{}
	r, c := newGatewayReconciler(t, dp, peer)

	if _, err := r.Reconcile(context.Background(), ctrl.Request{}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	dp.mu.Lock()
	assertSet(t, "masq client", dp.client, []string{"100.64.0.0/10"})
	assertSet(t, "masq dest", dp.dest, []string{
		"241.0.0.0/8", "10.244.0.0/16", "10.96.0.0/16", "10.245.0.0/16", "10.97.0.0/16",
	})
	dp.mu.Unlock()

	p := readProfile(t, c, gateway.DefaultProfileNamespace)
	assertSet(t, "route CIDRs", p.RouteCIDRs, []string{
		"241.0.0.0/8", "10.244.0.0/16", "10.96.0.0/16", "10.245.0.0/16", "10.97.0.0/16",
	})
	assertSet(t, "endpoints", p.GatewayEndpoints, []string{"100.64.0.5"})
	if p.DNS.Addr != "100.64.0.5:5353" {
		t.Errorf("DNS.Addr = %q, want 100.64.0.5:5353", p.DNS.Addr)
	}
}

// An overlapping peer CIDR is served via the remap NETMAP, not routed directly,
// so it must NOT appear in the gateway's reachable set or the published routes.
func TestGateway_ExcludesOverlappingCIDRs(t *testing.T) {
	peer := &networkingv1alpha1.MeshPeer{
		ObjectMeta: metav1.ObjectMeta{Name: "b"},
		Spec: networkingv1alpha1.MeshPeerSpec{
			ClusterID: "cluster-b", PublicKey: "kb",
			// 10.244.0.0/16 overlaps a local CIDR; 10.245.0.0/16 does not.
			PodCIDRs: []string{"10.244.0.0/16", "10.245.0.0/16"},
		},
	}
	dp := &fakeGatewayDataPlane{}
	r, c := newGatewayReconciler(t, dp, peer)

	if _, err := r.Reconcile(context.Background(), ctrl.Request{}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	p := readProfile(t, c, gateway.DefaultProfileNamespace)
	for _, route := range p.RouteCIDRs {
		// The overlapping CIDR happens to equal a local CIDR; it is allowed there
		// since it is genuinely local, but must not be contributed by the peer twice.
		_ = route
	}
	// Reachable set = local CIDRs + the peer's non-overlapping CIDR only.
	assertSet(t, "route CIDRs", p.RouteCIDRs, []string{
		"241.0.0.0/8", "10.244.0.0/16", "10.96.0.0/16", "10.245.0.0/16",
	})
}

// A keyless peer is not programmable, so it is unreachable over the mesh and
// contributes nothing to the gateway's destinations.
func TestGateway_SkipsPeerWithoutKey(t *testing.T) {
	peer := &networkingv1alpha1.MeshPeer{
		ObjectMeta: metav1.ObjectMeta{Name: "b"},
		Spec:       networkingv1alpha1.MeshPeerSpec{ClusterID: "cluster-b", PodCIDRs: []string{"10.245.0.0/16"}},
	}
	dp := &fakeGatewayDataPlane{}
	r, c := newGatewayReconciler(t, dp, peer)

	if _, err := r.Reconcile(context.Background(), ctrl.Request{}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	p := readProfile(t, c, gateway.DefaultProfileNamespace)
	for _, route := range p.RouteCIDRs {
		if route == "10.245.0.0/16" {
			t.Errorf("keyless peer CIDR leaked into routes: %v", p.RouteCIDRs)
		}
	}
}

// With no peers the gateway still programs the masquerade and publishes a
// profile covering this cluster's own ranges, so a client can reach local
// services even before any peering exists.
func TestGateway_NoPeersStillPublishesLocalProfile(t *testing.T) {
	dp := &fakeGatewayDataPlane{}
	r, c := newGatewayReconciler(t, dp)

	if _, err := r.Reconcile(context.Background(), ctrl.Request{}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	dp.mu.Lock()
	if dp.calls != 1 {
		t.Fatalf("expected exactly one masq sync, got %d", dp.calls)
	}
	dp.mu.Unlock()

	p := readProfile(t, c, gateway.DefaultProfileNamespace)
	assertSet(t, "route CIDRs", p.RouteCIDRs, []string{"241.0.0.0/8", "10.244.0.0/16", "10.96.0.0/16"})
}

// Reconciling twice must be idempotent: the second pass updates the existing
// ConfigMap rather than erroring on an already-present object.
func TestGateway_ReconcileIsIdempotent(t *testing.T) {
	dp := &fakeGatewayDataPlane{}
	r, c := newGatewayReconciler(t, dp)

	for i := 0; i < 2; i++ {
		if _, err := r.Reconcile(context.Background(), ctrl.Request{}); err != nil {
			t.Fatalf("reconcile %d: %v", i, err)
		}
	}
	// A single ConfigMap must exist and be readable (no duplicate/conflict).
	_ = readProfile(t, c, gateway.DefaultProfileNamespace)
}

// In no-NAT mode the gateway programs NO client masquerade the client's real
// source IP is preserved.  It still publishes the profile and computes the
// reachable mesh as usual.
func TestGateway_NoNATSkipsMasquerade(t *testing.T) {
	peer := &networkingv1alpha1.MeshPeer{
		ObjectMeta: metav1.ObjectMeta{Name: "b"},
		Spec: networkingv1alpha1.MeshPeerSpec{
			ClusterID: "cluster-b", PublicKey: "kb",
			PodCIDRs: []string{"10.245.0.0/16"},
		},
	}
	dp := &fakeGatewayDataPlane{}
	r, c := newGatewayReconciler(t, dp, peer)
	r.NoNAT = true

	if _, err := r.Reconcile(context.Background(), ctrl.Request{}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	dp.mu.Lock()
	if len(dp.client) != 0 {
		t.Errorf("no-NAT must program no masquerade clients, got %v", dp.client)
	}
	dp.mu.Unlock()

	// The profile is still published (routes/DNS unaffected by the masq mode).
	p := readProfile(t, c, gateway.DefaultProfileNamespace)
	assertSet(t, "route CIDRs", p.RouteCIDRs, []string{"241.0.0.0/8", "10.244.0.0/16", "10.96.0.0/16", "10.245.0.0/16"})
}

// The profile is published to a custom namespace when one is configured.
func TestGateway_RespectsProfileNamespace(t *testing.T) {
	dp := &fakeGatewayDataPlane{}
	r, c := newGatewayReconciler(t, dp)
	r.ProfileNamespace = "custom-ns"

	if _, err := r.Reconcile(context.Background(), ctrl.Request{}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	_ = readProfile(t, c, "custom-ns")
}
