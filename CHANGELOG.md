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

Nothing yet — changes land here after v0.3.0.

## [0.3.0] — 2026-06-25

### Added

- **DataWerx Signal — grounded AI over the mesh (designs 0014, 0015).** A
  read-only natural-language layer that answers questions about a mesh and
  returns a structured root-cause result. The model reasons over *only* the
  deterministic evidence the existing read surfaces already produce (snapshot,
  diagnosis, reachability, golden-signal SLO) and must cite the signal behind
  every claim, so an answer can never drift from what
  `dwx mesh snapshot/diagnose/reach/slo` report. It is reached over the
  Anthropic Messages API using only the standard library (no new module
  dependency); `dwx signal --print-context` prints the exact grounded evidence
  with no API key required. The pure grounding contract is `pkg/signal`;
  `pkg/evidence` adds the reporter that pushes Evidence to the premium control
  plane's additive `POST /api/v1/evidence` endpoint for the fleet view. The
  managed/fleet/historical tier is premium (datawerx-admin).
- **Unified `dwx` CLI (design 0016).** A single AWS-style binary fronts every
  free service as a noun: `dwx mesh` (verify/snapshot/diagnose/graph/reach/slo/
  policy/join), `dwx edge`, `dwx signal`, and `dwx mcp`. Premium services are
  discovered at runtime as `dwx-<service>` PATH plugins (kubectl-style), so the
  open core links no premium code. The CLI logic now lives in importable
  `internal/cli/{mesh,mcp,signalcli}` packages.

### Changed

- **`dwxctl`, `dwx-mcp`, and `dwx-signal` are deprecated aliases** for
  `dwx mesh`, `dwx mcp`, and `dwx signal`. They still ship and work (the cask
  installs all three) and `dwx` also honors them via multi-call when symlinked;
  `dwxctl`/`dwx-signal` print a one-line stderr deprecation hint.

## [0.2.0] — 2026-06-22

### Added

- **Edge device connector — open-core contract (design 0013).** A single
  non-Kubernetes device (an IoT box, a VM, a laptop) can reach mesh services by
  name over a WireGuard tunnel it dials outbound. The open core ships the
  tier-agnostic half:
  - The cluster-scoped `EdgeDevice` CRD (`networking.datawerx.io/v1alpha1`,
    short name `ed`) with hand-written deepcopy and the CRD YAML in `config/crd`
    and the Helm chart — the integration point both tiers program, exactly like
    `MeshPeer`.
  - The pure `pkg/edge` planner: deterministic broker-less device-IP allocation
    (`AllocateDeviceIPs`, mirroring `dns.AllocateClusterSetIPs`), the
    terminator-side peer plan (`PlanDevicePeer`, a device's own `/32` only), the
    device-side profile + `wg-quick` rendering (`BuildDeviceProfile`, reusing
    `gateway.AccessProfile`), the `dwxedge.v1` enrollment-token codec, and the
    fail-closed `ValidateEdgeCIDR` startup screen. Exhaustively table-tested and
    fuzzed.
  - `dwxctl edge` (`enroll`/`profile`/`list`) — authors the `EdgeDevice`
    contract via the pure planner and renders the device artifact;
    `--generate` keeps the private key on the device.

  The *managed* terminator, reconciler, and enrollment are premium (injected via
  `pkg/agent.Options.RegisterPremium`); the capability of edge reach also remains
  free via the BYO-overlay + gateway role. The reconcile loop and data plane
  never branch on tier.

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

[Unreleased]: https://github.com/DataWerx/datawerx-mesh/compare/v0.3.0...HEAD
[0.3.0]: https://github.com/DataWerx/datawerx-mesh/compare/v0.2.0...v0.3.0
[0.2.0]: https://github.com/DataWerx/datawerx-mesh/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/DataWerx/datawerx-mesh/releases/tag/v0.1.0
