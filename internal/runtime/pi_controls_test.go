package agentruntime

import (
	"context"
	"encoding/json"
	"github.com/2found/2ai/ai"
	"reflect"
	"strings"
	"testing"

	"github.com/2found/2ai/agentcore"
)

func TestPiPayloadControlsPreserveNativeFieldsAcrossDialects(t *testing.T) {
	parallel := false
	schema := &agentcore.OutputSchema{Strict: true, Schema: map[string]any{"type": "object"}}
	for _, tc := range []struct {
		api, tools, choice, schemaKey string
	}{
		{"openai-completions", `[{"type":"function","function":{"name":"write"}}]`, `{"type":"function","function":{"name":"write"}}`, "response_format"},
		{"openai-responses", `[{"type":"function","name":"write"}]`, `{"type":"function","name":"write"}`, "text"},
		{"azure-openai-responses", `[{"type":"function","name":"write"}]`, `{"type":"function","name":"write"}`, "text"},
		{"anthropic-messages", `[{"name":"write"}]`, `{"type":"tool","name":"write","disable_parallel_tool_use":true}`, "output_config"},
	} {
		t.Run(tc.api, func(t *testing.T) {
			raw := json.RawMessage(`{"messages":[{"signed":"opaque"}],"extension":9007199254740993,"text":{"verbosity":"low"},"output_config":{"effort":"high"},"tools":` + tc.tools + `}`)
			result, err := ai.ApplyNativeControls(tc.api, raw, ai.NativeGenerationControls{ToolChoice: agentcore.ToolChoice{Mode: agentcore.ToolChoiceNamed, Name: "write"}, ParallelToolCalls: &parallel, OutputSchema: schema})
			if err != nil {
				t.Fatal(err)
			}
			var got map[string]json.RawMessage
			_ = json.Unmarshal(result, &got)
			var choice, expected any
			_ = json.Unmarshal(got["tool_choice"], &choice)
			_ = json.Unmarshal([]byte(tc.choice), &expected)
			if !reflect.DeepEqual(choice, expected) || string(got["extension"]) != "9007199254740993" || string(got["messages"]) != `[{"signed":"opaque"}]` {
				t.Fatalf("lost native payload or controls: %s", result)
			}
			if tc.api != "anthropic-messages" && string(got["parallel_tool_calls"]) != "false" {
				t.Fatalf("parallel control lost: %s", result)
			}
			if !strings.Contains(string(got[tc.schemaKey]), `"schema":{"type":"object"}`) || !strings.Contains(string(got["text"]), `"verbosity":"low"`) || !strings.Contains(string(got["output_config"]), `"effort":"high"`) {
				t.Fatalf("schema mapping overwrote native options: %s", result)
			}
			if tc.api == "anthropic-messages" && (strings.Contains(string(got[tc.schemaKey]), `"name"`) || strings.Contains(string(got[tc.schemaKey]), `"strict"`)) {
				t.Fatalf("OpenAI schema fields leaked to Anthropic: %s", result)
			}
		})
	}
}

