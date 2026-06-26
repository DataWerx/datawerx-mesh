// Package signal turns DataWerx's deterministic mesh read surfaces into a
// grounded, natural-language question-answering layer — the open-core core of
// "DataWerx Signal".
//
// The design principle is grounded AI: the model never reasons about the mesh
// from raw cluster access or from training priors. It reasons only over the
// EVIDENCE this package assembles — the same versioned snapshot, rule-based
// diagnosis, expected-reachability matrix, and golden-signal SLO report that
// `dwx` and `dwx mcp` already serve. Every field those engines produce is
// itself grounded in a concrete observation (a peer phase, a handshake age, a
// conflict reason, the compiled firewall), so an answer can be required to cite
// the signal it came from. The model selects, ranks, and explains; it does not
// invent.
//
// The split mirrors the rest of the repo: everything here is pure and
// exhaustively unit-testable with no cluster and no network. Evidence assembly,
// the system prompt, the response schema, request building, and response
// parsing are all functions of their inputs. The single impure edge — the HTTPS
// call to the model — lives in client.go behind an injectable transport.
//
// Read-only by construction. Signal answers questions; it never mutates the
// mesh. Acting on the mesh (create-export, failover-policy, intent-based
// networking) stays the governed, audited, premium surface, exactly as the
// open-core read-only/action seam requires.
package signal

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/DataWerx/datawerx-mesh/pkg/reach"
	"github.com/DataWerx/datawerx-mesh/pkg/slo"
	"github.com/DataWerx/datawerx-mesh/pkg/verify"
)

const (
	// DefaultModel is the Claude model Signal reasons with. Opus-tier: the
	// grounding/selection task is correctness-sensitive, not latency-sensitive.
	DefaultModel = "claude-opus-4-8"

	// DefaultMaxTokens bounds the response. The structured root-cause output is
	// small; the headroom covers adaptive thinking.
	DefaultMaxTokens = 8192
)

// Evidence is the grounded fact base Signal answers from. It is a flattened,
// curated projection of one mesh snapshot plus the three deterministic analyses
// derived from it. The model is instructed that this is the ONLY set of facts it
// may use, so the surface here is the boundary of what an answer can claim.
type Evidence struct {
	// GeneratedAt is the snapshot's assembly time (Unix seconds), so the model
	// can reason about staleness and so an answer can be situated in time.
	GeneratedAt int64 `json:"generatedAt,omitempty"`

	// Health is the same pass/warn/fail report `dwx mesh verify` renders.
	Health verify.Report `json:"health"`

	Peers     []verify.PeerSnapshot   `json:"peers"`
	Conflicts []verify.ConflictReport `json:"conflicts,omitempty"`
	Exports   []verify.ExportSnapshot `json:"exports,omitempty"`
	Imports   []verify.ImportSnapshot `json:"imports,omitempty"`
	Policies  []verify.PolicySnapshot `json:"policies,omitempty"`
	Events    []verify.EventSnapshot  `json:"events,omitempty"`

	// Diagnosis is the rule-based root-cause finding list (verify.Diagnose),
	// most-severe first. Each finding carries the Signal it is grounded in — the
	// citations an answer should reuse.
	Diagnosis []verify.Finding `json:"diagnosis"`

	// Reachability is the expected cross-cluster reach matrix (pkg/reach): for
	// each remote cluster, whether and why it can reach into this one.
	Reachability []reach.Reachability `json:"reachability"`

	// Connectivity is the golden-signal SLO report (pkg/slo): expected reach
	// reconciled against observed tunnel liveness. Impaired is the key fault.
	Connectivity []slo.Signal `json:"connectivity"`
}

// BuildEvidence assembles the grounded fact base from one snapshot. It composes
// the same pure analyzers every other read surface uses, so Signal's evidence
// can never disagree with `dwx mesh snapshot/diagnose/reach/slo` or `dwx mcp`.
func BuildEvidence(snap verify.Snapshot) Evidence {
	return Evidence{
		GeneratedAt:  snap.GeneratedAt,
		Health:       snap.Health,
		Peers:        snap.Peers,
		Conflicts:    snap.Conflicts,
		Exports:      snap.Exports,
		Imports:      snap.Imports,
		Policies:     snap.Policies,
		Events:       snap.Events,
		Diagnosis:    verify.Diagnose(snap),
		Reachability: reach.FromSnapshot(snap).Reachabilities,
		Connectivity: slo.FromSnapshot(snap).Signals,
	}
}

