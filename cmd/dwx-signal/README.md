# dwx signal — grounded Q&A over a DataWerx mesh

`dwx signal` is the open-core core of **DataWerx Signal**.  It answers
natural-language questions about a mesh and returns a structured, grounded
root-cause result.

## Grounded AI, not a chatbot

The model never touches the cluster and never reasons from training priors. It
reasons over **only** the evidence this tool assembles whici is the same 
deterministic read surfaces `dwx` and `dwx mcp` already serve:

- the versioned `verify.Snapshot` (health, peers, exports/imports, policies, events)
- the rule-based `verify.Diagnose` findings (each carrying the `signal` it is grounded in)
- the expected `reach` reachability matrix (composes the real firewall compiler)
- the golden-signal `slo` connectivity report (expected reach vs. observed liveness)

Every claim in the answer must cite the evidence signal it came from. The model
selects, ranks, and explains; the facts come from the deterministic engines, so
an answer can never drift from what `snapshot`/`diagnose`/`reach`/`slo` report.

It is **read-only**, exactly like `dwx mcp`. Acting on the mesh stays the
governed, audited, premium surface.

## Usage

```sh
# From a snapshot file (dwx mesh snapshot / dwx mcp output) — no cluster needed.
dwx signal --snapshot snap.json "Why can't payments reach inventory?"

# Live cluster (uses the ambient kubeconfig / in-cluster service account).
dwx signal "Which clusters are unhealthy?"

# Inspect the exact grounded evidence the model would receive — no API key.
dwx signal --print-context --snapshot snap.json "anything"

# JSON output, for piping into the AI ops layer.
dwx signal --json --snapshot snap.json "Explain the connectivity problem"
```

The question may also be piped on stdin. The model is reached over the standard
Messages API with `ANTHROPIC_API_KEY`; `--print-context` needs no key.

## Output

```
Problem:    ...
Cause:      ...
Impact:     ...
Confidence: 0.92

Recommended actions:
  1. ...

Grounded in:
  - peer[payments].phase=Error: ...
  - conflict[payments]: ...
```

## Design

- Pure grounding logic (evidence assembly, prompt, schema, request/response
  shaping) lives in `pkg/signal` and is exhaustively unit-tested with no
  cluster and no network.
- The single impure edge — the HTTPS call to the model — is `pkg/signal.Client`,
  behind an injectable transport, using only the standard library (no new module
  dependency on the open core).
