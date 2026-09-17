package agentcore_test

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/lohi-ai/agentray/agentcore"
	"github.com/lohi-ai/agentray/agentcore/plugins/goal"
	"github.com/lohi-ai/agentray/agentcore/plugins/subagent"
	"github.com/lohi-ai/agentray/agentcore/plugins/todo"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
)

// The corrected requirement across a crash.
//
// TestVeryLongRunFollowsTheRequirementTheUserChangedItTo proves the pin carries
// a mid-run correction through hundreds of compactions. That is one of the two
// ways a long run loses its objective. The other is a crash.
//
// Resume does not replay the log message by message — it restarts from the last
// compaction's Retained transcript, and retainedTranscript strips the run's own
// leading system prompt on the way in (the resuming run re-derives its persona).
// The pin is also a system message, sitting right behind that one. If it goes
// out with it, a recovered run comes back working on the requirement the user
// cancelled, with nothing in the window to say otherwise — the exact failure R2
// fixed, reintroduced by the recovery path.

const (
	pinTask       = "Audit every shard in the LEDGER corpus and file one report per region."
	pinCorrection = "CHANGE OF PLAN: stop auditing the ledger corpus entirely. Audit the PAYROLL corpus instead and file one report per department."
)

// TestCorrectedRequirementSurvivesACrash runs long enough to compact many times,
// steers a correction in partway, crashes, and then resumes into a fresh agent
// with no memory of anything but the log.
func TestCorrectedRequirementSurvivesACrash(t *testing.T) {
	const (
		turns  = 400
		budget = 4000
	)

	store := newE2EStore()

	// --- the run that crashes ---------------------------------------------------

	prov := &foldingProvider{finishAt: 10 * turns} // never volunteers to finish
	work := &e2eWorkTool{size: 600}

	steered := false
	steer := func(context.Context) []agentcore.Message {
		prov.mu.Lock()
		n := prov.turns
		prov.mu.Unlock()
		if !steered && n >= 100 {
			steered = true
			return []agentcore.Message{{Role: agentcore.RoleUser, Content: pinCorrection}}
		}
		return nil
	}

	limits := agentcore.DefaultLimits()
	limits.MaxTurns = turns // the run is cut off here: no leaf, like a crash
	limits.MaxToolCalls = 10 * turns
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
			SessionID:           "pin-crash",
			GetSteeringMessages: steer,
		}},
		goal.Until(scaleGoal),
	)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if _, err := agent.Prompt(context.Background(), pinTask); err != nil {
		t.Fatalf("Prompt: %v", err)
	}

	prov.mu.Lock()
	summaries := prov.summaries
	prov.mu.Unlock()
	if !steered {
		t.Fatal("the correction was never delivered; this run proves nothing")
	}
	if summaries < 10 {
		t.Fatalf("only %d compactions before the crash: the correction was never summarized away, "+
			"so the pin is not what would be carrying it", summaries)
	}

	// --- the run that recovers --------------------------------------------------

	// A fresh agent, a fresh provider, no steering. Everything it knows comes
	// from the log.
	resumeProv := &foldingProvider{finishAt: 1}
	limits2 := agentcore.DefaultLimits()
	limits2.MaxTurns = 5
	limits2.MaxContextTokens = budget

	resumed, err := agentcore.Build(
		e2eConfig{cfg: agentcore.Config{
			Provider:      resumeProv,
			Model:         "fold-model",
			Tools:         agentcore.NewToolSet(work),
			Policy:        agentcore.NewAllowList("work"),
			Limits:        &limits2,
			Compaction:    &cs,
			Session:       store,
			SessionID:     "pin-crash",
			ResumeSession: true,
		}},
		goal.Until(scaleGoal),
	)
	if err != nil {
		t.Fatalf("Build (resume): %v", err)
	}
	if _, err := resumed.Prompt(context.Background(), "continue"); err != nil {
		t.Fatalf("resume: %v", err)
	}

	resumeProv.mu.Lock()
	window := resumeProv.lastReq
	resumeProv.mu.Unlock()

	pin, pins := "", 0
	for _, m := range window {
		if m.Role == agentcore.RoleSystem && strings.HasPrefix(m.Content, "[pinned goal") {
			pin = m.Content
			pins++
		}
	}
	if pins > 1 {
		t.Fatalf("the recovered window carries %d pinned requirements. Resume restarts from a "+
			"transcript that already begins with a pin, so a recovery that adds its own leaves the "+
			"model two statements of what the run is for — and after a second crash, three", pins)
	}
	if pin == "" {
		t.Fatalf("the recovered run has no pinned requirement at all. Resume restarts from the last "+
			"compaction's Retained transcript, so whatever the pin was holding — including a "+
			"correction the user steered in %d compactions ago — is simply gone, and the run comes "+
			"back with no statement of what it is for", summaries)
	}
	t.Logf("the recovered run is looking at:\n%s", pin)

	if !strings.Contains(pin, pinCorrection) {
		t.Fatalf("the recovered run lost the user's correction: it came back working on the "+
			"requirement that was cancelled, with nothing in the window to say otherwise:\n%s", pin)
	}
	if !strings.Contains(pin, pinTask) {
		t.Fatalf("the recovered run kept the correction but lost the original it corrects, so "+
			"\"stop auditing the ledger corpus\" is all it has to go on:\n%s", pin)
	}
	if strings.Index(pin, pinTask) > strings.Index(pin, pinCorrection) {
		t.Fatalf("the recovered pin puts the correction before what it corrects, inverting which "+
			"one supersedes:\n%s", pin)
	}
}

