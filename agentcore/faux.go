package agentcore

import (
	"context"
	"strings"
)

// FauxProvider is a scripted LLM seam for tests: it replays a fixed list of
// responses in order, with no network, no keys, and no tokens (pi faux-provider
// pattern). It lets the loop, hooks, and permission gate be tested
// deterministically.
type FauxProvider struct {
	Responses []ChatResponse
	calls     int
	Recorded  []ChatRequest
}

// NewFauxProvider builds a provider that returns the given responses in order.
func NewFauxProvider(responses ...ChatResponse) *FauxProvider {
	return &FauxProvider{Responses: responses}
}

func (f *FauxProvider) Name() string        { return "faux" }
func (f *FauxProvider) SupportsTools() bool { return true }

// Chat returns the next scripted response. After the script is exhausted it
// returns a plain assistant message with stop reason "stop" so loops terminate.
func (f *FauxProvider) Chat(_ context.Context, req ChatRequest) (ChatResponse, error) {
	f.Recorded = append(f.Recorded, req)
	if f.calls >= len(f.Responses) {
		return ChatResponse{
			Message:    Message{Role: RoleAssistant, Content: "(end)"},
			StopReason: "stop",
		}, nil
	}
	resp := f.Responses[f.calls]
	f.calls++
	return resp, nil
}

// Stream adapts Chat into a delta channel: it emits the content word-by-word (so
// tests exercise token concatenation), then one delta per tool call, then a
// terminal Done — mirroring how the real provider streams a turn.
func (f *FauxProvider) Stream(ctx context.Context, req ChatRequest) (<-chan ChatDelta, error) {
	ch := make(chan ChatDelta, 8)
	go func() {
		defer close(ch)
		resp, _ := f.Chat(ctx, req)
		for i, word := range strings.Fields(resp.Message.Content) {
			frag := word
			if i > 0 {
				frag = " " + word
			}
			ch <- ChatDelta{ContentDelta: frag}
		}
		for i := range resp.Message.ToolCalls {
			tc := resp.Message.ToolCalls[i]
			ch <- ChatDelta{ToolCall: &tc}
		}
		ch <- ChatDelta{Done: true, StopReason: resp.StopReason, Usage: resp.Usage}
	}()
	return ch, nil
}

// AssistantText is a helper to script a plain text response.
func AssistantText(content string) ChatResponse {
	return ChatResponse{Message: Message{Role: RoleAssistant, Content: content}, StopReason: "stop"}
}

// AssistantToolCall is a helper to script a tool-calling response.
func AssistantToolCall(id, name, args string) ChatResponse {
	return ChatResponse{
		Message:    Message{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: id, Name: name, Arguments: args}}},
		StopReason: "tool_calls",
	}
}
