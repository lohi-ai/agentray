package agentcore

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// echoTool is a trivial tool that returns its arguments, recording invocation.
type echoTool struct {
	name   string
	called int
}

func (e *echoTool) Name() string { return e.name }
func (e *echoTool) Schema() ToolSchema {
	return ToolSchema{Name: e.name, Description: "echo", Parameters: map[string]any{"type": "object"}}
}
func (e *echoTool) Run(_ context.Context, args string) (string, error) {
	e.called++
	return "ran " + e.name + " with " + args, nil
}

// TestLoopRunsPermittedTool drives a faux two-turn script: turn 1 calls the
// tool, turn 2 produces the final answer. Verifies the tool executed and the
// trace recorded it as allowed.
func TestLoopRunsPermittedTool(t *testing.T) {
	tool := &echoTool{name: "run_query"}
	faux := NewFauxProvider(
		AssistantToolCall("c1", "run_query", `{"sql":"select 1"}`),
		AssistantText("done: the query returned 1"),
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
	if tool.called != 1 {
		t.Fatalf("expected tool called once, got %d", tool.called)
	}
	if res.Final != "done: the query returned 1" {
		t.Fatalf("unexpected final: %q", res.Final)
	}
	if len(res.Tools) != 1 || !res.Tools[0].Allowed {
		t.Fatalf("expected 1 allowed tool trace, got %+v", res.Tools)
	}
}

// TestPermissionGateBlocks verifies the beforeToolCall permission gate denies a
// tool the policy doesn't permit, feeds the reason back to the model (not a
// silent failure), and records allowed=false.
func TestPermissionGateBlocks(t *testing.T) {
	tool := &echoTool{name: "write_dashboard"}
	faux := NewFauxProvider(
		AssistantToolCall("c1", "write_dashboard", `{}`),
		AssistantText("understood, I cannot write dashboards"),
	)
	agent, err := New(Config{
		Provider: faux,
		Model:    "test",
		Tools:    NewToolSet(tool),
		Policy:   NewAllowList(), // permits nothing
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	res, err := agent.Prompt(context.Background(), "build a dashboard")
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if tool.called != 0 {
		t.Fatalf("blocked tool must not execute, got %d calls", tool.called)
	}
	if len(res.Tools) != 1 || res.Tools[0].Allowed {
		t.Fatalf("expected 1 blocked trace, got %+v", res.Tools)
	}
	// The block reason is returned to the model as a tool message.
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

// TestPermittedToolsFiltersSchemas verifies a disabled tool never reaches the
// model's advertised schema list.
func TestPermittedToolsFiltersSchemas(t *testing.T) {
	faux := NewFauxProvider(AssistantText("hi"))
	agent, _ := New(Config{
		Provider: faux,
		Model:    "test",
		Tools:    NewToolSet(&echoTool{name: "a"}, &echoTool{name: "b"}),
		Policy:   NewAllowList("a"),
	})
	if _, err := agent.Prompt(context.Background(), "go"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	req := faux.Recorded[0]
	if len(req.Tools) != 1 || req.Tools[0].Name != "a" {
		t.Fatalf("expected only tool 'a' advertised, got %+v", req.Tools)
	}
}

// TestPromptStreamEmitsTokens drives the streaming path: turn 1 calls a tool,
// turn 2 answers. It asserts the streamed tokens concatenate to the final
// answer and that a tool StreamEvent fired for the executed call (parity with
// the persisted trace).
func TestPromptStreamEmitsTokens(t *testing.T) {
	tool := &echoTool{name: "run_query"}
	faux := NewFauxProvider(
		AssistantToolCall("c1", "run_query", `{"sql":"select 1"}`),
		AssistantText("the query returned exactly one"),
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

	var tokens strings.Builder
	var toolEvents int
	res, err := agent.PromptStream(context.Background(), "run a query", func(ev StreamEvent) {
		switch ev.Type {
		case StreamToken:
			tokens.WriteString(ev.Token)
		case StreamTool:
			toolEvents++
			if ev.Tool == nil || ev.Tool.Tool != "run_query" {
				t.Errorf("unexpected tool event: %+v", ev.Tool)
			}
		}
	})
	if err != nil {
		t.Fatalf("PromptStream: %v", err)
	}
	if res.Final != "the query returned exactly one" {
		t.Fatalf("unexpected final: %q", res.Final)
	}
	// Streamed tokens must reconstruct the final answer exactly.
	if tokens.String() != res.Final {
		t.Fatalf("streamed tokens %q != final %q", tokens.String(), res.Final)
	}
	if toolEvents != 1 {
		t.Fatalf("expected 1 tool stream event, got %d", toolEvents)
	}
	if tool.called != 1 {
		t.Fatalf("expected tool executed once, got %d", tool.called)
	}
}

// barrierTool is a parallel-eligible tool that signals arrival then blocks until
// released, so a test can prove two such tools in one turn ran concurrently:
// under sequential execution the first would never see its peer arrive and would
// time out.
type barrierTool struct {
	name    string
	arrived chan<- string
	release <-chan struct{}
}

func (b *barrierTool) Name() string   { return b.name }
func (b *barrierTool) Parallel() bool { return true }
func (b *barrierTool) Schema() ToolSchema {
	return ToolSchema{Name: b.name, Description: "barrier", Parameters: map[string]any{"type": "object"}}
}
func (b *barrierTool) Run(_ context.Context, _ string) (string, error) {
	b.arrived <- b.name
	select {
	case <-b.release:
		return "ran " + b.name, nil
	case <-time.After(2 * time.Second):
		return "", errors.New("timeout: peer never ran concurrently")
	}
}

// TestParallelToolsRunConcurrently verifies that when every tool call in a turn
// targets a ParallelTool, the batch executes concurrently and results are still
// applied in the model's original order.
func TestParallelToolsRunConcurrently(t *testing.T) {
	arrived := make(chan string, 2)
	release := make(chan struct{})
	a := &barrierTool{name: "qa", arrived: arrived, release: release}
	b := &barrierTool{name: "qb", arrived: arrived, release: release}

	// Once both tools have arrived, release them — proving concurrency.
	go func() {
		<-arrived
		<-arrived
		close(release)
	}()

	twoCalls := ChatResponse{
		Message: Message{Role: RoleAssistant, ToolCalls: []ToolCall{
			{ID: "c1", Name: "qa", Arguments: "{}"},
			{ID: "c2", Name: "qb", Arguments: "{}"},
		}},
		StopReason: "tool_calls",
	}
	faux := NewFauxProvider(twoCalls, AssistantText("both done"))
	agent, err := New(Config{
		Provider: faux,
		Model:    "test",
		Tools:    NewToolSet(a, b),
		Policy:   NewAllowList("qa", "qb"),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	res, err := agent.Prompt(context.Background(), "run both")
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if len(res.Tools) != 2 {
		t.Fatalf("expected 2 tool traces, got %d", len(res.Tools))
	}
	// Order preserved despite concurrent execution.
	if res.Tools[0].Tool != "qa" || res.Tools[1].Tool != "qb" {
		t.Fatalf("tool order not preserved: %+v", res.Tools)
	}
	for _, tr := range res.Tools {
		if tr.Error != "" {
			t.Fatalf("tool %q errored (ran sequentially?): %s", tr.Tool, tr.Error)
		}
	}
}

// prepareTool rewrites its arguments before validation/execution and records
// what Run actually received.
type prepareTool struct {
	gotArgs string
}

func (p *prepareTool) Name() string { return "prep" }
func (p *prepareTool) Schema() ToolSchema {
	return ToolSchema{Name: "prep", Description: "prep", Parameters: map[string]any{"type": "object"}}
}
func (p *prepareTool) PrepareArguments(raw string) string { return `{"normalized":true}` }
func (p *prepareTool) Run(_ context.Context, args string) (string, error) {
	p.gotArgs = args
	return "ok", nil
}

// TestPrepareArgumentsNormalizes verifies ArgPreparer runs before execution and
// the normalized args reach both the tool and the persisted trace.
func TestPrepareArgumentsNormalizes(t *testing.T) {
	tool := &prepareTool{}
	faux := NewFauxProvider(
		AssistantToolCall("c1", "prep", `{"raw":1}`),
		AssistantText("done"),
	)
	agent, _ := New(Config{
		Provider: faux,
		Model:    "test",
		Tools:    NewToolSet(tool),
		Policy:   NewAllowList("prep"),
	})
	if _, err := agent.Prompt(context.Background(), "go"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if tool.gotArgs != `{"normalized":true}` {
		t.Fatalf("tool did not receive normalized args: %q", tool.gotArgs)
	}
}

// keyFaux is a FauxProvider that also implements KeyUpdater, recording the keys
// pushed to it so a test can confirm per-turn refresh.
type keyFaux struct {
	*FauxProvider
	mu   sync.Mutex
	keys []string
}

func (k *keyFaux) UpdateAPIKey(key string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.keys = append(k.keys, key)
}

// TestRefreshKeyAppliedPerTurn verifies the per-turn resolver's non-empty result
// is pushed into a KeyUpdater provider before the turn runs.
func TestRefreshKeyAppliedPerTurn(t *testing.T) {
	kf := &keyFaux{FauxProvider: NewFauxProvider(AssistantText("hi"))}
	agent, err := New(Config{
		Provider: kf,
		Model:    "test",
		RefreshKey: func(_ context.Context, provider string) (string, error) {
			return "rotated-" + provider, nil
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := agent.Prompt(context.Background(), "go"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	kf.mu.Lock()
	defer kf.mu.Unlock()
	if len(kf.keys) != 1 || kf.keys[0] != "rotated-faux" {
		t.Fatalf("expected one refreshed key 'rotated-faux', got %v", kf.keys)
	}
}

// TestCancelledContextStopsBeforeProvider verifies an already-cancelled context
// aborts the run before any provider call, with stop reason "aborted".
func TestCancelledContextStopsBeforeProvider(t *testing.T) {
	faux := NewFauxProvider(AssistantText("should not be reached"))
	agent, _ := New(Config{Provider: faux, Model: "test"})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res, err := agent.Prompt(ctx, "go")
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if res.StopReason != "aborted" {
		t.Fatalf("expected stop reason 'aborted', got %q", res.StopReason)
	}
	if len(faux.Recorded) != 0 {
		t.Fatalf("provider must not be called on a cancelled context, got %d calls", len(faux.Recorded))
	}
}

// errorProvider always fails, counting its calls — used to drive the escalation
// ladder.
type errorProvider struct {
	name  string
	calls int
}

func (e *errorProvider) Name() string        { return e.name }
func (e *errorProvider) SupportsTools() bool { return true }
func (e *errorProvider) Chat(context.Context, ChatRequest) (ChatResponse, error) {
	e.calls++
	return ChatResponse{}, errors.New("provider down")
}
func (e *errorProvider) Stream(context.Context, ChatRequest) (<-chan ChatDelta, error) {
	e.calls++
	return nil, errors.New("provider down")
}

// TestEscalationFallsBackOnError verifies that when the primary rung errors the
// loop retries the turn on the next rung, succeeds, and uses that rung's model.
func TestEscalationFallsBackOnError(t *testing.T) {
	bad := &errorProvider{name: "lite"}
	good := NewFauxProvider(AssistantText("recovered"))
	agent, err := New(Config{
		Provider:   bad,
		Model:      "lite-model",
		Escalation: []ModelRung{{Provider: good, Model: "pro-model"}},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	res, err := agent.Prompt(context.Background(), "go")
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if res.Final != "recovered" {
		t.Fatalf("expected fallback answer, got %q", res.Final)
	}
	if bad.calls != 1 {
		t.Fatalf("expected primary tried once, got %d", bad.calls)
	}
	if len(good.Recorded) != 1 || good.Recorded[0].Model != "pro-model" {
		t.Fatalf("expected fallback rung used with its own model, got %+v", good.Recorded)
	}
}

// TestEscalationExhaustedReturnsError verifies that when every rung fails the run
// surfaces the error rather than looping forever, having tried each rung once.
func TestEscalationExhaustedReturnsError(t *testing.T) {
	bad1 := &errorProvider{name: "a"}
	bad2 := &errorProvider{name: "b"}
	agent, _ := New(Config{
		Provider:   bad1,
		Model:      "a",
		Escalation: []ModelRung{{Provider: bad2, Model: "b"}},
	})
	if _, err := agent.Prompt(context.Background(), "go"); err == nil {
		t.Fatal("expected an error when the whole ladder fails")
	}
	if bad1.calls != 1 || bad2.calls != 1 {
		t.Fatalf("expected each rung tried once, got %d and %d", bad1.calls, bad2.calls)
	}
}

// panicTool always panics, simulating a buggy tool (nil deref, etc.).
type panicTool struct{ name string }

func (p *panicTool) Name() string { return p.name }
func (p *panicTool) Schema() ToolSchema {
	return ToolSchema{Name: p.name, Description: "panics", Parameters: map[string]any{"type": "object"}}
}
func (p *panicTool) Run(context.Context, string) (string, error) {
	panic("boom")
}

// TestPanickingToolDegradesToError verifies a tool that panics is recovered into
// an ordinary error result — the run finishes and the model sees the failure
// rather than the whole run crashing.
func TestPanickingToolDegradesToError(t *testing.T) {
	faux := NewFauxProvider(
		AssistantToolCall("c1", "kaboom", `{}`),
		AssistantText("handled the failure"),
	)
	agent, err := New(Config{
		Provider: faux,
		Model:    "test",
		Tools:    NewToolSet(&panicTool{name: "kaboom"}),
		Policy:   NewAllowList("kaboom"),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	res, err := agent.Prompt(context.Background(), "go")
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if res.Final != "handled the failure" {
		t.Fatalf("expected run to continue past the panic, got %q", res.Final)
	}
	if len(res.Tools) != 1 || !strings.Contains(res.Tools[0].Error, "panicked") {
		t.Fatalf("expected a recovered panic trace, got %+v", res.Tools)
	}
	var sawError bool
	for _, m := range res.Messages {
		if m.Role == RoleTool && strings.Contains(m.Content, "error:") && strings.Contains(m.Content, "panicked") {
			sawError = true
		}
	}
	if !sawError {
		t.Fatal("panic error was not fed back to the model")
	}
}

// flakyTool always errors, counting executions, so a test can prove the circuit
// breaker stops executing it after repeated failures.
type flakyTool struct {
	name   string
	called int
}

func (f *flakyTool) Name() string { return f.name }
func (f *flakyTool) Schema() ToolSchema {
	return ToolSchema{Name: f.name, Description: "flaky", Parameters: map[string]any{"type": "object"}}
}
func (f *flakyTool) Run(context.Context, string) (string, error) {
	f.called++
	return "", errors.New("always fails")
}

// TestCircuitBreakerDisablesFailingTool drives a tool that errors every call. It
// must execute up to the failure threshold, then be disabled for the rest of the
// run: dropped from the advertised schemas and refused (without executing) if the
// model calls it anyway. The run continues to its final answer.
func TestCircuitBreakerDisablesFailingTool(t *testing.T) {
	tool := &flakyTool{name: "flaky"}
	// Four tool-calling turns; the fourth lands after the tool is disabled.
	faux := NewFauxProvider(
		AssistantToolCall("c1", "flaky", `{}`),
		AssistantToolCall("c2", "flaky", `{}`),
		AssistantToolCall("c3", "flaky", `{}`),
		AssistantToolCall("c4", "flaky", `{}`),
		AssistantText("giving up on that tool"),
	)
	agent, err := New(Config{
		Provider: faux,
		Model:    "test",
		Tools:    NewToolSet(tool),
		Policy:   NewAllowList("flaky"),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	res, err := agent.Prompt(context.Background(), "go")
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}

	// Executed exactly maxToolFailures times; the post-disable call was refused.
	if tool.called != maxToolFailures {
		t.Fatalf("expected tool executed %d times, got %d", maxToolFailures, tool.called)
	}

	// The disable note was fed back to the model.
	var sawDisable bool
	for _, m := range res.Messages {
		if strings.Contains(m.Content, "disabled for the rest of this run") {
			sawDisable = true
		}
	}
	if !sawDisable {
		t.Fatal("model was not told the tool was disabled")
	}

	// The post-disable call traced as blocked, not executed.
	last := res.Tools[len(res.Tools)-1]
	if last.Allowed || !strings.Contains(last.Reason, "disabled") {
		t.Fatalf("expected post-disable call refused, got %+v", last)
	}

	// The fourth request (after disabling on turn 3) must not advertise the tool.
	req4 := faux.Recorded[3]
	for _, s := range req4.Tools {
		if s.Name == "flaky" {
			t.Fatal("disabled tool was still advertised to the model")
		}
	}
}

// recoveringTool fails its first failFor calls, then succeeds — a transient
// outage that heals, the common real case the breaker must not over-punish.
type recoveringTool struct {
	name    string
	failFor int
	called  int
}

func (r *recoveringTool) Name() string { return r.name }
func (r *recoveringTool) Schema() ToolSchema {
	return ToolSchema{Name: r.name, Description: "recovers", Parameters: map[string]any{"type": "object"}}
}
func (r *recoveringTool) Run(context.Context, string) (string, error) {
	r.called++
	if r.called <= r.failFor {
		return "", errors.New("transient outage")
	}
	return "ok", nil
}

// TestCircuitBreakerResetsOnSuccess verifies the breaker counts *consecutive*
// failures: a tool that fails a couple times then recovers is never disabled and
// keeps running. Guards against a regression to a cumulative counter.
func TestCircuitBreakerResetsOnSuccess(t *testing.T) {
	tool := &recoveringTool{name: "flaky", failFor: 2} // fails 1-2, succeeds 3-4
	faux := NewFauxProvider(
		AssistantToolCall("c1", "flaky", `{}`),
		AssistantToolCall("c2", "flaky", `{}`),
		AssistantToolCall("c3", "flaky", `{}`),
		AssistantToolCall("c4", "flaky", `{}`),
		AssistantText("worked once it recovered"),
	)
	agent, err := New(Config{
		Provider: faux,
		Model:    "test",
		Tools:    NewToolSet(tool),
		Policy:   NewAllowList("flaky"),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	res, err := agent.Prompt(context.Background(), "go")
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	// All four calls executed — two failures (below threshold) never tripped it,
	// and the success reset the counter.
	if tool.called != 4 {
		t.Fatalf("expected tool executed 4 times, got %d", tool.called)
	}
	for _, m := range res.Messages {
		if strings.Contains(m.Content, "disabled for the rest of this run") {
			t.Fatal("a recovering tool must not be disabled")
		}
	}
}

// pFlaky always errors and is parallel-eligible; pEcho succeeds and is parallel-
// eligible. Together they exercise a fan-out turn where one tool is down.
type pFlaky struct {
	name   string
	called int
}

func (f *pFlaky) Name() string   { return f.name }
func (f *pFlaky) Parallel() bool { return true }
func (f *pFlaky) Schema() ToolSchema {
	return ToolSchema{Name: f.name, Description: "flaky", Parameters: map[string]any{"type": "object"}}
}
func (f *pFlaky) Run(context.Context, string) (string, error) {
	f.called++
	return "", errors.New("endpoint down")
}

type pEcho struct {
	name   string
	called int
}

func (e *pEcho) Name() string   { return e.name }
func (e *pEcho) Parallel() bool { return true }
func (e *pEcho) Schema() ToolSchema {
	return ToolSchema{Name: e.name, Description: "echo", Parameters: map[string]any{"type": "object"}}
}
func (e *pEcho) Run(context.Context, string) (string, error) {
	e.called++
	return "ok", nil
}

// TestCircuitBreakerIsPerToolInParallelBatch drives a turn that fans out two
// parallel tools — one always-failing, one healthy — repeated across turns. The
// breaker must trip only the failing tool: the healthy one keeps executing and
// stays advertised, and trace order within each batch is preserved.
func TestCircuitBreakerIsPerToolInParallelBatch(t *testing.T) {
	bad := &pFlaky{name: "down"}
	good := &pEcho{name: "up"}
	batch := func(c1, c2 string) ChatResponse {
		return ChatResponse{
			Message: Message{Role: RoleAssistant, ToolCalls: []ToolCall{
				{ID: c1, Name: "down", Arguments: "{}"},
				{ID: c2, Name: "up", Arguments: "{}"},
			}},
			StopReason: "tool_calls",
		}
	}
	faux := NewFauxProvider(
		batch("a1", "b1"),
		batch("a2", "b2"),
		batch("a3", "b3"),
		batch("a4", "b4"),
		AssistantText("done with what worked"),
	)
	agent, err := New(Config{
		Provider: faux,
		Model:    "test",
		Tools:    NewToolSet(bad, good),
		Policy:   NewAllowList("down", "up"),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if _, err := agent.Prompt(context.Background(), "go"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	// The failing tool is capped at the threshold; the healthy one runs every turn.
	if bad.called != maxToolFailures {
		t.Fatalf("expected failing tool executed %d times, got %d", maxToolFailures, bad.called)
	}
	if good.called != 4 {
		t.Fatalf("expected healthy tool executed 4 times, got %d", good.called)
	}
	// The healthy tool is still advertised on the post-disable turn; the bad one isn't.
	req4 := faux.Recorded[3]
	var sawUp, sawDown bool
	for _, s := range req4.Tools {
		sawUp = sawUp || s.Name == "up"
		sawDown = sawDown || s.Name == "down"
	}
	if !sawUp || sawDown {
		t.Fatalf("expected only healthy tool advertised on turn 4, sawUp=%v sawDown=%v", sawUp, sawDown)
	}
}

// stuckProvider models a model that never gives up: every turn it re-issues the
// same tool call, regardless of the error fed back. It is the relentless-retry
// case the breaker exists to contain.
type stuckProvider struct {
	tool  string
	calls int
}

func (s *stuckProvider) Name() string        { return "stuck" }
func (s *stuckProvider) SupportsTools() bool  { return true }
func (s *stuckProvider) toolCall() ChatResponse {
	return ChatResponse{
		Message:    Message{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "x", Name: s.tool, Arguments: "{}"}}},
		StopReason: "tool_calls",
	}
}
func (s *stuckProvider) Chat(context.Context, ChatRequest) (ChatResponse, error) {
	s.calls++
	return s.toolCall(), nil
}
func (s *stuckProvider) Stream(context.Context, ChatRequest) (<-chan ChatDelta, error) {
	s.calls++
	ch := make(chan ChatDelta, 2)
	tc := s.toolCall().Message.ToolCalls[0]
	ch <- ChatDelta{ToolCall: &tc}
	ch <- ChatDelta{Done: true, StopReason: "tool_calls"}
	close(ch)
	return ch, nil
}

// TestCircuitBreakerCapsRelentlessModel verifies that when the model never stops
// calling a broken tool, the breaker caps real executions at the threshold and
// the run still terminates cleanly at MaxTurns rather than hammering the tool
// every turn — the exact "stop burning turns on a broken tool" guarantee.
func TestCircuitBreakerCapsRelentlessModel(t *testing.T) {
	tool := &flakyTool{name: "stuck_tool"}
	provider := &stuckProvider{tool: "stuck_tool"}
	maxTurns := 6
	agent, err := New(Config{
		Provider: provider,
		Model:    "test",
		Tools:    NewToolSet(tool),
		Policy:   NewAllowList("stuck_tool"),
		Limits:   &Limits{MaxTurns: maxTurns, MaxToolCalls: 100, MaxToolResultLen: defaultMaxToolResultBytes, MaxContextTokens: defaultContextTokenBudget},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	res, err := agent.Prompt(context.Background(), "go")
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if tool.called != maxToolFailures {
		t.Fatalf("breaker must cap executions at %d even under relentless retries, got %d", maxToolFailures, tool.called)
	}
	if res.StopReason != "max_turns" {
		t.Fatalf("expected run to end at max_turns, got %q", res.StopReason)
	}
}

func TestTruncateBytesIsRuneSafe(t *testing.T) {
	s := strings.Repeat("é", 100) // 2 bytes each
	out := truncateBytes(s, 50)
	if len(out) > 50 {
		t.Fatalf("truncate exceeded budget: %d", len(out))
	}
	if !strings.HasSuffix(out, "[truncated]") {
		t.Fatalf("expected truncation marker, got %q", out)
	}
}
