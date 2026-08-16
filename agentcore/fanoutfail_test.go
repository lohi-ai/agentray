package agentcore_test

// One sibling fails. What does that cost the seven that didn't?
//
// A wide fan-out is the most expensive single move a run makes: eight child runs
// dispatched concurrently, each of them a paid-for conversation. Failure in that
// batch is not exotic — a child hits a provider error, a child runs out of turns,
// a child comes back empty. The question this round asks is what the loop does
// with the rest of the batch when one member dies, and there are three ways to
// get it wrong, all of them silent:
//
//   - abort the turn, and seven paid-for child runs are thrown away;
//   - drop the failed call's result, and the transcript carries an assistant
//     tool_call with no matching tool message, which providers reject on the very
//     next request;
//   - swallow the failure into something bland, and the model never learns a
//     shard went un-reconciled — it answers as if all eight came back.
//
// The failed children's tokens were burned either way, so they also have to be
// billed, or the budget gate meters a fan-out at less than it cost.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/lohi-ai/agentray/agentcore"
	"github.com/lohi-ai/agentray/agentcore/plugins/goal"
	"github.com/lohi-ai/agentray/agentcore/plugins/subagent"
)

const failMarker = "FAILFAN-SUBTASK"

const (
	failWidth = 8
	// erroringShard's child hits a hard provider error mid-run.
	erroringShard = 3
	// silentShard's child finishes without producing an answer, which is a
	// different failure: the run succeeded and returned nothing usable.
	silentShard = 5
)

// failFanProvider drives a parent that fans out to failWidth children, two of
// which fail in different ways.
type failFanProvider struct {
	mu sync.Mutex

	parentTurns int
	childCalls  int

	// spent is every token this provider handed back, on every call it answered
	// successfully. A call that returns an error reports no usage — agentcore
	// cannot bill what the provider never told it about — so it is excluded here
	// too, and the assertion stays honest.
	spent agentcore.Usage

	// requests is every message set the PARENT saw.
	requests [][]agentcore.Message
}

func (*failFanProvider) Name() string        { return "failfan" }
func (*failFanProvider) SupportsTools() bool { return true }

func (p *failFanProvider) Chat(_ context.Context, req agentcore.ChatRequest) (agentcore.ChatResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	record := func(resp agentcore.ChatResponse) (agentcore.ChatResponse, error) {
		p.spent = agentcore.Usage{
			InputTokens:  p.spent.InputTokens + resp.Usage.InputTokens,
			OutputTokens: p.spent.OutputTokens + resp.Usage.OutputTokens,
		}
		return resp, nil
	}

	if len(req.Messages) > 0 && strings.HasPrefix(req.Messages[0].Content, "You are a context summarization") {
		return record(usageFor(req, agentcore.AssistantText(
			"## Goal\nReconcile\n## Progress\n### Done\n- [x] shards\n## Next Steps\n1. keep going")))
	}

	if shard, ok := failShard(req.Messages); ok {
		p.childCalls++
		switch shard {
		case erroringShard:
			// A hard failure inside the child's own run. Not retryable: the point
			// is a child that dies, not one that recovers.
			return agentcore.ChatResponse{}, errors.New("upstream model rejected the request: shard file unavailable")
		case silentShard:
			// Ran, billed, produced nothing usable.
			return record(usageFor(req, agentcore.AssistantText(goal.Done)))
		default:
			return record(usageFor(req, agentcore.AssistantText(
				failFinding(shard)+"\n"+goal.Done)))
		}
	}

	p.parentTurns++
	n := p.parentTurns
	p.requests = append(p.requests, req.Messages)

	if n == 1 {
		calls := make([]agentcore.ToolCall, 0, failWidth)
		for i := 0; i < failWidth; i++ {
			calls = append(calls, agentcore.ToolCall{
				ID:   fmt.Sprintf("fs%d", i),
				Name: subagent.ToolSpawnSubagent,
				Arguments: fmt.Sprintf(`{"task":%q}`,
					fmt.Sprintf("%s: reconcile shard %d and report", failMarker, i)),
			})
		}
		return record(usageFor(req, agentcore.ChatResponse{
			Message: agentcore.Message{Role: agentcore.RoleAssistant, ToolCalls: calls},
		}))
	}
	return record(usageFor(req, agentcore.AssistantText("Reconciliation complete.\n"+goal.Done)))
}

