# DataWerx Mesh — Roadmap


## Positioning

**DataWerx Mesh is CNI-agnostic, GitOps-native, broker-less multi-cluster
networking.** A per-node WireGuard DaemonSet links clusters with no central
broker and no gateway-node chokepoint, configured declaratively through
Kubernetes CRDs.

## Market context & differentiation

| Product | Layer | Architecture | DNS / discovery | Overlapping CIDRs | CNI lock-in | Managed control plane | License |
|---|---|---|---|---|---|---|---|
| **Submariner** | L3 | Central **broker** + **gateway nodes** | Lighthouse (MCS) — free | Globalnet — free | No | **None** | Apache-2.0 (CNCF) |
| **Cilium ClusterMesh** | L3/4 | Per-node (eBPF) | Transparent — free | Limited | **Yes (Cilium)** | Isovalent (paid) | OSS + commercial |
| **Skupper** | L7 | App proxy (VAN) | Per-namespace | N/A (proxy) | No | Red Hat | Apache-2.0 |
| **Tailscale** | L3 | Per-node WireGuard | MagicDNS (device, not svc) | N/A | No | SaaS (paid, per-user) | OSS client + closed |
| **KubeSlice (Avesha)** | L3 | Overlay "slices" | Auto (per slice) | Namespace sameness | No | Enterprise (paid) | OSS + enterprise |
| **DataWerx Mesh** | L3 | **Per-node, broker-less — own WireGuard _or_ ride your existing overlay** | **MCS (free)** | **Basic remap free / eBPF paid** | **No** | **Paid SaaS** | **Apache-2.0 core + commercial** |

**Where we are distinct (not a replica):**