// pinResumeInstruction is what an operator adds when restarting a run they have
// learned something about. It is a requirement, not a resume hint.
const pinResumeInstruction = "ALSO: include contractor payroll in every department report."

// TestInstructionGivenAtResumeReachesTheModel covers the way a long run is
// actually corrected in practice.
//
// A run crashes. Someone restarts it, and while they are there they tell it the
// thing they have since worked out. That instruction arrives as the resume's
// task — and the recovered history replaces the caller's seed messages wholesale,
// so it used to be discarded before the first provider call. The run came back
// on its old objective, never did what it was asked, and nothing recorded that a
// user had been ignored. Silently dropping input is worse than refusing the
// resume, because nobody finds out.
func TestInstructionGivenAtResumeReachesTheModel(t *testing.T) {
	const budget = 4000

	store := newE2EStore()
	work := &e2eWorkTool{size: 600}
	cs := agentcore.DefaultCompactionSettings()
	cs.KeepRecentTokens = 1500

	build := func(prov *foldingProvider, turns int, resume bool) *agentcore.Agent {
		limits := agentcore.DefaultLimits()
		limits.MaxTurns = turns
		limits.MaxToolCalls = 10 * turns
		limits.MaxContextTokens = budget
		a, err := agentcore.Build(e2eConfig{cfg: agentcore.Config{
			Provider: prov, Model: "fold-model",
			Tools:         agentcore.NewToolSet(work),
			Policy:        agentcore.NewAllowList("work"),
			Limits:        &limits,
			Compaction:    &cs,
			Session:       store,
			SessionID:     "resume-instruction",
			ResumeSession: resume,
		}}, goal.Until(scaleGoal))
		if err != nil {
			t.Fatalf("Build: %v", err)
		}
		return a
	}

	first := &foldingProvider{finishAt: 100000}
	if _, err := build(first, 120, false).Prompt(context.Background(), pinTask); err != nil {
		t.Fatalf("first run: %v", err)
	}

	// Resume for a single turn, so the window examined is the FIRST request the
	// recovered run made — before any chance to pick the instruction up later.
	second := &foldingProvider{finishAt: 100000}
	if _, err := build(second, 1, true).Prompt(context.Background(), pinResumeInstruction); err != nil {
		t.Fatalf("resume: %v", err)
	}

	second.mu.Lock()
	window := second.lastReq
	second.mu.Unlock()

	found := false
	for _, m := range window {
		if strings.Contains(m.Content, pinResumeInstruction) {
			found = true
			if m.Role != agentcore.RoleUser {
				t.Fatalf("the resume instruction reached the model as a %s message, not as something "+
					"the user said", m.Role)
			}
			if !m.Directive {
				t.Fatal("the resume instruction is not marked as a directive, so the pin will not " +
					"carry it and the next compaction summarizes away a requirement the user gave " +
					"seconds earlier")
			}
		}
	}
	if !found {
		t.Fatalf("the instruction the operator gave when restarting the run never reached the model "+
			"(%d messages in the first recovered request). The run came back on its old objective "+
			"and no record anywhere says the user was ignored", len(window))
	}

	// And it must be in the log, or the next resume loses it again.
	entries, err := store.Log(context.Background(), "resume-instruction")
	if err != nil {
		t.Fatalf("Log: %v", err)
	}
	logged := false
	for _, e := range entries {
		if e.Kind == agentcore.EntryMessage && e.Message != nil &&
			strings.Contains(e.Message.Content, pinResumeInstruction) {
			logged = true
		}
	}
	if !logged {
		t.Fatal("the resume instruction is model-visible but not in the durable log: the next " +
			"recovery drops it again, and the run's record does not show it was ever given")
	}
}

