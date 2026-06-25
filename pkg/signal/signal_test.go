package signal

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/DataWerx/datawerx-mesh/pkg/slo"
	"github.com/DataWerx/datawerx-mesh/pkg/verify"
)

// faultySnapshot is a small mesh with a clear, multi-signal fault: a peer the
// agent refused for a CIDR overlap (which also shows up as a topology conflict),
// and a healthy-but-quiet second peer. It exercises diagnosis, reachability, and
// the SLO reconciliation together.
func faultySnapshot() verify.Snapshot {
	const now = 1_000_000
	return verify.BuildSnapshot(verify.SnapshotInputs{
		Now:          now,
		RequiredCRDs: []string{"meshpeers.networking.datawerx.io"},
		PresentCRDs:  map[string]bool{"meshpeers.networking.datawerx.io": true},
		AgentFound:   true,
		AgentDesired: 3,
		AgentReady:   3,
		Peers: []verify.PeerSnapshot{
			{
				ClusterID:     "payments",
				Phase:         "Error",
				Message:       "remote CIDR 10.244.0.0/16 overlaps local range",
				PodCIDRs:      []string{"10.244.0.0/16"},
				LastHandshake: 0,
			},
			{
				ClusterID:     "inventory",
				Phase:         "Connected",
				PodCIDRs:      []string{"10.10.0.0/16"},
				LastHandshake: now - 30, // fresh
			},
		},
		Conflicts: []verify.ConflictReport{
			{ClusterID: "payments", Reason: "CIDR 10.244.0.0/16 overlaps local range"},
		},
	})
}

func TestBuildEvidenceComposesAllAnalyzers(t *testing.T) {
	ev := BuildEvidence(faultySnapshot())

	if len(ev.Peers) != 2 {
		t.Fatalf("expected 2 peers, got %d", len(ev.Peers))
	}
	if len(ev.Diagnosis) == 0 {
		t.Fatal("expected diagnosis findings for the Error peer / conflict")
	}
	if len(ev.Reachability) != 2 {
		t.Fatalf("expected reachability for 2 clusters, got %d", len(ev.Reachability))
	}
	if len(ev.Connectivity) != 2 {
		t.Fatalf("expected connectivity for 2 clusters, got %d", len(ev.Connectivity))
	}

	// The fault must surface as a critical, grounded finding that names payments.
	var sawCritical bool
	for _, f := range ev.Diagnosis {
		if f.Severity == verify.SeverityCritical && strings.Contains(f.Signal, "payments") {
			if f.Signal == "" {
				t.Error("critical finding has empty grounding signal")
			}
			sawCritical = true
		}
	}
	if !sawCritical {
		t.Errorf("expected a grounded critical finding for payments; got %+v", ev.Diagnosis)
	}

	// The healthy peer should reconcile to a real verdict (not Impaired).
	for _, s := range ev.Connectivity {
		if s.Cluster == "inventory" && s.Verdict == slo.VerdictImpaired {
			t.Errorf("fresh connected peer should not be Impaired: %+v", s)
		}
	}
}

func TestEvidenceJSONIsDeterministicAndComplete(t *testing.T) {
	ev := BuildEvidence(faultySnapshot())
	b, err := ev.JSON()
	if err != nil {
		t.Fatalf("JSON: %v", err)
	}
	// Round-trips and carries the curated keys the model is told to rely on.
	var round map[string]json.RawMessage
	if err := json.Unmarshal(b, &round); err != nil {
		t.Fatalf("evidence JSON does not round-trip: %v", err)
	}
	for _, key := range []string{"health", "peers", "diagnosis", "reachability", "connectivity"} {
		if _, ok := round[key]; !ok {
			t.Errorf("evidence JSON missing %q key", key)
		}
	}
}

func TestResponseSchemaIsStrict(t *testing.T) {
	schema := ResponseSchema()
	if schema["additionalProperties"] != false {
		t.Error("schema must set additionalProperties:false for strict structured output")
	}
	required, ok := schema["required"].([]string)
	if !ok {
		t.Fatalf("required must be []string, got %T", schema["required"])
	}
	want := map[string]bool{
		"problem": true, "cause": true, "impact": true,
		"confidence": true, "recommendedActions": true, "citations": true,
	}
	for _, r := range required {
		delete(want, r)
	}
	if len(want) != 0 {
		t.Errorf("schema missing required fields: %v", want)
	}
	// Must serialize cleanly (it is embedded in the request body).
	if _, err := json.Marshal(schema); err != nil {
		t.Fatalf("schema does not marshal: %v", err)
	}
}

