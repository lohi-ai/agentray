package agentcore_test

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/lohi-ai/agentray/agentcore"
	"github.com/lohi-ai/agentray/agentcore/plugins/goal"
	"github.com/lohi-ai/agentray/agentcore/plugins/observe"
)

// The whole-agent guarantee.
//
// Every subsystem here already has a unit test proving it works alone:
// compaction bounds a transcript, the goal gate re-opens a premature finish,
// the session log reduces back to its history, the policy blocks an
// out-of-scope call, the monitor prices a model call, the log invariant
// notices an unlogged message. What none of them proves is that the six still
// hold WHEN COMBINED, over a run long enough for compaction to rewrite history
// dozens of times.
//
// That is where the interesting failures live, because compaction is a
// deliberate history rewrite and every other subsystem has an opinion about
// history. A goal pin can be summarized away. A synthetic gate nudge can reach
// the model without ever reaching the log, so a resumed run continues from a
// conversation that never happened. A recorded goal can drift from the one
// being enforced. None of those show up in a short run, and none show up in a
// unit test that stubs the others out.
//
// So this file runs ONE agent with all six live and asserts each one's
// invariant at the end of a ~90-turn durable run.

const (
	e2eTarget   = 90 // provider work calls before the run tries to finish
	e2eDenyTurn = 3  // the turn on which the model reaches outside its scopes
)

// --- session store -----------------------------------------------------------

// e2eStore is an append-only in-memory SessionStore. It records every entry the
// loop commits, which is what makes the log-invariant check meaningful: the
// plugin compares what the model SAW against what this store RECEIVED.
type e2eStore struct {
	mu  sync.Mutex
	log map[string][]agentcore.SessionEntry
}

func newE2EStore() *e2eStore {
	return &e2eStore{log: map[string][]agentcore.SessionEntry{}}
}

func (s *e2eStore) Append(_ context.Context, id string, e agentcore.SessionEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	e.Seq = len(s.log[id])
	s.log[id] = append(s.log[id], e)
	return nil
}

func (s *e2eStore) Log(_ context.Context, id string) ([]agentcore.SessionEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]agentcore.SessionEntry, len(s.log[id]))
	copy(out, s.log[id])
	return out, nil
}

// LogFrom and CheckpointSeq make this double a WINDOWING store, like the two
// real ones. Without them every test using it would quietly exercise
// LoadResumeLog's fallback instead of the path production takes — and a resume
// window that is never actually served by any test in the suite is a feature
// that only runs for the first time in front of a user.

func (s *e2eStore) LogFrom(_ context.Context, id string, sinceSeq int) ([]agentcore.SessionEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []agentcore.SessionEntry
	for _, e := range s.log[id] {
		if e.Seq >= sinceSeq {
			out = append(out, e)
		}
	}
	return out, nil
}

func (s *e2eStore) CheckpointSeq(_ context.Context, id string) (int, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	seq, branched := 0, false
	for _, e := range s.log[id] {
		if e.Kind == agentcore.EntryLeafMove {
			branched = true
		}
		if e.Kind == agentcore.EntryCompaction && e.Final && e.Retained != nil && e.State != nil {
			seq = e.Seq
		}
	}
	return seq, branched, nil
}

// --- tools -------------------------------------------------------------------

// e2eWorkTool returns a payload big enough that a handful of calls exhaust the
// context budget, so compaction runs for real instead of being configured and
// never triggered.
type e2eWorkTool struct {
	mu    sync.Mutex
	calls int
	size  int
}

func (*e2eWorkTool) Name() string { return "work" }
func (*e2eWorkTool) Schema() agentcore.ToolSchema {
	return agentcore.ToolSchema{
		Name:        "work",
		Description: "does a unit of work and returns its output",
		Parameters:  map[string]any{"type": "object"},
	}
}

func (w *e2eWorkTool) Run(context.Context, string) (string, error) {
	w.mu.Lock()
	n := w.calls + 1
	w.calls = n
	w.mu.Unlock()
	return fmt.Sprintf("result#%d %s", n, strings.Repeat("x", w.size)), nil
}

func (w *e2eWorkTool) Calls() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.calls
}

// e2eForbiddenTool is in the toolset but not in the allow-list. The policy is
// the only thing standing between a model that asks for it and the side effect
// it would have, so the test asserts on this counter rather than trusting the
// trace alone: a governance test that only checks what was REPORTED cannot tell
// a blocked call from an executed call that was mislabeled.
type e2eForbiddenTool struct {
	mu    sync.Mutex
	calls int
}