// TestRestatingARequirementDoesNotMakeItTheNewestOne is the hazard that comes
// with letting a resume carry an instruction.
//
// The pin's ORDERING is its meaning: it tells the model that a later entry
// supersedes an earlier one. So a requirement the pin already holds must never
// be appended again — doing so takes the original task, which a correction
// cancelled hundreds of turns ago, and puts it back in front of the correction
// as though the user had just re-issued it. This is not a hypothetical caller:
// internal/runtime resumes a run with the transcript's last user message as the
// task, which is very often something the pin already carries.
func TestRestatingARequirementDoesNotMakeItTheNewestOne(t *testing.T) {
	const budget = 4000

	store := newE2EStore()
	work := &e2eWorkTool{size: 600}
	cs := agentcore.DefaultCompactionSettings()
	cs.KeepRecentTokens = 1500

	steered := false
	build := func(prov *foldingProvider, turns int, resume bool) *agentcore.Agent {
		limits := agentcore.DefaultLimits()
		limits.MaxTurns = turns
		limits.MaxToolCalls = 10 * turns
		limits.MaxContextTokens = budget
		steer := func(context.Context) []agentcore.Message {
			prov.mu.Lock()
			n := prov.turns
			prov.mu.Unlock()
			if !steered && !resume && n >= 60 {
				steered = true
				return []agentcore.Message{{Role: agentcore.RoleUser, Content: pinCorrection}}
			}
			return nil
		}
		a, err := agentcore.Build(e2eConfig{cfg: agentcore.Config{
			Provider: prov, Model: "fold-model",
			Tools:               agentcore.NewToolSet(work),
			Policy:              agentcore.NewAllowList("work"),
			Limits:              &limits,
			Compaction:          &cs,
			Session:             store,
			SessionID:           "restate",
			ResumeSession:       resume,
			GetSteeringMessages: steer,
		}}, goal.Until(scaleGoal))
		if err != nil {
			t.Fatalf("Build: %v", err)
		}
		return a
	}

	first := &foldingProvider{finishAt: 100000}
	if _, err := build(first, 200, false).Prompt(context.Background(), pinTask); err != nil {
		t.Fatalf("first run: %v", err)
	}
	if !steered {
		t.Fatal("the correction was never delivered; this run proves nothing")
	}

	// The restart re-states the ORIGINAL task — the requirement the correction
	// cancelled. Nothing about that is new.
	second := &foldingProvider{finishAt: 100000}
	if _, err := build(second, 200, true).Prompt(context.Background(), pinTask); err != nil {
		t.Fatalf("resume: %v", err)
	}

	second.mu.Lock()
	window := second.lastReq
	second.mu.Unlock()

	pin := ""
	for _, m := range window {
		if m.Role == agentcore.RoleSystem && strings.HasPrefix(m.Content, "[pinned goal") {
			pin = m.Content
		}
	}
	if pin == "" {
		t.Fatal("no pinned requirement in the recovered window")
	}
	if strings.Count(pin, pinTask) != 1 {
		t.Fatalf("the re-stated task appears %d times in the pin: restating a requirement was "+
			"recorded as issuing a new one:\n%s", strings.Count(pin, pinTask), pin)
	}
	if strings.Index(pin, pinTask) > strings.Index(pin, pinCorrection) {
		t.Fatalf("re-stating the original task moved it AFTER the correction that cancelled it, so "+
			"the pin now tells the model the cancelled requirement is the one that wins:\n%s", pin)
	}
}

// TestCorrectedRequirementSurvivesRepeatedCrashes is the same question asked of
// a run that has been recovered more than once.
//
// One recovery is the easy case. A run that crashes, resumes, works, crashes
// again and resumes again folds a pin that came out of a checkpoint back into a
// new checkpoint — and each cycle is a chance to duplicate it, drop the half
// that came from the log, or lose the ordering that says which requirement wins.
// A long autonomous run is exactly the thing that gets restarted repeatedly.
func TestCorrectedRequirementSurvivesRepeatedCrashes(t *testing.T) {
	const (
		budget    = 4000
		perLife   = 250
		lifetimes = 4
	)

	store := newE2EStore()
	work := &e2eWorkTool{size: 600}
	cs := agentcore.DefaultCompactionSettings()
	cs.KeepRecentTokens = 1500

	steered := false
	totalSummaries := 0
	var lastWindow []agentcore.Message

	for life := 0; life < lifetimes; life++ {
		prov := &foldingProvider{finishAt: 10 * perLife}
		steer := func(context.Context) []agentcore.Message {
			prov.mu.Lock()
			n := prov.turns
			prov.mu.Unlock()
			// The correction lands in the FIRST lifetime, so every later one is
			// carrying it purely through the checkpoint.
			if !steered && life == 0 && n >= 100 {
				steered = true
				return []agentcore.Message{{Role: agentcore.RoleUser, Content: pinCorrection}}
			}
			return nil
		}

		limits := agentcore.DefaultLimits()
		limits.MaxTurns = perLife
		limits.MaxToolCalls = 10 * perLife
		limits.MaxContextTokens = budget

		agent, err := agentcore.Build(
			e2eConfig{cfg: agentcore.Config{
				Provider:            prov,
				Model:               "fold-model",
				Tools:               agentcore.NewToolSet(work),
				Policy:              agentcore.NewAllowList("work"),
				Limits:              &limits,
				Compaction:          &cs,
				Session:             store,
				SessionID:           "pin-crash-loop",
				ResumeSession:       life > 0,
				GetSteeringMessages: steer,
			}},
			goal.Until(scaleGoal),
		)
		if err != nil {
			t.Fatalf("life %d Build: %v", life, err)
		}
		// A restart with nothing new to say passes no task, which is the shape
		// internal/runtime's resume takes (it hands back the transcript's own last
		// user message, so nothing new enters the conversation). A resume that DOES
		// carry a new instruction is TestInstructionGivenAtResumeReachesTheModel.
		task := pinTask
		if life > 0 {
			task = ""
		}
		if _, err := agent.Prompt(context.Background(), task); err != nil {
			t.Fatalf("life %d: %v", life, err)
		}

		prov.mu.Lock()
		totalSummaries += prov.summaries
		lastWindow = prov.lastReq
		prov.mu.Unlock()
	}

	if !steered {
		t.Fatal("the correction was never delivered; this run proves nothing")
	}

	pin, pins := "", 0
	for _, m := range lastWindow {
		if m.Role == agentcore.RoleSystem && strings.HasPrefix(m.Content, "[pinned goal") {
			pin = m.Content
			pins++
		}
	}
	t.Logf("%d lifetimes, %d compactions total; %d pin(s) in the final window:\n%s", lifetimes, totalSummaries, pins, pin)
	if pins != 1 {
		t.Fatalf("after %d recoveries the window holds %d pinned requirements, not one: each cycle "+
			"folds a pin that came out of a checkpoint into a new checkpoint, so a duplicate "+
			"compounds with every restart:\n%s", lifetimes-1, pins, pin)
	}
	for _, want := range []string{pinTask, pinCorrection} {
		if !strings.Contains(pin, want) {
			t.Fatalf("after %d recoveries the pin has lost %q — the objective decayed across "+
				"restarts rather than across compactions:\n%s", lifetimes-1, want, pin)
		}
	}
	if strings.Index(pin, pinTask) > strings.Index(pin, pinCorrection) {
		t.Fatalf("after %d recoveries the pin's ordering inverted, so the cancelled requirement "+
			"now reads as the one that supersedes:\n%s", lifetimes-1, pin)
	}
}

