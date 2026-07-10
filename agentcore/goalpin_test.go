package agentcore

import (
	"context"
	"strings"
	"testing"
)

// goalTranscript builds a long transcript whose FIRST user message is a
// distinctive goal, followed by enough bulky turns that an older span exists to
// compact.
func goalTranscript(goal string) []Message {
	msgs := []Message{
		{Role: RoleSystem, Content: "you are an agent"},
		{Role: RoleUser, Content: goal},
	}
	for i := 0; i < 8; i++ {
		msgs = append(msgs,
			Message{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "c", Name: "q", Arguments: "{}"}}},
			Message{Role: RoleTool, Name: "q", Content: "result " + bigText(2000)},
			Message{Role: RoleAssistant, Content: "answer " + bigText(500)},
			Message{Role: RoleUser, Content: "follow up " + bigText(2000)},
		)
	}
	return msgs
}

// findGoalPin returns the content after the goal marker, if a pin exists.
func findGoalPin(msgs []Message) (string, bool) {
	for _, m := range msgs {
		if m.Role == RoleSystem && strings.HasPrefix(m.Content, goalMarker) {
			return strings.TrimSpace(strings.TrimPrefix(m.Content, goalMarker)), true
		}
	}
	return "", false
}

// TestGoalPinnedOnFirstCompaction verifies the original task is lifted into a
// goal-pinned system message the first time it would be summarized away, kept
// verbatim (not the lossy LLM summary), and placed in the leading-system head.
func TestGoalPinnedOnFirstCompaction(t *testing.T) {
	goal := "Migrate the billing service to the new pricing API by Friday"
	prov := &scriptedSummaryProvider{summary: "## Goal\nsomething the model paraphrased\n## Next Steps\n1. go"}
	msgs := goalTranscript(goal)

	out := compactWithSummary(context.Background(), prov, "m", msgs, CompactionSettings{KeepRecentTokens: 3000})

	got, ok := findGoalPin(out)
	if !ok {
		t.Fatalf("expected a pinned goal after compaction, got %+v", names(out))
	}
	if got != goal {
		t.Fatalf("goal pin must be the verbatim original task; got %q want %q", got, goal)
	}
	// The pin must sit in the leading-system head (before the summary), so the next
	// compaction's leadingSystemCount keeps it.
	pinIdx, sumIdx := -1, -1
	for i, m := range out {
		if m.Role == RoleSystem && strings.HasPrefix(m.Content, goalMarker) {
			pinIdx = i
		}
		if m.Role == RoleSystem && strings.HasPrefix(m.Content, summaryMarker) {
			sumIdx = i
		}
	}
	if pinIdx < 0 || sumIdx < 0 || pinIdx > sumIdx {
		t.Fatalf("goal pin must precede the summary; pinIdx=%d sumIdx=%d", pinIdx, sumIdx)
	}
}

// TestGoalSurvivesRepeatedCompaction is the core long-running property: after
// many compactions (each folding the prior summary into a new one), the original
// goal is still present verbatim exactly once — it never drifts and never
// duplicates.
func TestGoalSurvivesRepeatedCompaction(t *testing.T) {
	goal := "Keep the nightly ETL green and alert me on any row-count drop over 5%"
	prov := &scriptedSummaryProvider{summary: "## Goal\nparaphrase that should NOT replace the pin\n## Progress\n- did stuff"}
	msgs := goalTranscript(goal)

	for round := 0; round < 5; round++ {
		msgs = compactWithSummary(context.Background(), prov, "m", msgs, CompactionSettings{KeepRecentTokens: 3000})
		// Simulate the run growing again between compactions so there is always an
		// older span to fold on the next round.
		for i := 0; i < 8; i++ {
			msgs = append(msgs,
				Message{Role: RoleAssistant, Content: "more work " + bigText(2000)},
				Message{Role: RoleUser, Content: "next " + bigText(2000)},
			)
		}

		pins := 0
		for _, m := range msgs {
			if m.Role == RoleSystem && strings.HasPrefix(m.Content, goalMarker) {
				pins++
			}
		}
		if pins != 1 {
			t.Fatalf("round %d: expected exactly one goal pin, got %d", round, pins)
		}
		got, _ := findGoalPin(msgs)
		if got != goal {
			t.Fatalf("round %d: goal drifted to %q", round, got)
		}
	}
}

func names(msgs []Message) []string {
	out := make([]string, len(msgs))
	for i, m := range msgs {
		c := m.Content
		if len(c) > 24 {
			c = c[:24]
		}
		out[i] = string(m.Role) + ":" + c
	}
	return out
}
