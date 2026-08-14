package observe

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/lohi-ai/agentray/agentcore"
	"github.com/lohi-ai/agentray/agentcore/plugins/repeatguard"
)

// newTracker builds the run-scoped tracker directly, for the unit cases below.
func newTracker(report func(LogInvariantViolation)) *logInvariant {
	return &logInvariant{counts: map[string]int{}, report: report}
}

// echoTool is a trivial always-succeeding tool: these tests are about what
// reaches the log, not about what a tool does.
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

func TestLogInvariant_QuietWhenEverythingIsLogged(t *testing.T) {
	var violations []LogInvariantViolation
	li := newTracker(func(v LogInvariantViolation) { violations = append(violations, v) })

	history := []agentcore.Message{
		{Role: agentcore.RoleUser, Content: "hello"},
		{Role: agentcore.RoleAssistant, Content: "hi"},
	}
	li.noteAll(history)
	li.check(1, history)
	if len(violations) != 0 {
		t.Fatalf("a fully logged history must not report: %+v", violations)
	}
}

func TestLogInvariant_CatchesAnUnloggedInjection(t *testing.T) {
	var violations []LogInvariantViolation
	li := newTracker(func(v LogInvariantViolation) { violations = append(violations, v) })

	li.note(agentcore.Message{Role: agentcore.RoleUser, Content: "hello"})
	// A nudge reached the model but was never persisted — the exact bug the
	// invariant exists to catch.
	history := []agentcore.Message{
		{Role: agentcore.RoleUser, Content: "hello"},
		{Role: agentcore.RoleUser, Content: "keep going, you did not finish"},
	}
	li.check(3, history)
	if len(violations) != 1 {
		t.Fatalf("expected exactly one violation, got %d", len(violations))
	}
	v := violations[0]
	if v.Turn != 3 || v.Role != agentcore.RoleUser || !strings.Contains(v.Excerpt, "keep going") {
		t.Fatalf("violation does not identify the message: %+v", v)
	}
	if !strings.Contains(v.Error(), "never logged") {
		t.Fatalf("error text = %q", v.Error())
	}
}

func TestLogInvariant_CountsDuplicatesSeparately(t *testing.T) {
	var violations []LogInvariantViolation
	li := newTracker(func(v LogInvariantViolation) { violations = append(violations, v) })

	m := agentcore.Message{Role: agentcore.RoleUser, Content: "same text"}
	li.note(m) // logged once
	li.check(1, []agentcore.Message{m, m})
	if len(violations) != 1 {
		t.Fatal("a second identical message must not hide behind the first")
	}
	// Once both are logged, it goes quiet.
	violations = nil
	li.note(m)
	li.check(1, []agentcore.Message{m, m})
	if len(violations) != 0 {
		t.Fatalf("both copies are logged now: %+v", violations)
	}
}

func TestLogInvariant_ToolResultsWithSameTextButDifferentCallsAreDistinct(t *testing.T) {
	var violations []LogInvariantViolation
	li := newTracker(func(v LogInvariantViolation) { violations = append(violations, v) })

	logged := agentcore.Message{Role: agentcore.RoleTool, Name: "run_sql", ToolCallID: "call_1", Content: "0 rows"}
	unlogged := agentcore.Message{Role: agentcore.RoleTool, Name: "run_sql", ToolCallID: "call_2", Content: "0 rows"}
	li.note(logged)
	li.check(1, []agentcore.Message{logged, unlogged})
	if len(violations) != 1 {
		t.Fatal("identical text under a different call id is a different message")
	}
}

func TestLogInvariant_RebaseAcceptsADeliberateRewrite(t *testing.T) {
	var violations []LogInvariantViolation
	li := newTracker(func(v LogInvariantViolation) { violations = append(violations, v) })

	li.noteAll([]agentcore.Message{
		{Role: agentcore.RoleUser, Content: "a"},
		{Role: agentcore.RoleAssistant, Content: "b"},
	})
	// Compaction replaces the transcript; the log carries the bracket.
	compacted := []agentcore.Message{{Role: agentcore.RoleUser, Content: "summary of a and b"}}
	li.rebase(compacted)
	li.check(2, compacted)
	if len(violations) != 0 {
		t.Fatalf("a rebased history must not report: %+v", violations)
	}
	// And the pre-rebase messages are no longer accepted, so a rewrite that
	// silently kept stale history would still be caught.
	li.check(2, []agentcore.Message{{Role: agentcore.RoleUser, Content: "a"}})
	if len(violations) != 1 {
		t.Fatal("rebase must replace the baseline, not extend it")
	}
}

func TestLogInvariant_ReportsAtMostOnePerCheck(t *testing.T) {
	var violations []LogInvariantViolation
	li := newTracker(func(v LogInvariantViolation) { violations = append(violations, v) })
	li.check(1, []agentcore.Message{
		{Role: agentcore.RoleUser, Content: "one"},
		{Role: agentcore.RoleUser, Content: "two"},
		{Role: agentcore.RoleUser, Content: "three"},
	})
	if len(violations) != 1 {
		t.Fatalf("a systemic divergence must not flood the sink: got %d reports", len(violations))
	}
}

