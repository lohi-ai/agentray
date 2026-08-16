package agentcore_test

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

import (
	"context"
	"strings"
	"testing"

	"github.com/lohi-ai/agentray/agentcore"
	"github.com/lohi-ai/agentray/agentcore/plugins/goal"
)

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
