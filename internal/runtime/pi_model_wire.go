package agentruntime

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/lohi-ai/agentray/agentcore"
	"github.com/lohi-ai/agentray/ai"
)

type piModelWire struct {
	api, provider, endpoint string
	compat                  map[string]any
}

// Resolve native-only APIs without constructing a legacy client for a different
// wire. Existing legacy providers still supply their established host defaults.
func (t ModelTier) resolvePiWire(nativeOAuthPool bool, streamOptions map[string]json.RawMessage) (piModelWire, error) {
	vendor := ai.NormalizeVendor(t.Provider)
	if vendor == ai.VendorPiMessages || vendor == "radius" {
		return piModelWire{api: ai.VendorPiMessages, provider: vendor, endpoint: t.BaseURL}, nil
	}
	if vendor == ai.VendorAzureResponses {
		return t.resolveAzurePiWire(streamOptions)
	}
	if nativeOAuthPool && ai.NormalizeOAuthVendor(t.Provider) == ai.VendorGoogleAntigravity {
		endpoint := strings.TrimSpace(t.BaseURL)
		defaults := endpoint == ""
		if defaults {
			endpoint = "https://daily-cloudcode-pa.googleapis.com"
		}
		return piModelWire{api: ai.VendorGoogleAntigravity, provider: ai.VendorGoogleAntigravity, endpoint: endpoint, compat: map[string]any{"antigravityDefaultEndpoints": defaults}}, nil
	}
	if nativeOAuthPool && ai.NormalizeOAuthVendor(t.Provider) == ai.VendorClaudeCode {
		endpoint := t.BaseURL
		if strings.TrimSpace(endpoint) == "" {
			endpoint = "https://api.anthropic.com"
		}
		return piModelWire{api: "anthropic-messages", provider: ai.VendorClaudeCode, endpoint: endpoint}, nil
	}
	if nativeOAuthPool && ai.NormalizeOAuthVendor(t.Provider) == ai.VendorDevin {
		endpoint := strings.TrimSpace(t.BaseURL)
		if endpoint == "" {
			endpoint = ai.DevinDefaultBaseURL
		}
		return piModelWire{api: "devin-agent", provider: ai.VendorDevin, endpoint: endpoint}, nil
	}
	var provider agentcore.LLMProvider
	var err error
	if nativeOAuthPool {
		if ai.NormalizeOAuthVendor(t.Provider) != ai.VendorOpenAICodex {
			// Only the vendors handled above have a native pooled stream; refusing
			// here beats silently building a Codex wire for an unknown vendor.
			return piModelWire{}, fmt.Errorf("native OAuth pool has no wire for vendor %q", t.Provider)
		}
		p := ai.NewCodexProvider()
		p.BaseURL = t.BaseURL
		provider = p
	} else {
		provider, err = t.RawProvider()
	}
	if err != nil {
		return piModelWire{}, err
	}
	var api, endpoint string
	var compat map[string]any
	switch p := provider.(type) {
	case *ai.CodexProvider:
		api, endpoint = "openai-codex-responses", p.BaseURL
		if endpoint == "" {
			endpoint = "https://chatgpt.com/backend-api"
		}
	case *ai.OpenAIProvider:
		api, endpoint = "openai-completions", p.BaseURL
		compat = map[string]any{"maxTokensField": p.Compat.MaxTokensField}
	case *ai.OpenAIResponsesProvider:
		api, endpoint = "openai-responses", strings.TrimSuffix(p.BaseURL, "/responses")
	case *ai.AnthropicProvider:
		api, endpoint = "anthropic-messages", p.BaseURL
	default:
		return piModelWire{}, fmt.Errorf("provider %q has no native Pi model binding", provider.Name())
	}
	return piModelWire{api: api, provider: provider.Name(), endpoint: endpoint, compat: compat}, nil
}

func (t ModelTier) resolveAzurePiWire(streamOptions map[string]json.RawMessage) (piModelWire, error) {
	env := map[string]json.RawMessage{}
	if raw := streamOptions["env"]; len(raw) > 0 && string(raw) != "null" {
		if err := json.Unmarshal(raw, &env); err != nil {
			return piModelWire{}, err
		}
	}
	if env == nil {
		env = map[string]json.RawMessage{}
	}
	// A resolved tier endpoint is authoritative. Provider environment discovery
	// supplies the endpoint only when this tier has none of its own.
	controls := map[string]any{"env": env}
	if strings.TrimSpace(t.BaseURL) != "" {
		controls["azureBaseUrl"] = t.BaseURL
	}
	rawControls, err := json.Marshal(controls)
	if err != nil {
		return piModelWire{}, err
	}
	rawModel, _ := json.Marshal(map[string]string{"id": t.Model, "baseUrl": t.BaseURL})
	config, err := ai.ResolveAzureResponsesConfig(rawModel, rawControls)
	if err != nil {
		return piModelWire{}, err
	}
	// Freeze endpoint/version/map at binding time. Requests and child/summary
	// bindings must not be redirected by later process-environment changes.
	mapping := ""
	_ = json.Unmarshal(env["AZURE_OPENAI_DEPLOYMENT_NAME_MAP"], &mapping)
	if mapping == "" {
		mapping = os.Getenv("AZURE_OPENAI_DEPLOYMENT_NAME_MAP")
	}
	if mapping == "" {
		// A truthy empty map disables later process fallback.
		mapping = " "
	}
	for key, value := range map[string]string{"AZURE_OPENAI_BASE_URL": config.BaseURL, "AZURE_OPENAI_API_VERSION": config.APIVersion, "AZURE_OPENAI_DEPLOYMENT_NAME_MAP": mapping} {
		env[key], _ = json.Marshal(value)
	}
	streamOptions["env"], err = json.Marshal(env)
	if err != nil {
		return piModelWire{}, err
	}
	return piModelWire{api: ai.VendorAzureResponses, provider: ai.VendorAzureResponses, endpoint: config.BaseURL}, nil
}
