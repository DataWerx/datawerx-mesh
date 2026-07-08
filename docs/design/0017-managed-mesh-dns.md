# Design 0017 — Managed mesh DNS (MagicDNS for edge and remote access)

- Status: **Accepted.** The authoritative mesh zone is `mesh.datawerx.internal`.
- Packages: `pkg/gateway` (the `AccessProfile`/`DNSConfig` seam, OSS),
  `pkg/edge` (device profile render, OSS), `pkg/dns` + `pkg/dnsserver`
  (the existing `clusterset.local` responder, OSS), and the **premium**
  control-plane zone service in `datawerx-admin` plus its agent-side consumer
  in `datawerx-premium` wired through `pkg/agent.Options.RegisterPremium`.
- Builds on: cross-cluster DNS via MCS (design 0001), the remote-access gateway
  role (`pkg/gateway`, `controllers.GatewayReconciler`), the edge device
  connector (design 0013), and the fixed `/api/v1` agent contract
  (`datawerx-admin/internal/agentapi`).

## Summary

Name resolution works well for pods **inside** a mesh cluster. It does not work
seamlessly for the two premium reach paths where the client is **not** a pod on
the cluster's DNS domain: the remote-access **gateway** role (laptops on a
shared overlay) and the managed **edge device connector** (roaming `/32` peers on
`dwx-edge0`). Today both paths hand the client a split-DNS hint that points at
the `clusterset.local` responder, but the resolver address is unset by default,
the responder is only reachable through an in-cluster ClusterIP Service, and
only explicitly-exported `*.clusterset.local` names resolve. A remote user who
types the ordinary `cluster.local` name every example uses gets nothing.

This document traces the current DNS behaviour end to end, enumerates the gaps,
and recommends a **managed control-plane DNS service** (a MagicDNS-style
capability served from `datawerx-admin`) as the premium answer, while keeping the
free-tier answer a deliberately simple bring-your-own resolver plus gateway
split-DNS. The managed zone reuses the existing `DNSConfig`/`AccessProfile`
shapes unchanged, so the free and premium clients stay byte-identical on the
wire.

## 1. Current state

### 1.1 In-cluster and pod-to-pod (works today, free)

The cross-cluster discovery pipeline from design 0001 is complete and solid
**for pods**:

- Exported services are aggregated into the `clusterset.local` zone. A pod
  resolves `payments.prod.svc.clusterset.local` to a ClusterSetIP carved from
  `DataWerx_CLUSTERSET_CIDR` (default `241.0.0.0/8`), which the data plane
  routes across the mesh. Zone name and FQDN construction live in
  `pkg/dns/naming.go:23` (`ClusterSetDomain`) and `:30` (`FQDN`).
- The responder runs on **every** agent pod on `:5353`
  (`pkg/dnsserver/server.go:30`, `DefaultBindAddress`), registered as a
  non-leader Runnable in `pkg/agent/agent.go:604`. It answers only the
  clusterset zone and refuses everything else
  (`pkg/dnsserver/server.go:120`, `InClusterSetZone` → `RcodeRefused`).
- Pods reach it because cluster CoreDNS is patched to forward the
  `clusterset.local` zone to the `datawerx-mesh-dns` **ClusterIP** Service
  (`config/coredns/datawerx-mesh-dns-service.yaml`, port 53 → 5353;
  `hack/e2e/patch-coredns.sh:40` writes the Corefile `forward` block).

The important property for this document: the responder is fronted by a
ClusterIP and reached via the cluster's own CoreDNS. That is exactly what a
non-pod client does **not** have.

### 1.2 Remote-access gateway (laptops over an overlay)

The gateway publishes an `AccessProfile` ConfigMap (`dwx-remote-access`,
`pkg/gateway/profile.go:36`) a thin client reads to configure itself. The
profile carries `GatewayEndpoints`, `RouteCIDRs` (the ClusterSetIP ranges plus
the reachable mesh CIDRs), and a `DNSConfig{Addr, SearchDomains}`
(`pkg/gateway/profile.go:45`).

The `DNSConfig` is assembled in `pkg/agent/agent.go:560`:

```go
DNS: gateway.DNSConfig{
    Addr:          strings.TrimSpace(os.Getenv(envGatewayDNSAddr)),
    SearchDomains: []string{dwxdns.ClusterSetDomain},
},
```

