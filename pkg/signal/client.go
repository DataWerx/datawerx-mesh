package signal

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// anthropicVersion is the Messages API version header value.
const anthropicVersion = "2023-06-01"

// defaultBaseURL is the Anthropic API root. Overridable on the Client for tests
// and for proxy/gateway deployments.
const defaultBaseURL = "https://api.anthropic.com"

// HTTPDoer is the minimal HTTP surface the Client needs. *http.Client satisfies
// it; tests inject a fake so the request-building and parsing paths run with no
// network.
type HTTPDoer interface {
	Do(*http.Request) (*http.Response, error)
}

// Client answers grounded questions by sending assembled Evidence to Claude and
// decoding the structured RootCause. It is the only impure piece of the package;
// everything it depends on (buildRequest, parseRootCause) is pure and tested
// independently.
type Client struct {
	APIKey    string
	Model     string
	MaxTokens int
	BaseURL   string
	HTTP      HTTPDoer
}

// NewClient returns a Client with product defaults: the Opus-tier model, the
// default token budget, the real Anthropic endpoint, and a default HTTP client.
// Only the API key is required.
func NewClient(apiKey string) *Client {
	return &Client{
		APIKey:    apiKey,
		Model:     DefaultModel,
		MaxTokens: DefaultMaxTokens,
		BaseURL:   defaultBaseURL,
		HTTP:      http.DefaultClient,
	}
}

// Answer sends the question and grounded evidence to the model and returns the
// structured root-cause result. The model is constrained to the RootCause schema
// and instructed to cite the evidence signals behind every claim.
func (c *Client) Answer(ctx context.Context, question string, ev Evidence) (RootCause, error) {
	if c.APIKey == "" {
		return RootCause{}, fmt.Errorf("no API key set (export ANTHROPIC_API_KEY)")
	}
	model := c.Model
	if model == "" {
		model = DefaultModel
	}
	maxTokens := c.MaxTokens
	if maxTokens <= 0 {
		maxTokens = DefaultMaxTokens
	}
	base := c.BaseURL
	if base == "" {
		base = defaultBaseURL
	}
	doer := c.HTTP
	if doer == nil {
		doer = http.DefaultClient
	}

	reqBody, err := buildRequest(model, maxTokens, question, ev)
	if err != nil {
		return RootCause{}, err
	}
	payload, err := json.Marshal(reqBody)
	if err != nil {
		return RootCause{}, fmt.Errorf("encode request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/v1/messages", bytes.NewReader(payload))
	if err != nil {
		return RootCause{}, fmt.Errorf("build HTTP request: %w", err)
	}
	httpReq.Header.Set("content-type", "application/json")
	httpReq.Header.Set("x-api-key", c.APIKey)
	httpReq.Header.Set("anthropic-version", anthropicVersion)

	resp, err := doer.Do(httpReq)
	if err != nil {
		return RootCause{}, fmt.Errorf("call model: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return RootCause{}, fmt.Errorf("read model response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return RootCause{}, fmt.Errorf("model API returned %s: %s", resp.Status, truncate(string(body), 512))
	}
	return parseRootCause(body)
}

// truncate bounds an error excerpt so a large error body does not flood the
// terminal.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
