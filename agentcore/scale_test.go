package agentcore_test

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/lohi-ai/agentray/agentcore"
	"github.com/lohi-ai/agentray/agentcore/plugins/goal"
	"github.com/lohi-ai/agentray/agentcore/plugins/subagent"
	"github.com/lohi-ai/agentray/agentcore/plugins/todo"
	"reflect"
	"strings"
	"sync"
	"testing"
)

// Does the agent still know what it is doing after five thousand model calls?
//
// The long-run e2e test next door proves the subsystems compose over ~90 turns.
// This one asks the question that only shows up two orders of magnitude later,
// and it is a question about FORGETTING. A long run's context window is rewritten
// hundreds of times: each compaction replaces a span of transcript with a lossy
// summary, and the next compaction summarizes that summary. Anything carried
// only in the transcript therefore decays — not abruptly, which would be
// noticeable, but by degrees, until the agent is diligently working on a
// paraphrase of a paraphrase of what it was asked to do. That is the failure
// mode this file exists to rule out, and it cannot be observed in a short run
// because a short run never compacts twice.
//
// Two things are supposed to be immune, by two different mechanisms:
//
//   - the GOAL, because the first compaction to touch the original task lifts
//     it into a pinned system message that every later compaction keeps
//     verbatim (compaction.go's goalMarker);
//   - the PLAN, because it does not live in the transcript at all — it lives in
//     a todo.Store that a context hook re-renders into every request.
//
// So the assertions below are not "the run finished". They are: after the
// original prompt has been summarized out of the window, is it still in front
// of the model word for word, and is the checklist still whole?
//
// The third question is cost. An agent that stays on task by accumulating
// context is not solving the problem, so the same run measures what it holds:
// the live window, what a resume would load, and what the append-only log
// actually costs per turn.

// --- what the run is asked to do ---------------------------------------------

// scaleTask is the original prompt. It is deliberately distinctive: the whole
// point is to find it, verbatim, in a request issued thousands of turns after
// the message carrying it was summarized away.
const scaleTask = "Audit every shard in the ledger corpus, reconcile the CLEARING-7742 discrepancy, and file one report per region."

// scaleGoal is the completion condition handed to the goal gate. It is separate
// durable state from the pinned task, and both must survive.
const scaleGoal = "Every shard audited and one report filed per region"

// scalePlanItems is the checklist the agent writes on its first turn and then
// only ever re-statuses. Content is never rewritten, so any drift in the text
// at the end of the run is loss, not revision.
var scalePlanItems = []string{
	"Enumerate the shards in the ledger corpus",
	"Reconcile the CLEARING-7742 discrepancy",
	"Cross-check regional totals against the clearing file",
	"Draft one report per region",
	"File the reports and record the audit trail",
}

// --- the scripted provider ----------------------------------------------------

// scaleProvider drives the whole run: the parent's turns, every child's turns,
// and the compaction summarizer. One object because they share a call budget —
// the test's headline number is the total across all three.
//
// Routing is by request content rather than by call order, because children run
// interleaved with the parent and a positional script would drift.
type scaleProvider struct {
	mu sync.Mutex

	// schedule
	workTurns   int // parent turns before it starts trying to finish
	planEvery   int // re-status the plan every N parent turns
	spawnEvery  int // delegate every N parent turns
	spawnTarget int // stop delegating after this many children

	// counters
	parentTurns int
	childCalls  int
	summaries   int
	spawns      int
	planUpdates int
	finishes    int

	// lastParentRequest is the last set of messages the PARENT model was shown.
	// The goal and plan assertions are made against this, not against the
	// stored transcript: what matters is what the model actually sees.
	lastParentRequest []agentcore.Message
}

func (*scaleProvider) Name() string        { return "scale" }
func (*scaleProvider) SupportsTools() bool { return true }

func (p *scaleProvider) Chat(_ context.Context, req agentcore.ChatRequest) (agentcore.ChatResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	// Compaction borrows the run's provider. Answering in the real checkpoint
	// shape keeps the summary-of-a-summary fold running for real: each summary
	// is fed back as the "previous summary" of the next one, which is the exact
	// path along which an unpinned goal would decay.
	if len(req.Messages) > 0 && strings.HasPrefix(req.Messages[0].Content, "You are a context summarization") {
		p.summaries++
		return usageFor(req, agentcore.AssistantText(fmt.Sprintf(
			"## Goal\nContinue the audit\n## Progress\n### Done\n- [x] batch %d processed\n## Next Steps\n1. keep auditing",
			p.summaries))), nil
	}

	if isChildRequest(req.Messages) {
		p.childCalls++
		// Two turns per child: one unit of work, then its answer. The sentinel
		// is there because a child inherits the parent's composition.
		if hasToolResult(req.Messages) {
			return usageFor(req, agentcore.AssistantText("shard reconciled\n"+goal.Done)), nil
		}
		return usageFor(req, agentcore.AssistantToolCall(
			fmt.Sprintf("cw%d", p.childCalls), "work", fmt.Sprintf(`{"shard":%d}`, p.childCalls))), nil
	}

	p.parentTurns++
	n := p.parentTurns
	p.lastParentRequest = req.Messages

	switch {
	case n == 1:
		// Write the plan before doing anything else, the way an agent given a
		// multi-step task is instructed to.
		p.planUpdates++
		return usageFor(req, agentcore.AssistantToolCall("plan1", todo.ToolName, planArgs(0))), nil

	case n == p.workTurns-1:
		// Close the checklist out before answering, the way an agent is told to.
		// It is a separate turn from the finish so the final plan state is
		// written by the tool rather than assumed by the test.
		p.planUpdates++
		return usageFor(req, agentcore.AssistantToolCall(
			fmt.Sprintf("plan%d", p.planUpdates), todo.ToolName, planArgs(len(scalePlanItems)))), nil

	case n >= p.workTurns:
		p.finishes++
		if p.finishes == 1 {
			// No sentinel: the gate must re-open the run. A run that simply
			// ended would prove nothing about the gate still being armed after
			// thousands of turns.
			return usageFor(req, agentcore.AssistantText("Audit looks complete.")), nil
		}
		return usageFor(req, agentcore.AssistantText("All shards audited, reports filed.\n"+goal.Done)), nil

	case p.spawns < p.spawnTarget && n%p.spawnEvery == 0:
		p.spawns++
		return usageFor(req, agentcore.AssistantToolCall(
			fmt.Sprintf("sp%d", p.spawns), subagent.ToolSpawnSubagent,
			fmt.Sprintf(`{"task":%q}`, fmt.Sprintf("%s: reconcile shard %d and report", childTaskMarker, p.spawns)))), nil

	case n%p.planEvery == 0:
		// Re-status against how far through the run we are; the content never
		// changes, so the end state is comparable to what was written on turn 1.
		p.planUpdates++
		return usageFor(req, agentcore.AssistantToolCall(
			fmt.Sprintf("plan%d", p.planUpdates), todo.ToolName,
			planArgs(n*len(scalePlanItems)/p.workTurns))), nil

	default:
		return usageFor(req, agentcore.AssistantToolCall(
			fmt.Sprintf("w%d", n), "work", fmt.Sprintf(`{"n":%d}`, n))), nil
	}
}

