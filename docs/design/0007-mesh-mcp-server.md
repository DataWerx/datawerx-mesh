# Design 0007 — Read-only MCP server for the mesh

- Status: **Implemented (read-only).**
- Milestone: **M5 (Intelligence & adoption).**
- Command: `cmd/dwx-mcp`.
- Implements 0004 §3.3.

## Summary

Expose the mesh's read-state as a [Model Context Protocol](https://modelcontextprotocol.io)
server, so any MCP-speaking agent (Claude, an IDE assistant, a custom ops bot)
can ask "what does cluster B import?" or "is the mesh healthy, and why not?"
against live cluster state.

It is the textbook expression of the open-core seam (0004 §2.4): **reading mesh
state is the free, adoption-driving surface; acting on the mesh is the governed,
audited, paid surface.** This server exposes *only* read tools. There is no tool
here that mutates the mesh, by construction.

## Tools (all read-only)

Every tool answers from a freshly gathered `verify.Snapshot` (0005), so the MCP
view and `dwxctl snapshot` share one source of truth and can never drift:

| Tool | Returns |
|------|---------|
| `mesh_snapshot` | the full versioned snapshot JSON |
| `mesh_health` | the pass/warn/fail health report |
| `mesh_diagnose` | the rule-based grounded findings (most severe first) |
| `mesh_graph` | the mesh dependency graph (clusters, services, edges) — see 0009 |
| `list_peers` | peers with phase, endpoint, CIDRs, handshake age |
| `list_service_imports` | imported services (type, ClusterSetIP, ports, clusters) |
| `list_service_exports` | exported services and their validity/conflict state |

The **act** MCP — apply a fix, mutate a `MeshNetworkPolicy`, rotate a key — is the
premium counterpart. It lives in the hosted plane behind SSO/RBAC and **audit
logging**, and is never compiled into this binary. A unit test asserts the free
build exposes zero mutating-sounding tools, so the seam can't erode by accident.

## Transport & implementation

The transport is the standard MCP **stdio** framing: newline-delimited JSON-RPC
2.0 messages on stdin/stdout. The server implements `initialize`,
`notifications/initialized`, `ping`, `tools/list`, and `tools/call` directly —
no MCP SDK — to keep the OSS binary tiny and dependency-light, consistent with
the rest of the agent. (`initialize` advertises only the `tools` capability; no
resources, no prompts, nothing that mutates.)

```
MCP client ──stdin (JSON-RPC)──►  dwx-mcp  ──► internal/meshstate.Snapshot ──► K8s API
           ◄──stdout (JSON-RPC)─           ◄── verify.Snapshot / Diagnose
```

The Kubernetes client is built lazily on the first tool call (so the process can
start outside a cluster, e.g. while an MCP host is wiring it up) and a fresh
snapshot is gathered per call, so answers are always current. A gather failure is
returned as a tool error (`isError`) rather than a transport fault, so the agent
sees a usable message.

## Structure

- `cmd/dwx-mcp/main.go` — flag parsing and process wiring.
- `server.go` — the stdio JSON-RPC loop and method dispatch; the snapshot source
  is injectable so the dispatch logic is unit-testable with no cluster.
- `rpc.go` — JSON-RPC types and the MCP result shapes.
- `tools.go` — the read-only tool registry; each tool is a pure
  `func(Snapshot) (string, error)`.

All cluster reads go through `internal/meshstate`, the same shell `dwxctl` uses.

## Open-core boundary

Free: this read-only server. Paid: the governed/audited act MCP in the hosted
plane, consuming the same contracts over the `EnterpriseControlPlaneClient`
channel. Free-read / paid-governed-act is the cleanest seam in the whole product
(0004 §2.4); this is where it's drawn most sharply.

## Testing

- `cmd/dwx-mcp` — dispatch tests with an injected fake snapshot: `initialize`
  advertises the protocol and tools capability, a notification yields no
  response, `tools/list` exposes only read-only tools, `tools/call` for
  `mesh_diagnose`/`list_service_imports` returns grounded content, and unknown
  methods/tools return the right errors. No cluster.
- Manual/e2e (future) — connect an MCP client to a kind mesh and confirm it can
  answer health/topology/import questions sourced from the live snapshot.

## Scope / non-goals

- No `resources` or `prompts` MCP surfaces yet (tools cover the need).
- No streaming/SSE transport; stdio is the right fit for a CLI-launched server.
- No write tools, ever, in this binary — that's the premium act MCP.
