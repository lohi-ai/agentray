package agentcore

import (
	"context"
	"testing"
)

// schemaTool advertises a JSON Schema requiring a string "sql" field and an
// optional enum "mode". It records the args it actually ran with.
type schemaTool struct {
	called  int
	lastArg string
}

func (s *schemaTool) Name() string { return "run_query" }
func (s *schemaTool) Schema() ToolSchema {
	return ToolSchema{
		Name:        "run_query",
		Description: "run a SQL query",
		Parameters: map[string]any{
			"type":     "object",
			"required": []any{"sql"},
			"properties": map[string]any{
				"sql":  map[string]any{"type": "string"},
				"mode": map[string]any{"type": "string", "enum": []any{"read", "write"}},
			},
		},
	}
}
func (s *schemaTool) Run(_ context.Context, args string) (string, error) {
	s.called++
	s.lastArg = args
	return "ok", nil
}

func TestValidateArgs_RequiredAndTypes(t *testing.T) {
	tool := &schemaTool{}
	schema := tool.Schema().Parameters
	cases := []struct {
		name    string
		args    string
		wantErr bool
	}{
		{"valid", `{"sql":"select 1"}`, false},
		{"valid with enum", `{"sql":"select 1","mode":"read"}`, false},
		{"missing required", `{"mode":"read"}`, true},
		{"wrong type", `{"sql":123}`, true},
		{"bad enum", `{"sql":"x","mode":"delete"}`, true},
		{"unknown field ok", `{"sql":"x","extra":true}`, false},
		{"malformed json", `{"sql":`, true},
		{"empty needs required", ``, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := validateArgs(c.args, schema)
			if (err != nil) != c.wantErr {
				t.Fatalf("validateArgs(%q) err=%v, wantErr=%v", c.args, err, c.wantErr)
			}
		})
	}
}

// TestLoopRejectsBadArgs verifies a schema-invalid tool call is blocked before
// execution, the precise reason reaches the model, and the tool never runs.
func TestLoopRejectsBadArgs(t *testing.T) {
	tool := &schemaTool{}
	faux := NewFauxProvider(
		AssistantToolCall("c1", "run_query", `{"mode":"read"}`), // missing required sql
		AssistantText("sorry, I omitted the sql field"),
	)
	agent, err := New(Config{
		Provider: faux,
		Model:    "test",
		Tools:    NewToolSet(tool),
		Policy:   NewAllowList("run_query"),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	res, err := agent.Prompt(context.Background(), "run a query")
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if tool.called != 0 {
		t.Fatalf("schema-invalid tool must not execute, got %d calls", tool.called)
	}
	if len(res.Tools) != 1 || res.Tools[0].Allowed {
		t.Fatalf("expected 1 blocked trace, got %+v", res.Tools)
	}
	// The second turn's request must carry the validation error as a tool result
	// so the model can self-correct.
	last := faux.Recorded[len(faux.Recorded)-1]
	var sawError bool
	for _, m := range last.Messages {
		if m.Role == RoleTool && contains(m.Content, "required") {
			sawError = true
		}
	}
	if !sawError {
		t.Fatalf("expected validation error fed back to model, messages=%+v", last.Messages)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}
