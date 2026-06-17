package meshgraph

import (
	"encoding/json"
	"testing"

	"github.com/datawerx/datawerx/pkg/verify"
)

// sampleSnapshot is a representative two-peer mesh: a healthy peer that serves an
// imported service, a conflicted peer, a multi-provider import, and an export.
func sampleSnapshot() verify.Snapshot {
	return verify.Snapshot{
		APIVersion:  verify.SnapshotAPIVersion,
		Kind:        verify.SnapshotKind,
		GeneratedAt: 1700000000,
		Peers: []verify.PeerSnapshot{
			{Name: "east", ClusterID: "east", Endpoint: "east:51820", Phase: "Connected"},
			{Name: "west", ClusterID: "west", Endpoint: "west:51820", Phase: "Error"},
		},
		Conflicts: []verify.ConflictReport{
			{ClusterID: "west", Reason: "CIDR 10.0.0.0/16 overlaps cluster \"east\""},
		},
		Imports: []verify.ImportSnapshot{
			{Namespace: "prod", Name: "payments", Type: "ClusterSetIP", IPs: []string{"241.0.0.5"}, Clusters: []string{"east", "west"}},
		},
		Exports: []verify.ExportSnapshot{
			{Namespace: "prod", Name: "ledger", Valid: true},
		},
	}
}

func findNode(g Graph, id string) (Node, bool) {
	for _, n := range g.Nodes {
		if n.ID == id {
			return n, true
		}
	}
	return Node{}, false
}

func hasEdge(g Graph, from, to string, kind EdgeKind) bool {
	for _, e := range g.Edges {
		if e.From == from && e.To == to && e.Kind == kind {
			return true
		}
	}
	return false
}

func TestBuild_Envelope(t *testing.T) {
	g := Build(sampleSnapshot())
	if g.APIVersion != GraphAPIVersion || g.Kind != GraphKind {
		t.Errorf("unexpected envelope: %s/%s", g.APIVersion, g.Kind)
	}
	if g.GeneratedAt != 1700000000 {
		t.Errorf("GeneratedAt not carried from snapshot: %d", g.GeneratedAt)
	}
}

func TestBuild_LocalNodeAlwaysPresent(t *testing.T) {
	g := Build(verify.Snapshot{})
	if len(g.Nodes) != 1 {
		t.Fatalf("empty mesh should yield exactly the local node, got %d", len(g.Nodes))
	}
	n := g.Nodes[0]
	if n.ID != LocalNodeID || !n.Local || n.Kind != NodeCluster {
		t.Errorf("unexpected local node: %+v", n)
	}
	if len(g.Edges) != 0 {
		t.Errorf("empty mesh should have no edges, got %+v", g.Edges)
	}
}

func TestBuild_PeersAndPeeringEdges(t *testing.T) {
	g := Build(sampleSnapshot())

	east, ok := findNode(g, "cluster/east")
	if !ok || east.Phase != "Connected" || east.Endpoint != "east:51820" || east.Conflict {
		t.Errorf("unexpected east node: %+v", east)
	}
	west, ok := findNode(g, "cluster/west")
	if !ok || west.Phase != "Error" || !west.Conflict {
		t.Errorf("west should be a conflicted error peer: %+v", west)
	}

	if !hasEdge(g, LocalNodeID, "cluster/east", EdgePeering) {
		t.Error("missing local→east peering edge")
	}
	for _, e := range g.Edges {
		if e.Kind == EdgePeering && e.To == "cluster/west" {
			if !e.Conflict || e.Phase != "Error" {
				t.Errorf("west peering edge should carry conflict+phase: %+v", e)
			}
		}
	}
}

func TestBuild_ImportFansInFromProviders(t *testing.T) {
	g := Build(sampleSnapshot())

	svc, ok := findNode(g, "svc/prod/payments")
	if !ok || !svc.Imported || svc.ImportType != "ClusterSetIP" || svc.Namespace != "prod" {
		t.Fatalf("unexpected payments service node: %+v", svc)
	}
	if len(svc.IPs) != 1 || svc.IPs[0] != "241.0.0.5" {
		t.Errorf("import IPs not carried: %+v", svc.IPs)
	}

	// Both providers serve the imported service, and the service is imported by
	// the local cluster.
	if !hasEdge(g, "cluster/east", "svc/prod/payments", EdgeServes) ||
		!hasEdge(g, "cluster/west", "svc/prod/payments", EdgeServes) {
		t.Error("missing serves edges from providers")
	}
	if !hasEdge(g, "svc/prod/payments", LocalNodeID, EdgeImports) {
		t.Error("missing service→local imports edge")
	}
}

