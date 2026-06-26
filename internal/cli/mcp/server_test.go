package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/DataWerx/datawerx-mesh/pkg/verify"
)

// fakeSnapshot is a small but representative snapshot: one Error peer with an
// overlap conflict, so diagnose has something concrete to report.
func fakeSnapshot() verify.Snapshot {
	return verify.BuildSnapshot(verify.SnapshotInputs{
		Now:          1_000_000,
		RequiredCRDs: verify.RequiredCRDs(),
		PresentCRDs: func() map[string]bool {
			m := map[string]bool{}
			for _, c := range verify.RequiredCRDs() {
				m[c] = true
			}
			return m
		}(),
		AgentFound: true, AgentDesired: 1, AgentReady: 1,
		Peers: []verify.PeerSnapshot{
			{Name: "cluster-a", ClusterID: "a", Phase: "Error", Message: "CIDR overlap with local cluster requires NAT remap"},
		},
		Conflicts: []verify.ConflictReport{{ClusterID: "a", Reason: `CIDR 10.244.0.0/16 overlaps cluster "b"`}},
		Imports:   []verify.ImportSnapshot{{Namespace: "prod", Name: "ledger", Type: "ClusterSetIP", IPs: []string{"241.0.0.5"}}},
	})
}

func testServer() *server {
	s, _ := newServer(serverConfig{})
	s.snapshot = func(context.Context) (verify.Snapshot, error) { return fakeSnapshot(), nil }
	return s
}

// drive feeds newline-delimited requests through Serve and returns the response
// objects (one per line written).
func drive(t *testing.T, s *server, lines ...string) []rpcResponse {
	t.Helper()
	var out bytes.Buffer
	if err := s.Serve(context.Background(), strings.NewReader(strings.Join(lines, "\n")+"\n"), &out); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	var resps []rpcResponse
	sc := bufio.NewScanner(&out)
	for sc.Scan() {
		var r rpcResponse
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			t.Fatalf("bad response line %q: %v", sc.Text(), err)
		}
		resps = append(resps, r)
	}
	return resps
}

func TestInitialize(t *testing.T) {
	resps := drive(t, testServer(), `{"jsonrpc":"2.0","id":1,"method":"initialize"}`)
	if len(resps) != 1 {
		t.Fatalf("want 1 response, got %d", len(resps))
	}
	result, _ := json.Marshal(resps[0].Result)
	if !strings.Contains(string(result), protocolVersion) {
		t.Errorf("initialize result missing protocol version: %s", result)
	}
	if !strings.Contains(string(result), `"tools"`) {
		t.Errorf("initialize should advertise the tools capability: %s", result)
	}
}

func TestNotificationProducesNoResponse(t *testing.T) {
	// A notification (no id) must not get a reply; the following request must.
	resps := drive(t, testServer(),
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"ping"}`,
	)
	if len(resps) != 1 {
		t.Fatalf("notification should yield no response; got %d responses", len(resps))
	}
}

func TestToolsListIsReadOnly(t *testing.T) {
	resps := drive(t, testServer(), `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	raw, _ := json.Marshal(resps[0].Result)

	var listed struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(raw, &listed); err != nil {
		t.Fatalf("decoding tools/list: %v", err)
	}
	if len(listed.Tools) == 0 {
		t.Fatal("expected some tools")
	}
	// The free build must expose zero mutating tools.
	mutating := []string{"apply", "create", "update", "delete", "set", "rotate", "mutate", "patch", "write"}
	for _, tl := range listed.Tools {
		for _, m := range mutating {
			if strings.Contains(strings.ToLower(tl.Name), m) {
				t.Errorf("read-only build exposed a mutating-sounding tool: %q", tl.Name)
			}
		}
	}
}

func TestToolCall_Diagnose(t *testing.T) {
	resps := drive(t, testServer(),
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"mesh_diagnose","arguments":{}}}`)
	text := toolResultText(t, resps[0])
	if !strings.Contains(text, "overlaps cluster") {
		t.Errorf("diagnose tool should cite the overlap signal, got: %s", text)
	}
}

func TestToolCall_ListImports(t *testing.T) {
	resps := drive(t, testServer(),
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"list_service_imports","arguments":{}}}`)
	text := toolResultText(t, resps[0])
	if !strings.Contains(text, "ledger") || !strings.Contains(text, "241.0.0.5") {
		t.Errorf("list_service_imports should include the imported service, got: %s", text)
	}
}

func TestToolCall_Graph(t *testing.T) {
	resps := drive(t, testServer(),
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"mesh_graph","arguments":{}}}`)
	text := toolResultText(t, resps[0])
	// The graph derives from the same snapshot, so it must carry the local node,
	// the peer, and the imported service.
	for _, want := range []string{"MeshGraph", "cluster/local", "cluster/a", "svc/prod/ledger"} {
		if !strings.Contains(text, want) {
			t.Errorf("mesh_graph should include %q, got: %s", want, text)
		}
	}
}

func TestToolCall_Reachability(t *testing.T) {
	resps := drive(t, testServer(),
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"mesh_reachability","arguments":{}}}`)
	text := toolResultText(t, resps[0])
	// The fake snapshot's one peer is in phase Error, so it must be Unreachable.
	for _, want := range []string{"MeshReachability", "\"cluster\": \"a\"", "Unreachable"} {
		if !strings.Contains(text, want) {
			t.Errorf("mesh_reachability should include %q, got: %s", want, text)
		}
	}
}

func TestToolCall_Connectivity(t *testing.T) {
	resps := drive(t, testServer(),
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"mesh_connectivity","arguments":{}}}`)
	text := toolResultText(t, resps[0])
	// The fake snapshot's peer is in phase Error, so its verdict is Down.
	for _, want := range []string{"MeshConnectivity", "\"cluster\": \"a\"", "Down"} {
		if !strings.Contains(text, want) {
			t.Errorf("mesh_connectivity should include %q, got: %s", want, text)
		}
	}
}

func TestToolCall_UnknownTool(t *testing.T) {
	resps := drive(t, testServer(),
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"do_evil","arguments":{}}}`)
	if resps[0].Error == nil {
		t.Errorf("unknown tool should be a JSON-RPC error")
	}
}

func TestUnknownMethod(t *testing.T) {
	resps := drive(t, testServer(), `{"jsonrpc":"2.0","id":1,"method":"resources/list"}`)
	if resps[0].Error == nil || resps[0].Error.Code != codeMethodNotFound {
		t.Errorf("unknown method should return method-not-found, got %+v", resps[0].Error)
	}
}

// toolResultText extracts the text content of a successful tool result.
func toolResultText(t *testing.T, resp rpcResponse) string {
	t.Helper()
	raw, _ := json.Marshal(resp.Result)
	var res struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatalf("decoding tool result: %v", err)
	}
	if res.IsError {
		t.Fatalf("tool returned an error result: %s", raw)
	}
	if len(res.Content) == 0 {
		t.Fatalf("tool result had no content: %s", raw)
	}
	return res.Content[0].Text
}
