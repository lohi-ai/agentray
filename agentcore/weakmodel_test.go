package agentcore_test

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/lohi-ai/agentray/agentcore"
	"github.com/lohi-ai/agentray/agentcore/plugins/goal"
)

// What does the harness cost when the model is not very good?
//
// The scale test next door proves the subsystems hold up over five thousand
// calls, but its model is perfect: it follows the completion contract exactly,
// and when it is told to keep going it goes and does something. Every rail in
// that run is therefore measured against a model that never needed it. The rails
// exist for the other case.
//
// The failure this file is about is specific, and it is the one that makes a
// weak model expensive rather than merely wrong. The goal gate is UNCAPPED by
// design — it re-opens a run that finishes without declaring itself done or
// blocked, for as long as it takes — and what makes that affordable is the stall
// breaker that gives up when the model has nothing left to give. That breaker
// used to be a verbatim comparison of consecutive answers, which is a fine proxy
// for a strong model and a broken one for a weak model: a weak model rephrases.
// It says "I have finished the audit", then "The audit is complete", then "I
// believe the work is done" — three ways of being stuck that no text comparison
// catches, and the run pays a full model call for every one of them until
// MaxTurns. On a long run that is a very large number.
//
// So these tests are about a boundary, not a feature. Below it, a model that is
// genuinely working must never be cut off, however many times it needs to be
// nudged — that is the whole point of an uncapped gate, and it is the property
// this file's fix could most easily have broken. Above it, a model that has
// stopped making progress must be stopped quickly and recorded honestly, as a
// stalled goal rather than an exhausted turn budget.

// weakModel is a model that cannot follow the completion contract, in the way
// weak models actually fail: it is not confused about the task and it is not
// looping on a tool. It believes it is finished and keeps saying so, in
// different words each time, never emitting the sentinel that would let the run
// close.
type weakModel struct {
	mu sync.Mutex

	// workBeforeFinish is how many tool calls it makes each time it is nudged.
	// Zero is the stalled model. Non-zero is the model that responds to a nudge
	// by actually going back to work, which must never be cut off.
	workBeforeFinish int
	// compliesWhenTold makes it emit the sentinel once the nudge escalates from
	// explaining the contract to dictating it. This is the model the escalation
	// exists for: it was never unwilling, it just needed telling precisely.
	compliesWhenTold bool

	calls    int
	finishes int
	worked   int
	// sawEscalated records whether the mechanically-worded nudge ever reached
	// the model, so a test can assert the escalation is what unblocked it rather
	// than assuming so.
	sawEscalated bool
}

// escalatedMarker is a phrase unique to the second-and-later nudge. Matching on
// it is how the model below can react to the escalation specifically, which is
// what makes "the escalation is what fixed it" an assertion rather than a guess.
const escalatedMarker = "make the LAST line of it exactly"

func (*weakModel) Name() string        { return "weak" }
func (*weakModel) SupportsTools() bool { return true }

func (p *weakModel) Chat(_ context.Context, req agentcore.ChatRequest) (agentcore.ChatResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++

	escalated := false
	for _, m := range req.Messages {
		if strings.Contains(m.Content, escalatedMarker) {
			escalated = true
		}
	}
	if escalated {
		p.sawEscalated = true
	}

	if p.compliesWhenTold && escalated {
		p.finishes++
		return usageFor(req, agentcore.AssistantText("The audit is complete.\n"+goal.Done)), nil
	}

	// Do the configured amount of work before each finish. Counted against the
	// finishes so far, so every nudge buys a fresh batch of tool calls.
	if p.worked < (p.finishes+1)*p.workBeforeFinish {
		p.worked++
		return usageFor(req, agentcore.AssistantToolCall(
			fmt.Sprintf("w%d", p.worked), "work", fmt.Sprintf(`{"n":%d}`, p.worked))), nil
	}

	// The finish that never satisfies the gate: no sentinel, and never the same
	// wording twice, so the verbatim breaker has nothing to match on.
	p.finishes++
	return usageFor(req, agentcore.AssistantText(
		fmt.Sprintf("I believe the audit is finished (%s).", ordinal(p.finishes)))), nil
}

func (p *weakModel) Stream(ctx context.Context, req agentcore.ChatRequest) (<-chan agentcore.ChatDelta, error) {
	resp, err := p.Chat(ctx, req)
	if err != nil {
		return nil, err
	}
	ch := make(chan agentcore.ChatDelta, 4)
	go func() {
		defer close(ch)
		if resp.Message.Content != "" {
			ch <- agentcore.ChatDelta{ContentDelta: resp.Message.Content}
		}
		for i := range resp.Message.ToolCalls {
			tc := resp.Message.ToolCalls[i]
			ch <- agentcore.ChatDelta{ToolCall: &tc}
		}
		ch <- agentcore.ChatDelta{Done: true, Usage: resp.Usage}
	}()
	return ch, nil
}

// ordinal keeps every finish textually distinct. The point is not the wording;
// it is that no two finishes are byte-identical, which is exactly the shape a
// verbatim stall breaker cannot see.
func ordinal(n int) string { return fmt.Sprintf("attempt %d", n) }

// weakMaxTurns is the turn ceiling these runs are given. It stands in for a long
// run's ceiling: large enough that reaching it is a real cost and an obviously
// wrong outcome, so a test that ends there has caught something.
const weakMaxTurns = 400

