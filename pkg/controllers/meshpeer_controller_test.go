package controllers_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	networkingv1alpha1 "github.com/datawerx/datawerx/pkg/apis/networking/v1alpha1"
	"github.com/datawerx/datawerx/pkg/controllers"
)

// fakeDataPlane is an in-memory stand-in for the WireGuard manager so the
// reconciler can be tested without touching the kernel.
type fakeDataPlane struct {
	mu             sync.Mutex
	configureCalls []configureCall
	removed        []string
	handshake      int64
	handshakeErr   error
	configureErr   error
}

type configureCall struct {
	key        string
	endpoint   string
	allowedIPs []string
}

func (f *fakeDataPlane) ConfigurePeer(key, endpoint string, allowedIPs []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.configureErr != nil {
		return f.configureErr
	}
	f.configureCalls = append(f.configureCalls, configureCall{key, endpoint, append([]string(nil), allowedIPs...)})
	return nil
}

func (f *fakeDataPlane) RemovePeer(key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removed = append(f.removed, key)
	return nil
}

func (f *fakeDataPlane) PeerHandshake(key string) (int64, error) {
	return f.handshake, f.handshakeErr
}

func (f *fakeDataPlane) lastConfigure() (configureCall, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.configureCalls) == 0 {
		return configureCall{}, false
	}
	return f.configureCalls[len(f.configureCalls)-1], true
}

const testFinalizer = "networking.datawerx.io/meshpeer-cleanup"

func newReconciler(t *testing.T, dp controllers.PeerDataPlane, localCIDRs []string, objs ...client.Object) (*controllers.MeshPeerReconciler, client.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := networkingv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	fc := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		WithStatusSubresource(&networkingv1alpha1.MeshPeer{}).
		Build()
	r := &controllers.MeshPeerReconciler{
		Client:     fc,
		Scheme:     scheme,
		DataPlane:  dp,
		LocalCIDRs: localCIDRs,
	}
	return r, fc
}

func reconcile(t *testing.T, r *controllers.MeshPeerReconciler, name string) ctrl.Result {
	t.Helper()
	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: name}})
	if err != nil {
		t.Fatalf("Reconcile(%s) unexpected error: %v", name, err)
	}
	return res
}

func getPeer(t *testing.T, c client.Client, name string) *networkingv1alpha1.MeshPeer {
	t.Helper()
	var mp networkingv1alpha1.MeshPeer
	if err := c.Get(context.Background(), types.NamespacedName{Name: name}, &mp); err != nil {
		t.Fatalf("get %s: %v", name, err)
	}
	return &mp
}

func TestReconcile_AddsFinalizerThenConfigures(t *testing.T) {
	peer := &networkingv1alpha1.MeshPeer{
		ObjectMeta: metav1.ObjectMeta{Name: "alpha"},
		Spec: networkingv1alpha1.MeshPeerSpec{
			ClusterID: "alpha", PublicKey: "ka", Endpoint: "1.2.3.4:51820",
			PodCIDRs: []string{"10.50.0.0/16"},
		},
	}
	dp := &fakeDataPlane{handshake: 1717000000}
	r, c := newReconciler(t, dp, []string{"10.244.0.0/16"}, peer)

	// First pass installs the finalizer and returns without programming.
	reconcile(t, r, "alpha")
	if _, ok := dp.lastConfigure(); ok {
		t.Fatal("did not expect ConfigurePeer before finalizer is set")
	}
	after := getPeer(t, c, "alpha")
	if len(after.Finalizers) == 0 || after.Finalizers[0] != testFinalizer {
		t.Fatalf("expected finalizer to be added, got %v", after.Finalizers)
	}

	// Second pass programs the data plane and writes status.
	reconcile(t, r, "alpha")
	call, ok := dp.lastConfigure()
	if !ok {
		t.Fatal("expected ConfigurePeer to be called")
	}
	if call.key != "ka" || call.endpoint != "1.2.3.4:51820" {
		t.Errorf("ConfigurePeer called with key=%q endpoint=%q", call.key, call.endpoint)
	}
	if len(call.allowedIPs) != 1 || call.allowedIPs[0] != "10.50.0.0/16" {
		t.Errorf("ConfigurePeer allowedIPs = %v, want [10.50.0.0/16]", call.allowedIPs)
	}

	final := getPeer(t, c, "alpha")
	if final.Status.Phase != networkingv1alpha1.MeshPeerPhaseConnected {
		t.Errorf("phase = %q, want Connected", final.Status.Phase)
	}
	if final.Status.LastHandshakeTime != 1717000000 {
		t.Errorf("handshake = %d, want 1717000000", final.Status.LastHandshakeTime)
	}
}

