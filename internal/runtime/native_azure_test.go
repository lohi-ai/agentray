package agentruntime

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/lohi-ai/agentray/ai"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestNativeAzureDefaultDispatcher(t *testing.T) {
	for _, key := range []string{"AZURE_OPENAI_BASE_URL", "AZURE_OPENAI_RESOURCE_NAME", "AZURE_OPENAI_API_VERSION", "AZURE_OPENAI_DEPLOYMENT_NAME_MAP"} {
		t.Setenv(key, "")
	}
	var requests, keys, effects atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := requests.Add(1)
		if r.URL.Path != "/v1/responses" || r.URL.Query().Get("api-version") != "scoped-version" || r.Header.Get("Api-Key") != fmt.Sprintf("refreshed-%d", n) || r.Header.Get("Authorization") != "" {
			t.Errorf("unexpected Azure request: %s", r.URL)
		}
		var payload struct {
			Model string
			Input json.RawMessage
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
			return
		}
		if payload.Model != "deployment" {
			t.Errorf("deployment not selected: %s", payload.Model)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		if n == 1 {
			writeNativeResponsesEvents(w, map[string]any{"type": "response.output_item.done", "output_index": 0, "item": map[string]any{"type": "function_call", "id": "fc_1", "call_id": "call", "name": "write", "arguments": "{}"}}, nativeResponsesTerminal("first", 3, 1))
		} else {
			if !strings.Contains(string(payload.Input), "written") || !strings.Contains(string(payload.Input), "function_call_output") {
				t.Error("tool result was not replayed")
			}
			writeNativeResponsesAnswer(w, "done", "second", 5, 2)
		}
	}))
	defer server.Close()
	raw, _ := json.Marshal(map[string]any{
		"streamMode": "native", "callbacks": []string{"getApiKey"},
		"streamOptions": map[string]any{"env": map[string]string{"AZURE_OPENAI_API_VERSION": "scoped-version", "AZURE_OPENAI_DEPLOYMENT_NAME_MAP": "test=deployment"}},
		"initialState":  map[string]any{"model": map[string]any{"id": "test", "api": "azure-openai-responses", "provider": "azure-openai-responses", "baseUrl": server.URL + "/v1", "maxTokens": 8192}, "tools": []any{map[string]any{"name": "write", "description": "Write", "parameters": map[string]any{"type": "object", "properties": map[string]any{}}}}},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var mu sync.Mutex
	traces := []json.RawMessage{}
	agent, err := NewNativeAgent(ctx, NativeAgentConfig{Options: raw, Callback: func(_ context.Context, method string, params json.RawMessage, _ func(json.RawMessage) error) (json.RawMessage, error) {
		switch method {
		case "getApiKey":
			if string(params) != `"azure-openai-responses"` {
				return nil, fmt.Errorf("wrong provider %s", params)
			}
			return json.Marshal(fmt.Sprintf("refreshed-%d", keys.Add(1)))
		case "tool":
			effects.Add(1)
			return json.RawMessage(`{"content":[{"type":"text","text":"written"}],"details":{}}`), nil
		default:
			return nil, fmt.Errorf("unexpected callback %s", method)
		}
	}, OnTrace: func(_ context.Context, raw json.RawMessage) {
		mu.Lock()
		defer mu.Unlock()
		traces = append(traces, append(json.RawMessage(nil), raw...))
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer agent.Close()
	if err := agent.Prompt(ctx, json.RawMessage(`"write"`)); err != nil {
		t.Fatal(err)
	}
	state, err := agent.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		Messages []json.RawMessage
		Error    *string
	}
	if err := json.Unmarshal(state, &result); err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 2 || keys.Load() != 2 || effects.Load() != 1 || result.Error != nil || len(result.Messages) < 4 {
		t.Fatalf("requests=%d keys=%d effects=%d state=%s", requests.Load(), keys.Load(), effects.Load(), state)
	}
	var final struct {
		API, Model, StopReason string
		Content                []struct{ Type, Text string }
	}
	if err := json.Unmarshal(result.Messages[len(result.Messages)-1], &final); err != nil {
		t.Fatal(err)
	}
	if final.API != "azure-openai-responses" || final.Model != "test" || final.StopReason != "stop" || len(final.Content) != 1 || final.Content[0].Text != "done" {
		t.Fatalf("wrong final message: %s", state)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(traces) != 2 {
		t.Fatalf("request traces=%d", len(traces))
	}
	for _, raw := range append(traces, state) {
		if strings.Contains(string(raw), "refreshed-") {
			t.Fatal("credential leaked to state or traces")
		}
	}
}

func clearNativeAzureEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{"AZURE_OPENAI_BASE_URL", "AZURE_OPENAI_RESOURCE_NAME", "AZURE_OPENAI_API_VERSION", "AZURE_OPENAI_DEPLOYMENT_NAME_MAP"} {
		t.Setenv(key, "")
	}
}
func TestNativeAzureModelTierHTTPControlsAndDurability(t *testing.T) {
	clearNativeAzureEnv(t)
	testNativeRunnerHTTPProviderControlsAndDurability(t, true, ai.VendorAzureResponses, false)
}
func TestNativeAzureInheritedByChildrenAndSummary(t *testing.T) {
	clearNativeAzureEnv(t)
	testNativeHTTPProviderInheritedByChildrenAndSummary(t, true, false, false, true, false)
}

func TestNativeAzureUnfinishedToolHasNoEffect(t *testing.T) {
	clearNativeAzureEnv(t)
	testNativeStreamFailureHasNoEffect(t, false, true, false)
}