// What the run learned early, against what it can still answer with at the end.
//
// Every other long-run test in this package asks whether the machinery survives:
// the window stays bounded, the pin persists, the plan comes back, the tokens
// add up. None of them asks the question the run exists to answer — after a
// hundred compactions, is the final answer still built on what the run actually
// found?
//
// A run that discovers 24 facts in its first 30 turns and then works for another
// 300 has compacted its discovery phase away many times over. Each compaction
// folds the previous checkpoint forward, so a fact survives only if every fold
// between then and the end carried it. One drop is permanent — nothing later
// re-reads the transcript. The failure is silent and it is the worst kind: the
// run finishes, reports success, and answers with a subset of what it knows.

// findingsTotal is how many facts the run discovers before the long middle
// stretch that compacts them away.
const findingsTotal = 24

var findingRe = regexp.MustCompile(`FINDING-(\d+)=([A-Z0-9]+)`)

func findingCode(k int) string { return fmt.Sprintf("FINDING-%d=SHARD%03dOK", k, k) }

// findingsIn returns the distinct finding numbers readable anywhere in a span,
// which is exactly the set the model could answer from.
func findingsIn(msgs []agentcore.Message) map[string]string {
	found := map[string]string{}
	for _, m := range msgs {
		for _, mt := range findingRe.FindAllStringSubmatch(m.Content, -1) {
			found[mt[1]] = mt[2]
		}
		for _, tc := range m.ToolCalls {
			for _, mt := range findingRe.FindAllStringSubmatch(tc.Arguments, -1) {
				found[mt[1]] = mt[2]
			}
		}
	}
	return found
}

// recallLookupTool is the discovery half of the run: each call returns a bulky
// result with one finding in it, the way a real query or file read does.
type recallLookupTool struct{ pad int }

func (*recallLookupTool) Name() string { return "lookup" }
func (*recallLookupTool) Schema() agentcore.ToolSchema {
	return agentcore.ToolSchema{
		Name:        "lookup",
		Description: "Inspect one shard and report what it holds.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"shard": map[string]any{"type": "integer"},
			},
			"required": []string{"shard"},
		},
	}
}

func (t *recallLookupTool) Run(_ context.Context, args string) (string, error) {
	var in struct {
		Shard int `json:"shard"`
	}
	_ = json.Unmarshal([]byte(args), &in)
	// The finding leads, because a summarizer reading a bounded serialization of
	// this result sees its head. Burying it would test truncation, not recall.
	return findingCode(in.Shard) + "\n" + strings.Repeat("shard detail. ", t.pad), nil
}

// recallProvider plays three roles, and the honest one is the summarizer.
//
// It is written as a COMPETENT model, not a generous one: on the update path it
// carries forward every finding stated in the previous checkpoint plus every
// finding in the new span, and it writes them out in full. That is the best a
// real model could do with what it is handed. Anything the run loses under this
// provider is lost by the machinery — the span it chose to summarize, the
// checkpoint ceiling, the fold — and not by a model that forgot.
type recallProvider struct {
	mu sync.Mutex

	workTurns int

	turns     int
	summaries int

	// answered is what the run could still see when it wrote its final answer.
	answered map[string]string
	// widestSummary is the largest checkpoint the run ever produced, in bytes.
	widestSummary int
}

func (*recallProvider) Name() string        { return "recall" }
func (*recallProvider) SupportsTools() bool { return true }

func (p *recallProvider) Chat(_ context.Context, req agentcore.ChatRequest) (agentcore.ChatResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if len(req.Messages) > 0 && strings.HasPrefix(req.Messages[0].Content, "You are a context summarization") {
		p.summaries++
		summary := p.checkpoint(req.Messages)
		if len(summary) > p.widestSummary {
			p.widestSummary = len(summary)
		}
		return usageFor(req, agentcore.AssistantText(summary)), nil
	}

	p.turns++
	n := p.turns
	switch {
	case n <= findingsTotal:
		return usageFor(req, agentcore.AssistantToolCall(
			fmt.Sprintf("lk%d", n), "lookup", fmt.Sprintf(`{"shard":%d}`, n))), nil
	case n >= p.workTurns:
		// The answer is written from context and nothing else — the same
		// constraint the real model is under.
		p.answered = findingsIn(req.Messages)
		keys := make([]string, 0, len(p.answered))
		for k, v := range p.answered {
			keys = append(keys, "FINDING-"+k+"="+v)
		}
		sort.Strings(keys)
		return usageFor(req, agentcore.AssistantText(
			"Audit complete. "+strings.Join(keys, " ")+"\n"+goal.Done)), nil
	default:
		return usageFor(req, agentcore.AssistantToolCall(
			fmt.Sprintf("w%d", n), "work", fmt.Sprintf(`{"n":%d}`, n))), nil
	}
}

