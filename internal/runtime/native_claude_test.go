package agentruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lohi-ai/agentray/agentcore"
	"github.com/lohi-ai/agentray/ai"
)

type nativeClaudeAccountSource struct{ acquired *atomic.Int32 }

func (s *nativeClaudeAccountSource) Acquire(context.Context) (ai.OAuthToken, error) {
	return ai.OAuthToken{AccountID: "account", AccessToken: fmt.Sprintf("refreshed-%d", s.acquired.Add(1))}, nil
}
func (*nativeClaudeAccountSource) Report(context.Context, ai.OAuthToken, error) {}
func TestNativeClaudePoolControlsAndDurability(t *testing.T) {
	testNativeRunnerHTTPProviderControlsAndDurability(t, false, ai.VendorClaudeCode, false)
}
func TestNativeClaudePoolChildrenAndSummary(t *testing.T) {
	testNativeHTTPProviderInheritedByChildrenAndSummary(t, false, true, false, false, false, ai.VendorClaudeCode)
}
func TestNativeClaudePoolBinding(t *testing.T) {
	var acquired atomic.Int32
	pool := &nativeClaudeAccountSource{acquired: &acquired}
	tier := ModelTier{TierConfig: TierConfig{Provider: ai.VendorClaudeCode, Model: "claude-test", TokenSource: pool, APIKey: ai.OAuthPoolKey}}
	cfg, _, stream, err := tier.bindNativeTier(agentcore.PiConfig{}, PiModelOptions{RefreshKey: func(context.Context, string) (string, error) { t.Error("static refresh invoked"); return "stale", nil }}, nil)
	if err != nil || stream == nil {
		t.Fatal("native Claude pool not bound", err)
	}
	var wire struct {
		InitialState struct {
			Model struct{ API, Provider, BaseURL string }
		}
	}
	if err = json.Unmarshal(cfg.Options, &wire); err != nil {
		t.Fatal(err)
	}
	if wire.InitialState.Model.API != "anthropic-messages" || wire.InitialState.Model.Provider != ai.VendorClaudeCode || wire.InitialState.Model.BaseURL != "https://api.anthropic.com" {
		t.Fatal("wrong Claude wire", wire)
	}
	key, err := cfg.Callback(context.Background(), "getApiKey", json.RawMessage(`"claude-code"`), nil)
	if err != nil || string(key) != "null" || acquired.Load() != 0 {
		t.Fatal("binding exposed/acquired credential", err)
	}
	if _, err = cfg.Callback(context.Background(), "getApiKey", json.RawMessage(`"anthropic"`), nil); err == nil {
		t.Fatal("foreign vendor accessed pool")
	}
	if _, err = cfg.Callback(context.Background(), "prepareRequest", json.RawMessage(`{"model":{"id":"claude-test","api":"anthropic-messages","provider":"claude-code","baseUrl":"https://other.test"}}`), nil); err == nil {
		t.Fatal("changed endpoint admitted")
	}
	if _, _, err = tier.BindPi(agentcore.PiConfig{}, PiModelOptions{}); err == nil {
		t.Fatal("worker binding accepted native pool")
	}
	if acquired.Load() != 0 {
		t.Fatal("invalid binding acquired credential")
	}
}

func TestNativeClaudePoolRotation(t *testing.T) {
	pool := &nativeCodexAccountSource{tokens: []ai.OAuthToken{{AccountID: "first", AccessToken: "first-fixture"}, {AccountID: "second", AccessToken: "second-fixture"}}}
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		switch r.Header.Get("Authorization") {
		case "Bearer first-fixture":
			w.WriteHeader(401)
			fmt.Fprint(w, `{"error":{"message":"expired"}}`)
		case "Bearer second-fixture":
			w.Header().Set("Content-Type", "text/event-stream")
			writeNativeAnthropicAnswer(w, "pooled Claude answer", 3, 2)
		default:
			t.Error("unbound or stale credential sent")
			http.Error(w, "invalid", 400)
		}
	}))
	defer server.Close()
	p := representativeBuildParams()
	p.Sandbox, p.HTTPTool, p.Subagents = nil, nil, nil
	p.Goal, p.PrepareNextTurn = "", nil
	p.Tools = nil
	p.RefreshKey = func(context.Context, string) (string, error) {
		t.Error("pool used static refresh")
		return "stale", nil
	}
	tier := ModelTier{TierConfig: TierConfig{Provider: ai.VendorClaudeCode, Model: "native-http", BaseURL: server.URL, APIKey: ai.OAuthPoolKey, TokenSource: pool}}
	runner := NewRunner(nil, WithPiRuntime(PiRuntimeConfig{}))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result, err := runner.runModelLoop(ctx, p, RunOptions{Prompt: "hello"}, tier, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Final != "pooled Claude answer" || requests.Load() != 2 || pool.acquired != 2 || len(pool.reports) != 2 {
		t.Fatalf("final=%q requests=%d acquired=%d reports=%d", result.Final, requests.Load(), pool.acquired, len(pool.reports))
	}
	var auth *agentcore.ProviderError
	if pool.reports[0].account != "first" || !errors.As(pool.reports[0].err, &auth) || auth.Status != 401 || pool.reports[1].account != "second" || pool.reports[1].err != nil {
		t.Fatal("account reporting mismatch")
	}
	for _, token := range pool.tokens {
		if strings.Contains(string(result.NativeState)+string(result.NativeTelemetry), token.AccessToken) {
			t.Fatal("credential persisted in native artifacts")
		}
	}
}
