# Design 0010 — Expected reachability matrix

- Status: **Implemented (expected reachability).**
- Milestone: **M5 (Intelligence & adoption).**
- Package: `pkg/reach` (pure), surfaced by `dwxctl reach` and the `dwx-mcp`
  `mesh_reachability` tool.
- Implements the free, deterministic floor under 0004 §4 #2 (connectivity
  golden signals).

## Summary

Answer *"why can't cluster A reach this one?"* deterministically. For each remote
cluster, `pkg/reach` reports whether it is expected to reach into the local
cluster — Reachable, Blocked, Degraded, or Unreachable — with the grounded
reason and a per-destination breakdown. It is the single most-asked
multi-cluster question, turned into a first-class, cited answer.

The snapshot (0005) already says *whether a peer is connected* (phase, handshake)
and the dependency graph (0009) says *who could talk to whom*; this composes both
with policy to say **whether traffic is actually expected to flow, and what stops
it**. It is the substrate the AI diagnosis stands on — a model can now ground an
explanation in a concrete reachability verdict instead of inferring one.

## Expected, not observed

This is **expected** reachability, derived from declared state: peer phase,
topology conflicts, and the compiled `MeshNetworkPolicy` set. It does not send a
packet. The complementary **observed** reachability — active synthetic probes
recorded back onto status (0004 §4 #2) — is the roadmapped runtime counterpart;
the two are designed to be compared, and the expected matrix is what makes a
probe result interpretable (*"the probe failed, and policy says it should have
succeeded → a data-plane bug, not a policy one"*).

## What it computes

For each remote cluster, in order:

1. **Connectivity** — not a Connected peer ⇒ **Unreachable** ("the tunnel is not
   established; nothing flows regardless of policy"). This is the dominant real
   cause.
2. **Routing** — Connected but named in a topology conflict (a CIDR overlap) ⇒
   **Degraded** ("routing is ambiguous; resolve the conflict before reachability
   is reliable").
3. **Policy** — Connected and clean: run the cluster's CIDRs against the real
   firewall.
   - No `MeshNetworkPolicy` protects anything ⇒ **Reachable** (open by default).
   - Protected destinations exist and at least one permits the cluster ⇒
     **Reachable** (with a per-destination breakdown of the rest).
   - Protected destinations exist and none permit the cluster ⇒ **Blocked**.

The `Dests` breakdown lists each protected local CIDR and whether the cluster is
allowed to it, so the answer is specific enough to act on.

## Composed, not re-derived

The policy verdict goes through the **real compiler**: `meshfw.BuildFirewall`
compiles the policy set exactly as the data plane does, and a new exported
`meshfw.Interpret` reads the compiled ruleset back into accept/protected
decisions. `pkg/reach` (and `pkg/impact`, refactored onto the same helper)
consume those decisions, so a reachability verdict is **provably consistent with
the iptables rules the agent programs** — never a parallel re-implementation of
firewall semantics. Because `meshfw` resolves a cluster-ID selector to that
cluster's CIDRs, matching a peer's advertised CIDRs against the firewall's
source values is exact.

## Surface

- `dwxctl reach [--output text|json]` — the matrix from live cluster state.
- `dwx-mcp` `mesh_reachability` tool — the same matrix as JSON, so an agent can
  ask the reachability question directly. Read-only, like every tool there.

Both build from the same `verify.Snapshot` every other read command uses (the
snapshot now carries policy ingress sources, added for exactly this), so the
matrix can never disagree with `dwxctl snapshot`, `graph`, or `diagnose`.

## Open-core boundary

Free: this pure, model-free expected-reachability matrix. Paid: the hosted plane
that ingests it fleet-wide alongside observed-probe history for a connectivity
SLO, RCA timeline, and the AI SRE that proposes the fix — consuming this contract
over the existing telemetry seam. The OSS produces the verdict; the hosted layer
reasons over it across the fleet and time.

## Testing

- `pkg/reach` — table-driven unit tests (97%+): not-connected ⇒ Unreachable,
  conflict ⇒ Degraded, no-policy ⇒ open, allow vs default-deny ⇒
  Reachable/Blocked, partial allow, any-source allow, determinism/sorting, and
  the `FromSnapshot` end-to-end projection. No cluster.
- `cmd/dwx-mcp` — a dispatch test asserts `mesh_reachability` returns the matrix
  with the expected verdict for the injected snapshot.

## Scope / non-goals

- **Observed reachability** (active probes) — the runtime counterpart, roadmapped.
- **Egress / cross-peer pairs** — the matrix is ingress-into-this-cluster, which
  is what `MeshNetworkPolicy` governs and what a local snapshot can answer
  authoritatively. A fleet-wide all-pairs matrix is a hosted concern (it needs
  every cluster's snapshot).
- **Port-level verdicts** — the breakdown is per protected destination CIDR;
  per-port reachability is a future refinement (the compiled ruleset carries the
  ports already).