func (*e2eForbiddenTool) Name() string { return "wire_money" }
func (*e2eForbiddenTool) Schema() agentcore.ToolSchema {
	return agentcore.ToolSchema{
		Name:        "wire_money",
		Description: "moves money — out of scope for this agent",
		Parameters:  map[string]any{"type": "object"},
	}
}

func (f *e2eForbiddenTool) Run(context.Context, string) (string, error) {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	return "sent", nil
}

func (f *e2eForbiddenTool) Calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// --- provider ----------------------------------------------------------------

// e2eProvider scripts the whole run by turn number, and doubles as the
// compaction summarizer so the summary path executes live rather than being
// stubbed to a constant.
//
// Its finish is deliberately two-staged: the first attempt omits the goal
// sentinel and must be REJECTED by the gate, the second supplies it. A run that
// merely ends proves nothing about the gate; a run that is forced to keep going
// once and then allowed to stop proves the gate is load-bearing.
type e2eProvider struct {
	mu        sync.Mutex
	calls     int
	summaries int
	finishes  int
}

func (*e2eProvider) Name() string        { return "e2e" }
func (*e2eProvider) SupportsTools() bool { return true }

func (p *e2eProvider) Chat(_ context.Context, req agentcore.ChatRequest) (agentcore.ChatResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	// Compaction borrows the run's provider to summarize. Recognize that call by
	// its system prompt and answer in the checkpoint shape it expects.
	if len(req.Messages) > 0 && strings.HasPrefix(req.Messages[0].Content, "You are a context summarization") {
		p.summaries++
		return usageFor(req, agentcore.AssistantText(
			"## Goal\nSurvive the long run\n## Next Steps\n1. keep working",
		)), nil
	}

	p.calls++
	switch {
	case p.calls == e2eDenyTurn:
		// Reach for a tool this agent was never granted.
		return usageFor(req, agentcore.AssistantToolCall("deny1", "wire_money", `{"amount":100}`)), nil

	case p.calls >= e2eTarget:
		p.finishes++
		if p.finishes == 1 {
			// A finish with no sentinel — the gate must re-open the run.
			return usageFor(req, agentcore.AssistantText("I think that's everything.")), nil
		}
		return usageFor(req, agentcore.AssistantText("Long run complete.\n"+goal.Done)), nil

	default:
		// Unique arguments per call so no redundancy pass can collapse these
		// results — the run must pay for its context with compaction.
		return usageFor(req, agentcore.AssistantToolCall(
			fmt.Sprintf("w%d", p.calls), "work", fmt.Sprintf(`{"n":%d}`, p.calls),
		)), nil
	}
}

func (p *e2eProvider) Stream(ctx context.Context, req agentcore.ChatRequest) (<-chan agentcore.ChatDelta, error) {
	ch := make(chan agentcore.ChatDelta, 8)
	go func() {
		defer close(ch)
		resp, _ := p.Chat(ctx, req)
		if resp.Message.Content != "" {
			ch <- agentcore.ChatDelta{ContentDelta: resp.Message.Content}
		}
		for i := range resp.Message.ToolCalls {
			tc := resp.Message.ToolCalls[i]
			ch <- agentcore.ChatDelta{ToolCall: &tc}
		}
		ch <- agentcore.ChatDelta{Done: true, StopReason: resp.StopReason}
	}()
	return ch, nil
}

func (p *e2eProvider) stats() (calls, summaries int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls, p.summaries
}

// usageFor reports token counts that GROW with the request, the way a real
// provider's do. Both details matter:
//
// The monitor needs non-zero usage or the cost assertion is vacuous. Compaction
// needs usage that grows, because estimateContextTokens trusts the provider
// over its own byte estimate — it finds the most recent assistant message
// carrying Usage and takes that as the context size. A scripted provider that
// reports a CONSTANT token count therefore pins the estimate below any budget
// and compaction never fires, no matter how long the transcript gets. That is
// the correct behavior (the provider is the authority on its own context
// accounting) and a silent way to write a long-run test that never exercises
// the thing it is named after.
func usageFor(req agentcore.ChatRequest, r agentcore.ChatResponse) agentcore.ChatResponse {
	bytes := 0
	for _, m := range req.Messages {
		bytes += len(m.Content)
		for _, tc := range m.ToolCalls {
			bytes += len(tc.Name) + len(tc.Arguments)
		}
	}
	r.Usage = agentcore.Usage{InputTokens: bytes / 4, OutputTokens: 200}
	return r
}

