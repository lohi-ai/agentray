package agentcore

import (
	"context"
	"strings"
	"sync"
	"testing"
)

// TestContextHookRewritesRequest verifies a context hook can rewrite the message
// list the model reasons over, without mutating the persisted run history.
func TestContextHookRewritesRequest(t *testing.T) {
	faux := NewFauxProvider(AssistantText("done"))

	redact := func(_ context.Context, msgs []Message) []Message {
		out := make([]Message, len(msgs))
		copy(out, msgs)
		for i := range out {
			out[i].Content = strings.ReplaceAll(out[i].Content, "SECRET", "[redacted]")
		}
		return out
	}

	agent, err := New(Config{
		Provider: faux,
		Model:    "test",
		Hooks:    Hooks{Context: []ContextHook{redact}},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	res, err := agent.Prompt(context.Background(), "my SECRET token")
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}

	// The provider saw the redacted view.
	var sawContents []string
	for _, req := range faux.Recorded {
		for _, m := range req.Messages {
			if m.Role == RoleUser {
				sawContents = append(sawContents, m.Content)
			}
		}
	}
	if len(sawContents) == 0 || strings.Contains(strings.Join(sawContents, "|"), "SECRET") {
		t.Fatalf("provider should have seen redacted content, saw %v", sawContents)
	}
	if !strings.Contains(strings.Join(sawContents, "|"), "[redacted]") {
		t.Fatalf("provider should have seen [redacted], saw %v", sawContents)
	}
	// Persisted history is untouched: the original user message is intact.
	var keptOriginal bool
	for _, m := range res.Messages {
		if m.Role == RoleUser && strings.Contains(m.Content, "SECRET") {
			keptOriginal = true
		}
	}
	if !keptOriginal {
		t.Fatalf("context hook must not mutate persisted history: %+v", res.Messages)
	}
}

// TestBeforeProviderRequestHookRuns verifies a before_provider_request hook sees
// and can rewrite the assembled request.
func TestBeforeProviderRequestHookRuns(t *testing.T) {
	faux := NewFauxProvider(AssistantText("ok"))

	bump := func(_ context.Context, req ChatRequest) ChatRequest {
		req.MaxTokens = 4096
		return req
	}
	agent, err := New(Config{
		Provider: faux,
		Model:    "test",
		Hooks:    Hooks{BeforeProviderRequest: []ProviderRequestHook{bump}},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := agent.Prompt(context.Background(), "go"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if len(faux.Recorded) == 0 || faux.Recorded[len(faux.Recorded)-1].MaxTokens != 4096 {
		t.Fatalf("before_provider_request hook did not rewrite the request, recorded %+v", faux.Recorded)
	}
}

// TestPanickingObserverDoesNotAbort verifies the default (continue) error policy:
// a panicking message_end observer is attributed via OnError and the run finishes
// normally.
func TestPanickingObserverDoesNotAbort(t *testing.T) {
	var mu sync.Mutex
	var errors []string
	panicker := func(context.Context, Message) { panic("boom") }
	agent, err := New(Config{
		Provider: NewFauxProvider(AssistantText("survived")),
		Model:    "test",
		Hooks: Hooks{
			MessageEnd: []MessageEndHook{panicker},
			OnError: func(source string, err error) {
				mu.Lock()
				errors = append(errors, source+": "+err.Error())
				mu.Unlock()
			},
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	res, err := agent.Prompt(context.Background(), "go")
	if err != nil {
		t.Fatalf("a panicking observer must not abort the run: %v", err)
	}
	if res.Final != "survived" {
		t.Fatalf("run should have completed, final=%q", res.Final)
	}
	if len(errors) == 0 || !strings.Contains(errors[0], "message_end[0]") {
		t.Fatalf("panic should be attributed to its source, got %v", errors)
	}
}

// TestHookThrowPolicyAborts verifies the opt-in throw policy surfaces a hook
// failure from the loop instead of swallowing it.
func TestHookThrowPolicyAborts(t *testing.T) {
	boom := func(context.Context, Message) { panic("fatal") }
	agent, err := New(Config{
		Provider: NewFauxProvider(AssistantText("never")),
		Model:    "test",
		Hooks: Hooks{
			MessageEnd:  []MessageEndHook{boom},
			ErrorPolicy: HookThrow,
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = agent.Prompt(context.Background(), "go")
	if err == nil {
		t.Fatalf("HookThrow must surface the hook error")
	}
	if !strings.Contains(err.Error(), "message_end[0]") {
		t.Fatalf("aborted error should attribute the source, got %v", err)
	}
}

// TestDeterministicEmitOrder verifies multiple handlers of one event fire in
// registration order, every turn.
func TestDeterministicEmitOrder(t *testing.T) {
	var order []string
	mk := func(tag string) MessageEndHook {
		return func(context.Context, Message) { order = append(order, tag) }
	}
	// Two turns: a tool call then a final answer.
	faux := NewFauxProvider(
		AssistantToolCall("c1", "noop", `{}`),
		AssistantText("done"),
	)
	agent, err := New(Config{
		Provider: faux,
		Model:    "test",
		Tools:    NewToolSet(noopTool{}),
		Policy:   NewAllowList("noop"),
		Hooks:    Hooks{MessageEnd: []MessageEndHook{mk("a"), mk("b"), mk("c")}},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := agent.Prompt(context.Background(), "go"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	// Two assistant messages × three observers, each in a,b,c order.
	want := []string{"a", "b", "c", "a", "b", "c"}
	if strings.Join(order, ",") != strings.Join(want, ",") {
		t.Fatalf("emit order = %v, want %v", order, want)
	}
}

// TestReentrantHookCallsIntoAgent verifies a hook may call back into the agent
// (run a nested sub-agent) without deadlock, and the outer run still completes.
func TestReentrantHookCallsIntoAgent(t *testing.T) {
	// The nested agent the hook drives.
	inner, err := New(Config{
		Provider: NewFauxProvider(AssistantText("inner-done")),
		Model:    "test",
	})
	if err != nil {
		t.Fatalf("New inner: %v", err)
	}

	var nestedFinal string
	reentrant := func(ctx context.Context, _ Message) {
		r, ierr := inner.Prompt(ctx, "sub-task")
		if ierr == nil {
			nestedFinal = r.Final
		}
	}
	outer, err := New(Config{
		Provider: NewFauxProvider(AssistantText("outer-done")),
		Model:    "test",
		Hooks:    Hooks{MessageEnd: []MessageEndHook{reentrant}},
	})
	if err != nil {
		t.Fatalf("New outer: %v", err)
	}
	res, err := outer.Prompt(context.Background(), "task")
	if err != nil {
		t.Fatalf("reentrant hook must not deadlock or fail: %v", err)
	}
	if res.Final != "outer-done" {
		t.Fatalf("outer run final = %q", res.Final)
	}
	if nestedFinal != "inner-done" {
		t.Fatalf("nested agent did not run from the hook: %q", nestedFinal)
	}
}

// TestPanickingBeforeHookDoesNotExecuteToolUnderThrow verifies that under
// HookThrow a panicking before-tool-call hook refuses the call (blocked) rather
// than letting it execute.
func TestPanickingBeforeHookDoesNotExecuteToolUnderThrow(t *testing.T) {
	var ran bool
	tool := funcTool{
		name: "danger",
		run:  func(context.Context, string) (string, error) { ran = true; return "ok", nil },
	}
	boom := func(context.Context, ToolCall) Decision { panic("hook down") }
	agent, err := New(Config{
		Provider: NewFauxProvider(
			AssistantToolCall("c1", "danger", `{}`),
			AssistantText("end"),
		),
		Model:  "test",
		Tools:  NewToolSet(tool),
		Policy: NewAllowList("danger"),
		Hooks: Hooks{
			Before:      []BeforeToolCall{boom},
			ErrorPolicy: HookThrow,
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	res, err := agent.Prompt(context.Background(), "go")
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if ran {
		t.Fatalf("tool must not execute when a before-hook fails under HookThrow")
	}
	// The block reason was fed back to the model as a tool result.
	var sawBlock bool
	for _, m := range res.Messages {
		if m.Role == RoleTool && strings.Contains(m.Content, "blocked") {
			sawBlock = true
		}
	}
	if !sawBlock {
		t.Fatalf("expected a blocked tool result, got %+v", res.Messages)
	}
}

// funcTool is a minimal Tool backed by a func, for tests.
type funcTool struct {
	name string
	run  func(context.Context, string) (string, error)
}

func (f funcTool) Name() string { return f.name }
func (f funcTool) Schema() ToolSchema {
	return ToolSchema{Name: f.name, Description: f.name, Parameters: map[string]any{"type": "object"}}
}
func (f funcTool) Run(ctx context.Context, args string) (string, error) { return f.run(ctx, args) }