func (p *scaleProvider) Stream(ctx context.Context, req agentcore.ChatRequest) (<-chan agentcore.ChatDelta, error) {
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

// childTaskMarker tags a delegated task so the provider can tell a child's
// request from the parent's. Children are seeded with the task text verbatim.
const childTaskMarker = "SHARD-SUBTASK"

func isChildRequest(msgs []agentcore.Message) bool {
	for _, m := range msgs {
		if m.Role == agentcore.RoleUser && strings.Contains(m.Content, childTaskMarker) {
			return true
		}
	}
	return false
}

func hasToolResult(msgs []agentcore.Message) bool {
	for _, m := range msgs {
		if m.Role == agentcore.RoleTool {
			return true
		}
	}
	return false
}

// planArgs renders the checklist with the first `done` items completed and the
// next one in progress. Item CONTENT is identical in every call, so the final
// plan can be compared to scalePlanItems character for character.
func planArgs(done int) string {
	done = min(max(done, 0), len(scalePlanItems))
	items := make([]todo.Item, 0, len(scalePlanItems))
	for i, content := range scalePlanItems {
		status := todo.StatusPending
		switch {
		case i < done:
			status = todo.StatusCompleted
		case i == done:
			status = todo.StatusInProgress
		}
		items = append(items, todo.Item{Content: content, Status: status})
	}
	b, err := json.Marshal(struct {
		Items []todo.Item `json:"items"`
	}{items})
	if err != nil {
		panic(err)
	}
	return string(b)
}

// --- footprint measurement ----------------------------------------------------

// footprint is what a durable run costs to keep, split into the two numbers
// that answer different questions. A single "size" number would hide the
// distinction that matters: one of these must stay flat as a run grows, the
// other cannot.
type footprint struct {
	sessions int // parent + one per delegated child
	entries  int // append-only records across all of them
	bytes    int // those records as they are serialized into storage
	retained int // of those bytes, the transcripts carried on compaction entries
}

// measure walks every session the run created and sizes the log the way storage
// actually stores it: internal/runtime marshals each SessionEntry to JSON and
// writes it as one row, so json.Marshal per entry is the honest unit rather
// than an in-memory struct size.
func measureFootprint(t *testing.T, store *e2eStore) footprint {
	t.Helper()
	var fp footprint
	store.mu.Lock()
	defer store.mu.Unlock()
	for _, entries := range store.log {
		fp.sessions++
		fp.entries += len(entries)
		for _, e := range entries {
			b, err := json.Marshal(e)
			if err != nil {
				t.Fatalf("marshal entry: %v", err)
			}
			fp.bytes += len(b)
			if len(e.Retained) > 0 {
				r, err := json.Marshal(e.Retained)
				if err != nil {
					t.Fatalf("marshal retained: %v", err)
				}
				fp.retained += len(r)
			}
		}
	}
	return fp
}

// transcriptBytes sizes a live message window the way the provider bills it.
func transcriptBytes(msgs []agentcore.Message) int {
	n := 0
	for _, m := range msgs {
		n += len(m.Content)
		for _, tc := range m.ToolCalls {
			n += len(tc.Name) + len(tc.Arguments)
		}
	}
	return n
}

// --- the run ------------------------------------------------------------------

// scaleResult is everything the assertions need from one run.
type scaleResult struct {
	res    agentcore.RunResult
	prov   *scaleProvider
	plan   *todo.Store
	store  *e2eStore
	work   *e2eWorkTool
	fp     footprint
	llm    int // total provider calls: parent + children + summaries
	sessID string
}

// runAtScale drives one durable run of the requested length with the plan, the
// goal gate, delegation, compaction and the session log all live.
func runAtScale(t *testing.T, sessionID string, workTurns, spawnTarget int) scaleResult {
	t.Helper()

	prov := &scaleProvider{
		workTurns:   workTurns,
		planEvery:   max(workTurns/20, 2),
		spawnEvery:  max(workTurns/(spawnTarget+1), 2),
		spawnTarget: spawnTarget,
	}
	store := newE2EStore()
	plan := todo.NewStore()
	work := &e2eWorkTool{size: 600}

	limits := agentcore.DefaultLimits()
	limits.MaxTurns = 4 * workTurns
	limits.MaxToolCalls = 4 * workTurns
	// A tight window relative to the per-turn payload, so compaction runs
	// hundreds of times rather than a handful. That frequency is the point: the
	// goal has to survive a summary of a summary of a summary.
	limits.MaxContextTokens = 4000

	cs := agentcore.DefaultCompactionSettings()
	cs.KeepRecentTokens = 1500

	agent, err := agentcore.Build(
		e2eConfig{cfg: agentcore.Config{
			Provider:   prov,
			Model:      "scale-model",
			Tools:      agentcore.NewToolSet(work),
			Policy:     agentcore.NewAllowList("work", todo.ToolName, subagent.ToolSpawnSubagent),
			Limits:     &limits,
			Compaction: &cs,
			Session:    store,
			SessionID:  sessionID,
		}},
		goal.Until(scaleGoal),
		todo.With(plan),
		// No MaxPerRun: the budget is derived from the run's tool ceiling, which
		// is exactly the behavior a run this long needs (a fixed 8 would refuse
		// every spawn after the eighth).
		subagent.SelfOnly(),
	)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	res, err := agent.Prompt(context.Background(), scaleTask)
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}

	prov.mu.Lock()
	llm := prov.parentTurns + prov.childCalls + prov.summaries
	prov.mu.Unlock()

	return scaleResult{
		res: res, prov: prov, plan: plan, store: store, work: work,
		fp: measureFootprint(t, store), llm: llm, sessID: sessionID,
	}
}

