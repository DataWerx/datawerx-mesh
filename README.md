<div align="center">

# DataWerx Mesh

**Connect any Kubernetes Service, by name, from any cluster across clouds, regions, and on-prem**

No LoadBalancer. No public IP. No central broker. No CNI lock-in.

Works with Tailscale, NetBird, Cilium, WireGuard, cloud VPN

-or-

Use our batteries-included built-in overlay

[![License: Apache-2.0](https://img.shields.io/badge/License-Apache_2.0-blue.svg)](LICENSE)
[![Free forever](https://img.shields.io/badge/core-free%20forever-brightgreen.svg)](COMMITMENT.md)
[![CI](https://github.com/DataWerx/datawerx-mesh/actions/workflows/ci.yml/badge.svg)](https://github.com/DataWerx/datawerx-mesh/actions/workflows/ci.yml)
[![OpenSSF Scorecard](https://api.scorecard.dev/projects/github.com/DataWerx/datawerx-mesh/badge)](https://scorecard.dev/viewer/?uri=github.com/DataWerx/datawerx-mesh)
[![Go 1.26](https://img.shields.io/badge/Go-1.26-00ADD8.svg)](https://go.dev)


<img src="docs/images/logo.svg" height=100px/>


[Quickstart](#quickstart-link-two-clusters-in-5-minutes) · [Try it yourself](#try-it-yourself) · [How it works](#how-it-works) · [Why it's different](#why-its-different) · [Ask an AI](#2--signal--ask-an-ai-why-the-mesh-is-unhealthy) · [Docs](docs/README.md)

</div>

## What DataWerx Mesh is

**It's the open-source multi-cluster service layer for Kubernetes.**

It makes a Service in one cluster reachable — *by a stable name and IP* — from every other cluster in the mesh, **even when their pod CIDRs overlap**.

It is a single, tiny Go agent that runs as a **DaemonSet** (one pod per node). The agent watches a small set of CRDs and converges each node's network toward the desired state: it brings up cross-cluster connectivity, propagates exported Services, allocates a stable cluster-set IP per service, and serves a `*.clusterset.local` DNS zone — all with **no central broker** and **no required CNI**.

You implement the standard [Kubernetes Multi-Cluster Services API (MCS, KEP-1645)](https://github.com/kubernetes/enhancements/tree/master/keps/sig-multicluster/1645-multi-cluster-services-api) — `ServiceExport` / `ServiceImport` — and DataWerx provides the network and the machinery that MCS assumes you already have.

## One agent, four ways to use it

DataWerx is open-core: the agent and CLI in this repo are Apache-2.0 and **free forever**. The same agent grows into three more capabilities, each with a working open-source foundation you can run today and a premium managed tier that adds the org-scale conveniences.

| | What it does | Free, in this repo | Premium adds |
|---|---|---|---|
| 🌐 **Mesh** | Cross-cluster Services by name over an encrypted mesh. The core. | Everything — connectivity, MCS discovery, cluster-set VIPs + DNS, overlap remap, policy, metrics. | — |
| 🧭 **Remote** | Reach in-cluster Services by name from a laptop, VM, or CI runner. | The **gateway role** (`DataWerx_ROLE=gateway`) over an overlay you run, plus the published client access profile. | SSO device sign-in, outbound-dialed tunnels, no-NAT identity-preserving return routes. |
| 📟 **Edge** | Attach edge / IoT devices to the mesh as roaming peers. | The `EdgeDevice` **contract** and the `dwx edge` authoring + handoff CLI. | A managed terminator that programs each device, fleet enrollment, and lifecycle. |
| 🤖 **Signal** | Ask an AI *"is the mesh healthy, and why not?"* and get a grounded answer. | `dwx signal` grounded Q&A + the read-only `dwx mcp` server, over live state or a snapshot. | A managed control plane, fleet/historical view, and an AI SRE that proposes and opens the fix. |

Each capability has a step-by-step runbook below in **[Try it yourself](#try-it-yourself)**. The free/premium line never branches the reconcile loop — premium is additive behind a clean seam, never required to run the mesh. See **[COMMITMENT.md](COMMITMENT.md)**.

## The problem it solves

Kubernetes has no built-in way for a pod in cluster A to reach a Service in cluster B by name:

- **DNS is cluster-local** — `payments.prod.svc.cluster.local` means nothing in another cluster.
- **ClusterIPs aren't routable** across clusters.
- **Pod ranges collide** — two clusters both on `10.244.0.0/16` is the canonical clash.
- **The MCS API is just a contract** — it standardizes *what* a multi-cluster service looks like, but deliberately leaves out *the network and the controllers* that make it real.

DataWerx solves the **whole** path: connectivity, discovery, a stable VIP, naming, overlap, and policy.

## What you get

| | |
|---|---|
| 🌐 **Cross-cluster service discovery** | Standard K8s MCS `ServiceExport` / `ServiceImport`. Export a Service; it resolves everywhere. |
| 📛 **Service-by-name** | A `*.clusterset.local` DNS zone, served by the agent and wired into CoreDNS. |
| 🎯 **Stable cluster-set VIP + load balancing** | One virtual IP per service, **the same in every cluster**, DNAT'd and load-balanced across all exporting clusters. |
| 🔀 **Overlapping CIDRs just work** | Two clusters on the same range? DataWerx remaps them 1:1 into a virtual range instead of giving up. |
| 🔐 **Encrypted by default — or bring your own** | Built-in per-node WireGuard, *or* ride an overlay you already run (Tailscale, NetBird, Cilium, plain WireGuard, cloud VPN). |
| 🧩 **CNI-agnostic & broker-less** | Works with any CNI. No central coordinator, no database, no single point of failure. |
| 🛡️ **Cross-cluster network policy** | `MeshNetworkPolicy` extends segmentation across the mesh. |
| 📈 **Observable & operable** | Prometheus metrics, a Grafana starter dashboard, a Helm chart, and `dwx mesh verify` health checks. |
| 🤖 **AI-ready / agent-queryable** | A **read-only MCP server** plus versioned JSON state contracts (snapshot + dependency graph) let Claude — or any agent — answer *"is the mesh healthy, and why not?"* from live state. [→ Signal](#2--signal--ask-an-ai-why-the-mesh-is-unhealthy) |

## How it works

![DataWerx Mesh Demo](/docs/images/dw-mesh.png)

1. **Topology** — you declare each remote cluster as a `MeshPeer` CRD (its identity/public key, reachable endpoint, and CIDRs). Your GitOps pipeline owns these. There is no broker.
2. **Connectivity** — the per-node agent programs the data plane so remote ranges are reachable: over its **own WireGuard device**, or as **host routes over an overlay you already run** (Tailscale, NetBird, Cilium, and others).
3. **Discovery (MCS)** — you mark a Service with a `ServiceExport`. The agent publishes a broker-less `EndpointExport`; every cluster that sees it builds a matching `ServiceImport`.
4. **Naming + VIP** — for each imported service the agent allocates a stable **cluster-set IP** — a pure, deterministic function of the service set, so every cluster computes the *same* IP with no coordination — and serves `name.namespace.svc.clusterset.local`. Traffic to that VIP is DNAT'd and load-balanced to the exporting clusters' real pods.

→ Full walkthrough: **[docs/how-it-works.md](docs/how-it-works.md)** · Component reference: **[ARCHITECTURE.md](ARCHITECTURE.md)**

## Why it's different

DataWerx **speaks** the standard MCS API so you avoid lock-in, but the plumbing underneath is fundamentally less invasive than the alternatives.

> **Plain MCS is a contract, not an implementation.** The MCS API defines `ServiceExport`/`ServiceImport`, the `clusterset.local` name, and the idea of a ClusterSetIP — but it explicitly leaves out the **connectivity**, the **controllers**, **how endpoints propagate**, and **how a VIP is chosen consistently**. DataWerx provides all of that. *(Deep dive: [docs/mcs-conformance.md](docs/mcs-conformance.md).)*

| | Architecture | DNS / discovery | Overlapping CIDRs | CNI lock-in | Brings the network? |
|---|---|---|---|---|---|
| **DataWerx Mesh** | **Per-node, broker-less** | **MCS, served** | **1:1 remap built in** | **No** | **Built-in WireGuard _or_ your existing overlay** |
| **Submariner** | Central broker + gateway nodes | Lighthouse (MCS) | Globalnet | No | Yes (its own way) |
| **Cilium ClusterMesh** | Per-node (eBPF) | Transparent | Limited | **Yes — requires Cilium** | Yes |
| **Tailscale** | Per-node WireGuard | Device DNS, **not Services** | N/A | No | Yes (its overlay) |
| **Plain MCS API** | *(spec only)* | Name only | Unspecified | No | **No — assumes a flat network** |

**In short, DataWerx gives you**
- Cilium's per-node performance story *without* requiring Cilium,
- Submariner's MCS discovery *without* the broker and gateway chokepoints,
- and it works on whatever transport you already have — or brings its own.

## Quickstart: link two clusters in 5 minutes

See it work for yourself. This scripted demo creates and links **two clusters** and calls a Service across them, all on one laptop with [kind](https://kind.sigs.k8s.io). The steps map 1:1 onto your real-world clusters.

**Prereqs:** Docker, `kind`, `kubectl`, `helm`, `wg` (wireguard-tools), and the WireGuard kernel module (`sudo modprobe wireguard`).

```sh
git clone https://github.com/DataWerx/datawerx-mesh && cd datawerx-mesh

go build -o dwx ./cmd/dwx          # the unified DataWerx CLI; put it on your PATH

hack/e2e/kind-up.sh                # two kind clusters + agent + reciprocal peering
dwx mesh verify --context kind-dwx-a   # → Mesh peers: 1 connected

hack/demo/quickstart.sh            # export an echo Service in A, call it by name from B
# → hi-from-a   ← the Service in cluster A, reached by name from cluster B

hack/e2e/kind-down.sh              # clean up
```

That's a working two-cluster mesh with a service called across it. The **[full quickstart](docs/quickstart.md)** does the same thing by hand so you understand every piece.

> `dwx` is the unified CLI (the old `dwxctl` / `dwx-mcp` / `dwx-signal` names still work as aliases). Build it with `go build -o dwx ./cmd/dwx`, or install a release binary — see **[docs/install.md](docs/install.md)**.

## Try it yourself

Four runbooks, one per capability. They build on each other: the first stands up a real two-cluster mesh on your laptop, and the rest exercise Remote, Edge, and Signal against it. Every command is what you'd run against your own clusters — only the `--context` names change.

Build the CLI once and keep the demo mesh from the quickstart running:

```sh
go build -o dwx ./cmd/dwx
hack/e2e/kind-up.sh        # if it isn't already up
```

### 1 · Mesh — a Service called across clusters

This is the quickstart above, broken into the pieces you'll reproduce in production.

```sh
# Stand up the two-cluster mesh and confirm the peering converged.
hack/e2e/kind-up.sh
dwx mesh verify --context kind-dwx-a            # → Mesh peers: 1 connected
dwx mesh verify --context kind-dwx-b            # → Mesh peers: 1 connected

# Export an echo Service in cluster A and call it by name from cluster B.
hack/demo/quickstart.sh                         # → hi-from-a

# Inspect what each cluster sees.
dwx mesh snapshot --context kind-dwx-a | jq '.imports, .exports'
dwx mesh graph --context kind-dwx-a --format mermaid   # paste into any Markdown viewer
```

**On your own clusters:** install the agent with Helm (below), declare each remote cluster as a `MeshPeer` — or let `dwx mesh join` mint and swap the bundles for you — then export Services with the standard `ServiceExport`. The by-hand walkthrough is **[docs/quickstart.md](docs/quickstart.md)**; production install and DNS wiring are in **[docs/cross-cluster-services.md](docs/cross-cluster-services.md)**.

```sh
# Zero-friction peering between two real clusters — no hand-written CRDs.
dwx mesh join export --cluster-id cluster-a --endpoint a.example.com:51820 \
  --generate --pod-cidrs 10.244.0.0/16 --service-cidrs 10.96.0.0/16 > a.token
dwx mesh join export --cluster-id cluster-b --endpoint b.example.com:51820 \
  --generate --pod-cidrs 10.245.0.0/16 --service-cidrs 10.97.0.0/16 > b.token
# Swap the tokens, then import each on the other cluster:
dwx mesh join import --context cluster-a --bundle-file b.token
dwx mesh join import --context cluster-b --bundle-file a.token
```

### 2 · Signal — ask an AI why the mesh is (un)healthy

`dwx signal` answers natural-language questions about the mesh and returns a **grounded** root cause — every claim cites the exact signal it came from (a peer phase, a handshake age, a CIDR overlap), never a guess. The model reasons over *only* the evidence the tool assembles from the same read surfaces `dwx mesh snapshot/diagnose/reach/slo` already serve. It is read-only.

```sh
# See the exact grounded evidence the model would receive — NO API key needed.
dwx signal --print-context --context kind-dwx-a "Why can't cluster B reach echo?"

# Get an AI answer over live state (needs an Anthropic API key).
export ANTHROPIC_API_KEY=sk-ant-...
dwx signal --context kind-dwx-a "Which clusters are unhealthy, and what's the most likely cause?"

# Or reason over a saved snapshot, with no cluster access at all.
dwx mesh snapshot --context kind-dwx-a > snap.json
dwx signal --snapshot snap.json "Explain the connectivity problem"
```

Prefer to keep an agent in the loop? The same state is exposed through a **read-only [MCP](https://modelcontextprotocol.io) server**. Point Claude Desktop or Claude Code at the cluster — no API key, no SaaS — by adding to your MCP host config:

```json
{
  "mcpServers": {
    "datawerx-mesh": { "command": "dwx", "args": ["mcp", "--context", "kind-dwx-a"] }
  }
}
```

Then ask *"Is the DataWerx mesh healthy? Are any tunnels stale or any CIDRs overlapping?"* The OSS surface exposes **zero mutating tools** by construction; the hosted AI SRE that proposes and opens a fix is the additive paid layer. → **[docs/ai-agents.md](docs/ai-agents.md)**.

### 3 · Remote — reach a mesh Service from a laptop or VM

The free foundation of **DataWerx Remote** is the **gateway role**: an agent that forwards traffic from clients on an overlay you run (Tailscale, NetBird, plain WireGuard) into the mesh, and publishes a client access profile.

On a Helm install, it's a clean `--set` — `clientCIDRs` is the overlay range your clients connect from, `advertiseIPs` is the gateway's overlay address, and `dnsAddr` is the clusterset DNS responder for split-DNS:

```sh
helm upgrade dwx oci://ghcr.io/datawerx/datawerx-mesh/charts/datawerx-mesh \
  -n datawerx-system --reuse-values \
  --set role=gateway \
  --set gateway.clientCIDRs=100.64.0.0/10 \
  --set gateway.advertiseIPs=100.100.10.1 \
  --set gateway.dnsAddr=10.96.0.10:53 \
  --set securityContext.privileged=true
```

The demo mesh in this lab is installed straight from manifests rather than Helm, so toggle the same env on its running agent:

```sh
kubectl --context kind-dwx-a -n datawerx-system set env daemonset/dwx-mesh-agent \
  DataWerx_ROLE=gateway \
  DataWerx_GATEWAY_CLIENT_CIDRS=100.64.0.0/10 \
  DataWerx_GATEWAY_ADVERTISE_IPS=100.100.10.1 \
  DataWerx_GATEWAY_DNS_ADDR=10.96.0.10:53
kubectl --context kind-dwx-a -n datawerx-system rollout status daemonset/dwx-mesh-agent

# Either way, the gateway publishes its client access profile — clients consume
# this to learn the clusterset routes and the split-DNS to install.
kubectl --context kind-dwx-a -n datawerx-system get configmap dwx-remote-access -o yaml
```

A client on the overlay then routes the advertised clusterset ranges through the gateway and points its resolver at the clusterset DNS — and reaches `echo.demo.svc.clusterset.local` exactly like an in-cluster pod.

> The gateway forwards packets, so its nodes need `net.ipv4.ip_forward=1`. The agent sets it at startup when allowed; on a read-only `/proc/sys` either run the gateway pod privileged or pre-set the sysctl on the nodes. See **[docs/troubleshooting.md](docs/troubleshooting.md#remote-access-gateway-datawerx_rolegateway)**.

**Premium (DataWerx Remote)** turns this into a turnkey product: SSO device sign-in, tunnels the device dials **outbound** (no public ingress, no port-forwards), and identity-preserving no-NAT return routes so pods see the client's real IP. Background: **[docs/byo-overlay.md](docs/byo-overlay.md)**.

### 4 · Edge — attach an edge / IoT device as a roaming peer

The open core ships the `EdgeDevice` **contract** and the `dwx edge` CLI that authors it and renders a device config. This is the tier-agnostic half: you can enroll devices, scope their reach, and hand off a `wg-quick` profile today.

```sh
# Enroll a device (authors the free EdgeDevice CRD) and generate its keypair.
# The private key prints to stderr — store it ON THE DEVICE, never upload it.
dwx edge enroll --context kind-dwx-a \
  --device-id field-sensor-01 --generate \
  --allowed-services 'telemetry-*,inventory'

# List enrolled devices and their status.
dwx edge list --context kind-dwx-a

# Preview the object without applying it.
dwx edge enroll --device-id field-sensor-01 --generate --dry-run

# Render a wg-quick config to hand to the device. A managed terminator assigns
# the device address; in pure open core, supply --address yourself.
dwx edge profile --context kind-dwx-a \
  --device-id field-sensor-01 \
  --endpoint edge.example.com:51821 \
  --peer-public-key <terminator-public-key> \
  --route-cidrs 241.0.0.0/8 \
  --address 100.80.0.5/32
```

A device only **carries traffic** once an edge terminator is running. The free path to that is the **BYO-overlay + gateway** combination above. **Premium (DataWerx Edge)** provides the managed terminator that programs each device as a roaming `/32` peer, plus fleet enrollment and lifecycle — so `dwx edge profile` needs no manual `--address`. Design: **[docs/design/0013-edge-device-connector.md](docs/design/0013-edge-device-connector.md)**.

When you're done, tear the lab down with `hack/e2e/kind-down.sh`.

## Install on your own clusters

Install the published chart straight from GHCR — no clone needed:

```sh
helm install dwx oci://ghcr.io/datawerx/datawerx-mesh/charts/datawerx-mesh \
  -n datawerx-system --create-namespace \
  --set clusterID=cluster-a \
  --set fullnameOverride=dwx-mesh-agent
```

> `fullnameOverride=dwx-mesh-agent` names the DaemonSet exactly what the `dwx` CLI looks for by default, so `dwx mesh verify` and `dwx signal` work with no extra `--daemonset` flag. Omit it and pass `--daemonset <release>-datawerx-mesh` instead.

Then:

1. Declare each remote cluster as a `MeshPeer` (GitOps-friendly), or bootstrap with `dwx mesh join`.
2. Export Services with the standard `ServiceExport`.
3. Point CoreDNS at the `clusterset.local` zone (the chart and [quickstart](docs/quickstart.md) show how).

Common settings (full list in **[docs/configuration.md](docs/configuration.md)**):

| Setting | Helm value | Purpose |
|---|---|---|
| Cluster identity | `clusterID` | This cluster's identity in the mesh. |
| Local ranges | `localCIDRs` | Local pod/service ranges, for overlap detection. |
| Overlap remap | `remapCIDR` | Enable overlapping-CIDR remap (off → refuse + `Phase=Error`). |
| BYO overlay | `dataplane=routed` | Ride an existing overlay instead of owning WireGuard. |
| Remote-access gateway | `role=gateway`, `gateway.*` | Forward remote clients into the mesh (DataWerx Remote, free foundation). |
| Active probing | `probe.enabled` | Synthetic reachability probes that feed the connectivity SLO. |
| Premium control plane | `premium.saasEndpoint` | Point the agent at a managed control plane. |

### Verify, observe, operate

- **Health:** `dwx mesh verify` reports CRDs, the agent DaemonSet, peer phases, export validity, and import counts. `dwx mesh diagnose` adds rule-based "obvious cause" findings; `dwx mesh slo` reports the connectivity golden signals.
- **Metrics:** Prometheus instrumentation + a Grafana starter dashboard (`charts/datawerx-mesh/dashboards`).
- **Guides:** [operations](docs/operations.md) · [troubleshooting](docs/troubleshooting.md) · [security](docs/security.md).

## Documentation

| Guide | What it covers |
|---|---|
| **[Quickstart](docs/quickstart.md)** | Two clusters talking, in minutes — by hand. |
| **[Install the CLI](docs/install.md)** | `dwx` via Homebrew, download, or source; cosign verify; `dwx mcp` MCP setup. |
| **[Signal — ask an AI](docs/ai-agents.md)** | The read-only MCP server, the state contracts, and how to wire up Claude or any agent. |
| **[How it works](docs/how-it-works.md)** | The whole system in one page. |
| **[Architecture](ARCHITECTURE.md)** | Components, data flow, design rules. |
| **[Bring your own overlay](docs/byo-overlay.md)** | Run on Tailscale / NetBird / etc., and the gateway (Remote) role. |
| **[Cross-cluster services](docs/cross-cluster-services.md)** | Export, import, DNS; headless vs. ClusterSetIP. |
| **[Edge device connector](docs/design/0013-edge-device-connector.md)** | The `EdgeDevice` contract and the free/premium edge split. |
| **[MCS conformance](docs/mcs-conformance.md)** | Exactly how DataWerx maps to the MCS API. |
| **[Configuration](docs/configuration.md)** | Every setting, in one place. |
| **[Operations](docs/operations.md)** · **[Troubleshooting](docs/troubleshooting.md)** · **[Security](docs/security.md)** | Running it for real. |
| **[Examples](examples/)** · **[Changelog](CHANGELOG.md)** · **[Releasing](RELEASING.md)** | Terraform/Backstage starters, release notes, the release process. |

## Free forever, but we can help too

The open-source core is **Apache-2.0 and free forever**: encrypted connectivity (or BYO overlay), service discovery, cluster-set VIPs + DNS, overlapping-CIDR remap, network policy, the gateway role, the `EdgeDevice` contract, grounded `dwx signal` + the MCP server, the Helm chart, and metrics. **We won't move shipped features behind a paywall** — see **[COMMITMENT.md](COMMITMENT.md)**.

The premium tier adds org-scale conveniences — a managed control plane, zero-touch fleet auto-mesh, SSO/RBAC/audit, a topology UI, a high-performance eBPF engine, turnkey **Remote** access and managed **Edge** fleets, and a **Signal** AI SRE that explains *and fixes* — all **additive**, never required to run the mesh. See **[ROADMAP.md](ROADMAP.md)**.

## Project status

The core engine, cross-cluster DNS, overlap remap, the gateway role, the edge contract, grounded AI surfaces, observability, Helm packaging, and a multi-cluster e2e suite are in place; see **[ROADMAP.md](ROADMAP.md)** for the milestone-by-milestone state and the free/premium lines.

## Contributing & security

Contributions welcome under Apache-2.0 — see **[CONTRIBUTING.md](CONTRIBUTING.md)** and **[CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md)**. To report a vulnerability, see **[SECURITY.md](SECURITY.md)**.

## License

[Apache-2.0](LICENSE).
</content>
</invoke>
