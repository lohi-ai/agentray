package agentruntime

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/lohi-ai/agentray/agentcore"
	"github.com/lohi-ai/agentray/ai"
)

func TestNativeCodexCredentialRefreshAndSessionCleanup(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var mu sync.Mutex
	accounts := []string{}
	closed := make(chan string, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		account := r.Header.Get("Chatgpt-Account-Id")
		if r.Header.Get("Authorization") == "" {
			t.Error("missing per-request credential")
		}
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		defer func() { closed <- account }()
		mu.Lock()
		accounts = append(accounts, account)
		mu.Unlock()
		for {
			_, _, err = conn.ReadMessage()
			if err != nil {
				return
			}
			item := map[string]any{"type": "message", "id": "msg", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "native codex answer"}}}
			events := []any{map[string]any{"type": "response.output_item.added", "output_index": 0, "item": map[string]any{"type": "message", "id": "msg"}}, map[string]any{"type": "response.output_item.done", "output_index": 0, "item": item}, map[string]any{"type": "response.completed", "response": map[string]any{"id": "response", "status": "completed", "output": []any{item}}}}
			for _, event := range events {
				raw, _ := json.Marshal(event)
				if err = conn.WriteMessage(websocket.TextMessage, raw); err != nil {
					return
				}
			}
		}
	}))
	defer server.Close()
	session := t.Name()
	defer ai.ResetCodexWebSocketDebugStats(session)
	defer ai.CloseCodexResponsesSessions(session)
	options := piModelJSON(map[string]any{"initialState": map[string]any{"model": map[string]any{"id": "test", "api": "openai-codex-responses", "provider": "openai-codex", "baseUrl": server.URL, "input": []string{"text"}}}, "streamMode": "native", "sessionId": session, "transport": "auto", "callbacks": []string{"getApiKey"}})
	acquired := 0
	agent, err := NewNativeAgent(ctx, NativeAgentConfig{Options: options, Callback: func(_ context.Context, method string, params json.RawMessage, _ func(json.RawMessage) error) (json.RawMessage, error) {
		if method != "getApiKey" {
			return nil, fmt.Errorf("unexpected callback %s", method)
		}
		if string(params) != `"openai-codex"` {
			t.Errorf("wrong credential provider: %s", params)
		}
		acquired++
		claims, _ := json.Marshal(map[string]any{"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": fmt.Sprintf("account-%d", acquired)}})
		return json.Marshal("h." + base64.StdEncoding.EncodeToString(claims) + ".s")
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer agent.Close()
	for i := 0; i < 2; i++ {
		if err = agent.Prompt(ctx, json.RawMessage(`"hello"`)); err != nil {
			t.Fatal(err)
		}
	}
	state, err := agent.State(ctx)
	if err != nil || !strings.Contains(string(state), "native codex answer") {
		t.Fatalf("state=%s err=%v", state, err)
	}
	mu.Lock()
	got := strings.Join(accounts, ",")
	mu.Unlock()
	if acquired != 2 || got != "account-1,account-2" {
		t.Fatalf("credential/account isolation: acquired=%d accounts=%s", acquired, got)
	}
	stats := ai.GetCodexWebSocketDebugStats(session)
	if stats == nil || stats.Requests != 2 || stats.ConnectionsCreated != 2 || stats.ConnectionsReused != 0 {
		t.Fatalf("unexpected stats: %+v", stats)
	}
	if err = agent.Close(); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		select {
		case <-closed:
		case <-ctx.Done():
			t.Fatal("agent close left cached socket alive")
		}
	}
	if ai.GetCodexWebSocketDebugStats(session) == nil {
		t.Fatal("agent close erased diagnostic counters")
	}
}

func TestNativeCodexRejectsMissingCredential(t *testing.T) {
	_, err := (ai.NativeProvider{}).Stream(context.Background(), json.RawMessage(`{"id":"test","api":"openai-codex-responses","provider":"openai-codex"}`), ai.NormalizeContext(ai.Context{}), nil)
	if err == nil || err.Error() != "No API key for provider: openai-codex" {
		t.Fatalf("got %v", err)
	}
}

type nativeCodexAccountSource struct {
	acquired int
	tokens   []ai.OAuthToken
	reports  []struct {
		account string
		err     error
	}
}

