package agentcore

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// recordingTool captures the exact argument string it was handed, so a test can
// assert what reached the tool vs. what was traced.
type recordingTool struct {
	name     string
	lastArgs string
	called   int
}

func (r *recordingTool) Name() string { return r.name }
func (r *recordingTool) Schema() ToolSchema {
	return ToolSchema{Name: r.name, Description: "rec", Parameters: map[string]any{"type": "object"}}
}
func (r *recordingTool) Run(_ context.Context, args string) (string, error) {
	r.called++
	r.lastArgs = args
	return "ok", nil
}

// stubResolver is a test CredentialResolver driven by a func.
type stubResolver struct {
	resolve func(string) (string, error)
}

func (s stubResolver) Resolve(_ context.Context, args string) (string, error) {
	return s.resolve(args)
}

// TestCredentialResolverInjectsAtTrustBoundary proves the core F7 property: the
// resolved secret reaches the tool, but the trace (and therefore the persisted
// record and the model-visible call) keeps the {{cred:NAME}} placeholder — the
// literal is never observable outside the executing tool.
func TestCredentialResolverInjectsAtTrustBoundary(t *testing.T) {
	tool := &recordingTool{name: "call_api"}
	faux := NewFauxProvider(
		AssistantToolCall("c1", "call_api", `{"key":"{{cred:API_KEY}}"}`),
		AssistantText("done"),
	)
	env := DefaultEnv()
	env.Credentials = stubResolver{resolve: func(args string) (string, error) {
		return strings.ReplaceAll(args, "{{cred:API_KEY}}", "sk-secret-value"), nil
	}}
	agent, err := New(Config{
		Provider: faux,
		Model:    "test",
		Tools:    NewToolSet(tool),
		Policy:   NewAllowList("call_api"),
		Env:      &env,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	res, err := agent.Prompt(context.Background(), "go")
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}

	// The tool received the resolved secret.
	if want := `{"key":"sk-secret-value"}`; tool.lastArgs != want {
		t.Fatalf("tool args: want %q, got %q", want, tool.lastArgs)
	}
	// The trace kept the placeholder — no secret leaked into the persisted record.
	if len(res.Tools) != 1 {
		t.Fatalf("expected 1 trace, got %d", len(res.Tools))
	}
	if got := res.Tools[0].Args; got != `{"key":"{{cred:API_KEY}}"}` {
		t.Fatalf("trace args leaked or changed: %q", got)
	}
	if strings.Contains(res.Tools[0].Args, "sk-secret-value") {
		t.Fatal("secret value leaked into the tool trace")
	}
}

// TestCredentialResolverFailsClosed verifies a resolver error blocks the call
// (the tool never runs) and the reason is returned to the model.
func TestCredentialResolverFailsClosed(t *testing.T) {
	tool := &recordingTool{name: "call_api"}
	faux := NewFauxProvider(
		AssistantToolCall("c1", "call_api", `{"key":"{{cred:MISSING}}"}`),
		AssistantText("understood"),
	)
	env := DefaultEnv()
	env.Credentials = stubResolver{resolve: func(string) (string, error) {
		return "", errors.New("unknown credential \"MISSING\"")
	}}
	agent, err := New(Config{
		Provider: faux,
		Model:    "test",
		Tools:    NewToolSet(tool),
		Policy:   NewAllowList("call_api"),
		Env:      &env,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	res, err := agent.Prompt(context.Background(), "go")
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if tool.called != 0 {
		t.Fatalf("tool must not run when resolution fails, got %d calls", tool.called)
	}
	if len(res.Tools) != 1 || res.Tools[0].Allowed {
		t.Fatalf("expected 1 blocked trace, got %+v", res.Tools)
	}
	var sawBlock bool
	for _, m := range res.Messages {
		if m.Role == RoleTool && strings.Contains(m.Content, "blocked:") {
			sawBlock = true
		}
	}
	if !sawBlock {
		t.Fatal("block reason was not returned to the model")
	}
}

// TestNoCredentialResolverPassesArgsThrough is the default-off path: with no
// resolver wired, arguments reach the tool byte-for-byte unchanged.
func TestNoCredentialResolverPassesArgsThrough(t *testing.T) {
	tool := &recordingTool{name: "call_api"}
	faux := NewFauxProvider(
		AssistantToolCall("c1", "call_api", `{"key":"{{cred:API_KEY}}"}`),
		AssistantText("done"),
	)
	agent, err := New(Config{
		Provider: faux,
		Model:    "test",
		Tools:    NewToolSet(tool),
		Policy:   NewAllowList("call_api"),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := agent.Prompt(context.Background(), "go"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if want := `{"key":"{{cred:API_KEY}}"}`; tool.lastArgs != want {
		t.Fatalf("args should pass through unchanged: want %q, got %q", want, tool.lastArgs)
	}
}
