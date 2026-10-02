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
	for _, tc := range []struct {
		name, provider, base, api, identity, endpoint string
		caps                                          agentcore.ModelCapabilities
	}{
		{name: "openai", provider: "openai", base: "https://example.test/v1", api: "openai-completions", identity: "openai", endpoint: "https://example.test/v1"},
		{name: "model responses", provider: "openai", base: "https://example.test/v1", api: "openai-responses", identity: "openai", endpoint: "https://example.test/v1", caps: agentcore.ModelCapabilities{StatefulResponses: agentcore.CapabilitySupported}},
		{name: "explicit responses endpoint", provider: ai.VendorOpenAIResponses, base: "https://example.test/v1/responses", api: "openai-responses", identity: ai.VendorOpenAIResponses, endpoint: "https://example.test/v1"},
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
