package agentruntime

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/2found/2ai/agentcore"
)

func TestPiRunnerDispatchUsesNativeProviderAndGovernedTools(t *testing.T) {
	ctx := piSessionContext(t)
	var calls, effects atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer native-key" {
			t.Error("runner lost resolved model binding")
		}
		var body struct{ Messages []json.RawMessage }
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if !strings.Contains(string(piSessionJSON(body.Messages)), "prior native user") {
			t.Error("native history lost at runner boundary")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		if calls.Add(1) == 1 {
			fmt.Fprint(w, "data: "+`{"id":"first","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call","type":"function","function":{"name":"write","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`+"\n\n")
		} else {
			fmt.Fprint(w, "data: "+`{"id":"second","choices":[{"index":0,"delta":{"role":"assistant","content":"native runner answer"},"finish_reason":"stop"}],"usage":{"prompt_tokens":9,"completion_tokens":2,"total_tokens":11}}`+"\n\n")
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer server.Close()
	p := representativeBuildParams()
	p.Sandbox, p.HTTPTool, p.Subagents = nil, nil, nil
	p.Goal, p.PrepareNextTurn, p.RefreshKey = "", nil, nil
	p.Tools = []agentcore.Tool{piComposedTool{&effects}}
	r := NewRunner(nil)
	tier := ModelTier{TierConfig: TierConfig{Provider: "openai", Model: "runner-native", BaseURL: server.URL + "/v1", APIKey: "native-key"}}
	var tokens string
	result, err := r.runModelLoop(ctx, p, RunOptions{Prompt: "execute", NativeHistory: json.RawMessage(`[{"role":"user","content":"prior native user","timestamp":1,"extension":{"preserve":true}}]`)}, tier, func(event agentcore.StreamEvent) {
		if event.Type == agentcore.StreamToken {
			tokens += event.Token
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 || effects.Load() != 1 || result.Final != "native runner answer" || tokens != result.Final {
		t.Fatalf("runner did not execute native loop: calls=%d effects=%d result=%+v tokens=%s", calls.Load(), effects.Load(), result, tokens)
	}
	if len(result.NativeTelemetry) == 0 || !strings.Contains(string(result.NativeState), `"extension":{"preserve":true}`) {
		t.Fatal("runner discarded native artifacts")
	}
	if result.Usage.InputTokens != 9 || result.Usage.OutputTokens != 2 || !result.Usage.CostUnpriced {
		t.Fatalf("runner lost native usage: %+v", result.Usage)
	}
	entries, err := p.Session.Log(ctx, p.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := recoverPiState(entries); err != nil {
		t.Fatal(err)
	}
	lease, release, err := agentcore.AcquireSessionLease(context.Background(), p.Session, p.SessionID)
	if err != nil || lease.Err() != nil {
		t.Fatalf("runner leaked session lease: %v", err)
	}
	_ = release()
}

func TestPiRunnerControlsAndSchemaValidationWithNativeProvider(t *testing.T) {
	for _, tc := range []struct {
		name, answer           string
		unsupported, wantError bool
	}{
		{"valid", `{"ok":true}`, false, false},
		{"wrong type", `{"ok":"wrong"}`, false, true},
		{"invalid JSON", `not JSON`, false, true},
		{"unsupported hint still validates", `{"ok":false}`, true, false},
		{"unsupported hint cannot accept bad output", `{"ok":"wrong"}`, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := piSessionContext(t)
			var requests, effects atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				n := requests.Add(1)
				var payload map[string]json.RawMessage
				if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
					t.Error(err)
				}
				if n == 1 {
					if string(payload["tool_choice"]) != `"required"` || string(payload["parallel_tool_calls"]) != "false" || len(payload["tools"]) == 0 {
						t.Errorf("lost generation controls: %s", piSessionJSON(payload))
					}
				} else if len(payload["tool_choice"]) != 0 || len(payload["parallel_tool_calls"]) != 0 || (len(payload["tools"]) != 0 && string(payload["tools"]) != "[]") {
					t.Errorf("forced tools escaped into ceiling wrap: %s", piSessionJSON(payload))
				}
				if (len(payload["response_format"]) == 0) != tc.unsupported {
					t.Errorf("schema capability not honored: %s", piSessionJSON(payload))
				}
				w.Header().Set("Content-Type", "text/event-stream")
				if n == 1 {
					fmt.Fprint(w, "data: "+`{"id":"tool","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call","type":"function","function":{"name":"write","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`+"\n\n")
				} else {
					fmt.Fprintf(w, "data: {\"id\":\"text\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":%s},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":3,\"total_tokens\":10}}\n\n", piSessionJSON(tc.answer))
				}
				fmt.Fprint(w, "data: [DONE]\n\n")
			}))
			defer server.Close()
			p := representativeBuildParams()
			p.Sandbox, p.HTTPTool, p.Subagents = nil, nil, nil
			p.Goal, p.PrepareNextTurn, p.RefreshKey = "", nil, nil
			p.Tools = []agentcore.Tool{piComposedTool{&effects}}
			p.MaxTurns = 1
			p.ToolChoice = agentcore.ToolChoice{Mode: agentcore.ToolChoiceRequired}
			parallel := false
			p.ParallelToolCalls = &parallel
			p.OutputSchema = &agentcore.OutputSchema{Name: "answer", Strict: true, Schema: map[string]any{
				"type": "object", "properties": map[string]any{"ok": map[string]any{"type": "boolean"}}, "required": []string{"ok"}, "additionalProperties": false,
			}}
			tier := ModelTier{TierConfig: TierConfig{Provider: "openai", Model: "controls", BaseURL: server.URL + "/v1", APIKey: "test"}}
			if tc.unsupported {
				tier.Capabilities.StructuredOutput = agentcore.CapabilityUnsupported
			}
			r := NewRunner(nil, WithPiRuntime(PiRuntimeConfig{}))
			result, err := r.runModelLoop(ctx, p, RunOptions{Prompt: "write and return JSON", ToolChoice: p.ToolChoice, ParallelToolCalls: &parallel}, tier, nil)
			if tc.wantError {
				if err == nil || !strings.Contains(err.Error(), "structured output") || result.Final != "" || result.StopReason != "error" {
					t.Fatalf("accepted invalid output: %+v %v", result, err)
				}
			} else if err != nil || result.Final != tc.answer || result.StopReason != "max_turns" {
				t.Fatalf("valid structured wrap rejected: %+v %v", result, err)
			}
			if requests.Load() != 2 || effects.Load() != 1 || result.Usage.InputTokens != 7 || result.Usage.OutputTokens != 3 {
				t.Fatalf("schema failure lost billed usage or repeated effects: calls=%d effects=%d usage=%+v", requests.Load(), effects.Load(), result.Usage)
			}
			entries, err := p.Session.Log(ctx, p.SessionID)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := recoverPiState(entries); err != nil {
				t.Fatalf("structured failure damaged native recovery: %v", err)
			}
			if !strings.Contains(string(result.NativeState), string(piSessionJSON(tc.answer))) {
				t.Fatal("native history discarded original answer")
			}
		})
	}
}

