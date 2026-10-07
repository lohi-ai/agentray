package agentruntime

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/2found/2ai/ai"
	"github.com/2found/2ai/jsonjs"
)

func TestNativePromptUnicode(t *testing.T) {
	for _, raw := range []string{`"plain"`, `"\ud800x\udfff"`, `"\ud83d\ude80"`, `"a\u0000b"`, `"\ufffd\ud800"`} {
		t.Run(raw, func(t *testing.T) {
			decoded, err := jsonjs.DecodeJSON([]byte(raw))
			if err != nil {
				t.Fatal(err)
			}
			want := decoded.(string)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			var system, user string
			agent, err := NewNativeAgent(ctx, NativeAgentConfig{Options: json.RawMessage(`{"streamMode":"native","initialState":{"systemPrompt":` + raw + `}}`), StreamFn: func(_ context.Context, _ json.RawMessage, transcript ai.TranscriptContext, _ map[string]any) (*ai.AssistantMessageEventStream, error) {
				system = ai.GetCurrentSystemPrompt(transcript.Messages())
				for _, message := range transcript.Messages() {
					if message.Role == "user" {
						user = ai.ContentText(message.Content)
					}
				}
				stream := ai.NewAssistantMessageEventStream()
				stream.Push(ai.AssistantMessageEvent{Type: "done", Reason: "stop", Message: &ai.Message{Role: "assistant", Content: ai.BlockContent(), StopReason: "stop", Usage: &ai.Usage{}}})
				stream.End()
				return stream, nil
			}})
			if err != nil {
				t.Fatal(err)
			}
			defer agent.Close()
			if err := agent.Prompt(ctx, json.RawMessage(raw)); err != nil {
				t.Fatal(err)
			}
			if system != want || user != want {
				t.Fatalf("provider received system=%s user=%s; want %s", jsonjs.QuoteString(system), jsonjs.QuoteString(user), raw)
			}
			state, err := agent.State(ctx)
			if err != nil {
				t.Fatal(err)
			}
			value, err := jsonjs.DecodeJSON(state)
			if err != nil {
				t.Fatal(err)
			}
			if value.(map[string]any)["systemPrompt"] != want {
				t.Fatalf("state lost prompt: %s", state)
			}
		})
	}
}
