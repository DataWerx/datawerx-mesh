# Design 0014 — DataWerx Signal (grounded AI over the mesh)

- Open-core PoC landed (`pkg/signal`, `cmd/dwx-signal`);
  the premium control-plane service is not started.
- Implemented by `pkg/signal` (the grounded-evidence
  assembly) and the `dwx-signal` CLI, mirroring the control plane's
  `internal/signal`. Nothing in the module imports `os/signal`, so the package
  name is unambiguous; `pkg/slo.Signal` is a distinct, qualified type.
- Packages: `pkg/signal` is a pure, OSS, grounding contract, `cmd/dwx-signal`
  (read-only CLI, OSS), and the **premium** managed AI-ops service in
  `datawerx-admin` (control-plane-side; not yet built).
- Read-only MCP server - the intent/impact dry-run planner,
  reachability, and the connectivity golden signals.

## Summary

DataWerx Signal turns the mesh's deterministic read surfaces into a grounded,
natural-language question-answering and AI-operations layer. The name is literal:
every read surface already emits **grounded signals** — each `verify.Diagnose`
finding cites the `Signal` it read; `pkg/slo` reconciles the connectivity
**golden signals**. Signal (the product) is the layer that reasons over those
signals and never beyond them.

The defining principle is **grounded AI**: the model reasons only over Evidence
assembled from the deterministic engines (snapshot/diagnose/reach/slo), and
every claim must cite the signal it came from. The model selects, ranks, and
explains; it never invents peers, IPs, metrics, or events. That is what makes an
AI answer trustworthy enough to act on in infrastructure.

## Four Dimensions

Open-core rule: *reading mesh state is the free,
adoption-driving surface; acting on the mesh is the governed, audited, paid
surface.* Signal honours that, but the AI layer has more states than read/write,
so the seam is four dimensions. A capability is **free only if it is all four**:

| Dimension | Free (open core) | Premium (control plane) |
|---|---|---|
| Mutation | reads / explains / simulates | **writes** the mesh |
| Scope | a single cluster (the agent's ego-centric view) | **cross-cluster / fleet** |
| Time | point-in-time snapshot | **historical** (timelines, drift) |
| Inference | bring-your-own key, runs locally | **managed** (hosted, RBAC + audit) |

Three of these are forced by where the data physically lives: the open-core
agent is **ego-centric and stateless**, so fleet-wide and historical AI
cannot live in it — they require the control plane (`datawerx-admin`), which already owns Postgres, SSO/RBAC, and audit.

**Rule:** *Free = read-only, single-cluster, point-in-time, BYO-inference.
Premium = anything that writes, spans clusters, looks back in time, or is
managed.*

## Plan is free, apply is premium

The actionable seam is already half-builtin `pkg/impact` and computes a
proposed change's blast radius and is **deliberately not exposed via MCP**. So
for intent-based networking ("connect inventory across AWS and GCP"):

- Signal translates the intent into proposed `ServiceExport` / `ServiceImport`
  manifests and shows the diff and blast radius via `pkg/impact` → **free**
  (generation + dry-run is still read-only).
- Signal **applies** it, and runs the closed loop ("keep p99 < 100 ms") →
  **premium** (write + telemetry + governance).

This is the adoption funnel: a free user can ask, diagnose, and even *see the
proposed change*; they pay to **apply** it, to go **fleet-wide**, to look **back
in time**, or to have it **managed and governed**.

## Tiering
| Capability | Tier | Where |
|---|---|---|
| Single-cluster snapshot / graph / evidence | Free | `pkg/verify`, `pkg/meshgraph`, `pkg/signal` |
| Read-only Q&A & root cause (point-in-time, BYO key) | Free | `cmd/dwx-signal`, `dwx-mcp` |
| Reachability "why can't A reach B", connectivity SLO | Free | `pkg/reach`, `pkg/slo` |
| Intent → proposed manifests + blast radius (dry-run) | Free | `pkg/impact` + Signal |
| Cross-cluster / fleet graph with Q&A | Premium | `datawerx-admin` |
| Incident history, "explain the outage at 2:13 PM", drift | Premium | `datawerx-admin` (needs persistence) |
| Managed inference (no BYO key, RBAC + audit per question) | Premium | `datawerx-admin` |
| Apply a change / create-export / failover policy | Premium | `datawerx-admin` (governed write) |
| Intent-based networking - apply continuous optimization | Premium | `datawerx-admin` |
| Write MCP tools (`create_export`, `create_failover_policy`) | Premium | premium MCP surface (not `dwx-mcp`) |

`dwx-mcp` stays read-only by construction (0007). Write tools live in the premium
surface, never in the OSS binary.

## Architecture

`pkg/signal` is the shared grounding contract: pure `Evidence`, `RootCause`,
the system prompt, the response schema, and the Messages-API request/response
shaping. It's the OSS half, exactly as `pkg/edge` is the free contract for
premium edge. The lone impure edge (the model call) is an
injectable-transport `Client`; free `dwx-signal` uses it with a
bring-your-own key, and `--print-context` exposes the exact grounded evidence with no key at all.

Premium Signal is a managed service in `datawerx-admin`. The SaaS the enterprise agent already talks to over `/api/v1`. It:

- **aggregates** many clusters' evidence into a fleet view. 
- **persists** evidence over time for incident timelines and drift detection,
- runs **managed inference** with per-question/answer **RBAC and audit**
- gates **actions** (apply / intent / optimisation) behind the same governed,
  audited write path the control plane already owns.

Because `datawerx-admin` shares no code with the agent, the premium service does **not** import `pkg/signal`; it mirrors
the `Evidence` / `RootCause` shapes as a contract — the same discipline
`internal/topology.RemotePeerConfig` already follows. Keep the shapes in lockstep and guard them with a compatibility test, as the agent API is guarded today.

## Read-only and grounded, always

Signal answers; it does not silently mutate. Even premium *actions* are explicit,
governed, and audited — never a side effect of a question. And every answer, free
or premium, must cite the evidence signal behind each claim; an answer with empty
citations is, by contract, untrusted.