So the search domain is hard-wired to `clusterset.local`, and the resolver
address is **whatever the operator put in `DataWerx_GATEWAY_DNS_ADDR`** — which
has **no default** (`pkg/agent/agent.go:173`; the env table in `CLAUDE.md`
confirms "none"). If it is unset the client receives a search domain but no
server to ask.

### 1.3 Edge device connector (roaming `/32` peers)

The edge device profile **reuses `gateway.AccessProfile` unchanged**
(`pkg/edge/profile.go:78`), so its DNS shape is identical. The free
`dwx mesh edge` CLI populates it from flags: `--dns` (resolver host:port, default
empty) and `--dns-search` (default `clusterset.local`), at
`internal/cli/mesh/edge.go:146`. The device-side `wg-quick` render emits the
resolver into the `[Interface]` (`pkg/edge/profile.go:218`), but
`dnsResolver()` (`pkg/edge/profile.go:236`) deliberately extracts only the host
and **drops `SearchDomains` entirely** — the comment there notes the search
domains "travel in the profile's Access.DNS for clients that support split DNS",
i.e. the wg-quick file itself never carries them.

On the managed side, the premium reconciler (`datawerx-premium/edge/reconciler.go`)
allocates a device IP and programs the device as a `/32` peer, but it does **not**
assemble a `DeviceProfile` or any DNS at all — the managed edge path has no DNS
story yet. A managed device gets connectivity and nothing to resolve names with.

## 2. The gaps

**Gap A — the resolver address is unset by default and never derived.**
`DataWerx_GATEWAY_DNS_ADDR` and the edge `--dns` flag both default to empty. The
operator must hand-pick an overlay-reachable `host:port` for the responder and
set it. Miss that step and remote/edge clients get search domains but no server.
DNS does not work out of the box.

**Gap B — the responder is only reachable through an in-cluster ClusterIP.**
The zone responder is fronted by the `datawerx-mesh-dns` ClusterIP Service on the
cluster service network. A laptop or edge device on the overlay can only reach a
ClusterIP if that service CIDR is in `RouteCIDRs`, the gateway routes it, and the
gateway node's kube-proxy DNATs it. For edge devices there is no gateway
masquerade at all — the device is a `/32` peer on `dwx-edge0`, so reaching a
ClusterIP depends on the terminator node's kube-proxy and the device routing the
service CIDR. Nothing in the profile guarantees any of this lines up.

**Gap C — only `clusterset.local` resolves, never `cluster.local`.**
`pkg/dnsserver` refuses any name outside the clusterset zone
(`server.go:120`). The ordinary in-cluster names every Kubernetes example uses
(`<svc>.<ns>.svc.cluster.local`) return REFUSED to a remote/edge client. A user
must know to author a `ServiceExport` and switch to the `.clusterset.local`
name. That is a real adoption cliff for the "it should just work like a VPN"
promise.

**Gap D — no unified mesh-wide namespace and no per-cluster or device names.**
`clusterset.local` is a single flattened namespace: a name aggregates every
cluster that exports it. There is no way to name one specific cluster's service,
no names for clusters, nodes, gateways, or edge devices, and no name for a
service that was never exported. MagicDNS-style products give every peer and
service a stable name; here only explicitly-exported services get one.

**Gap E — split-DNS is assumed, not programmed.** For the gateway path the
profile is only a hint; the consuming "thin client / kubectl plugin" that would
apply it does not ship in these repos, so nothing actually configures a remote
laptop's resolver. For the edge path `wg-quick` sets `DNS = <resolver>` as a
**global** resolver for the host while the tunnel is up (it hijacks all queries)
and, because the render drops the search domains (Gap in 1.3), a
systemd-resolved host cannot scope the mesh zone to the interface. The
`SearchDomains` field the profile carries is effectively unused by the edge
render.

**Gap F — the managed edge path has no DNS at all.** As noted in 1.3, the
premium reconciler programs peers but never builds a device profile or DNS
block. So in the tier that is supposed to be the polished experience, edge DNS is
entirely absent.

## 3. Recommendation

