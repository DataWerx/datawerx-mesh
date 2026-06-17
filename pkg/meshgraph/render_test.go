package meshgraph

import (
	"strings"
	"testing"

	"github.com/DataWerx/datawerx-mesh/pkg/verify"
)

func TestDOT_StructureAndStyling(t *testing.T) {
	dot := Build(sampleSnapshot()).DOT()

	if !strings.HasPrefix(dot, "digraph mesh {") || !strings.HasSuffix(strings.TrimSpace(dot), "}") {
		t.Fatalf("DOT is not a well-formed digraph:\n%s", dot)
	}
	if !strings.Contains(dot, "rankdir=LR;") {
		t.Error("DOT should lay out left-to-right")
	}
	// The local cluster is filled with the local color.
	if !strings.Contains(dot, `"cluster/local"`) || !strings.Contains(dot, "#d8e8ff") {
		t.Error("DOT should style the local cluster node")
	}
	// The conflicted, errored west peer edge is drawn in the trouble color.
	if !strings.Contains(dot, "#cc0000") {
		t.Error("DOT should color the conflicted/error peering edge red")
	}
	// Every node id appears as a quoted identifier and every edge as an arrow.
	if !strings.Contains(dot, `"cluster/local" -> "cluster/east"`) {
		t.Errorf("DOT missing the local→east edge:\n%s", dot)
	}
	if !strings.Contains(dot, `"svc/prod/payments" -> "cluster/local"`) {
		t.Errorf("DOT missing the import edge:\n%s", dot)
	}
}

func TestDOT_EscapesLabels(t *testing.T) {
	// A service name with a quote must not break the DOT string.
	snap := verify.Snapshot{
		Exports: []verify.ExportSnapshot{{Namespace: "ns", Name: `wei"rd`, Valid: true}},
	}
	dot := Build(snap).DOT()
	if !strings.Contains(dot, `wei\"rd`) {
		t.Errorf("DOT did not escape the quote in the label:\n%s", dot)
	}
}

func TestDOT_Deterministic(t *testing.T) {
	if Build(sampleSnapshot()).DOT() != Build(sampleSnapshot()).DOT() {
		t.Error("DOT rendering is not deterministic")
	}
}

func TestMermaid_StructureAndStyling(t *testing.T) {
	m := Build(sampleSnapshot()).Mermaid()

	if !strings.HasPrefix(m, "flowchart LR") {
		t.Fatalf("Mermaid should start with a flowchart header:\n%s", m)
	}
	if !strings.Contains(m, "classDef local") || !strings.Contains(m, "classDef conflict") {
		t.Error("Mermaid should declare the local and conflict classes")
	}
	if !strings.Contains(m, "-->|") {
		t.Error("Mermaid should render labeled edges")
	}
	// The local class is applied to exactly the local node alias.
	if !strings.Contains(m, " local;") || !strings.Contains(m, " conflict;") {
		t.Errorf("Mermaid should assign nodes to the styling classes:\n%s", m)
	}
}

func TestMermaid_Deterministic(t *testing.T) {
	if Build(sampleSnapshot()).Mermaid() != Build(sampleSnapshot()).Mermaid() {
		t.Error("Mermaid rendering is not deterministic")
	}
}

func TestRender_ServiceLabelVariants(t *testing.T) {
	// Exercise every service-label branch: a merged import+export, an import with
	// no resolved type, and a plain export.
	snap := verify.Snapshot{
		Imports: []verify.ImportSnapshot{
			{Namespace: "prod", Name: "both", Type: "Headless", Clusters: []string{"east"}},
			{Namespace: "prod", Name: "typeless", Clusters: []string{"east"}},
		},
		Exports: []verify.ExportSnapshot{
			{Namespace: "prod", Name: "both", Valid: true},
			{Namespace: "prod", Name: "shipped", Valid: true},
		},
	}
	g := Build(snap)
	dot := g.DOT()
	mermaid := g.Mermaid()

	for _, want := range []string{"import+export", "export"} {
		if !strings.Contains(dot, want) {
			t.Errorf("DOT missing service label %q:\n%s", want, dot)
		}
	}
	// The typeless import falls back to the bare "import" label.
	if !strings.Contains(dot, `typeless\nimport`) {
		t.Errorf("DOT should label a typeless import as import:\n%s", dot)
	}
	if !strings.Contains(mermaid, "import+export") {
		t.Errorf("Mermaid should render the merged label:\n%s", mermaid)
	}
}

func TestRender_PolicyAllowsEdge(t *testing.T) {
	snap := verify.Snapshot{
		Peers: []verify.PeerSnapshot{{Name: "east", ClusterID: "east", Phase: "Connected"}},
		Policies: []verify.PolicySnapshot{
			{Name: "ledger-allow", Ingress: []verify.PolicyIngressSnapshot{
				{From: []verify.PolicySourceSnapshot{{ClusterIDs: []string{"east"}}}},
			}},
		},
	}
	g := Build(snap)
	dot := g.DOT()
	if !strings.Contains(dot, "allows (ledger-allow)") {
		t.Errorf("DOT should label the allows edge with the policy:\n%s", dot)
	}
	if !strings.Contains(dot, `"cluster/east" -> "cluster/local"`) {
		t.Errorf("DOT should draw the allows edge into local:\n%s", dot)
	}
	if m := g.Mermaid(); !strings.Contains(m, "allows (ledger-allow)") {
		t.Errorf("Mermaid should label the allows edge:\n%s", m)
	}
}

func TestRender_UnknownKindsFallBackGracefully(t *testing.T) {
	// Build never emits these kinds, but the renderers should degrade to the raw
	// kind string rather than panic or drop the element if the schema ever grows.
	g := Graph{
		APIVersion: GraphAPIVersion, Kind: GraphKind,
		Nodes: []Node{
			{ID: "a", Kind: NodeKind("gateway"), Label: "a"},
			{ID: "b", Kind: NodeKind("gateway"), Label: "b"},
		},
		Edges: []Edge{{From: "a", To: "b", Kind: EdgeKind("routes")}},
	}
	dot := g.DOT()
	if !strings.Contains(dot, "routes") {
		t.Errorf("DOT should fall back to the raw edge kind:\n%s", dot)
	}
	if m := g.Mermaid(); !strings.Contains(m, "routes") {
		t.Errorf("Mermaid should fall back to the raw edge kind:\n%s", m)
	}
}

func TestMermaid_EmptyMeshIsValid(t *testing.T) {
	m := Build(verify.Snapshot{}).Mermaid()
	if !strings.HasPrefix(m, "flowchart LR") {
		t.Errorf("empty-mesh Mermaid should still be a valid flowchart:\n%s", m)
	}
	// Just the local node, assigned the local class.
	if strings.Contains(m, "-->") {
		t.Errorf("empty mesh should have no edges:\n%s", m)
	}
}
