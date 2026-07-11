package agentcore

import (
	"fmt"
	"strings"
)

// ProviderSpec is the resolved configuration for constructing a provider:
// vendor name, decrypted key, and an optional base-URL override.
type ProviderSpec struct {
	Name    string // "openai" | "anthropic" | any OpenAI-compatible vendor
	APIKey  string
	BaseURL string
	Compat  Compat // optional; zero value falls back to the vendor default
}

// NewProvider resolves a ProviderSpec into an LLMProvider. Adding a vendor is
// additive here — a new case (or, for OpenAI-compatible vendors, just a compat
// entry + base_url) — and never requires touching agent.go (§12 AC).
func NewProvider(spec ProviderSpec) (LLMProvider, error) {
	switch strings.ToLower(strings.TrimSpace(spec.Name)) {
	case "", "openai":
		compat := spec.Compat
		if compat.MaxTokensField == "" {
			compat = DefaultCompat()
		}
		return NewOpenAIProvider(spec.APIKey, spec.BaseURL, compat), nil
	case "anthropic":
		return NewAnthropicProvider(spec.APIKey, spec.BaseURL), nil
	case "google", "gemini":
		// Gemini on Google's OpenAI-compatible surface. An explicit BaseURL
		// overrides the default endpoint (e.g. a regional proxy).
		p := NewGeminiProvider(spec.APIKey)
		if b := strings.TrimSpace(spec.BaseURL); b != "" {
			p.BaseURL = strings.TrimRight(b, "/")
		}
		return p, nil
	default:
		// OpenAI-compatible vendors are config, not code: route them through the
		// OpenAI provider with a caller-supplied base_url + compat. The provider
		// keeps the vendor's identity so traces and per-turn key refresh attribute
		// to the vendor's tier, not "openai".
		if spec.Compat.MaxTokensField != "" && strings.TrimSpace(spec.BaseURL) != "" {
			p := NewOpenAIProvider(spec.APIKey, spec.BaseURL, spec.Compat)
			p.Vendor = strings.ToLower(strings.TrimSpace(spec.Name))
			return p, nil
		}
		return nil, fmt.Errorf("agentcore: unknown provider %q", spec.Name)
	}
}