func TestReconcile_HandshakeReadErrorKeepsLastValue(t *testing.T) {
	peer := &networkingv1alpha1.MeshPeer{
		ObjectMeta: metav1.ObjectMeta{Name: "gamma", Finalizers: []string{testFinalizer}},
		Spec: networkingv1alpha1.MeshPeerSpec{
			ClusterID: "gamma", PublicKey: "kg", Endpoint: "5.6.7.8:51820",
			PodCIDRs: []string{"10.70.0.0/16"},
		},
		Status: networkingv1alpha1.MeshPeerStatus{
			Phase: networkingv1alpha1.MeshPeerPhaseConnected, LastHandshakeTime: 1717000000,
		},
	}
	// A transient read failure must not clobber the good timestamp with 0.
	dp := &fakeDataPlane{handshake: 0, handshakeErr: errors.New("netlink boom")}
	r, c := newReconciler(t, dp, nil, peer)

	res := reconcile(t, r, "gamma")
	if res.RequeueAfter == 0 {
		t.Error("expected RequeueAfter so LastHandshakeTime auto-refreshes")
	}
	final := getPeer(t, c, "gamma")
	if final.Status.LastHandshakeTime != 1717000000 {
		t.Errorf("handshake = %d, want 1717000000 preserved on read error", final.Status.LastHandshakeTime)
	}
}

func TestReconcile_OverlapWithholdsConflictingCIDRs(t *testing.T) {
	peer := &networkingv1alpha1.MeshPeer{
		ObjectMeta: metav1.ObjectMeta{Name: "beta", Finalizers: []string{testFinalizer}},
		Spec: networkingv1alpha1.MeshPeerSpec{
			ClusterID: "beta", PublicKey: "kb",
			PodCIDRs: []string{"10.244.0.0/16", "10.60.0.0/16"},
		},
	}
	dp := &fakeDataPlane{}
	r, c := newReconciler(t, dp, []string{"10.244.0.0/16"}, peer)

	reconcile(t, r, "beta")

	call, ok := dp.lastConfigure()
	if !ok {
		t.Fatal("expected ConfigurePeer to be called")
	}
	if len(call.allowedIPs) != 1 || call.allowedIPs[0] != "10.60.0.0/16" {
		t.Errorf("expected only non-conflicting CIDR routed, got %v", call.allowedIPs)
	}
	final := getPeer(t, c, "beta")
	if final.Status.Phase != networkingv1alpha1.MeshPeerPhaseError {
		t.Errorf("phase = %q, want Error (overlap)", final.Status.Phase)
	}
}

func TestReconcile_MissingPublicKeyReturnsError(t *testing.T) {
	peer := &networkingv1alpha1.MeshPeer{
		ObjectMeta: metav1.ObjectMeta{Name: "gamma", Finalizers: []string{testFinalizer}},
		Spec:       networkingv1alpha1.MeshPeerSpec{ClusterID: "gamma"},
	}
	dp := &fakeDataPlane{}
	r, c := newReconciler(t, dp, nil, peer)

	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "gamma"}})
	if err == nil {
		t.Fatal("expected error for missing public key")
	}
	if _, ok := dp.lastConfigure(); ok {
		t.Error("ConfigurePeer should not be called for an invalid spec")
	}
	final := getPeer(t, c, "gamma")
	if final.Status.Phase != networkingv1alpha1.MeshPeerPhaseError {
		t.Errorf("phase = %q, want Error", final.Status.Phase)
	}
}

