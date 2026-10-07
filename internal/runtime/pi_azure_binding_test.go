package agentruntime

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/2found/2ai/ai"
)

func TestAzureModelBindingFreezesResolvedConfiguration(t *testing.T) {
	for _, tc := range []struct{ name, base, processBase, resource, scoped, wantURL, wantVersion, wantDeployment string }{
		{name: "tier endpoint", base: "https://resource.openai.azure.com/openai/", wantURL: "https://resource.openai.azure.com/openai/v1", wantVersion: "v1", wantDeployment: "model"},
		{name: "process endpoint", processBase: "https://process.example/v1", wantURL: "https://process.example/v1", wantVersion: "v1", wantDeployment: "model"},
		{name: "process resource", resource: "resource", wantURL: "https://resource.openai.azure.com/openai/v1", wantVersion: "v1", wantDeployment: "model"},
		{name: "scoped endpoint", processBase: "https://process.example/v1", scoped: `{"AZURE_OPENAI_BASE_URL":"https://scoped.example/v1","AZURE_OPENAI_API_VERSION":"scoped","AZURE_OPENAI_DEPLOYMENT_NAME_MAP":"model=deployment","UNRELATED":"keep"}`, wantURL: "https://scoped.example/v1", wantVersion: "scoped", wantDeployment: "deployment"},
		{name: "tier defeats environment route", base: "https://bound.example/v1", processBase: "https://process.example/v1", scoped: `{"AZURE_OPENAI_BASE_URL":"https://scoped.example/v1","AZURE_OPENAI_RESOURCE_NAME":"other"}`, wantURL: "https://bound.example/v1", wantVersion: "v1", wantDeployment: "model"},
		{name: "empty scoped fallback", processBase: "https://process.example/v1", scoped: `{"AZURE_OPENAI_BASE_URL":"","AZURE_OPENAI_API_VERSION":"","AZURE_OPENAI_DEPLOYMENT_NAME_MAP":""}`, wantURL: "https://process.example/v1", wantVersion: "v1", wantDeployment: "model"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearNativeAzureEnv(t)
			t.Setenv("AZURE_OPENAI_BASE_URL", tc.processBase)
			t.Setenv("AZURE_OPENAI_RESOURCE_NAME", tc.resource)
			env := tc.scoped
			if env == "" {
				env = "{}"
			}
			original := NativeAgentConfig{Options: json.RawMessage(`{"streamOptions":{"env":` + env + `,"temperature":0.25}}`)}
			before := string(original.Options)
			tier := ModelTier{TierConfig: TierConfig{Provider: " Azure-OpenAI-Responses ", Model: "model", BaseURL: tc.base, APIKey: "secret-key"}}
			cfg, _, err := tier.BindPi(original, PiModelOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if string(original.Options) != before || strings.Contains(string(cfg.Options), "secret-key") {
				t.Fatal("binding mutated input or exposed credential")
			}
			var wire struct {
				InitialState  struct{ Model json.RawMessage }
				StreamOptions json.RawMessage
			}
			if err := json.Unmarshal(cfg.Options, &wire); err != nil {
				t.Fatal(err)
			}
			t.Setenv("AZURE_OPENAI_BASE_URL", "https://changed.example/")
			t.Setenv("AZURE_OPENAI_RESOURCE_NAME", "changed")
			t.Setenv("AZURE_OPENAI_API_VERSION", "changed")
			t.Setenv("AZURE_OPENAI_DEPLOYMENT_NAME_MAP", "model=changed")
			config, err := ai.ResolveAzureResponsesConfig(wire.InitialState.Model, wire.StreamOptions)
			if err != nil || config.BaseURL != tc.wantURL || config.APIVersion != tc.wantVersion || config.DeploymentName != tc.wantDeployment {
				t.Fatalf("configuration drifted: %+v %v", config, err)
			}
			params, err := ai.BuildAzureResponsesParams(wire.InitialState.Model, ai.NormalizeContext(ai.Context{}), wire.StreamOptions)
			if err != nil {
				t.Fatal(err)
			}
			var payload struct {
				Model       string
				Temperature float64
			}
			if err := json.Unmarshal(params, &payload); err != nil {
				t.Fatal(err)
			}
			if payload.Model != tc.wantDeployment || payload.Temperature != 0.25 {
				t.Fatalf("wrong request: %s", params)
			}
			if _, err := cfg.Callback(context.Background(), "getApiKey", piModelJSON("other"), nil); err == nil {
				t.Fatal("unbound provider received key")
			}
			key, err := cfg.Callback(context.Background(), "getApiKey", piModelJSON(ai.VendorAzureResponses), nil)
			if err != nil || string(key) != `"secret-key"` {
				t.Fatalf("key binding: %s %v", key, err)
			}
			var changed map[string]any
			_ = json.Unmarshal(wire.InitialState.Model, &changed)
			changed["baseUrl"] = "https://changed.example/"
			if _, err := cfg.Callback(context.Background(), "prepareRequest", piModelJSON(map[string]any{"model": changed}), nil); err == nil {
				t.Fatal("endpoint mutation admitted")
			}
		})
	}
}