// --- the test -----------------------------------------------------------------

// TestVeryLongRunKeepsItsGoalAndPlan is the headline: ~5000 model calls,
// thousands of tool calls, hundreds of delegated children, and at the end the
// agent is still working on the task it was given, with its checklist intact
// and a bounded amount of state.
func TestVeryLongRunKeepsItsGoalAndPlan(t *testing.T) {
	r := runAtScale(t, "scale-longrun", 4200, 300)
	p := r.prov

	// --- the run really was at scale ---------------------------------------
	// Asserted first, because every claim below is only interesting if the run
	// was long enough to threaten it. A regression that quietly shortened the
	// run would otherwise turn this whole file green and meaningless.
	t.Logf("model calls: %d total (parent %d, children %d, compaction summaries %d)",
		r.llm, p.parentTurns, p.childCalls, p.summaries)
	t.Logf("tool calls: %d work, %d plan updates, %d children spawned", r.work.Calls(), p.planUpdates, p.spawns)

	if r.llm < 5000 {
		t.Fatalf("run was not long enough to prove anything: %d model calls", r.llm)
	}
	if r.work.Calls() < 3000 {
		t.Fatalf("expected thousands of tool calls, got %d", r.work.Calls())
	}
	if p.spawns < 300 {
		t.Fatalf("expected hundreds of children, got %d", p.spawns)
	}
	if p.summaries < 100 {
		t.Fatalf("compaction barely engaged (%d summaries): the window was never rewritten enough "+
			"to threaten the goal, so this test proves nothing", p.summaries)
	}

	// --- the goal survived --------------------------------------------------
	// The load-bearing pair. First: the message that originally carried the task
	// is GONE from the window — compaction really did summarize it away, so the
	// pin below is doing the work rather than the original message still being
	// present by luck.
	last := p.lastParentRequest
	for _, m := range last {
		if m.Role == agentcore.RoleUser && m.Content == scaleTask {
			t.Fatal("the original user message is still in the window: compaction never reached it, " +
				"so this run does not test whether the goal survives being summarized away")
		}
	}
	// Second: the task is nonetheless in front of the model, word for word,
	// carried by the pin that every compaction preserves.
	pins := 0
	for _, m := range last {
		if m.Role == agentcore.RoleSystem && strings.HasPrefix(m.Content, "[pinned goal") {
			pins++
			if !strings.Contains(m.Content, scaleTask) {
				t.Fatalf("the pinned goal drifted from the original task:\n%q", m.Content)
			}
		}
	}
	if pins != 1 {
		t.Fatalf("want exactly one pinned goal in the final window, got %d "+
			"(0 = the objective was summarized away; >1 = each compaction is adding another)", pins)
	}

	// The gate's condition is separate durable state, and it must not have
	// drifted either — nor been recorded twice by the hundreds of compactions.
	log, err := r.store.Log(context.Background(), r.sessID)
	if err != nil {
		t.Fatalf("Log: %v", err)
	}
	goals := 0
	for _, e := range log {
		if e.Kind == agentcore.EntryGoal {
			goals++
			if e.Goal != scaleGoal {
				t.Fatalf("goal drifted in the log: %q", e.Goal)
			}
		}
	}
	if goals != 1 {
		t.Fatalf("want the goal recorded exactly once, got %d entries", goals)
	}

	// And it was still being enforced at the end: the run's first finish was
	// rejected for missing the sentinel and only the second was accepted.
	if p.finishes < 2 {
		t.Fatalf("the goal gate never re-opened the premature finish (finishes=%d) — "+
			"after %d turns it had stopped holding the run to its contract", p.finishes, p.parentTurns)
	}
	if !strings.Contains(r.res.Final, goal.Done) {
		t.Fatalf("run ended without the completion sentinel: %q (stop=%q)", r.res.Final, r.res.StopReason)
	}
	if r.res.StopReason == goal.StopReasonStalled {
		t.Fatalf("run stalled rather than completing: %q", r.res.Final)
	}

	// --- the plan survived --------------------------------------------------
	// Whole, in order, and with the text unchanged: the provider only ever
	// re-statuses items, so any difference here is loss.
	items := r.plan.List()
	if len(items) != len(scalePlanItems) {
		t.Fatalf("plan lost items: %d of %d remain", len(items), len(scalePlanItems))
	}
	for i, want := range scalePlanItems {
		if items[i].Content != want {
			t.Fatalf("plan item %d drifted:\n got %q\nwant %q", i, items[i].Content, want)
		}
	}
	if items[len(items)-1].Status != todo.StatusCompleted {
		t.Fatalf("plan never finished: last item is %q", items[len(items)-1].Status)
	}

	// Holding the plan is not the same as showing it. The claim that matters is
	// that the model could still SEE its checklist on its last turn, thousands
	// of turns and hundreds of compactions after writing it.
	rendered := ""
	for _, m := range last {
		if m.Role == agentcore.RoleSystem && strings.HasPrefix(m.Content, todo.ContextPrefix) {
			rendered = m.Content
		}
	}
	if rendered == "" {
		t.Fatal("the plan was not pinned into the final request: the agent held a checklist it could no longer read")
	}
	for _, want := range scalePlanItems {
		if !strings.Contains(rendered, want) {
			t.Fatalf("plan item missing from the pinned checklist: %q\n%s", want, rendered)
		}
	}

	// --- the footprint ------------------------------------------------------
	windowBytes := transcriptBytes(r.res.Messages)
	resumed := agentcore.ReduceSession(log)
	resumeBytes := transcriptBytes(resumed.Messages)

	// What a resume actually READS, which is a different question from what it
	// ends up with. Reducing to 35 messages is no comfort if getting there meant
	// pulling 10,000 entries out of Postgres first — the fold is cheap, the read
	// is not, and it is the read that grows with the run.
	resumeWindow, werr := agentcore.LoadResumeLog(context.Background(), r.store, r.sessID)
	if werr != nil {
		t.Fatalf("LoadResumeLog: %v", werr)
	}
	t.Logf("live window:  %d messages, %d KiB", len(r.res.Messages), windowBytes/1024)
	t.Logf("resume loads: %d messages, %d KiB, read from %d of %d log entries",
		len(resumed.Messages), resumeBytes/1024, len(resumeWindow), len(log))

	// The read a resume performs must be bounded by the context window, not by
	// the length of the run. Anything else means crash recovery gets slower the
	// longer the agent has been working — precisely backwards.
	if len(resumeWindow) > 100 {
		t.Fatalf("resume read %d entries after %d turns; the read still scales with the run",
			len(resumeWindow), p.parentTurns)
	}
	// And it must still be the SAME resume: a smaller read that recovers a
	// different conversation is not an optimization.
	if !reflect.DeepEqual(agentcore.ReduceSession(resumeWindow), resumed) {
		t.Fatal("the windowed read recovers a different state than the whole log")
	}
	t.Logf("durable log:  %d sessions, %d entries, %d KiB (%d KiB of it compaction transcripts), %d B/turn",
		r.fp.sessions, r.fp.entries, r.fp.bytes/1024, r.fp.retained/1024, r.fp.bytes/p.parentTurns)

	// The window the model reasons in must be a function of the context budget,
	// not of how long the run has been going. 4200 turns uncompacted would be
	// ~8400 messages and several megabytes.
	if len(r.res.Messages) > 80 {
		t.Fatalf("the live window grew with the run: %d messages after %d turns", len(r.res.Messages), p.parentTurns)
	}
	if windowBytes > 2*limitsWindowBytes {
		t.Fatalf("the live window is %d KiB, far past the %d-token budget it was given",
			windowBytes/1024, 4000)
	}

	// What a crashed run pays to come back. This is the number that would
	// silently degrade if a completed compaction stopped acting as a
	// checkpoint: resume would replay the whole log instead of starting from
	// the last summary, and every recovery of a long run would load megabytes.
	if len(resumed.Messages) > 80 {
		t.Fatalf("resume would load %d messages: the log is being replayed rather than resumed from its "+
			"last compaction checkpoint", len(resumed.Messages))
	}

	// The append-only log is the one thing that CANNOT be bounded — it is the
	// run's archive, and a 4200-turn run writes 4200 turns of records. What it
	// must not do is grow faster than the run: the cost of a turn has to stay
	// flat, so a run twice as long costs twice as much and not four times.
	// Sub-linearity is checked directly in TestLogGrowthIsLinearInRunLength.
	perTurn := r.fp.bytes / p.parentTurns
	if perTurn > 4096 {
		t.Fatalf("the log costs %d B per turn: something is accumulating per-turn state", perTurn)
	}
}