func (p *failFanProvider) Stream(ctx context.Context, req agentcore.ChatRequest) (<-chan agentcore.ChatDelta, error) {
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
		ch <- agentcore.ChatDelta{Done: true, StopReason: resp.StopReason, Usage: resp.Usage}
	}()
	return ch, nil
}

// failShard reports which shard a child was given, or false for a parent turn.
func failShard(msgs []agentcore.Message) (int, bool) {
	for _, m := range msgs {
		if m.Role != agentcore.RoleUser || !strings.Contains(m.Content, failMarker) {
			continue
		}
		var shard int
		if _, err := fmt.Sscanf(m.Content[strings.Index(m.Content, failMarker):],
			failMarker+": reconcile shard %d", &shard); err == nil {
			return shard, true
		}
	}
	return 0, false
}

func failFinding(shard int) string {
	return fmt.Sprintf("FINDING shard %d: balance %d reconciled", shard, 2000+shard)
}

// TestAFailedChildDoesNotCostTheRunItsHealthySiblings is the round. Run it with
// -race: the failures happen inside a concurrent batch.
func TestAFailedChildDoesNotCostTheRunItsHealthySiblings(t *testing.T) {
	prov := &failFanProvider{}

	limits := agentcore.DefaultLimits()
	limits.MaxTurns = 20
	limits.MaxToolCalls = 60

	agent, err := agentcore.Build(
		e2eConfig{cfg: agentcore.Config{
			Provider:  prov,
			Model:     "failfan",
			Tools:     agentcore.NewToolSet(),
			Policy:    agentcore.NewAllowList(subagent.ToolSpawnSubagent),
			Limits:    &limits,
			Session:   newE2EStore(),
			SessionID: "failfan",
		}},
		goal.Until("every shard reconciled"),
		subagent.SelfOnly(),
	)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	res, err := agent.Prompt(context.Background(), "Reconcile every shard, in parallel where you can.")
	if err != nil {
		t.Fatalf("two failed children took the whole run down: %v. Seven other child runs had "+
			"already been paid for when the eighth died", err)
	}

	prov.mu.Lock()
	requests, children, spent := prov.requests, prov.childCalls, prov.spent
	prov.mu.Unlock()

	if children < failWidth {
		t.Fatalf("only %d of %d children ran: the batch did not fan out", children, failWidth)
	}
	if len(requests) < 2 {
		t.Fatalf("the parent never saw the results of its fan-out (%d turns)", len(requests))
	}

	after := requests[1]

	// 1. The healthy siblings' work has to be there. It was paid for.
	var lost []int
	for i := 0; i < failWidth; i++ {
		if i == erroringShard || i == silentShard {
			continue
		}
		if !strings.Contains(messagesText(after), failFinding(i)) {
			lost = append(lost, i)
		}
	}
	if len(lost) > 0 {
		t.Fatalf("shards %v came back from healthy children and did not reach the parent. Two "+
			"siblings failing must not discard work the run already paid for", lost)
	}

	// 2. Every tool call needs a matching tool result, or the next request is
	// malformed and the provider rejects it — a failure mode that shows up one
	// turn later as something unrelated.
	answered := map[string]bool{}
	for _, m := range after {
		if m.Role == agentcore.RoleTool {
			answered[m.ToolCallID] = true
		}
	}
	for _, m := range after {
		for _, tc := range m.ToolCalls {
			if !answered[tc.ID] {
				t.Fatalf("tool call %q (%s) has no matching tool result. A dangling call makes the "+
					"very next request invalid to every provider that validates the pairing",
					tc.ID, tc.Name)
			}
		}
	}

	// 3. The model has to be able to TELL that a shard went un-reconciled.
	// Reporting eight shards done when six came back is the worst outcome
	// available here, and it is what a swallowed failure produces.
	for _, shard := range []int{erroringShard, silentShard} {
		id := fmt.Sprintf("fs%d", shard)
		var note string
		for _, m := range after {
			if m.Role == agentcore.RoleTool && m.ToolCallID == id {
				note = m.Content + " " + m.Error
			}
		}
		if strings.TrimSpace(note) == "" {
			t.Fatalf("child %d failed and its slot in the transcript says nothing. The model then "+
				"answers as though all %d shards were reconciled", shard, failWidth)
		}
		if strings.Contains(note, failFinding(shard)) {
			t.Fatalf("child %d failed but its slot claims a finding: %q", shard, note)
		}
	}

	// 4. The failed children burned tokens. Every call the provider actually
	// answered has to be in the run's total, including the ones inside children
	// whose spawn ended in an error.
	got := res.Usage.InputTokens + res.Usage.OutputTokens
	want := spent.InputTokens + spent.OutputTokens
	if got < want {
		t.Fatalf("the run reported %d tokens against %d the provider handed back — a failed child's "+
			"spend went unbilled. Its tokens were burned exactly like a successful one's, and the "+
			"budget gate meters on this number", got, want)
	}

	t.Logf("%d children, %d failed: %d of %d findings reached the parent; %d tokens billed against %d spent",
		children, 2, failWidth-2, failWidth, got, want)
}

