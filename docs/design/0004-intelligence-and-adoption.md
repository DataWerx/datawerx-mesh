# 0004 — Intelligence & Adoption Strategy (handoff brief)

> Status: **strategy / not yet scheduled.** This is a forward-looking brief, not
> a shipped design. It captures the "nice project → essential service" thesis and
> the AI layer that stacks on top, expressed in this repo's existing open-core
> free-hook / paid-seam discipline (see `ROADMAP.md`, `ARCHITECTURE.md`,
> `COMMITMENT.md`). Companion design docs (`0005+`) should be spun out per item
> as work is scheduled.

---

## 0. How to use this document (read first if you are a future session)

You are likely a fresh Claude Code session asked to implement or extend one of
the items below. Before writing code:

1. **Honor the open-core seam.** The free/premium split is the single
   `ControlPlaneClient` interface (`pkg/client/controlplane.go`) plus the
   `pkg/agent.Options.RegisterPremium` injection seam (the premium operator lives
   in a separate private repo). The reconcile loop and data plane
   **never branch on tier**. Premium is additive. Read §2 before building
   anything — it is the law for everything here.
2. **The prime directive of this brief:** *the OSS binary stays model-free and
   tiny.* What ships free is a **data contract** (structured state, events,
   dry-run planners). The **intelligence** (LLM/ML reasoning, generation,
   autonomous action) lives in the hosted premium plane and consumes those
   contracts. Put the contract in OSS; put the model in the SaaS.
3. **Ground everything in the pure-logic convention.** "Compute desired state
   from inputs" goes in a side-effect-free package (`pkg/topology`, `pkg/dns`,
   `pkg/nat` planners, `pkg/verify`), exhaustively unit-tested with no
   Kubernetes and no kernel. Reconcilers/managers/CLIs are thin shells.
4. **Pick up work by the sequence in §3.** Items are ordered by leverage and
   dependency. §4/§5 are the full catalogs (nothing dropped); §6 is the
   guardrails; §7 lists the concrete repo artifacts to produce; §8 is a map of
   the existing seams you will plug into.

---

## 1. Strategic framing (the "why" — do not lose this)

**The gap between "nice project" and "essential service" is not more L3.**
Submariner, Cilium ClusterMesh, and Tailscale already own "connect the
clusters" and are better funded. Competing on connectivity features keeps
DataWerx a nice project. You become *essential* by owning a **job that is
painful, recurring, and currently unowned**, and by raising switching cost.

Two levers, and everything in this brief serves one of them:

- **Lever A — collapse time-to-value** (the adoption flywheel). Today, free
  onboarding means hand-authoring `MeshPeer` CRDs and exchanging WireGuard keys
  (the `hack/e2e/kind-up.sh` dance). That friction converts admirers, not users.
- **Lever B — become the day-2 system of record** for multi-cluster: the thing
  on-call opens when "A can't reach B," the thing that holds the
  topology/policy/flow truth. Once it knows the whole mesh, leaving is expensive.

### The keystone insight (this is the whole strategy in one idea)

The agent sits at a vantage point **no competitor has in one place**: every node
sees the full cross-cluster topology, the live data plane (it *programs* the
NAT/routes), the service dependency graph (who exports/imports what),
connectivity health (handshakes, CIDR conflicts), and policy. That is an
extraordinarily rich, structured, multi-source state.

> **Make a machine-readable "mesh state snapshot" a first-class free artifact.**

It pays off twice: it is simultaneously the **day-2 observability hook** that
drives adoption (Lever B) *and* the **substrate every AI feature consumes**
(§5). One free data contract, two strategic payoffs. It is item #1 for a reason
— build it first; the rest compounds on it.

---

## 2. The open-core seam discipline (guardrails — binding)

Every item below obeys these. Violating them breaks either the "free forever"
trust promise (`COMMITMENT.md`) or the "tiny and boring" OSS property.

1. **OSS is model-free.** The free hook is a *data contract* (a JSON schema, a
   CRD, an event stream, a pure planner), never an inference dependency. The
   moment the OSS binary needs to call a model, the discipline is broken.