func TestPiRunnerInvalidControlsFailBeforeProviderIO(t *testing.T) {
	for _, tc := range []string{"named tool", "invalid schema"} {
		t.Run(tc, func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				http.Error(w, "unexpected I/O", 400)
			}))
			defer server.Close()
			p := representativeBuildParams()
			p.Sandbox, p.HTTPTool, p.Subagents = nil, nil, nil
			p.Goal, p.PrepareNextTurn, p.RefreshKey = "", nil, nil
			if tc == "named tool" {
				p.ToolChoice = agentcore.ToolChoice{Mode: agentcore.ToolChoiceNamed, Name: "not_advertised"}
			} else {
				p.OutputSchema = &agentcore.OutputSchema{Schema: map[string]any{"type": "invalid"}}
			}
			r := NewRunner(nil, WithPiRuntime(PiRuntimeConfig{}))
			tier := ModelTier{TierConfig: TierConfig{Provider: "openai", Model: "controls", BaseURL: server.URL + "/v1", APIKey: "test"}}
			_, err := r.runModelLoop(piSessionContext(t), p, RunOptions{Prompt: "test"}, tier, nil)
			if err == nil || requests.Load() != 0 {
				t.Fatalf("invalid controls reached provider: %v requests=%d", err, requests.Load())
			}
		})
	}
}