// The other way a fan-out loses work: the PARENT dies while the batch is in
// flight.
//
// spawn_subagent declares itself RetrySafe on the strength of a specific claim —
// a self-fork's child session id is derived deterministically from (parent
// session, tool call id), so a replayed spawn reattaches to the child's own log
// rather than running a second copy. Everything expensive rests on that: the
// batch is where a run's tokens actually go, and a resume that re-runs four
// completed children pays for them twice and hides it inside a recovery that
// looks successful.
//
// This drives it for real: eight children, the parent killed once four have
// finished, then a resume on the same durable session.
type crashFanProvider struct {
	mu sync.Mutex

	cancel context.CancelFunc
	armed  bool // cancel the parent when the fifth child arrives

	finished int
	// runsPerShard counts how many times each shard's child actually called the
	// provider, ACROSS both lifetimes. A reattached child adds nothing.
	runsPerShard map[int]int
	// doneShard records the shards whose child actually produced its finding, as
	// opposed to merely having started. A shard that was cut off mid-run SHOULD
	// be re-run by the resume; one that finished must not be.
	doneShard map[int]bool

	parentTurns int
	lastRequest []agentcore.Message
}

func (*crashFanProvider) Name() string        { return "crashfan" }
func (*crashFanProvider) SupportsTools() bool { return true }

func (p *crashFanProvider) Chat(ctx context.Context, req agentcore.ChatRequest) (agentcore.ChatResponse, error) {
	// A real provider fails a cancelled call. A fake that ignores ctx would let
	// the "crashed" run keep going and quietly turn this into a test of nothing.
	if err := ctx.Err(); err != nil {
		return agentcore.ChatResponse{}, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()

	if len(req.Messages) > 0 && strings.HasPrefix(req.Messages[0].Content, "You are a context summarization") {
		return usageFor(req, agentcore.AssistantText(
			"## Goal\nReconcile\n## Progress\n### Done\n- [x] shards\n## Next Steps\n1. keep going")), nil
	}

	if shard, ok := failShard(req.Messages); ok {
		p.runsPerShard[shard]++
		if p.armed && p.finished >= 4 {
			// The parent dies mid-batch. Everything still in flight goes with it.
			p.armed = false
			p.cancel()
			return agentcore.ChatResponse{}, context.Canceled
		}
		p.finished++
		p.doneShard[shard] = true
		return usageFor(req, agentcore.AssistantText(failFinding(shard)+"\n"+goal.Done)), nil
	}

	p.parentTurns++
	p.lastRequest = req.Messages
	if p.parentTurns == 1 {
		calls := make([]agentcore.ToolCall, 0, failWidth)
		for i := 0; i < failWidth; i++ {
			calls = append(calls, agentcore.ToolCall{
				ID:   fmt.Sprintf("fs%d", i),
				Name: subagent.ToolSpawnSubagent,
				Arguments: fmt.Sprintf(`{"task":%q}`,
					fmt.Sprintf("%s: reconcile shard %d and report", failMarker, i)),
			})
		}
		return usageFor(req, agentcore.ChatResponse{
			Message: agentcore.Message{Role: agentcore.RoleAssistant, ToolCalls: calls},
		}), nil
	}
	return usageFor(req, agentcore.AssistantText("Reconciliation complete.\n"+goal.Done)), nil
}

func (p *crashFanProvider) Stream(ctx context.Context, req agentcore.ChatRequest) (<-chan agentcore.ChatDelta, error) {
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
		ch <- agentcore.ChatDelta{Done: true, StopReason: resp.StopReason, Usage: resp.Usage}
	}()
	return ch, nil
}

