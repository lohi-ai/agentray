package agentcore_test

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/lohi-ai/agentray/agentcore"
	"github.com/lohi-ai/agentray/agentcore/plugins/goal"
	"github.com/lohi-ai/agentray/agentcore/plugins/subagent"
	"github.com/lohi-ai/agentray/agentcore/plugins/todo"
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