func TestBuild_ExportEdge(t *testing.T) {
	g := Build(sampleSnapshot())
	svc, ok := findNode(g, "svc/prod/ledger")
	if !ok || !svc.Exported {
		t.Fatalf("ledger should be an exported service node: %+v", svc)
	}
	if !hasEdge(g, LocalNodeID, "svc/prod/ledger", EdgeExports) {
		t.Error("missing local→ledger exports edge")
	}
}

func TestBuild_ImportProviderWithoutMeshPeer(t *testing.T) {
	// A cluster that contributes to an import but has no MeshPeer of its own must
	// still get a cluster node, so the serves edge is never dangling.
	snap := verify.Snapshot{
		Imports: []verify.ImportSnapshot{
			{Namespace: "prod", Name: "db", Type: "Headless", Clusters: []string{"ghost"}},
		},
	}
	g := Build(snap)
	if _, ok := findNode(g, "cluster/ghost"); !ok {
		t.Error("provider cluster with no MeshPeer should still be a node")
	}
	if !hasEdge(g, "cluster/ghost", "svc/prod/db", EdgeServes) {
		t.Error("missing serves edge from the orphan provider")
	}
}

func TestBuild_ServiceImportedAndExportedMerges(t *testing.T) {
	snap := verify.Snapshot{
		Imports: []verify.ImportSnapshot{{Namespace: "prod", Name: "cache", Type: "Headless", Clusters: []string{"east"}}},
		Exports: []verify.ExportSnapshot{{Namespace: "prod", Name: "cache", Valid: true}},
	}
	g := Build(snap)
	svc, ok := findNode(g, "svc/prod/cache")
	if !ok || !svc.Imported || !svc.Exported {
		t.Errorf("a service both imported and exported should merge into one node: %+v", svc)
	}
	count := 0
	for _, n := range g.Nodes {
		if n.ID == "svc/prod/cache" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("expected exactly one merged service node, got %d", count)
	}
}

func TestBuild_Deterministic(t *testing.T) {
	// Shuffling the input order must not change the output, since Build sorts.
	a := sampleSnapshot()
	b := sampleSnapshot()
	b.Peers[0], b.Peers[1] = b.Peers[1], b.Peers[0]
	b.Imports[0].Clusters = []string{"west", "east"}

	ja, _ := Build(a).JSON()
	jb, _ := Build(b).JSON()
	if string(ja) != string(jb) {
		t.Errorf("graph is not order-independent:\n%s\n---\n%s", ja, jb)
	}
}

func TestBuild_EdgesAndNodesAreSorted(t *testing.T) {
	g := Build(sampleSnapshot())
	for i := 1; i < len(g.Nodes); i++ {
		if g.Nodes[i-1].ID > g.Nodes[i].ID {
			t.Errorf("nodes not sorted by id at %d: %q > %q", i, g.Nodes[i-1].ID, g.Nodes[i].ID)
		}
	}
	for i := 1; i < len(g.Edges); i++ {
		prev, cur := g.Edges[i-1], g.Edges[i]
		if prev.Kind > cur.Kind || (prev.Kind == cur.Kind && prev.From > cur.From) {
			t.Errorf("edges not sorted at %d: %+v then %+v", i, prev, cur)
		}
	}
}