// limitsWindowBytes is the byte size of the 4000-token context budget the scale
// run is given, at the ~4-bytes-per-token estimate the loop itself uses. The
// window is allowed a multiple of it (compaction fires AFTER the budget is
// exceeded, and the tail is kept whole), but not an unbounded one.
const limitsWindowBytes = 4000 * 4

// TestLogGrowthIsLinearInRunLength is the super-linearity detector, and it is
// here because the obvious way to make resume cheap is to make writing
// expensive. A completed compaction stores the transcript it left behind, so a
// run that compacts hundreds of times writes hundreds of transcripts; if each
// one carried more than a bounded window — the whole history, say — the log
// would grow with the SQUARE of the run and a long agent would quietly become
// unaffordable to store.
//
// Two runs, one four times the other. Linear growth lands near 4x. Quadratic
// growth lands near 16x, and the ceiling below is set to catch it long before
// that.
func TestLogGrowthIsLinearInRunLength(t *testing.T) {
	small := runAtScale(t, "scale-growth-small", 250, 10)
	large := runAtScale(t, "scale-growth-large", 1000, 40)

	turnRatio := float64(large.prov.parentTurns) / float64(small.prov.parentTurns)
	byteRatio := float64(large.fp.bytes) / float64(small.fp.bytes)

	t.Logf("small: %d turns, %d entries, %d KiB (%d KiB retained)",
		small.prov.parentTurns, small.fp.entries, small.fp.bytes/1024, small.fp.retained/1024)
	t.Logf("large: %d turns, %d entries, %d KiB (%d KiB retained)",
		large.prov.parentTurns, large.fp.entries, large.fp.bytes/1024, large.fp.retained/1024)
	t.Logf("%.1fx the turns cost %.1fx the bytes", turnRatio, byteRatio)

	if byteRatio > 1.5*turnRatio {
		t.Fatalf("the log grows faster than the run: %.1fx the turns cost %.1fx the bytes — "+
			"per-turn cost is not flat, so a long run's storage compounds", turnRatio, byteRatio)
	}

	// The other direction: the two runs must actually differ in length, or the
	// ratio is comparing a run to itself and passes for free.
	if turnRatio < 3 {
		t.Fatalf("the two runs are too close in length (%.1fx) to say anything about growth", turnRatio)
	}
}

