package agentruntime

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/lohi-ai/agentray/agentcore"
)

func TestPiSessionRecoveryKeepsNativeMessages(t *testing.T) {
	initial := agentcore.SessionEntry{Kind: piStateEntry, Content: `{"messages":[{"role":"system","content":"system","timestamp":1}],"model":{"id":"test"},"tools":[],"thinkingLevel":"off","isStreaming":false}`}
	event := `{"type":"message_end","message":{"role":"custom","timestamp":2,"content":[{"type":"image","mimeType":"image/png","data":"abc"}],"extension":{"signature":"opaque > & \u2028"}}}`
	state, err := recoverPiState([]agentcore.SessionEntry{initial, {Kind: piEventEntry, Content: event}})
	if err != nil {
		t.Fatal(err)
	}
	var restored map[string]json.RawMessage
	_ = json.Unmarshal(state, &restored)
	if _, ok := restored["isStreaming"]; ok {
		t.Fatal("restored runtime-owned state")
	}
	var messages []map[string]json.RawMessage
	if err := json.Unmarshal(restored["messages"], &messages); err != nil || len(messages) != 2 {
		t.Fatalf("native messages lost: %s, %v", state, err)
	}
	if string(messages[1]["role"]) != `"custom"` || len(messages[1]["extension"]) == 0 || len(messages[1]["content"]) == 0 {
		t.Fatalf("message projected through legacy schema: %s", state)
	}
}

func TestPiSessionRecoveryCannotMaskUnsettledEffectsWithReusedCallID(t *testing.T) {
	entries := []agentcore.SessionEntry{
		{Kind: piStateEntry, Content: `{"messages":[],"tools":[]}`},
		{Kind: piEffectStart, CallID: "reused", Content: `{"effectId":"first"}`},
		{Kind: piEffectStart, CallID: "reused", Content: `{"effectId":"second"}`},
		{Kind: piEffectDone, CallID: "reused", Content: `{"effectId":"second"}`},
		{Kind: piStateEntry, Content: `{"messages":[],"tools":[]}`},
	}
	_, err := recoverPiState(entries)
	if !errors.Is(err, ErrPiUnsettledEffect) || !strings.Contains(err.Error(), "reused/first") {
		t.Fatalf("lost first effect behind second receipt or checkpoint: %v", err)
	}
}

func TestPiSessionRecoveryRejectsLegacyAndUnplacedResults(t *testing.T) {
	for name, entries := range map[string][]agentcore.SessionEntry{
		"legacy":         {{Kind: agentcore.EntryMessage}},
		"empty":          nil,
		"broken":         {{Kind: piStateEntry, Content: `{`}},
		"missing result": {{Kind: piStateEntry, Content: `{"messages":[{"role":"assistant","content":[{"type":"toolCall","id":"call","name":"write","arguments":{}}]}]}`}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := recoverPiState(entries); err == nil {
				t.Fatal("accepted unsafe recovery")
			}
		})
	}
}

func TestPiSessionRecoversExactStartedResult(t *testing.T) {
	result := `{"role":"toolResult","toolCallId":"call","toolName":"write","content":[{"type":"text","text":"ok"}],"timestamp":1234,"details":{"signature":"opaque"},"isError":false}`
	initial := agentcore.SessionEntry{Kind: piStateEntry, Content: `{"messages":[{"role":"assistant","content":[{"type":"toolCall","id":"call","name":"write"}]}]}`}
	start := agentcore.SessionEntry{Kind: piEventEntry, Content: `{"type":"message_start","message":` + result + `}`}
	end := agentcore.SessionEntry{Kind: piEventEntry, Content: `{"type":"message_end","message":` + result + `}`}
	for _, finished := range []bool{false, true} {
		entries := []agentcore.SessionEntry{initial, start}
		if finished {
			entries = append(entries, end)
		}
		raw, err := recoverPiState(entries)
		if err != nil {
			t.Fatal(err)
		}
		var state struct{ Messages []json.RawMessage }
		if err := json.Unmarshal(raw, &state); err != nil || len(state.Messages) != 2 || string(state.Messages[1]) != result {
			t.Fatalf("native result lost or duplicated (finished=%v): %s, %v", finished, raw, err)
		}
	}
}

func TestPiSessionRecoveryRejectsCrossBatchResult(t *testing.T) {
	_, err := recoverPiState([]agentcore.SessionEntry{{Kind: piStateEntry, Content: `{"messages":[
		{"role":"assistant","content":[{"type":"toolCall","id":"reused","name":"write"}]},
		{"role":"assistant","content":[{"type":"toolCall","id":"reused","name":"write"}]},
		{"role":"toolResult","toolCallId":"reused","content":[]}
	]}`}})
	if !errors.Is(err, ErrPiUnsettledEffect) {
		t.Fatalf("later result masked earlier missing result: %v", err)
	}
}

func TestPiSessionRecoveryRequiresNativeResult(t *testing.T) {
	_, err := recoverPiState([]agentcore.SessionEntry{
		{Kind: piStateEntry, Content: `{"messages":[{"role":"assistant","content":[{"type":"toolCall","id":"call","name":"write"}]}]}`},
		{Kind: piEffectStart, CallID: "call", Content: `{"effectId":"effect"}`},
		{Kind: piEffectDone, CallID: "call", Content: `{"effectId":"effect","result":{"content":[]}}`},
		{Kind: piEventEntry, Content: `{"type":"tool_execution_end","toolCallId":"call","toolName":"write","result":{"content":[]},"isError":false}`},
	})
	if !errors.Is(err, ErrPiUnsettledEffect) {
		t.Fatalf("invented native message from an execution receipt: %v", err)
	}
}
