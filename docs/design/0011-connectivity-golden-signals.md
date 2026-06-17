# Design 0011 — Connectivity golden signals

- Status: **Implemented (handshake-observed reconciliation).**
- Milestone: **M5 (Intelligence & adoption).**
- Package: `pkg/slo` (pure), surfaced by `dwxctl slo` and the `dwx-mcp`
  `mesh_connectivity` tool.
- Implements the free, deterministic floor under 0004 §4 #2 (connectivity SLO /
  golden signals).

## Summary

The reachability matrix (0010) says what topology and policy *expect*. This says
whether it is *working*: for each remote cluster it reconciles the expected
reachability against the **observed** connectivity — whether the tunnel is
actually passing traffic — into one verdict. The verdict that matters most is
**Impaired**: the cluster should be reachable, the configuration is correct, but
the tunnel is dead. That is the gap between "the YAML looks right" and "A can
reach B", made explicit.

## Where intent meets reality

```
pkg/reach (expected)  ─┐
                       ├─►  pkg/slo.Assess  ─►  per-cluster verdict + reason
observed liveness     ─┘
```

| Expected (reach) | Tunnel live? | Verdict | Meaning |
|------------------|--------------|---------|---------|
| Reachable | yes | **Healthy** | permitted and passing traffic — intent and reality agree |
| Reachable | no | **Impaired** | permitted but the handshake is stale/absent — traffic is not flowing |
| Blocked | — | **Blocked** | policy default-denies it; unreachability is intended, not a fault |
| Degraded | — | **Degraded** | a CIDR overlap makes routing ambiguous |
| Unreachable | — | **Down** | the peer is not connected at all |

`Report.Healthy()` is false only when something is **Impaired** — the one
verdict that means "should work, doesn't". `dwxctl slo` exits non-zero then, so
it can gate a deploy or page.

## The observed signal is abstracted

`Assess` takes the expected matrix and a `map[cluster]Liveness`; it does not care
*how* liveness was measured. Today the provider is the **WireGuard handshake age**
the agent already records (`FromSnapshot` reads it from the snapshot) — a real,
zero-cost observed signal: a recent handshake means the tunnel is passing
traffic. A peer absent from the map, or with a handshake older than
`verify.StaleHandshakeSeconds`, or that never handshaked, is not live.

This abstraction is deliberate. The **active synthetic prober** — a per-cluster
probe responder that an agent dials to test true application-layer reachability —
is a second provider of exactly the same `Liveness` signal, reconciled by exactly
this logic. It has since landed (design 0012, `pkg/probe`): it slots in under
`Assess` with no change to the verdict engine, via
`probe.Observations.Liveness()`, which returns the same `map[cluster]Liveness`.
The handshake provider ships the value with zero new infrastructure; the prober
sharpens the observed signal from "the tunnel handshook" to "a packet crossed".

## Composed, pure, consistent

`pkg/slo` composes `pkg/reach` (which composes `meshfw` + the snapshot), so a
connectivity verdict is consistent all the way down to the iptables rules the
data plane programs. It is pure — a function of a matrix and a liveness map — so
it is exhaustively table-tested with no cluster and no kernel, and both surfaces
build from the same `verify.Snapshot` as every other read command.

## Surface

- `dwxctl slo [--output text|json]` — the golden-signal report; non-zero exit on
  any Impaired cluster.
- `dwx-mcp` `mesh_connectivity` tool — the same report as JSON, so an agent can
  ask "is the mesh actually working?" and get the expected-vs-observed verdict.

## Open-core boundary

Free: this pure, model-free reconciliation over signals the agent already has.
Paid: the hosted plane that ingests the verdicts fleet-wide for a real
connectivity **SLO** — history, error budgets, alerting, an RCA timeline, and the
AI SRE that proposes the fix — plus the active-probe data feeding the same
contract. The OSS produces the verdict; the hosted layer reasons over it across
the fleet and time.

## Testing

- `pkg/slo` — table-driven unit tests (94%): Reachable+live ⇒ Healthy,
  Reachable+stale/absent/never ⇒ Impaired, Blocked/Degraded/Down pass-through,
  the just-handshaked (age 0) edge, `Healthy()`, determinism/sorting, and the
  `FromSnapshot` end-to-end projection. No cluster.
- `cmd/dwx-mcp` — a dispatch test asserts `mesh_connectivity` returns the report
  with the expected verdict for the injected snapshot.

## Scope / non-goals

- **Active synthetic probing** (application-layer reachability via a probe
  responder) — the runtime provider of the observed signal, delivered in design
  0012 (`pkg/probe`); this design received it unchanged.
- **Fleet-wide SLO / error budgets / history** — hosted, per the boundary above.
- **Latency / throughput golden signals** — the handshake gives a binary live
  signal; richer signals arrive with the active prober and `pkg/metrics`.