// "Off" is expressed by declining the run, never by installing a tracker that
// does nothing: an extension the loop holds but that never acts is
// indistinguishable, from the outside, from one that is broken.
func TestLogInvariant_DeclinesRunsItCannotCheck(t *testing.T) {
	cases := []struct {
		name   string
		plugin LogInvariant
		info   agentcore.RunInfo
	}{
		{"no reporter", LogInvariant{}, agentcore.RunInfo{Durable: true}},
		{"no durable log", LogInvariant{Report: func(LogInvariantViolation) {}}, agentcore.RunInfo{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ext, err := tc.plugin.BeginRun(context.Background(), tc.info)
			if err != nil {
				t.Fatalf("declining is not an error: %v", err)
			}
			if ext != nil {
				t.Fatal("expected the plugin to decline the run")
			}
		})
	}
}

// TestLogInvariant_EndToEndOnADurableRun drives the real loop and proves the
// wiring: every message the loop injects on a durable run is logged, so a clean
// run reports nothing.
func TestLogInvariant_EndToEndCleanRun(t *testing.T) {
	store := agentcore.NewMemorySessionStore()
	var violations []LogInvariantViolation

	provider := &agentcore.FauxProvider{Responses: []agentcore.ChatResponse{
		{Message: agentcore.Message{Role: agentcore.RoleAssistant, Content: "", ToolCalls: []agentcore.ToolCall{{ID: "c1", Name: "echo", Arguments: `{"text":"hi"}`}}}},
		{Message: agentcore.Message{Role: agentcore.RoleAssistant, Content: "done"}},
	}}
	agent, err := agentcore.New(agentcore.Config{
		Provider:  provider,
		Model:     "m",
		Tools:     agentcore.NewToolSet(&echoTool{name: "echo"}),
		Policy:    agentcore.NewAllowList("echo"),
		Session:   store,
		SessionID: "s1",
		Extensions: []agentcore.ExtensionFactory{
			LogInvariant{Report: func(v LogInvariantViolation) { violations = append(violations, v) }},
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := agent.Prompt(context.Background(), "say hi"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if len(violations) != 0 {
		t.Fatalf("a clean durable run must log everything it shows the model: %+v", violations)
	}
}

// TestLogInvariant_EndToEndWithRepeatNudgeStaysClean is the regression guard for
// the newest injector: the repeat-tool reminder writes a synthetic user message
// into the live history, and this proves it reaches the durable log too. If a
// future change drops that appendEntry, this test fails instead of resume
// quietly diverging.
func TestLogInvariant_EndToEndWithRepeatNudgeStaysClean(t *testing.T) {
	store := agentcore.NewMemorySessionStore()
	var violations []LogInvariantViolation

	// Three identical tool calls in a row trip the repeat guard, which injects a
	// synthetic user message. If that injection were not persisted, this test
	// would fail — which is exactly the regression guard we want.
	repeat := agentcore.ChatResponse{Message: agentcore.Message{Role: agentcore.RoleAssistant, ToolCalls: []agentcore.ToolCall{{ID: "c", Name: "echo", Arguments: `{"text":"same"}`}}}}
	provider := &agentcore.FauxProvider{Responses: []agentcore.ChatResponse{
		repeat, repeat, repeat,
		{Message: agentcore.Message{Role: agentcore.RoleAssistant, Content: "done"}},
	}}
	agent, err := agentcore.New(agentcore.Config{
		Provider:  provider,
		Model:     "m",
		Tools:     agentcore.NewToolSet(&echoTool{name: "echo"}),
		Policy:    agentcore.NewAllowList("echo"),
		Session:   store,
		SessionID: "s1",
		Extensions: []agentcore.ExtensionFactory{
			repeatguard.At(3),
			LogInvariant{Report: func(v LogInvariantViolation) { violations = append(violations, v) }},
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	res, err := agent.Prompt(context.Background(), "go")
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if len(violations) != 0 {
		t.Fatalf("the repeat-guard nudge must be persisted like a steer: %+v", violations)
	}
	// And it really did fire, so the test is not vacuous.
	found := false
	for _, m := range res.Messages {
		if m.Role == agentcore.RoleUser && strings.Contains(m.Content, "repeating the exact same tool call") {
			found = true
		}
	}
	if !found {
		t.Fatal("the repeat nudge never fired — this test proves nothing without it")
	}
	// The log carries it too.
	log, _ := store.Log(context.Background(), "s1")
	loggedNudge := false
	for _, e := range log {
		if e.Kind == agentcore.EntryMessage && e.Message != nil && strings.Contains(e.Message.Content, "repeating the exact same tool call") {
			loggedNudge = true
		}
	}
	if !loggedNudge {
		t.Fatal("nudge is visible to the model but missing from the durable log")
	}
}