2. **Data contract in OSS; model in the SaaS.** The hosted premium plane
   consumes the free contract over the existing seam — the
   `EnterpriseControlPlaneClient` REST channel and/or a telemetry push. Most
   premium is server-side and naturally closed; operator-side premium lives in a
   separate private repo and injects via `pkg/agent.Options.RegisterPremium`.
3. **Grounded + verifiable in infra.** Any AI diagnosis must cite the concrete
   signal it used (peer phase, handshake age, CIDR conflict, NAT rule, policy).
   Any AI *action* goes through a **dry-run + governed/audited** path, never a
   silent auto-apply. One confidently-wrong route or policy change erodes trust
   permanently.
4. **Free-read / paid-act.** Reading mesh state is free and drives adoption.
   *Acting* on the mesh (apply fix, mutate policy) is the governed, audited,
   SSO/RBAC-gated paid surface. This is the cleanest seam in the whole product —
   use it deliberately (esp. for the MCP server, §3.3).
5. **Data sensitivity is a product, not just a risk.** Topology and flows are
   sensitive. A **bring-your-own-LLM / on-prem inference** option becomes its
   own paid tier for the FIPS/compliance audience the roadmap already targets.
6. **No AI-washing.** If a feature would work as well on a generic log stream,
   it is a commodity wrapper anyone can clone. Every AI play here must consume
   the *cross-cluster graph* that only DataWerx holds — that is the moat.

---

## 3. The execution sequence (1–5) — the backbone

Build in this order. Each item lists: **Goal**, **Why**, **Free hook**, **Paid
seam**, **Where it plugs in** (real files/interfaces), **Acceptance**, **Deps**.

### 3.1 — Mesh state snapshot (the keystone)

- **Goal:** a single, versioned, machine-readable snapshot of the whole mesh's
  observed state.
- **Why:** keystone (§1). Day-2 observability hook *and* the substrate for every
  AI feature. Do this first.
- **Free hook:** extend `pkg/verify` (pure) to assemble a `MeshSnapshot` struct
  and emit it as stable JSON; expose via `dwxctl` (e.g. `dwxctl snapshot` /
  `dwxctl verify --output json`). Contents: peers + phases + handshake age,
  CIDR conflicts, exports/imports, ClusterSetIP allocations, `MeshNetworkPolicy`
  set, recent reconcile events, throughput/handshake metric pointers. Version
  the schema (`apiVersion`-style field) so consumers and the SaaS can evolve.
- **Paid seam:** the SaaS ingests snapshots fleet-wide (via
  `EnterpriseControlPlaneClient` / telemetry push) for history, cross-cluster
  rollups, and as the input to AI diagnosis (§3.4).
- **Where it plugs in:** `pkg/verify` (pure assembly — keep it side-effect-free,
  reuse the report logic that already backs `dwxctl verify`), `cmd/dwxctl`
  (surface), `pkg/metrics` (pointers/values). Inputs come from the same CRDs the
  reconcilers read (`MeshPeer`, `EndpointExport`, `ServiceImport`,
  `MeshNetworkPolicy`).
- **Acceptance:** `dwxctl snapshot` on the two-cluster kind e2e emits valid,
  schema-versioned JSON capturing peers, a synthetic CIDR conflict, an
  export→import, and a policy; covered by unit tests (pure) + an e2e assertion.
- **Deps:** none. Foundation for #3, #4, #5.

### 3.2 — Zero-friction join (`dwxctl join`)

- **Goal:** turn "hand-author MeshPeer + swap WG keys" into one command.
- **Why:** Lever A. The single biggest adoption lever; removes the friction that
  currently converts admirers instead of users.
- **Free hook:** `dwxctl join` that mints/consumes a bootstrap bundle between two
  clusters (keypair generation, endpoint discovery, reciprocal `MeshPeer`
  authoring) — possibly fronted by a short-lived `MeshBootstrap` CR. Keep the
  cryptographic/topology decisions in a pure planner; the CLI is the shell.
