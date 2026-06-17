# Design 0012 — Active synthetic probing

- Status: **Implemented (runtime; live path e2e-validated).**
- Milestone: **M5 (Intelligence & adoption).**
- Package: `pkg/probe` (pure core + thin runtime shell), wired in
  `cmd/manager/main.go` behind `DataWerx_PROBE_ENABLE`.
- Completes the runtime half of 0011: the second provider of the observed
  connectivity signal `slo.Assess` reconciles.

## Summary

The connectivity golden signals (0011) reconcile *expected* reachability against
an *observed* liveness signal, and the first provider of that signal is the
WireGuard handshake age the agent already records. A handshake proves the tunnel
is up; it does not prove an application packet can cross it. The active prober
closes that gap. Every node serves a tiny **responder**, and every node dials its
connected peers' responders, so the mesh continuously answers the sharper
question — *can cluster A actually reach a workload in this cluster right now?* —
and records the answer as the very same `slo.Liveness` value the handshake feeds.

```
node A prober  ──HTTP GET /dwx/probe──►  node B responder ──► 200 {clusterID:"B"}
      │                                                              │
      └─ probe.Classify ──► probe.Observations ──► slo.Liveness ──► slo.Assess
```

## Why it is a drop-in, not a new pipeline

0011 deliberately abstracted the observed signal behind `slo.Liveness` and
documented that "the active synthetic prober ... slots in under `Assess` with no
change to the verdict engine." This design honors that to the letter:
`probe.Observations.Liveness(now)` returns `map[cluster]slo.Liveness`, the exact
input `slo.Assess` already takes. A fresh successful probe is an age near zero
(live → **Healthy**); a failed or never-seen probe is a negative age (not live →
**Impaired** when reach says the peer *should* be reachable). The verdict logic,
its tests, and both read surfaces are untouched. `TestProberSignalFeedsSLO` makes
the claim executable.

## Pure core, thin shell

As with the rest of the tree, the decisions are pure and exhaustively tested with
no network:

- `PlanTargets(peers)` — dial only connected, conflict-free peers that advertise
  a responder; skip the rest, since probing them would only re-state what reach
  already explains.
- `Classify(cluster, status, body, rtt, now, err)` — the whole verdict for one
  dial. Only a 200 carrying the **expected cluster's** envelope is healthy. The
  cluster-ID cross-check is deliberate: a 200 from the *wrong* cluster means
  traffic is misrouted (often a CIDR overlap), which is a failure even though
  something answered.
- `Observations.Liveness(now)` — projects the latest per-cluster result into the
  `slo.Liveness` signal, clamping clock skew so a slightly-future observation
  still reads as live.

The runtime shell — `Responder` (an HTTP `manager.Runnable`) and `Prober` (a
`manager.Runnable` that dials on a ticker) — performs only the I/O. The `Prober`
takes a `ProbeFunc` and a `PeerLister` seam, so its loop is unit-tested with
fakes; the live cross-cluster dial is exercised by the kind e2e, which is the
honest boundary for a feature that fundamentally needs two clusters and a kernel.

## Surface and configuration

Off by default. `DataWerx_PROBE_ENABLE=true` turns on both halves on every agent.

| Variable | Purpose | Default |
|----------|---------|---------|
| `DataWerx_PROBE_ENABLE` | Enable the responder and prober. | `false` |
| `DataWerx_PROBE_RESPONDER_ADDR` | Responder listen address. | `:9998` |
| `DataWerx_PROBE_PORT` | Port the prober dials on each peer's endpoint host. | the responder port |
| `DataWerx_PROBE_INTERVAL` | Probe cadence. | `30s` |
| `DataWerx_PROBE_TIMEOUT` | Per-dial timeout. | `5s` |

A peer's responder is dialed at its WireGuard **endpoint host** paired with the
probe port: the endpoint host is the node's mesh-reachable address, and that node
runs the responder. Operators expose the responder port the same way they expose
the WireGuard endpoint.

Metrics: `dwx_probe_results_total{cluster,result}` and `dwx_probe_rtt_seconds`
join the existing series on the manager's `/metrics`. A growing failure series
for a connected peer is the live "should reach, can't" signal.

## Open-core boundary

Free: the responder, the prober, and the local observed signal feeding `slo`
on each node — the OSS produces a real, current verdict for *this* node's view.
Paid: the hosted plane that ingests those verdicts fleet-wide into a true
connectivity SLO — history, error budgets, alerting, an RCA timeline, latency and
loss percentiles from the probe stream, and the AI SRE that proposes the fix.
The mechanism is open; reasoning over it across the fleet and over time is hosted.

## Testing

- `pkg/probe` — table-driven unit tests (94%): target planning, every `Classify`
  branch (healthy, dial error, non-200, bad envelope, misrouted cluster), the
  `Observations` → `slo.Liveness` projection including clock skew, the
  responder handler and its real listener lifecycle, a loopback `Responder` ⇄
  `httpProbe` round-trip, the prober cycle with fakes, and the executable
  `slo.Assess` drop-in claim. The status writeback adds: the `NextProbeStatus`
  churn-control fold, the cycle handing results to the `Publisher`, the snapshot
  `Probed`/`ProbeAge` derivation, and the end-to-end `slo.FromSnapshot` proof
  that a fresh probe supersedes the handshake. No second cluster.
- e2e (kind, roadmapped with the rest of the integration suite) — the only place
  the true cross-cluster dial can run, gated behind `//go:build integration`.

## Writing the observation back to the read surfaces

The prober records its verdict onto the peer's `MeshPeer` status, so the
read surfaces show probe-observed liveness rather than only the handshake. Two
`int64` status fields carry it, mirroring `LastHandshakeTime`:

- `lastProbeAttempt` — epoch of the most recent probe, any outcome.
- `lastProbeTime` — epoch of the most recent *successful* probe.

`verify.BuildSnapshot` derives a per-peer `Probed` flag and `ProbeAge` from those
(trusting the probe only while the last attempt is within
`StaleHandshakeSeconds`, so disabling the prober reverts to the handshake on its
own), and `slo.FromSnapshot` uses `ProbeAge` in place of the handshake age
whenever `Probed` is set. The result: a peer whose tunnel handshook fine but
whose application probe is failing now reports **Impaired** through `dwxctl slo`
and `mesh_connectivity`, and a peer with a stale handshake but a fresh successful
probe reports **Healthy**.

Because `MeshPeer` is cluster-scoped and every node probes, the status reflects
the most recent probing node's view, last-writer-wins. To bound write churn the
prober persists through the pure `probe.NextProbeStatus` fold, which writes only
when the healthy/unhealthy state flips or the stored attempt has aged past a
refresh window — at most about once a minute per peer per node. The write touches
only the status subresource fields the reconciler leaves alone, so the two
writers never collide.

## Scope / non-goals

- **Latency / loss golden signals** — the prober records RTT as a metric; turning
  it into percentile SLOs is the hosted layer.
- **Fleet-wide SLO / error budgets / history** — hosted, per the boundary above.