// --- composition -------------------------------------------------------------

// e2eConfig adapts a Config into a Plugin so it can be composed alongside real
// plugins. Config alone cannot express this run: it records a goal but installs
// no gate, and it has no field for the monitor or the log invariant.
type e2eConfig struct{ cfg agentcore.Config }

func (e2eConfig) Name() string { return "config" }

func (c e2eConfig) Register(r *agentcore.Registry) error { return r.ApplyConfig(c.cfg) }

// traceSink collects one record per model call.
type traceSink struct {
	mu      sync.Mutex
	records []observe.TraceRecord
}

func (s *traceSink) Record(r observe.TraceRecord) {
	s.mu.Lock()
	s.records = append(s.records, r)
	s.mu.Unlock()
}

func (s *traceSink) len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.records)
}

// --- the test ----------------------------------------------------------------

// TestLongRunEndToEndAcrossAllSubsystems is the composed guarantee: one durable
// ~90-turn run with compaction, the goal gate, the session log, tool calls, the
// permission policy, and both observers all live at once. Each block below
// asserts one subsystem's invariant held THROUGH the others.
func TestLongRunEndToEndAcrossAllSubsystems(t *testing.T) {
	const (
		condition = "Complete the long-running task without losing the thread"
		sessionID = "e2e-longrun"
	)

	store := newE2EStore()
	prov := &e2eProvider{}
	work := &e2eWorkTool{size: 900}
	forbidden := &e2eForbiddenTool{}
	sink := &traceSink{}

	var (
		violMu     sync.Mutex
		violations []observe.LogInvariantViolation
	)

	limits := agentcore.DefaultLimits()
	limits.MaxTurns = 400
	limits.MaxToolCalls = 500
	limits.MaxContextTokens = 4000 // tight budget -> frequent compaction

	cs := agentcore.DefaultCompactionSettings()
	cs.KeepRecentTokens = 1500

	agent, err := agentcore.Build(
		e2eConfig{cfg: agentcore.Config{
			Provider: prov,
			Model:    "e2e-model",
			Tools:    agentcore.NewToolSet(work, forbidden),
			// wire_money is deliberately absent: in the toolset, out of scope.
			Policy:     agentcore.NewAllowList("work"),
			Limits:     &limits,
			Compaction: &cs,
			Session:    store,
			SessionID:  sessionID,
		}},
		goal.Until(condition),
		observe.Monitor{
			Pricing: observe.Pricing{"e2e-model": {InputPerM: 1.00, OutputPerM: 2.00}},
			Sink:    sink,
		},
		observe.LogInvariant{Report: func(v observe.LogInvariantViolation) {
			violMu.Lock()
			violations = append(violations, v)
			violMu.Unlock()
		}},
	)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	res, err := agent.Prompt(context.Background(), "Work the long task to completion.")
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}

	calls, summaries := prov.stats()

	// --- the run finished at all -------------------------------------------
	if res.Turns < e2eTarget/2 {
		t.Fatalf("expected a long run, got %d turns (stop=%q)", res.Turns, res.StopReason)
	}
	if work.Calls() < 50 {
		t.Fatalf("tool calls did not drive the run: work executed %d times", work.Calls())
	}

	// --- compaction ---------------------------------------------------------
	// It must have run repeatedly, and it must have done its job: a 90-turn
	// transcript is ~180 messages uncompacted.
	if summaries < 3 {
		t.Fatalf("compaction did not engage over %d provider calls: %d summaries", calls, summaries)
	}
	if len(res.Messages) > 60 {
		t.Fatalf("compaction did not bound the context: %d messages after %d turns",
			len(res.Messages), res.Turns)
	}

	// --- goal ---------------------------------------------------------------
	// The gate rejected the sentinel-less finish and accepted the next one, so
	// the run ended on the contract rather than on exhaustion.
	if prov.finishes < 2 {
		t.Fatalf("goal gate never re-opened the premature finish (finishes=%d)", prov.finishes)
	}
	if !strings.Contains(res.Final, goal.Done) {
		t.Fatalf("run ended without the completion sentinel: %q (stop=%q)", res.Final, res.StopReason)
	}
	if res.StopReason == goal.StopReasonStalled {
		t.Fatalf("run stalled instead of completing: %q", res.Final)
	}

	// --- governance ---------------------------------------------------------
	// The out-of-scope call was refused, told the model why, and — the part that
	// matters — never reached the tool.
	if n := forbidden.Calls(); n != 0 {
		t.Fatalf("policy leaked: forbidden tool executed %d times", n)
	}
	var denied []agentcore.ToolTrace
	for _, tr := range res.Tools {
		if !tr.Allowed {
			denied = append(denied, tr)
		}
	}
	if len(denied) != 1 {
		t.Fatalf("expected exactly one denied trace, got %d: %+v", len(denied), denied)
	}
	if denied[0].Tool != "wire_money" {
		t.Fatalf("wrong tool denied: %q", denied[0].Tool)
	}
	if !strings.Contains(denied[0].Reason, "not permitted") {
		t.Fatalf("denial reason not recorded: %q", denied[0].Reason)
	}
	// The advertised set never included it, so the policy filtered as well as gated.
	if permitted := agentcore.NewAllowList("work").PermittedTools(
		context.Background(), []string{"work", "wire_money"},
	); len(permitted) != 1 || permitted[0] != "work" {
		t.Fatalf("policy advertised an out-of-scope tool: %v", permitted)
	}

	// --- observer: monitor --------------------------------------------------
	// Every model call was traced, including the compaction calls, and cost was
	// stamped onto the run.
	if got, want := sink.len(), calls+summaries; got != want {
		t.Fatalf("monitor missed model calls: traced %d, provider served %d", got, want)
	}
	if res.Usage.CostUSD <= 0 {
		t.Fatalf("run cost not accounted: %v", res.Usage.CostUSD)
	}

	// --- observer: log invariant -------------------------------------------
	// The headline durability property: across every compaction above, each
	// message the model saw reached the durable log. A violation here means a
	// resumed run would continue from a history that never happened.
	//
	// Zero violations is not vacuous: the plugin declines a non-durable run
	// (nothing to diverge from), and the session block below proves this run
	// WAS durable — it wrote a goal entry, a leaf, and a reducible transcript.
	// So the checker was armed and found nothing, rather than never running.
	violMu.Lock()
	got := append([]observe.LogInvariantViolation(nil), violations...)
	violMu.Unlock()
	if len(got) != 0 {
		t.Fatalf("model-visible messages went unlogged (%d), first: %v", len(got), got[0])
	}

	// --- session ------------------------------------------------------------
	log, err := store.Log(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("Log: %v", err)
	}
	if len(log) == 0 {
		t.Fatal("durable log is empty")
	}
	var goalEntries int
	for _, e := range log {
		if e.Kind == agentcore.EntryGoal {
			goalEntries++
			if e.Goal != condition {
				t.Fatalf("goal drifted in the log: %q", e.Goal)
			}
		}
	}
	if goalEntries != 1 {
		t.Fatalf("expected the goal recorded once, got %d entries", goalEntries)
	}

	rs := agentcore.ReduceSession(log)
	if !rs.Completed {
		t.Fatal("reduced session is not completed — no leaf was written")
	}
	if rs.PendingCompaction {
		t.Fatal("reduced session has an unfinished compaction")
	}
	if len(rs.Messages) == 0 || rs.Messages[len(rs.Messages)-1].Content != res.Final {
		t.Fatalf("reduced log does not end at the run's answer")
	}

	// --- session: resume ----------------------------------------------------
	// A finished run resumes to its recorded answer without asking the model
	// anything. If this costs a provider call, every crash-recovery retry pays
	// for work that was already done.
	before, _ := prov.stats()
	resumed, err := agentcore.Build(
		e2eConfig{cfg: agentcore.Config{
			Provider:      prov,
			Model:         "e2e-model",
			Tools:         agentcore.NewToolSet(work, forbidden),
			Policy:        agentcore.NewAllowList("work"),
			Limits:        &limits,
			Session:       store,
			SessionID:     sessionID,
			ResumeSession: true,
		}},
		goal.Until(condition),
	)
	if err != nil {
		t.Fatalf("Build resume: %v", err)
	}
	rr, err := resumed.Prompt(context.Background(), "Work the long task to completion.")
	if err != nil {
		t.Fatalf("resume Prompt: %v", err)
	}
	if rr.Final != res.Final {
		t.Fatalf("resume returned a different answer:\n got %q\nwant %q", rr.Final, res.Final)
	}
	if after, _ := prov.stats(); after != before {
		t.Fatalf("resume of a completed run made %d provider calls", after-before)
	}
}