func TestBuildRequestEmbedsPromptSchemaAndEvidence(t *testing.T) {
	ev := BuildEvidence(faultySnapshot())
	req, err := buildRequest(DefaultModel, DefaultMaxTokens, "Why can't payments reach inventory?", ev)
	if err != nil {
		t.Fatalf("buildRequest: %v", err)
	}
	if req.Model != DefaultModel {
		t.Errorf("model = %q, want %q", req.Model, DefaultModel)
	}
	if req.System != SystemPrompt {
		t.Error("system prompt not attached")
	}
	if req.Thinking == nil || req.Thinking.Type != "adaptive" {
		t.Error("expected adaptive thinking")
	}
	if req.OutputConfig.Format.Type != "json_schema" {
		t.Errorf("output format type = %q, want json_schema", req.OutputConfig.Format.Type)
	}
	if len(req.Messages) != 1 || req.Messages[0].Role != "user" {
		t.Fatalf("expected one user message, got %+v", req.Messages)
	}
	content := req.Messages[0].Content
	if !strings.Contains(content, "Why can't payments reach inventory?") {
		t.Error("question not in user content")
	}
	if !strings.Contains(content, "payments") || !strings.Contains(content, "EVIDENCE") {
		t.Error("evidence not embedded in user content")
	}
	// The whole request must marshal (it is what we POST).
	if _, err := json.Marshal(req); err != nil {
		t.Fatalf("request does not marshal: %v", err)
	}
}

func TestParseRootCause(t *testing.T) {
	// A thinking block precedes the structured text block, as on Opus-tier models.
	body := []byte(`{
		"model": "claude-opus-4-8",
		"stop_reason": "end_turn",
		"content": [
			{"type": "thinking", "thinking": ""},
			{"type": "text", "text": "{\"problem\":\"payments cannot reach inventory\",\"cause\":\"CIDR overlap refused the payments peer\",\"impact\":\"all payments->inventory traffic\",\"confidence\":0.92,\"recommendedActions\":[\"renumber payments\"],\"citations\":[\"conflict[payments]: overlap\"]}"}
		]
	}`)
	rc, err := parseRootCause(body)
	if err != nil {
		t.Fatalf("parseRootCause: %v", err)
	}
	if rc.Confidence != 0.92 {
		t.Errorf("confidence = %v, want 0.92", rc.Confidence)
	}
	if len(rc.Citations) != 1 || !strings.Contains(rc.Citations[0], "payments") {
		t.Errorf("citations not parsed: %+v", rc.Citations)
	}
	if len(rc.RecommendedActions) != 1 {
		t.Errorf("recommendedActions not parsed: %+v", rc.RecommendedActions)
	}
}

func TestParseRootCauseRefusal(t *testing.T) {
	body := []byte(`{
		"stop_reason": "refusal",
		"stop_details": {"category": "cyber", "explanation": "declined"},
		"content": []
	}`)
	if _, err := parseRootCause(body); err == nil {
		t.Fatal("expected an error on a refusal stop reason")
	} else if !strings.Contains(err.Error(), "refus") {
		t.Errorf("error should mention refusal, got %v", err)
	}
}

func TestParseRootCauseNoTextBlock(t *testing.T) {
	body := []byte(`{"stop_reason":"end_turn","content":[{"type":"thinking","thinking":""}]}`)
	if _, err := parseRootCause(body); err == nil {
		t.Fatal("expected an error when no text block is present")
	}
}

// roundTripFunc adapts a function to http.RoundTripper for a network-free Client.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestClientAnswerHappyPath(t *testing.T) {
	var capturedKey, capturedVersion, capturedBody string
	fake := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		capturedKey = r.Header.Get("x-api-key")
		capturedVersion = r.Header.Get("anthropic-version")
		b, _ := io.ReadAll(r.Body)
		capturedBody = string(b)
		resp := `{"model":"claude-opus-4-8","stop_reason":"end_turn","content":[{"type":"text","text":"{\"problem\":\"p\",\"cause\":\"c\",\"impact\":\"i\",\"confidence\":0.9,\"recommendedActions\":[\"a\"],\"citations\":[\"s\"]}"}]}`
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(resp)),
			Header:     make(http.Header),
		}, nil
	})}

	c := NewClient("test-key")
	c.HTTP = fake

	rc, err := c.Answer(context.Background(), "what is wrong?", BuildEvidence(faultySnapshot()))
	if err != nil {
		t.Fatalf("Answer: %v", err)
	}
	if rc.Cause != "c" || rc.Confidence != 0.9 {
		t.Errorf("unexpected root cause: %+v", rc)
	}
	if capturedKey != "test-key" {
		t.Errorf("x-api-key header = %q", capturedKey)
	}
	if capturedVersion != anthropicVersion {
		t.Errorf("anthropic-version header = %q", capturedVersion)
	}
	if !strings.Contains(capturedBody, "EVIDENCE") {
		t.Error("request body did not carry the evidence")
	}
}

func TestClientAnswerRequiresAPIKey(t *testing.T) {
	c := NewClient("")
	if _, err := c.Answer(context.Background(), "q", Evidence{}); err == nil {
		t.Fatal("expected an error with no API key")
	}
}

func TestClientAnswerSurfacesAPIError(t *testing.T) {
	fake := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusTooManyRequests,
			Status:     "429 Too Many Requests",
			Body:       io.NopCloser(strings.NewReader(`{"error":{"type":"rate_limit_error"}}`)),
			Header:     make(http.Header),
		}, nil
	})}
	c := NewClient("k")
	c.HTTP = fake
	_, err := c.Answer(context.Background(), "q", Evidence{})
	if err == nil || !strings.Contains(err.Error(), "429") {
		t.Fatalf("expected a 429 error, got %v", err)
	}
}
