package controllers_test

import (
	"context"
	"sync"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	networkingv1alpha1 "github.com/datawerx/datawerx/pkg/apis/networking/v1alpha1"
	"github.com/datawerx/datawerx/pkg/controllers"
)

type fakeMasqDataPlane struct {
	mu     sync.Mutex
	local  []string
	remote []string
	calls  int
}

func (f *fakeMasqDataPlane) SyncMeshNoMasq(local, remote []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.local = append([]string(nil), local...)
	f.remote = append([]string(nil), remote...)
	f.calls++
	return nil
}

func newMasqReconciler(t *testing.T, dp controllers.MasqExemptDataPlane, local []string, objs ...*networkingv1alpha1.MeshPeer) *controllers.MasqExemptReconciler {
	t.Helper()
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = networkingv1alpha1.AddToScheme(scheme)
	b := fake.NewClientBuilder().WithScheme(scheme)
	for _, o := range objs {
		b = b.WithObjects(o)
	}
	return &controllers.MasqExemptReconciler{
		Client: b.Build(), Scheme: scheme, DataPlane: dp, LocalCIDRs: local,
	}
}

// A directly-routed (non-overlapping) peer: its pod+service CIDRs must be
// exempted from masquerade so cross-cluster return traffic keeps its real source.
func TestMasqExempt_ExemptsRoutedRemoteCIDRs(t *testing.T) {
	peer := &networkingv1alpha1.MeshPeer{
		ObjectMeta: metav1.ObjectMeta{Name: "b"},
		Spec: networkingv1alpha1.MeshPeerSpec{
			ClusterID: "cluster-b", PublicKey: "kb",
			PodCIDRs:     []string{"10.245.0.0/16"},
			ServiceCIDRs: []string{"10.97.0.0/16"},
		},
	}
	dp := &fakeMasqDataPlane{}
	r := newMasqReconciler(t, dp, []string{"10.244.0.0/16", "10.96.0.0/16"}, peer)

	if _, err := r.Reconcile(context.Background(), ctrl.Request{}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	dp.mu.Lock()
	defer dp.mu.Unlock()
	assertSet(t, "local", dp.local, []string{"10.244.0.0/16", "10.96.0.0/16"})
	assertSet(t, "remote", dp.remote, []string{"10.245.0.0/16", "10.97.0.0/16"})
}

// An overlapping peer is handled by the remap NETMAP, not by exemption, so its
// conflicting CIDRs must NOT appear in the no-masq set, which would pre-empt the
// source translation. Only the non-overlapping CIDR is exempted.
func TestMasqExempt_ExcludesOverlappingCIDRs(t *testing.T) {
	peer := &networkingv1alpha1.MeshPeer{
		ObjectMeta: metav1.ObjectMeta{Name: "b"},
		Spec: networkingv1alpha1.MeshPeerSpec{
			ClusterID: "cluster-b", PublicKey: "kb",
			PodCIDRs: []string{"10.244.0.0/16", "10.60.0.0/16"}, // first overlaps local
		},
	}
	dp := &fakeMasqDataPlane{}
	r := newMasqReconciler(t, dp, []string{"10.244.0.0/16"}, peer)

	if _, err := r.Reconcile(context.Background(), ctrl.Request{}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	dp.mu.Lock()
	defer dp.mu.Unlock()
	assertSet(t, "remote", dp.remote, []string{"10.60.0.0/16"})
}

// Two peers contributing overlapping and distinct remote CIDRs: the exemption
// set is the de-duplicated union - a CIDR shared by two peers appears once.
func TestMasqExempt_DedupesAcrossPeers(t *testing.T) {
	p1 := &networkingv1alpha1.MeshPeer{
		ObjectMeta: metav1.ObjectMeta{Name: "b"},
		Spec: networkingv1alpha1.MeshPeerSpec{
			ClusterID: "cluster-b", PublicKey: "kb",
			PodCIDRs: []string{"10.245.0.0/16", "10.250.0.0/16"},
		},
	}
	p2 := &networkingv1alpha1.MeshPeer{
		ObjectMeta: metav1.ObjectMeta{Name: "c"},
		Spec: networkingv1alpha1.MeshPeerSpec{
			ClusterID: "cluster-c", PublicKey: "kc",
			PodCIDRs: []string{"10.245.0.0/16", "10.251.0.0/16"}, // 10.245 shared with p1
		},
	}
	dp := &fakeMasqDataPlane{}
	r := newMasqReconciler(t, dp, []string{"10.244.0.0/16"}, p1, p2)

	if _, err := r.Reconcile(context.Background(), ctrl.Request{}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	dp.mu.Lock()
	defer dp.mu.Unlock()
	assertSet(t, "remote", dp.remote, []string{"10.245.0.0/16", "10.250.0.0/16", "10.251.0.0/16"})
}

// With no peers, the reconciler must still call the data plane with an empty
// remote set so the exemption chain is cleared; teardown-on-empty. A reconciler
// that simply returned early on zero peers would leave stale ACCEPT rules behind.
func TestMasqExempt_NoPeersClearsExemption(t *testing.T) {
	dp := &fakeMasqDataPlane{}
	r := newMasqReconciler(t, dp, []string{"10.244.0.0/16"})

	if _, err := r.Reconcile(context.Background(), ctrl.Request{}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	dp.mu.Lock()
	defer dp.mu.Unlock()
	if dp.calls != 1 {
		t.Fatalf("expected exactly one sync call (to clear), got %d", dp.calls)
	}
	if len(dp.remote) != 0 {
		t.Errorf("expected empty remote set to clear the chain, got %v", dp.remote)
	}
	assertSet(t, "local", dp.local, []string{"10.244.0.0/16"})
}

// IPv6 CIDRs must flow through unmangled. The family split happens in the data
// plane, not the reconciler, and a non-overlapping v6 remote is exempted.
func TestMasqExempt_PassesThroughIPv6(t *testing.T) {
	peer := &networkingv1alpha1.MeshPeer{
		ObjectMeta: metav1.ObjectMeta{Name: "b"},
		Spec: networkingv1alpha1.MeshPeerSpec{
			ClusterID: "cluster-b", PublicKey: "kb",
			PodCIDRs: []string{"10.245.0.0/16", "fd00:beef::/64"},
		},
	}
	dp := &fakeMasqDataPlane{}
	r := newMasqReconciler(t, dp, []string{"10.244.0.0/16", "fd00:aaaa::/64"}, peer)

	if _, err := r.Reconcile(context.Background(), ctrl.Request{}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	dp.mu.Lock()
	defer dp.mu.Unlock()
	assertSet(t, "remote", dp.remote, []string{"10.245.0.0/16", "fd00:beef::/64"})
	assertSet(t, "local", dp.local, []string{"10.244.0.0/16", "fd00:aaaa::/64"})
}

// A peer without a public key is not programmable, so it contributes nothing.
func TestMasqExempt_SkipsPeerWithoutKey(t *testing.T) {
	peer := &networkingv1alpha1.MeshPeer{
		ObjectMeta: metav1.ObjectMeta{Name: "b"},
		Spec:       networkingv1alpha1.MeshPeerSpec{ClusterID: "cluster-b", PodCIDRs: []string{"10.245.0.0/16"}},
	}
	dp := &fakeMasqDataPlane{}
	r := newMasqReconciler(t, dp, []string{"10.244.0.0/16"}, peer)

	if _, err := r.Reconcile(context.Background(), ctrl.Request{}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	dp.mu.Lock()
	defer dp.mu.Unlock()
	if len(dp.remote) != 0 {
		t.Errorf("expected no remote CIDRs for keyless peer, got %v", dp.remote)
	}
}

func assertSet(t *testing.T, label string, got, want []string) {
	t.Helper()
	set := map[string]bool{}
	for _, g := range got {
		set[g] = true
	}
	if len(got) != len(want) {
		t.Fatalf("%s = %v, want exactly %v", label, got, want)
	}
	for _, w := range want {
		if !set[w] {
			t.Errorf("%s missing %q (got %v)", label, w, got)
		}
	}
}
