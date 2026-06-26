package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/DataWerx/datawerx-mesh/internal/meshstate"
	"github.com/DataWerx/datawerx-mesh/pkg/verify"
)

// protocolVersion is the MCP revision this server speaks.
const protocolVersion = "2024-11-05"

const serverName = "dwx-mesh"

// serverConfig holds the cluster-access settings.
type serverConfig struct {
	namespace   string
	daemonset   string
	kubeconfig  string
	kubecontext string
}

// server is the read-only MCP server. Snapshot is injectable so the dispatch
// logic is unit-testable without a cluster; in production it lazily builds a
// client and gathers live state.
type server struct {
	cfg      serverConfig
	snapshot func(context.Context) (verify.Snapshot, error)

	mu     sync.Mutex
	client client.Client
}

func newServer(cfg serverConfig) (*server, error) {
	if cfg.namespace == "" {
		cfg.namespace = meshstate.DefaultNamespace
	}
	if cfg.daemonset == "" {
		cfg.daemonset = meshstate.DefaultDaemonSet
	}
	s := &server{cfg: cfg}
	s.snapshot = s.liveSnapshot
	return s, nil
}

// liveSnapshot lazily builds the client (so the process can start outside a
// cluster) and gathers a fresh snapshot per call — the MCP client always sees
// current state.
func (s *server) liveSnapshot(ctx context.Context) (verify.Snapshot, error) {
	s.mu.Lock()
	if s.client == nil {
		c, err := meshstate.NewClient(s.cfg.kubeconfig, s.cfg.kubecontext)
		if err != nil {
			s.mu.Unlock()
			return verify.Snapshot{}, err
		}
		s.client = c
	}
	c := s.client
	s.mu.Unlock()

	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return meshstate.Snapshot(cctx, c, s.cfg.namespace, s.cfg.daemonset)
}

// Serve reads newline-delimited JSON-RPC messages from in and writes responses
// to out until in is exhausted (the MCP stdio transport).
func (s *server) Serve(ctx context.Context, in io.Reader, out io.Writer) error {
	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	enc := json.NewEncoder(out)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		resp, ok := s.handle(ctx, line)
		if !ok {
			continue // a notification — no response is written
		}
		if err := enc.Encode(resp); err != nil {
			return fmt.Errorf("writing response: %w", err)
		}
	}
	return scanner.Err()
}

// handle dispatches one request. The second return is false for notifications
// (no response is owed).
func (s *server) handle(ctx context.Context, line []byte) (rpcResponse, bool) {
	var req rpcRequest
	if err := json.Unmarshal(line, &req); err != nil {
		return errorResponse(nil, codeParseError, "invalid JSON"), true
	}
	// A request with no id is a notification: act on it but never reply.
	isNotification := len(req.ID) == 0

	switch req.Method {
	case "initialize":
		return okResponse(req.ID, initializeResult()), true
	case "notifications/initialized":
		return rpcResponse{}, false
	case "ping":
		return okResponse(req.ID, map[string]any{}), true
	case "tools/list":
		return okResponse(req.ID, map[string]any{"tools": toolDescriptors()}), true
	case "tools/call":
		return s.handleToolCall(ctx, req), true
	default:
		if isNotification {
			return rpcResponse{}, false
		}
		return errorResponse(req.ID, codeMethodNotFound, "unknown method: "+req.Method), true
	}
}

// handleToolCall runs a read-only tool against a fresh snapshot.
func (s *server) handleToolCall(ctx context.Context, req rpcRequest) rpcResponse {
	var params struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return errorResponse(req.ID, codeInvalidParams, "invalid tool params")
	}
	tool, ok := tools[params.Name]
	if !ok {
		return errorResponse(req.ID, codeInvalidParams, "unknown tool: "+params.Name)
	}

	snap, err := s.snapshot(ctx)
	if err != nil {
		// A gather failure is reported as a tool error (not a protocol error) so
		// the agent sees a usable message rather than a transport fault.
		return okResponse(req.ID, toolError(fmt.Sprintf("failed to read mesh state: %v", err)))
	}
	text, err := tool.run(snap)
	if err != nil {
		return okResponse(req.ID, toolError(err.Error()))
	}
	return okResponse(req.ID, toolText(text))
}