- **Paid seam:** zero-touch fleet auto-mesh — a cluster joins from one SSO token
  and the SaaS syncer (`pkg/syncer`) materializes all `MeshPeer`s (already
  roadmapped as "zero-touch fleet registration & auto-mesh formation").
- **Where it plugs in:** `cmd/dwxctl`, a new pure `pkg/bootstrap` (or extend
  `pkg/topology`) for the bundle/handshake logic, `pkg/wg` key generation
  helpers, `MeshPeer` authoring. Mirror the manual flow in `hack/e2e/kind-up.sh`
  so the e2e harness can adopt it.
- **Acceptance:** two fresh kind clusters meshed via `dwxctl join` only, no
  hand-written CRDs; the existing e2e connectivity assertions pass.
- **Deps:** independent of #1, but ship after/with it so the snapshot can verify
  the joined state.

### 3.3 — Read-only MCP server for the mesh

- **Goal:** expose mesh read-state as an MCP server so any AI agent can query
  "what does cluster B import? is the mesh healthy?"
- **Why:** where the puck is going (agentic ops); cheap; on-brand; drives
  developer adoption; and it is the **textbook free-read / paid-act seam** (§2.4).
- **Free hook:** a read-only MCP server over the §3.1 snapshot + CRDs (list
  peers, list imports/exports, get health, get policy). Ships as an opt-in
  component; reuses the snapshot schema so there is one source of truth.
- **Paid seam:** the **act** MCP — apply a fix, mutate `MeshNetworkPolicy`,
  rotate a key — gated behind SSO/RBAC and **audit logging**. Same governed
  surface premium uses elsewhere.
- **Where it plugs in:** new `cmd/dwx-mcp` (or a subcommand) reading via the same
  client the controllers use; consumes `pkg/verify`'s snapshot. Keep all writes
  out of the free build.
- **Acceptance:** an MCP client (e.g. Claude) connected to a kind mesh can
  answer health/topology/import questions sourced from the live snapshot; the
  free build exposes zero mutating tools.
- **Deps:** #1 (consumes the snapshot).

### 3.4 — Hosted AI connectivity diagnosis ("why can't A reach B?")

- **Goal:** answer the #1 multi-cluster support ticket automatically, with the
  exact fix.
- **Why:** sharpest pain; the data moat is real *today* (the signals exist only
  in your data plane). First real AI revenue.
- **Free hook:** the §3.1 snapshot is the input contract. Optionally a free,
  rule-based "obvious cause" checker in `pkg/verify` (e.g. CIDR conflict →
  `Phase=Error`, stale handshake, missing import) — deterministic, no model.
- **Paid seam:** a hosted LLM agent ingests snapshot + history → root cause +
  the exact CRD/policy change, and can open the PR. "An AI SRE for your mesh."
  Lives entirely server-side. Must cite the signals it used (§2.3).
- **Where it plugs in:** OSS side = snapshot (§3.1) + optional rule checker in
  `pkg/verify`; premium side = SaaS consuming via `EnterpriseControlPlaneClient`
  / telemetry. BYO-LLM/on-prem variant per §2.5.
- **Acceptance (OSS portion):** the rule-based checker explains a seeded CIDR
  overlap and a stale-handshake peer from the snapshot alone, with the signal
  cited; unit-tested.
- **Deps:** #1.

### 3.5 — Intent dry-run planner → NL→CRD compiler

- **Goal:** "let payments/prod reach ledger in the EU cluster, deny the rest" →
  the right CRDs, safely.
- **Why:** nobody writes cross-cluster policy by hand; intent-based networking is
  the high-value, low-supply job. The CRDs are already your declarative target.
- **Free hook:** a **pure dry-run/impact planner** — given a proposed change to
  `MeshPeer`/`MeshNetworkPolicy`, report what it would expose, what it conflicts
  with, what it would break. No model; pure logic, exhaustively testable. Hugely
  useful on its own and a safety net for any generator.
- **Paid seam:** the LLM that compiles natural language → CRDs, then runs the
  free planner for impact analysis before proposing. Generation is paid; the
  dry-run that makes it *safe* is free.
