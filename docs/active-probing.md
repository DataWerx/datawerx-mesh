# Active mesh probing

DataWerx's connectivity verdicts (`dwxctl slo`, the `mesh_connectivity` MCP tool)
reconcile what topology and policy *expect* against what the mesh is *observed* to
be doing. Out of the box the observed signal is the WireGuard handshake age every
agent already records — a handshake proves the tunnel is up. Active probing adds
a sharper observation: it proves an application packet can actually cross.

When enabled, every agent does two things:

- **Responds.** It serves a tiny HTTP responder at `/dwx/probe` that returns a
  signed-by-cluster-ID envelope. This is what remote clusters dial to prove they
  can reach *into* this one.
- **Probes.** It dials each of its connected peers' responders on a timer and
  records the result, feeding the same liveness signal the handshake feeds.

A 200 from the wrong cluster is treated as a failure, not a success: it means
traffic is being misrouted (often a CIDR overlap), which is exactly the kind of
silent breakage active probing exists to catch.

## Enable it

Probing is off by default. Turn it on for every agent:

```yaml
env:
  - name: DataWerx_PROBE_ENABLE
    value: "true"
```

| Variable | Purpose | Default |
|----------|---------|---------|
| `DataWerx_PROBE_ENABLE` | Enable the responder and the prober. | `false` |
| `DataWerx_PROBE_RESPONDER_ADDR` | Responder listen address (`host:port`). | `:9998` |
| `DataWerx_PROBE_PORT` | Port the prober dials on each peer's endpoint host. | the responder port |
| `DataWerx_PROBE_INTERVAL` | How often to dial every peer. | `30s` |
| `DataWerx_PROBE_TIMEOUT` | Per-dial timeout. | `5s` |

The prober dials each peer at its WireGuard **endpoint host** paired with the
probe port. The endpoint host is the node's mesh-reachable address and that node
runs the responder, so you expose the responder port the same way you expose the
WireGuard endpoint — through the node's network path that peers already use.

## What you get

Two metrics on the manager's `/metrics`:

- `dwx_probe_results_total{cluster,result}` — successes and failures per peer. A
  rising `result="failure"` series for a connected peer is the live
  "should reach, can't" signal.
- `dwx_probe_rtt_seconds` — round-trip latency of successful probes.

Failures are logged at info with a grounded reason (`dial failed`,
`HTTP 503`, `misrouted`, …); successes log at debug (`-v 1`).

## How it relates to `dwxctl slo`

The prober writes its verdict back to each peer's `MeshPeer` status
(`lastProbeAttempt` / `lastProbeTime`), and `dwxctl slo` and the
`mesh_connectivity` MCP tool prefer that probe-observed liveness over the
handshake whenever it is recent. So a peer that policy and topology permit and
whose tunnel handshook — but whose application probe is failing — comes out
**Impaired**, the one verdict that means "configured right, not working".
Disabling the prober reverts the verdict to handshake-observed on its own once
the last probe ages out. Check it with:

```sh
dwxctl slo --output text
kubectl get meshpeer <name> -o jsonpath='{.status.lastProbeTime}'
```

See [design 0012](design/0012-active-probing.md) for the architecture and
[design 0011](design/0011-connectivity-golden-signals.md) for the verdict engine.

## Open-core boundary

The OSS ships the responder, the prober, and the local verdict on each node. The
hosted plane ingests those verdicts fleet-wide into a real connectivity SLO —
history, error budgets, alerting, latency and loss percentiles from the probe
stream, and the AI SRE that proposes the fix.
