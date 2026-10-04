package engine_test

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/lohi-ai/agentray/agentcore/engine"
	"github.com/lohi-ai/agentray/ai"
)

func TestPiPromptImageReferences(t *testing.T) {
	raw, err := os.ReadFile("testdata/pi-prompt-images.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		UpstreamCommit string
		Cases          []struct {
			Input    struct{ InputKind, Shape, Phase, Operation string }
			Expected json.RawMessage
		}
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	if fixture.UpstreamCommit != "eeac84ca92498ac18b6832754d01aef1d3c5f654" || len(fixture.Cases) != 240 {
		t.Fatal("unexpected image-reference oracle coverage")
	}
	for _, tc := range fixture.Cases {
		t.Run(tc.Input.InputKind+"/"+tc.Input.Shape+"/"+tc.Input.Phase+"/"+tc.Input.Operation, func(t *testing.T) {
			input := tc.Input
			makeImage := func(data string) *ai.ContentBlock {
				return &ai.ContentBlock{Type: "image", Data: data, MIMEType: "image/png"}
			}
			original := makeImage("original")
			images := []*ai.ContentBlock{original}
			if input.Shape == "repeated" {
				images = append(images, original)
			}
			supplied := &ai.Message{Role: "user", Content: ai.BlockContent(ai.ContentBlock{Type: "text", Text: "ready"}), Timestamp: 1}
			var prompt any = "go"
			if input.InputKind == "message" {
				prompt = supplied
			}
			if input.InputKind == "list" {
				prompt = engine.NewList(supplied)
			}
			var retained *ai.Message
			applied := false
			observations := []any{}
			project := func(message *ai.Message) any {
				if message == nil {
					return nil
				}
				content := []any{}
				for _, block := range message.Content.Blocks {
					if block.Type == "image" {
						content = append(content, map[string]any{"type": block.Type, "data": block.Data, "same": block == original})
					} else {
						content = append(content, map[string]any{"type": block.Type, "text": block.Text})
					}
				}
				return map[string]any{"content": content}
			}
			apply := func() {
				applied = true
				switch input.Operation {
				case "source_edit":
					original.Data = "source"
				case "images_slot":
					images[0] = makeImage("slot")
				case "images_append":
					images = append(images, makeImage("appended"))
				case "content_edit":
					if retained != nil && len(retained.Content.Blocks) > 1 {
						retained.Content.Blocks[1].Data = "content"
					} else {
						applied = false
					}
				case "content_replace":
					if retained != nil && len(retained.Content.Blocks) > 1 {
						retained.Content.Blocks[1] = makeImage("replacement")
					} else {
						applied = false
					}
				}
			}
			observe := func(stage string, message *ai.Message) {
				if input.Phase == stage {
					apply()
				}
				observations = append(observations, map[string]any{"stage": stage, "message": project(message)})
			}
			config := engine.AgentConfig{Config: engine.Config{Now: func() int64 { return 1000 },
				TransformContext: func(_ context.Context, m *engine.MessageList) (*engine.MessageList, error) {
					observe("transform", retained)
					return m, nil
				},
				ConvertToLLM: func(m *engine.MessageList) (*engine.MessageList, error) { observe("convert", retained); return m, nil },
				FinishTurn:   func(context.Context, *engine.Turn) (string, error) { observe("finish", retained); return "end", nil },
			}, StreamFn: func(_ context.Context, _ json.RawMessage, current ai.TranscriptContext, _ map[string]any) (*ai.AssistantMessageEventStream, error) {
				for _, message := range current.Messages() {
					if message.Role == "user" {
						observe("provider", &message)
						break
					}
				}
				return loopListResponse(19, "stop"), nil
			}}
			agent, err := engine.NewAgent(engine.AgentOptions{InitialState: engine.InitialState{Model: json.RawMessage(`{"id":"test","api":"test","provider":"test"}`)}, AgentConfig: config})
			if err != nil {
				t.Fatal(err)
			}
			agent.Subscribe(&engine.Listener{Handle: func(_ context.Context, event engine.Event) error {
				if event.Type == "agent_start" {
					observe(event.Type, retained)
				}
				if (event.Type == "message_start" || event.Type == "message_end") && event.Message.Role == "user" {
					retained = event.Message
					observe(event.Type, retained)
				}
				return nil
			}})
			if err := agent.Prompt(context.Background(), prompt, images...); err != nil {
				t.Fatal(err)
			}
			if err := agent.WaitForIdle(context.Background()); err != nil {
				t.Fatal(err)
			}
			observe("settled", retained)
			imageValues := []any{}
			for _, image := range images {
				imageValues = append(imageValues, map[string]any{"data": image.Data, "same": image == original})
			}
			var history any
			for _, message := range agent.State().Messages.Values() {
				if message.Role == "user" {
					history = project(message)
					break
				}
			}
			assertStateListJSON(t, map[string]any{"observations": observations, "applied": applied, "original": original.Data, "images": imageValues, "retained": project(retained), "history": history}, tc.Expected)
		})
	}
}