func TestReconcile_ConfigureFailureSetsErrorStatus(t *testing.T) {
	peer := &networkingv1alpha1.MeshPeer{
		ObjectMeta: metav1.ObjectMeta{Name: "delta", Finalizers: []string{testFinalizer}},
		Spec: networkingv1alpha1.MeshPeerSpec{
			ClusterID: "delta", PublicKey: "kd", PodCIDRs: []string{"10.70.0.0/16"},
		},
	}
	dp := &fakeDataPlane{configureErr: errors.New("netlink boom")}
	r, c := newReconciler(t, dp, nil, peer)

	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "delta"}})
	if err == nil {
		t.Fatal("expected error when ConfigurePeer fails")
	}
	final := getPeer(t, c, "delta")
	if final.Status.Phase != networkingv1alpha1.MeshPeerPhaseError {
		t.Errorf("phase = %q, want Error", final.Status.Phase)
	}
}

func TestReconcile_DeletionTearsDownAndRemovesFinalizer(t *testing.T) {
	now := metav1.Now()
	peer := &networkingv1alpha1.MeshPeer{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "epsilon",
			Finalizers:        []string{testFinalizer},
			DeletionTimestamp: &now,
		},
		Spec: networkingv1alpha1.MeshPeerSpec{ClusterID: "epsilon", PublicKey: "ke"},
	}
	dp := &fakeDataPlane{}
	r, c := newReconciler(t, dp, nil, peer)

	reconcile(t, r, "epsilon")

	dp.mu.Lock()
	removed := append([]string(nil), dp.removed...)
	dp.mu.Unlock()
	if len(removed) != 1 || removed[0] != "ke" {
		t.Fatalf("expected RemovePeer(ke), got %v", removed)
	}

	// Once the finalizer is removed the object is fully deleted.
	var mp networkingv1alpha1.MeshPeer
	err := c.Get(context.Background(), types.NamespacedName{Name: "epsilon"}, &mp)
	if err == nil {
		t.Errorf("expected object to be gone after finalizer removal, still present with finalizers %v", mp.Finalizers)
	}
}

func TestReconcile_NotFoundRemovesCachedPeer(t *testing.T) {
	// Program a peer so the reconciler caches its key.
	peer := &networkingv1alpha1.MeshPeer{
		ObjectMeta: metav1.ObjectMeta{Name: "zeta", Finalizers: []string{testFinalizer}},
		Spec: networkingv1alpha1.MeshPeerSpec{
			ClusterID: "zeta", PublicKey: "kz", PodCIDRs: []string{"10.80.0.0/16"},
		},
	}
	dp := &fakeDataPlane{}
	r, c := newReconciler(t, dp, nil, peer)

	reconcile(t, r, "zeta") // caches key "kz"

	// Hard-delete the object out from under the reconciler (simulating a peer
	// that vanished without going through finalization).
	if err := c.Delete(context.Background(), peer); err != nil {
		t.Fatalf("delete: %v", err)
	}
	// The object still has a finalizer, so remove it to force true NotFound.
	fresh := getPeer(t, c, "zeta")
	fresh.Finalizers = nil
	if err := c.Update(context.Background(), fresh); err != nil {
		t.Fatalf("clearing finalizer: %v", err)
	}

	reconcile(t, r, "zeta") // now NotFound

	dp.mu.Lock()
	removed := append([]string(nil), dp.removed...)
	dp.mu.Unlock()
	found := false
	for _, k := range removed {
		if k == "kz" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected RemovePeer(kz) on NotFound, got %v", removed)
	}
}

