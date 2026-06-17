package meshstate

import (
	"context"
	"errors"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	mcsv1alpha1 "github.com/DataWerx/datawerx-mesh/pkg/apis/multicluster/v1alpha1"
	networkingv1alpha1 "github.com/DataWerx/datawerx-mesh/pkg/apis/networking/v1alpha1"
	"github.com/DataWerx/datawerx-mesh/pkg/verify"
)

// newFakeClient builds a client over the package scheme seeded with objs, so the
// gather runs against the same scheme NewClient registers in production.
func newFakeClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	scheme, err := buildScheme()
	if err != nil {
		t.Fatalf("buildScheme: %v", err)
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
}

func crd(name string) *apiextensionsv1.CustomResourceDefinition {
	return &apiextensionsv1.CustomResourceDefinition{ObjectMeta: metav1.ObjectMeta{Name: name}}
}

func allRequiredCRDs() []client.Object {
	var out []client.Object
	for _, name := range verify.RequiredCRDs() {
		out = append(out, crd(name))
	}
	return out
}

func TestSnapshot_FullMesh(t *testing.T) {
	objs := allRequiredCRDs()
	objs = append(objs,
		&appsv1.DaemonSet{
			ObjectMeta: metav1.ObjectMeta{Namespace: DefaultNamespace, Name: DefaultDaemonSet},
			Status:     appsv1.DaemonSetStatus{DesiredNumberScheduled: 3, NumberReady: 3},
		},
		// Two peers whose CIDRs overlap, so the pure detector must surface a conflict.
		&networkingv1alpha1.MeshPeer{
			ObjectMeta: metav1.ObjectMeta{Name: "west"},
			Spec: networkingv1alpha1.MeshPeerSpec{
				ClusterID: "west", PublicKey: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=",
				Endpoint: "west.example:51820", PodCIDRs: []string{"10.10.0.0/16"},
			},
			Status: networkingv1alpha1.MeshPeerStatus{Phase: networkingv1alpha1.MeshPeerPhaseConnected, LastHandshakeTime: 1},
		},
		&networkingv1alpha1.MeshPeer{
			ObjectMeta: metav1.ObjectMeta{Name: "east"},
			Spec: networkingv1alpha1.MeshPeerSpec{
				ClusterID: "east", PublicKey: "BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB=",
				Endpoint: "east.example:51820", PodCIDRs: []string{"10.10.0.0/16"},
			},
			Status: networkingv1alpha1.MeshPeerStatus{Phase: networkingv1alpha1.MeshPeerPhasePending},
		},
		&mcsv1alpha1.ServiceExport{
			ObjectMeta: metav1.ObjectMeta{Namespace: "prod", Name: "payments"},
			Status: mcsv1alpha1.ServiceExportStatus{Conditions: []metav1.Condition{
				{Type: mcsv1alpha1.ServiceExportValid, Status: metav1.ConditionTrue, Reason: "Exported"},
			}},
		},
		&mcsv1alpha1.ServiceExport{
			ObjectMeta: metav1.ObjectMeta{Namespace: "prod", Name: "ledger"},
			Status: mcsv1alpha1.ServiceExportStatus{Conditions: []metav1.Condition{
				{Type: mcsv1alpha1.ServiceExportValid, Status: metav1.ConditionFalse, Reason: "NoService", Message: "Service prod/ledger not found"},
				{Type: mcsv1alpha1.ServiceExportConflict, Status: metav1.ConditionTrue, Reason: "TypeMismatch", Message: "port mismatch with cluster east"},
			}},
		},
		&mcsv1alpha1.ServiceImport{
			ObjectMeta: metav1.ObjectMeta{Namespace: "prod", Name: "payments"},
			Spec: mcsv1alpha1.ServiceImportSpec{
				Type: mcsv1alpha1.ClusterSetIP, IPs: []string{"241.0.0.5"},
				Ports: []mcsv1alpha1.ServicePort{{Name: "http", Protocol: corev1.ProtocolTCP, Port: 80}},
			},
			Status: mcsv1alpha1.ServiceImportStatus{Clusters: []mcsv1alpha1.ClusterStatus{{Cluster: "east"}, {Cluster: "west"}}},
		},
		&networkingv1alpha1.MeshNetworkPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: "ledger-allow"},
			Spec: networkingv1alpha1.MeshNetworkPolicySpec{
				Destinations: []string{"10.0.0.0/24"},
				Ingress:      []networkingv1alpha1.MeshIngressRule{{From: []networkingv1alpha1.MeshPeerSelector{{ClusterIDs: []string{"west", "east"}}}}},
			},
			Status: networkingv1alpha1.MeshNetworkPolicyStatus{Phase: networkingv1alpha1.MeshNetworkPolicyPhaseReady},
		},
		// A Warning on a mesh object is kept; a Normal one and a Warning on a
		// non-mesh object are filtered out.
		warningEvent("MeshPeer", "", "east", "Degraded", "no handshake"),
		normalEvent("MeshPeer", "", "west", "Connected", "tunnel up"),
		warningEvent("Pod", "prod", "payments-abc", "BackOff", "crashloop"),
	)

	snap, err := Snapshot(context.Background(), newFakeClient(t, objs...), DefaultNamespace, DefaultDaemonSet)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}

	if snap.APIVersion != verify.SnapshotAPIVersion || snap.Kind != verify.SnapshotKind {
		t.Errorf("unexpected envelope: %s/%s", snap.APIVersion, snap.Kind)
	}
	if snap.GeneratedAt == 0 {
		t.Error("GeneratedAt should be stamped with the wall clock")
	}

	// Peers are sorted by cluster ID, so east precedes west.
	if len(snap.Peers) != 2 || snap.Peers[0].ClusterID != "east" || snap.Peers[1].ClusterID != "west" {
		t.Fatalf("unexpected peers: %+v", snap.Peers)
	}
	if got := snap.Peers[1].PublicKey; got != "AAAAAAAA…" {
		t.Errorf("peer public key not truncated: %q", got)
	}
	if snap.Peers[1].PodCIDRs[0] != "10.10.0.0/16" {
		t.Errorf("peer CIDRs not carried: %+v", snap.Peers[1])
	}

	// The overlapping CIDRs must produce a topology conflict computed by the same
	// pure detector the data plane uses.
	if len(snap.Conflicts) == 0 {
		t.Error("expected a CIDR-overlap conflict from the overlapping peers")
	}

	if len(snap.Exports) != 2 {
		t.Fatalf("unexpected exports: %+v", snap.Exports)
	}
	for _, e := range snap.Exports {
		switch e.Name {
		case "payments":
			if !e.Valid || e.Conflict {
				t.Errorf("payments export should be valid, not in conflict: %+v", e)
			}
		case "ledger":
			if e.Valid || !e.Conflict {
				t.Errorf("ledger export should be invalid and in conflict: %+v", e)
			}
			if e.Message != "Service prod/ledger not found" {
				t.Errorf("ledger export should surface the invalid reason: %q", e.Message)
			}
		}
	}

	if len(snap.Imports) != 1 || snap.Imports[0].Type != "ClusterSetIP" || snap.Imports[0].IPs[0] != "241.0.0.5" {
		t.Fatalf("unexpected imports: %+v", snap.Imports)
	}
	if len(snap.Imports[0].Clusters) != 2 || len(snap.Imports[0].Ports) != 1 || snap.Imports[0].Ports[0].Port != 80 {
		t.Errorf("import ports/clusters not projected: %+v", snap.Imports[0])
	}

	if len(snap.Policies) != 1 || snap.Policies[0].Name != "ledger-allow" || snap.Policies[0].IngressRules != 1 {
		t.Fatalf("unexpected policies: %+v", snap.Policies)
	}
	// The ingress sources are projected and sorted, so a consumer can answer "who
	// may reach me" deterministically.
	pol := snap.Policies[0]
	if len(pol.Ingress) != 1 || len(pol.Ingress[0].From) != 1 {
		t.Fatalf("policy ingress not projected: %+v", pol.Ingress)
	}
	if got := pol.Ingress[0].From[0].ClusterIDs; len(got) != 2 || got[0] != "east" || got[1] != "west" {
		t.Errorf("policy ingress clusterIDs should be projected and sorted, got %v", got)
	}

	if len(snap.Events) != 1 || snap.Events[0].Object != "MeshPeer/east" {
		t.Fatalf("expected exactly the one mesh Warning event, got: %+v", snap.Events)
	}

	if len(snap.Metrics) == 0 {
		t.Error("expected metric pointers")
	}

	// The embedded health report must be a strict superset: the agent is ready and
	// the CRDs are present, so nothing fails.
	if snap.Health.Failed() {
		t.Errorf("health unexpectedly failed: %+v", snap.Health)
	}
}

