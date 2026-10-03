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
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(503)
		_, _ = w.Write([]byte(`{"error":{"message":"busy"}}`))
	}))
	defer server.Close()
	model, _ := json.Marshal(map[string]any{"id": "fixture", "api": "openai-completions", "provider": "openai", "baseUrl": server.URL, "maxTokens": 100, "contextWindow": 1000})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	waits, observed := 0, 0
	runner := nativeRungAttempts{
		policy: agentcore.RetryPolicy{MaxAttempts: 2, MaxDelay: time.Millisecond},
		open: func(ctx context.Context, _ int) (*ai.AssistantMessageEventStream, error) {
			return NativeProviderStream(ctx, model, ai.TranscriptContext{}, map[string]any{"apiKey": "fixture", "maxRetries": 0})
		},
		commit: func(context.Context) error { t.Error("failed request committed"); return nil },
		wait: func(_ context.Context, d time.Duration) error {
			waits++
			if d != time.Millisecond {
				t.Error("HTTP retry hint not capped", d)
			}
			return nil
		},
		observe: func(_ context.Context, a nativeRetryAttempt) error {
			observed++
			var pe *agentcore.ProviderError
			if !errors.As(a.failure, &pe) || pe.Status != 503 || pe.RetryAfter != 7*time.Second {
				t.Errorf("HTTP failure lost: %v", a.failure)
			}
			return nil
		},
	}
	out := ai.NewAssistantMessageEventStream()
	last, err := runner.run(ctx, out)
	if err != nil || requests.Load() != 2 || waits != 1 || observed != 2 || last.terminal.Type != "error" {
		t.Fatalf("HTTP retry: requests=%d waits=%d observed=%d err=%v", requests.Load(), waits, observed, err)
	}
	assertAttemptEmpty(t, out)
	if err = last.publish(out); err != nil {
		t.Fatal(err)
	}
	calls := 0
	runner.open = func(ctx context.Context, _ int) (*ai.AssistantMessageEventStream, error) {
		calls++
		return NativeProviderStream(ctx, model, ai.TranscriptContext{}, map[string]any{"apiKey": "fixture", "onPayload": func(context.Context, json.RawMessage, json.RawMessage) (json.RawMessage, error) {
			return nil, &agentcore.ProviderError{Status: 503, Message: "host callback failed"}
		}})
	}
	runner.observe = nil
	runner.wait = func(context.Context, time.Duration) error { t.Error("host callback retried"); return nil }
	_, err = runner.run(ctx, ai.NewAssistantMessageEventStream())
	if err == nil || calls != 1 || requests.Load() != 2 {
		t.Fatal("callback crossed provider retry boundary", calls, requests.Load(), err)
	}
}
