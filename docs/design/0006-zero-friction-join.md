# Design 0006 — Zero-friction join (`dwxctl join`)

- Status: **Implemented (basic).**
- Milestone: **M5 (Intelligence & adoption).**
- Package: `pkg/bootstrap` (pure), surfaced by `cmd/dwxctl join`.
- Implements 0004 §3.2 (Lever A — collapse time-to-value).

## Summary

Turn "hand-author two `MeshPeer` CRDs and swap WireGuard keys" into a single
command. One cluster mints a small **bundle** describing itself; the other
consumes it and authors the reciprocal `MeshPeer`. Run it once each way and the
mesh forms with no hand-written CRDs.

This is the biggest free adoption lever. Today the manual flow (the
`hack/e2e/kind-up.sh` dance — generate keys, read endpoints, write reciprocal
CRDs by hand) converts admirers, not users. `dwxctl join` removes that friction
without changing the data plane or the reconcile loop at all: it just produces
the same `MeshPeer` objects a human would have written.

## The bundle

A `bootstrap.Bundle` is one cluster's self-description, exchanged to bootstrap a
peering. It carries exactly what the other side needs to reach this cluster — and
nothing secret:

```
clusterID      stable mesh ID
publicKey      WireGuard public key (shareable by design)
endpoint       reachable host:port
podCIDRs       advertised pod ranges
serviceCIDRs   advertised service ranges
```

No private key ever appears in a bundle. The bundle is rendered as a single
shareable **token**: a version tag, a dot, and the base64url-encoded JSON
(`dwxmesh.v1.<base64url>`). The tag lets a human eyeball what it is and lets the
decoder reject a foreign or future format early instead of decoding garbage.

## The flow

```
cluster A                                  cluster B
  dwxctl join export ──► tokenA  ───────►    dwxctl join import --bundle tokenA
  dwxctl join import ◄─────────── tokenB ◄── dwxctl join export
        │                                          │
   authors MeshPeer "b"                       authors MeshPeer "a"
```

- `dwxctl join export` mints and prints this cluster's bundle token. The public
  key is either supplied (the node's existing key) or freshly generated with
  `--generate`, in which case the private key is printed to **stderr** with
  instructions to store it as the node secret (`DataWerx_WG_PRIVATE_KEY`).
- `dwxctl join import --bundle <token>` decodes and validates the peer's bundle
  and authors the reciprocal `MeshPeer`. This is the one mutating `dwxctl`
  command; `--dry-run` prints the object instead of applying it.

The authored `MeshPeer` is named deterministically from the cluster ID
(`topology.SanitizeName`), so re-importing the same bundle is an idempotent
upsert rather than an error, and is tagged
`app.kubernetes.io/managed-by: dwxctl-join` to distinguish it from GitOps- or
syncer-authored peers for later cleanup and audit.

## Pure planner, thin shell

All the decision logic — bundle encode/decode, validation, and `MeshPeer`
authoring — is pure and deterministic in `pkg/bootstrap`, so it is exhaustively
table-tested with no cluster. The CLI is a thin shell that gathers inputs, calls
the planner, and applies the resulting object.

`Validate` is strict: the identity fields must be present, the public key must
parse as a WireGuard key, the endpoint must be `host:port`, and every advertised
CIDR must parse and be safe to route (it reuses `topology.IsDangerousCIDR`, so a
bundle can never smuggle in a default/loopback/link-local/multicast range). CIDRs
are normalized (trimmed, sorted, deduped) so a bundle for a given set of ranges
encodes identically regardless of input order.

Key generation is the one non-deterministic helper; it is crypto-only (no
socket) so it belongs here rather than in the kernel-touching `pkg/wg`.

## Open-core boundary

`dwxctl join` is free: it is the manual GitOps flow, automated. The **premium**
counterpart is zero-touch fleet auto-mesh — a cluster joins from a single SSO
token and the SaaS syncer (`pkg/syncer`) materializes every `MeshPeer`, with no
token exchange at all (already roadmapped as "zero-touch fleet registration &
auto-mesh formation"). The two share the same target: `MeshPeer` CRDs the
reconciler programs identically.

## Testing

- `pkg/bootstrap` — table-driven unit tests: token round-trip, order-independent
  encoding, rejection of foreign tokens and every bad field, deterministic
  peer-object naming/labelling, and keypair generation producing parseable keys.
- shell round-trip (`hack/e2e/join_test.sh`, hermetic, in CI) — builds `dwxctl`,
  pipes `join export` into `join import --dry-run`, and asserts the reciprocal
  `MeshPeer` carries the right identity, CIDRs, and join label, and that garbage
  and foreign tokens are rejected. No cluster.
- e2e (`test/e2e/join_test.go`, `-tags e2e`) — `TestJoinFormsMesh` wipes both
  clusters' `MeshPeer`s and re-forms the mesh with `dwxctl join` only, then
  asserts every peer reaches Connected. `hack/e2e/join.sh` mirrors the manual
  flow and is wired into `hack/e2e/kind-up.sh` behind `JOIN=1`, so the harness
  forms the mesh with no hand-written CRDs.

## Scope / non-goals

- A short-lived `MeshBootstrap` CR fronting the exchange (0004 sketched it as an
  option) is not implemented; the token is sufficient and broker-less.
- Endpoint auto-discovery (reading a LoadBalancer/NodePort address) is left to
  the operator passing `--endpoint`; auto-discovery can follow.
- Key *rotation* via join is out of scope (the reconciler already tears down a
  stale peer on a `PublicKey` change; re-importing an updated bundle upserts).
