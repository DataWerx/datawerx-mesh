# Design 0001 — Cross-cluster DNS via the MCS API

- Status: **Draft**
- Milestone: **M1**
- Tracking: see `ROADMAP.md`

## Summary

Make a Kubernetes `Service` in one mesh cluster resolvable and reachable **by
name** from every other cluster, using the upstream **Multi-Cluster Services
(MCS) API** (KEP-1645): `ServiceExport`, `ServiceImport`, and the
`*.clusterset.local` DNS zone.

This is the single most important free-tier feature after raw connectivity:
without it, callers must hardcode ephemeral pod/cluster IPs, which is unusable
in production. Every credible competitor (Submariner Lighthouse, Cilium
ClusterMesh, KubeSlice) ships cross-cluster discovery for free; so do we.

## Why MCS (and not a bespoke scheme)

- **Ecosystem compatibility.** `ServiceExport`/`ServiceImport` and
  `clusterset.local` are a SIG-Multicluster standard. Adopting them means our
  users, and tools that already understand MCS, get a drop-in experience —
  exactly how Submariner Lighthouse behaves.
- **No new mental model.** Users already know `Service`. Exporting is a marker
  CRD with the *same name/namespace* as the `Service`; importing is automatic.
- **Future-proof.** Aligns us with where Kubernetes multi-cluster is going.

We implement our own thin copies of the types (under the standard
`multicluster.x-k8s.io` group) to honor this repo's "hand-written types, no
codegen" convention, while remaining wire- and DNS-compatible with the spec.

## User-facing behavior

```yaml
# In cluster A — export an existing Service named "payments" in ns "prod":
apiVersion: multicluster.x-k8s.io/v1alpha1
kind: ServiceExport
metadata:
  name: payments      # MUST match the Service name
  namespace: prod
```

From a pod in **any** mesh cluster:

```
payments.prod.svc.clusterset.local   →  ClusterSetIP (or headless pod IPs)
```

The name resolves to a virtual **ClusterSetIP** (for normal Services) or to the
union of backing pod IPs (for **Headless** Services), and the WireGuard data
plane already routes those IPs across the mesh.

## Architecture

```
        Cluster A (exporting)                         Cluster B (importing)
 ┌────────────────────────────────┐           ┌──────────────────────────────────┐
 │ Service "payments" (prod)      │           │                                  │
 │ ServiceExport "payments"       │           │                                  │
 │            │                   │           │                                  │
 │   Export controller            │           │                                  │
 │   - validates the Service      │           │                                  │
 │   - resolves type/ports/IPs    │           │                                  │
 │   - publishes an               │           │                                  │
 │     ExportedEndpoint via the   │   control │  Import controller               │
 │     ControlPlaneClient ────────┼─ plane  ──┼─► aggregates all clusters'       │
 │     (CRD in free tier,         │  (CRDs    │   exports for payments/prod      │
 │      SaaS in premium)          │  or SaaS) │   via dns.PlanServiceImport()    │
 │                                │           │            │                     │
 │                                │           │   writes ServiceImport "payments"│
 │                                │           │            │                     │
 │                                │           │   CoreDNS clusterset.local plugin│
 │                                │           │   answers <svc>.<ns>.svc.        │
 │                                │           │   clusterset.local from the      │
 │                                │           │   ServiceImport                  │
 └────────────────────────────────┘           └──────────────────────────────────┘
```

### Components

1. **API types** (`pkg/apis/multicluster/v1alpha1`) — `ServiceExport`,
   `ServiceImport` (+ scheme + hand-written deepcopy). **Drafted in this PR.**

2. **Pure aggregation logic** (`pkg/dns`) — `PlanServiceImport()` merges the
   per-cluster `ExportedEndpoint`s for one `<name, namespace>` into a single
   desired `ServiceImport` (resolved type, merged ports, per-cluster IPs) and
   reports MCS **conflicts** (type mismatch, port disagreement). Side-effect
   free, table-tested — same pattern as `topology.PlanPeer`. **Drafted in this
   PR** (`naming.go` + tests).

