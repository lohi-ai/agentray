package agentruntime

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lohi-ai/agentray/agentcore"
	"github.com/lohi-ai/agentray/agentcore/plugins/observe"
	"github.com/lohi-ai/agentray/agentcore/plugins/subagent"
	"github.com/lohi-ai/agentray/ai"
)

func TestNativeRunnerDefaultHTTPProviderControlsAndDurability(t *testing.T) {
	testNativeRunnerHTTPProviderControlsAndDurability(t, false, "openai", false)
}
func TestNativeResponsesHTTPProviderControlsAndDurability(t *testing.T) {
	for _, provider := range []string{"openai", ai.VendorOpenAIResponses} {
		t.Run(provider, func(t *testing.T) { testNativeRunnerHTTPProviderControlsAndDurability(t, true, provider, false) })
	}
}
func TestNativeAnthropicHTTPProviderControlsAndDurability(t *testing.T) {
	testNativeRunnerHTTPProviderControlsAndDurability(t, false, "anthropic", false)
}
func TestNativeAnthropicFederationHTTPProviderControlsAndDurability(t *testing.T) {
	testNativeRunnerHTTPProviderControlsAndDurability(t, false, "anthropic", true)
}

func TestNativeAnthropicFederationScopedBinding(t *testing.T) {
	for _, key := range []string{"ANTHROPIC_FEDERATION_RULE_ID", "ANTHROPIC_ORGANIZATION_ID", "ANTHROPIC_IDENTITY_TOKEN_FILE", "ANTHROPIC_CUSTOM_HEADERS"} {
		t.Setenv(key, "")
	}
	identity := filepath.Join(t.TempDir(), "identity")
	if err := os.WriteFile(identity, []byte("native-fixture-assertion"), 0600); err != nil {
		t.Fatal(err)
	}
	var exchanges, requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if serveNativeFederationExchange(t, w, r, &exchanges) {
			return
		}
		requests.Add(1)
		if r.Header.Get("Authorization") != "Bearer native-federated-access" {
			t.Error("scoped federation lost auth")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		writeNativeAnthropicAnswer(w, "scoped answer", 1, 1)
	}))
	defer server.Close()
	options := piModelJSON(map[string]any{"streamOptions": map[string]any{"env": map[string]string{"ANTHROPIC_FEDERATION_RULE_ID": "native-rule", "ANTHROPIC_ORGANIZATION_ID": "native-organization", "ANTHROPIC_IDENTITY_TOKEN_FILE": identity}}})
	tier := ModelTier{TierConfig: TierConfig{Provider: "anthropic", Model: "native-http", BaseURL: server.URL}}
	binding, _, err := tier.BindPi(agentcore.PiConfig{Options: options}, PiModelOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	agent, err := NewNativeAgent(ctx, NativeAgentConfig{Options: binding.Options, Callback: binding.Callback})
	if err != nil {
		t.Fatal(err)
	}
	defer agent.Close()
	if err := agent.Prompt(ctx, json.RawMessage(`"hello"`)); err != nil {
		t.Fatal(err)
	}
	state, err := agent.State(ctx)
	if err != nil || !strings.Contains(string(state), "scoped answer") || exchanges.Load() != 1 || requests.Load() != 1 {
		t.Fatalf("scoped federation: %s exchanges=%d requests=%d err=%v", state, exchanges.Load(), requests.Load(), err)
	}
}
func testNativeRunnerHTTPProviderControlsAndDurability(t *testing.T, responses bool, provider string, federated bool) {
	claudePool := provider == ai.VendorClaudeCode
	anthropic := provider == "anthropic" || claudePool
	azure := provider == ai.VendorAzureResponses
	piMessages := provider == "radius" || provider == ai.VendorPiMessages
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var requests, effects, refreshes atomic.Int32
	var exchanges atomic.Int32
	if federated {
		configureNativeFederationEnv(t)
	}
	var traceMu sync.Mutex
	traces := []observe.TraceRecord{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if federated && serveNativeFederationExchange(t, w, r, &exchanges) {
			return
		}
		n := requests.Add(1)
		path := "/v1/chat/completions"
		if piMessages {
			path = "/v1/messages"
		}
		if responses {
			path = "/v1/responses"
		}
		header, credential := "Authorization", fmt.Sprintf("Bearer refreshed-%d", n)
		if azure {
			header, credential = "Api-Key", fmt.Sprintf("refreshed-%d", n)
			if r.URL.Query().Get("api-version") != "v1" {
				t.Error("Azure API version missing")
			}
		}
		if anthropic {
			path, header, credential = "/v1/messages", "X-Api-Key", fmt.Sprintf("refreshed-%d", n)
		}
		if claudePool {
			header, credential = "Authorization", fmt.Sprintf("Bearer refreshed-%d", n)
			if r.Header.Get("X-Api-Key") != "" {
				t.Error("pooled Claude sent API-key auth")
			}
		}
		if federated {
			header, credential = "Authorization", "Bearer native-federated-access"
			if r.Header.Get("X-Api-Key") != "" || !strings.Contains(r.Header.Get("Anthropic-Beta"), "oauth-2025-04-20") {
				t.Error("wrong federation auth headers")
			}
		}
		if r.URL.Path != path || r.Header.Get(header) != credential {
			t.Errorf("wrong model/credential: %s %s", r.URL.Path, r.Header.Get("Authorization"))
		}
		var payload map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
			return
		}
		historyKey, formatKey := "messages", "response_format"
		var gatewayOptions map[string]json.RawMessage
		if piMessages {
			historyKey = "context"
			_ = json.Unmarshal(payload["options"], &gatewayOptions)
		}
		if responses {
			historyKey, formatKey = "input", "text"
		}
		if anthropic {
			formatKey = "output_config"
		}
		if !strings.Contains(string(payload[historyKey]), "prior native user") {
			t.Error("lost native history")
		}
		effort := payload["reasoning_effort"]
		if piMessages {
			effort = gatewayOptions["reasoning"]
		}
		if responses {
			var reasoning map[string]json.RawMessage
			_ = json.Unmarshal(payload["reasoning"], &reasoning)
			effort = reasoning["effort"]
		}
		if anthropic {
			var thinking struct {
				Type   string
				Budget int `json:"budget_tokens"`
			}
			_ = json.Unmarshal(payload["thinking"], &thinking)
			var ceiling int
			_ = json.Unmarshal(payload["max_tokens"], &ceiling)
			if thinking.Type != "enabled" || thinking.Budget != ceiling-1024 {
				t.Errorf("thinking budget not clamped: %s cap=%d", payload["thinking"], ceiling)
			}
		} else if piMessages && string(effort) != `"xhigh"` {
			t.Errorf("gateway reasoning changed: %s", effort)
		} else if !piMessages && string(effort) != `"high"` {
			t.Errorf("simple thinking clamp missing: %s", effort)
		}
		if !piMessages && len(payload[formatKey]) == 0 {
			t.Error("onPayload lost structured-output control")
		}
		if n == 1 {
			if anthropic {
				var choice struct {
					Type    string
					Disable bool `json:"disable_parallel_tool_use"`
				}
				_ = json.Unmarshal(payload["tool_choice"], &choice)
				if choice.Type != "any" || !choice.Disable {
					t.Errorf("lost Anthropic tool controls: %s", payload["tool_choice"])
				}
			} else if piMessages {
				if string(gatewayOptions["toolChoice"]) != `"required"` {
					t.Errorf("gateway tool choice lost: %s", payload["options"])
				}
			} else if string(payload["tool_choice"]) != `"required"` || string(payload["parallel_tool_calls"]) != "false" {
				t.Error("onPayload lost tool controls")
			}
		} else {
			if len(payload["tool_choice"]) > 0 || len(payload["parallel_tool_calls"]) > 0 || len(gatewayOptions["toolChoice"]) > 0 {
				t.Error("tool-free ceiling wrap retained forced controls")
			}
			signature := `"reasoning_content":"private reasoning"`
			if piMessages {
				signature = `"thinkingSignature":"opaque"`
			}
			if responses {
				signature = `"encrypted_content":"opaque"`
			}
			if anthropic {
				signature = `"signature":"opaque"`
			}
			if !strings.Contains(string(payload[historyKey]), signature) {
				t.Errorf("reasoning replay lost: %s", payload[historyKey])
			}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		if piMessages {
			if n == 1 {
				writeNativePiMessagesEvents(w,
					map[string]any{"type": "thinking_start", "contentIndex": 0},
					map[string]any{"type": "thinking_end", "contentIndex": 0, "content": "private reasoning", "contentSignature": "opaque"},
					map[string]any{"type": "toolcall_start", "contentIndex": 1, "id": "call", "toolName": "write"},
					map[string]any{"type": "toolcall_end", "contentIndex": 1, "toolCall": map[string]any{"type": "toolCall", "id": "call", "name": "write", "arguments": map[string]any{}}},
					nativePiMessagesTerminal("toolUse", 3, 1))
			} else {
				writeNativePiMessagesAnswer(w, `{"ok":true}`, 9, 2)
			}
			return
		}
		if anthropic {
			if n == 1 {
				writeNativeAnthropicEvents(w,
					map[string]any{"type": "message_start", "message": map[string]any{"id": "first", "model": "native-http", "usage": map[string]int{"input_tokens": 3, "output_tokens": 1}}},
					map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "thinking", "thinking": "private reasoning", "signature": "opaque"}},
					map[string]any{"type": "content_block_stop", "index": 0},
					map[string]any{"type": "content_block_start", "index": 1, "content_block": map[string]any{"type": "tool_use", "id": "call", "name": "write", "input": map[string]any{}}},
					map[string]any{"type": "content_block_stop", "index": 1},
					map[string]any{"type": "message_delta", "delta": map[string]string{"stop_reason": "tool_use"}},
				)
			} else {
				writeNativeAnthropicAnswer(w, `{"ok":true}`, 9, 2)
			}
			return
		}
		if responses {
			if n == 1 {
				writeNativeResponsesEvents(w,
					map[string]any{"type": "response.output_item.done", "output_index": 0, "item": map[string]any{"type": "reasoning", "id": "rs_1", "summary": []any{map[string]any{"type": "summary_text", "text": "private reasoning"}}, "encrypted_content": "opaque"}},
					map[string]any{"type": "response.output_item.done", "output_index": 1, "item": map[string]any{"type": "function_call", "id": "fc_1", "call_id": "call", "name": "write", "arguments": "{}"}},
					nativeResponsesTerminal("first", 3, 1),
				)
			} else {
				writeNativeResponsesAnswer(w, `{"ok":true}`, "second", 9, 2)
			}
			return
		}
		if n == 1 {
			fmt.Fprint(w, "data: "+`{"id":"first","choices":[{"delta":{"reasoning_content":"private reasoning","tool_calls":[{"index":0,"id":"call","function":{"name":"write","arguments":"{}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":3,"completion_tokens":1}}`+"\n\n")
		} else {
			fmt.Fprint(w, "data: "+`{"id":"second","choices":[{"delta":{"content":"{\"ok\":true}"},"finish_reason":"stop"}],"usage":{"prompt_tokens":9,"completion_tokens":2}}`+"\n\n")
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer server.Close()
	p := representativeBuildParams()
	p.Sandbox, p.HTTPTool, p.Subagents = nil, nil, nil
	p.Goal, p.PrepareNextTurn = "", nil
	p.Tools = []agentcore.Tool{nativeChildWrite{&effects}}
	p.MaxTurns = 1
	p.ToolChoice = agentcore.ToolChoice{Mode: agentcore.ToolChoiceRequired}
	parallel := false
	p.ParallelToolCalls = &parallel
	p.OutputSchema = &agentcore.OutputSchema{Name: "answer", Strict: true, Schema: map[string]any{"type": "object", "properties": map[string]any{"ok": map[string]any{"type": "boolean"}}, "required": []string{"ok"}, "additionalProperties": false}}
	if piMessages {
		p.ParallelToolCalls, p.OutputSchema = nil, nil
	}
	p.RefreshKey = func(context.Context, string) (string, error) {
		return fmt.Sprintf("refreshed-%d", refreshes.Add(1)), nil
	}
	if federated {
		p.RefreshKey = nil
	}
	if claudePool {
		p.RefreshKey = func(context.Context, string) (string, error) {
			t.Error("pool used static key refresh")
			return "stale-key", nil
		}
	}
	p.Tracer = observe.SinkFunc(func(trace observe.TraceRecord) {
		traceMu.Lock()
		defer traceMu.Unlock()
		traces = append(traces, trace)
	})
	runner := NewRunner(nil, WithPiRuntime(PiRuntimeConfig{}))
	tier := ModelTier{TierConfig: TierConfig{Provider: provider, Model: "native-http", BaseURL: server.URL + "/v1", APIKey: "stale-key"}}
	if anthropic {
		tier.BaseURL = server.URL
	}
	if claudePool {
		tier.TokenSource = &nativeClaudeAccountSource{acquired: &refreshes}
	}
	if federated {
		tier.APIKey = ""
	}
	if responses {
		tier.Capabilities.StatefulResponses = agentcore.CapabilitySupported
	}
	if provider == ai.VendorOpenAIResponses {
		tier.BaseURL += "/responses"
	}
	tokens := ""
	result, err := runner.runModelLoop(ctx, p, RunOptions{Prompt: "write", ReasoningEffort: "xhigh", NativeHistory: json.RawMessage(`[{"role":"user","content":"prior native user","timestamp":1,"extension":{"opaque":"keep"}}]`)}, tier, func(event agentcore.StreamEvent) {
		if event.Type == agentcore.StreamToken {
			tokens += event.Token
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	wantRefreshes := int32(2)
	if federated {
		wantRefreshes = 0
		if exchanges.Load() != 1 {
			t.Errorf("federation exchange count: %d", exchanges.Load())
		}
	}
	if result.Final != `{"ok":true}` || tokens != result.Final || result.StopReason != "max_turns" || requests.Load() != 2 || effects.Load() != 1 || refreshes.Load() != wantRefreshes {
		t.Fatalf("native HTTP run: final=%q stop=%s requests=%d effects=%d refreshes=%d tokens=%q", result.Final, result.StopReason, requests.Load(), effects.Load(), refreshes.Load(), tokens)
	}
	if result.Usage.InputTokens != 12 || result.Usage.OutputTokens != 3 {
		t.Fatalf("usage lost: %+v", result.Usage)
	}
	if !strings.Contains(string(result.NativeState), `"extension":{"opaque":"keep"}`) || !strings.Contains(string(result.NativeState), "private reasoning") || len(result.NativeTelemetry) == 0 {
		t.Fatal("native artifacts lost")
	}
	if (piMessages || claudePool) && strings.Contains(string(result.NativeState)+string(result.NativeTelemetry), "refreshed-") {
		t.Fatal("gateway credential leaked into artifacts")
	}
	if federated && (strings.Contains(string(result.NativeState)+string(result.NativeTelemetry), "native-federated-access") || strings.Contains(string(result.NativeState)+string(result.NativeTelemetry), "native-fixture-assertion")) {
		t.Fatal("federation credential leaked into artifacts")
	}
	entries, err := p.Session.Log(ctx, p.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	starts, done := 0, 0
	for _, entry := range entries {
		if entry.Kind == piEffectStart {
			starts++
		}
		if entry.Kind == piEffectDone {
			done++
		}
	}
	if starts != 1 || done != 1 {
		t.Fatalf("effect journal %d/%d", starts, done)
	}
	if _, err := recoverPiState(entries); err != nil {
		t.Fatal(err)
	}
	lease, release, err := agentcore.AcquireSessionLease(ctx, p.Session, p.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if lease.Err() != nil {
		t.Fatal(lease.Err())
	}
	_ = release()
	traceMu.Lock()
	defer traceMu.Unlock()
	if len(traces) != 2 {
		t.Fatalf("trace count: %d", len(traces))
	}
	for _, trace := range traces {
		if trace.SessionKey != p.SessionID {
			t.Fatalf("trace attribution lost: %+v", trace)
		}
	}
}

func TestNativeDefaultHTTPProviderInheritedByChildrenAndSummary(t *testing.T) {
	testNativeHTTPProviderInheritedByChildrenAndSummary(t, false, false, false, false, false)
}
func TestNativeResponsesHTTPProviderInheritedByChildrenAndSummary(t *testing.T) {
	testNativeHTTPProviderInheritedByChildrenAndSummary(t, true, false, false, false, false)
}
func TestNativeAnthropicHTTPProviderInheritedByChildrenAndSummary(t *testing.T) {
	testNativeHTTPProviderInheritedByChildrenAndSummary(t, false, true, false, false, false)
}
func TestNativeAnthropicFederationInheritedByChildrenAndSummary(t *testing.T) {
	testNativeHTTPProviderInheritedByChildrenAndSummary(t, false, true, true, false, false)
}
func testNativeHTTPProviderInheritedByChildrenAndSummary(t *testing.T, responses, anthropic, federated, azure, piMessages bool, pooledProvider ...string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	gemini := len(pooledProvider) > 0 && pooledProvider[0] == "google"
	claudePool := len(pooledProvider) > 0 && pooledProvider[0] == ai.VendorClaudeCode
	antigravity := len(pooledProvider) > 0 && pooledProvider[0] == ai.VendorGoogleAntigravity
	var parentCalls, childCalls, effects, summaryCalls, exchanges, acquisitions atomic.Int32
	if federated {
		configureNativeFederationEnv(t)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if federated && serveNativeFederationExchange(t, w, r, &exchanges) {
			return
		}
		if (claudePool || antigravity) && (!strings.HasPrefix(r.Header.Get("Authorization"), "Bearer refreshed-") || r.Header.Get("X-Api-Key") != "") {
			t.Error("child/summary lost Claude pool auth")
		}
		if federated && r.Header.Get("Authorization") != "Bearer native-federated-access" {
			t.Error("child/summary lost federation auth")
		}
		var body struct {
			Project string
			Request struct {
				Contents          []json.RawMessage
				SystemInstruction json.RawMessage
				Tools             []struct{ FunctionDeclarations []struct{ Name string } }
			}
			Context ai.Context
			Options struct {
				SessionID  string
				ToolChoice json.RawMessage
			}
			Messages []json.RawMessage
			Input    []json.RawMessage
			System   json.RawMessage
			Tools    []struct {
				Name     string
				Function struct{ Name string }
			}
			ToolChoice     json.RawMessage `json:"tool_choice"`
			PromptCacheKey string          `json:"prompt_cache_key"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			return
		}
		path := "/v1/chat/completions"
		if antigravity {
			path = "/v1internal:streamGenerateContent"
			if body.Project != "native-project" {
				t.Error("project missing")
			}
			body.Messages, body.System = body.Request.Contents, body.Request.SystemInstruction
			for _, tool := range body.Request.Tools {
				for _, decl := range tool.FunctionDeclarations {
					body.Tools = append(body.Tools, struct {
						Name     string
						Function struct{ Name string }
					}{Name: decl.Name})
				}
			}
		}
		if piMessages {
			path = "/v1/messages"
			body.Messages = nil
			for _, message := range body.Context.Messages {
				body.Messages = append(body.Messages, passiveNativeJSON(message))
			}
			body.ToolChoice = body.Options.ToolChoice
			for _, tool := range ai.GetCurrentTools(body.Context.Messages) {
				body.Tools = append(body.Tools, struct {
					Name     string
					Function struct{ Name string }
				}{Name: tool.Name})
			}
		}
		if responses {
			path = "/v1/responses"
			body.Messages = body.Input
		}
		if anthropic {
			path = "/v1/messages"
		}
		if r.URL.Path != path {
			t.Errorf("wrong API endpoint: %s", r.URL.Path)
		}
		if azure && (r.Header.Get("Api-Key") != "key" || r.Header.Get("Authorization") != "" || r.URL.Query().Get("api-version") != "v1") {
			t.Error("Azure child/summary lost auth or API version")
		}
		messages := string(passiveNativeJSON(body.Messages))
		delta := map[string]any{}
		finish := "stop"
		call := func(name, args string) {
			finish = "tool_calls"
			delta["tool_calls"] = []any{map[string]any{"index": 0, "id": "call", "function": map[string]any{"name": name, "arguments": args}}}
		}
		if strings.Contains(messages+string(body.System), "Output only the updated summary") {
			summaryCalls.Add(1)
			if len(body.Tools) > 0 || len(body.ToolChoice) > 0 {
				t.Error("summary received executable tools")
			}
			delta["content"] = "native summary"
		} else if strings.Contains(messages, "PARENT-ONLY") {
			if parentCalls.Add(1) == 1 {
				call(subagent.ToolSpawnSubagent, `{"task":"isolated-child-task"}`)
			} else {
				delta["content"] = "parent answer"
			}
		} else {
			if !strings.Contains(messages, "isolated-child-task") || strings.Contains(messages, "parent private history") {
				t.Errorf("child context leak: %s", messages)
			}
			for _, tool := range body.Tools {
				if tool.Function.Name == subagent.ToolSpawnSubagent || tool.Name == subagent.ToolSpawnSubagent {
					t.Error("child kept delegation beyond depth limit")
				}
			}
			header := "X-Session-Id"
			if responses {
				header = "Session_id"
			}
			if !gemini && !antigravity && !anthropic && !azure && !piMessages && r.Header.Get(header) == "" {
				t.Error("child lost provider session affinity")
			}
			if piMessages && body.Options.SessionID == "" {
				t.Error("gateway child lost session affinity")
			}
			if azure && body.PromptCacheKey == "" {
				t.Error("Azure child lost prompt cache key")
			}
			if childCalls.Add(1) == 1 {
				call("write", `{}`)
			} else {
				delta["content"] = "child answer"
			}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		if antigravity {
			part := map[string]any{}
			if calls, ok := delta["tool_calls"].([]any); ok {
				function := calls[0].(map[string]any)["function"].(map[string]any)
				part["functionCall"] = map[string]any{"id": "call", "name": function["name"], "args": json.RawMessage(function["arguments"].(string))}
			} else {
				part["text"] = delta["content"]
			}
			writeNativeAntigravity(w, []map[string]any{part}, 1, 1)
			return
		}
		if piMessages {
			if calls, ok := delta["tool_calls"].([]any); ok {
				function := calls[0].(map[string]any)["function"].(map[string]any)
				writeNativePiMessagesCall(w, function["name"].(string), function["arguments"].(string), 1, 1)
			} else {
				writeNativePiMessagesAnswer(w, delta["content"].(string), 1, 1)
			}
			return
		}
		if anthropic {
			if calls, ok := delta["tool_calls"].([]any); ok {
				function := calls[0].(map[string]any)["function"].(map[string]any)
				writeNativeAnthropicEvents(w,
					map[string]any{"type": "message_start", "message": map[string]any{"id": "response", "model": "native-http", "usage": map[string]int{"input_tokens": 1, "output_tokens": 1}}},
					map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "tool_use", "id": "call", "name": function["name"], "input": map[string]any{}}},
					map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "input_json_delta", "partial_json": function["arguments"]}},
					map[string]any{"type": "content_block_stop", "index": 0},
					map[string]any{"type": "message_delta", "delta": map[string]string{"stop_reason": "tool_use"}},
				)
			} else {
				writeNativeAnthropicAnswer(w, delta["content"].(string), 1, 1)
			}
			return
		}
		if responses {
			if calls, ok := delta["tool_calls"].([]any); ok {
				function := calls[0].(map[string]any)["function"].(map[string]any)
				writeNativeResponsesEvents(w, map[string]any{"type": "response.output_item.done", "output_index": 0, "item": map[string]any{"type": "function_call", "id": "fc_1", "call_id": "call", "name": function["name"], "arguments": function["arguments"]}}, nativeResponsesTerminal("response", 1, 1))
			} else {
				writeNativeResponsesAnswer(w, delta["content"].(string), "response", 1, 1)
			}
			return
		}
		chunk := map[string]any{"id": "response", "choices": []any{map[string]any{"delta": delta, "finish_reason": finish}}, "usage": map[string]int{"prompt_tokens": 1, "completion_tokens": 1}}
		fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", passiveNativeJSON(chunk))
	}))
	defer server.Close()
	p := representativeBuildParams()
	p.Sandbox, p.HTTPTool = nil, nil
	p.Goal, p.PrepareNextTurn, p.RefreshKey = "", nil, nil
	p.Tools = []agentcore.Tool{nativeChildWrite{&effects}}
	runtime := PiRuntimeConfig{}
	runner := NewRunner(nil, WithPiRuntime(runtime))
	tier := ModelTier{TierConfig: TierConfig{Provider: "openrouter", Model: "native-http", BaseURL: server.URL + "/v1", APIKey: "key"}}
	if len(pooledProvider) > 0 && pooledProvider[0] == "google" {
		tier.Provider = "google"
	}
	if piMessages {
		tier.Provider = "radius"
	}
	if anthropic {
		tier.Provider, tier.BaseURL = "anthropic", server.URL
	}
	if claudePool {
		tier.Provider, tier.TokenSource = ai.VendorClaudeCode, &nativeClaudeAccountSource{acquired: &acquisitions}
	}
	if antigravity {
		tier.Provider, tier.BaseURL, tier.TokenSource = ai.VendorGoogleAntigravity, server.URL, &nativeAntigravityAccountSource{acquired: &acquisitions}
	}
	if federated {
		tier.APIKey = ""
	}
	if responses {
		tier.Provider = "openai"
		if azure {
			tier.Provider = ai.VendorAzureResponses
		}
		tier.Capabilities.StatefulResponses = agentcore.CapabilitySupported
	}
	result, err := runner.runModelLoop(ctx, p, RunOptions{Prompt: "PARENT-ONLY", NativeHistory: json.RawMessage(`[{"role":"user","content":"parent private history","timestamp":1}]`)}, tier, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Final != "parent answer" || parentCalls.Load() != 2 || childCalls.Load() != 2 || effects.Load() != 1 || result.Usage.InputTokens != 4 {
		t.Fatalf("native child HTTP run: %+v requests %d/%d effects %d", result, parentCalls.Load(), childCalls.Load(), effects.Load())
	}
	store := p.Session.(*agentcore.MemorySessionStore)
	if len(store.Sessions()) != 2 {
		t.Fatalf("sessions: %v", store.Sessions())
	}
	for _, id := range store.Sessions() {
		if id == p.SessionID {
			continue
		}
		entries, err := store.Log(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		receipt, done, err := piCompletedChild(entries)
		if err != nil || !done || receipt.Final != "child answer" {
			t.Fatalf("child receipt: %+v %v", receipt, err)
		}
	}
	history := json.RawMessage(`[{"role":"user","content":"summarize source","timestamp":1}]`)
	summary, usage, err := summarizePiHistoryWithUsage(ctx, runtime, tier, history, nativeAgentRevision, nil, nil)
	if federated && exchanges.Load() != 1 {
		t.Errorf("parent/child/summary exchanges: %d", exchanges.Load())
	}
	if (claudePool || antigravity) && acquisitions.Load() != 5 {
		t.Errorf("Claude parent/child/summary acquisitions=%d", acquisitions.Load())
	}
	if err != nil || summary != "native summary" || summaryCalls.Load() != 1 || usage.InputTokens != 1 {
		t.Fatalf("native summary HTTP: %q %+v %v", summary, usage, err)
	}
}

func writeNativeAnthropicEvents(w http.ResponseWriter, events ...map[string]any) {
	for _, event := range events {
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event["type"], passiveNativeJSON(event))
	}
	fmt.Fprint(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
}

func writeNativeAnthropicAnswer(w http.ResponseWriter, text string, input, output int) {
	writeNativeAnthropicEvents(w,
		map[string]any{"type": "message_start", "message": map[string]any{"id": "response", "model": "native-http", "usage": map[string]int{"input_tokens": input, "output_tokens": output}}},
		map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]string{"type": "text", "text": ""}},
		map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]string{"type": "text_delta", "text": text}},
		map[string]any{"type": "content_block_stop", "index": 0},
		map[string]any{"type": "message_delta", "delta": map[string]string{"stop_reason": "end_turn"}},
	)
}

func writeNativeResponsesEvents(w http.ResponseWriter, events ...map[string]any) {
	for _, event := range events {
		fmt.Fprintf(w, "data: %s\n\n", passiveNativeJSON(event))
	}
	fmt.Fprint(w, "data: [DONE]\n\n")
}
func nativeResponsesTerminal(id string, input, output int) map[string]any {
	return map[string]any{"type": "response.completed", "response": map[string]any{"id": id, "status": "completed", "usage": map[string]int{"input_tokens": input, "output_tokens": output, "total_tokens": input + output}}}
}
func nativeResponsesText(text string) map[string]any {
	return map[string]any{"type": "response.output_item.done", "output_index": 0, "item": map[string]any{"type": "message", "id": "msg_answer", "phase": "final_answer", "content": []any{map[string]any{"type": "output_text", "text": text}}}}
}

func writeNativeResponsesAnswer(w http.ResponseWriter, text, id string, input, output int) {
	writeNativeResponsesEvents(w,
		map[string]any{"type": "response.output_item.added", "output_index": 0, "item": map[string]any{"type": "message", "id": "msg_answer"}},
		map[string]any{"type": "response.output_text.delta", "output_index": 0, "delta": text},
		nativeResponsesText(text), nativeResponsesTerminal(id, input, output))
}

func TestNativeResponsesUnfinishedToolHasNoEffect(t *testing.T) {
	testNativeStreamFailureHasNoEffect(t, false, false, false)
}
func TestNativeAnthropicTruncatedStreamHasNoEffect(t *testing.T) {
	testNativeStreamFailureHasNoEffect(t, true, false, false)
}
func testNativeStreamFailureHasNoEffect(t *testing.T, anthropic, azure, piMessages bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var requests, effects atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestNumber := requests.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		if requestNumber > 1 {
			var payload map[string]json.RawMessage
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Error(err)
				return
			}
			history, callType := "input", `"type":"function_call"`
			if piMessages {
				history, callType = "context", `"type":"toolCall"`
			}
			if anthropic {
				history, callType = "messages", `"type":"tool_use"`
			}
			if strings.Contains(string(payload[history]), callType) {
				t.Error("rejected call replayed to provider")
			}
			if piMessages {
				writeNativePiMessagesAnswer(w, "recovered", 1, 1)
			} else if anthropic {
				writeNativeAnthropicAnswer(w, "recovered", 1, 1)
			} else {
				writeNativeResponsesAnswer(w, "recovered", "recovered", 1, 1)
			}
			return
		}
		if piMessages {
			writeNativePiMessagesEvents(w,
				map[string]any{"type": "toolcall_start", "contentIndex": 0, "id": "partial", "toolName": "write"},
				map[string]any{"type": "toolcall_end", "contentIndex": 0, "toolCall": map[string]any{"type": "toolCall", "id": "partial", "name": "write", "arguments": map[string]any{}}})
			return
		}
		if anthropic {
			// Even a finalized tool block and tool_use stop reason must not
			// execute if the provider truncates before message_stop.
			for _, event := range []map[string]any{
				{"type": "message_start", "message": map[string]any{"id": "truncated", "model": "native-http", "usage": map[string]int{"input_tokens": 2, "output_tokens": 1}}},
				{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "tool_use", "id": "partial", "name": "write", "input": map[string]any{}}},
				{"type": "content_block_delta", "index": 0, "delta": map[string]string{"type": "input_json_delta", "partial_json": "{}"}},
				{"type": "content_block_stop", "index": 0},
				{"type": "message_delta", "delta": map[string]string{"stop_reason": "tool_use"}},
			} {
				fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event["type"], passiveNativeJSON(event))
			}
			return
		}
		writeNativeResponsesEvents(w,
			map[string]any{"type": "response.output_item.added", "output_index": 0, "item": map[string]any{"type": "function_call", "id": "fc_partial", "call_id": "partial", "name": "write", "arguments": ""}},
			map[string]any{"type": "response.function_call_arguments.delta", "output_index": 0, "delta": "{}"},
			nativeResponsesTerminal("incomplete-tool", 2, 1),
		)
	}))
	defer server.Close()
	p := representativeBuildParams()
	p.Sandbox, p.HTTPTool, p.Subagents = nil, nil, nil
	p.Goal, p.PrepareNextTurn, p.RefreshKey = "", nil, nil
	p.Tools = []agentcore.Tool{nativeChildWrite{&effects}}
	runner := NewRunner(nil, WithPiRuntime(PiRuntimeConfig{}))
	tier := ModelTier{TierConfig: TierConfig{Provider: ai.VendorOpenAIResponses, Model: "native-http", BaseURL: server.URL + "/v1", APIKey: "key"}}
	if azure {
		tier.Provider = ai.VendorAzureResponses
	}
	wantError := "OpenAI Responses stream completed with an unfinished tool call: write (partial|fc_partial)"
	if piMessages {
		tier.Provider = "radius"
		wantError = "radius stream ended without a terminal event"
	}
	if anthropic {
		tier.Provider, tier.BaseURL = "anthropic", server.URL
		wantError = "Anthropic stream ended before message_stop"
	}
	result, err := runner.runModelLoop(ctx, p, RunOptions{Prompt: "write"}, tier, nil)
	if err == nil || !strings.Contains(err.Error(), wantError) {
		t.Fatalf("unfinished tool error: %v", err)
	}
	if result.StopReason != "error" || requests.Load() != 1 || effects.Load() != 0 || len(result.Tools) != 0 {
		t.Fatalf("unfinished tool escaped: stop=%s requests=%d effects=%d tools=%d", result.StopReason, requests.Load(), effects.Load(), len(result.Tools))
	}
	if !strings.Contains(string(result.NativeState), wantError) || strings.Contains(string(result.NativeState), "partialJson") {
		t.Fatal("unfinished error or scratch cleanup lost")
	}
	entries, err := p.Session.Log(ctx, p.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Kind == piEffectStart || entry.Kind == piEffectDone {
			t.Fatal("unfinished call was journaled as executable")
		}
	}
	if _, err := recoverPiState(entries); err != nil {
		t.Fatal(err)
	}
	lease, release, err := agentcore.AcquireSessionLease(ctx, p.Session, p.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if lease.Err() != nil {
		t.Fatal(lease.Err())
	}
	_ = release()
	p.ResumeSession = true
	resumed, resumeErr := runner.runModelLoop(ctx, p, RunOptions{Prompt: "retry after provider failure"}, tier, nil)
	if resumeErr != nil || resumed.Final != "recovered" || effects.Load() != 0 || requests.Load() != 2 {
		t.Fatalf("provider failure resume: final=%q effects=%d requests=%d err=%v", resumed.Final, effects.Load(), requests.Load(), resumeErr)
	}
}

func configureNativeFederationEnv(t *testing.T) {
	t.Helper()
	identity := filepath.Join(t.TempDir(), "identity")
	if err := os.WriteFile(identity, []byte("native-fixture-assertion"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ANTHROPIC_FEDERATION_RULE_ID", "native-rule")
	t.Setenv("ANTHROPIC_ORGANIZATION_ID", "native-organization")
	t.Setenv("ANTHROPIC_IDENTITY_TOKEN_FILE", identity)
	t.Setenv("ANTHROPIC_WORKSPACE_ID", "")
	t.Setenv("ANTHROPIC_SERVICE_ACCOUNT_ID", "")
	t.Setenv("ANTHROPIC_CUSTOM_HEADERS", "")
}

func serveNativeFederationExchange(t *testing.T, w http.ResponseWriter, r *http.Request, exchanges *atomic.Int32) bool {
	t.Helper()
	if r.URL.Path != "/v1/oauth/token" {
		return false
	}
	exchanges.Add(1)
	var payload map[string]string
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		t.Error(err)
		return true
	}
	if payload["assertion"] != "native-fixture-assertion" || payload["federation_rule_id"] != "native-rule" || payload["organization_id"] != "native-organization" {
		t.Error("lost federation binding")
	}
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprint(w, `{"access_token":"native-federated-access","expires_in":3600}`)
	return true
}
