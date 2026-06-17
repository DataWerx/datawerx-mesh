# Open-core commitment

DataWerx Mesh is **open core**. This document is our written, public promise
about what that means — so you can build on the free tier with confidence.

## Free forever

The following are **Apache-2.0 licensed and will remain free forever**. We will
not move them behind a paywall:

- The **node agent** and its data plane (per-node WireGuard connectivity,
  routing, ClusterSetIP NAT/load-balancing).
- **Bring-your-own-overlay (routed) mode** — running the full Kubernetes
  multi-cluster layer on top of an existing Tailscale / NetBird / Cilium /
  WireGuard / cloud-VPN transport. The k8s layer is free; you keep your transport.
- The `MeshPeer` CRD and the **GitOps (self-hosted) topology** path
  (`LocalGitOpsClient`).
- **Cross-cluster DNS** via the MCS API (`ServiceExport` / `ServiceImport` /
  `clusterset.local`) — export, import, the responder, and headless propagation.
- **Basic overlapping-CIDR** handling.
- The Helm chart, `dwxctl`, Prometheus metrics, and the Grafana dashboard.

If we ever need to revise this list, we will do so transparently and never
retroactively for an already-released version.

## What is premium

The paid tier is **additive** and targets org-scale operation, not core
connectivity: the managed SaaS control plane, zero-touch fleet automation, the
web UI, SSO/RBAC/audit/multi-tenancy, global/hierarchical discovery, the
high-performance eBPF overlap engine, and traffic engineering. See `ROADMAP.md`
for the full free/paid line.

## API stability promise

The seam between the open and commercial halves is the **`ControlPlaneClient`
interface** (`pkg/client/controlplane.go`). We treat it as a published contract:

- Within a `v1alpha1`/`v0.x` line, changes are additive where possible; breaking
  changes are called out in release notes with a migration note.
- The `MeshPeer`, `ServiceExport`, `ServiceImport`, and `EndpointExport` API
  shapes follow normal Kubernetes API-versioning conventions — additive within a
  version; a new version (e.g. `v1beta1`) for breaking changes, with conversion.

## License & contributions

- Core is licensed under **Apache-2.0** (see `LICENSE`).
- Contributions are accepted under the same license (DCO sign-off). By
  contributing you agree your work may ship in both the open core and, where it
  touches shared interfaces, the commercial product.