// runWeak drives one gated run against the weak model.
func runWeak(t *testing.T, id string, m *weakModel) (agentcore.RunResult, *e2eWorkTool) {
	t.Helper()

	limits := agentcore.DefaultLimits()
	limits.MaxTurns = weakMaxTurns
	limits.MaxToolCalls = weakMaxTurns
	work := &e2eWorkTool{size: 100}

	agent, err := agentcore.Build(
		e2eConfig{cfg: agentcore.Config{
			Provider:  m,
			Model:     "weak-model",
			Tools:     agentcore.NewToolSet(work),
			Policy:    agentcore.NewAllowList("work"),
			Limits:    &limits,
			Session:   newE2EStore(),
			SessionID: id,
		}},
		goal.Until(scaleGoal),
	)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	res, err := agent.Prompt(context.Background(), scaleTask)
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	return res, work
}

// TestWeakModelStallsCheaplyInsteadOfBurningTheBudget is the regression lock on
// the expensive failure. The model here is stuck and cannot say so in the
// required words, and it never repeats itself, so the only thing that can stop
// it is a breaker that looks at what it DID rather than what it wrote.
func TestWeakModelStallsCheaplyInsteadOfBurningTheBudget(t *testing.T) {
	m := &weakModel{} // no work between finishes: the stalled model
	res, work := runWeak(t, "weak-stall", m)

	t.Logf("model calls: %d, tool calls: %d, stop=%q", m.calls, work.Calls(), res.StopReason)

	// The honest outcome. max_turns here would not merely be a worse number: it
	// would be the wrong diagnosis recorded in the run, telling whoever reads it
	// that the agent ran out of room to work rather than that it had stopped
	// working a few hundred turns earlier.
	if res.StopReason != goal.StopReasonStalled {
		t.Fatalf("want the run recorded as a stalled goal, got %q — the gate kept nudging a model "+
			"that had stopped making progress, and the run's own record of why it ended is wrong",
			res.StopReason)
	}

	// The cost. The gate is uncapped, so nothing but the stall breaker stands
	// between a paraphrasing model and the whole turn budget; this is the number
	// that regressed from 4 to 400 the moment the breaker stopped matching.
	if m.calls > 8 {
		t.Fatalf("the stalled model cost %d model calls before the gate gave up (ceiling was %d); "+
			"each one is a paid call spent re-reading the same nudge", m.calls, weakMaxTurns)
	}
	if m.calls >= weakMaxTurns {
		t.Fatalf("the run burned its entire turn budget (%d calls) on a model that did nothing", m.calls)
	}

	// It really was the no-progress breaker and not luck: a stalled model makes
	// no tool calls at all, which is precisely why the verbatim breaker had
	// nothing to catch.
	if work.Calls() != 0 {
		t.Fatalf("this model is supposed to do no work between finishes, got %d tool calls", work.Calls())
	}
}

// TestGoalGateStaysUncappedWhileTheModelWorks is the other side of the boundary,
// and the property most at risk from any change that makes the gate give up
// sooner. A model that answers a nudge by going back to work is not stalled, no
// matter how many times it does it or how badly it phrases its finishes. If this
// fails, the gate has become a turn cap wearing a stall breaker's name — and it
// would fail silently in production as runs that quietly stopped early.
func TestGoalGateStaysUncappedWhileTheModelWorks(t *testing.T) {
	m := &weakModel{workBeforeFinish: 2} // works, finishes badly, works again
	res, work := runWeak(t, "weak-working", m)

	t.Logf("model calls: %d, tool calls: %d, finishes: %d, stop=%q",
		m.calls, work.Calls(), m.finishes, res.StopReason)

	if res.StopReason == goal.StopReasonStalled {
		t.Fatalf("the gate called a working model stalled after %d finishes and %d tool calls: "+
			"progress between nudges is the definition of not-stalled", m.finishes, work.Calls())
	}

	// And it was nudged many times, not once or twice — otherwise the run never
	// got near the boundary and passing here proves nothing.
	if m.finishes < 10 {
		t.Fatalf("the model only finished %d times; the run never exercised repeated nudging", m.finishes)
	}
	if work.Calls() < 20 {
		t.Fatalf("the model only worked %d times: this run is not the working case it claims to be", work.Calls())
	}
}

// TestEscalatedNudgeRecoversAModelThatJustNeededTelling is why the fix is not
// only a cheaper way to fail. The first nudge explains the contract in prose,
// which is the version a model already ignored once; the second stops explaining
// and dictates the literal line. This model complies the moment it is told that
// way — so the escalation converts a run that would have been abandoned as
// stalled into one that completes.
func TestEscalatedNudgeRecoversAModelThatJustNeededTelling(t *testing.T) {
	m := &weakModel{compliesWhenTold: true}
	res, _ := runWeak(t, "weak-recovers", m)

	t.Logf("model calls: %d, stop=%q, final=%q", m.calls, res.StopReason, res.Final)

	if !m.sawEscalated {
		t.Fatal("the escalated nudge never reached the model, so this test is not testing escalation")
	}
	if res.StopReason == goal.StopReasonStalled {
		t.Fatal("the run was abandoned as stalled even though the model complied once told precisely")
	}
	if !strings.Contains(res.Final, goal.Done) {
		t.Fatalf("the run did not close on the completion sentinel: %q", res.Final)
	}
	// Cheap, too: explaining then dictating is two nudges, not a long negotiation.
	if m.calls > 5 {
		t.Fatalf("recovery took %d model calls; the escalation is supposed to land on the second nudge", m.calls)
	}
}
