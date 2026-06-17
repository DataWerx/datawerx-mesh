package controllers_test

import (
	"context"
	"strings"
	"sync"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	networkingv1alpha1 "github.com/DataWerx/datawerx-mesh/pkg/apis/networking/v1alpha1"
	"github.com/DataWerx/datawerx-mesh/pkg/controllers"
	"github.com/DataWerx/datawerx-mesh/pkg/meshfw"
)

type fakeFirewall struct {
	mu   sync.Mutex
	last meshfw.Ruleset
	err  error
}

func (f *fakeFirewall) SyncFirewall(rs meshfw.Ruleset) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.last = rs
	return nil
}

func (f *fakeFirewall) snapshot() meshfw.Ruleset {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.last
}

func newPolicyReconciler(t *testing.T, dp controllers.FirewallDataPlane, objs ...client.Object) (*controllers.MeshNetworkPolicyReconciler, client.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	if err := networkingv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		WithStatusSubresource(&networkingv1alpha1.MeshNetworkPolicy{}).
		Build()
	return &controllers.MeshNetworkPolicyReconciler{Client: c, Scheme: scheme, DataPlane: dp}, c
}

func fwContains(rs meshfw.Ruleset, substr string) bool {
	for _, r := range rs.Rules {
		if strings.Contains(strings.Join(r.Args, " "), substr) {
			return true
		}
	}
	return false
}

func TestMeshNetworkPolicy_CompilesAndResolvesPeers(t *testing.T) {
	peer := &networkingv1alpha1.MeshPeer{
		ObjectMeta: metav1.ObjectMeta{Name: "b"},
		Spec:       networkingv1alpha1.MeshPeerSpec{ClusterID: "cluster-b", PublicKey: "kb", PodCIDRs: []string{"10.40.0.0/16"}},
	}
	pol := &networkingv1alpha1.MeshNetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "db", Generation: 1},
		Spec: networkingv1alpha1.MeshNetworkPolicySpec{
			Destinations: []string{"10.96.5.0/24"},
			Ingress: []networkingv1alpha1.MeshIngressRule{{
				From:  []networkingv1alpha1.MeshPeerSelector{{ClusterIDs: []string{"cluster-b"}}},
				Ports: []networkingv1alpha1.MeshNetworkPolicyPort{{Protocol: "TCP", Port: 5432}},
			}},
		},
	}
	dp := &fakeFirewall{}
	r, c := newPolicyReconciler(t, dp, peer, pol)

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "db"}}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	rs := dp.snapshot()
	if !fwContains(rs, "-s 10.40.0.0/16 -d 10.96.5.0/24 -p tcp -m tcp --dport 5432 -j ACCEPT") {
		t.Errorf("expected peer-resolved allow rule; got rules %d", len(rs.Rules))
	}

	var got networkingv1alpha1.MeshNetworkPolicy
	_ = c.Get(context.Background(), types.NamespacedName{Name: "db"}, &got)
	if got.Status.Phase != networkingv1alpha1.MeshNetworkPolicyPhaseReady {
		t.Errorf("phase = %q, want Ready", got.Status.Phase)
	}
	if got.Status.ObservedGeneration != 1 {
		t.Errorf("observedGeneration = %d, want 1", got.Status.ObservedGeneration)
	}
}

func TestMeshNetworkPolicy_SyncErrorSetsErrorPhase(t *testing.T) {
	pol := &networkingv1alpha1.MeshNetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Generation: 1},
		Spec:       networkingv1alpha1.MeshNetworkPolicySpec{Destinations: []string{"10.96.0.0/16"}},
	}
	dp := &fakeFirewall{err: errContext("iptables down")}
	r, c := newPolicyReconciler(t, dp, pol)

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "p"}}); err == nil {
		t.Fatal("expected reconcile error when data plane fails")
	}
	var got networkingv1alpha1.MeshNetworkPolicy
	_ = c.Get(context.Background(), types.NamespacedName{Name: "p"}, &got)
	if got.Status.Phase != networkingv1alpha1.MeshNetworkPolicyPhaseError {
		t.Errorf("phase = %q, want Error", got.Status.Phase)
	}
}

func TestMeshNetworkPolicy_ReportsSkippedV6(t *testing.T) {
	pol := &networkingv1alpha1.MeshNetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "v6", Generation: 1},
		Spec: networkingv1alpha1.MeshNetworkPolicySpec{
			Destinations: []string{"fd00::/64", "10.96.0.0/16"},
		},
	}
	dp := &fakeFirewall{}
	r, c := newPolicyReconciler(t, dp, pol)
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "v6"}}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	var got networkingv1alpha1.MeshNetworkPolicy
	_ = c.Get(context.Background(), types.NamespacedName{Name: "v6"}, &got)
	if got.Status.Phase != networkingv1alpha1.MeshNetworkPolicyPhaseReady {
		t.Errorf("phase = %q, want Ready", got.Status.Phase)
	}
	if !strings.Contains(got.Status.Message, "fd00::/64") {
		t.Errorf("status should report the skipped v6 input, got %q", got.Status.Message)
	}
}

// errContext is a tiny error helper to avoid importing errors just here.
type errContext string

func (e errContext) Error() string { return string(e) }
