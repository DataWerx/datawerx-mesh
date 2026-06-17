# Design 0003 — eBPF overlapping-CIDR remap (premium datapath)

- Status: **Control plane implemented; kernel loader is the premium build.**
- Builds on: Design 0002 (iptables NETMAP remap).
- Package: `pkg/dataplane/ebpf`.

> **Implementation status.** The control-plane half — the pure BPF-map planner
> (`ebpf.BuildRemapMaps`) and the full-state reconcile `Manager` over an
> injectable `MapOps` seam — is implemented and unit-tested in the open-core
> tree, behind the same `controllers.RemapDataPlane` interface as the iptables
> backend. The kernel half (the compiled CO-RE object, the TC attach, and a
> libbpf/cilium-ebpf-backed `MapOps`) is the **premium build**: `ebpf.Load`
> ships as a stub that returns `ErrNotCompiled` so selecting the backend in the
> open-core binary fails fast. Select with `DataWerx_REMAP_BACKEND=ebpf`.

## Why an eBPF datapath

Design 0002 programs the 1:1 NETMAP with two iptables chains
(`DWX-REMAP-PRE` / `DWX-REMAP-POST`). That is correct and dependency-light, but
for high-throughput meshes it has costs:

- **Per-packet chain traversal.** Every packet on the mesh device walks the nat
  table's PREROUTING/POSTROUTING chains. NETMAP itself is stateless, but the
  rules still sit in the conntrack-backed `nat` table.
- **Rule-set churn.** A full-state rebuild re-creates chains; large fleets with
  many overlapping peers amplify this.
- **No batching of the rewrite with other mesh logic.**

A TC/eBPF program attached to the mesh device does the rewrite in the kernel
fast path with O(1) LPM-trie lookups, no conntrack, and no iptables chains —
and is the natural place to later fuse policy (Design: MeshNetworkPolicy) and
remap into a single pass.

## Datapath model

The remap is the same bidirectional, stateless 1:1 NETMAP as 0002, just moved
into eBPF. Two programs are attached to the mesh device via `clsact`:

```
                       ┌──────────────────────────────────────────┐
 from peer (tunnel) ──►│ TC ingress: dst ∈ virtual → dst := real  │──► local pod
                       └──────────────────────────────────────────┘
                       ┌────────────────────────────────────────────┐
 to peer (tunnel)  ◄───│ TC egress:  src ∈ real    → src := virtual │◄── local pod
                       └────────────────────────────────────────────┘
```

Address arithmetic is a 1:1 block translation that preserves host bits (the two
prefixes are equal length, enforced by the planner):

```
new_addr = new_base | (old_addr & ~prefix_mask)
```

After rewriting L3 addresses the program fixes the IPv4 header checksum (and the
L4 pseudo-header checksum for TCP/UDP) incrementally, then returns `TC_ACT_OK`.

### Maps

Two `BPF_MAP_TYPE_LPM_TRIE` maps, keyed by `struct bpf_lpm_trie_key` (a prefix
length + the IPv4 network address), valued by `{ base, prefix_len }`:

| Map | Key (prefix) | Value (rewrite base) | Hook |
|-----|--------------|----------------------|------|
| `ingress` | virtual CIDR | real CIDR base | TC ingress (dst) |
| `egress`  | real CIDR    | virtual CIDR base | TC egress (src) |

`ebpf.BuildRemapMaps` computes exactly these contents from the `nat.RemapEntry`
set — the identical input the iptables planner consumes — so the two backends
are provably equivalent at the map/rule boundary and share all the upstream
logic (`topology.VirtualCIDR` / `topology.PlanRemap`, `RemapReconciler`).

## Open-core boundary

```
RemapReconciler ──(controllers.RemapDataPlane)──► nat.Manager      (open core, iptables)
                                              └──► ebpf.Manager     (premium, TC/eBPF)
```

The reconciler is unchanged and never learns which backend is active — the
open-core design rule. `ebpf.Manager.SyncRemap` is a full-state reconcile that
diffs desired map contents against the live maps and applies the minimal
add/update/delete delta, so the kernel fast path never has a window with zero
rules (unlike a flush-and-rebuild). The diff logic is unit-tested against an
in-memory `MapOps` fake.

### What's open-core vs premium

| Piece | Where | Status |
|-------|-------|--------|
| `BuildRemapMaps` (pure planner) | open core | done, tested |
| `Manager` full-state reconcile + `MapOps` seam | open core | done, tested |
| Backend selection (`DataWerx_REMAP_BACKEND`) | open core | done |
| Compiled CO-RE object + TC attach + libbpf `MapOps` (`Load`) | **premium** | stubbed (`ErrNotCompiled`) behind `-tags ebpf_datapath` |

## Build & deploy (premium)

The premium build is produced with the BPF object generated (bpf2go / clang +
libbpf CO-RE) and the loader compiled in:

```sh
go generate ./pkg/dataplane/ebpf/...     # bpf2go: compile remap.bpf.c → embedded object
go build -tags ebpf_datapath -o dwx-manager ./cmd/manager
```

Runtime requirements beyond the open-core agent: a kernel with BTF
(`CONFIG_DEBUG_INFO_BTF`), `clsact` qdisc support, and `CAP_BPF`/`CAP_NET_ADMIN`
(already granted to the DaemonSet). Then set `DataWerx_REMAP_BACKEND=ebpf`.

## Testing strategy

- **Pure planner** (`maps_test.go`): bidirectional correctness, 1:1 prefix
  enforcement, IPv4-only, dedupe, determinism.
- **Reconcile** (`manager_test.go`): programs both maps, identical re-sync is a
  no-op, stale entries are pruned from both maps, list errors surface, and the
  open-core `Load` returns `ErrNotCompiled`.
- **Kernel** (premium): a `bpftool`/netns harness that loads the object, writes
  the maps, and asserts a packet's addresses are rewritten — analogous to the
  iptables `dataplane`-tagged test and the overlap e2e. Not in open core.
