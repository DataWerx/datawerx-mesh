package signal

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestBedrockClientAnswer(t *testing.T) {
	const rootCauseJSON = `{"problem":"p","cause":"c","impact":"i","confidence":0.9,"recommendedActions":["x"],"citations":["sig"]}`
	var gotAuth, gotAmzDate, gotPath string
	var gotBody map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotAmzDate = r.Header.Get("X-Amz-Date")
		gotPath = r.URL.EscapedPath()
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"stop_reason": "end_turn",
			"content":     []map[string]string{{"type": "text", "text": rootCauseJSON}},
		})
	}))
	defer srv.Close()

	c := &BedrockClient{
		Region:  "us-east-1",
		Model:   "anthropic.claude-3-5-sonnet-20240620-v1:0",
		Creds:   AWSCredentials{AccessKeyID: "AKID", SecretAccessKey: "SECRET"},
		BaseURL: srv.URL,
		HTTP:    srv.Client(),
		Now:     func() time.Time { return time.Unix(1_700_000_000, 0) },
	}
	rc, err := c.Answer(context.Background(), "why?", Evidence{})
	if err != nil {
		t.Fatalf("Answer: %v", err)
	}
	if rc.Problem != "p" || rc.Confidence != 0.9 {
		t.Fatalf("unexpected RootCause: %+v", rc)
	}
	if !strings.HasPrefix(gotAuth, "AWS4-HMAC-SHA256 Credential=AKID/") ||
		!strings.Contains(gotAuth, "/us-east-1/bedrock/aws4_request") ||
		!strings.Contains(gotAuth, "Signature=") {
		t.Errorf("Authorization is not a bedrock SigV4 signature: %q", gotAuth)
	}
	if gotAmzDate == "" {
		t.Error("missing X-Amz-Date header")
	}
	if !strings.Contains(gotPath, "/model/") || !strings.HasSuffix(gotPath, "/invoke") {
		t.Errorf("unexpected request path: %s", gotPath)
	}
	// The model id appears in the path. Its colon may be percent-encoded or not
	// depending on URL normalization; either form is accepted.
	if !strings.Contains(gotPath, "v1:0") && !strings.Contains(gotPath, "v1%3A0") {
		t.Errorf("model id missing from request path: %s", gotPath)
	}
	// On Bedrock the body carries anthropic_version and no model field.
	if gotBody["anthropic_version"] != bedrockAnthropicVersion {
		t.Errorf("body anthropic_version = %v, want %s", gotBody["anthropic_version"], bedrockAnthropicVersion)
	}
	if _, hasModel := gotBody["model"]; hasModel {
		t.Errorf("body should not carry a model field on Bedrock, got %v", gotBody["model"])
	}
}

func TestNewBedrockClientValidation(t *testing.T) {
	t.Setenv("AWS_REGION", "")
	t.Setenv("AWS_DEFAULT_REGION", "")
	t.Setenv("AWS_ACCESS_KEY_ID", "AKID")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "SECRET")

	if _, err := NewBedrockClient("", "model"); err == nil {
		t.Error("missing region should error")
	}
	if _, err := NewBedrockClient("us-east-1", ""); err == nil {
		t.Error("missing model should error")
	}
	t.Setenv("AWS_ACCESS_KEY_ID", "")
	if _, err := NewBedrockClient("us-east-1", "m"); err == nil {
		t.Error("missing credentials should error")
	}
}