// checkpoint writes the structured summary a competent model would write: the
// findings it can see, all of them, in the section the format reserves for
// completed work.
func (p *recallProvider) checkpoint(msgs []agentcore.Message) string {
	found := findingsIn(msgs)
	nums := make([]int, 0, len(found))
	for k := range found {
		var n int
		fmt.Sscanf(k, "%d", &n)
		nums = append(nums, n)
	}
	sort.Ints(nums)

	var b strings.Builder
	b.WriteString("## Goal\nAudit every shard and report each shard's code.\n")
	b.WriteString("## Progress\n### Done\n")
	for _, n := range nums {
		// Write back the code as READ, never as reconstructed from the shard
		// number. A summarizer that re-derives the fact would silently repair a
		// checkpoint the ceiling had damaged, and a real model cannot do that —
		// it only has what the previous checkpoint told it.
		fmt.Fprintf(&b, "- [x] shard %d inspected, code FINDING-%d=%s\n", n, n, found[fmt.Sprint(n)])
	}
	b.WriteString("## Next Steps\n1. Inspect the remaining shards\n2. Report every code\n")
	return b.String()
}

func (p *recallProvider) Stream(ctx context.Context, req agentcore.ChatRequest) (<-chan agentcore.ChatDelta, error) {
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

// runRecall drives the audit run and returns its final answer alongside the
// provider's record of what the run could still see when it answered.
// maxSummaryTokens of 0 leaves the checkpoint ceiling at its default.
func runRecall(t *testing.T, maxSummaryTokens int) (agentcore.RunResult, *recallProvider) {
	t.Helper()
	const workTurns = 320

	prov := &recallProvider{workTurns: workTurns}

	limits := agentcore.DefaultLimits()
	limits.MaxTurns = 4 * workTurns
	limits.MaxToolCalls = 4 * workTurns
	limits.MaxContextTokens = 4000

	cs := agentcore.DefaultCompactionSettings()
	cs.KeepRecentTokens = 1500
	if maxSummaryTokens > 0 {
		cs.MaxSummaryTokens = maxSummaryTokens
	}

	agent, err := agentcore.Build(
		e2eConfig{cfg: agentcore.Config{
			Provider:   prov,
			Model:      "recall-model",
			Tools:      agentcore.NewToolSet(&recallLookupTool{pad: 60}, &e2eWorkTool{size: 600}),
			Policy:     agentcore.NewAllowList("lookup", "work", todo.ToolName),
			Limits:     &limits,
			Compaction: &cs,
			Session:    newE2EStore(),
			SessionID:  "recall-audit",
		}},
		goal.Until("every shard's code reported"),
		todo.With(todo.NewStore()),
	)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	res, err := agent.Prompt(context.Background(),
		"Inspect every shard and report each shard's code in your final answer.")
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	return res, prov
}

// TestTheAnswerStillKnowsWhatTheRunFoundEarly is the recall audit.
func TestTheAnswerStillKnowsWhatTheRunFoundEarly(t *testing.T) {
	res, prov := runRecall(t, 0)

	prov.mu.Lock()
	answered, summaries, widest := prov.answered, prov.summaries, prov.widestSummary
	prov.mu.Unlock()

	// A recall test that never compacted proves nothing.
	if summaries < 10 {
		t.Fatalf("only %d compactions: the findings never had to survive one", summaries)
	}

	var lost []int
	for k := 1; k <= findingsTotal; k++ {
		if _, ok := answered[fmt.Sprint(k)]; !ok {
			lost = append(lost, k)
		}
	}
	t.Logf("%d compactions, widest checkpoint %d B; %d of %d findings reached the answer",
		summaries, widest, findingsTotal-len(lost), findingsTotal)

	if len(lost) > 0 {
		t.Fatalf("%d of %d findings did not survive to the final answer (%v). Every fold between "+
			"discovery and the end was handed the previous checkpoint and asked to carry it "+
			"forward, and the provider here does carry it — so a finding that is gone was "+
			"dropped by the machinery, not forgotten by a model. The run still reports success: "+
			"it answers with a subset of what it found and nothing anywhere says so. "+
			"%d compactions, widest checkpoint %d B",
			len(lost), findingsTotal, lost, summaries, widest)
	}

	// The answer has to actually contain them, not merely have had them in view.
	for k := 1; k <= findingsTotal; k++ {
		if !strings.Contains(res.Final, findingCode(k)) {
			t.Fatalf("finding %d was in context but missing from the final answer", k)
		}
	}
}

// The same run with a checkpoint ceiling too small to hold every finding.
//
// Losing facts here is correct and expected — a budget is a budget, and the run
// can look a shard up again. What is NOT acceptable is answering with a fact
// that is wrong: the checkpoint is fed to the next fold as the previous summary
// and carried forward, so a code mangled by the ceiling becomes something the
// run believes and reports for the rest of its life, with nothing marking it
// damaged. Under pressure the run must forget, not confabulate.
func TestUnderCheckpointPressureTheRunForgetsRatherThanConfabulates(t *testing.T) {
	res, prov := runRecall(t, 120) // 480 bytes: room for a handful of findings

	prov.mu.Lock()
	answered, summaries := prov.answered, prov.summaries
	prov.mu.Unlock()

	if summaries < 10 {
		t.Fatalf("only %d compactions: the ceiling never had to bite", summaries)
	}
	if len(answered) == findingsTotal {
		t.Fatalf("all %d findings fit under a 480-byte checkpoint — the ceiling did not bite, "+
			"so this test proved nothing about what happens when it does", findingsTotal)
	}

	for k, code := range answered {
		var n int
		if _, err := fmt.Sscanf(k, "%d", &n); err != nil {
			t.Fatalf("the run answered with an unparseable finding number %q", k)
		}
		if want := strings.TrimPrefix(findingCode(n), fmt.Sprintf("FINDING-%d=", n)); code != want {
			t.Fatalf("the run answered finding %d as %q, but its real code is %q. The ceiling cut "+
				"through the fact instead of dropping it, and a half-code reads as a whole one all "+
				"the way to the final answer.\n---\n%s", n, code, want, res.Final)
		}
	}
	t.Logf("%d compactions under a 480-byte checkpoint: %d of %d findings kept, every one of them intact",
		summaries, len(answered), findingsTotal)
}

// What the run says it spent, against what it actually spent.
//
// A long run makes three kinds of provider call and only one of them is the
// obvious one: its own turns, the summarization call every compaction makes, and
// a whole child run per delegation. All three are billed. If RunResult.Usage
// omits any of them the run under-reports, and the under-report is not cosmetic
// — the budget gate is handed that same number, so a run whose real spend is
// several times its reported spend runs past a ceiling that was supposed to stop
// it. "Minimal LLM token usage" is not a property you can pursue on a number
// that is wrong.
//
// The provider is the single point every one of those calls goes through, so it
// is the honest place to count from.

const auditChildMarker = "AUDIT-SUBTASK"

// auditProvider is the ground truth: every call the run makes, of every kind,
// passes through here and is added up.
type auditProvider struct {
	mu sync.Mutex

	workTurns int

	parentTurns int
	childCalls  int
	summaries   int
	spawns      int

	// spent is the sum of the Usage handed back on every call — parent turns,
	// child turns and summarizations alike.
	spent agentcore.Usage

	// byKind splits the same total by which kind of call spent it, so a failure
	// names the leak ("the whole child column is missing") instead of only its
	// size.
	byKind map[string]int
}

func (*auditProvider) Name() string        { return "audit" }
func (*auditProvider) SupportsTools() bool { return true }

func (p *auditProvider) Chat(_ context.Context, req agentcore.ChatRequest) (agentcore.ChatResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	var resp agentcore.ChatResponse
	kind := "parent"
	switch {
	case len(req.Messages) > 0 && strings.HasPrefix(req.Messages[0].Content, "You are a context summarization"):
		p.summaries++
		kind = "summary"
		resp = usageFor(req, agentcore.AssistantText(
			"## Goal\nAudit\n## Progress\n### Done\n- [x] a batch\n## Next Steps\n1. keep going"))

	case isAuditChild(req.Messages):
		p.childCalls++
		kind = "child"
		if hasToolResult(req.Messages) {
			resp = usageFor(req, agentcore.AssistantText("shard reconciled\n"+goal.Done))
		} else {
			resp = usageFor(req, agentcore.AssistantToolCall(
				fmt.Sprintf("cw%d", p.childCalls), "work", fmt.Sprintf(`{"shard":%d}`, p.childCalls)))
		}

	default:
		p.parentTurns++
		n := p.parentTurns
		switch {
		case n >= p.workTurns:
			resp = usageFor(req, agentcore.AssistantText("all shards audited\n"+goal.Done))
		case n%9 == 0 && p.spawns < 12:
			p.spawns++
			resp = usageFor(req, agentcore.AssistantToolCall(
				fmt.Sprintf("sp%d", p.spawns), subagent.ToolSpawnSubagent,
				fmt.Sprintf(`{"task":%q}`, fmt.Sprintf("%s: reconcile shard %d", auditChildMarker, p.spawns))))
		default:
			resp = usageFor(req, agentcore.AssistantToolCall(
				fmt.Sprintf("w%d", n), "work", fmt.Sprintf(`{"n":%d}`, n)))
		}
	}

	p.spent.InputTokens += resp.Usage.InputTokens
	p.spent.OutputTokens += resp.Usage.OutputTokens
	if p.byKind == nil {
		p.byKind = map[string]int{}
	}
	p.byKind[kind] += resp.Usage.InputTokens + resp.Usage.OutputTokens
	return resp, nil
}

func (p *auditProvider) Stream(ctx context.Context, req agentcore.ChatRequest) (<-chan agentcore.ChatDelta, error) {
	resp, err := p.Chat(ctx, req)
	if err != nil {
		return nil, err
	}
	ch := make(chan agentcore.ChatDelta, 4)
	go func() {
		defer close(ch)
		// Report usage the way the wire formats do: input tokens are known
		// before a single output token exists, so they go out first, and the
		// final delta restates the running totals. A provider that only
		// reported on Done would be the easier case and would not exercise the
		// merge.
		ch <- agentcore.ChatDelta{Usage: agentcore.Usage{InputTokens: resp.Usage.InputTokens}}
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

func isAuditChild(msgs []agentcore.Message) bool {
	for _, m := range msgs {
		if m.Role == agentcore.RoleUser && strings.Contains(m.Content, auditChildMarker) {
			return true
		}
	}
	return false
}

// TestRunAccountsForEveryProviderCallItMakes is the audit. Every kind of call
// has to be in the total, and the three kinds are counted separately so a
// failure names which one leaked.
func TestRunAccountsForEveryProviderCallItMakes(t *testing.T) {
	const workTurns = 220

	prov := &auditProvider{workTurns: workTurns}
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
			Model:      "audit-model",
			Tools:      agentcore.NewToolSet(work),
			Policy:     agentcore.NewAllowList("work", todo.ToolName, subagent.ToolSpawnSubagent),
			Limits:     &limits,
			Compaction: &cs,
			Session:    store,
			SessionID:  "usage-audit",
		}},
		goal.Until("every shard audited"),
		todo.With(plan),
		subagent.SelfOnly(),
	)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	res, err := agent.Prompt(context.Background(), "Audit every shard in the corpus.")
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}

	prov.mu.Lock()
	spent := prov.spent
	byKind := prov.byKind
	parentTurns, childCalls, summaries, spawns := prov.parentTurns, prov.childCalls, prov.summaries, prov.spawns
	prov.mu.Unlock()

	// The run has to have actually done all three things, or the audit is
	// vacuous — a test that reports perfect accounting of a run that never
	// delegated and never compacted proves nothing about either.
	if summaries < 5 {
		t.Fatalf("only %d compactions: the run never exercised summarization spend", summaries)
	}
	if spawns < 5 || childCalls < 2*spawns {
		t.Fatalf("only %d children (%d child calls): the run never exercised delegated spend",
			spawns, childCalls)
	}

	got := res.Usage.InputTokens + res.Usage.OutputTokens
	want := spent.InputTokens + spent.OutputTokens
	t.Logf("provider saw %d calls (%d parent turns, %d child calls, %d summarizations) = %d tokens "+
		"%v; the run reported %d",
		parentTurns+childCalls+summaries, parentTurns, childCalls, summaries, want, byKind, got)

	if got != want {
		missing := want - got
		t.Fatalf("the run reported %d tokens against %d actually spent — %d unaccounted (%.1f%%). "+
			"The budget gate is handed the reported number, so a run whose real spend is larger "+
			"than what it admits to runs past the ceiling that was meant to stop it. Spend by kind "+
			"of call was %v across %d parent turns, %d child calls and %d summarizations — compare "+
			"the missing amount against those columns to see which one leaked",
			got, want, missing, float64(missing)*100/float64(want),
			byKind, parentTurns, childCalls, summaries)
	}
}

