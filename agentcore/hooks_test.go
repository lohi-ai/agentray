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

// TestBeforeAgentStartRewritesPrompt verifies a before_agent_start hook can
// replace the assembled system prompt for the very first request — the one seam
// PrepareNextTurn (which only runs after a turn completes) cannot reach.
func TestBeforeAgentStartRewritesPrompt(t *testing.T) {
	faux := NewFauxProvider(AssistantText("done"))

	pin := func(_ context.Context, start RunStart) RunStart {
		start.System = start.System + "\n\nHOUSE RULE: cite your sources."
		return start
	}
	agent, err := New(Config{
		Provider:   faux,
		Model:      "test",
		Definition: AgentDefinition{Soul: "you are a test agent"},
		Hooks:      Hooks{BeforeAgentStart: []BeforeAgentStartHook{pin}},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := agent.Prompt(context.Background(), "go"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if len(faux.Recorded) == 0 {
		t.Fatal("provider was never called")
	}
	first := faux.Recorded[0]
	if len(first.Messages) == 0 || first.Messages[0].Role != RoleSystem {
		t.Fatalf("first request should lead with a system message, got %+v", first.Messages)
	}
	if !strings.Contains(first.Messages[0].Content, "HOUSE RULE") {
		t.Fatalf("before_agent_start did not reach the first request: %q", first.Messages[0].Content)
	}
	// The hook appended rather than replaced: the assembled prompt survives.
	if !strings.Contains(first.Messages[0].Content, "you are a test agent") {
		t.Fatalf("hook clobbered the assembled prompt: %q", first.Messages[0].Content)
	}
}

// TestBeforeAgentStartInjectsPersistedMessage verifies an injected seed message
// reaches the model AND the durable log, so a resume replays the same
// conversation the run actually had.
func TestBeforeAgentStartInjectsPersistedMessage(t *testing.T) {
	faux := NewFauxProvider(AssistantText("done"))
	store := newMemSessionStore()

	inject := func(_ context.Context, start RunStart) RunStart {
		start.Messages = append(start.Messages, Message{Role: RoleUser, Content: "also: keep it short"})
		return start
	}
	agent, err := New(Config{
		Provider:  faux,
		Model:     "test",
		Session:   store,
		SessionID: "sess-1",
		Hooks:     Hooks{BeforeAgentStart: []BeforeAgentStartHook{inject}},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := agent.Prompt(context.Background(), "go"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if len(faux.Recorded) == 0 {
		t.Fatal("provider was never called")
	}
	var sawInRequest bool
	for _, m := range faux.Recorded[0].Messages {
		if strings.Contains(m.Content, "keep it short") {
			sawInRequest = true
		}
	}
	if !sawInRequest {
		t.Fatalf("injected message never reached the model: %+v", faux.Recorded[0].Messages)
	}

	entries, err := store.Log(context.Background(), "sess-1")
	if err != nil {
		t.Fatalf("Log: %v", err)
	}
	var sawInLog bool
	for _, e := range entries {
		if e.Kind == EntryMessage && e.Message != nil && strings.Contains(e.Message.Content, "keep it short") {
			sawInLog = true
		}
	}
	if !sawInLog {
		t.Fatalf("injected seed message was not persisted: %+v", entries)
	}
}

// TestBeforeAgentStartEmptyMeansUnchanged pins the per-field "empty means no
// change" convention: a hook that edits only the messages must not have to
// restate the system prompt to keep it.
func TestBeforeAgentStartEmptyMeansUnchanged(t *testing.T) {
	faux := NewFauxProvider(AssistantText("done"))

	onlyMessages := func(_ context.Context, start RunStart) RunStart {
		return RunStart{Messages: append(start.Messages, Message{Role: RoleUser, Content: "extra"})}
	}
	agent, err := New(Config{
		Provider:   faux,
		Model:      "test",
		Definition: AgentDefinition{Soul: "keep me"},
		Hooks:      Hooks{BeforeAgentStart: []BeforeAgentStartHook{onlyMessages}},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := agent.Prompt(context.Background(), "go"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	got := faux.Recorded[0].Messages
	if len(got) == 0 || got[0].Role != RoleSystem || !strings.Contains(got[0].Content, "keep me") {
		t.Fatalf("empty System should have kept the assembled prompt, got %+v", got)
	}
}

// TestTurnHooksFireOnNonStreamedRun is the point of turn hooks: the stream
// events only reach an attached viewer, so metering must not depend on one.
func TestTurnHooksFireOnNonStreamedRun(t *testing.T) {
	faux := NewFauxProvider(
		AssistantToolCall("c1", "noop", `{}`),
		AssistantText("done"),
	)
	var starts, ends []TurnInfo
	agent, err := New(Config{
		Provider: faux,
		Model:    "test",
		Tools:    NewToolSet(&echoTool{name: "noop"}),
		Policy:   NewAllowList("noop"),
		Hooks: Hooks{
			TurnStart: []TurnHook{func(_ context.Context, i TurnInfo) { starts = append(starts, i) }},
			TurnEnd:   []TurnHook{func(_ context.Context, i TurnInfo) { ends = append(ends, i) }},
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	res, err := agent.Prompt(context.Background(), "go") // nil sink: no stream events at all
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if len(starts) != res.Turns || len(ends) != res.Turns {
		t.Fatalf("want %d turn_start and turn_end hooks, got %d/%d", res.Turns, len(starts), len(ends))
	}
	if starts[0].Turn != 1 || ends[len(ends)-1].Turn != res.Turns {
		t.Fatalf("turn numbering wrong: starts=%+v ends=%+v", starts, ends)
	}
	if ends[len(ends)-1].StopReason == "" {
		t.Fatal("turn_end should carry the turn's stop reason")
	}
	if starts[0].StopReason != "" {
		t.Fatal("turn_start must not carry a stop reason")
	}
}

// TestTurnEndFiresOnceOnAbortedTurn verifies a turn that dies mid-flight (a
// provider error) still closes exactly once, so an observer never books a turn
// that started and never ended.
func TestTurnEndFiresOnceOnAbortedTurn(t *testing.T) {
	var ends int
	agent, err := New(Config{
		Provider: errProvider{},
		Model:    "test",
		Retry:    &RetryPolicy{MaxAttempts: 1},
		Hooks: Hooks{
			TurnEnd: []TurnHook{func(context.Context, TurnInfo) { ends++ }},
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := agent.Prompt(context.Background(), "go"); err == nil {
		t.Fatal("want a provider error")
	}
	if ends != 1 {
		t.Fatalf("want exactly one turn_end on an aborted turn, got %d", ends)
	}
}

// TestAgentEndSeesFinalResult verifies agent_end observes the RunResult the
// caller receives — including a failed run's synthesized failure turn.
func TestAgentEndSeesFinalResult(t *testing.T) {
	faux := NewFauxProvider(AssistantText("the answer"))
	var seen RunResult
	var calls int
	agent, err := New(Config{
		Provider: faux,
		Model:    "test",
		Hooks: Hooks{
			AgentEnd: []AgentEndHook{func(_ context.Context, r RunResult) { seen = r; calls++ }},
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	res, err := agent.Prompt(context.Background(), "go")
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if calls != 1 {
		t.Fatalf("agent_end should fire exactly once, got %d", calls)
	}
	if seen.Final != res.Final || seen.Turns != res.Turns {
		t.Fatalf("agent_end saw %+v, caller got %+v", seen, res)
	}
}

// compactableAgent wires an agent whose seeded history is well over its context
// budget, so the loop reaches its compaction decision on turn 1. The history is
// the shared longTranscript fixture minus its own system header (drive supplies
// one).
func compactableAgent(t *testing.T, hooks Hooks) (*Agent, *FauxProvider, []Message) {
	t.Helper()
	faux := NewFauxProvider(AssistantText("summarized"), AssistantText("done"))
	limits := DefaultLimits()
	limits.MaxContextTokens = 4000
	settings := CompactionSettings{KeepRecentTokens: 1000}
	agent, err := New(Config{
		Provider:   faux,
		Model:      "test",
		Limits:     &limits,
		Compaction: &settings,
		Hooks:      hooks,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return agent, faux, longTranscript()[1:]
}

// compacted reports whether a transcript carries a compaction marker — either
// the model summary or the deterministic elide breadcrumb.
func compacted(msgs []Message) bool {
	for _, m := range msgs {
		if strings.Contains(m.Content, summaryMarker) || strings.Contains(m.Content, elideMarker) {
			return true
		}
	}
	return false
}

// TestCompactionFixtureActuallyCompacts is the control for the three
// before_compact tests below: without a hook, this fixture really does compact,
// so "no marker" in those tests means the hook suppressed it.
func TestCompactionFixtureActuallyCompacts(t *testing.T) {
	agent, _, history := compactableAgent(t, Hooks{})
	res, err := agent.Continue(context.Background(), history, "keep going")
	if err != nil {
		t.Fatalf("Continue: %v", err)
	}
	if !compacted(res.Messages) {
		t.Fatal("fixture never compacted; the before_compact tests would be vacuous")
	}
}

// TestBeforeCompactSkipSuppressesCompaction verifies a hook can defer a
// compaction the loop had already decided to run.
func TestBeforeCompactSkipSuppressesCompaction(t *testing.T) {
	var asked int
	agent, _, history := compactableAgent(t, Hooks{
		BeforeCompact: []BeforeCompactHook{func(_ context.Context, req CompactRequest) CompactDecision {
			asked++
			if req.Budget != 4000 {
				t.Errorf("hook should see the run budget, got %d", req.Budget)
			}
			if req.Settings.KeepRecentTokens != 1000 {
				t.Errorf("hook should see the effective settings, got %+v", req.Settings)
			}
			if len(req.Messages) == 0 {
				t.Error("hook should see the transcript about to be compacted")
			}
			return CompactDecision{Skip: true}
		}},
	})
	res, err := agent.Continue(context.Background(), history, "keep going")
	if err != nil {
		t.Fatalf("Continue: %v", err)
	}
	if asked == 0 {
		t.Fatal("before_compact was never consulted")
	}
	if compacted(res.Messages) {
		t.Fatal("compaction ran despite Skip")
	}
}

// TestBeforeCompactReplacesTranscript verifies a consumer-supplied compaction
// wins over the built-in one and costs no summarization call.
func TestBeforeCompactReplacesTranscript(t *testing.T) {
	replacement := []Message{{Role: RoleUser, Content: "my own compaction"}}
	agent, faux, history := compactableAgent(t, Hooks{
		BeforeCompact: []BeforeCompactHook{func(context.Context, CompactRequest) CompactDecision {
			return CompactDecision{Messages: replacement}
		}},
	})
	if _, err := agent.Continue(context.Background(), history, "keep going"); err != nil {
		t.Fatalf("Continue: %v", err)
	}
	// The only recorded request is the reasoning turn — no summarization call was
	// billed — and it reasons over the replacement transcript.
	if len(faux.Recorded) != 1 {
		t.Fatalf("a supplied compaction should bill no extra provider call, got %d", len(faux.Recorded))
	}
	var sawReplacement bool
	for _, m := range faux.Recorded[0].Messages {
		if m.Content == "my own compaction" {
			sawReplacement = true
		}
	}
	if !sawReplacement {
		t.Fatalf("replacement transcript not used: %+v", faux.Recorded[0].Messages)
	}
}

// TestBeforeCompactPanicFallsThrough verifies the safe direction: a broken hook
// cannot suppress compaction, it just loses its say.
func TestBeforeCompactPanicFallsThrough(t *testing.T) {
	var errSource string
	agent, _, history := compactableAgent(t, Hooks{
		BeforeCompact: []BeforeCompactHook{func(context.Context, CompactRequest) CompactDecision {
			panic("hook is broken")
		}},
		OnError: func(source string, _ error) { errSource = source },
	})
	res, err := agent.Continue(context.Background(), history, "keep going")
	if err != nil {
		t.Fatalf("Continue: %v", err)
	}
	if errSource != "before_compact[0]" {
		t.Fatalf("panicking hook should be attributed, got %q", errSource)
	}
	if !compacted(res.Messages) {
		t.Fatal("built-in compaction should still have run")
	}
}
