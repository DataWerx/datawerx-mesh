package syncer

import (
	"context"
	"errors"
	"testing"

	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	networkingv1alpha1 "github.com/DataWerx/datawerx-mesh/pkg/apis/networking/v1alpha1"
	dwxclient "github.com/DataWerx/datawerx-mesh/pkg/client"
	"github.com/DataWerx/datawerx-mesh/pkg/topology"
)

// fakeCP is a ControlPlaneClient that also implements RevisionedControlPlane.
type fakeCP struct {
	peers []dwxclient.RemotePeerConfig
	rev   string
	err   error
	calls int
}

func (f *fakeCP) Authenticate(context.Context) error { return nil }

func (f *fakeCP) FetchTopology(ctx context.Context) ([]dwxclient.RemotePeerConfig, error) {
	peers, _, err := f.FetchTopologyWithRevision(ctx)
	return peers, err
}

func (f *fakeCP) FetchTopologyWithRevision(context.Context) ([]dwxclient.RemotePeerConfig, string, error) {
	f.calls++
	return f.peers, f.rev, f.err
}

func newSyncer(t *testing.T, cp dwxclient.ControlPlaneClient, objs ...ctrlclient.Object) (*Syncer, ctrlclient.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := networkingv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	return &Syncer{CP: cp, K8s: c, Log: logr.Discard()}, c
}

func peer(id, key, endpoint string, cidrs ...string) dwxclient.RemotePeerConfig {
	return dwxclient.RemotePeerConfig{ClusterID: id, PublicKey: key, Endpoint: endpoint, PodCIDRs: cidrs}
}

func getPeer(t *testing.T, c ctrlclient.Client, name string) (*networkingv1alpha1.MeshPeer, bool) {
	t.Helper()
	var mp networkingv1alpha1.MeshPeer
	if err := c.Get(context.Background(), types.NamespacedName{Name: name}, &mp); err != nil {
		return nil, false
	}
	return &mp, true
}

func TestSyncer_UpsertsAndLabels(t *testing.T) {
	cp := &fakeCP{peers: []dwxclient.RemotePeerConfig{
		peer("cluster-a", "ka", "1.1.1.1:51820", "10.1.0.0/16"),
		peer("cluster-b", "kb", "2.2.2.2:51820", "10.2.0.0/16"),
	}, rev: "r1"}
	s, c := newSyncer(t, cp)

	s.syncOnce(context.Background())

	for _, name := range []string{"cluster-a", "cluster-b"} {
		mp, ok := getPeer(t, c, name)
		if !ok {
			t.Fatalf("MeshPeer %s not created", name)
		}
		if mp.Labels[managedByLabel] != managedByValue {
			t.Errorf("%s missing managed-by label: %v", name, mp.Labels)
		}
	}
}

func TestSyncer_SkipsSelf(t *testing.T) {
	cp := &fakeCP{peers: []dwxclient.RemotePeerConfig{
		peer("self", "ks", "1.1.1.1:1"),
		peer("other", "ko", "2.2.2.2:1"),
	}}
	s, c := newSyncer(t, cp)
	s.LocalClusterID = "self"

	s.syncOnce(context.Background())

	if _, ok := getPeer(t, c, "self"); ok {
		t.Error("expected the local cluster to be skipped, but a MeshPeer was created for it")
	}
	if _, ok := getPeer(t, c, "other"); !ok {
		t.Error("expected MeshPeer for 'other'")
	}
}

func TestSyncer_PrunesStaleButKeepsForeign(t *testing.T) {
	// A previously-synced peer this syncer owns…
	managedStale := &networkingv1alpha1.MeshPeer{
		ObjectMeta: metav1.ObjectMeta{Name: "old", Labels: map[string]string{managedByLabel: managedByValue}},
		Spec:       networkingv1alpha1.MeshPeerSpec{ClusterID: "old", PublicKey: "kold"},
	}
	// …and a peer authored by a human / GitOps (no managed-by label).
	foreign := &networkingv1alpha1.MeshPeer{
		ObjectMeta: metav1.ObjectMeta{Name: "human"},
		Spec:       networkingv1alpha1.MeshPeerSpec{ClusterID: "human", PublicKey: "khuman"},
	}
	cp := &fakeCP{peers: []dwxclient.RemotePeerConfig{peer("cluster-a", "ka", "1.1.1.1:1")}}
	s, c := newSyncer(t, cp, managedStale, foreign)

	s.syncOnce(context.Background())

	if _, ok := getPeer(t, c, "old"); ok {
		t.Error("stale managed MeshPeer should have been pruned")
	}
	if _, ok := getPeer(t, c, "human"); !ok {
		t.Error("foreign (unlabeled) MeshPeer must never be pruned")
	}
	if _, ok := getPeer(t, c, "cluster-a"); !ok {
		t.Error("current peer should have been created")
	}
}