func TestPiRunnerNativeStructuredOutputDialects(t *testing.T) {
	for _, provider := range []string{"openai-responses", "anthropic"} {
		t.Run(provider, func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				var payload map[string]json.RawMessage
				if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
					t.Error(err)
				}
				key, path, choice := "text", "/v1/responses", `"none"`
				if provider == "anthropic" {
					key, path, choice = "output_config", "/v1/messages", `{"type":"none"}`
				}
				if r.URL.Path != path || string(payload["tool_choice"]) != choice || !strings.Contains(string(payload[key]), `"format":{"`) || !strings.Contains(string(payload[key]), `"type":"json_schema"`) {
					t.Errorf("wrong native dialect %s: %s", r.URL.Path, piSessionJSON(payload))
				}
				w.Header().Set("Content-Type", "text/event-stream")
				event := func(raw string) {
					var value struct{ Type string }
					_ = json.Unmarshal([]byte(raw), &value)
					fmt.Fprintf(w, "event: %s\ndata: %s\n\n", value.Type, raw)
				}
				if provider == "anthropic" {
					event(`{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"controls","content":[],"usage":{"input_tokens":4,"output_tokens":0}}}`)
					event(`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`)
					event(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"{\"ok\":true}"}}`)
					event(`{"type":"content_block_stop","index":0}`)
					event(`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}`)
					event(`{"type":"message_stop"}`)
				} else {
					event(`{"type":"response.created","response":{"id":"resp_1"}}`)
					event(`{"type":"response.output_item.added","output_index":0,"item":{"type":"message","id":"msg_1","role":"assistant","content":[]}}`)
					event(`{"type":"response.output_text.delta","output_index":0,"delta":"{\"ok\":true}"}`)
					event(`{"type":"response.output_item.done","output_index":0,"item":{"type":"message","id":"msg_1","role":"assistant","content":[{"type":"output_text","text":"{\"ok\":true}"}]}}`)
					event(`{"type":"response.completed","response":{"id":"resp_1","status":"completed","usage":{"input_tokens":4,"output_tokens":2,"total_tokens":6}}}`)
				}
			}))
			defer server.Close()
			p := representativeBuildParams()
			p.Sandbox, p.HTTPTool, p.Subagents = nil, nil, nil
			p.Goal, p.PrepareNextTurn, p.RefreshKey = "", nil, nil
			p.ToolChoice = agentcore.ToolChoice{Mode: agentcore.ToolChoiceNone}
			p.OutputSchema = &agentcore.OutputSchema{Schema: map[string]any{"type": "object"}}
			tier := ModelTier{TierConfig: TierConfig{Provider: provider, Model: "controls", BaseURL: server.URL + "/v1", APIKey: "test"}}
			if provider == "anthropic" {
				tier.BaseURL = server.URL
			}
			r := NewRunner(nil, WithPiRuntime(PiRuntimeConfig{}))
			result, err := r.runModelLoop(piSessionContext(t), p, RunOptions{Prompt: "return JSON"}, tier, nil)
			if err != nil || result.Final != `{"ok":true}` || requests.Load() != 1 || result.Usage.InputTokens != 4 || result.Usage.OutputTokens != 2 {
				t.Fatalf("native structured %s: final=%q stop=%s usage=%+v err=%v (requests=%d)", provider, result.Final, result.StopReason, result.Usage, err, requests.Load())
			}
		})
	}
}
