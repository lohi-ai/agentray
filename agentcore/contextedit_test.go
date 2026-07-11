package agentcore

import (
	"strings"
	"testing"
)

// editFixture builds a transcript whose old span holds tool results in various
// states of staleness, followed by a bulky recent tail that keeps the cut point
// ahead of them. budget is chosen so the byte estimate exceeds budget/2.
func editFixture() []Message {
	big := strings.Repeat("x", 4000)
	return []Message{
		{Role: RoleSystem, Content: "system prompt"},
		// Turn 1: read a.txt (will be superseded by an identical later read).
		{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "c1", Name: "read_file", Arguments: `{"path":"a.txt"}`}}},
		{Role: RoleTool, ToolCallID: "c1", Name: "read_file", Content: "a-v1 " + big},
		// Turn 2: read b.txt (will be staled by a later edit to b.txt).
		{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "c2", Name: "read_file", Arguments: `{"path":"b.txt"}`}}},
		{Role: RoleTool, ToolCallID: "c2", Name: "read_file", Content: "b-v1 " + big},
		// Turn 3: read c.txt (never superseded, but bulky and old → age rule).
		{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "c3", Name: "read_file", Arguments: `{"path":"c.txt"}`}}},
		{Role: RoleTool, ToolCallID: "c3", Name: "read_file", Content: "c-v1 " + big},
		// Turn 4: small old result — below every clearing floor, must survive.
		{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "c4", Name: "run_shell", Arguments: `{"command":"true"}`}}},
		{Role: RoleTool, ToolCallID: "c4", Name: "run_shell", Content: "exit_code: 0"},
		// Turn 5: edit b.txt — stales c2's read.
		{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "c5", Name: "edit_file", Arguments: `{"path":"b.txt","old_string":"x","new_string":"y"}`}}},
		{Role: RoleTool, ToolCallID: "c5", Name: "edit_file", Content: "path: b.txt\nreplacements: 1"},
		// Turn 6: identical re-read of a.txt — supersedes c1.
		{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "c6", Name: "read_file", Arguments: `{"path":"a.txt"}`}}},
		{Role: RoleTool, ToolCallID: "c6", Name: "read_file", Content: "a-v2 " + big},
		// Recent tail: bulky enough that findCutPoint lands after turn 6's read.
		{Role: RoleUser, Content: strings.Repeat("recent tail ", 400)},
		{Role: RoleAssistant, Content: "working on it"},
	}
}

func TestEditContextBelowThresholdUntouched(t *testing.T) {
	msgs := editFixture()
	// Huge budget → estimate is under half of it → no pass.
	out, edited := editContext(msgs, 10_000_000, DefaultCompactionSettings())
	if edited {
		t.Fatal("must not edit below the soft threshold")
	}
	if &out[0] != &msgs[0] {
		t.Fatal("below threshold the original slice must be returned")
	}
}

func TestEditContextClearsByRule(t *testing.T) {
	msgs := editFixture()
	// Small budget + small keep window: threshold exceeded, cut lands before
	// the tail user message.
	out, edited := editContext(msgs, 1000, CompactionSettings{KeepRecentTokens: 500})
	if !edited {
		t.Fatal("expected the pass to fire")
	}
	byID := map[string]string{}
	for _, m := range out {
		if m.Role == RoleTool {
			byID[m.ToolCallID] = m.Content
		}
	}
	// Rule 1: c1 superseded by identical c6.
	if !strings.Contains(byID["c1"], "superseded by a newer identical read_file") {
		t.Fatalf("c1 = %q, want superseded placeholder", byID["c1"])
	}
	// Rule 2: c2 staled by the later edit_file on b.txt.
	if !strings.Contains(byID["c2"], "b.txt was modified later") {
		t.Fatalf("c2 = %q, want stale-read placeholder", byID["c2"])
	}
	// Rule 3: c3 cleared on age alone.
	if !strings.Contains(byID["c3"], "cleared to save context") {
		t.Fatalf("c3 = %q, want age placeholder", byID["c3"])
	}
	// Small result survives every rule.
	if byID["c4"] != "exit_code: 0" {
		t.Fatalf("c4 = %q, small result must survive", byID["c4"])
	}
	// The newest read of a.txt (c6) is not "superseded", but it is itself old
	// and bulky, so the age rule clears it — with the re-runnable placeholder,
	// never the misleading superseded one.
	if !strings.Contains(byID["c6"], "cleared to save context") {
		t.Fatalf("c6 = %q, want age placeholder", byID["c6"])
	}
	// Linkage survives clearing so the transcript stays provider-valid.
	for _, m := range out {
		if m.Role == RoleTool && m.ToolCallID == "" {
			t.Fatalf("cleared result lost its ToolCallID: %+v", m)
		}
	}
}

func TestEditContextDoesNotMutateInput(t *testing.T) {
	msgs := editFixture()
	orig := msgs[2].Content
	if _, edited := editContext(msgs, 1000, CompactionSettings{KeepRecentTokens: 500}); !edited {
		t.Fatal("expected the pass to fire")
	}
	if msgs[2].Content != orig {
		t.Fatal("input slice was mutated")
	}
}

func TestEditContextIdempotent(t *testing.T) {
	msgs := editFixture()
	settings := CompactionSettings{KeepRecentTokens: 500}
	out, edited := editContext(msgs, 1000, settings)
	if !edited {
		t.Fatal("first pass must edit")
	}
	// Second pass finds only placeholders in the old span → reports no edit,
	// so the loop can proceed to full compaction instead of wedging.
	if _, again := editContext(out, 1000, settings); again {
		t.Fatal("second pass must be a no-op")
	}
}

// TestEditContextKeepsRecentWindow: a bulky tool result inside the keep-recent
// tail is never cleared, even when the pass fires.
func TestEditContextKeepsRecentWindow(t *testing.T) {
	big := strings.Repeat("y", 8000)
	msgs := []Message{
		{Role: RoleSystem, Content: "sys"},
		{Role: RoleUser, Content: strings.Repeat("old padding ", 500)},
		{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "r1", Name: "read_file", Arguments: `{"path":"z.txt"}`}}},
		{Role: RoleTool, ToolCallID: "r1", Name: "read_file", Content: big},
	}
	out, _ := editContext(msgs, 1000, CompactionSettings{KeepRecentTokens: 4000})
	last := out[len(out)-1]
	if last.Content != big {
		t.Fatalf("recent tool result must stay verbatim, got %q", last.Content[:60])
	}
}
