package agentruntime

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/2found/2ai/agentcore"
	"github.com/2found/2ai/ai"
)

func TestResolvedNativeRungsKeepProviderRowCredentials(t *testing.T) {
	primaryPool := &nativeCodexAccountSource{}
	fallbackPool := &nativeCodexAccountSource{}
	primary := TierConfig{Provider: ai.VendorOpenAICodex, ProviderID: "primary-row", Model: "primary", BaseURL: "https://same.test", APIKey: "primary-key", TokenSource: primaryPool, ContextWindow: 12345}
	for _, cross := range []bool{false, true} {
		t.Run(map[bool]string{false: "same row", true: "other row"}[cross], func(t *testing.T) {
			fb := TierConfig{Model: "fallback", APIKey: "fallback-key", TokenSource: fallbackPool, ContextWindow: 999}
			if cross {
				fb.Provider, fb.ProviderID, fb.BaseURL = primary.Provider, "fallback-row", primary.BaseURL
			}
			tier := ModelTier{primary}
			tier.Fallback = &fb
			before := tier
			rungs := tier.resolvedRungs()
			if len(rungs) != 2 || rungs[0].tier.ContextWindow != 12345 || rungs[1].tier.ContextWindow != ai.ContextWindowFor(primary.Provider, "fallback") {
				t.Fatal("rung/window resolution changed")
			}
			expectedPool := primaryPool
			expectedKey, expectedRow := "primary-key", "primary-row"
			if cross {
				expectedPool = fallbackPool
				expectedKey, expectedRow = "fallback-key", "fallback-row"
			}
			next := rungs[1].tier
			if next.TokenSource != expectedPool || next.APIKey != expectedKey || next.ProviderID != expectedRow || rungs[1].sharePrimary == cross {
				t.Fatal("fallback crossed credential identity")
			}
			if next.Fallback != nil || next.FallbackModel != "" || rungs[0].tier.Fallback != nil {
				t.Fatal("resolved rung retained recursive fallback")
			}
			cfg, _, stream, err := next.bindNativeTier(NativeAgentConfig{}, PiModelOptions{}, nil)
			if err != nil || stream == nil {
				t.Fatalf("native pool binding: %v", err)
			}
			key, err := cfg.Callback(context.Background(), "getApiKey", json.RawMessage(`"openai-codex"`), nil)
			if err != nil || string(key) != "null" || primaryPool.acquired != 0 || fallbackPool.acquired != 0 {
				t.Fatal("binding acquired or serialized pool credential")
			}
			if !reflect.DeepEqual(tier, before) || !reflect.DeepEqual(*tier.Fallback, fb) {
				t.Fatal("resolution mutated caller config")
			}
		})
	}
}
func TestResolvedNativeRungsKeepWireAndStaticKeyBoundaries(t *testing.T) {
	tier := ModelTier{TierConfig{Provider: "openai", ProviderID: "a", Model: "primary", BaseURL: "https://same.test/v1", APIKey: "primary-key", Fallback: &TierConfig{Provider: "openai", ProviderID: "b", Model: "fallback", BaseURL: "https://same.test/v1", APIKey: "fallback-key", Capabilities: agentcore.ModelCapabilities{StatefulResponses: agentcore.CapabilitySupported}}}}
	rungs := tier.resolvedRungs()
	if len(rungs) != 2 || rungs[1].sharePrimary {
		t.Fatal("different provider row reused primary")
	}
	for i, rung := range rungs {
		cfg, _, _, err := rung.tier.bindNativeTier(NativeAgentConfig{}, PiModelOptions{}, nil)
		if err != nil {
			t.Fatal(err)
		}
		var wire struct {
			InitialState struct{ Model struct{ API string } }
		}
		_ = json.Unmarshal(cfg.Options, &wire)
		wantAPI, wantKey := "openai-completions", `"primary-key"`
		if i == 1 {
			wantAPI, wantKey = "openai-responses", `"fallback-key"`
		}
		key, err := cfg.Callback(context.Background(), "getApiKey", json.RawMessage(`"openai"`), nil)
		if err != nil || string(key) != wantKey || wire.InitialState.Model.API != wantAPI {
			t.Fatalf("rung %d api=%s key mismatch=%v err=%v", i, wire.InitialState.Model.API, string(key) != wantKey, err)
		}
	}
}