3. **Export controller** (`pkg/controllers`, next PR) — watches `ServiceExport`
   + the referenced `Service`; computes that cluster's `ExportedEndpoint` and
   publishes it through the **same `ControlPlaneClient` seam** used for peers
   (local CRDs in free, SaaS in premium). Sets `ServiceExport` status
   conditions (`Valid`, `Conflict`).

4. **Import controller** (`pkg/controllers`, next PR) — for each exported
   `<name, namespace>` visible across the mesh, calls `dns.PlanServiceImport`
   and reconciles a local `ServiceImport` (cluster-local). Idempotent; cleans
   up when the last export disappears.

5. **DNS integration** (next PR) — a CoreDNS plugin (or generated zone) that
   answers the `clusterset.local` zone from `ServiceImport` objects. Start with
   a CoreDNS configuration + a lightweight resolver backed by the informer
   cache; a native plugin can follow.

### The `EndpointExport` wire format (tier-agnostic integration point)

Rather than coupling the controllers to the `ControlPlaneClient` interface, the
export→import hop uses a dedicated CRD, **`EndpointExport`**
(`networking.datawerx.io`), exactly mirroring how `MeshPeer` carries peer
topology:

- **Free tier:** the export controller writes one `EndpointExport` per exported
  service (named `<cluster>-<service>`), and the user's GitOps pipeline mirrors
  `EndpointExport`s between clusters. No broker, no central service.
- **Premium tier:** the SaaS syncer materializes remote `EndpointExport`s
  locally (just like the topology syncer materializes `MeshPeer`s).

Either way the **import controller only ever reads `EndpointExport` objects**, so
its logic — and the pure aggregation/allocation in `pkg/dns` — is identical
across tiers, the core design rule. This is simpler and more
consistent than threading a second concern through `ControlPlaneClient`.

### Consistent, broker-less ClusterSetIP allocation

A ClusterSetIP must be the **same in every cluster** or routing breaks, yet we
have no broker to coordinate. `dns.AllocateClusterSetIPs` solves this as a
**pure function of the CIDR and the full sorted set of service keys**: each
service hashes to an offset in the range with deterministic linear probing.
Every cluster that observes the same set of `EndpointExport`s independently
computes the identical mapping — consistent allocation with zero coordination.
Default range `241.0.0.0/8` (reserved Class E, no collision with cluster CIDRs).

## ClusterSetIP allocation

Normal (non-headless) Services get a stable virtual **ClusterSetIP** per
imported service, carved from a dedicated mesh range (default `241.0.0.0/8`,
configurable) that does not collide with cluster pod/service CIDRs. Allocation
is deterministic and conflict-checked; the address is programmed into the data
plane like any other remote CIDR. (Headless Services skip this and resolve to
the union of pod IPs.) Detailed allocator design is deferred to the import
controller PR; the API and aggregation already carry `Type` and `IPs`.

## Conflict handling (per MCS)

When clusters export the same `<name, namespace>` with incompatible
definitions:

- **Type** (ClusterSetIP vs Headless) must agree. On mismatch, the aggregator
  picks a deterministic winner (lowest cluster ID) and flags the others; the
  export controller surfaces a `Conflict` condition on the losing
  `ServiceExport`s.
- **Ports** are merged as a set keyed by `(port, protocol)`; a key with
  conflicting names is a conflict, resolved deterministically and surfaced.

`dns.PlanServiceImport` computes both the resolved plan and the conflict list so
the controllers only perform side effects.

## Testing

- `pkg/dns` — table-driven unit tests for naming and aggregation/conflicts
  (no K8s), 100% target, mirroring `pkg/topology`.
- Controllers — envtest + fake `ControlPlaneClient` (M2).
- e2e — exported service reachable by name across two `kind` clusters (M2).

## Out of scope (tracked separately)

- CoreDNS native plugin packaging (config-based first).
- ClusterSetIP allocator internals (import-controller PR).
- Global/hierarchical discovery (premium).
- SessionAffinityConfig propagation (kept minimal in the draft API).
