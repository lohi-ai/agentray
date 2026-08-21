package agentcore

import (
	"context"
	"strings"
	"testing"
)

// The defect these guard: a run that reached a ceiling returned
// lastAssistantText(), which is empty for the ordinary case where every turn was
// a tool call. The caller was handed nothing after paying for every token the run
// spent, and the chat surface rendered that as "the agent never answered".
//
// Every ceiling now ends the same way the budget one already did — one tool-free
// turn where the model says what it found.

// countSteers reports how many injected wrap-up steers are in the history. The
// steer must appear once: two ceilings tripping in one run (a tool budget that
// runs out on the last permitted turn) must not stack instructions on the model.
func countSteers(messages []Message) int {
	n := 0
	for _, m := range messages {
		if m.Role != RoleUser {
			continue
		}
		if strings.Contains(m.Content, "Do not call any more tools") {
			n++
		}
	}
	return n
}

// TestMaxTurnsEndsWithAnAnswerNotSilence is the reported failure: twelve turns of
// real work, then "Something went wrong. Try again."
func TestMaxTurnsEndsWithAnAnswerNotSilence(t *testing.T) {
	work := &echoTool{name: "run_sql"}
	faux := NewFauxProvider(
		AssistantToolCall("c1", "run_sql", `{"q":1}`),
		AssistantToolCall("c2", "run_sql", `{"q":2}`),
		AssistantToolCall("c3", "run_sql", `{"q":3}`),
		// The borrowed turn. A model handed an empty tool list answers in text.
		AssistantText("Signup to activation is the weakest step; I would check the email step next."),
		// Never reached: a fourth work call would mean the run walked past its cap.
		AssistantToolCall("c4", "run_sql", `{"q":4}`),
	)
	limits := DefaultLimits()
	limits.MaxTurns = 3
	agent, err := New(Config{Provider: faux, Model: "test", Tools: NewToolSet(work), Policy: NewAllowList("run_sql"), Limits: &limits})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	res, err := agent.Prompt(context.Background(), "what is my weakest step")
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if res.Final == "" {
		t.Fatal("run hit its turn cap and returned no answer at all — the caller pays for the tokens either way")
	}
	if !strings.Contains(res.Final, "weakest step") {
		t.Fatalf("final = %q, want the wrap-up the model wrote", res.Final)
	}
	if res.StopReason != "max_turns" {
		t.Fatalf("stop reason = %q, want max_turns — the wrap-up must not disguise which ceiling stopped the run", res.StopReason)
	}
	if work.called != 3 {
		t.Fatalf("tool called %d times, want 3: the wrap-up turn is tool-free and must not buy extra work", work.called)
	}
	if n := countSteers(res.Messages); n != 1 {
		t.Fatalf("steer injected %d times, want exactly 1", n)
	}
}

// TestMaxToolCallsEndsWithAnAnswerNotSilence covers the other work ceiling. It
// used to return from inside the batch guard, so the blocked tool results were
// the last thing in the history and nothing ever read them.
func TestMaxToolCallsEndsWithAnAnswerNotSilence(t *testing.T) {
	work := &echoTool{name: "run_sql"}
	faux := NewFauxProvider(
		AssistantToolCall("c1", "run_sql", `{"q":1}`),
		AssistantToolCall("c2", "run_sql", `{"q":2}`),
		AssistantToolCall("c3", "run_sql", `{"q":3}`), // blocked: budget spent
		AssistantText("Two queries in, activation looks like the gap. I ran out of queries before confirming it."),
	)
	limits := DefaultLimits()
	limits.MaxToolCalls = 2
	agent, err := New(Config{Provider: faux, Model: "test", Tools: NewToolSet(work), Policy: NewAllowList("run_sql"), Limits: &limits})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	res, err := agent.Prompt(context.Background(), "what is my weakest step")
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if !strings.Contains(res.Final, "activation looks like the gap") {
		t.Fatalf("final = %q, want the wrap-up written after the tool budget ran out", res.Final)
	}
	if res.StopReason != "max_tool_calls" {
		t.Fatalf("stop reason = %q, want max_tool_calls", res.StopReason)
	}
	if work.called != 2 {
		t.Fatalf("tool called %d times, want 2 — the blocked batch must not execute", work.called)
	}
}

