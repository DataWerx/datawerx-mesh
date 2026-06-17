# Ask an AI about your mesh

![Ask Claude why a cluster can't reach this one; it calls the read-only mesh_reachability tool and answers with the grounded cause and fix](images/ai-demo.gif)

*(Illustrative — the verdicts are the real output of `mesh_reachability` / `dwxctl reach`; regenerate the asset with `hack/demo/gen_ai_demo.py`.)*

Multi-cluster networking fails in confusing ways — *"why can't A reach B?"* is
the #1 support question, and the answer is scattered across peers, routes, DNS,
CIDR conflicts, and policy in several clusters at once. DataWerx is built so an
**AI agent can answer that question from live state**, because the agent already
holds the whole picture in one place.

This page shows how. It needs no managed service and no API key — the
intelligence runs in *your* AI host (Claude, an IDE assistant, a custom ops bot)
against a **read-only** server you point at your cluster.

## The idea in one minute

DataWerx keeps the open-source binary **model-free**. Instead of calling an LLM,
it publishes the mesh's state as **stable, machine-readable data contracts**, and
ships a **read-only [MCP](https://modelcontextprotocol.io) server** so any agent
can read them:

- **The snapshot** (`dwxctl snapshot`) — one versioned JSON document with every
  peer, conflict, export/import, policy, recent event, and health check.
- **The dependency graph** (`dwxctl graph`) — who peers with whom, which services
  flow between clusters, and which policies allow whom — as JSON, Graphviz, or
  Mermaid.
- **The diagnosis** (`dwxctl diagnose`) — a deterministic, rule-based "obvious
  cause" checker. Every finding **cites the exact signal it read** (a peer phase,
  a handshake age, a CIDR overlap), so an AI explanation built on top stays
  grounded in fact instead of guessing.
- **The reachability matrix** (`dwxctl reach`) — the direct answer to *"why can't
  A reach B?"*: for each remote cluster, whether it is expected to reach this one
  (Reachable / Blocked / Degraded / Unreachable) and the grounded reason — peer
  phase, a CIDR conflict, or a default-deny policy. It composes the real firewall
  compiler, so its verdict matches what the data plane programs.
- **The connectivity golden signals** (`dwxctl slo`) — reconciles that *expected*
  reachability against the *observed* tunnel liveness (the WireGuard handshake)
  into one verdict per cluster: Healthy, **Impaired** (should be reachable but the
  tunnel is dead), Blocked, Degraded, or Down. The difference between "the config
  is right" and "it's actually working".
- **The MCP server** (`dwx-mcp`) — exposes all of the above as agent tools.

That is the differentiator: the signals an AI needs to diagnose cross-cluster
connectivity (peer phase, handshake, CIDR conflict, NAT, DNS, policy) exist
**only in DataWerx's data plane**, and it hands them to your agent as a clean
contract.

> **Free to read, governed to act.** Reading the mesh is open source and runs
> anywhere. *Acting* on it — applying a fix, mutating a policy, rotating a key —
> is deliberately **not** in this binary; there is no mutating tool in `dwx-mcp`,
> by construction. The hosted "AI SRE" that proposes and opens a fix is the
> additive paid layer (see [ROADMAP.md](../ROADMAP.md)); the OSS gives you the
> grounded read surface it stands on.

## Set it up (Claude Desktop / Claude Code)

`dwx-mcp` speaks MCP over stdio and reads your cluster through your kubeconfig.
[Install the CLIs](install.md), then add the server to your MCP host config
(`claude_desktop_config.json`, or `.mcp.json` for Claude Code):

```json
{
  "mcpServers": {
    "datawerx-mesh": {
      "command": "dwx-mcp",
      "args": ["--context", "my-cluster"]
    }
  }
}
```

Restart the host and the mesh tools appear. That's it — no key, no SaaS.

## Ask it things

Once connected, ask in plain language. The agent answers from the live snapshot:

- *"Is the DataWerx mesh healthy? If not, what's the most likely cause?"*
- *"Why can't cluster `payments` reach `ledger`?"*
- *"What services does this cluster import, and from which clusters?"*
- *"Which clusters are allowed to reach me, and under which policy?"*
- *"Are any tunnels stale or any CIDRs overlapping?"*

Under the hood the agent calls these **read-only** tools, each answering from a
fresh snapshot so it never sees stale state:

| Tool | Answers |
|------|---------|
| `mesh_health` | the pass/warn/fail health report |
| `mesh_diagnose` | grounded "obvious cause" findings, most severe first |
| `mesh_reachability` | per-cluster "can it reach us, and why not" verdicts |
| `mesh_connectivity` | expected-vs-observed golden-signal verdicts (is it *working*) |
| `mesh_graph` | the cross-cluster dependency graph |
| `mesh_snapshot` | the full versioned snapshot |
| `list_peers` | peers with phase, endpoint, CIDRs, handshake age |
| `list_service_imports` / `list_service_exports` | the service flows in/out |

## Or use it without an AI

Everything the agent reads, you can read directly — the AI is a convenience over
the same contracts, not a dependency:

```sh
dwxctl verify         # pass/warn/fail health check (exits non-zero on failure)
dwxctl diagnose       # grounded "obvious cause" findings
dwxctl reach          # why can't A reach this cluster — per-peer verdicts
dwxctl slo            # connectivity golden signals — is it actually working
dwxctl snapshot       # the full JSON snapshot — pipe into jq
dwxctl graph --format mermaid   # paste into any Markdown doc to see the topology
```

## The contracts are versioned and validated

The snapshot and graph carry an `apiVersion`, and their **JSON Schemas are
published** under [`docs/contracts/`](contracts/) so a consumer can validate
what it receives. The schemas are generated from the same Go structs that emit
the JSON, so they can't drift:

```sh
dwxctl snapshot --schema    # the mesh snapshot JSON Schema
dwxctl graph --schema       # the dependency graph JSON Schema
```

## Why this is trustworthy

- **Model-free OSS.** The open-source binary never calls an LLM; it produces a
  data contract. Your topology and flows aren't sent anywhere by reading them.
- **Grounded.** `dwxctl diagnose` (and the `mesh_diagnose` tool) cite the
  concrete signal behind every finding — the contract any AI explanation must
  stay anchored to.
- **Read-only by construction.** The free MCP server exposes zero mutating
  tools; a unit test enforces it so the boundary can't erode by accident.

## Going deeper

- [Design 0005 — mesh snapshot & diagnosis](design/0005-mesh-snapshot.md)
- [Design 0007 — the read-only MCP server](design/0007-mesh-mcp-server.md)
- [Design 0009 — the dependency graph](design/0009-mesh-dependency-graph.md)
- [Published contract schemas](contracts/)
- [Install the CLIs](install.md)
