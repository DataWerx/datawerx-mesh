package controllers_test

import (
	"context"
	"sync"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/prometheus/client_golang/prometheus/testutil"

	networkingv1alpha1 "github.com/datawerx/datawerx/pkg/apis/networking/v1alpha1"
	"github.com/datawerx/datawerx/pkg/controllers"
	dwxmetrics "github.com/datawerx/datawerx/pkg/metrics"
	"github.com/datawerx/datawerx/pkg/nat"
	"github.com/datawerx/datawerx/pkg/topology"
)

// TestReconcile_OverlapRemapRoutesVirtual: with remap enabled, an overlapping
// peer becomes Connected and the conflicting CIDR is routed under its virtual
// range instead of being refused.
func TestReconcile_OverlapRemapRoutesVirtual(t *testing.T) {
	peer := &networkingv1alpha1.MeshPeer{
		ObjectMeta: metav1.ObjectMeta{Name: "beta", Finalizers: []string{testFinalizer}},
		Spec: networkingv1alpha1.MeshPeerSpec{
			ClusterID: "cluster-b", PublicKey: "kb",
			PodCIDRs: []string{"10.244.0.0/16", "10.60.0.0/16"},
		},
	}
	dp := &fakeDataPlane{}
	r, c := newReconciler(t, dp, []string{"10.244.0.0/16"}, peer)
	r.ClusterID = "cluster-a"
	r.RemapPool = "172.16.0.0/12"

	reconcile(t, r, "beta")

	call, ok := dp.lastConfigure()
	if !ok {
		t.Fatal("expected ConfigurePeer")
	}
	wantVirtual, _ := topology.VirtualCIDR("172.16.0.0/12", "cluster-b", "10.244.0.0/16")
	has := func(s string) bool {
		for _, c := range call.allowedIPs {
			if c == s {
				return true
			}
		}
		return false
	}
	if !has("10.60.0.0/16") {
		t.Errorf("expected the non-conflicting CIDR routed, got %v", call.allowedIPs)
	}
	if !has(wantVirtual) {
		t.Errorf("expected the overlapping CIDR routed under virtual %s, got %v", wantVirtual, call.allowedIPs)
	}
	if has("10.244.0.0/16") {
		t.Errorf("the real overlapping CIDR must NOT be routed directly, got %v", call.allowedIPs)
	}

	final := getPeer(t, c, "beta")
	if final.Status.Phase != networkingv1alpha1.MeshPeerPhaseConnected {
		t.Errorf("phase = %q, want Connected (remapped)", final.Status.Phase)
	}
}

type fakeRemapDataPlane struct {
	mu   sync.Mutex
	last []nat.RemapEntry
}

func (f *fakeRemapDataPlane) SyncRemap(e []nat.RemapEntry) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.last = append([]nat.RemapEntry(nil), e...)
	return nil
}