func TestSnapshot_MissingAgentAndCRDs(t *testing.T) {
	// No CRDs and no DaemonSet seeded.
	snap, err := Snapshot(context.Background(), newFakeClient(t), DefaultNamespace, DefaultDaemonSet)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if !snap.Health.Failed() {
		t.Error("health should fail when the CRDs and agent are absent")
	}
	if len(snap.Peers) != 0 || len(snap.Exports) != 0 || len(snap.Imports) != 0 || len(snap.Policies) != 0 {
		t.Errorf("expected an empty mesh, got %+v", snap)
	}
}

func TestSnapshot_EventsAreBestEffort(t *testing.T) {
	// A client whose Event list fails must still yield a complete snapshot of
	// everything else, with the events block simply empty.
	scheme, err := buildScheme()
	if err != nil {
		t.Fatalf("buildScheme: %v", err)
	}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(allRequiredCRDs()...).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				if _, ok := list.(*corev1.EventList); ok {
					return errors.New("forbidden: cannot list events")
				}
				return cl.List(ctx, list, opts...)
			},
		}).
		Build()

	snap, err := Snapshot(context.Background(), c, DefaultNamespace, DefaultDaemonSet)
	if err != nil {
		t.Fatalf("Snapshot should tolerate an events read failure: %v", err)
	}
	if len(snap.Events) != 0 {
		t.Errorf("expected no events when the list fails, got %+v", snap.Events)
	}
}

