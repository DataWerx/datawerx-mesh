# Changelog

All notable changes to DataWerx Mesh are recorded here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project aims to
follow [Semantic Versioning](https://semver.org/spec/v2.0.0.html) once it reaches
a tagged release.

The open-core boundary is stable and load-bearing: every entry below ships in the
Apache-2.0 binary. Premium components live behind the `ControlPlaneClient` seam
and `pkg/agent.Options.RegisterPremium` (in a separate private repo) and are
called out as such.

## [Unreleased]

Nothing yet — changes land here after v0.1.0.

## [0.1.0] — 2026-06-20

The first release: a working, broker-less, multi-cluster Kubernetes networking
mesh, the read-only intelligence surfaces over it, and the supply-chain pipeline
that ships it.

### Added

- **Core data plane.** Per-node WireGuard agent as a DaemonSet, the `MeshPeer`
  CRD, and a thin reconciler over the pure planner in `pkg/topology`. Key
  rotation, configurable NAT-traversal (listen port + persistent-keepalive), and
  a cross-cluster TCP MSS clamp so the reduced mesh MTU never black-holes large
  flows.
- **Open-core seam.** One interface, `ControlPlaneClient` — `LocalGitOpsClient`
  (free, GitOps-authored `MeshPeer` CRDs) and the premium
  `EnterpriseControlPlaneClient` — selected once at startup. The reconcile loop
  never branches on tier.
- **Bring-your-own-overlay mode.** `pkg/routed` programs host routes over an
  existing overlay (Tailscale, NetBird, Cilium, plain WireGuard) instead of
  owning a device.
- **Cross-cluster services (MCS, KEP-1645).** `ServiceExport`/`ServiceImport`,
  the broker-less `EndpointExport` wire format, deterministic hash-based
  ClusterSetIP allocation, a `clusterset.local` DNS responder, and a
  kube-proxy-style iptables data plane. Headless `ServiceImport`s also mirror
  their cross-cluster endpoints as `discovery.k8s.io` `EndpointSlice`s labeled
  `multicluster.kubernetes.io/service-name` and `…/source-cluster`, so native
  consumers and an MCS-aware kube-proxy discover them through the standard
  surface, not only the DNS responder.
- **Mesh firewall.** `MeshNetworkPolicy` compiled by the pure `pkg/meshfw` into
  an iptables filter-table applier.
- **Overlapping-CIDR remap.** Opt-in 1:1 NETMAP of conflicting remote ranges into
  a virtual pool (`DataWerx_REMAP_CIDR`); the high-performance eBPF backend is
  the premium path.
- **Remote-access gateway role.** `DataWerx_ROLE=gateway` lets laptops on a
  shared overlay reach the mesh, publishing an access-profile ConfigMap.
- **Read-only intelligence surfaces** (the free data contracts under design
  0004), each a pure core behind `dwxctl` and the `dwx-mcp` MCP server:
  health `verify`, mesh `snapshot`, rule-based `diagnose`, zero-friction `join`,
  the change-impact `policy --dry-run`, the dependency `graph`, the expected
  reachability matrix (`reach`), and the connectivity golden signals (`slo`).
- **Active synthetic probing.** An opt-in prober (`DataWerx_PROBE_ENABLE`) writes
  its verdict back onto `MeshPeer` status (`lastProbeAttempt`, `lastProbeTime`);
  `dwxctl slo` / `mesh_connectivity` prefer probe-observed liveness over the
  WireGuard handshake when it is recent. A peer that handshook but whose
  application probe fails reports **Impaired**; disabling the prober reverts to
  handshake-observed liveness on its own.
- **Operator tooling.** `dwxctl` CLI and the read-only `dwx-mcp` Model Context
  Protocol server, so an AI agent can ask about the live mesh without any
  mutating capability.
- **Observability.** Prometheus metrics (a cache-backed state collector plus
  DNS/NAT event metrics) and a starter Grafana dashboard.
- **Packaging & supply chain.** Helm chart (`charts/datawerx-mesh`), a distroless
  static image, and a release pipeline producing a multi-arch signed image, an
  SBOM, cosign keyless signatures, and GoReleaser CLI archives with a Homebrew
  cask.
- **Ecosystem starters.** Artifact Hub metadata, a Terraform module (with the
  `helm`/`kubernetes` providers pinned below 3.0 for the `set { }` block form),
  and a Backstage catalog entry under `examples/`. An `ecosystem` CI workflow
  validates the GoReleaser config, the Terraform module (`fmt` + `validate`), and
  the Artifact Hub / Backstage manifests so these artifacts can't silently rot.

### Security

- CI security posture: `govulncheck`, CodeQL (security-extended), OpenSSF
  Scorecard, Dependabot, Go native fuzzing of the parsers, and SHA-pinned GitHub
  Actions. Reporting policy and an operator hardening checklist in `SECURITY.md`.

[Unreleased]: https://github.com/DataWerx/datawerx-mesh/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/DataWerx/datawerx-mesh/releases/tag/v0.1.0