**Yes — build a managed control-plane DNS service for the premium tier.** It is
the only option that closes Gaps A through F cleanly and in one place, and it is
squarely the kind of org-scale, zero-configuration capability the open-core seam
reserves for premium (a client never has to be told a resolver address, and the
control plane already holds the authoritative topology and service catalog needed
to answer).

The alternative — hardening the existing per-cluster split-DNS so it works
without a control plane — cannot close Gap A (there is no component with a
mesh-wide view to pick and advertise a reachable resolver), Gap D (no source of
per-cluster or device names), or Gap F (nothing to serve a managed device its
profile). It can only paper over B, C, and E per cluster, by hand. So
split-DNS stays the **free** answer, deliberately minimal, and the managed zone
is the premium answer.

### 3.1 What the managed service is

A **MagicDNS-style mesh zone** rooted at `mesh.datawerx.internal`. The
`.internal` suffix is reserved for private networks, so the zone can never
collide with a real TLD. It is **authoritative**, built purely from what the
control plane already stores:

- `internal/topology` — clusters and peers (`RemotePeerConfig`, one per cluster).
- `internal/catalog` — exported/imported services
  (`catalog.Service{Namespace, Name, ExportedFrom, ImportedBy}`,
  `internal/catalog/catalog.go:28`), already fed by the additive
  `POST /api/v1/services` report (`internal/agentapi/agentapi.go:49`).

From those the control plane composes records such as:

- `<svc>.<ns>.<cluster>.mesh.datawerx.internal` → that cluster's ClusterSetIP (per-cluster
  name, closes Gap D).