1. **Broker-less + per-node data plane.** Every node owns its own `dwx-mesh0`
   and routes directly to remote CIDRs. No designated gateway node funnels
   cross-cluster traffic (Submariner's model) and no central broker is a single
   point of failure. This is Cilium's per-node performance story **without**
   requiring Cilium as the CNI.
2. **CNI-agnostic.** Works regardless of the cluster's CNI (vs. Cilium
   ClusterMesh's hard requirement).
3. **GitOps-native.** Topology is declarative CRDs authored by the user's
   pipeline — no imperative `subctl join` / broker handshake. Reconcile is the
   same in free and paid.
4. **Managed control plane is the paid product, not the connectivity.**
   Submariner has *no* managed offering — that is open white space. We sell the
   org-scale operational layer, not the packets.

**Assumption validated / revised by research:**

- ✅ *Cross-cluster DNS must be free.* Every credible competitor includes it.
  Putting it behind a paywall makes the free tier a non-starter. → **M1.**
- 🔁 *Overlap handling cannot be paid-only.* Submariner Globalnet ships
  overlapping-CIDR support for free; charging for basic overlap would make us
  look worse than a free alternative. → **Basic 1:1 remap is free (M3); the
  high-performance eBPF engine is the paid upsell.**
- ✅ *Identity, governance, fleet automation, and a managed plane are the
  defensible paid line.* This is exactly where Tailscale, KubeSlice, and
  Isovalent actually charge.

## The free / paid line

The test for any feature: *"Does a small team self-hosting need this to
succeed?"* → **free**. *"Does it only matter at org scale / with compliance?"* →
**paid**.

| Capability | Free (Apache-2.0) | Paid (commercial) |
|---|---|---|
| Per-node WireGuard L3 connectivity | ✅ | ✅ |
| Run on your existing overlay (Tailscale/NetBird/…) — routed mode | ✅ | ✅ |
| GitOps `MeshPeer` CRDs | ✅ | ✅ auto-generated |
| Cross-cluster DNS (MCS `clusterset.local`) | ✅ | ✅ global / hierarchical |
| Overlapping CIDRs | ✅ basic 1:1 remap | ✅ **eBPF high-perf engine** |
| Topology source | self-authored CRDs | ✅ **managed SaaS, zero-touch auto-mesh** |
| Identity | ServiceAccount RBAC | ✅ **SSO/OIDC, RBAC, multi-tenant** |
| Observability | Prometheus metrics + status | ✅ **UI, topology graph, dashboards** |
| Audit & compliance | — | ✅ **audit log, FIPS builds, policy** |
| Cost attribution | — | ✅ **per-cluster/per-slice cost insights** |
| Traffic engineering | — | ✅ **QoS, multipath, HA relays** |
| Support | community | ✅ **SLA, managed upgrades** |

## Milestones

### Milestone 0 — Core engine ✅ (shipped)
- Per-node WireGuard data plane (`pkg/wg`), `MeshPeer` CRD, reconciler.
- Open-core seam (`ControlPlaneClient`): `LocalGitOpsClient` + premium stub.
- Pure topology logic (`pkg/topology`), full unit suite, CI (race + coverage).

### Milestone 1 — Cross-cluster DNS & MCS ✅ (shipped)
Make exported services resolvable by name across the mesh.
- [x] `ServiceExport` / `ServiceImport` CRDs (MCS API, `multicluster.x-k8s.io`).
- [x] Design doc for export→import→DNS pipeline (`docs/design/0001-cross-cluster-dns.md`).
- [x] Export controller: watch `ServiceExport` + `Service`, publish `EndpointExport`.
- [x] `EndpointExport` wire format (broker-less, GitOps-mirrored / SaaS-materialized).
- [x] Import controller: aggregate `EndpointExport`s into a local `ServiceImport`.
- [x] Consistent, broker-less ClusterSetIP allocation (pure, hash-based).
- [x] CoreDNS `clusterset.local` integration (per-pod responder + forward config).
- [x] Headless service / `EndpointSlice` propagation (pod IPs unioned mesh-wide).
- [x] ClusterSetIP data-plane DNAT/load-balancing (`pkg/nat`, kube-proxy-style
      iptables; pure planner + applier + reconciler).
- [x] envtest coverage for the controllers (`test/integration`, `//go:build
      integration`): real API-server validation of finalizers, the status
      subresource, the export→import→ServiceImport flow, and CRD schema enums.


### Milestone 2 — Production hardening
- [x] Controller integration tests (`envtest`), gated `//go:build integration`.
- [x] Data-plane integration tests (netns + root), gated `//go:build dataplane`
      (`pkg/wg` real device/routes, `pkg/nat` real iptables; nightly CI).
- [x] Multi-cluster e2e (`kind` × 2) — connectivity + headless DNS + teardown
      (`test/e2e`, `//go:build e2e`; `hack/e2e/` setup; nightly CI workflow).
- [~] Data-plane hardening: **WireGuard key rotation** (reconciler tears down the
      stale peer on a `PublicKey` change) and **NAT traversal** (configurable
      listen port + persistent-keepalive) done; **IPv6** rule formatting in the
      NAT planner fixed (`/128`, bracketed `--to-destination`). Remaining:
      full dual-stack ClusterSetIP allocation + ip6tables applier, and in-place
      key hot-reload (restart-to-rotate works today).
- [x] Prometheus metrics (`pkg/metrics`: cache-backed state collector +
      DNS/NAT event metrics on the controller-runtime registry) + Grafana
      starter dashboard; observability docs in `docs/configuration.md`.
- [x] Helm chart (`charts/datawerx-mesh`: DaemonSet, RBAC, CRDs, DNS Service,
      premium opts, metrics Service/ServiceMonitor; lint+template CI).
- [x] `dwxctl verify` health diagnostics (`cmd/dwxctl` + pure `pkg/verify`:
      CRDs, agent DaemonSet, peer phases, export validity, import counts).

### Milestone 3 — Overlap handling (free basics)
- [x] Basic 1:1 NAT remap of overlapping remote CIDRs into a virtual `172.x`
      range (parity with Submariner Globalnet) — **free**, opt-in via
      `DataWerx_REMAP_CIDR`. Pure core (`topology.VirtualCIDR`/`PlanRemap`,
      `nat.BuildRemapRules`) + iptables NETMAP applier + reconciler wiring, all
      unit + dataplane tested. Design + the bidirectional source+dest NAT model:
      [`docs/design/0002-overlap-nat-remap.md`](docs/design/0002-overlap-nat-remap.md).
      End-to-end NAT-direction correctness is gated by the two-cluster overlap
      e2e (`OVERLAP=1`, `test/e2e`), which runs nightly in CI alongside the
      distinct-CIDR run.
- [ ] Premium **eBPF** high-performance remap engine — **paid**, private repo.

### Milestone 4 — Free GA
- [x] 15-minute two-cluster quickstart (`docs/quickstart.md`).
- [x] Security posture + reporting + operator hardening checklist (`SECURITY.md`).
- [x] Supply chain: release pipeline with multi-arch image, **SBOM (Syft)** +
      **cosign keyless signing/attestation**, packaged Helm chart
      (`.github/workflows/release.yml`).
- [x] Apache-2.0 `LICENSE` + written **"free forever"** pledge and
      `ControlPlaneClient` stability guarantee (`COMMITMENT.md`).
- [ ] Hosted docs site (content lives in `docs/`).

### Milestone 5 — Intelligence & adoption
The "nice project → essential service" layer: make the mesh's state a
first-class free artifact, collapse onboarding friction, and build the free data
contracts the hosted AI plane consumes. The strategy and guardrails live in
[`docs/design/0004-intelligence-and-adoption.md`](docs/design/0004-intelligence-and-adoption.md);
the rule is absolute — **the OSS binary stays model-free; the free hook is always
a data contract, never an inference dependency.**

| Capability | Free hook (Apache-2.0) | Paid seam (hosted/closed) |
|---|---|---|
| **Mesh state snapshot** | versioned `MeshSnapshot` JSON via `dwxctl snapshot` ([0005](docs/design/0005-mesh-snapshot.md)) | fleet-wide ingestion, history, cross-cluster rollups |
| **Connectivity diagnosis** | rule-based "obvious cause" checker citing signals (`dwxctl diagnose`) | hosted LLM → root cause + exact fix, opens the PR ("AI SRE") |
| **Zero-friction join** | `dwxctl join` bootstrap bundle ([0006](docs/design/0006-zero-friction-join.md)) | zero-touch fleet auto-mesh from one SSO token |
| **Read-only mesh MCP** | `dwx-mcp` read tools over the snapshot ([0007](docs/design/0007-mesh-mcp-server.md)) | **act** MCP (apply/mutate) behind SSO/RBAC + audit |
| **Intent dry-run** | pure change-impact planner (`dwxctl policy --dry-run`, [0008](docs/design/0008-intent-dryrun-planner.md)) | LLM compiling NL → CRDs, gated by the free planner |

- [x] Mesh state snapshot — pure assembly in `pkg/verify`, schema-versioned JSON,
      `dwxctl snapshot` / `verify --output json`.
- [x] Rule-based connectivity diagnosis — `verify.Diagnose`, grounded findings
      that cite their signal, surfaced by `dwxctl diagnose`.
- [x] Zero-friction join — pure `pkg/bootstrap` bundle/keygen + `dwxctl join`
      export/import (idempotent reciprocal `MeshPeer` authoring).
- [x] Read-only mesh MCP server — `cmd/dwx-mcp`, stdio JSON-RPC, zero mutating
      tools in the free build (free-read / paid-act seam).
- [x] Intent dry-run / change-impact planner — pure `pkg/impact` composing
      `meshfw` + `topology`, `dwxctl policy --dry-run`.
- [ ] Hosted AI diagnosis, NL→CRD compiler, and act MCP — **paid**, private repo,
      consuming the contracts above over `EnterpriseControlPlaneClient`.
- [ ] BYO-LLM / on-prem inference tier for the FIPS/compliance audience.

### Premium track (parallel, private repo)
- [ ] Managed control plane / SaaS (`EnterpriseControlPlaneClient` backend).
- [ ] Zero-touch fleet registration & auto-mesh formation.
- [ ] Web UI: topology graph, live health, throughput/handshake dashboards.
- [ ] SSO/OIDC + RBAC + audit log + multi-tenancy.
- [ ] Global / hierarchical service discovery.
- [ ] eBPF overlap engine; QoS / multipath / HA relays.
- [ ] Cost attribution; SLA & support; FIPS / compliance builds.

## GA gates

**Free GA:** two clusters, GitOps-applied `MeshPeer`s; a `ServiceExport`ed
service in cluster A resolves and is reachable by `<svc>.<ns>.svc.clusterset.local`
from cluster B; overlapping CIDRs communicate via basic remap (or fail with a
clear message pre-M3); metrics scrape; documented upgrade path; a stranger can
reproduce it from the README in 15 minutes.

**Paid GA:** SSO login → cluster auto-joins from a token → mesh forms with zero
hand-written CRDs → dashboard shows live health → audit log captures the change.

## How the Open and Closed/Premium halves are managed

- **Public repo (this one):** the entire free product + the *interfaces* premium
  plugs into. Must build and run standalone. License: **Apache-2.0**.
- **Private repo:** `EnterpriseControlPlaneClient` impl, SaaS backend, eBPF
  engine, UI. Imports the public module; implements its interfaces. License:
  **commercial EULA**.
- **Injection:** most premium is *server-side* (the SaaS the agent calls over
  REST) — naturally closed, never shipped to users. Client-side premium bits use
  Go build tags (`//go:build enterprise`) or a separate `cmd/manager-enterprise`.
  The OSS binary stays 100% pure.
- **Governance:** Apache-2.0 core; audit deps for GPL/AGPL in CI; trademark the
  name; DCO/CLA on contributions; pre-commit the "free forever" line to avoid
  the trust backlash that has hit other open-core vendors.