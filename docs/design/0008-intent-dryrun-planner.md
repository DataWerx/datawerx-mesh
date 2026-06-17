# Design 0008 — Intent dry-run / change-impact planner

- Status: **Implemented (the free planner).**
- Milestone: **M5 (Intelligence & adoption).**
- Package: `pkg/impact` (pure), surfaced by `dwxctl policy --dry-run`.
- Implements 0004 §3.5 (the free half).

## Summary

Before a `MeshNetworkPolicy` or `MeshPeer` change is applied, report what it
would expose, what it would conflict with, and what it would break. Pure logic,
no model, exhaustively testable — and useful on its own as a safety net.

Nobody writes cross-cluster policy confidently by hand; an over-broad allow or a
CIDR-conflicting peer is easy to author and painful to discover in production.
The dry-run planner makes a change *legible before it lands*. It is also the
foundation the paid natural-language→CRD compiler stands on: **generation is the
paid layer; the dry-run that makes any generated change safe to trust is free.**

## What it analyzes

### Policy changes — `AnalyzePolicyChange`

Given the current policy set and a proposed set (the CLI builds the proposed set
by replacing/adding the policy under review), it compiles **both** through the
real firewall compiler (`meshfw.BuildFirewall`) and diffs the results:

- **Newly exposed** — reachabilities the proposed set permits that the current
  set did not (the blast radius).
- **Newly denied** — reachabilities the current set permitted that the proposed
  set drops (what the change would *break*).
- **Newly / no-longer protected** — destinations entering or leaving default-deny.
- **Warnings** — the footguns: an any-source allow (`0.0.0.0/0`), a
  default-deny-everything posture (a policy with no destinations), and selectors
  naming clusters the topology doesn't know (which silently resolve to no
  sources, so the rule allows nothing).
- **Skipped** — inputs the compiler dropped (non-IPv4 today).

Because it diffs the output of the *real* `meshfw` compiler — not a reimplementation
— the analysis is provably consistent with what the data plane will actually
program.

### Peer changes — `AnalyzePeerChange`

Given a proposed `MeshPeerSpec`, the local ranges, and the existing peer set, it
runs the real `topology.PlanPeer` and `topology.DetectTopologyConflicts` and
reports:

- the resulting **phase** and routable vs **withheld** CIDRs (overlap/malformed);
- **dangerous** CIDRs that are never safe to route;
- **new topology conflicts** the proposed peer introduces against the existing
  peers (overlap, duplicate ID, shared key) — computed as the set difference
  between conflicts *with* and *without* the proposed peer, so only genuinely new
  problems surface.

`PeerImpact.Safe()` is true only when nothing is withheld, dangerous, or newly
conflicting — a one-line gate for "is this peer clean to apply?".

## Composition, not new semantics

`pkg/impact` owns no policy or topology semantics of its own. It composes the
existing pure compilers and diffs their outputs, which keeps it consistent with
the data plane and keeps the whole thing side-effect-free and table-testable with
no cluster — the same discipline as `pkg/topology`, `pkg/dns`, and the `pkg/nat`
planners.

## Surface

`dwxctl policy --dry-run -f <manifest>` reads a proposed `MeshNetworkPolicy` or
`MeshPeer` from a YAML/JSON file, gathers the current state from the cluster, and
prints the impact (or `--output json`). It never applies anything — only
`--dry-run` is supported, by design. For a peer change it exits non-zero when the
result isn't `Safe()`, so it can gate CI.

## Open-core boundary

Free: this pure dry-run/impact planner. Paid: the hosted LLM that compiles
natural language ("let payments/prod reach ledger in the EU cluster, deny the
rest") into the right CRDs, then runs *this* free planner for impact analysis
before proposing — and opens the PR. The generator is server-side; the planner
that makes it safe is open core.

## Testing

- `pkg/impact` — table-driven unit tests: new exposure/protection, the three
  over-broad warnings, newly-denied detection, peer overlap-with-local (withheld
  + not safe), peer conflict-with-existing (new conflict surfaced), and a clean
  peer reported `Safe`. No cluster.

## Scope / non-goals

- The natural-language compiler (paid, server-side) is not in this repo.
- Impact across more than the policy/peer dimensions (e.g. DNS/import effects of
  a peer change) is a future extension; the snapshot (0005) gives it the inputs.