func TestSnapshot_CoreReadFailurePropagates(t *testing.T) {
	// A failure listing a core object (here MeshPeers) is load-bearing and must
	// surface as an error, not a silently partial snapshot.
	scheme, err := buildScheme()
	if err != nil {
		t.Fatalf("buildScheme: %v", err)
	}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(allRequiredCRDs()...).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				if _, ok := list.(*networkingv1alpha1.MeshPeerList); ok {
					return errors.New("boom")
				}
				return cl.List(ctx, list, opts...)
			},
		}).
		Build()

	if _, err := Snapshot(context.Background(), c, DefaultNamespace, DefaultDaemonSet); err == nil {
		t.Fatal("expected the MeshPeer list failure to propagate")
	}
}

func TestBuildScheme_RegistersEveryGatheredType(t *testing.T) {
	scheme, err := buildScheme()
	if err != nil {
		t.Fatalf("buildScheme: %v", err)
	}
	// Every object the gather reads must be recognized, or a List/Get would fail
	// at runtime with "no kind is registered".
	for _, obj := range []client.Object{
		&appsv1.DaemonSet{},
		&corev1.Event{},
		&apiextensionsv1.CustomResourceDefinition{},
		&networkingv1alpha1.MeshPeer{},
		&networkingv1alpha1.MeshNetworkPolicy{},
		&mcsv1alpha1.ServiceExport{},
		&mcsv1alpha1.ServiceImport{},
	} {
		if _, _, err := scheme.ObjectKinds(obj); err != nil {
			t.Errorf("scheme does not recognize %T: %v", obj, err)
		}
	}
}

func warningEvent(kind, ns, name, reason, msg string) *corev1.Event {
	return event(kind, ns, name, reason, msg, corev1.EventTypeWarning)
}

func normalEvent(kind, ns, name, reason, msg string) *corev1.Event {
	return event(kind, ns, name, reason, msg, corev1.EventTypeNormal)
}

func event(kind, ns, name, reason, msg, typ string) *corev1.Event {
	// Events live in a namespace even when their involved object is cluster-scoped;
	// the agent emits mesh-object events into its own namespace.
	return &corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Namespace: DefaultNamespace, Name: kind + "-" + name + "-" + reason},
		InvolvedObject: corev1.ObjectReference{Kind: kind, Namespace: ns, Name: name},
		Reason:         reason,
		Message:        msg,
		Type:           typ,
		Count:          1,
	}
}
