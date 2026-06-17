# Design 0002 — Overlapping-CIDR NAT remap

- Status: **Implemented (basic)** — opt-in via `DataWerx_REMAP_CIDR`.
- Supersedes the "refuse overlaps" behavior in the reconciler when enabled.

> **Implementation status.** The pure core (`topology.VirtualCIDR` /
> `topology.PlanRemap`, `nat.BuildRemapRules`), the iptables NETMAP applier
> (`nat.Manager.SyncRemap`), and the wiring (`MeshPeerReconciler` routing virtual
> ranges + `RemapReconciler` programming NETMAP) are implemented and unit-tested,
> with a `dataplane`-tagged test asserting the real iptables rules. **The
> end-to-end NAT-direction correctness is gated by a two-cluster overlap e2e**
> (both clusters on the same CIDR) that must pass in CI before relying on it in
> production — it cannot run in a single-host dev sandbox. Disabled by default.

## Problem

Two independently-administered clusters routinely reuse the same private ranges
(the Kubernetes default pod CIDR `10.244.0.0/16` being the canonical clash).
Today the open core **refuses to route** a remote CIDR that overlaps a local one
(`Phase=Error`) — correct, but a dead end. M3 makes overlapping clusters
actually communicate, with a basic (free) 1:1 NAT remap. The high-performance
eBPF engine is the premium upsell (separate repo); this doc covers the **free
iptables/nftables** path.

## Why a naive "remap the destination" is wrong

The seductive-but-broken approach: give each conflicting remote CIDR a unique
**virtual** CIDR from a pool (e.g. `172.16.0.0/12`), route the virtual CIDR over
WireGuard, and DNAT virtual→real on egress.

That handles the **forward** path, but breaks the **return** path. Consider
both clusters on `10.244.0.0/16`:

```
local pod 10.244.1.5  ──► dst 172.20.1.7 (virtual for remote 10.244.1.7)
   egress NETMAP dst: 172.20.0.0/16 → 10.244.0.0/16
   packet now: src 10.244.1.5  dst 10.244.1.7   ← sent over the tunnel
```

The remote cluster receives `src 10.244.1.5` — but **`10.244.1.5` also exists in
the remote cluster** (its own pod). The reply is delivered locally in the remote
cluster, never coming back. The **source** overlaps, so destination-only NAT is
insufficient.

## Correct model: dual 1:1 NETMAP (source *and* destination)

Each cluster is assigned a unique **virtual /16 (or /N)** per remote peer from
the `172.x` pool. Every cluster presents its own pods to the mesh under its
virtual range, and addresses remote pods by their virtual range:

```
                        on egress to the tunnel (local agent)
local pod 10.244.1.5  ─► SNAT src 10.244.0.0/16 → 172.21.0.0/16   (this cluster's virtual range)
                         DNAT dst 172.20.0.0/16 → 10.244.0.0/16   (remote's real range)
packet on wire:          src 172.21.1.5   dst 10.244.1.7

                        on ingress at the remote agent (mirror rules)
                         the remote sees src 172.21.1.5 (unique, no clash) → replies to it,
                         which routes back over the tunnel and is un-NAT'd symmetrically.
```

Because the mapping is a **stateless 1:1 NETMAP** in both directions (whole-CIDR,
order-preserving), no conntrack table is needed and it is symmetric. This is the
same shape as Submariner Globalnet's static path.

Key invariant: **the virtual range assignment must be agreed by both ends.** A
cluster's virtual range for itself, and the remote's virtual range, must match on
both sides or the mappings won't compose.

## Allocation of virtual ranges (broker-less)

Reuse the broker-less pattern already proven for ClusterSetIP allocation: make
the virtual-range assignment a **pure, deterministic function** of the cluster
IDs and the real CIDRs, so both peers compute the same mapping independently.

- Pool: `172.16.0.0/12` (configurable via `DataWerx_REMAP_CIDR`).
- For a peering between `clusterID`s A and B over real CIDR `C`, assign a virtual
  block by hashing a canonical key (e.g. sorted `{A,B,C}`) into the pool,
  matching `C`'s prefix length. Deterministic + symmetric ⇒ both ends agree
  with no coordination. Collisions resolved by deterministic probe over the
  sorted peering set (same technique as `dns.AllocateClusterSetIPs`).

This goes in a new pure function, `topology.PlanRemap`, fully unit-testable.

## Component changes

1. **`pkg/topology` (pure):** replace "drop conflicting CIDRs" with
   `PlanRemap`: for each conflicting remote CIDR, emit a `Remap{Virtual, Real}`
   and the local self-virtual range. `PlanPeer` then routes the **virtual**
   remote ranges (and the peer becomes `Connected`, with status noting the
   remap). Non-conflicting CIDRs are routed directly as today.
2. **`pkg/nat` (planner + applier):** add a NETMAP ruleset builder
   (`BuildRemapRuleset`) — pure, deterministic, like `BuildRuleset` — emitting
   `-j NETMAP --to <cidr>` rules in the `nat` table for both directions, plus
   the iptables/ip6tables applier (same full-state rebuild discipline).
3. **`pkg/wg`:** routes the virtual ranges into `dwx-mesh0` (it already routes
   arbitrary CIDRs — the planner just hands it virtual ranges instead of real
   ones for the conflicting cases).
4. **DNS/discovery:** remote service/pod IPs advertised to a remapping peer must
   be expressed in the **virtual** range so names resolve to routable addresses.
   The import/responder path translates real→virtual using the same `PlanRemap`
   mapping.
5. **`cmd/manager` + Helm:** `DataWerx_REMAP_CIDR` (default `172.16.0.0/12`);
   register the remap reconciler/applier.
6. **Metrics:** `dwx_remapped_cidrs` gauge; `dwx_remap_syncs_total{result}`.

## Testing

- `pkg/topology` / `pkg/nat`: exhaustive table-driven tests for `PlanRemap` and
  `BuildRemapRuleset` (determinism, symmetry, collision handling, prefix-length
  matching, no-overlap passthrough).
- `dataplane` tag: apply NETMAP rules in a netns and verify with `iptables -S`.
- `e2e`: a dedicated case with **both clusters on `10.244.0.0/16`** — a pod in A
  reaches a service in B by name through the remap.

## Scope / non-goals

- **Free** tier: stateless whole-CIDR NETMAP (this doc). Adequate for typical
  overlapping-cluster cases; throughput is bounded by iptables.
- **Premium** (separate repo): the eBPF TC/XDP engine with a per-`{clusterID,
  origIP}` BPF map — line-rate, no iptables, finer-grained per-service remap.
- Not addressed here: more than two clusters sharing one real CIDR
  simultaneously addressing each other (needs per-peer virtual ranges, which the
  allocation scheme above already supports, but the e2e matrix grows — tracked
  for the implementation PR).