func TestBuild_PolicyAllowsEdges(t *testing.T) {
	snap := verify.Snapshot{
		Peers: []verify.PeerSnapshot{{Name: "east", ClusterID: "east", Phase: "Connected"}},
		Policies: []verify.PolicySnapshot{
			{
				Name:         "ledger-allow",
				Destinations: []string{"10.0.0.0/24"},
				Ingress: []verify.PolicyIngressSnapshot{
					{From: []verify.PolicySourceSnapshot{{ClusterIDs: []string{"east"}}}},
					// A CIDR-only source produces no node or edge: it is not a cluster.
					{From: []verify.PolicySourceSnapshot{{CIDRs: []string{"192.168.0.0/16"}}}},
				},
			},
		},
	}
	g := Build(snap)

	allows := 0
	for _, e := range g.Edges {
		if e.Kind == EdgeAllows {
			allows++
			if e.From != "cluster/east" || e.To != LocalNodeID || e.Policy != "ledger-allow" {
				t.Errorf("unexpected allows edge: %+v", e)
			}
		}
	}
	if allows != 1 {
		t.Fatalf("expected exactly one allows edge (cluster source only), got %d", allows)
	}
	if _, ok := findNode(g, "cluster/east"); !ok {
		t.Error("the allowed source cluster should be a node")
	}
}

func TestBuild_PolicyAllowsUnpeeredSourceGetsNode(t *testing.T) {
	// A policy may name a cluster that has no MeshPeer. It still gets a node so the
	// allows edge is not dangling, but no peering edge — signaling it is not peered.
	snap := verify.Snapshot{
		Policies: []verify.PolicySnapshot{
			{Name: "p", Ingress: []verify.PolicyIngressSnapshot{
				{From: []verify.PolicySourceSnapshot{{ClusterIDs: []string{"ghost"}}}},
			}},
		},
	}
	g := Build(snap)
	if _, ok := findNode(g, "cluster/ghost"); !ok {
		t.Fatal("unpeered allowed source should still be a node")
	}
	for _, e := range g.Edges {
		if e.Kind == EdgePeering && e.To == "cluster/ghost" {
			t.Error("an unpeered allowed source must not have a peering edge")
		}
	}
}

func TestBuild_TwoPoliciesAllowingSameSourceAreDistinctEdges(t *testing.T) {
	snap := verify.Snapshot{
		Peers: []verify.PeerSnapshot{{Name: "east", ClusterID: "east", Phase: "Connected"}},
		Policies: []verify.PolicySnapshot{
			{Name: "a", Ingress: []verify.PolicyIngressSnapshot{{From: []verify.PolicySourceSnapshot{{ClusterIDs: []string{"east"}}}}}},
			{Name: "b", Ingress: []verify.PolicyIngressSnapshot{{From: []verify.PolicySourceSnapshot{{ClusterIDs: []string{"east"}}}}}},
		},
	}
	g := Build(snap)
	policies := map[string]bool{}
	for _, e := range g.Edges {
		if e.Kind == EdgeAllows {
			policies[e.Policy] = true
		}
	}
	if !policies["a"] || !policies["b"] {
		t.Errorf("each policy should yield its own allows edge, got %v", policies)
	}
}

func TestBuild_DuplicateProviderDedupesServesEdge(t *testing.T) {
	// The same cluster listed twice as a provider must yield a single serves edge.
	snap := verify.Snapshot{
		Imports: []verify.ImportSnapshot{
			{Namespace: "prod", Name: "db", Type: "Headless", Clusters: []string{"east", "east"}},
		},
	}
	g := Build(snap)
	serves := 0
	for _, e := range g.Edges {
		if e.Kind == EdgeServes {
			serves++
		}
	}
	if serves != 1 {
		t.Errorf("duplicate provider should collapse to one serves edge, got %d", serves)
	}
}

func TestBuild_EmptyProviderClusterIDLabel(t *testing.T) {
	// A blank contributing cluster id should still produce a labeled node rather
	// than an empty one.
	snap := verify.Snapshot{
		Imports: []verify.ImportSnapshot{
			{Namespace: "prod", Name: "db", Type: "Headless", Clusters: []string{""}},
		},
	}
	g := Build(snap)
	n, ok := findNode(g, "cluster/")
	if !ok || n.Label != "<unknown>" {
		t.Errorf("blank provider id should be labeled <unknown>: %+v", n)
	}
}

func TestGraph_JSONIsValidAndStable(t *testing.T) {
	g := Build(sampleSnapshot())
	out, err := g.JSON()
	if err != nil {
		t.Fatalf("JSON: %v", err)
	}
	var round Graph
	if err := json.Unmarshal(out, &round); err != nil {
		t.Fatalf("graph JSON does not round-trip: %v", err)
	}
	if round.Kind != GraphKind {
		t.Errorf("round-trip lost kind: %+v", round)
	}
}
