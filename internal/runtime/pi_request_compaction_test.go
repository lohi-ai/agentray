package agentruntime

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/2found/2ai/agentcore"
)

func piLongRequest() []json.RawMessage {
	result := []json.RawMessage{json.RawMessage(`{"role":"system","content":"policy","toolsAdded":[{"name":"read","parameters":{"type":"object"}}]}`), json.RawMessage(`{"role":"user","content":"complete the task"}`)}
	for i := 0; i < 4; i++ {
		result = append(result, json.RawMessage(`{"role":"assistant","content":[{"type":"thinking","thinking":"reason","thinkingSignature":"opaque"},{"type":"toolCall","id":"reused","name":"read","arguments":{}}]}`))
		raw, _ := json.Marshal(map[string]any{"role": "toolResult", "toolCallId": "reused", "toolName": "read", "content": []any{map[string]any{"type": "text", "text": strings.Repeat("source ", 500)}}, "details": map[string]any{"opaque": i}})
		result = append(result, raw)
	}
	return result
}

func piRequestJSON(value any) json.RawMessage { raw, _ := json.Marshal(value); return raw }

func TestPiRequestCompactionRejectsCorruptJournal(t *testing.T) {
	if _, err := recoverPiState([]agentcore.SessionEntry{{Kind: agentcore.EntryPiState, Content: `{"messages":[]}`}, {Kind: agentcore.EntryPiContextSummary, Content: `{"prefix_count":0}`}}); err == nil {
		t.Fatal("corrupt summary was accepted on recovery")
	}
}
