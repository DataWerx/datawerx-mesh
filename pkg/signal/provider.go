package signal

import "context"

// Provider answers a grounded question with a structured root cause. Both the
// Anthropic-direct Client and the Bedrock client implement it, so a caller can
// target either backend without changing how it asks.
type Provider interface {
	Answer(ctx context.Context, question string, ev Evidence) (RootCause, error)
}

var (
	_ Provider = (*Client)(nil)
	_ Provider = (*BedrockClient)(nil)
)
