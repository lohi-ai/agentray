package agentruntime

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/lohi-ai/agentray/agentcore"
)

func TestPiGoalRecoveryRequiresOrderedExplicitEffectCommits(t *testing.T) {
	entry := func(kind agentcore.SessionEntryKind, value any) agentcore.SessionEntry {
		raw, _ := json.Marshal(value)
		return agentcore.SessionEntry{Kind: kind, CallID: "reused", Content: string(raw)}
	}
	initial := entry(agentcore.EntryPiGoal, "original")
	start := entry(agentcore.EntryPiEffectStart, map[string]string{"effectId": "physical-1"})
	commit := entry(agentcore.EntryPiGoalRevision, piGoalRevision{GoalRevision: agentcore.GoalRevision{Previous: "original", Goal: "next", Reason: "finding"}, EffectID: "physical-1", SourceCallID: "reused/bridge-1"})
	done := entry(agentcore.EntryPiEffectDone, map[string]string{"effectId": "physical-1"})
	second := entry(agentcore.EntryPiGoalRevision, piGoalRevision{GoalRevision: agentcore.GoalRevision{Previous: "next", Goal: "final", Reason: "second finding"}, EffectID: "physical-2", SourceCallID: "reused"})
	valid := []agentcore.SessionEntry{initial, start, commit, done, entry(agentcore.EntryPiEffectStart, map[string]string{"effectId": "physical-2"}), second, entry(agentcore.EntryPiEffectDone, map[string]string{"effectId": "physical-2"})}
	if got, found, err := piStoredGoal(valid); err != nil || !found || got != "final" {
		t.Fatalf("lost ordered revisions with reused call IDs: %q %v %v", got, found, err)
	}
	for name, entries := range map[string][]agentcore.SessionEntry{
		"no initial":           {start, commit},
		"no intent":            {initial, commit},
		"late commit":          {initial, start, done, commit},
		"duplicate commit":     {initial, start, commit, commit},
		"unmarked replacement": {initial, entry(agentcore.EntryPiGoal, "next")},
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := piStoredGoal(entries); err == nil {
				t.Fatal("accepted invalid revision history")
			}
		})
	}
	for _, field := range []string{"previous", "goal", "reason", "effectId", "sourceCallId"} {
		t.Run("missing "+field, func(t *testing.T) {
			var raw map[string]any
			_ = json.Unmarshal([]byte(commit.Content), &raw)
			delete(raw, field)
			if _, _, err := piStoredGoal([]agentcore.SessionEntry{initial, start, entry(agentcore.EntryPiGoalRevision, raw)}); err == nil {
				t.Fatal("accepted incomplete revision")
			}
		})
	}
	// A committed condition does not manufacture the tool result or erase the
	// unsettled physical effect after a crash.
	crashed := append([]agentcore.SessionEntry{entry(agentcore.EntryPiState, map[string]any{"messages": []any{}})}, initial, start, commit)
	if _, err := recoverPiState(crashed); err == nil || !strings.Contains(err.Error(), "unsettled") {
		t.Fatalf("revision made crashed effect replayable: %v", err)
	}
}
