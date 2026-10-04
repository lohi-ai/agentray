package agentruntime

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lohi-ai/agentray/agentcore"
	"github.com/lohi-ai/agentray/telemetry/llm"
)

func TestNativeAuxiliaryCompletionUsesFallbackAndTelemetry(t *testing.T) {
	var mu sync.Mutex
	var requests []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model    string
			Stream   bool
			Tools    []json.RawMessage
			Messages json.RawMessage
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		mu.Lock()
		requests = append(requests, body.Model)
		mu.Unlock()
		if !body.Stream || len(body.Tools) != 0 || !strings.Contains(string(body.Messages), "review evidence") || r.Header.Get("Authorization") != "Bearer fixture-key" {
			t.Error("auxiliary request lost native controls/input")
		}
		if body.Model == "primary" {
			w.WriteHeader(400)
			fmt.Fprint(w, `{"error":{"message":"model unavailable"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"review complete\"},\"finish_reason\":null}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":2}}\n\ndata: [DONE]\n\n")
	}))
	defer server.Close()
	tier := ModelTier{TierConfig{Provider: "openai", Model: "primary", BaseURL: server.URL, APIKey: "fixture-key", Fallback: &TierConfig{Model: "fallback"}}}
	var records []llm.TraceRecord
	ctx, cancel := context.WithTimeout(llm.WithTraceID(context.Background(), "auxiliary"), 5*time.Second)
	defer cancel()
	response, err := tier.complete(ctx, llm.SinkFunc(func(record llm.TraceRecord) { mu.Lock(); defer mu.Unlock(); records = append(records, record) }), agentcore.ChatRequest{MaxTokens: 256, Messages: []agentcore.Message{{Role: agentcore.RoleUser, Content: "review evidence"}}})
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if response.Message.Content != "review complete" || response.Usage.InputTokens != 3 || len(requests) != 2 || len(records) != 2 || records[0].Err == "" || records[1].Model != "fallback" || records[1].TraceID != "auxiliary" {
		t.Fatalf("completion/telemetry: %+v requests=%v records=%+v", response, requests, records)
	}
}