func TestRemapReconciler_BuildsLocalNETMAP(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = networkingv1alpha1.AddToScheme(scheme)
	peer := &networkingv1alpha1.MeshPeer{
		ObjectMeta: metav1.ObjectMeta{Name: "b"},
		Spec:       networkingv1alpha1.MeshPeerSpec{ClusterID: "cluster-b", PublicKey: "kb", PodCIDRs: []string{"10.244.0.0/16"}},
	}
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(peer).Build()
	dp := &fakeRemapDataPlane{}
	r := &controllers.RemapReconciler{
		Client: fc, Scheme: scheme, DataPlane: dp,
		ClusterID: "cluster-a", RemapPool: "172.16.0.0/12", LocalCIDRs: []string{"10.244.0.0/16"},
	}

	if _, err := r.Reconcile(context.Background(), ctrl.Request{}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	dp.mu.Lock()
	defer dp.mu.Unlock()
	if len(dp.last) != 1 {
		t.Fatalf("expected 1 NETMAP entry, got %#v", dp.last)
	}
	wantVirtual, _ := topology.VirtualCIDR("172.16.0.0/12", "cluster-a", "10.244.0.0/16")
	if dp.last[0].Real != "10.244.0.0/16" || dp.last[0].Virtual != wantVirtual {
		t.Errorf("entry = %#v, want {10.244.0.0/16, %s}", dp.last[0], wantVirtual)
	}
}

// A peer advertising a dangerous range must never be remapped.
// It is purely rejected and never translated into the mesh.
func TestRemapReconciler_ExcludesDangerousRanges(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = networkingv1alpha1.AddToScheme(scheme)
	peer := &networkingv1alpha1.MeshPeer{
		ObjectMeta: metav1.ObjectMeta{Name: "evil"},
		Spec:       networkingv1alpha1.MeshPeerSpec{ClusterID: "evil", PublicKey: "k", PodCIDRs: []string{"0.0.0.0/0"}},
	}
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(peer).Build()
	dp := &fakeRemapDataPlane{}
	r := &controllers.RemapReconciler{
		Client: fc, Scheme: scheme, DataPlane: dp,
		ClusterID: "cluster-a", RemapPool: "172.16.0.0/12", LocalCIDRs: nil,
	}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	dp.mu.Lock()
	defer dp.mu.Unlock()
	if len(dp.last) != 0 {
		t.Errorf("a default route must not be remapped, got %#v", dp.last)
	}
}

func TestRemapReconciler_SetsMetrics(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = networkingv1alpha1.AddToScheme(scheme)
	peer := &networkingv1alpha1.MeshPeer{
		ObjectMeta: metav1.ObjectMeta{Name: "b"},
		Spec:       networkingv1alpha1.MeshPeerSpec{ClusterID: "cluster-b", PublicKey: "kb", PodCIDRs: []string{"10.244.0.0/16"}},
	}
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(peer).Build()
	r := &controllers.RemapReconciler{
		Client: fc, Scheme: scheme, DataPlane: &fakeRemapDataPlane{},
		ClusterID: "cluster-a", RemapPool: "172.16.0.0/12", LocalCIDRs: []string{"10.244.0.0/16"},
	}

	before := testutil.ToFloat64(dwxmetrics.RemapSyncs.WithLabelValues("success"))
	if _, err := r.Reconcile(context.Background(), ctrl.Request{}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if got := testutil.ToFloat64(dwxmetrics.RemapSyncs.WithLabelValues("success")); got != before+1 {
		t.Errorf("success counter = %v, want %v", got, before+1)
	}
	if got := testutil.ToFloat64(dwxmetrics.RemapEntries); got != 1 {
		t.Errorf("active_entries gauge = %v, want 1", got)
	}
}

// TestRemapReconciler_DetectsCrossPeerVirtualCollision: two local CIDRs that
// overlap different peers, so each is planned in its own PlanRemap call, but
// hash to the same virtual range must be detected globally and fail safe, rather
// than programming an ambiguous NETMAP. A /15 pool holds only two /16 blocks, so
// three distinct local /16s cannot all get unique virtuals (a.k.a. pigeonhole).
func TestRemapReconciler_DetectsCrossPeerVirtualCollision(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = networkingv1alpha1.AddToScheme(scheme)

	// Three peers, each overlapping exactly ONE distinct local /16 — so no single
	// PlanRemap call sees more than one local. The within-call guard can't catch
	// it; only the global view in buildEntries can.
	peers := []*networkingv1alpha1.MeshPeer{
		{ObjectMeta: metav1.ObjectMeta{Name: "x"}, Spec: networkingv1alpha1.MeshPeerSpec{ClusterID: "cluster-x", PublicKey: "kx", PodCIDRs: []string{"10.0.0.0/16"}}},
		{ObjectMeta: metav1.ObjectMeta{Name: "y"}, Spec: networkingv1alpha1.MeshPeerSpec{ClusterID: "cluster-y", PublicKey: "ky", PodCIDRs: []string{"10.1.0.0/16"}}},
		{ObjectMeta: metav1.ObjectMeta{Name: "z"}, Spec: networkingv1alpha1.MeshPeerSpec{ClusterID: "cluster-z", PublicKey: "kz", PodCIDRs: []string{"10.2.0.0/16"}}},
	}
	objs := make([]client.Object, len(peers))
	for i := range peers {
		objs[i] = peers[i]
	}
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	dp := &fakeRemapDataPlane{}
	r := &controllers.RemapReconciler{
		Client: fc, Scheme: scheme, DataPlane: dp,
		ClusterID:  "cluster-a",
		RemapPool:  "172.16.0.0/15", // only two /16 blocks
		LocalCIDRs: []string{"10.0.0.0/16", "10.1.0.0/16", "10.2.0.0/16"},
	}

	if _, err := r.Reconcile(context.Background(), ctrl.Request{}); err == nil {
		t.Fatal("expected a cross-peer virtual collision error for an oversubscribed pool")
	}
	dp.mu.Lock()
	defer dp.mu.Unlock()
	if len(dp.last) != 0 {
		t.Errorf("no NETMAP entries should be programmed when a collision is detected, got %#v", dp.last)
	}
}

// TestRemapReconciler_DetectsCrossPeerRouteCollision: three peers all reusing
// the same overlapping CIDR route under per-cluster virtual ranges that, in a
// 2-block pool, cannot all be unique — so two peers would route the same virtual
// into the mesh device. Must be detected globally and fail safe.
func TestRemapReconciler_DetectsCrossPeerRouteCollision(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = networkingv1alpha1.AddToScheme(scheme)

	peers := []*networkingv1alpha1.MeshPeer{
		{ObjectMeta: metav1.ObjectMeta{Name: "x"}, Spec: networkingv1alpha1.MeshPeerSpec{ClusterID: "cluster-x", PublicKey: "kx", PodCIDRs: []string{"10.0.0.0/16"}}},
		{ObjectMeta: metav1.ObjectMeta{Name: "y"}, Spec: networkingv1alpha1.MeshPeerSpec{ClusterID: "cluster-y", PublicKey: "ky", PodCIDRs: []string{"10.0.0.0/16"}}},
		{ObjectMeta: metav1.ObjectMeta{Name: "z"}, Spec: networkingv1alpha1.MeshPeerSpec{ClusterID: "cluster-z", PublicKey: "kz", PodCIDRs: []string{"10.0.0.0/16"}}},
	}
	objs := make([]client.Object, len(peers))
	for i := range peers {
		objs[i] = peers[i]
	}
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	dp := &fakeRemapDataPlane{}
	r := &controllers.RemapReconciler{
		Client: fc, Scheme: scheme, DataPlane: dp,
		ClusterID:  "cluster-a",
		RemapPool:  "172.16.0.0/15", // two /16 blocks; three peer routes can't be unique
		LocalCIDRs: []string{"10.0.0.0/16"},
	}

	if _, err := r.Reconcile(context.Background(), ctrl.Request{}); err == nil {
		t.Fatal("expected a cross-peer route-virtual collision error")
	}
}

func TestRemapReconciler_NoOverlapNoEntries(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = networkingv1alpha1.AddToScheme(scheme)
	peer := &networkingv1alpha1.MeshPeer{
		ObjectMeta: metav1.ObjectMeta{Name: "b"},
		Spec:       networkingv1alpha1.MeshPeerSpec{ClusterID: "cluster-b", PublicKey: "kb", PodCIDRs: []string{"10.245.0.0/16"}},
	}
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(peer).Build()
	dp := &fakeRemapDataPlane{}
	r := &controllers.RemapReconciler{
		Client: fc, Scheme: scheme, DataPlane: dp,
		ClusterID: "cluster-a", RemapPool: "172.16.0.0/12", LocalCIDRs: []string{"10.244.0.0/16"},
	}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	dp.mu.Lock()
	defer dp.mu.Unlock()
	if len(dp.last) != 0 {
		t.Errorf("expected no entries (no overlap), got %#v", dp.last)
	}
}