// --- the summarizer that tells the truth -------------------------------------

// TestVeryLongRunKeepsItsCheckpointInsideTheBudget closes the blind spot in the
// test above.
//
// scaleProvider answers every compaction with a fixed 130-byte string. That is
// convenient and it is a lie: it makes the checkpoint a constant, when the whole
// reason a checkpoint is dangerous is that it is not one. The update prompt asks
// the summarizer to preserve what it already captured and fold in what is new,
// which is a monotonic instruction — every fold appends and none subtracts — so
// a model that simply DOES WHAT IT IS ASKED returns a longer checkpoint each
// time. A fixed-size stub can never show that, and so the run above passed
// while the loop had no ceiling on the summary at all.
//
// What that cost, measured on this test before the ceiling existed: the
// checkpoint climbed until the provider's own output cap stopped it at 2048
// tokens — half the entire 4000-token budget — and stayed there. The window
// never came back under its ceiling after a compaction, so compaction re-fired
// on the next turn: 1 summarization call per 2.1 turns of actual work, and 37%
// of the run spent over the budget the run was given. Nothing crashed. The agent
// just quietly spent half its model calls re-summarizing and ran permanently
// over a limit that existed to keep it inside a real model's window.
//
// So this run uses a summarizer that folds honestly, and asserts the three
// numbers that distinguish a bounded checkpoint from a ratcheting one.
func TestVeryLongRunKeepsItsCheckpointInsideTheBudget(t *testing.T) {
	const (
		turns  = 1500
		budget = 4000 // tokens
	)

	prov := &foldingProvider{finishAt: turns}
	store := newE2EStore()
	work := &e2eWorkTool{size: 600}

	limits := agentcore.DefaultLimits()
	limits.MaxTurns = 2 * turns
	limits.MaxToolCalls = 2 * turns
	limits.MaxContextTokens = budget

	cs := agentcore.DefaultCompactionSettings()
	cs.KeepRecentTokens = 1500

	agent, err := agentcore.Build(
		e2eConfig{cfg: agentcore.Config{
			Provider:   prov,
			Model:      "fold-model",
			Tools:      agentcore.NewToolSet(work),
			Policy:     agentcore.NewAllowList("work"),
			Limits:     &limits,
			Compaction: &cs,
			Session:    store,
			SessionID:  "scale-fold",
		}},
		goal.Until(scaleGoal),
	)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if _, err := agent.Prompt(context.Background(), scaleTask); err != nil {
		t.Fatalf("Prompt: %v", err)
	}

	prov.mu.Lock()
	defer prov.mu.Unlock()

	if prov.summaries < 50 {
		t.Fatalf("only %d compactions: the checkpoint was never folded enough times to ratchet, "+
			"so this test proves nothing", prov.summaries)
	}

	over := 0
	maxWindow := 0
	for _, b := range prov.windowBytes {
		if b > limitsWindowBytes {
			over++
		}
		if b > maxWindow {
			maxWindow = b
		}
	}
	perCompaction := float64(prov.turns) / float64(prov.summaries)
	t.Logf("turns %d, compactions %d (1 per %.1f turns)", prov.turns, prov.summaries, perCompaction)
	t.Logf("checkpoint: %d B at its largest (ceiling %d B)", prov.maxSummaryBytes, checkpointCeilingBytes)
	t.Logf("window: %d B at its largest, %d of %d turns over the %d B budget",
		maxWindow, over, prov.turns, limitsWindowBytes)

	// The direct assertion: the checkpoint has a ceiling and it holds. The slack
	// is for the elision marker the clamp inserts when it has to cut.
	if prov.maxSummaryBytes > checkpointCeilingBytes+128 {
		t.Fatalf("the checkpoint grew to %d B against a %d B ceiling: nothing bounds the summary, "+
			"so it will keep ratcheting until it fills the window by itself",
			prov.maxSummaryBytes, checkpointCeilingBytes)
	}

	// The consequence that actually hurts: a compaction must return the
	// transcript to UNDER the budget, not merely trim it. A run that sits over
	// its ceiling has lost the guarantee the ceiling exists for — that the
	// request still fits a real model's window.
	if over > prov.turns/100 {
		t.Fatalf("%d of %d turns ran over the %d B budget: compaction is no longer bringing the "+
			"transcript back under the ceiling", over, prov.turns, limitsWindowBytes)
	}

	// The cost. A compaction that does not buy headroom re-fires immediately, and
	// each firing is a full model call — the failure shows up on the bill and in
	// latency long before it shows up as an error.
	if perCompaction < 4 {
		t.Fatalf("compaction ran once per %.1f turns: it is thrashing, so the run spends a large "+
			"fraction of its model calls re-summarizing instead of working", perCompaction)
	}
}

// checkpointCeilingBytes is the share of the run's context budget a compaction
// checkpoint is allowed to occupy — a quarter of it, at the loop's own
// ~4-bytes-per-token estimate. The recent tail already claims half, so a
// checkpoint at this ceiling leaves a compaction real headroom before the next.
const checkpointCeilingBytes = (4000 / 4) * 4

// foldingProvider is scaleProvider's summarizer, told the truth: it obeys the
// update prompt literally, carrying the previous checkpoint forward and
// appending the new work to it. That is the honest reading of "preserve what you
// captured and fold in what is new", and it is what makes a checkpoint grow.
//
// It deliberately IGNORES the request's MaxTokens. A hosted provider honors it,
// but a self-hosted or OpenAI-compatible endpoint may not, and the difference
// must not matter: the loop asks for a bounded checkpoint and must also enforce
// one on what comes back. Ignoring the cap here is what makes this test cover
// the enforcement rather than the request.
type foldingProvider struct {
	mu       sync.Mutex
	finishAt int

	turns     int
	summaries int

	maxSummaryBytes int
	windowBytes     []int // bytes the PARENT model was shown, per turn
	lastReq         []agentcore.Message
}