- `<svc>.<ns>.svc.mesh.datawerx.internal` → the aggregated ClusterSetIP (the familiar
  flattened name, a rename of today's `clusterset.local` semantics).
- `<cluster>.mesh.datawerx.internal`, `<device>.mesh.datawerx.internal` → cluster gateways and
  enrolled edge devices, so peers and devices are nameable.
- Optionally a `cluster.local` alias view per cluster, so the ordinary example
  names resolve for a remote client scoped to a chosen home cluster (closes
  Gap C).

Record assembly must be a **pure package** (`datawerx-admin/internal/meshdns`),
table-tested with no DB/HTTP/clock, exactly like `internal/topology` and
`internal/signal`. This keeps it consistent with the control plane's pure/IO
split and lets the same records be served three ways without duplicating logic.

### 3.2 Where it lives (the premium seam)

Two halves, matching every other premium capability:

**Control-plane side (`datawerx-admin`, premium):**

- `internal/meshdns` — the pure zone builder (topology + catalog → records).
- A served surface. Preferred: extend the access profile the agent already
  fetches so the resolved DNS block rides the existing `/api/v1` responses (no
  new listener on the client). The control plane additionally runs an
  authoritative responder for the zone at a stable, overlay-reachable relay
  address (it already brokers relays in `handleTopology` via `applyRelays`,
  `internal/agentapi/agentapi.go`), so `DNSConfig.Addr` can be **derived by the
  control plane** rather than typed by an operator (closes Gap A).

**Agent side (`datawerx-premium`, premium, wired via `RegisterPremium`):**

- The premium operator fetches the resolved DNS block and stamps it into the
  **existing** `gateway.AccessProfile.DNS` and `edge.DeviceProfile.Access.DNS`.
  Because the shapes are unchanged, the published `dwx-remote-access` ConfigMap
  and the `dwxedge.v1` token are wire-identical to the free versions — only the
  values (a control-plane-derived resolver, the `mesh.datawerx.internal` search domain,
  and any synced records) differ. The managed edge terminator finally builds a
  `DeviceProfile` with a populated DNS block (closes Gap F).

The open core imports **nothing** from either premium half. `pkg/gateway`,
`pkg/edge`, `pkg/dns`, and `pkg/dnsserver` are untouched except that
`DNSConfig`/`AccessProfile` become the tier-agnostic carrier they already are.

### 3.3 Contract additions (`/api/v1`)

The managed DNS block is **additive** to the fixed contract, the same discipline
as `/api/v1/evidence` (design 0015). An unmodified open-core agent never asks
for it and keeps working. Preferred shape: a `dns` object on the existing
topology response (`topologyResponse`, `internal/agentapi/agentapi.go:96`)
carrying the resolved `DNSConfig` plus any static records, so the premium agent
learns its DNS the same time it learns its peers, on the same revision cadence.
The alternative is a dedicated `GET /api/v1/dns`; choose one during
implementation, but keep it additive and behind the machine-token auth already
used for topology.

## 4. Free-tier story (unchanged, documented)

Free tier keeps the bring-your-own answer, and we should say so plainly in the
operator docs:

- In-cluster and cross-cluster pod DNS work for free through MCS
  (`ServiceExport` → `clusterset.local`), exactly as today.
- Remote/edge clients get **split-DNS to the clusterset responder** that the
  operator points at with `DataWerx_GATEWAY_DNS_ADDR` (gateway) or `--dns`
  (edge). The operator is responsible for making that address reachable over
  their overlay — for example a `hostPort` on the responder, or the
  `datawerx-mesh-dns` ClusterIP if the service CIDR is routed at the gateway.
- Free means the resolver address is configured by hand and only exported
  `*.clusterset.local` names resolve. Zero-config resolution, per-cluster and
  device names, and `cluster.local` aliasing are the premium delta.

## 5. Implementation plan

Phased, smallest useful slice first, each phase self-contained.

1. **Free-tier hardening (open core, small).** Two changes that make the free
   split-DNS actually usable and cost nothing to premium:
   - Emit the profile's `SearchDomains` on the edge `wg-quick` `DNS =` line
     (resolver first, then bare domains — the documented wg-quick idiom), so a
     systemd-resolved device can scope the mesh zone to the tunnel instead of
     taking `DNS = <resolver>` as a global override. Touches only
     `pkg/edge/profile.go` `WireGuardQuickConfig`/`dnsResolver`; the existing
     substring test at `pkg/edge/profile_test.go:139` still passes.
   - Optionally let the gateway **derive** `DNSConfig.Addr` from an advertised
     gateway IP plus the responder port when `DataWerx_GATEWAY_DNS_ADDR` is
     unset, instead of leaving it empty (a best-effort default that narrows Gap
     A for free). Guard it so an explicit value always wins.
2. **Pure zone builder (`datawerx-admin/internal/meshdns`, premium).** Topology
   + catalog → records, fully table-tested, no I/O. Define the `mesh.datawerx.internal`
   naming scheme and the per-cluster/aggregated/device record set here.
3. **Serve the DNS block (`datawerx-admin`, premium).** Add the additive `dns`
   field to the topology response (or `GET /api/v1/dns`), with the
   control-plane-derived resolver address and static records. Compatibility
   guarded like the rest of `/api/v1` (`internal/agentapi/compat_test.go`).
4. **Authoritative responder (`datawerx-admin`, premium).** Run the
   `mesh.datawerx.internal` zone at a stable relay address so clients have a real server
   to query; reuse the relay brokering already in `applyRelays`.
5. **Agent-side consumption (`datawerx-premium`, premium).** Fetch the resolved
   DNS block and stamp it into `gateway.AccessProfile.DNS` and the managed
   `edge.DeviceProfile` via `RegisterPremium`. The managed edge terminator
   builds and serves a full `DeviceProfile` (closes Gap F).
6. **`cluster.local` aliasing (premium, optional).** Serve a per-client
   home-cluster `cluster.local` view so the ordinary example names resolve for a
   remote client (closes Gap C for users who never export a service).

## 6. Testing

- `internal/meshdns` — table-driven record assembly, no K8s/DB, ~100% like its
  sibling pure packages.
- `internal/agentapi` — the additive `dns` field is exercised by the existing
  compat test so an unmodified open-core agent still round-trips topology.
- `pkg/edge` — extend `WireGuardQuickConfig` tests to assert the search domain
  now rides the `DNS =` line.
- e2e — a remote client and an edge device each resolve a `mesh.datawerx.internal` name
  against a managed control plane across two `kind` clusters (premium e2e,
  alongside the existing `clusterset.local` e2e).

## 7. Out of scope

- DNS-over-TLS / DoH for the mesh zone.
- Per-user DNS policy / filtering (a Cloudflare-One-style egress control).
- Reverse (PTR) records for mesh addresses.
- Replacing the in-cluster CoreDNS forward for pods — the pod path stays on
  `clusterset.local` and is unchanged.