func crashFanConfig(prov *crashFanProvider, store agentcore.SessionStore, resume bool) e2eConfig {
	limits := agentcore.DefaultLimits()
	limits.MaxTurns = 20
	limits.MaxToolCalls = 60
	return e2eConfig{cfg: agentcore.Config{
		Provider:      prov,
		Model:         "crashfan",
		Tools:         agentcore.NewToolSet(),
		Policy:        agentcore.NewAllowList(subagent.ToolSpawnSubagent),
		Limits:        &limits,
		Session:       store,
		SessionID:     "crashfan",
		ResumeSession: resume,
	}}
}

func TestAResumedFanOutDoesNotPayForItsCompletedChildrenTwice(t *testing.T) {
	store := newE2EStore()
	prov := &crashFanProvider{armed: true, runsPerShard: map[int]int{}, doneShard: map[int]bool{}}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	prov.cancel = cancel

	first, err := agentcore.Build(crashFanConfig(prov, store, false),
		goal.Until("every shard reconciled"), subagent.SelfOnly())
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	// Note the shape of the stop: a cancelled run returns a NIL error with
	// StopReason "aborted" (loop.go stops between turns and hands back what it
	// has, which is right for a viewer that walked away). So the crash is
	// detected on the stop reason, not on err — and a caller that checks only
	// err sees a successful run here.
	firstRes, _ := first.Prompt(ctx, "Reconcile every shard, in parallel where you can.")
	if firstRes.StopReason != "aborted" {
		t.Fatalf("the parent was cancelled mid-batch but stopped for %q", firstRes.StopReason)
	}

	prov.mu.Lock()
	runsBefore := map[int]int{}
	for k, v := range prov.runsPerShard {
		runsBefore[k] = v
	}
	doneBefore := map[int]bool{}
	for k, v := range prov.doneShard {
		doneBefore[k] = v
	}
	prov.mu.Unlock()

	if len(doneBefore) < 4 {
		t.Fatalf("only %d children finished before the crash: the resume has nothing to reattach to",
			len(doneBefore))
	}
	if len(doneBefore) == failWidth {
		t.Fatalf("the whole batch finished before the cancellation landed — nothing was interrupted, "+
			"so this proves nothing about recovery (%v)", doneBefore)
	}

	// Same session, same store, fresh agent and a fresh context.
	resumed, err := agentcore.Build(crashFanConfig(prov, store, true),
		goal.Until("every shard reconciled"), subagent.SelfOnly())
	if err != nil {
		t.Fatalf("Build (resume): %v", err)
	}
	if _, err := resumed.Prompt(context.Background(), ""); err != nil {
		t.Fatalf("resume: %v", err)
	}

	prov.mu.Lock()
	runs := prov.runsPerShard
	final := prov.lastRequest
	prov.mu.Unlock()

	var repaid []int
	for shard := range doneBefore {
		if runs[shard] > runsBefore[shard] {
			repaid = append(repaid, shard)
		}
	}
	if len(repaid) > 0 {
		t.Fatalf("shards %v re-ran a child that had already completed before the crash (runs now %v, "+
			"before the resume %v). The deterministic child-session id is what makes spawn_subagent "+
			"safe to declare RetrySafe; if a replayed spawn does not reattach, every recovery of a "+
			"wide batch silently pays for the whole batch again",
			repaid, runs, runsBefore)
	}

	// Reattaching is only worth anything if the recovered answers are actually
	// there — a resume that skips the work AND loses the result is worse than
	// re-running it.
	var missing []int
	for shard := range doneBefore {
		if !strings.Contains(messagesText(final), failFinding(shard)) {
			missing = append(missing, shard)
		}
	}
	if len(missing) > 0 {
		t.Fatalf("shards %v completed before the crash, were not re-run after it, and their findings "+
			"are not in the recovered window either — the run paid for them and kept nothing", missing)
	}

	// And the work that was actually interrupted has to get done. A recovery
	// that reattaches the finished children and quietly abandons the rest turns
	// a crash into a permanently partial answer.
	var abandoned []int
	for shard := 0; shard < failWidth; shard++ {
		if !doneBefore[shard] && !strings.Contains(messagesText(final), failFinding(shard)) {
			abandoned = append(abandoned, shard)
		}
	}
	if len(abandoned) > 0 {
		t.Fatalf("shards %v were interrupted by the crash and never finished after the resume. The "+
			"run comes back, reports success, and answers with the shards that happened to beat the "+
			"cancellation", abandoned)
	}

	t.Logf("%d of %d children finished before the crash; runs-per-shard after resume %v",
		len(doneBefore), failWidth, runs)
}
