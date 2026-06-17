# DataWerx Mesh

New here? Read these in order.  They're short.

1. **[Quickstart](quickstart.md)** — link two clusters and call a service across them in ~5 minutes.
2. **[How it works](how-it-works.md)** — the mental model: what a packet does and why.
3. **[Cross-cluster services](cross-cluster-services.md)** — export a Service, resolve it by name everywhere.

## Guides

- **[Install the CLIs](install.md)** — `dwxctl` and the `dwx-mcp` MCP server via Homebrew, download, or source.
- **[Ask an AI about your mesh](ai-agents.md)** — point Claude or any agent at the live mesh state through the read-only MCP server.
- **[Bring your own overlay](byo-overlay.md)** — run DataWerx on top of Tailscale, NetBird, Cilium, or plain WireGuard.
- **[Configuration](configuration.md)** — every environment variable and Helm value, plus metrics and the `dwxctl` health check.
- **[Logging](logging.md)** — structured output, verbosity levels, and field conventions for debugging and diagnosis.
- **[Operations (day-2)](operations.md)** — reboot recovery, key rotation, upgrades/skew, health checks.
- **[Active mesh probing](active-probing.md)** — observe true cross-cluster reachability, not just the handshake.
- **[Troubleshooting](troubleshooting.md)** — symptoms → causes → fixes.

## Reference

- **[Design notes (ADRs)](design/)** — the decisions behind cross-cluster DNS, overlapping-CIDR remap, and the eBPF datapath.
- **[MCS conformance](mcs-conformance.md)** — what of the Multi-Cluster Services API is implemented, and the known deltas.
- **[Threat model](security.md)** — trust boundaries, threats, and operator responsibilities.
- **[CoreDNS wiring](../config/coredns/README.md)** — forward the `clusterset.local` zone in production.
- **[Helm chart](../charts/datawerx-mesh/README.md)** — chart values reference.

## Project

- **[What's free forever](../COMMITMENT.md)** · **[Roadmap](../ROADMAP.md)** · **[Architecture](../ARCHITECTURE.md)**
- **[Contributing](../CONTRIBUTING.md)** · **[Security](../SECURITY.md)**