func (*foldingProvider) Name() string        { return "fold" }
func (*foldingProvider) SupportsTools() bool { return true }

func (p *foldingProvider) Chat(_ context.Context, req agentcore.ChatRequest) (agentcore.ChatResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if len(req.Messages) > 0 && strings.HasPrefix(req.Messages[0].Content, "You are a context summarization") {
		p.summaries++
		out := priorCheckpoint(req.Messages[1].Content)
		if out == "" {
			out = "## Goal\n" + scaleGoal + "\n## Progress\n### Done"
		}
		out += fmt.Sprintf("\n- [x] fold %d: reconciled shard set %d against the clearing file", p.summaries, p.summaries)
		if len(out) > p.maxSummaryBytes {
			p.maxSummaryBytes = len(out)
		}
		return usageFor(req, agentcore.AssistantText(out)), nil
	}

	p.turns++
	p.lastReq = req.Messages
	p.windowBytes = append(p.windowBytes, transcriptBytes(req.Messages))
	if p.turns >= p.finishAt {
		return usageFor(req, agentcore.AssistantText("All shards audited.\n"+goal.Done)), nil
	}
	return usageFor(req, agentcore.AssistantToolCall(
		fmt.Sprintf("w%d", p.turns), "work", fmt.Sprintf(`{"n":%d}`, p.turns))), nil
}

