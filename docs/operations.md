# "Day 2" operations

How DataWerx Mesh behaves under the events that matter to operators, and the
procedures for the ones that need action.

## Node reboot / agent restart (automatic)

The agent holds no durable state of its own. The source of truth is the
`MeshPeer` CRDs (and, in the premium tier, the control plane that authors them).
On restart it performs a **full-state reconcile**: it re-reads every `MeshPeer`
and re-programs the WireGuard peers, host routes, NAT, and MSS clamp to match.

- WireGuard peers and routes installed in the kernel survive a process
  restart.  The agent does not flush them on shutdown, so traffic keeps flowing during a rolling agent upgrade.
- After a full **node reboot** (kernel state lost), the agent re-creates
  everything from the CRDs on startup. No manual step is required. This recovery is covered by a unit test (`TestReconcile_RecoversAfterRestart`).
- The reconcilers are **idempotent and drift-correcting**. They reconcile against the complete current object set, and the MSS-clamp/return-route ensurers re-apply on an interval, so a CNI restart or firewall reload self-heals.

## WireGuard key rotation

Each node has a private key from the `DataWerx_WG_PRIVATE_KEY` Secret. Its public key is what remote clusters program as a peer. To rotate keys:

1. Generate a new keypair and update this cluster's `DataWerx_WG_PRIVATE_KEY` Secret and restart the agent (DaemonSet rollout). `SyncInterface` loads the new key **without flushing existing peers** (`ReplacePeers: false`), so the device keeps serving while the key changes.
2. Update the **public key** this cluster advertises in every remote cluster's
   `MeshPeer` for it (GitOps in the free tier; the control plane in premium).
   Until step 2 propagates, remote peers can't complete a handshake with the new
   key, so rotate during a maintenance window or rotate one side at a time.

Rotating a *remote* peer's key is handled automatically: changing
`spec.publicKey` makes the reconciler remove the stale peer and program the new
one (`TestReconcile_KeyRotationRemovesStalePeer`).

## Upgrades and version skew

- The agent is a DaemonSet, so an upgrade is done with a normal rolling update. Kernel state persists across the pod restart, so the data path is not interrupted.
- The **CRD contract (`MeshPeer`, MCS types)** is the compatibility surface. It is `v1alpha1` and additive-only within the alpha; new fields are optional and ignored by older agents, so a brief agent/CRD skew during a rollout is safe.  Apply CRD updates before rolling the agent.
- In the premium tier the topology syncer short-circuits on an unchanged
  revision, so a mixed-version fleet converges to the same `MeshPeer` set.

## Routine health checks

- `dwxctl verify` — read-only health check for CRDs present, agent DaemonSet ready,   MeshPeers `Connected`, exports/imports valid). Safe to run anytime; exits non-zero on failure, so it doubles as a smoke test in CI.
- `dwxctl snapshot` — emit the full versioned mesh state as stable JSON (peers, conflicts, exports/imports, policies, recent events, metric pointers); pipe it into `jq` or diff it over time.
- `dwxctl diagnose` — rule-based "obvious cause" analysis of why the mesh is unhealthy, each finding citing the concrete signal it read; exits non-zero on a critical cause so it can gate a pipeline.
- `dwxctl graph [--format json|dot|mermaid]` — render the mesh dependency graph (this cluster, its peers, and the services that flow between them); `--format dot | dot -Tsvg` draws it, `--format mermaid` embeds inline in a README or runbook.
- `dwxctl reach [--output text|json]` — the expected cross-cluster reachability into this cluster: for each remote cluster, Reachable / Blocked / Degraded / Unreachable with the grounded reason (peer phase, CIDR conflict, or default-deny policy). The direct answer to "why can't A reach B".
- `dwxctl slo [--output text|json]` — connectivity golden signals: reconciles expected reachability against observed tunnel liveness (the WireGuard handshake) into one verdict per cluster — Healthy, Impaired (should be reachable but the tunnel is dead), Blocked, Degraded, or Down. Exits non-zero on any Impaired cluster.
- These read-only commands share one gather path with the `dwx-mcp` MCP server, so they can never disagree about what the mesh looks like.
- `dwxctl snapshot --schema` / `dwxctl graph --schema` print the JSON Schema for each artifact (no cluster access). The schemas are published under [`docs/contracts/`](contracts/) for consumers to validate against.
- Alert on `dwx_meshpeers{phase!="Connected"}`,   `dwx_clusterset_nat_syncs_total{result="error"}`, and
  `dwx_remap_syncs_total{result="error"}`.
- Stale tunnels: `dwx_meshpeer_last_handshake_timestamp_seconds` going stale
  (now − value growing) indicates a peer that can't handshake — check the
  endpoint reachability and keys.

## Common failures

See [troubleshooting](troubleshooting.md) — including the MTU section for the
"handshake works, large transfers hang" symptom.
