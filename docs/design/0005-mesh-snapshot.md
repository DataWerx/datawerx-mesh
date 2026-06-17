# Design 0005 — Mesh state snapshot & rule-based diagnosis

- Status: **Implemented.**
- Milestone: **M5 (Intelligence & adoption).**
- Package: `pkg/verify` (pure), surfaced by `cmd/dwxctl` and `cmd/dwx-mcp`.
- Builds the keystone artifact described in `docs/design/0004-intelligence-and-adoption.md` §3.1.

## Summary

A single, versioned, machine-readable snapshot of the whole mesh's observed
state, assembled as a pure function and emitted as stable JSON. It is two things
at once — a day-2 observability hook an operator can pipe into `jq`, and the
input contract every higher-layer feature consumes (the diagnosis checker here,
the MCP server in 0007, and the hosted plane later). One free data contract,
several payoffs.

The agent already sits at a vantage point no competitor has in one place: every
node sees the cross-cluster topology, the live data plane it programs, the
service dependency graph (who exports/imports what), connectivity health
(handshakes, CIDR conflicts), and policy. The snapshot makes that structured
state a first-class, stable artifact instead of something you reconstruct by
hand from `kubectl get` across clusters.

## What's in the snapshot

`verify.Snapshot` (`apiVersion: mesh.datawerx.io/snapshot/v1alpha1`,
`kind: MeshSnapshot`) carries:

- **Health** — the same pass/warn/fail report `dwxctl verify` renders, embedded
  so the snapshot is a strict superset of the health check.
- **Peers** — each `MeshPeer`'s cluster ID, endpoint, phase, advertised CIDRs,
  truncated public key, and computed handshake age.
- **Conflicts** — the advertised-topology conflicts from the *same* pure
  `topology.DetectTopologyConflicts` the syncer uses (overlaps, duplicate IDs,
  shared keys).
- **Exports / Imports** — `ServiceExport` validity/conflict state and the
  resolved `ServiceImport`s (type, ClusterSetIP(s), ports, contributing
  clusters).
- **Policies** — each `MeshNetworkPolicy`'s destinations, phase, rule count, and
  the resolved ingress sources (cluster IDs + CIDRs) and ports per rule, so a
  consumer can answer "which clusters may reach this one?" — the input the
  dependency graph's policy-reachability edges (0009) are built from.
- **Events** — recent Warning events touching mesh objects, as corroborating
  signal.
- **Metrics** — pointers to the relevant Prometheus series so a consumer knows
  where to look.

### Stability & determinism

The schema is versioned in an `apiVersion`-style field so consumers (and a
future hosted plane) can branch on it as it evolves. `BuildSnapshot` sorts every
collection and truncates key material, so the JSON is byte-stable for a given
input regardless of the order the API returned objects in — important for diffing
snapshots over time and for hermetic tests.

### Never carries secrets

Public keys are truncated (`shortKey`); private keys, SSO tokens, and full key
material never appear. This mirrors the project's logging discipline.

## Assembly is pure

`BuildSnapshot(SnapshotInputs) Snapshot` is side-effect-free and lives in
`pkg/verify` next to the health report it embeds, so the whole thing is
exhaustively table-tested with no Kubernetes and no kernel. The Kubernetes reads
live in `internal/meshstate.Snapshot`, a thin shell shared by every surface so
they can never disagree about what the mesh looks like. This is the same
pure-core/thin-shell split the reconcilers follow.

## Rule-based diagnosis (the free "obvious cause" checker)

`verify.Diagnose(Snapshot) []Finding` is the deterministic floor under the
hosted AI diagnosis (0004 §3.4): given a snapshot it returns the grounded reasons
the mesh is unhealthy — a CIDR overlap, a dead tunnel, an invalid export, a
failed policy — **each finding citing the concrete signal it read** (a phase, a
handshake age, a conflict reason). There is no model here and there never will
be; the `Signal` field is the contract any explanation must stay grounded in, so
when the paid LLM layer arrives it inherits the same grounding requirement rather
than inventing one.

Findings are ranked (critical first) and each carries a suggested remedy keyed to
the kind of fault (e.g. an overlap suggests renumbering or `DataWerx_REMAP_CIDR`).

## Surfaces

- `dwxctl snapshot` — emit the snapshot JSON.
- `dwxctl verify [--output json]` — the health report (the snapshot's embedded
  `Health` block).
- `dwxctl diagnose [--output json]` — run the checker; exits non-zero on a
  critical finding so it can gate a pipeline.
- The read-only MCP server (0007) exposes all three as tools.
- `dwxctl snapshot --schema` prints the contract's JSON Schema, published under
  `docs/contracts/` and generated from the Go structs so it cannot drift.

## Open-core boundary

Everything here is free and model-free. The hosted plane consumes snapshots
fleet-wide over the existing `EnterpriseControlPlaneClient` / telemetry seam for
history, cross-cluster rollups, and as the input to the AI SRE — but the OSS
binary only ever *produces* the contract; it never calls a model. This is the
prime directive of 0004 §2: data contract in OSS, model in the SaaS.

## Testing

- `pkg/verify` — table-driven unit tests for assembly (versioning, determinism,
  key truncation, handshake age) and for diagnosis (overlap is critical and
  cited, stale/never handshake, healthy mesh is quiet, severity ordering). No
  K8s.
- e2e (future) — `dwxctl snapshot` on the two-cluster kind harness emits valid,
  schema-versioned JSON capturing peers, a synthetic CIDR conflict, an
  export→import, and a policy.

## Out of scope (tracked separately)

- Fleet-wide snapshot ingestion/history (premium, server-side).
- Scraping live metric *values* into the snapshot (pointers only for now).
- The hosted LLM diagnosis that consumes this contract (0004 §3.4, private repo).
