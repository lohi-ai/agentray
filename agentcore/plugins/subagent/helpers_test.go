package subagent_test

import (
	"context"
	"encoding/json"

	"github.com/lohi-ai/agentray/agentcore"
)

// echoTool is a trivial always-succeeding tool. These tests are about who runs,
// with what scope, and what comes back — not about what a tool does.
type echoTool struct{ name string }

func (t *echoTool) Name() string { return t.name }

func (t *echoTool) Schema() agentcore.ToolSchema {
	return agentcore.ToolSchema{
		Name:        t.name,
		Description: "echo the given text",
		Parameters: map[string]any{
			"type":       "object",
			"properties": map[string]any{"text": map[string]any{"type": "string"}},
			"required":   []string{"text"},
		},
	}
}

func (t *echoTool) Run(_ context.Context, args string) (string, error) {
	var in struct {
		Text string `json:"text"`
	}
	_ = json.Unmarshal([]byte(args), &in)
	return in.Text, nil
}

// AssistantToolCall builds a scripted assistant turn that issues one tool call.
func AssistantToolCall(id, name, args string) agentcore.ChatResponse {
	return agentcore.ChatResponse{Message: agentcore.Message{
		Role:      agentcore.RoleAssistant,
		ToolCalls: []agentcore.ToolCall{{ID: id, Name: name, Arguments: args}},
	}}
}

// lastAssistantText returns the final assistant text in a reduced history.
func lastAssistantText(msgs []agentcore.Message) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == agentcore.RoleAssistant && msgs[i].Content != "" {
			return msgs[i].Content
		}
	}
	return ""
}