// TestWrapUpIsHonestAboutHavingNothing pins the instruction, not just the
// mechanism. A summary that invents a conclusion from a run that never reached
// one is worse than the silence it replaced, so the steer has to tell the model
// it may say it could not answer.
func TestWrapUpIsHonestAboutHavingNothing(t *testing.T) {
	for _, reason := range []string{"max_turns", "max_tool_calls"} {
		steer := finalizeSteer(reason)
		if !strings.Contains(steer, "Do not call any more tools") {
			t.Fatalf("%s steer must strip the model of tool use in words as well as schema: %q", reason, steer)
		}
		if !strings.Contains(steer, "not enough to answer") {
			t.Fatalf("%s steer must license an honest 'I could not answer': %q", reason, steer)
		}
	}
	if finalizeSteer("budget_exhausted") != budgetExhaustedSteer {
		t.Fatal("an unrecognised reason must fall back to the budget steer, not to empty text")
	}
}

// TestWrapUpBorrowsExactlyOneTurn is the containment guard. The wrap-up strips
// the tool schemas, but a provider is free to ignore that; if the grant were
// expressed against the turn budget it would renew forever under bookkeeping
// refunds, which is a hang rather than a wasted turn.
func TestWrapUpBorrowsExactlyOneTurn(t *testing.T) {
	work := &echoTool{name: "run_sql"}
	script := make([]ChatResponse, 0, 40)
	for i := 0; i < 40; i++ {
		script = append(script, AssistantToolCall("c", "run_sql", `{"q":1}`))
	}
	faux := NewFauxProvider(script...)
	limits := DefaultLimits()
	limits.MaxTurns = 4
	limits.MaxToolCalls = 100
	agent, err := New(Config{Provider: faux, Model: "test", Tools: NewToolSet(work), Policy: NewAllowList("run_sql"), Limits: &limits})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	res, err := agent.Prompt(context.Background(), "never stop")
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if res.Turns != limits.MaxTurns+1 {
		t.Fatalf("turns = %d, want %d — the wrap-up is one borrowed turn, not an extension", res.Turns, limits.MaxTurns+1)
	}
	if res.StopReason != "max_turns" {
		t.Fatalf("stop reason = %q, want max_turns", res.StopReason)
	}
}

// TestCleanFinishOnTheLastTurnKeepsItsOwnAnswer is why the wrap-up borrows a turn
// instead of reserving one. Reserving would spend every run's last working turn
// buying an answer only overrunning runs need, and would overwrite a complete
// answer with a summary of itself.
func TestCleanFinishOnTheLastTurnKeepsItsOwnAnswer(t *testing.T) {
	work := &echoTool{name: "run_sql"}
	faux := NewFauxProvider(
		AssistantToolCall("c1", "run_sql", `{"q":1}`),
		AssistantToolCall("c2", "run_sql", `{"q":2}`),
		AssistantText("Activation is the weakest step, at 24%."),
	)
	limits := DefaultLimits()
	limits.MaxTurns = 3
	agent, err := New(Config{Provider: faux, Model: "test", Tools: NewToolSet(work), Policy: NewAllowList("run_sql"), Limits: &limits})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	res, err := agent.Prompt(context.Background(), "what is my weakest step")
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if res.Final != "Activation is the weakest step, at 24%." {
		t.Fatalf("final = %q — a run that finished on its last turn must keep its own answer", res.Final)
	}
	if res.StopReason == "max_turns" {
		t.Fatal("a clean finish on the last permitted turn is not a ceiling stop")
	}
	if n := countSteers(res.Messages); n != 0 {
		t.Fatalf("steer injected %d times into a run that finished on its own", n)
	}
}
