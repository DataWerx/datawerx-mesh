package signal

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// bedrockAnthropicVersion is the version string Bedrock expects in the request
// body for Anthropic models. The model id travels in the URL, not the body.
const bedrockAnthropicVersion = "bedrock-2023-05-31"

// BedrockClient reasons with an Anthropic model hosted on Amazon Bedrock. It
// speaks the same Messages request and response shapes as Client; only the
// endpoint (Bedrock InvokeModel) and the auth (SigV4) differ. SigV4 is
// hand-rolled, so the open core gains no AWS SDK dependency.
type BedrockClient struct {
	// Region is the AWS region, e.g. us-east-1.
	Region string
	// Model is the Bedrock model id or inference-profile id, e.g.
	// anthropic.claude-3-5-sonnet-20240620-v1:0.
	Model     string
	MaxTokens int
	Creds     AWSCredentials
	// BaseURL overrides the endpoint for tests. Empty uses the regional
	// bedrock-runtime host.
	BaseURL string
	HTTP    HTTPDoer
	// Now is the clock used for the SigV4 timestamp; injectable for tests.
	Now func() time.Time
}

// NewBedrockClient builds a client from a region and model id, taking SigV4
// credentials from the standard AWS environment variables. Region falls back to
// AWS_REGION / AWS_DEFAULT_REGION when empty.
func NewBedrockClient(region, model string) (*BedrockClient, error) {
	region = strings.TrimSpace(region)
	if region == "" {
		region = strings.TrimSpace(os.Getenv("AWS_REGION"))
	}
	if region == "" {
		region = strings.TrimSpace(os.Getenv("AWS_DEFAULT_REGION"))
	}
	if region == "" {
		return nil, fmt.Errorf("bedrock: no region (set --region or AWS_REGION)")
	}
	if strings.TrimSpace(model) == "" {
		return nil, fmt.Errorf("bedrock: no model id (set --model to a Bedrock model or inference-profile id)")
	}
	creds := AWSCredentials{
		AccessKeyID:     os.Getenv("AWS_ACCESS_KEY_ID"),
		SecretAccessKey: os.Getenv("AWS_SECRET_ACCESS_KEY"),
		SessionToken:    os.Getenv("AWS_SESSION_TOKEN"),
	}
	if creds.AccessKeyID == "" || creds.SecretAccessKey == "" {
		return nil, fmt.Errorf("bedrock: missing AWS credentials (set AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY)")
	}
	return &BedrockClient{
		Region:    region,
		Model:     model,
		MaxTokens: DefaultMaxTokens,
		Creds:     creds,
		HTTP:      http.DefaultClient,
		Now:       time.Now,
	}, nil
}

// Answer sends the question and grounded evidence to the Bedrock model and
// returns the structured root-cause result.
func (c *BedrockClient) Answer(ctx context.Context, question string, ev Evidence) (RootCause, error) {
	if c.Creds.AccessKeyID == "" || c.Creds.SecretAccessKey == "" {
		return RootCause{}, fmt.Errorf("bedrock: missing AWS credentials")
	}
	if strings.TrimSpace(c.Model) == "" {
		return RootCause{}, fmt.Errorf("bedrock: no model id")
	}
	maxTokens := c.MaxTokens
	if maxTokens <= 0 {
		maxTokens = DefaultMaxTokens
	}
	reqBody, err := buildRequest(c.Model, maxTokens, question, ev)
	if err != nil {
		return RootCause{}, err
	}
	// On Bedrock the model id is in the URL path, and the body carries the
	// Bedrock anthropic_version instead of a model field.
	reqBody.Model = ""
	reqBody.AnthropicVersion = bedrockAnthropicVersion
	payload, err := json.Marshal(reqBody)
	if err != nil {
		return RootCause{}, fmt.Errorf("encode request: %w", err)
	}

	base := c.BaseURL
	if base == "" {
		base = "https://bedrock-runtime." + c.Region + ".amazonaws.com"
	}
	endpoint := strings.TrimRight(base, "/") + "/model/" + url.PathEscape(c.Model) + "/invoke"

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return RootCause{}, fmt.Errorf("build HTTP request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")

	now := c.Now
	if now == nil {
		now = time.Now
	}
	signV4(httpReq, payload, c.Creds, c.Region, "bedrock", now())

	doer := c.HTTP
	if doer == nil {
		doer = http.DefaultClient
	}
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
		return RootCause{}, fmt.Errorf("bedrock returned %s: %s", resp.Status, truncate(string(body), 512))
	}
	return parseRootCause(body)
}
