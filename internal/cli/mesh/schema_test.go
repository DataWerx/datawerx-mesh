package mesh

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/DataWerx/datawerx-mesh/pkg/meshgraph"
	"github.com/DataWerx/datawerx-mesh/pkg/verify"
)

// updateGolden rewrites the committed schema files instead of asserting against
// them: go test ./internal/cli/mesh -update-golden.
var updateGolden = flag.Bool("update-golden", false, "rewrite the committed contract schemas")

type goldenCase struct {
	gen  func() ([]byte, error)
	file string
}

func goldenCases() []goldenCase {
	return []goldenCase{
		{snapshotSchemaJSON, "mesh-snapshot.schema.json"},
		{graphSchemaJSON, "mesh-graph.schema.json"},
	}
}

func goldenPath(file string) string {
	return filepath.Join("..", "..", "..", "docs", "contracts", file)
}

// TestContractSchemasMatchGolden asserts the published schema files under
// docs/contracts are exactly what the generator produces from the Go structs, so
// they cannot silently drift from the wire format.
func TestContractSchemasMatchGolden(t *testing.T) {
	for _, c := range goldenCases() {
		t.Run(c.file, func(t *testing.T) {
			got, err := c.gen()
			if err != nil {
				t.Fatalf("generating schema: %v", err)
			}
			got = append(got, '\n') // the committed files end with a newline
			path := goldenPath(c.file)

			if *updateGolden {
				if err := os.WriteFile(path, got, 0o644); err != nil {
					t.Fatalf("updating golden: %v", err)
				}
				return
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("reading golden %s (run with -update-golden to create): %v", c.file, err)
			}
			if string(got) != string(want) {
				t.Errorf("%s is stale — regenerate with: go test ./internal/cli/mesh -update-golden", c.file)
			}
		})
	}
}

// TestSnapshotSchemaCoversRealOutput marshals a populated snapshot and its graph
// and asserts the generated schema describes every field that actually appears,
// so the published contract can never under-describe the real output.
func TestSnapshotSchemaCoversRealOutput(t *testing.T) {
	snap := sampleSnapshotForSchema()
	assertSchemaCovers(t, "snapshot", mustSchema(t, snapshotSchemaJSON), mustJSON(t, snap))
	assertSchemaCovers(t, "graph", mustSchema(t, graphSchemaJSON), mustJSON(t, meshgraph.Build(snap)))
}

// sampleSnapshotForSchema is a snapshot exercising every collection in the
// contract, so the coverage walk visits each field.
func sampleSnapshotForSchema() verify.Snapshot {
	return verify.BuildSnapshot(verify.SnapshotInputs{
		Now:          1700000000,
		RequiredCRDs: verify.RequiredCRDs(),
		PresentCRDs:  map[string]bool{},
		AgentFound:   true, AgentDesired: 1, AgentReady: 1,
		Peers:     []verify.PeerSnapshot{{Name: "east", ClusterID: "east", Endpoint: "east:51820", Phase: "Connected", PublicKey: "ABCDEFGHIJKL", LastHandshake: 1699999999}},
		Conflicts: []verify.ConflictReport{{ClusterID: "east", Reason: "overlap"}},
		Exports:   []verify.ExportSnapshot{{Namespace: "prod", Name: "ledger", Valid: true}},
		Imports:   []verify.ImportSnapshot{{Namespace: "prod", Name: "payments", Type: "ClusterSetIP", IPs: []string{"241.0.0.5"}, Ports: []verify.PortSnapshot{{Name: "http", Protocol: "TCP", Port: 80}}, Clusters: []string{"east"}}},
		Policies: []verify.PolicySnapshot{{Name: "p", Destinations: []string{"10.0.0.0/24"}, Phase: "Ready", Ingress: []verify.PolicyIngressSnapshot{
			{From: []verify.PolicySourceSnapshot{{ClusterIDs: []string{"east"}, CIDRs: []string{"10.1.0.0/16"}}}, Ports: []verify.PortSnapshot{{Protocol: "TCP", Port: 443}}},
		}}},
		Events:  []verify.EventSnapshot{{Type: "Warning", Reason: "Degraded", Message: "x", Object: "MeshPeer/east", Count: 1, LastSeen: 1699999990}},
		Metrics: []verify.MetricPointer{{Name: "dwx_meshpeers", Help: "h", Value: 1}},
	})
}

func mustSchema(t *testing.T, gen func() ([]byte, error)) map[string]any {
	t.Helper()
	raw, err := gen()
	if err != nil {
		t.Fatalf("schema: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("schema is not valid JSON: %v", err)
	}
	return m
}

func mustJSON(t *testing.T, v any) any {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshaling value: %v", err)
	}
	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decoding value: %v", err)
	}
	return out
}

// assertSchemaCovers walks a decoded JSON value against its schema, failing if
// the value contains an object key the schema does not describe (via properties
// or additionalProperties) or an array whose items the schema omits.
func assertSchemaCovers(t *testing.T, path string, schema map[string]any, value any) {
	switch v := value.(type) {
	case map[string]any:
		props, _ := schema["properties"].(map[string]any)
		addl := schema["additionalProperties"]
		for key, child := range v {
			sub, ok := props[key].(map[string]any)
			if !ok {
				if addlSchema, ok := addl.(map[string]any); ok {
					assertSchemaCovers(t, path+"."+key, addlSchema, child)
					continue
				}
				t.Errorf("%s.%s: present in output but not described by the schema", path, key)
				continue
			}
			assertSchemaCovers(t, path+"."+key, sub, child)
		}
	case []any:
		items, _ := schema["items"].(map[string]any)
		if items == nil {
			if len(v) > 0 {
				t.Errorf("%s: array has elements but the schema has no items", path)
			}
			return
		}
		for i, child := range v {
			assertSchemaCovers(t, path+"[]", items, child)
			_ = i
		}
	}
}
