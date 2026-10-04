package agentruntime

import (
	"encoding/json"
	"testing"

	"github.com/lohi-ai/agentray/agentcore"
	nativehost "github.com/lohi-ai/agentray/agentcore/host"
)

func TestPiRunProjectionDoesNotExposeThinkingAsAnswer(t *testing.T) {
	raw := json.RawMessage(`{"role":"assistant","content":[{"type":"thinking","thinking":"private reasoning","thinkingSignature":"opaque"},{"type":"text","text":"answer"},{"type":"toolCall","id":"call","name":"read","arguments":{"x":1}}],"usage":{"input":3,"output":4,"cacheRead":5,"cacheWrite":6,"cost":{"total":0.125}},"extension":{"keep":"original"}}`)
	m, err := nativehost.ProjectMessage(raw)
	if err != nil {
		t.Fatal(err)
	}
	if m.Content != "answer" || len(m.ToolCalls) != 1 || m.ToolCalls[0].Arguments != `{"x":1}` {
		t.Fatalf("incorrect display projection: %+v", m)
	}
	if m.Usage == nil || m.Usage.InputTokens != 3 || m.Usage.CacheReadTokens != 5 || m.Usage.CacheWriteTokens != 6 || m.Usage.CostUSD != 0.125 {
		t.Fatalf("usage projection: %+v", m.Usage)
	}
	custom, err := nativehost.ProjectMessage(json.RawMessage(`{"role":"custom","content":{"not":"a legacy message"}}`))
	if err != nil || custom.Role != "custom" || custom.Content != "" {
		t.Fatalf("imposed legacy schema on custom message: %+v %v", custom, err)
	}
}

func TestPiRunProjectionAcceptsArbitraryNativeToolDetails(t *testing.T) {
	for _, details := range []string{`"opaque"`, `42`, `[1,2]`, `null`, `{"custom":"data"}`} {
		p := piRunProjection{calls: map[string]agentcore.ToolTrace{}}
		if err := p.event(json.RawMessage(`{"type":"tool_execution_start","toolCallId":"call","toolName":"read","args":{}}`)); err != nil {
			t.Fatal(err)
		}
		if err := p.event(json.RawMessage(`{"type":"tool_execution_end","toolCallId":"call","toolName":"read","result":{"content":[],"details":` + details + `},"isError":false}`)); err != nil {
			t.Fatal(err)
		}
		if len(p.result.Tools) != 1 || p.result.Tools[0].CallID != "call" || !p.result.Tools[0].Allowed {
			t.Fatalf("lost tool result for details=%s: %+v", details, p.result.Tools)
		}
	}
}

func TestPiRunProjectionStreamsOnlyAnswerText(t *testing.T) {
	var tokens string
	p := piRunProjection{sink: func(event agentcore.StreamEvent) {
		if event.Type == agentcore.StreamToken {
			tokens += event.Token
		}
	}}
	for _, raw := range []string{
		`{"type":"message_update","assistantMessageEvent":{"type":"thinking_delta","delta":"private"}}`,
		`{"type":"message_update","assistantMessageEvent":{"type":"text_delta","delta":"hello "}}`,
		`{"type":"message_update","assistantMessageEvent":{"type":"toolcall_delta","delta":"arguments"}}`,
		`{"type":"message_update","assistantMessageEvent":{"type":"text_delta","delta":"world"}}`,
	} {
		if err := p.event(json.RawMessage(raw)); err != nil {
			t.Fatal(err)
		}
	}
	if tokens != "hello world" {
		t.Fatalf("unexpected answer stream: %q", tokens)
	}
}
