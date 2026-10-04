package agentruntime

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/lohi-ai/agentray/agentcore"
	"github.com/lohi-ai/agentray/agentcore/plugins/observe"
	"github.com/lohi-ai/agentray/ai"
)

func TestPiModelBindingPreservesResolvedWireAndEndpoint(t *testing.T) {
	clearNativeAzureEnv(t)
	for _, tc := range []struct {
		name, provider, base, api, identity, endpoint string
		caps                                          agentcore.ModelCapabilities
	}{
		{name: "openai", provider: "openai", base: "https://example.test/v1", api: "openai-completions", identity: "openai", endpoint: "https://example.test/v1"},
		{name: "model responses", provider: "openai", base: "https://example.test/v1", api: "openai-responses", identity: "openai", endpoint: "https://example.test/v1", caps: agentcore.ModelCapabilities{StatefulResponses: agentcore.CapabilitySupported}},
		{name: "explicit responses endpoint", provider: ai.VendorOpenAIResponses, base: "https://example.test/v1/responses", api: "openai-responses", identity: ai.VendorOpenAIResponses, endpoint: "https://example.test/v1"},
		{name: "azure responses", provider: ai.VendorAzureResponses, base: "https://resource.openai.azure.com", api: ai.VendorAzureResponses, identity: ai.VendorAzureResponses, endpoint: "https://resource.openai.azure.com/openai/v1"},
		{name: "anthropic", provider: "anthropic", base: "https://example.test/anthropic", api: "anthropic-messages", identity: "anthropic", endpoint: "https://example.test/anthropic"},
		{name: "google configured proxy", provider: "google", base: "https://example.test/gemini", api: "openai-completions", identity: "google", endpoint: "https://example.test/gemini"},
		{name: "compatible", provider: "private-router", base: "https://example.test/router", api: "openai-completions", identity: "private-router", endpoint: "https://example.test/router"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tier := ModelTier{TierConfig: TierConfig{Provider: tc.provider, Model: "test-model", BaseURL: tc.base, APIKey: "keep-out-of-model", ContextWindow: 12345, Capabilities: tc.caps}}
			cfg, known, err := tier.BindPi(agentcore.PiConfig{}, PiModelOptions{MaxTokens: 234, Pricing: observe.Pricing{"test-model": {InputPerM: 2, OutputPerM: 8, CacheReadPerM: 0.25, CacheWritePerM: 3}}})
			if err != nil {
				t.Fatal(err)
			}
			var options struct {
				StreamMode   string
				InitialState struct {
					Model struct {
						API, Provider, BaseURL   string
						ContextWindow, MaxTokens int
						Cost                     map[string]float64
					}
				}
				StreamOptions struct{ MaxTokens int }
			}
			if err := json.Unmarshal(cfg.Options, &options); err != nil {
				t.Fatal(err)
			}
			model := options.InitialState.Model
			if !known || options.StreamMode != "native" || model.API != tc.api || model.Provider != tc.identity || model.BaseURL != tc.endpoint || model.ContextWindow != 12345 || model.MaxTokens != 234 || options.StreamOptions.MaxTokens != 234 || model.Cost["cacheWrite"] != 3 {
				t.Fatalf("wrong binding: %s", cfg.Options)
			}
			if strings.Contains(string(cfg.Options), "keep-out-of-model") {
				t.Fatal("credential persisted in native model options")
			}
			key, err := cfg.Callback(context.Background(), "getApiKey", piModelJSON(tc.identity), nil)
			if err != nil || string(key) != `"keep-out-of-model"` {
				t.Fatalf("credential callback: %s %v", key, err)
			}
		})
	}
}

func piModelJSON(value any) json.RawMessage {
	b, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return b
}

func TestPiModelBindingHonorsLimitsAndFailingCredentialRefresh(t *testing.T) {
	tier := ModelTier{TierConfig: TierConfig{Provider: "openai", Model: "unknown", APIKey: "stale-key", BaseURL: "https://example.test/v1", Capabilities: agentcore.ModelCapabilities{MaxOutputTokens: 100, Tools: agentcore.CapabilityUnsupported, ImageInput: agentcore.CapabilityUnsupported}}}
	var refreshes int
	cfg, known, err := tier.BindPi(agentcore.PiConfig{Options: json.RawMessage(`{"initialState":{"systemPrompt":"keep","tools":[{"name":"write"}]},"streamOptions":{"temperature":0.25}}`)}, PiModelOptions{MaxTokens: 500, RefreshKey: func(context.Context, string) (string, error) { refreshes++; return "", errors.New("key unavailable") }})
	if err != nil {
		t.Fatal(err)
	}
	if known || !strings.Contains(string(cfg.Options), `"maxTokens":100`) || !strings.Contains(string(cfg.Options), `"tools":[]`) || !strings.Contains(string(cfg.Options), `"input":["text"]`) || !strings.Contains(string(cfg.Options), `"temperature":0.25`) {
		t.Fatalf("lost host limits/options: %s", cfg.Options)
	}
	if _, err := cfg.Callback(context.Background(), "getApiKey", piModelJSON("another-provider"), nil); err == nil || refreshes != 0 {
		t.Fatal("unbound provider could resolve credentials")
	}
	if key, err := cfg.Callback(context.Background(), "getApiKey", piModelJSON("openai"), nil); err == nil || len(key) != 0 || refreshes != 1 {
		t.Fatal("refresh failure fell back to stale credential")
	}
	decision, err := cfg.Callback(context.Background(), "beforeToolCall", json.RawMessage(`{}`), nil)
	if err != nil || !strings.Contains(string(decision), `"block":true`) {
		t.Fatalf("tool-unsupported model allowed execution: %s %v", decision, err)
	}
}

