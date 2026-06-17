# How it works

The whole system in one page.

## The problem

Kubernetes has no built-in way for a pod in cluster A to reach a Service in
cluster B by name. DNS is cluster-local, ClusterIPs aren't routable across
clusters, and the two clusters often reuse the same private pod ranges
(`10.244.0.0/16` is the canonical clash). DataWerx solves the **whole** path:
connectivity, naming, a stable VIP, and overlap.

## The pieces

```
   cluster A                                          cluster B
 ┌───────────────────────────┐                    ┌─────────────────────────────┐
 │ Service "payments"        │                    │ pod runs:                   │
 │   ↓ ServiceExport         │                    │   curl payments.prod        │
 │ agent publishes an        │  ── encrypted ──►  │       .svc.clusterset.local │
 │ EndpointExport (its IPs)  │   transport        │   ↓ CoreDNS → DataWerx DNS  │
 └───────────────────────────┘                    │   ↓ cluster-set VIP         │
                                                  │   ↓ DNAT + load-balance     │
                                                  │   → payments pods in A      │
                                                  └─────────────────────────────┘
```

1. **Topology** — you declare each remote cluster as a `MeshPeer` CRD (its
   public key/identity, reachable endpoint, and CIDRs). Your GitOps pipeline
   owns these. There is no broker.
2. **Connectivity** — a per-node agent (a DaemonSet) programs the data plane so
   remote pod/service ranges are reachable: either over its **own WireGuard
   device**, or — in [routed mode](byo-overlay.md) — as host routes over an
   **overlay you already run**.
3. **Discovery (MCS)** — you mark a Service with a `ServiceExport`. The agent
   publishes an `EndpointExport` (the broker-less wire format). Every cluster
   that sees it builds a matching `ServiceImport`.
4. **Naming + VIP** — for each imported service the agent allocates a stable
   **cluster-set IP** (a pure, deterministic function of the service set, so
   every cluster computes the *same* IP with no coordination) and serves
   `name.namespace.svc.clusterset.local` for it. Traffic to that VIP is
   DNAT'd and load-balanced across the exporting clusters' real pods.

## The reconcile loop

The agent is a standard controller-runtime operator. The core loop is
deliberately thin and identical in every tier:

```
MeshPeer / ServiceImport CRD  →  pure planner (no I/O, fully unit-tested)
                              →  data plane (WireGuard/routes, iptables NAT, DNS)
                              →  status
```

All the "what should the kernel look like" logic lives in pure functions
(`pkg/topology`, `pkg/dns`, `pkg/nat`, `pkg/meshfw`); the data-plane managers
just apply the result. That's why most of the project is testable without a
cluster.

## Overlapping CIDRs

If a remote cluster advertises a range that overlaps one of yours, routing it
directly would hijack local traffic. By default DataWerx **refuses** and reports
`Phase=Error`. Turn on remap (`DataWerx_REMAP_CIDR`) and it instead gives each
conflicting range a deterministic **virtual** range (e.g. under `172.16.0.0/12`)
and translates it 1:1 with stateless NAT — so overlapping clusters communicate
without renumbering. Details: [design/0002](design/0002-overlap-nat-remap.md).

## Where the tiers split

Everything above is the free core. The only seam is `ControlPlaneClient`: in the
free tier, topology comes from local `MeshPeer` CRDs; the (optional, paid)
premium tier swaps in a managed control plane that mirrors remote topology into
the *same* CRDs. The reconcile loop never knows the difference.
