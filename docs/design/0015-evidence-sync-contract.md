# Design 0015 — `/api/v1/evidence` (agent → control-plane evidence sync)

- Status: **Proposed.** Server endpoint + persistence in `datawerx-admin`
  (`internal/signal`, `internal/store`, `migrations/0009`, `POST /api/v1/evidence`);
  the premium-agent client in `datawerx-mesh` is not yet implemented.
- Builds on: the "DataWerx Signal" seam (0014), the FIXED open-core agent contract
  (`datawerx-admin/internal/agentapi`), and the grounded evidence the open-core
  CLI assembles (`pkg/signal.Evidence`, 0005).

## Summary

Deliniation between where/how the **premium-tier** agent pushes its grounded `Evidence`
(snapshot/diagnosis/reachability/connectivity) to the managed control plane,
so the control plane can do what the ego-centric open-core agent structurally
cannot.  Aggregate across clusters into a fleet view, and retain
it over time for incident timelines and drift.

This is **additive** to the fixed `/api/v1` contract. The unmodified open-core
agent never calls it. Evidence sync is a premium behaviour, gated the same way
the SaaS topology source is. An open-core agent keeps working against the same
control plane with no evidence sync at all.

## Endpoint

```
POST /api/v1/evidence
Authorization: Bearer <machine token>
Content-Type: application/json

<body: pkg/signal.Evidence JSON — exactly what `dwx-signal --print-context` emits>

200 OK  {"ok": true}
```

- **Auth:** the per-cluster machine bearer token, resolved by `agentapi.authenticate`.
  The cluster identity (tenant + cluster) comes from the **credential, never the
  body** — a cluster can only report its own evidence.
- **Idempotent upsert:** the control plane keeps the **latest** report per
  `(tenant, cluster)`. A new push replaces the previous one; the agent self-reports
  on a cadence, so there is no create/conflict semantics.
- **Status codes** (match the existing agent contract so the agent's retry logic
  is unchanged): `400` invalid/non-object payload, `401` missing/invalid/revoked
  token, `503` store temporarily unavailable (the agent retries with capped
  backoff, exactly as for `POST /api/v1/services`).
- **Body size:** capped at 4 MiB (`maxEvidenceBody`) — one cluster's grounded
  evidence is small; the headroom covers a large peer set and a verbose diagnosis.

## Persistence

`migrations/0009_cluster_evidence.sql` — `cluster_evidence(tenant_id, cluster_id,
generated_at, payload JSONB, reported_at, created_at)`, PK `(tenant_id, cluster_id)`.
The agent's evidence is stored **verbatim** as JSONB (`payload`) so the fleet
aggregation re-parses exactly what the agent observed; `generated_at` is the
agent's snapshot clock, `reported_at` the server clock. Tenant-scoped at the query
layer like every other store resource.

## Aggregation

`internal/signal` is the pure decision half (no DB/HTTP/clock), mirroring
`internal/topology`/`internal/catalog`:

- `ParseReport(clusterID, payload)` → `Report` extracts the diagnosis and
  connectivity subset; ignores unknown evidence fields so the contract can grow.
- `Aggregate([]Report)` → `Fleet`: per-cluster rollup verdict
  (`Healthy`/`AtRisk`/`Faulted`), a fleet `Summary`, the grounded `Problems` list
  (still citing each finding's `signal`), and a deterministic `Digest`.
- `Fleet.Digest` is a stable fingerprint (order-independent), so a poller or UI
  short-circuits when nothing changed which is the same pattern as `topology.Revision`.

The human/UI **fleet read surface** (`/api/v1/admin/...`, RBAC-gated) is the next
step.  It lists stored evidence, runs `ParseReport`+`Aggregate`, and serves the
`Fleet`. Managed inference (fleet Q&A) and governed actions layer.

## Contract discipline

`datawerx-admin` (private repo) shares no code with the agent, so `internal/signal` **mirrors**
the `pkg/signal.Evidence` shape rather than importing it — the discipline
`internal/topology.RemotePeerConfig` already follows. Keep the two in lockstep and
guard with a shape test, as the agent API is guarded by `compat_test.go` today.

## Cadence & privacy

The premium agent should push evidence on its existing sync cadence
(`DataWerx_SYNC_INTERVAL`) or on observed change. A future revision/digest gate can
suppress unchanged pushes. Note the tradeoff - evidence, 
topology, diagnosis **leaves the cluster** to the managed control plane. That is
the managed-tier model the user chose over the in-cluster-licensed alternative.
