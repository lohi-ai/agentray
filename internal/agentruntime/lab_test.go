package agentruntime

import (
	"context"
	"strings"
	"testing"

	"github.com/lohi-ai/agentray/agentcore"
	"github.com/lohi-ai/agentray/internal/storage"
)

// TestRunTestBlockedWithoutSandbox confirms test mode fails closed when no
// isolation sandbox is configured: it returns a "blocked" verdict carrying a
// setup prompt instead of executing the agent's tools for real on the host. This
// is the safety guard that lets a builder safely reach for test mode regardless
// of host config.
func TestRunTestBlockedWithoutSandbox(t *testing.T) {
	lab := NewLabService(nil, false)
	res, err := lab.RunTest(context.Background(), "u1", "p1", "agent1", "do a thing", "the answer")
	if err != nil {
		t.Fatalf("blocked run should not error: %v", err)
	}
	if res.Status != "blocked" {
		t.Fatalf("want blocked status, got %q", res.Status)
	}
	if res.SetupPrompt == "" {
		t.Fatal("blocked verdict must carry a setup prompt explaining the sandbox requirement")
	}
	if res.RunID != "" {
		t.Fatalf("blocked run must not claim a run id, got %q", res.RunID)
	}
	if res.Expected != "the answer" {
		t.Fatalf("expected output should round-trip into the verdict, got %q", res.Expected)
	}
}

// TestExplainControlsRequireMatchingProject confirms the explain control surface
// (advance / stop / steer) is scoped to the run's own project: a member of
// another project — or a request against an unknown run id — cannot drive,
// halt, or steer the run. This is the isolation guarantee for stepped runs.
func TestExplainControlsRequireMatchingProject(t *testing.T) {
	lab := NewLabService(nil, false)
	sess := &explainSession{
		projectID: "owner-proj",
		advance:   make(chan struct{}, 1),
		stop:      make(chan struct{}),
		steer:     make(chan agentcore.Message, liveQueueDepth),
	}
	lab.register("run-1", sess)

	// Unknown run id is never drivable.
	if lab.Advance("owner-proj", "ghost") || lab.Stop("owner-proj", "ghost") || lab.Steer("owner-proj", "ghost", "hi") {
		t.Fatal("controls must reject an unknown run id")
	}
	// A foreign project cannot drive a run it doesn't own.
	if lab.Advance("intruder", "run-1") || lab.Stop("intruder", "run-1") || lab.Steer("intruder", "run-1", "hi") {
		t.Fatal("controls must reject a project that does not own the run")
	}
	// The owning project can.
	if !lab.Advance("owner-proj", "run-1") {
		t.Fatal("owning project should be able to advance")
	}
	if !lab.Steer("owner-proj", "run-1", "use 7d") {
		t.Fatal("owning project should be able to steer")
	}
	if !lab.Stop("owner-proj", "run-1") {
		t.Fatal("owning project should be able to stop")
	}
}

// TestExplainControlsDeliverSignals confirms the controls actually move the run:
// advance releases the gate channel, steer enqueues a user-role correction, and
// stop closes the stop channel exactly once (a repeat stop is a safe no-op, not
// a double-close panic).
func TestExplainControlsDeliverSignals(t *testing.T) {
	lab := NewLabService(nil, false)
	sess := &explainSession{
		projectID: "p",
		advance:   make(chan struct{}, 1),
		stop:      make(chan struct{}),
		steer:     make(chan agentcore.Message, liveQueueDepth),
	}
	lab.register("r", sess)

	lab.Advance("p", "r")
	select {
	case <-sess.advance:
	default:
		t.Fatal("advance did not signal the gate channel")
	}

	lab.Steer("p", "r", "please use last 7 days")
	select {
	case m := <-sess.steer:
		if m.Role != agentcore.RoleUser || m.Content != "please use last 7 days" {
			t.Fatalf("steer enqueued wrong message: %+v", m)
		}
	default:
		t.Fatal("steer did not enqueue a correction")
	}

	lab.Stop("p", "r")
	select {
	case <-sess.stop:
	default:
		t.Fatal("stop did not close the stop channel")
	}
	// A repeat stop must not panic on the already-closed channel.
	if !lab.Stop("p", "r") {
		t.Fatal("repeat stop on a live session should still report handled")
	}
}

func TestRecordsFromCalls(t *testing.T) {
	calls := []storage.AgentLLMCall{
		{
			MessagesJSON:  `[{"role":"system","content":"# Identity\nYou help."},{"role":"user","content":"hi"}]`,
			ToolCallsJSON: `[{"id":"c1","name":"search","arguments":"{\"q\":\"x\"}"}]`,
			Tools:         []string{"search"},
			Response:      "thinking",
			StopReason:    "tool_calls",
			TokenInput:    100,
			TokenOutput:   12,
			CostUSD:       0.002,
		},
	}
	recs := recordsFromCalls(calls)
	if len(recs) != 1 {
		t.Fatalf("want 1 record, got %d", len(recs))
	}
	r := recs[0]
	if len(r.Messages) != 2 || r.Messages[0].Role != "system" {
		t.Fatalf("messages not parsed: %+v", r.Messages)
	}
	if len(r.ToolCalls) != 1 || r.ToolCalls[0].Name != "search" {
		t.Fatalf("tool calls not parsed: %+v", r.ToolCalls)
	}
	if r.TokensIn != 100 || r.CostUSD != 0.002 {
		t.Fatalf("accounting not carried: %+v", r)
	}
}

func TestRecordsFromCallsMalformedDegrades(t *testing.T) {
	calls := []storage.AgentLLMCall{{MessagesJSON: "not json", ToolCallsJSON: "{bad", Response: "x"}}
	recs := recordsFromCalls(calls)
	if len(recs) != 1 || recs[0].Response != "x" {
		t.Fatalf("malformed row should degrade to empty slices, not drop: %+v", recs)
	}
	if recs[0].Messages != nil {
		t.Fatalf("malformed messages should be nil, got %+v", recs[0].Messages)
	}
}

func TestLineDiff(t *testing.T) {
	d := lineDiff("a\nb\nc", "a\nx\nc")
	if !strings.Contains(d, "- b") || !strings.Contains(d, "+ x") {
		t.Fatalf("diff missing changed lines:\n%s", d)
	}
	if !strings.Contains(d, "  a") || !strings.Contains(d, "  c") {
		t.Fatalf("diff missing common lines:\n%s", d)
	}
	if lineDiff("same", "same") != "  same" {
		t.Fatalf("equal inputs should be all-common")
	}
}
