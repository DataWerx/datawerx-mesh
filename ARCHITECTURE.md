# Architecture

DataWerx Mesh is a Kubernetes operator that links clusters over WireGuard and
makes services discoverable across them. This document is the mental model: the
moving parts, how data flows, and the design rules the code holds to.

## TL/DR

A per-node DaemonSet agent watches a small set of CRDs and converges the node's
kernel data plane — peer connectivity using its own WireGuard device *or* host routes
over an overlay you already run, plus iptables NAT and firewall rules — toward
the declared desired state, identically in the free and premium tiers.

> New to the project? Read [docs/how-it-works.md](docs/how-it-works.md) first for
> the packet-level story; this file is the component/design reference.

## The design rules (read these first)

1. **Pure logic vs. side effects.** Every "compute desired state from inputs"
   decision lives in a side-effect-free package (`pkg/topology`, `pkg/dns`,
   `pkg/nat` planners, `pkg/verify`). These are exhaustively unit-tested with no
   Kubernetes and no kernel. Reconcilers and managers are thin shells that fetch
   inputs, call the pure function, and perform side effects.
2. **One open/closed seam.** The free and premium tiers are separated by the
   single `ControlPlaneClient` interface (`pkg/client`). The reconcile loop never
   branches on tier. Premium is additive behind this seam.
3. **Tier-agnostic CRDs are the integration points.** `MeshPeer` and
   `EndpointExport` are written by the free GitOps path *or* by a premium SaaS
   syncer; the reconcilers that consume them are identical either way.
4. **Declarative, drift-proof side effects.** Managers reconcile the kernel to a
   full desired state (idempotent, self-healing) rather than applying deltas.

## Components

```
cmd/manager        Agent entry point: tier selection, WG bring-up, controller wiring, signals.
cmd/dwxctl         Operator CLI (`verify`).

pkg/apis/…         CRD types (+ hand-written deepcopy, no codegen):
  networking.datawerx.io   MeshPeer, EndpointExport, MeshNetworkPolicy
  multicluster.x-k8s.io    ServiceExport, ServiceImport (MCS, KEP-1645)

pkg/client         ControlPlaneClient seam: LocalGitOpsClient (free) + EnterpriseControlPlaneClient (premium).
pkg/syncer         Premium topology syncer (mirrors remote topology into MeshPeer CRDs).
pkg/topology       PURE: peer desired-state (CIDR overlap partition, phase, virtual-CIDR remap).
pkg/dns            PURE: MCS aggregation, ClusterSetIP allocation (v4+v6), clusterset.local naming/resolution.
pkg/nat            ClusterSetIP DNAT + overlap NETMAP: PURE planners + iptables/ip6tables applier.
pkg/meshfw         Cross-cluster network policy: PURE firewall compiler + iptables applier.
pkg/wg             WireGuard data plane: wgctrl (crypto) + netlink (link/routes), thread-safe.
pkg/routed         BYO-overlay data plane: host routes over an existing overlay (no WireGuard device).
pkg/dataplane/ebpf Premium TC/eBPF overlap-remap backend (pure map planner; kernel loader is the paid build).
pkg/dnsserver      Authoritative clusterset.local responder (miekg/dns) + cache-backed resolver.
pkg/metrics        Prometheus instrumentation (cache-backed state collector + event metrics).
pkg/verify         PURE: dwxctl health-check report logic.
pkg/controllers    Thin reconcilers tying the above together.
```

`pkg/wg` and `pkg/routed` both satisfy the `controllers.PeerDataPlane` interface,
so the agent picks one at startup (`DataWerx_DATAPLANE`) and the reconciler is
identical either way.

## Data flow — connectivity

```
control plane (CRDs free / SaaS premium)
        │  writes
        ▼
   MeshPeer CRD ──► MeshPeerReconciler
                        │  topology.PlanPeer (pure: which CIDRs are safe to route)
                        ▼
                  PeerDataPlane     ── pkg/wg:     WireGuard peer + netlink routes  (standalone)
                                       pkg/routed: host routes via your overlay     (BYO mode)
```

The reconciler keeps an in-memory `keyIndex` (namespaced-name → public key) so
the `NotFound` deletion path can still tear a peer down, and a finalizer for
graceful teardown while the spec is still readable. Key rotation tears down the
stale key before programming the new one.

## Data flow — cross-cluster service discovery (MCS)

```
Cluster A                                   Cluster B
─────────                                   ─────────
Service + ServiceExport                     (EndpointExports mirrored here:
        │                                    GitOps in free / SaaS in premium)
ServiceExportReconciler                              │
  validates, publishes ──► EndpointExport ──────────►│
                                                     ▼
                                          ServiceImportReconciler
                                            dns.GroupExports + PlanServiceImport (pure)
                                            dns.AllocateClusterSetIPs (pure, broker-less)
                                                     │ writes
                                                     ▼
                                              ServiceImport CRD
                                                     │
                            ┌────────────────────────┴───────────────────────┐
                            ▼                                                  ▼
                  dnsserver (clusterset.local)                    ServiceNATReconciler
                  A/AAAA from ServiceImport /                     nat.BuildRuleset (pure)
                  EndpointExports                                  → iptables DNAT/LB
```

- **Headless services** resolve to the union of real pod IPs (already routable
  over WireGuard) — no NAT needed.
- **ClusterSetIP services** resolve to a virtual IP that the NAT layer DNATs and
  load-balances to the exporting clusters' real service IPs.

### Why ClusterSetIP allocation is broker-less

A ClusterSetIP must be identical in every cluster, yet there is no broker.
`dns.AllocateClusterSetIPs` is a **pure function of the CIDR + the full sorted
set of service keys** (hash + deterministic linear probe), so every cluster that
sees the same `EndpointExport`s computes the same mapping independently.

## Tiers

Selection happens once at startup in `cmd/manager` based on
`DataWerx_SAAS_ENDPOINT`. Free uses `LocalGitOpsClient` (reads local CRDs);
premium uses `EnterpriseControlPlaneClient` plus `pkg/syncer.Syncer`, which
mirrors the remote topology into the same CRDs (and detects conflicts, prunes
stale peers, and short-circuits on an unchanged revision). The reconcile loop
never branches on tier. See `COMMITMENT.md` for the free/paid line and the API
stability promise.

## Testing layers

| Layer | Tag | Scope |
|-------|-----|-------|
| unit | _(none)_ | pure logic + reconcilers via fake client/dataplane; hermetic, every push |
| integration | `integration` | reconcilers vs. a real API server (envtest) |
| e2e | `e2e` | two `kind` clusters, real WireGuard tunnels, connectivity + DNS |
| dataplane | `dataplane` | real WireGuard device/routes and iptables in a netns (root) |

The default `go test ./...` is hermetic and root-free; the tagged suites run in
dedicated CI jobs. See `CONTRIBUTING.md`.