func TestPiModelBindingFederationAdmissionAndRefreshAuthority(t *testing.T) {
	for _, key := range []string{"ANTHROPIC_FEDERATION_RULE_ID", "ANTHROPIC_ORGANIZATION_ID", "ANTHROPIC_IDENTITY_TOKEN_FILE", "ANTHROPIC_WORKSPACE_ID", "ANTHROPIC_SERVICE_ACCOUNT_ID"} {
		t.Setenv(key, "")
	}
	for _, tc := range []struct {
		name, provider, key, options, want string
		process                            bool
		refresh                            func(context.Context, string) (string, error)
	}{
		{name: "scoped federation", provider: "anthropic", options: `{"streamOptions":{"env":{"ANTHROPIC_FEDERATION_RULE_ID":"rule","ANTHROPIC_ORGANIZATION_ID":"org","ANTHROPIC_IDENTITY_TOKEN_FILE":"/not-read-during-admission"}}}`, want: "null"},
		{name: "process federation", provider: "anthropic", process: true, want: "null"},
		{name: "incomplete federation", provider: "anthropic", options: `{"streamOptions":{"env":{"ANTHROPIC_FEDERATION_RULE_ID":"rule"}}}`, want: "Pi provider credential is empty"},
		{name: "other provider", provider: "openai", process: true, want: "Pi provider credential is empty"},
		{name: "explicit key wins", provider: "anthropic", process: true, key: "key", want: `"key"`},
		{name: "refresh key wins", provider: "anthropic", process: true, want: `"fresh"`, refresh: func(context.Context, string) (string, error) { return "fresh", nil }},
		{name: "empty refresh is authoritative", provider: "anthropic", process: true, want: "Pi provider credential is empty", refresh: func(context.Context, string) (string, error) { return "", nil }},
		{name: "failed refresh is authoritative", provider: "anthropic", process: true, key: "stale-key", want: "refresh unavailable", refresh: func(context.Context, string) (string, error) { return "", errors.New("refresh unavailable") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.process {
				t.Setenv("ANTHROPIC_FEDERATION_RULE_ID", "rule")
				t.Setenv("ANTHROPIC_ORGANIZATION_ID", "org")
				t.Setenv("ANTHROPIC_IDENTITY_TOKEN_FILE", "/not-read-during-admission")
			}
			tier := ModelTier{TierConfig: TierConfig{Provider: tc.provider, Model: "test", APIKey: tc.key}}
			cfg, _, err := tier.BindPi(agentcore.PiConfig{Options: json.RawMessage(tc.options)}, PiModelOptions{RefreshKey: tc.refresh})
			if err != nil {
				t.Fatal(err)
			}
			key, err := cfg.Callback(context.Background(), "getApiKey", piModelJSON(tc.provider), nil)
			if err != nil {
				if err.Error() != tc.want || len(key) != 0 {
					t.Fatalf("credential admission: %s %v", key, err)
				}
			} else if string(key) != tc.want {
				t.Fatalf("credential: %s; want %s", key, tc.want)
			}
		})
	}
}

func TestPiModelBindingRejectsUnsupportedLifecycle(t *testing.T) {
	for _, config := range []TierConfig{
		{Provider: "openai", Model: "test", FallbackModel: "fallback"},
		{Provider: ai.VendorOpenAICodex, Model: "test", APIKey: ai.OAuthPoolKey},
		{Provider: "openai", Model: "test", BaseURL: "https://user:password@example.test/v1"},
	} {
		if _, _, err := (ModelTier{config}).BindPi(agentcore.PiConfig{}, PiModelOptions{}); err == nil {
			t.Fatalf("accepted unsupported model binding for %s", config.Provider)
		}
	}
}

func TestPiModelBindingGuardsRequestAndHookModel(t *testing.T) {
	tier := ModelTier{TierConfig: TierConfig{Provider: "openai", Model: "bound", APIKey: "secret", BaseURL: "https://bound.test/v1"}}
	var calls int
	var update json.RawMessage
	cfg, _, err := tier.BindPi(agentcore.PiConfig{Options: json.RawMessage(`{"callbacks":["prepareRequest"]}`), Callback: func(context.Context, string, json.RawMessage, func(json.RawMessage) error) (json.RawMessage, error) {
		calls++
		return update, nil
	}}, PiModelOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var options struct {
		InitialState struct{ Model json.RawMessage }
	}
	if err := json.Unmarshal(cfg.Options, &options); err != nil {
		t.Fatal(err)
	}
	model := options.InitialState.Model
	changed := json.RawMessage(strings.ReplaceAll(string(model), "bound.test", "old.test"))
	if _, err := cfg.Callback(context.Background(), "prepareRequest", piModelJSON(map[string]any{"model": changed}), nil); err == nil || calls != 0 {
		t.Fatal("unbound request reached the host hook")
	}
	update = piModelJSON(map[string]any{"model": changed})
	if _, err := cfg.Callback(context.Background(), "prepareRequest", piModelJSON(map[string]any{"model": model}), nil); err == nil || calls != 1 {
		t.Fatal("host hook redirected the bound credential")
	}
	update = json.RawMessage(`{"systemPrompt":"updated"}`)
	got, err := cfg.Callback(context.Background(), "prepareRequest", piModelJSON(map[string]any{"model": model}), nil)
	if err != nil || string(got) != string(update) || calls != 2 {
		t.Fatalf("lost valid host hook update: %s %v", got, err)
	}
}
