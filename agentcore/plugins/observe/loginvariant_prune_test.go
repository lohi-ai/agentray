package observe

import (
	"context"
	"fmt"
	"testing"

	"github.com/lohi-ai/agentray/agentcore"
)

// The reconstruction check re-runs the real fold before every provider call. On
// a run of a few dozen turns that is free; on a run of a few thousand it was
// quadratic, because the entry list was the whole run and the fold walked all of
// it every time.
//
// A checkpoint bounds it: the fold restarts at a completed compaction's retained
// transcript, so the entries before the newest one cannot change the answer.
// These tests hold both halves of that claim — that the list actually stays
// bounded, and that bounding it did not change what the check reports. The
// second is the one that matters: a cheap check that stopped catching things is
// worse than an expensive one.

// checkpoint is the completion half of a compaction bracket, carrying the
// transcript the fold restarts from.
func checkpoint(retained ...agentcore.Message) agentcore.SessionEntry {
	return agentcore.SessionEntry{Kind: agentcore.EntryCompaction, Final: true, Retained: retained}
}

// TestPrune_EntryListStaysBoundedAcrossAThousandTurns is the cost claim. The
// assertion is on the entry list rather than on elapsed time, because a timing
// threshold is a flake waiting for a loaded CI box — and the list length is what
// the fold's cost is proportional to.
func TestPrune_EntryListStaysBoundedAcrossAThousandTurns(t *testing.T) {
	li := newTracker(func(LogInvariantViolation) {})

	for turn := 1; turn <= 1000; turn++ {
		li.ObserveLogged(logEntry(agentcore.RoleAssistant, fmt.Sprintf("work %d", turn)))
		li.ObserveLogged(logEntry(agentcore.RoleTool, fmt.Sprintf("result %d", turn)))
		if turn%25 == 0 {
			li.ObserveLogged(agentcore.SessionEntry{Kind: agentcore.EntryCompaction})
			li.ObserveLogged(checkpoint(agentcore.Message{
				Role: agentcore.RoleSystem, Content: fmt.Sprintf("[checkpoint] through turn %d", turn),
			}))
		}
	}

	// 1,000 turns wrote ~2,040 entries. What is retained is the newest checkpoint
	// plus the turns after it, which is the window the fold actually reads.
	if got := len(li.entries); got > 60 {
		t.Fatalf("entry list holds %d entries after 1,000 turns — the check is still O(run)", got)
	}
	if len(li.entries) == 0 {
		t.Fatal("pruning must keep the checkpoint and the work after it, not everything")
	}
	if e := li.entries[0]; e.Kind != agentcore.EntryCompaction || !e.Final {
		t.Fatalf("the pruned list must begin at a checkpoint, begins at %s", e.Kind)
	}
}

// TestPrune_StillRebuildsTheLiveHistory is the correctness claim, stated the way
// the check itself states it: the pruned entries must fold to exactly the
// conversation the model is shown.
func TestPrune_StillRebuildsTheLiveHistory(t *testing.T) {
	var violations []LogInvariantViolation
	li := newTracker(func(v LogInvariantViolation) { violations = append(violations, v) })

	ctx := context.Background()
	li.ObserveLogged(logEntry(agentcore.RoleUser, "task"))
	li.ObserveLogged(logEntry(agentcore.RoleAssistant, "early work nobody will see again"))
	li.ObserveLogged(agentcore.SessionEntry{Kind: agentcore.EntryCompaction})
	li.ObserveLogged(checkpoint(agentcore.Message{Role: agentcore.RoleSystem, Content: "summary of the early work"}))
	// The loop announces the rewrite it just bracketed. Skipping it here would
	// leave the summary uncounted and fire the MEMBERSHIP check instead, which
	// would say nothing about pruning.
	li.ObserveMessages(ctx, agentcore.PhaseRebase, 1, []agentcore.Message{
		{Role: agentcore.RoleSystem, Content: "you are an agent"},
		{Role: agentcore.RoleSystem, Content: "summary of the early work"},
	})
	li.ObserveLogged(logEntry(agentcore.RoleAssistant, "later work"))

	// The history the loop is actually holding after that compaction: the summary
	// plus what came after, with the derived system prompt in front.
	live := []agentcore.Message{
		{Role: agentcore.RoleSystem, Content: "you are an agent"},
		{Role: agentcore.RoleSystem, Content: "summary of the early work"},
		{Role: agentcore.RoleAssistant, Content: "later work"},
	}
	li.ObserveMessages(ctx, agentcore.PhaseRequest, 2, live)
	if len(violations) != 0 {
		t.Fatalf("a correct run must stay quiet after pruning: %+v", violations)
	}
}

// TestPrune_StillCatchesADivergenceAfterTheCheckpoint is the test that would
// fail if pruning had quietly turned the check off. The fault is introduced
// AFTER the checkpoint, which is the only region a pruned list still covers —
// so if this passes for the wrong reason, nothing else here would notice.
func TestPrune_StillCatchesADivergenceAfterTheCheckpoint(t *testing.T) {
	var violations []LogInvariantViolation
	li := newTracker(func(v LogInvariantViolation) { violations = append(violations, v) })

	li.ObserveLogged(logEntry(agentcore.RoleUser, "task"))
	li.ObserveLogged(agentcore.SessionEntry{Kind: agentcore.EntryCompaction})
	li.ObserveLogged(checkpoint(agentcore.Message{Role: agentcore.RoleSystem, Content: "summary"}))
	li.ObserveLogged(logEntry(agentcore.RoleAssistant, "logged work"))

	// The live history carries a message that never reached the log — the exact
	// divergence a resumed run would continue without.
	live := []agentcore.Message{
		{Role: agentcore.RoleSystem, Content: "you are an agent"},
		{Role: agentcore.RoleSystem, Content: "summary"},
		{Role: agentcore.RoleAssistant, Content: "logged work"},
		{Role: agentcore.RoleUser, Content: "an injection nobody logged"},
	}
	li.ObserveMessages(context.Background(), agentcore.PhaseRequest, 2, live)
	if len(violations) == 0 {
		t.Fatal("pruning must not turn the check off for the window it still covers")
	}
}

// TestPrune_StopsOnceTheLogBranches is the safety valve. The fold walks the
// ACTIVE branch, so after a leaf move the entries before a checkpoint may be
// reachable again and dropping them would change the answer. A branched run
// keeps its whole list and pays the old cost — the right trade, since a branched
// run is a human inspecting a session, not a 4,000-turn autonomous one.
func TestPrune_StopsOnceTheLogBranches(t *testing.T) {
	li := newTracker(func(LogInvariantViolation) {})

	li.ObserveLogged(logEntry(agentcore.RoleUser, "task"))
	li.ObserveLogged(agentcore.SessionEntry{Kind: agentcore.EntryLeafMove, Target: "#0"})
	for i := range 10 {
		li.ObserveLogged(logEntry(agentcore.RoleAssistant, fmt.Sprintf("work %d", i)))
	}
	li.ObserveLogged(checkpoint(agentcore.Message{Role: agentcore.RoleSystem, Content: "summary"}))

	if got := len(li.entries); got != 13 {
		t.Fatalf("a branched log must keep every entry, kept %d of 13", got)
	}
}
