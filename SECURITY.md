# Security

## Reporting a vulnerability

Please report suspected vulnerabilities **privately** — do not open a public
issue. Use GitHub's "Report a vulnerability" (Security → Advisories) on this
repository, or email the maintainers. We aim to acknowledge within 3 business
days and to ship a fix or mitigation for confirmed high-severity issues
promptly.

For trust boundaries, assets, and the full threat/mitigation table, see the
[threat model](docs/security.md).

## Security posture

DataWerx Mesh is a node agent that programs kernel networking, so it runs with
elevated privileges by design. We minimize and contain that surface:

- **Minimal image.** The agent ships in `gcr.io/distroless/static:nonroot` — no
  shell, no package manager, statically linked (`CGO_ENABLED=0`).
- **Least privilege (target).** The agent needs `NET_ADMIN` (+ the `wireguard`
  module) to manage `dwx-mesh0`, routes, and iptables NAT. The example manifest
  and chart default to `privileged: true` for the simplest bring-up; **tighten
  to the `NET_ADMIN`/`SYS_MODULE` capability set in production** (the chart's
  `securityContext` is overridable).
- **Scoped RBAC.** The ClusterRole grants only the verbs the controllers use on
  the DataWerx + MCS resources (and read-only on Services/EndpointSlices). No
  blanket access.
- **Secret handling.** The WireGuard private key is provided via a Kubernetes
  Secret (projected env), never baked into the image. **Full public keys are
  never logged** — they are truncated via `shortKey`.
- **Tiers.** The premium SSO token (`DataWerx_ENTERPRISE_SSO_TOKEN`) is read from
  the environment and sent only as a bearer token to the configured SaaS
  endpoint over HTTPS.

## Supply chain

Release images are built reproducibly (`-trimpath`), published with an **SBOM**
(Syft), and **signed with cosign** (keyless / OIDC). Verify a release image:

```sh
cosign verify <image> \
  --certificate-identity-regexp 'https://github.com/DataWerx/datawerx-mesh/.*' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

CI runs `go vet`, `gofmt`, the race detector, and (separately) envtest, kind
e2e, and root data-plane suites. Dependencies are vendored through the Go module
proxy with checksum verification (`go.sum`).

The supply chain is scanned continuously: **`govulncheck`** (reachable-vuln
analysis of the module and the Go toolchain), **CodeQL** (static analysis with
the `security-extended` queries), and the **OpenSSF Scorecard** (posture
scoring, badged on the README) all run on every push and weekly. **Dependabot**
keeps the Go modules and GitHub Actions current so those scans stay meaningful.
CodeQL and Scorecard upload to code scanning, which requires a public repository
(or GitHub Advanced Security); they are gated on visibility and activate
automatically once the repository is public, so `govulncheck` is the scan that
runs regardless.

The pure parsers that handle untrusted or external input are **fuzzed** — most
importantly the join-bundle decoder (`bootstrap.Decode`, which parses a token
from another cluster), plus CIDR parsing, the cluster-ID→object-name sanitizer,
and the `clusterset.local` DNS-name parser. Their seed corpora run on every push;
a scheduled job does continuous active fuzzing.

Every GitHub Actions step is **pinned to a full commit SHA** (with a version
comment), so a moved or compromised tag cannot change what CI runs; Dependabot
updates the pins. Maintainers should also enable **branch protection** on `main`
— require pull requests and passing status checks (the `build & unit test`,
`govulncheck`, and, once the repo is public, `CodeQL` checks) and disallow force
pushes — which is the remaining OpenSSF Scorecard lever that is a repository
setting rather than code.

## Hardening checklist for operators

- [ ] Replace `privileged: true` with the `NET_ADMIN` + `SYS_MODULE` capability
      set once you've confirmed it works on your nodes.
- [ ] Project a real WireGuard private key from a Secret (not the ephemeral
      generated key).
- [ ] Restrict who can create/modify `MeshPeer` objects — a `MeshPeer` is
      authorized network access into the cluster.
- [ ] Run `dwx mesh verify` after install and in monitoring.
- [ ] Scrape the metrics endpoint and alert on `dwx_meshpeers{phase!="Connected"}`
      and `dwx_clusterset_nat_syncs_total{result="error"}`.
