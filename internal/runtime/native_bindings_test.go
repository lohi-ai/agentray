package agentruntime

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"testing"

	"github.com/lohi-ai/agentray/agentcore/engine"
	"github.com/lohi-ai/agentray/ai"
)

func TestNativeToolBindingRetainsNullResultsAndUpdates(t *testing.T) {
	for _, resultJSON := range []string{`null`, `{"content":[{"type":"text","text":"done"}],"details":{}}`} {
		t.Run(resultJSON, func(t *testing.T) {
			agent := &NativeAgent{config: NativeAgentConfig{Callback: func(_ context.Context, method string, _ json.RawMessage, emit func(json.RawMessage) error) (json.RawMessage, error) {
				if method != "tool" {
					t.Errorf("unexpected callback %q", method)
				}
				for _, partial := range []string{`null`, `{"content":[{"type":"text","text":"working"}]}`, `null`} {
					if err := emit(json.RawMessage(partial)); err != nil {
						return nil, err
					}
				}
				return json.RawMessage(resultJSON), nil
			}}}
			tools, err := agent.bindTools(json.RawMessage(`[{"name":"echo","description":"echo","parameters":{"type":"object"}}]`))
			if err != nil {
				t.Fatal(err)
			}
			var updates []*engine.ToolResult
			outcome, err := engine.RunToolCall(context.Background(), &ai.ContentBlock{Type: "toolCall", ID: "call", Name: "echo", Arguments: json.RawMessage(`{}`)}, tools, nil, &engine.Context{Tools: tools}, engine.ToolHooks{}, func(partial *engine.ToolResult) error {
				updates = append(updates, partial)
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if len(updates) != 3 || updates[0] != nil || updates[1] == nil || updates[2] != nil {
				t.Fatalf("lost null update shape: %+v", updates)
			}
			if len(updates[1].Content) != 1 || updates[1].Content[0].Text != "working" {
				t.Fatalf("lost update content: %+v", updates[1])
			}
			wantError := resultJSON == "null"
			wantText := "done"
			if wantError {
				wantText = "null is not an object (evaluating 'result.isError')"
			}
			if outcome.IsError != wantError || outcome.Result == nil || len(outcome.Result.Content) != 1 || outcome.Result.Content[0].Text != wantText {
				t.Fatalf("unexpected result: %+v", outcome)
			}
		})
	}
}

func TestNativeToolBindingExportsLiveArguments(t *testing.T) {
	for _, cyclic := range []bool{false, true} {
		t.Run(fmt.Sprint(cyclic), func(t *testing.T) {
			called := false
			agent := &NativeAgent{config: NativeAgentConfig{Callback: func(_ context.Context, method string, params json.RawMessage, _ func(json.RawMessage) error) (json.RawMessage, error) {
				called = true
				var fields struct{ Args json.RawMessage }
				if err := json.Unmarshal(params, &fields); err != nil {
					t.Fatal(err)
				}
				if method != "tool" || string(fields.Args) != `{"items":[{"value":"changed"},"appended",null],"overflow":null}` {
					t.Fatalf("unexpected tool transport: %s %s", method, params)
				}
				return json.RawMessage(`{"content":[],"details":{}}`), nil
			}}}
			tools, err := agent.bindTools(json.RawMessage(`[{"name":"echo","parameters":{"type":"object"}}]`))
			if err != nil {
				t.Fatal(err)
			}
			original := json.RawMessage(`{"items":[{"value":"original"}]}`)
			call := &ai.ContentBlock{Type: "toolCall", ID: "call", Name: "echo", Arguments: original}
			outcome, err := engine.RunToolCall(context.Background(), call, tools, nil, &engine.Context{Tools: tools}, engine.ToolHooks{
				Before: func(_ context.Context, call *engine.BeforeToolCall) (*engine.BeforeToolResult, error) {
					args := call.Args.(*engine.Object)
					items := args.Get("items").(*engine.Array)
					items.Get(0).(*engine.Object).Set("value", "changed")
					items.Append("appended", engine.Undefined)
					args.Set("overflow", math.Inf(1))
					args.Set("omitted", engine.Undefined)
					if cyclic {
						args.Set("self", args)
					}
					return nil, nil
				},
			}, nil)
			if err != nil || outcome.IsError != cyclic || called == cyclic {
				t.Fatalf("outcome=%+v err=%v called=%v", outcome, err, called)
			}
			if string(call.Arguments) != `{"items":[{"value":"original"}]}` {
				t.Fatalf("mutated raw model arguments: %s", call.Arguments)
			}
		})
	}
}
