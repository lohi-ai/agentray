//go:build pi

package agentcore_test

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

	"github.com/lohi-ai/agentray/agentcore"
)

func piNativeModel(api, endpoint string) map[string]any {
	return map[string]any{"id": "native-test", "name": "native-test", "api": api, "provider": "test",
		"baseUrl": endpoint, "reasoning": true, "input": []string{"text"}, "contextWindow": 8192, "maxTokens": 1024,
		"cost": map[string]int{"input": 0, "output": 0, "cacheRead": 0, "cacheWrite": 0}}
}

func TestPiNativeOpenAIStreamsAndExecutesTools(t *testing.T) {
	for _, runtime := range []string{"bun", "node"} {
		t.Run(runtime, func(t *testing.T) {
			var requests, effects, keys, deltas atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer local-test-key" {
					t.Errorf("unexpected native request path/auth")
					http.Error(w, "bad request", 400)
					return
				}
				var body struct {
					Messages []struct {
						Role    string
						Content json.RawMessage
					}
					Tools []json.RawMessage
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
					http.Error(w, "bad JSON", 400)
					return
				}
				if len(body.Tools) != 1 {
					t.Errorf("native tool declarations missing: %d", len(body.Tools))
				}
				w.Header().Set("Content-Type", "text/event-stream")
				if requests.Add(1) == 1 {
					fmt.Fprint(w, "data: "+`{"id":"reply-1","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call-1","type":"function","function":{"name":"echo","arguments":"{}"}}]},"finish_reason":null}]}`+"\n\n")
					fmt.Fprint(w, "data: "+`{"id":"reply-1","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`+"\n\n")
				} else {
					last := body.Messages[len(body.Messages)-1]
					if last.Role != "tool" || !strings.Contains(string(last.Content), "native tool result") {
						t.Errorf("native tool result not replayed: %+v", last)
					}
					fmt.Fprint(w, "data: "+`{"id":"reply-2","choices":[{"index":0,"delta":{"role":"assistant","content":"native answer"},"finish_reason":null}]}`+"\n\n")
					fmt.Fprint(w, "data: "+`{"id":"reply-2","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":8,"completion_tokens":2,"total_tokens":10}}`+"\n\n")
				}
				fmt.Fprint(w, "data: [DONE]\n\n")
			}))
			defer server.Close()
			a, ctx := piStart(t, agentcore.PiConfig{Runtime: runtime, Options: piJSON(map[string]any{
				"streamMode": "native", "callbacks": []string{"getApiKey"},
				"initialState": map[string]any{"model": piNativeModel("openai-completions", server.URL+"/v1"), "tools": []any{piTool("echo")}},
			}), Callback: func(_ context.Context, method string, params json.RawMessage, _ func(json.RawMessage) error) (json.RawMessage, error) {
				switch method {
				case "getApiKey":
					keys.Add(1)
					return piJSON("local-test-key"), nil
				case "tool":
					effects.Add(1)
					return piToolResult("native tool result", false), nil
				default:
					return nil, fmt.Errorf("native mode unexpectedly invoked %s", method)
				}
			}, OnEvent: func(_ context.Context, raw json.RawMessage) error {
				if strings.Contains(string(raw), `"type":"text_delta"`) {
					deltas.Add(1)
				}
				return nil
			}})
			if err := a.Prompt(ctx, piJSON("run native")); err != nil {
				t.Fatal(err)
			}
			state, err := a.State(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if requests.Load() != 2 || effects.Load() != 1 || keys.Load() != 2 || deltas.Load() == 0 || !strings.Contains(string(state), "native answer") {
				t.Fatalf("native model/tool flow failed: requests=%d effects=%d keys=%d deltas=%d state=%s", requests.Load(), effects.Load(), keys.Load(), deltas.Load(), state)
			}
		})
	}
}

func TestPiNativeAnthropicKeepsThinkingSignature(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" || r.Header.Get("X-Api-Key") != "local-test-key" {
			t.Error("wrong native Anthropic request")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, data := range []string{
			`{"type":"message_start","message":{"id":"msg-1","type":"message","role":"assistant","model":"native-test","content":[],"stop_reason":null,"usage":{"input_tokens":3,"output_tokens":0}}}`,
			`{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"reasoning"}}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"opaque-native-signature"}}`,
			`{"type":"content_block_stop","index":0}`,
			`{"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`,
			`{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"signed answer"}}`,
			`{"type":"content_block_stop","index":1}`,
			`{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":4}}`,
			`{"type":"message_stop"}`,
		} {
			var header struct{ Type string }
			_ = json.Unmarshal([]byte(data), &header)
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", header.Type, data)
		}
	}))
	defer server.Close()
	a, ctx := piStart(t, agentcore.PiConfig{Options: piJSON(map[string]any{
		"streamMode": "native", "callbacks": []string{"getApiKey"},
		"initialState": map[string]any{"model": piNativeModel("anthropic-messages", server.URL)},
	}), Callback: func(_ context.Context, method string, _ json.RawMessage, _ func(json.RawMessage) error) (json.RawMessage, error) {
		if method != "getApiKey" {
			return nil, fmt.Errorf("unexpected %s", method)
		}
		return piJSON("local-test-key"), nil
	}})
	if err := a.Prompt(ctx, piJSON("think")); err != nil {
		t.Fatal(err)
	}
	state, err := a.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(state), `"thinkingSignature":"opaque-native-signature"`) || !strings.Contains(string(state), "signed answer") {
		t.Fatalf("native reasoning signature lost: %s", state)
	}
}

func TestPiNativeCancellationAbortsHTTPRequestAndAllowsReuse(t *testing.T) {
	started, stopped := make(chan struct{}), make(chan struct{})
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		if requests.Add(1) == 1 {
			fmt.Fprint(w, "data: "+`{"id":"pending","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`+"\n\n")
			w.(http.Flusher).Flush()
			close(started)
			<-r.Context().Done()
			close(stopped)
			return
		}
		fmt.Fprint(w, "data: "+`{"id":"reused","choices":[{"index":0,"delta":{"role":"assistant","content":"reused"},"finish_reason":null}]}`+"\n\n")
		fmt.Fprint(w, "data: "+`{"id":"reused","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`+"\n\ndata: [DONE]\n\n")
	}))
	defer server.Close()
	a, ctx := piStart(t, agentcore.PiConfig{Options: piJSON(map[string]any{
		"streamMode": "native", "callbacks": []string{"getApiKey"},
		"initialState": map[string]any{"model": piNativeModel("openai-completions", server.URL+"/v1")},
	}), Callback: func(_ context.Context, method string, _ json.RawMessage, _ func(json.RawMessage) error) (json.RawMessage, error) {
		if method != "getApiKey" {
			return nil, fmt.Errorf("unexpected %s", method)
		}
		return piJSON("local-test-key"), nil
	}})
	defer a.Close()
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- a.Prompt(runCtx, piJSON("cancel")) }()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel result: %v", err)
	}
	select {
	case <-stopped:
	case <-ctx.Done():
		t.Fatal("native request outlived cancellation")
	}
	if _, err := a.Call(ctx, "waitForIdle", nil); err != nil {
		t.Fatal(err)
	}
	state, _ := a.State(ctx)
	if !strings.Contains(string(state), `"stopReason":"aborted"`) {
		t.Fatalf("missing native aborted message: %s", state)
	}
	if err := a.Prompt(ctx, piJSON("again")); err != nil {
		t.Fatal(err)
	}
	state, _ = a.State(ctx)
	if requests.Load() != 2 || !strings.Contains(string(state), `"text":"reused"`) {
		t.Fatalf("native agent not reusable: %s", state)
	}
}