func (p *foldingProvider) Stream(ctx context.Context, req agentcore.ChatRequest) (<-chan agentcore.ChatDelta, error) {
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

// priorCheckpoint pulls the "## Previous summary" section back out of an update
// request, which is how the fold is made real: what the loop hands back as the
// previous checkpoint is exactly what this provider carries forward, so the
// growth measured here is the growth a real summarizer would produce.
func priorCheckpoint(userContent string) string {
	const head = "## Previous summary\n"
	i := strings.Index(userContent, head)
	if i < 0 {
		return ""
	}
	rest := userContent[i+len(head):]
	if j := strings.Index(rest, "\n\n## New messages since that summary"); j >= 0 {
		return rest[:j]
	}
	return rest
}

// --- the requirement the user changed their mind about -----------------------

// scaleSteeredTask is what the run is originally asked to do, and
// scaleCorrection is the user changing their mind 150 turns in. They contradict
// on purpose: an agent that ends the run holding both, with no statement of
// which wins, has not actually been corrected.
const (
	scaleSteeredTask = "Audit every shard in the LEDGER corpus and file one report per region."
	scaleCorrection  = "CHANGE OF PLAN: stop auditing the ledger corpus entirely. Audit the PAYROLL corpus instead and file one report per department."
	scaleSecondFix   = "One more change: include contractor payroll in the department reports."
)

// TestVeryLongRunFollowsTheRequirementTheUserChangedItTo is the other half of
// "the objective must not drift", and the half that pinning got backwards.
//
// TestVeryLongRunKeepsItsGoalAndPlan proves the original task survives being
// summarized away. That protection is real, and on its own it is dangerous: a
// run of any length gets steered, and a pin built from the FIRST user message
// holds the requirement the user has since cancelled. The correction, being an
// ordinary transcript message, is summarized like anything else and diluted by
// each fold after that.
//
// Measured on this run before the pin was made to accumulate: at turn 1200,
// after 108 compactions, the superseded LEDGER task was in front of the model
// word for word under a header reading "keep working toward it", and the word
// PAYROLL — the thing the user actually asked for — appeared nowhere in the
// window at all. The agent was not confused. It was diligently working on a
// cancelled requirement, and every mechanism in the run was helping it.
func TestVeryLongRunFollowsTheRequirementTheUserChangedItTo(t *testing.T) {
	const (
		turns  = 1200
		budget = 4000
	)

	prov := &foldingProvider{finishAt: turns}
	store := newE2EStore()
	work := &e2eWorkTool{size: 600}

	// Two corrections, far apart, both long after the first compaction has
	// already pinned the original.
	var steered []string
	steer := func(context.Context) []agentcore.Message {
		prov.mu.Lock()
		n := prov.turns
		prov.mu.Unlock()
		switch {
		case len(steered) == 0 && n >= 150:
			steered = append(steered, scaleCorrection)
			return []agentcore.Message{{Role: agentcore.RoleUser, Content: scaleCorrection}}
		case len(steered) == 1 && n >= 600:
			steered = append(steered, scaleSecondFix)
			return []agentcore.Message{{Role: agentcore.RoleUser, Content: scaleSecondFix}}
		}
		return nil
	}

	limits := agentcore.DefaultLimits()
	limits.MaxTurns = 2 * turns
	limits.MaxToolCalls = 2 * turns
	limits.MaxContextTokens = budget

	cs := agentcore.DefaultCompactionSettings()
	cs.KeepRecentTokens = 1500

	agent, err := agentcore.Build(
		e2eConfig{cfg: agentcore.Config{
			Provider:            prov,
			Model:               "fold-model",
			Tools:               agentcore.NewToolSet(work),
			Policy:              agentcore.NewAllowList("work"),
			Limits:              &limits,
			Compaction:          &cs,
			Session:             store,
			SessionID:           "scale-steered",
			GetSteeringMessages: steer,
		}},
		goal.Until(scaleGoal),
	)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if _, err := agent.Prompt(context.Background(), scaleSteeredTask); err != nil {
		t.Fatalf("Prompt: %v", err)
	}

	prov.mu.Lock()
	last := prov.lastReq
	summaries := prov.summaries
	prov.mu.Unlock()

	if len(steered) != 2 {
		t.Fatalf("the run ended before both corrections were delivered (%d): nothing was steered, "+
			"so this test proves nothing", len(steered))
	}
	if summaries < 50 {
		t.Fatalf("only %d compactions: the corrections were never summarized away, so the pin was "+
			"never what was holding them", summaries)
	}

	// The corrections were delivered hundreds of turns ago and compacted many
	// times since. First: they really are gone from the transcript, so whatever
	// carries them now is doing so deliberately.
	for _, m := range last {
		if m.Role == agentcore.RoleUser && strings.TrimSpace(m.Content) == scaleCorrection {
			t.Fatal("the correction is still in the window as an ordinary message: compaction never " +
				"reached it, so this run does not test whether a correction survives being summarized away")
		}
	}

	pin := ""
	for _, m := range last {
		if m.Role == agentcore.RoleSystem && strings.HasPrefix(m.Content, "[pinned goal") {
			if pin != "" {
				t.Fatal("two pinned-requirement messages in one window: the pin is being appended to " +
					"rather than rebuilt, which is how a long run accumulates a second objective")
			}
			pin = m.Content
		}
	}
	if pin == "" {
		t.Fatal("no pinned requirement in the final window")
	}
	t.Logf("after %d turns and %d compactions the model is looking at:\n%s", turns, summaries, pin)

	// Both corrections are in front of the model, word for word.
	for _, want := range []string{scaleCorrection, scaleSecondFix} {
		if !strings.Contains(pin, want) {
			t.Fatalf("the pin lost a correction the user steered in:\n missing %q\n pin:\n%s", want, pin)
		}
	}

	// The original stays too — "stop auditing the ledger corpus" is not
	// actionable on its own, and neither is "include contractor payroll".
	if !strings.Contains(pin, scaleSteeredTask) {
		t.Fatalf("the pin dropped the original task, leaving corrections with nothing to correct:\n%s", pin)
	}

	// And the pin says which one wins. Two contradictory requirements in one
	// message with no ordering is worse than either alone.
	if !strings.Contains(pin, "supersede") {
		t.Fatalf("the pin states the original and its corrections without saying which takes "+
			"precedence, so the model is left to guess:\n%s", pin)
	}
	if strings.Index(pin, scaleSteeredTask) > strings.Index(pin, scaleCorrection) {
		t.Fatalf("corrections are rendered before the task they correct, inverting the precedence "+
			"the separator claims:\n%s", pin)
	}
	if strings.Index(pin, scaleCorrection) > strings.Index(pin, scaleSecondFix) {
		t.Fatalf("the two corrections are out of order, so 'later supersedes earlier' points the "+
			"wrong way:\n%s", pin)
	}

	// The pin is preserved verbatim forever, so it needs a ceiling for the same
	// reason the checkpoint does — a run steered a thousand times must not end up
	// with a thousand-entry pin filling the window.
	if len(pin) > checkpointCeilingBytes {
		t.Fatalf("the pin grew to %d B: it accumulates without a ceiling, which is the checkpoint "+
			"ratchet again in a message compaction never even summarizes", len(pin))
	}
}

// The run plan at scale.
//
// The plan is the one piece of run state that is immune to compaction *by
// construction*: it lives in a Store, not the transcript, and a context hook
// re-injects a fresh rendering into every request. That is the whole reason the
// capability exists — after a thousand turns the original task has been
// summarized away, but the checklist is still right there.
//
// It is also why the plan is dangerous. Anything that survives compaction and is
// pinned into every request subtracts from the same window compaction is fighting
// to protect, and it does so on every single turn for the rest of the run. So it
// needs a ceiling for exactly the reason the compaction checkpoint needed one.
//
// The existing scale test writes a fixed five-item plan and only re-statuses it.
// That is the friendly case. A real long run does not work that way: an agent
// decomposes as it discovers, so the checklist GROWS — new subtasks appended,
// finished ones left behind as a record of progress. These tests drive that
// shape.

// planScaleTask is a task whose shape invites decomposition, because that is the
// shape that grows a checklist.
const planScaleTask = "Audit every shard in the ledger corpus and file one report per region."

const planScaleGoal = "Every shard audited and one report filed per region"

// --- a model that discovers work as it goes -----------------------------------

// growingPlanProvider plays an agent that decomposes. Every planEvery turns it
// closes out the step it was on and appends the subtask it just discovered, then
// starts on that. Nothing is ever deleted — a finished item is the record that
// the work happened, which is exactly why a model keeps it.
type growingPlanProvider struct {
	mu sync.Mutex

	workTurns int
	planEvery int

	parentTurns int
	summaries   int
	planUpdates int
	finishes    int

	// discovered is the checklist as the model has built it so far.
	discovered []todo.Item

	// lastRequest is what the model was shown on its most recent turn. Every
	// assertion here is made against this, because the question is what the run
	// COSTS to keep the plan in front of the model, not what the store holds.
	lastRequest []agentcore.Message
}

func (*growingPlanProvider) Name() string        { return "growing-plan" }
func (*growingPlanProvider) SupportsTools() bool { return true }

func (p *growingPlanProvider) Chat(_ context.Context, req agentcore.ChatRequest) (agentcore.ChatResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if len(req.Messages) > 0 && strings.HasPrefix(req.Messages[0].Content, "You are a context summarization") {
		p.summaries++
		return usageFor(req, agentcore.AssistantText(fmt.Sprintf(
			"## Goal\nContinue the audit\n## Progress\n### Done\n- [x] batch %d processed\n## Next Steps\n1. keep auditing",
			p.summaries))), nil
	}

	p.parentTurns++
	n := p.parentTurns
	p.lastRequest = req.Messages

	switch {
	case n >= p.workTurns:
		p.finishes++
		if p.finishes == 1 {
			return usageFor(req, agentcore.AssistantText("Audit looks complete.")), nil
		}
		return usageFor(req, agentcore.AssistantText("All shards audited, reports filed.\n"+goal.Done)), nil

	case n == 1 || n%p.planEvery == 0:
		p.planUpdates++
		p.discover()
		return usageFor(req, agentcore.AssistantToolCall(
			fmt.Sprintf("plan%d", p.planUpdates), todo.ToolName, planItemArgs(p.discovered))), nil

	default:
		return usageFor(req, agentcore.AssistantToolCall(
			fmt.Sprintf("w%d", n), "work", fmt.Sprintf(`{"n":%d}`, n))), nil
	}
}

// discover closes the current step and appends the one it uncovered. Item text
// is the length a real agent writes — a short imperative sentence, not a word.
func (p *growingPlanProvider) discover() {
	for i := range p.discovered {
		if p.discovered[i].Status == todo.StatusInProgress {
			p.discovered[i].Status = todo.StatusCompleted
		}
	}
	p.discovered = append(p.discovered, todo.Item{
		Content: fmt.Sprintf("Reconcile shard %d against the regional clearing file and note the discrepancy",
			len(p.discovered)+1),
		Status: todo.StatusInProgress,
	})
}

func (p *growingPlanProvider) Stream(ctx context.Context, req agentcore.ChatRequest) (<-chan agentcore.ChatDelta, error) {
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
		ch <- agentcore.ChatDelta{Done: true}
	}()
	return ch, nil
}

