# Multi-Cluster Services (MCS) conformance

DataWerx implements cross-cluster service discovery following the Kubernetes
SIG-Multicluster **MCS API** ([KEP-1645]). This is a mapping of what
is implemented, what differs, and what is out of scope. You can judge
conformance rather than trust a marketing claim.

[KEP-1645]: https://github.com/kubernetes/enhancements/tree/master/keps/sig-multicluster/1645-multi-cluster-services-api

## Implemented

| MCS concept | Status | Notes |
|-------------|--------|-------|
| `ServiceExport` | ✅ | Mark a Service for export; validity reported via a status condition. |
| `ServiceImport` | ✅ | Created per exported service; `Type` is `ClusterSetIP` or `Headless`. |
| **ClusterSetIP** imports | ✅ | A stable virtual IP (v4, and v6 when enabled) load-balances across all exporting clusters (iptables DNAT). |
| **Headless** imports | ✅ | Resolves to the union of backing pod IPs across clusters. |
| `*.svc.clusterset.local` DNS | ✅ | Authoritative responder; cluster CoreDNS forwards the zone. `<svc>.<ns>.svc.clusterset.local`. |
| Cross-cluster endpoint aggregation | ✅ | Endpoints from every exporting cluster are mirrored and aggregated. |
| `EndpointSlice` mirroring | ✅ (headless) | For a headless import, the cross-cluster endpoints are materialized as `discovery.k8s.io` `EndpointSlice`s in the consuming cluster, labeled `multicluster.kubernetes.io/service-name` + `…/source-cluster`, so native consumers and an MCS-aware kube-proxy discover them through the standard surface, not only DNS. |
| `SessionAffinity` | ✅ (carried) | Mirrored from the exported Service (`ClientIP`/`None`). |
| Multi-port services | ✅ | Per-port DNAT/load-balancing. |
| Conflict resolution | ✅ | Canonical MCS resolution: on a type/port disagreement the **oldest `ServiceExport` wins** (earliest creation time, lowest cluster ID to break a tie), and the losing exports are reported as conflicts on status. |

## Differences from the spec

- **Transport.** MCS defines the API, not the data plane. DataWerx provides the
  data plane (WireGuard or a BYO overlay) and the standard objects on top, so it
  interoperates with MCS-aware tooling at the API/DNS layer.
- **`ServiceImport` IP semantics.** ClusterSetIPs are allocated from a
  DataWerx-managed range (`DataWerx_CLUSTERSET_CIDR`, default `241.0.0.0/8`) and
  realized by node-local DNAT; they are virtual and only meaningful inside the
  clusterset data plane.
- **No central MCS controller assumption.** DataWerx allocates ClusterSetIPs
  deterministically (pure hash into the range) rather than via a central
  allocator, so every node computes the same VIP without coordination.

## Out of scope (at least for today)

- `EndpointSlice` mirroring for **ClusterSetIP** imports. A ClusterSetIP service
  is realized through its virtual IP and node-local DNAT rather than per-endpoint
  slices, so DataWerx mirrors slices for headless imports (where per-endpoint
  discovery is the contract) and leaves ClusterSetIP to the VIP path.

## How it's validated

- **Executable conformance suite:** `test/conformance` machine-checks the
  KEP-1645 contract claims in this document — the `ServiceExport` marker shape and
  `Valid`/`Conflict` condition types, the `ServiceImport` type enum, the
  `ServicePort` subset, the `<svc>.<ns>.svc.clusterset.local` DNS name (build and
  round-trip), deterministic in-range ClusterSetIP allocation, namespaced scope
  from the shipped CRDs, and import aggregation/conflict reporting. It is
  hermetic and runs on every push (`go test ./test/conformance/...`, a named CI
  gate), so this table cannot drift from the code without failing CI.
- Pure aggregation/allocation/DNS logic: unit tests in `pkg/dns` (100%-target).
- End-to-end: the kind-based e2e (`test/e2e`, `.github/workflows/e2e.yml`)
  exports a service in cluster A and proves a pod in cluster B reaches it by its
  `clusterset.local` name — the full export → mirror → import → DNS → connect
  path.
- The full **upstream** MCS conformance e2e requires a real clusterset and the
  upstream `sigs.k8s.io/mcs-api` group; DataWerx ships a focused subset of the
  types, so that suite is a known delta rather than a drop-in.

If you need strict upstream-conformance for a specific MCS test, open an issue
with the case. EndpointSlice mirroring for ClusterSetIP imports (headless is
covered) is the remaining known delta.