// JSON renders the evidence as indented, stable JSON — what the model receives
// verbatim, and what `--print-context` shows for inspection without a model call.
func (e Evidence) JSON() ([]byte, error) {
	return json.MarshalIndent(e, "", "  ")
}

// RootCause is the structured answer Signal returns. The shape is the product
// contract for the AI ops layer: a problem statement, a grounded cause, the
// blast radius, a calibrated confidence, concrete next steps, and — the part
// that keeps it honest — the evidence signals every claim traces back to.
type RootCause struct {
	// Problem restates what was asked or what is wrong, in one or two sentences.
	Problem string `json:"problem"`
	// Cause is the most likely root cause, grounded in the evidence. When the
	// evidence is insufficient, it says so rather than guessing.
	Cause string `json:"cause"`
	// Impact is the blast radius: which clusters, services, or flows are affected.
	Impact string `json:"impact"`
	// Confidence is the model's calibrated confidence in [0,1]. Low confidence is
	// the correct output when the evidence does not determine an answer.
	Confidence float64 `json:"confidence"`
	// RecommendedActions are concrete, ordered next steps. For a read-only answer
	// these are operator actions (or "gather more data"), never silent mutations.
	RecommendedActions []string `json:"recommendedActions"`
	// Citations are the exact evidence signals (diagnosis Signal strings, reach
	// reasons, SLO reasons, peer/conflict fields) each claim is grounded in. An
	// empty list means the answer is ungrounded and should be distrusted.
	Citations []string `json:"citations"`
}

// SystemPrompt is the instruction that makes Signal a grounded analyst rather
// than a free-form chatbot. It is deliberately strict about the evidence
// boundary and the citation requirement — that is the whole value proposition.
const SystemPrompt = `You are DataWerx Signal, an expert SRE for multi-cluster Kubernetes networking built on the DataWerx Mesh.

You answer questions about ONE cluster's mesh using ONLY the EVIDENCE provided in the user message. The evidence is the machine-generated, deterministic state of the mesh: a health report, peers (with phase, handshake/probe age, and status message), topology conflicts, service exports/imports, mesh network policies, recent warning events, a rule-based diagnosis (each finding carries a "signal" it is grounded in), an expected cross-cluster reachability matrix, and a golden-signal connectivity report.

Hard rules:
- Use ONLY facts present in the EVIDENCE. Never invent peers, clusters, service names, IP addresses, metrics, or events that are not there. Never rely on outside knowledge of the user's environment.
- Every claim in "problem", "cause", and "impact" MUST be traceable to specific evidence, and you MUST list those exact signals in "citations" (copy the diagnosis "signal" strings, reachability/connectivity "reason" text, or the concrete peer/conflict/export fields you used).
- Prefer the rule-based diagnosis and the connectivity report as your starting point — they already encode the obvious causes. An "Impaired" connectivity verdict means a cluster that SHOULD be reachable has a dead tunnel; that is usually the answer to "why can't X reach Y".
- If the evidence does not determine an answer, say so plainly in "cause", set "confidence" low, and make "recommendedActions" about gathering the missing signal (e.g. enable active probing, check the agent pod, inspect a specific peer's status message).
- You are read-only. "recommendedActions" are steps for a human operator (or a note that acting on the mesh is a governed operation), never claims that you changed anything.
- Calibrate "confidence" to how directly the evidence supports the cause: ~0.9 when a critical diagnosis finding names it outright, lower when you are inferring across signals, low when the evidence is silent.

Return only the structured result.`

// ResponseSchema is the JSON Schema the model's output is constrained to via the
// Messages API output_config.format. It pins the RootCause shape so the response
// always parses, with no prose preamble to strip.
func ResponseSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"problem": map[string]any{
				"type":        "string",
				"description": "What was asked or what is wrong, in one or two sentences.",
			},
			"cause": map[string]any{
				"type":        "string",
				"description": "The most likely root cause, grounded in the evidence; say so if the evidence is insufficient.",
			},
			"impact": map[string]any{
				"type":        "string",
				"description": "Blast radius: which clusters, services, or flows are affected.",
			},
			"confidence": map[string]any{
				"type":        "number",
				"description": "Calibrated confidence in the cause, from 0 to 1.",
			},
			"recommendedActions": map[string]any{
				"type":        "array",
				"items":       map[string]any{"type": "string"},
				"description": "Concrete, ordered next steps for a human operator.",
			},
			"citations": map[string]any{
				"type":        "array",
				"items":       map[string]any{"type": "string"},
				"description": "The exact evidence signals each claim is grounded in.",
			},
		},
		"required":             []string{"problem", "cause", "impact", "confidence", "recommendedActions", "citations"},
		"additionalProperties": false,
	}
}