- **Where it plugs in:** extend `pkg/topology` (overlap/conflict logic already
  lives there — `DetectTopologyConflicts`, `PlanPeer`, `PlanRemap`) and
  `pkg/meshfw` (the pure firewall compiler) into a change-impact analyzer;
  surface via `dwxctl policy --dry-run`. Premium NL compiler is server-side.
- **Acceptance:** `dwxctl policy --dry-run` flags an over-broad policy and a
  CIDR-conflicting peer change before apply; pure unit tests cover the analyzer.
- **Deps:** benefits from #1; the planner itself is independent.

---

## 4. Full value-add catalog (non-AI) — nothing dropped

Ordered by leverage. (#1–#5 here feed the sequence in §3; kept verbatim so the
fidelity survives.)

| # | Value-add | Free hook (Apache-2.0) | Paid seam (hosted/closed) |
|---|---|---|---|
| 1 | **Zero-friction join** — replace the hand-authored `MeshPeer` + WG-key swap. | `dwxctl join` bootstrap bundle / `MeshBootstrap` CR | Zero-touch fleet auto-mesh from one SSO token (roadmapped) |
| 2 | **Connectivity SLO / golden signals** — answer "is it healthy and why not." | Continuous synthetic reachability probes → connectivity matrix as CR status + metrics + events | Historical SLO, alerting, RCA timeline, multi-tenant rollups |
| 3 | **Resilience for cross-cluster services** — locality-aware LB + failover on the existing ClusterSetIP path. | Prefer-local + failover-remote routing | Weighted/canary, QoS, multipath, HA relays (roadmapped) |
| 4 | **Service dependency map** — the cross-cluster "who talks to whom" graph. | Structured graph artifact (Grafana/Backstage-consumable) | Polished topology UI + history + drift |
| 5 | **Ecosystem fit = distribution.** | Backstage plugin, Terraform provider for `MeshPeer`s, Argo/Flux health, OTel export | Hosted fleet-wide aggregation |

> If only one ships: **#1 + the snapshot (§3.1).** Friction removal converts
> admirers into users; the snapshot makes them stay.

---

## 5. Full AI product catalog — nothing dropped

**Thesis:** AI on generic infra is a commodity wrapper; AI on *your*
cross-cluster graph + live data plane is defensible. Every play attaches to the
snapshot/flow data (§1 keystone), never to generic chat. Seam discipline per §2.