func TestReconcile_NotFoundUnknownPeerIsNoOp(t *testing.T) {
	dp := &fakeDataPlane{}
	r, _ := newReconciler(t, dp, nil)

	reconcile(t, r, "never-existed")

	dp.mu.Lock()
	defer dp.mu.Unlock()
	if len(dp.removed) != 0 {
		t.Errorf("expected no RemovePeer calls for unknown peer, got %v", dp.removed)
	}
}

func TestReconcile_KeyRotationRemovesStalePeer(t *testing.T) {
	peer := &networkingv1alpha1.MeshPeer{
		ObjectMeta: metav1.ObjectMeta{Name: "rot", Finalizers: []string{testFinalizer}},
		Spec: networkingv1alpha1.MeshPeerSpec{
			ClusterID: "rot", PublicKey: "key-old", PodCIDRs: []string{"10.60.0.0/16"},
		},
	}
	dp := &fakeDataPlane{}
	r, c := newReconciler(t, dp, nil, peer)

	// First reconcile programs key-old and caches it.
	reconcile(t, r, "rot")
	if call, ok := dp.lastConfigure(); !ok || call.key != "key-old" {
		t.Fatalf("expected ConfigurePeer(key-old), got %+v", call)
	}

	// Rotate the public key on the spec.
	fresh := getPeer(t, c, "rot")
	fresh.Spec.PublicKey = "key-new"
	if err := c.Update(context.Background(), fresh); err != nil {
		t.Fatalf("update: %v", err)
	}

	reconcile(t, r, "rot")

	dp.mu.Lock()
	removed := append([]string(nil), dp.removed...)
	dp.mu.Unlock()
	foundOld := false
	for _, k := range removed {
		if k == "key-old" {
			foundOld = true
		}
	}
	if !foundOld {
		t.Errorf("expected stale key-old to be removed on rotation, removed=%v", removed)
	}
	if call, ok := dp.lastConfigure(); !ok || call.key != "key-new" {
		t.Errorf("expected ConfigurePeer(key-new) after rotation, got %+v", call)
	}
}

// After a node reboot the kernel data plane and the agent's in-memory
// keyIndex are gone, but the MeshPeer CRD persists. A fresh agent must
// re-program the peer from the CRD on the next reconcile - full-state recovery
// with no manual intervention.
func TestReconcile_RecoversAfterRestart(t *testing.T) {
	peer := &networkingv1alpha1.MeshPeer{
		ObjectMeta: metav1.ObjectMeta{Name: "alpha", Finalizers: []string{testFinalizer}},
		Spec: networkingv1alpha1.MeshPeerSpec{
			ClusterID: "alpha", PublicKey: "ka", Endpoint: "1.2.3.4:51820",
			PodCIDRs: []string{"10.50.0.0/16"},
		},
	}
	dp1 := &fakeDataPlane{}
	r1, c := newReconciler(t, dp1, []string{"10.244.0.0/16"}, peer)

	reconcile(t, r1, "alpha") // finalizer already present → programs immediately
	if call, ok := dp1.lastConfigure(); !ok || call.key != "ka" {
		t.Fatalf("expected initial ConfigurePeer(ka), got %+v ok=%v", call, ok)
	}

	// Simulate a reboot: brand-new agent - empty kernel with empty keyIndex, same
	// persisted CRD via the same client.
	scheme := runtime.NewScheme()
	if err := networkingv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	dp2 := &fakeDataPlane{}
	r2 := &controllers.MeshPeerReconciler{
		Client: c, Scheme: scheme, DataPlane: dp2, LocalCIDRs: []string{"10.244.0.0/16"},
	}

	reconcile(t, r2, "alpha")
	call, ok := dp2.lastConfigure()
	if !ok {
		t.Fatal("fresh agent must re-program the peer from the CRD after a restart")
	}
	if call.key != "ka" || len(call.allowedIPs) != 1 || call.allowedIPs[0] != "10.50.0.0/16" {
		t.Errorf("recovery ConfigurePeer = %+v, want key=ka allowedIPs=[10.50.0.0/16]", call)
	}
}
