# Design 0009 — Mesh dependency graph

- Status: **Implemented (the free graph).**
- Milestone: **M5 (Intelligence & adoption).**
- Package: `pkg/meshgraph` (pure), surfaced by `dwxctl graph` and the `dwx-mcp`
  `mesh_graph` tool.
- Implements 0004 §4 item #4 (Service dependency map) — the free half.

## Summary

Turn the mesh snapshot (0005) into the cross-cluster dependency graph: this
cluster, the clusters it peers with, and the services that flow between them.
Pure logic, no model, exhaustively testable — and emitted three ways: a stable
JSON artifact, Graphviz DOT, and a Mermaid flowchart.

The agent already holds, in one place, the two things a dependency map needs and
no competitor has together: the cross-cluster topology (who peers with whom) and
the service dependency graph (who exports, who imports, and from which clusters
an import is served). The snapshot makes that state legible as data; this turns
it into the picture an operator actually draws on a whiteboard when "A can't
reach B" — except generated, current, and never out of date.

## Ego-centric by construction

A snapshot is one cluster's observed view of the mesh, so the graph is
**ego-centric**: a fixed `local` node at the center, the peer clusters around it,
and the imported/exported services. The snapshot carries no identity for the
cluster it was taken from, so `local` is a stable synthetic node
(`cluster/local`) rather than a discovered cluster ID. A fleet-wide graph that
stitches every cluster's ego-graph into one topology is a hosted concern (see
open-core boundary), not something a single snapshot can or should produce.

## The graph

`meshgraph.Graph` (`apiVersion: mesh.datawerx.io/graph/v1alpha1`,
`kind: MeshGraph`) is nodes and edges:

**Nodes**

| Kind | From | Carries |
|------|------|---------|
| `cluster` | the local cluster + each `MeshPeer` | phase, endpoint, conflict flag |
| `service` | each imported/exported service | namespace, import/export flags, import type, ClusterSetIP(s) |

A service that is both imported and exported merges into one node.

**Edges**

| Kind | Direction | Meaning |
|------|-----------|---------|
| `peering` | local → peer | a WireGuard peering, annotated with the peer's phase and a conflict flag |
| `serves` | peer → service | that peer contributes backends to a service the local cluster imports |
| `imports` | service → local | the local cluster imports the service |
| `exports` | local → service | the local cluster exports the service to the mesh |
| `allows` | source → local | a `MeshNetworkPolicy` permits that cluster as an ingress source, annotated with the policy name |

So a reachable import reads `peerEast —serves→ payments —imports→ local`, and an
export reads `local —exports→ ledger`. A provider cluster referenced only by an
import (no `MeshPeer` of its own) still gets a node, so a `serves` edge is never
dangling.

### Policy reachability

`allows` edges turn the policy set into a reachability picture: for each
`MeshNetworkPolicy`, every cluster named as an ingress source gets an edge into
the local cluster, labeled with the policy. CIDR-only sources are not mesh
clusters, so they produce no edge. A selector naming a cluster with no `MeshPeer`
still gets a node — but no `peering` edge, which is the visual tell that an
*allowed* source is not actually *peered* (the rule resolves to nothing, exactly
what `dwxctl policy --dry-run` warns about). Two policies allowing the same
source produce two edges, so no authorization is hidden by deduplication. The
sources come from the snapshot's `PolicySnapshot.Ingress`, added in 0005's schema
for this purpose.

### Conflicts

A peering edge (and its peer node) is flagged `conflict` when the peer's cluster
ID is named by a topology conflict from the same pure
`topology.DetectTopologyConflicts` the syncer and snapshot use. The detector
canonicalizes an overlap to one of the two cluster IDs, so this catches the
canonical side; `dwxctl diagnose` and the snapshot remain authoritative for the
full conflict detail. The graph is the *picture*, not the source of truth.

## Renderings

`Build` is the only logic; the three outputs are pure functions of the graph, so
they are byte-stable for a given snapshot:

- **JSON** — the stable artifact other tooling consumes (Backstage, a dashboard,
  a diff over time). Round-trips.
- **DOT** — pipe straight into `dot -Tsvg`. Clusters are boxes (the local one
  filled), services are ellipses, and a pending/error/conflicted peering edge is
  colored so trouble is visible at a glance.
- **Mermaid** — renders inline in GitHub/GitLab/Backstage and most wikis, so a
  live mesh diagram can live in a README or runbook with no Graphviz toolchain.

Determinism comes from `Build`: nodes are sorted by id, edges by kind then
endpoints, so the artifact diffs cleanly across snapshots over time and the
tests are hermetic.

## Composition, not new semantics

`pkg/meshgraph` owns no topology or service semantics of its own — it reads the
snapshot's already-resolved peers, imports, exports, and conflicts and arranges
them. That keeps it consistent with the data plane (same conflict detector, same
import resolution) and side-effect-free, the same discipline as `pkg/topology`,
`pkg/verify`, and `pkg/impact`. It never touches Kubernetes; the CLI and MCP
server gather the snapshot through `internal/meshstate` and hand it in.

## Surface

- `dwxctl graph [--format json|dot|mermaid]` — render the graph from live
  cluster state. JSON is the default.
- `dwx-mcp` `mesh_graph` tool — returns the graph JSON, so an agent can ask for
  the dependency structure directly. Read-only, like every tool in that binary.

Both build the graph from the same `verify.Snapshot` every other read command
uses, so the graph can never disagree with `dwxctl snapshot`.
`dwxctl graph --schema` prints the graph's JSON Schema (published under
`docs/contracts/`, generated from the Go structs so it cannot drift).

## Open-core boundary

Free: this structural graph artifact and its renderings. Paid: the hosted,
fleet-wide topology UI that ingests every cluster's snapshot to stitch the
ego-graphs into one live map, with history, drift detection ("this peering
disappeared yesterday"), and time-travel — exactly the §4 #4 paid seam. The OSS
binary only ever *produces* the contract; it never aggregates a fleet or stores
history.

## Testing

- `pkg/meshgraph` — table-driven unit tests: envelope/versioning, the
  always-present local node, peer and peering edges (phase + conflict), import
  fan-in from multiple providers, the export edge, an import provider with no
  `MeshPeer`, import+export node merging, order-independence (shuffled input →
  identical JSON), and the DOT/Mermaid renderings (structure, styling, label
  escaping, determinism, empty-mesh validity). No cluster.
- `cmd/dwx-mcp` — a dispatch test asserts `mesh_graph` returns a graph carrying
  the local node, the peer, and the imported service from the injected snapshot.

## Scope / non-goals

- **Per-destination policy edges.** `allows` edges land on the local cluster
  node, not on individual destination CIDRs — the graph answers "which clusters
  may reach this one, and under which policy", not "which clusters may reach this
  exact /24". Destination-granular edges are a future refinement; the snapshot
  now carries the destinations and ports to support it.
- **Fleet-wide stitching / history / drift** — hosted, per the boundary above.
- **Live metric values on edges** (throughput, handshake age as edge weight) —
  the snapshot carries metric pointers, not values (0005); deferred with it.