// What a long run actually re-bills.
//
// Prompt caching is the single biggest lever on the token cost of a long run:
// the transcript-so-far is re-sent on every turn, so if the provider can read it
// from cache instead of re-ingesting it, the run pays for the new tail only. The
// loop opts in with Config.PromptCacheKey and places a breakpoint with
// markCacheAnchors.
//
// A cache only pays if the PREFIX is stable. Two things have to hold, and
// neither is checked anywhere: the request's leading messages must be identical
// from turn to turn, and the breakpoint has to sit on a message that is itself
// stable — anchoring on a message the loop rewrites every turn caches a prefix
// that is guaranteed to miss.

// cacheProbeProvider records the exact request view of every parent turn — after
// context hooks and after anchor placement, which is what the provider is
// actually billed on.
type cacheProbeProvider struct {
	mu        sync.Mutex
	turns     int
	summaries int
	finishAt  int
	requests  [][]agentcore.Message
}

func (*cacheProbeProvider) Name() string        { return "cache-probe" }
func (*cacheProbeProvider) SupportsTools() bool { return true }

func (p *cacheProbeProvider) Chat(_ context.Context, req agentcore.ChatRequest) (agentcore.ChatResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if len(req.Messages) > 0 && strings.HasPrefix(req.Messages[0].Content, "You are a context summarization") {
		p.summaries++
		return usageFor(req, agentcore.AssistantText(
			"## Goal\nAudit the corpus\n## Progress\n### Done\n- [x] a batch\n## Next Steps\n1. keep going")), nil
	}

	p.turns++
	n := p.turns
	// Snapshot: the loop reuses its backing array between turns.
	p.requests = append(p.requests, append([]agentcore.Message(nil), req.Messages...))

	switch {
	case n >= p.finishAt:
		return usageFor(req, agentcore.AssistantText("done\n"+goal.Done)), nil
	case n%7 == 0:
		return usageFor(req, agentcore.AssistantToolCall(
			fmt.Sprintf("p%d", n), todo.ToolName, cachePlanArgs(n))), nil
	default:
		return usageFor(req, agentcore.AssistantToolCall(
			fmt.Sprintf("w%d", n), "work", fmt.Sprintf(`{"n":%d}`, n))), nil
	}
}