func planItemArgs(items []todo.Item) string {
	b, err := json.Marshal(struct {
		Items []todo.Item `json:"items"`
	}{items})
	if err != nil {
		panic(err)
	}
	return string(b)
}

// injectedPlanBytes sizes the pinned checklist as the provider bills it: the
// trailing system reminder the context hook adds to the request.
func injectedPlanBytes(msgs []agentcore.Message) int {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == agentcore.RoleSystem && strings.HasPrefix(msgs[i].Content, todo.ContextPrefix) {
			return len(msgs[i].Content)
		}
	}
	return 0
}

// --- the test -----------------------------------------------------------------

// planCeilingBytes is the share of the window the pinned plan may hold.
//
// The run below is capped at 4000 context tokens, and the plan is charged
// against that on every turn forever — it is not a cost the run pays once. A
// checklist is a navigation aid, not a record, so it gets a smaller share than
// the compaction checkpoint's budget/4: one eighth of the window, ~500 tokens at
// the ~4-bytes-per-token the loop estimates with.
const planCeilingBytes = (4000 / 8) * 4

// TestVeryLongRunKeepsItsPlanInsideTheBudget is the round's headline. An agent
// that decomposes for 900 turns builds a long checklist, and every item of it is
// pinned into every request from then on. Unbounded, that is a slow-motion
// context leak that compaction cannot touch and that gets worse the longer the
// run goes — the exact opposite of what the plan is for.
func TestVeryLongRunKeepsItsPlanInsideTheBudget(t *testing.T) {
	if testing.Short() {
		t.Skip("long run")
	}

	const workTurns = 900

	prov := &growingPlanProvider{workTurns: workTurns, planEvery: 3}
	store := newE2EStore()
	plan := todo.NewStore()
	work := &e2eWorkTool{size: 600}

	limits := agentcore.DefaultLimits()
	limits.MaxTurns = 4 * workTurns
	limits.MaxToolCalls = 4 * workTurns
	limits.MaxContextTokens = 4000

	cs := agentcore.DefaultCompactionSettings()
	cs.KeepRecentTokens = 1500

	agent, err := agentcore.Build(
		e2eConfig{cfg: agentcore.Config{
			Provider:   prov,
			Model:      "plan-scale",
			Tools:      agentcore.NewToolSet(work),
			Policy:     agentcore.NewAllowList("work", todo.ToolName),
			Limits:     &limits,
			Compaction: &cs,
			Session:    store,
			SessionID:  "plan-scale",
		}},
		goal.Until(planScaleGoal),
		todo.With(plan),
	)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	if _, err := agent.Prompt(context.Background(), planScaleTask); err != nil {
		t.Fatalf("Prompt: %v", err)
	}

	prov.mu.Lock()
	last := prov.lastRequest
	updates := prov.planUpdates
	items := len(prov.discovered)
	prov.mu.Unlock()

	pinned := injectedPlanBytes(last)
	window := transcriptBytes(last)

	t.Logf("plan updates=%d items=%d pinned=%dB window=%dB (plan is %d%% of the window)",
		updates, items, pinned, window, pinned*100/max(window, 1))

	if pinned == 0 {
		t.Fatal("no plan was pinned into the final request — the checklist the whole capability exists to keep is not there")
	}
	if pinned > planCeilingBytes {
		t.Fatalf("the pinned plan is %d bytes against a %d-byte share of the window: it grows with the run, "+
			"is immune to compaction by construction, and is charged on every turn — so a long run pays it "+
			"forever and it crowds out the work it was meant to keep on track", pinned, planCeilingBytes)
	}

	// Bounding the plan must not mean losing the model's place in it. What the
	// agent is doing RIGHT NOW is the one line it cannot navigate without.
	current := fmt.Sprintf("Reconcile shard %d against the regional clearing file", items)
	if !strings.Contains(planText(last), current) {
		t.Fatalf("the plan was bounded by dropping the in_progress step, which is the one item that "+
			"decides the next action:\n%s", planText(last))
	}

	// And progress must still be legible: an agent that cannot tell how far it
	// has come will redo work, which costs far more than the bytes saved.
	if !strings.Contains(planText(last), "completed") {
		t.Fatalf("the bounded plan does not account for the finished steps at all, so the run cannot "+
			"tell what it has already done:\n%s", planText(last))
	}
}

func planText(msgs []agentcore.Message) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == agentcore.RoleSystem && strings.HasPrefix(msgs[i].Content, todo.ContextPrefix) {
			return msgs[i].Content
		}
	}
	return ""
}

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