func TestPiToolChoiceValidationAndToolFreeWrap(t *testing.T) {
	for _, choice := range []agentcore.ToolChoice{{Mode: "invalid"}, {Mode: agentcore.ToolChoiceRequired}, {Mode: agentcore.ToolChoiceNamed, Name: "missing"}} {
		if err := ai.ValidateNativeToolChoice(choice, nil); err == nil {
			t.Fatalf("invalid choice accepted: %+v", choice)
		}
	}
	for _, api := range []string{"openai-completions", "openai-responses", "anthropic-messages", "azure-openai-responses"} {
		for _, mode := range []agentcore.ToolChoiceMode{agentcore.ToolChoiceAuto, agentcore.ToolChoiceNone, agentcore.ToolChoiceRequired} {
			raw := json.RawMessage(`{"tools":[{"name":"write","function":{"name":"write"}}]}`)
			result, err := ai.ApplyNativeControls(api, raw, ai.NativeGenerationControls{ToolChoice: agentcore.ToolChoice{Mode: mode}})
			if err != nil {
				t.Fatal(err)
			}
			want := `"tool_choice":"` + string(mode) + `"`
			if api == "anthropic-messages" {
				if mode == agentcore.ToolChoiceRequired {
					mode = "any"
				}
				want = `"tool_choice":{"type":"` + string(mode) + `"}`
			}
			if !strings.Contains(string(result), want) {
				t.Fatalf("wrong dialect: %s %s", api, result)
			}
		}
		parallel := true
		result, err := ai.ApplyNativeControls(api, json.RawMessage(`{"messages":[]}`), ai.NativeGenerationControls{ToolChoice: agentcore.ToolChoice{Mode: agentcore.ToolChoiceRequired}, ParallelToolCalls: &parallel})
		if err != nil || string(result) != `{"messages":[]}` {
			t.Fatalf("tool-free finalization forced another call: %s %v", result, err)
		}
	}
	if _, err := ai.ApplyNativeControls("openai-completions", json.RawMessage(`{"tools":[{"function":{"name":"write"}}]}`), ai.NativeGenerationControls{ToolChoice: agentcore.ToolChoice{Mode: agentcore.ToolChoiceNamed, Name: "hidden"}}); err == nil {
		t.Fatal("named choice bypassed request tool filtering")
	}
}

func TestPiModelControlCapabilitiesAndHookComposition(t *testing.T) {
	tier := ModelTier{TierConfig: TierConfig{Provider: "openai", Model: "test", APIKey: "test", BaseURL: "https://example.test/v1"}}
	parallel := false
	opts := PiModelOptions{ParallelToolCalls: &parallel, OutputSchema: &agentcore.OutputSchema{Schema: map[string]any{"type": "object"}}}
	cfg, _, err := tier.BindPi(NativeAgentConfig{Options: json.RawMessage(`{"callbacks":["onPayload"]}`), Callback: func(_ context.Context, method string, params json.RawMessage, _ func(json.RawMessage) error) (json.RawMessage, error) {
		if method != "onPayload" {
			t.Fatal(method)
		}
		return json.RawMessage(`{"tools":[{"function":{"name":"write"}}],"host_extension":true}`), nil
	}}, opts)
	if err != nil {
		t.Fatal(err)
	}
	var config struct {
		InitialState struct{ Model json.RawMessage }
	}
	_ = json.Unmarshal(cfg.Options, &config)
	result, err := cfg.Callback(context.Background(), "onPayload", piModelJSON(map[string]any{"model": config.InitialState.Model, "payload": map[string]any{}}), nil)
	if err != nil || !strings.Contains(string(result), `"host_extension":true`) || !strings.Contains(string(result), `"parallel_tool_calls":false`) || !strings.Contains(string(result), `"response_format"`) {
		t.Fatalf("callback composition lost controls: %s %v", result, err)
	}
	if _, err := cfg.Callback(context.Background(), "onPayload", json.RawMessage(`{"model":{},"payload":{}}`), nil); err == nil {
		t.Fatal("payload callback accepted unbound model")
	}
	tier.Capabilities.ToolChoice = agentcore.CapabilityUnsupported
	if _, _, err := tier.BindPi(NativeAgentConfig{}, PiModelOptions{ToolChoice: agentcore.ToolChoice{Mode: agentcore.ToolChoiceRequired}}); err == nil {
		t.Fatal("forced choice bypassed model capability")
	}
	tier.Capabilities.StructuredOutput = agentcore.CapabilityUnsupported
	opts.ToolChoice = agentcore.ToolChoice{Mode: agentcore.ToolChoiceNone}
	cfg, _, err = tier.BindPi(NativeAgentConfig{}, opts)
	if err != nil || !strings.Contains(string(cfg.Options), `"tools":[]`) || strings.Contains(string(cfg.Options), `"onPayload"`) {
		t.Fatalf("capability filtering lost none or retained unsupported hints: %s %v", cfg.Options, err)
	}
}
