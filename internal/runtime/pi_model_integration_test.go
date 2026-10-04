package agentruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/lohi-ai/agentray/agentcore"
	"github.com/lohi-ai/agentray/agentcore/plugins/observe"
)

func TestPiModelBindingDrivesNativeProviderAndRotatesKeys(t *testing.T) {
	ctx := piSessionContext(t)
	var requests, refreshes, effects atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := requests.Add(1)
		if r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != fmt.Sprintf("Bearer fresh-key-%d", n) {
			t.Error("native request lost bound endpoint or fresh credential")
		}
		var body struct {
			Model       string
			MaxTokens   int `json:"max_tokens"`
			Temperature float64
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			http.Error(w, "bad request", 400)
			return
		}
		if body.Model != "bound-model" || body.MaxTokens != 128 || body.Temperature != 0.25 {
			t.Errorf("native provider options not applied: %+v", body)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		if n == 1 {
			fmt.Fprint(w, "data: "+`{"id":"first","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call","type":"function","function":{"name":"write","arguments":"{}"}}]},"finish_reason":null}]}`+"\n\n")
			fmt.Fprint(w, "data: "+`{"id":"first","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8}}`+"\n\n")
		} else {
			fmt.Fprint(w, "data: "+`{"id":"second","choices":[{"index":0,"delta":{"role":"assistant","content":"bound native answer"},"finish_reason":null}]}`+"\n\n")
			fmt.Fprint(w, "data: "+`{"id":"second","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":8,"completion_tokens":2,"total_tokens":10}}`+"\n\n")
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer server.Close()
	tier := ModelTier{TierConfig: TierConfig{Provider: "openai", Model: "bound-model", BaseURL: server.URL + "/v1", APIKey: "stale-key", ContextWindow: 10000, Capabilities: agentcore.ModelCapabilities{MaxOutputTokens: 128}}}
	composed, err := agentcore.New(agentcore.Config{Provider: agentcore.NewFauxProvider(agentcore.AssistantText("unused")), Model: "bound-model", Tools: agentcore.NewToolSet(piComposedTool{&effects}), Policy: agentcore.NewAllowList("write")})
	if err != nil {
		t.Fatal(err)
	}
	host, err := composed.OpenPiTools(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close()
	var options map[string]any
	_ = json.Unmarshal(piSessionOptions(), &options)
	options["streamOptions"] = map[string]any{"temperature": 0.25}
	worker, known, err := tier.BindPi(agentcore.PiConfig{Options: piSessionJSON(options), Callback: func(_ context.Context, method string, _ json.RawMessage, _ func(json.RawMessage) error) (json.RawMessage, error) {
		return nil, fmt.Errorf("native request escaped to %s callback", method)
	}}, PiModelOptions{MaxTokens: 500, Pricing: observe.Pricing{"bound-model": {InputPerM: 1, OutputPerM: 2}}, RefreshKey: func(context.Context, string) (string, error) {
		return fmt.Sprintf("fresh-key-%d", refreshes.Add(1)), nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	var tokens string
	result, err := RunPi(ctx, PiRunConfig{Host: host, Session: PiSessionConfig{Pi: worker, Policy: agentcore.NewAllowList("write")}, Input: piSessionJSON("test"), PricingKnown: known, Sink: func(event agentcore.StreamEvent) {
		if event.Type == agentcore.StreamToken {
			tokens += event.Token
		}
	}})
	if err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 2 || refreshes.Load() != 2 || effects.Load() != 1 || tokens != "bound native answer" {
		t.Fatalf("native binding did not drive complete run: requests=%d refreshes=%d effects=%d tokens=%q", requests.Load(), refreshes.Load(), effects.Load(), tokens)
	}
	u := result.Projection.Usage
	if u.InputTokens != 13 || u.OutputTokens != 5 || u.CostUnpriced || math.Abs(u.CostUSD-0.000023) > 0.000000001 {
		t.Fatalf("native pricing did not use host rates: %+v", u)
	}
	if strings.Contains(string(result.State), "fresh-key") || strings.Contains(string(result.State), "stale-key") || strings.Contains(string(result.Telemetry), "fresh-key") {
		t.Fatal("provider credential escaped into state/telemetry")
	}
}

func TestPiModelBindingCancellationStopsCredentialRefresh(t *testing.T) {
	ctx := piSessionContext(t)
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	started, stopped := make(chan struct{}), make(chan struct{})
	tier := ModelTier{TierConfig: TierConfig{Provider: "openai", Model: "test", BaseURL: "http://127.0.0.1:1/v1"}}
	worker, _, err := tier.BindPi(agentcore.PiConfig{}, PiModelOptions{RefreshKey: func(ctx context.Context, _ string) (string, error) {
		close(started)
		<-ctx.Done()
		close(stopped)
		return "", ctx.Err()
	}})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := RunPi(runCtx, PiRunConfig{Session: PiSessionConfig{Pi: worker}, Input: piSessionJSON("cancel")})
		done <- err
	}()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	cancel()
	select {
	case <-stopped:
	case <-ctx.Done():
		t.Fatal("credential refresh outlived native cancellation")
	}
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancel returned %v", err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}

func TestPiModelPayloadHookCancellationStopsBeforeHTTP(t *testing.T) {
	ctx := piSessionContext(t)
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	started, stopped := make(chan struct{}), make(chan struct{})
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.Error(w, "unexpected request", 400)
	}))
	defer server.Close()
	tier := ModelTier{TierConfig: TierConfig{Provider: "openai", Model: "test", BaseURL: server.URL + "/v1", APIKey: "test"}}
	worker, _, err := tier.BindPi(agentcore.PiConfig{Options: json.RawMessage(`{"callbacks":["onPayload"]}`), Callback: func(ctx context.Context, method string, _ json.RawMessage, _ func(json.RawMessage) error) (json.RawMessage, error) {
		if method != "onPayload" {
			return nil, fmt.Errorf("unexpected callback: %s", method)
		}
		close(started)
		<-ctx.Done()
		close(stopped)
		return nil, ctx.Err()
	}}, PiModelOptions{OutputSchema: &agentcore.OutputSchema{Schema: map[string]any{"type": "object"}}})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := RunPi(runCtx, PiRunConfig{Session: PiSessionConfig{Pi: worker}, Input: piSessionJSON("cancel")})
		done <- err
	}()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	cancel()
	select {
	case <-stopped:
	case <-ctx.Done():
		t.Fatal("payload hook outlived native cancellation")
	}
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) || requests.Load() != 0 {
			t.Fatalf("payload cancellation reached HTTP: %v requests=%d", err, requests.Load())
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}

func TestPiModelBindingResumeRejectsPreviousEndpointBeforeResolvingKey(t *testing.T) {
	ctx := piSessionContext(t)
	var oldRequests, newRequests, refreshes atomic.Int32
	oldServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		oldRequests.Add(1)
		if r.Header.Get("Authorization") != "Bearer old-key" {
			t.Error("resumed credential reached previous endpoint")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: "+`{"id":"first","choices":[{"index":0,"delta":{"role":"assistant","content":"done"},"finish_reason":"stop"}]}`+"\n\ndata: [DONE]\n\n")
	}))
	defer oldServer.Close()
	newServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		newRequests.Add(1)
		http.Error(w, "unexpected request", 500)
	}))
	defer newServer.Close()
	tier := ModelTier{TierConfig: TierConfig{Provider: "openai", Model: "same-model", BaseURL: oldServer.URL + "/v1", APIKey: "old-key"}}
	worker, _, err := tier.BindPi(agentcore.PiConfig{}, PiModelOptions{})
	if err != nil {
		t.Fatal(err)
	}
	run := PiRunConfig{Input: piSessionJSON("first"), Session: PiSessionConfig{Pi: worker, Store: agentcore.NewMemorySessionStore(), SessionID: "bound-endpoint"}}
	if _, err := RunPi(ctx, run); err != nil {
		t.Fatal(err)
	}
	tier.BaseURL = newServer.URL + "/v1"
	worker, _, err = tier.BindPi(agentcore.PiConfig{}, PiModelOptions{RefreshKey: func(context.Context, string) (string, error) {
		refreshes.Add(1)
		return "new-key", nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	run.Session.Pi, run.Session.Resume, run.Input = worker, true, piSessionJSON("next")
	if _, err := RunPi(ctx, run); err == nil || !strings.Contains(err.Error(), "unbound model") {
		t.Fatalf("resume silently changed credential binding: %v", err)
	}
	if oldRequests.Load() != 1 || newRequests.Load() != 0 || refreshes.Load() != 0 {
		t.Fatalf("resume performed I/O before validating binding: old=%d new=%d refreshes=%d", oldRequests.Load(), newRequests.Load(), refreshes.Load())
	}
}