func (p *cacheProbeProvider) Stream(ctx context.Context, req agentcore.ChatRequest) (<-chan agentcore.ChatDelta, error) {
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

func cachePlanArgs(n int) string {
	return fmt.Sprintf(`{"items":[{"content":"audit batch %d","status":"in_progress"},`+
		`{"content":"file the reports","status":"pending"}]}`, n/7)
}

// sameMessage compares two request messages the way a provider's cache does:
// on what is sent, not on Go identity.
func sameMessage(a, b agentcore.Message) bool {
	if a.Role != b.Role || a.Content != b.Content || a.Name != b.Name || a.ToolCallID != b.ToolCallID {
		return false
	}
	if len(a.ToolCalls) != len(b.ToolCalls) {
		return false
	}
	for i := range a.ToolCalls {
		if a.ToolCalls[i] != b.ToolCalls[i] {
			return false
		}
	}
	return true
}

// commonPrefix returns how many leading messages two requests share.
func commonPrefix(a, b []agentcore.Message) int {
	n := 0
	for n < len(a) && n < len(b) && sameMessage(a[n], b[n]) {
		n++
	}
	return n
}

// TestTheCachedPrefixIsStableAcrossTurns is the round's measurement. A run that
// re-bills its whole window every turn costs several times what it should, and
// nothing else in the suite would notice.
func TestTheCachedPrefixIsStableAcrossTurns(t *testing.T) {
	const turns = 300

	prov := &cacheProbeProvider{finishAt: turns}
	store := newE2EStore()
	plan := todo.NewStore()
	work := &e2eWorkTool{size: 600}

	limits := agentcore.DefaultLimits()
	limits.MaxTurns = 4 * turns
	limits.MaxToolCalls = 4 * turns
	limits.MaxContextTokens = 4000

	cs := agentcore.DefaultCompactionSettings()
	cs.KeepRecentTokens = 1500

	agent, err := agentcore.Build(
		e2eConfig{cfg: agentcore.Config{
			Provider:        prov,
			Model:           "cache-model",
			Tools:           agentcore.NewToolSet(work),
			Policy:          agentcore.NewAllowList("work", todo.ToolName),
			Limits:          &limits,
			Compaction:      &cs,
			Session:         store,
			SessionID:       "cache-stability",
			PromptCacheKey:  "agent-scope-1",
			ReasoningEffort: "",
		}},
		goal.Until("the corpus is audited"),
		todo.With(plan),
	)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if _, err := agent.Prompt(context.Background(), "Audit every shard in the corpus."); err != nil {
		t.Fatalf("Prompt: %v", err)
	}

	prov.mu.Lock()
	reqs := prov.requests
	summaries := prov.summaries
	prov.mu.Unlock()

	if len(reqs) < 50 {
		t.Fatalf("only %d turns recorded; not a long run", len(reqs))
	}

	// The question a cache actually asks. A breakpoint tells the provider to
	// store the prefix up to and including that message. That entry is worth
	// something only if it is still a prefix of the NEXT request — otherwise the
	// run pays the cache-write premium every turn and never reads one back.
	//
	// This is the measurement that matters, and it is not the same as "the
	// requests share a long prefix": they can share almost everything and still
	// miss, if the anchor is placed past the point where they diverge.
	readable, written := 0, 0
	for i := 0; i < len(reqs)-1; i++ {
		cur, next := reqs[i], reqs[i+1]
		idx := -1
		for j := range cur {
			if cur[j].CacheAnchor {
				idx = j
			}
		}
		if idx < 0 {
			t.Fatalf("turn %d carries no cache anchor although PromptCacheKey is set: the run opted "+
				"into caching and then never told the provider where the stable prefix ends", i+1)
		}
		written++
		if commonPrefix(cur[:idx+1], next) == idx+1 {
			readable++
		}
	}
	t.Logf("cache entries written %d, still a prefix of the next request %d", written, readable)

	// The headline: how much of each request could actually be served from the
	// previous turn's cache.
	totalBytes, cachedBytes, resets := 0, 0, 0
	for i := 1; i < len(reqs); i++ {
		prev, cur := reqs[i-1], reqs[i]
		n := commonPrefix(prev, cur)
		total := transcriptBytes(cur)
		shared := transcriptBytes(cur[:n])
		totalBytes += total
		cachedBytes += shared
		// A reset is a turn that shares almost nothing with the one before it —
		// compaction rewrites the window, and that is expected. What matters is
		// that it is RARE.
		if total > 0 && shared*4 < total {
			resets++
		}
	}
	pct := cachedBytes * 100 / max(totalBytes, 1)
	t.Logf("%d turns, %d compactions: %d%% of request bytes were a prefix of the previous request "+
		"(%d full resets)", len(reqs), summaries, pct, resets)

	// Compactions legitimately rewrite the window, so a handful of unreadable
	// entries is expected. Anything beyond that means the anchor is systematically
	// misplaced.
	if readable < written-2*summaries-1 {
		t.Fatalf("only %d of %d cache entries were still a prefix of the next request (%d "+
			"compactions explain at most %d). The anchor goes on the FINAL message of the request, "+
			"but context hooks append a regenerated reminder there — it is not part of the "+
			"append-only history, so the cached prefix ends inside content guaranteed to differ "+
			"next turn. The run pays the cache-WRITE premium every turn and never reads one back",
			readable, written, summaries, 2*summaries)
	}
	if pct < 60 {
		t.Fatalf("only %d%% of each request is a prefix of the one before it, so a long run re-bills "+
			"most of its window every turn (%d resets over %d turns)", pct, resets, len(reqs))
	}
}