| AI product | Free hook | Paid seam | Why defensible / essential |
|---|---|---|---|
| **AI connectivity diagnosis** (§3.4) | Mesh snapshot from `dwxctl verify`/`snapshot` | Hosted LLM → root cause + exact fix (opens PR). "AI SRE for your mesh." | The signals (peer phase, handshake, CIDR conflict, NAT, policy, DNS) exist only in your data plane |
| **MCP server for the mesh** (§3.3) | Read-only MCP over snapshot/CRDs | **Act** MCP (apply fix, change policy) behind SSO/RBAC/**audit** | Textbook free-read / paid-governed-act; rides the agentic-ops wave |
| **Intent → CRDs** (§3.5) | Pure dry-run/impact planner | LLM compiling NL → `MeshPeer`/`MeshNetworkPolicy` + impact analysis | CRDs are already the declarative target; planner is pure free logic |
| **Auto-segmentation** | Flow observation + manual policy recommender | ML/LLM that synthesizes & *maintains* `MeshNetworkPolicy` as traffic drifts | Nobody writes cross-cluster policy by hand — the feature they actually want |
| **Predictive health / anomaly** | Throughput/handshake/flow metrics already emitted (`pkg/metrics`) | Hosted baseline + "this peer will saturate / is flapping before it pages you" | Classic free-telemetry → paid-intelligence |

> **Lead with diagnosis (§3.4) + read-only MCP (§3.3).** Diagnosis attacks the
> sharpest pain with a real data moat; the MCP server is cheap, on-brand, and has
> the cleanest seam.

---

## 6. Risks / anti-patterns (hold the line)

- **Don't AI-wash.** Must consume the cross-cluster graph, or it's clonable.
- **Keep the OSS binary model-free.** Free hook = data contract, not inference.
  Protects "tiny and boring" + the trust story.
- **Grounded + verifiable.** Diagnosis cites its signals; actions go through
  dry-run + the governed/audited seam; never silent auto-apply.
- **Data sensitivity is a product.** Offer BYO-LLM / on-prem inference as a paid
  compliance tier; be explicit about data handling in the hosted plane.
- **Don't paywall a shipped free feature** (`COMMITMENT.md`). New AI/intelligence
  is *additive*; the mesh must always run fully without it.

---

## 7. Concrete repo artifacts to produce next

When scheduling work, spin these out (matching the existing conventions):

1. `docs/design/0005-mesh-snapshot.md` — the `MeshSnapshot` schema (versioned),
   `pkg/verify` assembly, `dwxctl` surface, stability guarantee.
2. `docs/design/0006-zero-friction-join.md` — bundle/handshake format, pure
   planner, `dwxctl join`, e2e adoption.
3. `docs/design/0007-mesh-mcp-server.md` — read-only tool set, free/act split,
   `cmd/dwx-mcp`.
4. A **ROADMAP.md "M5 — Intelligence & adoption"** section, written in the same
   free-hook/paid-seam table style as the existing free/paid line, referencing
   this brief.
5. Premium-side (private repo) design for the hosted diagnosis agent and the act
   MCP — consuming the contracts above over `EnterpriseControlPlaneClient`.

---

## 8. Map of existing seams you will plug into (grounding)

So a future session can act without re-deriving the architecture:

- **Open/closed seam:** `pkg/client/controlplane.go` — `ControlPlaneClient`
  (`LocalGitOpsClient` free, `EnterpriseControlPlaneClient` premium). Selected
  once at startup in `cmd/manager/main.go` by `DataWerx_SAAS_ENDPOINT`.
- **Premium operator seam:** `pkg/agent.Options.RegisterPremium` — the premium
  operator lives in a separate private repo and injects commercial operator-side
  components here; the open-core build references none of them (coordinator =
  RFC 8628 device auth; nonat = no-NAT return routing).
- **Premium topology sync:** `pkg/syncer` mirrors remote topology into the same
  `MeshPeer` CRDs; conflict detection via pure `topology.DetectTopologyConflicts`.
- **CRDs (the tier-agnostic integration points):**
  `networking.datawerx.io` → `MeshPeer`, `EndpointExport`, `MeshNetworkPolicy`;
  `multicluster.x-k8s.io` → `ServiceExport`, `ServiceImport` (MCS, KEP-1645).
- **Pure logic (where new "compute desired state" goes):** `pkg/topology`
  (`PlanPeer`, `VirtualCIDR`, `PlanRemap`, `DetectTopologyConflicts`), `pkg/dns`
  (MCS aggregation, broker-less ClusterSetIP allocation), `pkg/nat` planners,
  `pkg/meshfw` (firewall compiler), `pkg/verify` (health/report — **snapshot
  home**).
- **Data planes (interchangeable behind `controllers.PeerDataPlane`):** `pkg/wg`
  (own WireGuard device) and `pkg/routed` (host routes over an existing overlay),
  chosen by `DataWerx_DATAPLANE`.
- **Operator CLI:** `cmd/dwxctl` (currently `verify`, backed by pure
  `pkg/verify`) — the natural home for `snapshot`, `join`, `policy --dry-run`.
- **Observability:** `pkg/metrics` (cache-backed state collector + DNS/NAT event
  metrics on the controller-runtime registry) + Grafana starter dashboard.
- **Testing layers:** unit (hermetic, every push), `integration` (envtest),
  `e2e` (two kind clusters), `dataplane` (netns + root). Keep new pure logic in
  the unit tier; gate anything needing a cluster/kernel behind the right tag.

---

*Provenance: distilled from a strategy session on the DataWerx Mesh open-core
model. Treat §1–§2 as the durable thesis and guardrails; §3 as the execution
order; §4–§5 as the complete catalogs; §7 as the next deliverables.*