func TestSyncer_RevisionShortCircuit(t *testing.T) {
	cp := &fakeCP{peers: []dwxclient.RemotePeerConfig{peer("cluster-a", "ka", "1.1.1.1:1")}, rev: "r1"}
	s, c := newSyncer(t, cp)

	s.syncOnce(context.Background()) // applies r1, creates cluster-a

	// Simulate drift: delete the peer out from under the syncer.
	mp, _ := getPeer(t, c, "cluster-a")
	if err := c.Delete(context.Background(), mp); err != nil {
		t.Fatalf("delete: %v", err)
	}

	// Same revision → the syncer must short-circuit and NOT recreate it.
	s.syncOnce(context.Background())
	if _, ok := getPeer(t, c, "cluster-a"); ok {
		t.Error("unchanged revision should have been skipped, but peer was reconciled")
	}

	// A new revision → the syncer converges again and recreates it.
	cp.rev = "r2"
	s.syncOnce(context.Background())
	if _, ok := getPeer(t, c, "cluster-a"); !ok {
		t.Error("new revision should have recreated the peer")
	}
}

func TestSyncer_FetchErrorDoesNotPrune(t *testing.T) {
	managed := &networkingv1alpha1.MeshPeer{
		ObjectMeta: metav1.ObjectMeta{Name: "keep", Labels: map[string]string{managedByLabel: managedByValue}},
		Spec:       networkingv1alpha1.MeshPeerSpec{ClusterID: "keep", PublicKey: "k"},
	}
	cp := &fakeCP{err: errors.New("control plane down")}
	s, c := newSyncer(t, cp, managed)

	s.syncOnce(context.Background())

	if _, ok := getPeer(t, c, "keep"); !ok {
		t.Error("a fetch error must not prune existing peers (would tear down the mesh on a blip)")
	}
}

func TestSyncer_PartialUpsertFailureDoesNotPruneOrAdvanceRevision(t *testing.T) {
	// A previously-synced managed peer that must survive a converge in which a
	// DIFFERENT peer's upsert fails transiently.
	managed := &networkingv1alpha1.MeshPeer{
		ObjectMeta: metav1.ObjectMeta{Name: "old", Labels: map[string]string{managedByLabel: managedByValue}},
		Spec:       networkingv1alpha1.MeshPeerSpec{ClusterID: "old", PublicKey: "kold"},
	}
	cp := &fakeCP{peers: []dwxclient.RemotePeerConfig{peer("cluster-a", "ka", "1.1.1.1:1")}, rev: "r1"}

	scheme := runtime.NewScheme()
	if err := networkingv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	failCreate := true
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(managed).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, cl ctrlclient.WithWatch, obj ctrlclient.Object, opts ...ctrlclient.CreateOption) error {
				if mp, ok := obj.(*networkingv1alpha1.MeshPeer); ok && mp.Name == "cluster-a" && failCreate {
					return errors.New("transient API error")
				}
				return cl.Create(ctx, obj, opts...)
			},
		}).Build()
	s := &Syncer{CP: cp, K8s: c, Log: logr.Discard()}

	s.syncOnce(context.Background())

	// A failed upsert must not advance the revision (else the next tick would
	// short-circuit and never retry)…
	if s.applied {
		t.Error("revision must not be recorded when an upsert failed")
	}
	// …and prune must be skipped so the existing managed peer is not deleted.
	if _, ok := getPeer(t, c, "old"); !ok {
		t.Error("existing managed peer must NOT be pruned when an upsert failed in the same pass")
	}

	// Next tick, same revision: must retry rather than short-circuit. Let it pass.
	failCreate = false
	s.syncOnce(context.Background())
	if _, ok := getPeer(t, c, "cluster-a"); !ok {
		t.Error("syncer should have retried the upsert on the next tick after a transient failure")
	}
}

func TestSyncer_InvokesConflictObserver(t *testing.T) {
	// Two peers sharing a public key → at least one conflict reported.
	cp := &fakeCP{peers: []dwxclient.RemotePeerConfig{
		peer("a", "dupkey", "1.1.1.1:1"),
		peer("b", "dupkey", "2.2.2.2:1"),
	}}
	var got []topology.TopologyConflict
	s, _ := newSyncer(t, cp)
	s.OnConflicts = func(cs []topology.TopologyConflict) { got = cs }

	s.syncOnce(context.Background())

	if len(got) == 0 {
		t.Error("expected the conflict observer to receive the duplicate-key conflict")
	}
}

func TestSyncer_NonRevisionedClientAlwaysSyncs(t *testing.T) {
	// A client that does NOT implement RevisionedControlPlane must sync every
	// pass (no revision to short-circuit on).
	cp := &plainCP{peers: []dwxclient.RemotePeerConfig{peer("c", "k", "1.1.1.1:1")}}
	s, c := newSyncer(t, cp)

	s.syncOnce(context.Background())
	mp, _ := getPeer(t, c, "c")
	if mp == nil {
		t.Fatal("peer not created")
	}
	_ = c.Delete(context.Background(), mp)
	s.syncOnce(context.Background())
	if _, ok := getPeer(t, c, "c"); !ok {
		t.Error("non-revisioned client should reconcile every pass")
	}
}

// plainCP implements only ControlPlaneClient (not RevisionedControlPlane).
type plainCP struct {
	peers []dwxclient.RemotePeerConfig
}

func (p *plainCP) Authenticate(context.Context) error { return nil }
func (p *plainCP) FetchTopology(context.Context) ([]dwxclient.RemotePeerConfig, error) {
	return p.peers, nil
}