// userContent renders the question plus the evidence into the single user-turn
// string the model sees. The evidence is fenced and labeled as the sole source
// of truth, reinforcing the system prompt at the point of use.
func userContent(question string, ev Evidence) (string, error) {
	evJSON, err := ev.JSON()
	if err != nil {
		return "", fmt.Errorf("render evidence: %w", err)
	}
	var b strings.Builder
	b.WriteString("Question:\n")
	b.WriteString(strings.TrimSpace(question))
	b.WriteString("\n\nEVIDENCE (the only facts you may use; do not go beyond it):\n")
	b.Write(evJSON)
	b.WriteString("\n")
	return b.String(), nil
}

// --- Anthropic Messages API wire types (stdlib JSON; no SDK dependency) ---
//
// Kept minimal and local so the open core gains no new module dependency and
// stays buildable in the wgctrl-restricted CI/sandbox. The shapes follow the
// public Messages API contract.

type apiRequest struct {
	Model        string          `json:"model"`
	MaxTokens    int             `json:"max_tokens"`
	System       string          `json:"system,omitempty"`
	Thinking     *thinkingConfig `json:"thinking,omitempty"`
	OutputConfig outputConfig    `json:"output_config"`
	Messages     []apiMessage    `json:"messages"`
}

type thinkingConfig struct {
	Type string `json:"type"`
}

type outputConfig struct {
	Effort string       `json:"effort,omitempty"`
	Format outputFormat `json:"format"`
}

type outputFormat struct {
	Type   string         `json:"type"`
	Schema map[string]any `json:"schema"`
}

type apiMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type apiResponse struct {
	Content     []apiContentBlock `json:"content"`
	StopReason  string            `json:"stop_reason"`
	StopDetails *apiStopDetails   `json:"stop_details"`
	Model       string            `json:"model"`
}

type apiContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type apiStopDetails struct {
	Category    string `json:"category"`
	Explanation string `json:"explanation"`
}

// buildRequest assembles the Messages API request body. Adaptive thinking is on
// (the grounding/selection task benefits from it) and the output is constrained
// to the RootCause schema with high effort.
func buildRequest(model string, maxTokens int, question string, ev Evidence) (apiRequest, error) {
	content, err := userContent(question, ev)
	if err != nil {
		return apiRequest{}, err
	}
	return apiRequest{
		Model:     model,
		MaxTokens: maxTokens,
		System:    SystemPrompt,
		Thinking:  &thinkingConfig{Type: "adaptive"},
		OutputConfig: outputConfig{
			Effort: "high",
			Format: outputFormat{Type: "json_schema", Schema: ResponseSchema()},
		},
		Messages: []apiMessage{{Role: "user", Content: content}},
	}, nil
}

// parseRootCause decodes a Messages API response body into a RootCause. It
// handles the safety-refusal stop reason explicitly and reads the first text
// block (any leading thinking block is skipped), which output_config.format
// guarantees is valid JSON for our schema.
func parseRootCause(body []byte) (RootCause, error) {
	var resp apiResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return RootCause{}, fmt.Errorf("decode model response: %w", err)
	}
	if resp.StopReason == "refusal" {
		detail := "the model declined to answer"
		if resp.StopDetails != nil && resp.StopDetails.Explanation != "" {
			detail = resp.StopDetails.Explanation
		}
		return RootCause{}, fmt.Errorf("model refused: %s", detail)
	}
	for _, block := range resp.Content {
		if block.Type != "text" {
			continue
		}
		var rc RootCause
		if err := json.Unmarshal([]byte(block.Text), &rc); err != nil {
			return RootCause{}, fmt.Errorf("decode structured answer: %w", err)
		}
		return rc, nil
	}
	return RootCause{}, fmt.Errorf("model response carried no text block (stop_reason=%q)", resp.StopReason)
}
