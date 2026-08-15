package ai

import (
	"context"
	"strings"

	"github.com/lohi-ai/agentray/agentcore"
)

// Model is one entry from a provider's live list-models response. IDs are
// whatever the vendor returned — never a hardcoded catalog.
type Model struct {
	ProviderID     string `json:"provider_id"`
	ProviderVendor string `json:"provider_vendor"`
	ProviderName   string `json:"provider_name"`
	ID             string `json:"id"`
	// ContextWindow is the model's input window in tokens: the vendor's own
	// figure when its list-models response carried one, otherwise this package's
	// fallback, otherwise 0 for "unknown". It is what the compaction budget is
	// capped against, so 0 must stay distinguishable from a real number.
	ContextWindow int `json:"context_window,omitempty"`
}

// Provider is the runtime unit: identity, auth, live model list, Chat/Stream.
type Provider interface {
	agentcore.LLMProvider
	ID() string
	Vendor() string
	DisplayName() string
	BaseURL() string
	APIKey() string
	ListModels(ctx context.Context) ([]Model, error)
}

// Spec constructs a provider. Vendor is openai | anthropic | google | gemini
// or any OpenAI-compatible name (which requires BaseURL). ID is the caller's
// stable handle (a workspace provider row id); empty ID falls back to Vendor.
type Spec struct {
	ID      string
	Vendor  string
	Name    string
	APIKey  string
	BaseURL string
	HTTP    HTTPDoer
}

// NormalizeVendor maps aliases onto the built-in vendor ids.
func NormalizeVendor(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "", "openai":
		return "openai"
	case "anthropic":
		return "anthropic"
	case "google", "gemini":
		return "google"
	case "openai-compat", "openai-compatible", "compat":
		return "openai-compat"
	default:
		return strings.ToLower(strings.TrimSpace(v))
	}
}
