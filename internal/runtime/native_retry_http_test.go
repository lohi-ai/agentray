package agentruntime

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lohi-ai/agentray/agentcore"
	"github.com/lohi-ai/agentray/ai"
)

func TestNativeRungRetriesUseProviderHTTPMetadata(t *testing.T) {
	for _, api := range []string{"openai-completions", "pi-messages"} {
		t.Run(api, func(t *testing.T) { testNativeRungRetriesUseProviderHTTPMetadata(t, api) })
	}
}

func testNativeRungRetriesUseProviderHTTPMetadata(t *testing.T, api string) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(503)
		_, _ = w.Write([]byte(`{"error":{"message":"busy"}}`))
	}))
	defer server.Close()
	model, _ := json.Marshal(map[string]any{"id": "fixture", "api": api, "provider": "gateway", "baseUrl": server.URL, "maxTokens": 100, "contextWindow": 1000})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	waits, observed := 0, 0
	provider := ai.FallbackProvider{Retry: agentcore.RetryPolicy{MaxAttempts: 2, MaxDelay: time.Millisecond}}
	runner := ai.FallbackRequest{Candidates: 1,
		Open: func(ctx context.Context, _, _ int) (*ai.AssistantMessageEventStream, error) {
			return (ai.NativeProvider{}).Stream(ctx, model, ai.TranscriptContext{}, map[string]any{"apiKey": "fixture", "maxRetries": 0})
		},
		Commit: func(context.Context, int) error { t.Error("failed request committed"); return nil },
		Observe: func(_ context.Context, _ int, a ai.FallbackAttempt) error {
			observed++
			var pe *agentcore.ProviderError
			if !errors.As(a.Failure, &pe) || pe.Status != 503 || pe.RetryAfter != 7*time.Second {
				t.Errorf("HTTP failure lost: %v", a.Failure)
			}
			return nil
		},
	}
	provider.Wait = func(_ context.Context, d time.Duration) error {
		waits++
		if d != time.Millisecond {
			t.Error("HTTP retry hint not capped", d)
		}
		return nil
	}
	out := ai.NewAssistantMessageEventStream()
	last, err := provider.Run(ctx, out, runner)
	if err != nil || requests.Load() != 2 || waits != 1 || observed != 2 || last.Terminal.Type != "error" {
		t.Fatalf("HTTP retry: requests=%d waits=%d observed=%d err=%v", requests.Load(), waits, observed, err)
	}
	calls := 0
	runner.Open = func(ctx context.Context, _, _ int) (*ai.AssistantMessageEventStream, error) {
		calls++
		return (ai.NativeProvider{}).Stream(ctx, model, ai.TranscriptContext{}, map[string]any{"apiKey": "fixture", "onPayload": func(context.Context, json.RawMessage, json.RawMessage) (json.RawMessage, error) {
			return nil, &agentcore.ProviderError{Status: 503, Message: "host callback failed"}
		}})
	}
	runner.Observe = nil
	provider.Wait = func(context.Context, time.Duration) error { t.Error("host callback retried"); return nil }
	_, err = provider.Run(ctx, ai.NewAssistantMessageEventStream(), runner)
	if err == nil || calls != 1 || requests.Load() != 2 {
		t.Fatal("callback crossed provider retry boundary", calls, requests.Load(), err)
	}
}