func (p *nativeCodexAccountSource) Acquire(context.Context) (ai.OAuthToken, error) {
	if p.acquired >= len(p.tokens) {
		return ai.OAuthToken{}, fmt.Errorf("pool exhausted")
	}
	token := p.tokens[p.acquired]
	p.acquired++
	return token, nil
}
func (p *nativeCodexAccountSource) Report(_ context.Context, token ai.OAuthToken, err error) {
	p.reports = append(p.reports, struct {
		account string
		err     error
	}{token.AccountID, err})
}
func TestNativeRunnerCodexPoolBindingAndRotation(t *testing.T) {
	pool := &nativeCodexAccountSource{}
	for _, account := range []string{"first", "second"} {
		claims, _ := json.Marshal(map[string]any{"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": account}})
		pool.tokens = append(pool.tokens, ai.OAuthToken{AccountID: account, AccessToken: "h." + base64.StdEncoding.EncodeToString(claims) + ".s"})
	}
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if websocket.IsWebSocketUpgrade(r) {
			http.Error(w, "SSE only", 503)
			return
		}
		requests.Add(1)
		if r.Header.Get("Authorization") == "Bearer "+ai.OAuthPoolKey || r.Header.Get("Authorization") == "Bearer stale-key" {
			t.Error("sent static pool credential")
		}
		_, _ = io.Copy(io.Discard, r.Body)
		if r.Header.Get("Chatgpt-Account-Id") == "first" {
			w.WriteHeader(401)
			fmt.Fprint(w, `{"error":{"message":"expired"}}`)
			return
		}
		if r.Header.Get("Chatgpt-Account-Id") != "second" {
			t.Error("wrong account")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		writeNativeResponsesAnswer(w, "pooled native answer", "r", 3, 2)
	}))
	defer server.Close()
	p := representativeBuildParams()
	p.Sandbox, p.HTTPTool, p.Subagents = nil, nil, nil
	p.Goal, p.PrepareNextTurn = "", nil
	p.Tools = nil
	p.ProviderSessionID = t.Name()
	defer ai.ResetCodexWebSocketDebugStats(p.ProviderSessionID)
	defer ai.CloseCodexResponsesSessions(p.ProviderSessionID)
	p.RefreshKey = func(context.Context, string) (string, error) {
		t.Error("pool binding invoked static key refresh")
		return "stale-key", nil
	}
	tier := ModelTier{TierConfig: TierConfig{Provider: ai.VendorOpenAICodex, Model: "test", BaseURL: server.URL, APIKey: ai.OAuthPoolKey, TokenSource: pool}}
	runner := NewRunner(nil, WithPiRuntime(PiRuntimeConfig{}))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result, err := runner.runModelLoop(ctx, p, RunOptions{Prompt: "hello"}, tier, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Final != "pooled native answer" || requests.Load() != 2 || pool.acquired != 2 || len(pool.reports) != 2 {
		t.Fatalf("final=%q requests=%d acquired=%d reports=%d", result.Final, requests.Load(), pool.acquired, len(pool.reports))
	}
	var auth *agentcore.ProviderError
	if pool.reports[0].account != "first" || !errors.As(pool.reports[0].err, &auth) || auth.Status != 401 || pool.reports[1].account != "second" || pool.reports[1].err != nil {
		t.Fatal("account report lifecycle mismatch")
	}
	for _, token := range pool.tokens {
		if strings.Contains(string(result.NativeState), token.AccessToken) || strings.Contains(string(result.NativeTelemetry), token.AccessToken) {
			t.Fatal("credential persisted in native artifacts")
		}
	}
	if _, _, err = tier.BindPi(NativeAgentConfig{}, PiModelOptions{}); err == nil {
		t.Fatal("unbound worker path accepted account pool")
	}
}

func TestNativeCodexPoolBindingKeepsModelAndCredentialBoundaries(t *testing.T) {
	pool := &nativeCodexAccountSource{}
	tier := ModelTier{TierConfig: TierConfig{Provider: ai.VendorOpenAICodex, Model: "test", BaseURL: "https://example.test", APIKey: ai.OAuthPoolKey, TokenSource: pool}}
	cfg, _, err := tier.bindPi(NativeAgentConfig{}, PiModelOptions{RefreshKey: func(context.Context, string) (string, error) { t.Error("unexpected key refresh"); return "secret", nil }}, true)
	if err != nil {
		t.Fatal(err)
	}
	key, err := cfg.Callback(context.Background(), "getApiKey", json.RawMessage(`"openai-codex"`), nil)
	if err != nil || string(key) != "null" || pool.acquired != 0 {
		t.Fatalf("credential entered JSON callback: %s %v", key, err)
	}
	if _, err = cfg.Callback(context.Background(), "getApiKey", json.RawMessage(`"openai"`), nil); err == nil {
		t.Fatal("foreign provider accepted")
	}
	if _, err = cfg.Callback(context.Background(), "prepareRequest", json.RawMessage(`{"model":{"id":"test","api":"openai-codex-responses","provider":"openai-codex","baseUrl":"https://other.test"}}`), nil); err == nil {
		t.Fatal("changed endpoint accepted")
	}
	if pool.acquired != 0 {
		t.Fatal("invalid binding acquired credential")
	}
}