func TestAzureModelBindingRejectsInvalidRoutesAndRefresh(t *testing.T) {
	clearNativeAzureEnv(t)
	for _, base := range []string{"", "not a URL", "file:///tmp/config", "https://user:password@example.test/v1"} {
		if _, _, err := (ModelTier{TierConfig: TierConfig{Provider: ai.VendorAzureResponses, Model: "test", BaseURL: base}}).BindPi(NativeAgentConfig{}, PiModelOptions{}); err == nil {
			t.Fatalf("invalid host route admitted: %q", base)
		}
	}
	sentinel := errors.New("refresh rejected")
	for _, failure := range []error{nil, sentinel} {
		tier := ModelTier{TierConfig: TierConfig{Provider: ai.VendorAzureResponses, Model: "test", BaseURL: "https://example.test/v1", APIKey: "stale"}}
		cfg, _, err := tier.BindPi(NativeAgentConfig{}, PiModelOptions{RefreshKey: func(context.Context, string) (string, error) { return "", failure }})
		if err != nil {
			t.Fatal(err)
		}
		key, err := cfg.Callback(context.Background(), "getApiKey", piModelJSON(ai.VendorAzureResponses), nil)
		if err == nil || len(key) > 0 || (failure != nil && !errors.Is(err, sentinel)) {
			t.Fatalf("refresh fell back: %s %v", key, err)
		}
	}
}

func TestAzureModelBindingRowRefreshIsRequiredForSiblingRows(t *testing.T) {
	tier := ModelTier{TierConfig: TierConfig{Provider: ai.VendorAzureResponses, ProviderID: "a", Model: "primary", Fallback: &TierConfig{Provider: ai.VendorAzureResponses, ProviderID: "b", Model: "fallback"}}}
	options := PiModelOptions{RefreshKey: func(context.Context, string) (string, error) { return "ambiguous", nil }}
	if _, err := (BuildParams{}).nativeLadderOptions(tier, options); err == nil || !strings.Contains(err.Error(), "provider-row credential refresh") {
		t.Fatalf("sibling rows accepted vendor-only refresh: %v", err)
	}
	p := BuildParams{RefreshProviderKey: func(_ context.Context, row, _, _ string) (string, error) { return row + "-key", nil }}
	resolve, err := p.nativeLadderOptions(tier, options)
	if err != nil {
		t.Fatal(err)
	}
	for _, rung := range tier.resolvedRungs() {
		opts, err := resolve(rung.tier)
		if err != nil {
			t.Fatal(err)
		}
		key, err := opts.RefreshKey(context.Background(), ai.VendorAzureResponses)
		if err != nil || key != rung.tier.ProviderID+"-key" {
			t.Fatal("row refresh lost identity", err)
		}
	}
}
