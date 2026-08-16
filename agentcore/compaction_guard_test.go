package agentcore

import (
	"context"
	"strings"
	"testing"
)

// TestSerializeConversationBoundsToolPayloads verifies one giant tool result
// (or argument blob) cannot blow the summarizer's own request: serialization
// truncates head+tail with the marker in between.
func TestSerializeConversationBoundsToolPayloads(t *testing.T) {
	span := []Message{
		{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "c", Name: "run_sql", Arguments: `{"q":"` + bigText(50_000) + `"}`}}},
		{Role: RoleTool, Name: "run_sql", Content: bigText(100_000)},
	}
	out := serializeConversation(span)
	if len(out) > maxSerializedToolResult+maxSerializedToolArgs+512 {
		t.Fatalf("serialized span too large: %d bytes", len(out))
	}
	if !strings.Contains(out, "truncated") {
		t.Fatal("expected a truncation marker in the serialized span")
	}
}

// TestElideOversizedTailShrinksBelowBudget pins the guard mechanics: bulky tool
// results are collapsed oldest-first, linkage kept, final two messages
// untouched.
func TestElideOversizedTailShrinksBelowBudget(t *testing.T) {
	tail := []Message{
		{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "c1", Name: "q"}}},
		{Role: RoleTool, ToolCallID: "c1", Name: "q", Content: bigText(40_000)},
		{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "c2", Name: "q"}}},
		{Role: RoleTool, ToolCallID: "c2", Name: "q", Content: bigText(40_000)},
		{Role: RoleAssistant, Content: "working on it"},
	}
	out, shrunk := elideOversizedTail(tail, 5_000)
	if !shrunk {
		t.Fatal("guard did not shrink an oversized tail")
	}
	if estimateBytesTokens(out) > 5_000+ /*headroom for the untouched final turn*/ 10_000 {
		t.Fatalf("tail still oversized: ~%d tokens", estimateBytesTokens(out))
	}
	if out[1].ToolCallID != "c1" || out[1].Name != "q" {
		t.Fatalf("elided result lost call linkage: %+v", out[1])
	}
	if len(out[1].Content) > elidedResultBytes {
		t.Fatalf("first bulky result should have been cut down, got %d bytes", len(out[1].Content))
	}
	if !strings.Contains(out[1].Content, "truncated") {
		t.Fatalf("a cut-down result must say so, or the model reads a partial answer as a whole one: %q",
			out[1].Content)
	}
	if out[4].Content != "working on it" {
		t.Fatal("final message must be untouched")
	}
	// Original slice unmodified (guard copies).
	if !strings.HasPrefix(tail[1].Content, "xx") {
		t.Fatal("guard mutated the caller's slice")
	}
}

// TestElidedResultKeepsBothEnds is the reason the guard cuts rather than
// deletes. A tool result puts its CONCLUSION at the end — the finding, the
// verdict, the error — so a guard that keeps only a placeholder, or only the
// head, throws away the part the call was made for. It costs a kilobyte to keep
// both ends, and the alternative advice ("re-run the tool") is not affordable
// for the results most worth keeping: a spawn_subagent answer is a whole child
// run that has already been paid for.
func TestElidedResultKeepsBothEnds(t *testing.T) {
	const opening = "OPENING: connected to the ledger corpus"
	const verdict = "VERDICT: unreconciled balance of 1743 in the clearing file"
	tail := []Message{
		{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "c1", Name: "spawn_subagent"}}},
		{Role: RoleTool, ToolCallID: "c1", Name: "spawn_subagent",
			Content: opening + "\n" + bigText(40_000) + "\n" + verdict},
		{Role: RoleAssistant, Content: "working on it"},
	}
	out, shrunk := elideOversizedTail(tail, 5_000)
	if !shrunk {
		t.Fatal("guard did not shrink an oversized tail")
	}
	got := out[1].Content
	if !strings.Contains(got, verdict) {
		t.Fatalf("the child's verdict was cut away — that is the one line the delegation was for:\n%s", got)
	}
	if !strings.Contains(got, opening) {
		t.Fatalf("the result's opening was cut away, so the model cannot tell what produced it:\n%s", got)
	}
}

// TestElideOversizedTailNoOpUnderBudget verifies the guard is a strict no-op on
// a tail that already fits.
func TestElideOversizedTailNoOpUnderBudget(t *testing.T) {
	tail := []Message{
		{Role: RoleTool, ToolCallID: "c", Name: "q", Content: bigText(2_000)},
		{Role: RoleAssistant, Content: "ok"},
	}
	out, shrunk := elideOversizedTail(tail, 5_000)
	if shrunk {
		t.Fatal("guard shrank an in-budget tail")
	}
	if &out[0] != &tail[0] {
		t.Fatal("no-op should return the input slice")
	}
}

// TestCompactionUnwedgesOversizedSingleTurn is the stuck-compaction regression
// (pi's split-turn case): a transcript whose "recent" turn alone dwarfs the
// keep budget used to survive compaction unchanged — the next check would
// trigger compaction again, forever, without shrinking anything. The tail
// guard must shrink it even when there is nothing new to fold into the
// summary.
func TestCompactionUnwedgesOversizedSingleTurn(t *testing.T) {
	prov := &scriptedSummaryProvider{summary: "## Goal\ncontinue"}
	msgs := []Message{
		{Role: RoleSystem, Content: "you are an agent"},
		// A prior compaction summary followed ONLY by one huge turn: the cut
		// lands right after the summary, so there is nothing new to fold — the
		// path that used to return the transcript unchanged.
		{Role: RoleSystem, Content: summaryMarker + "\nearlier work"},
		{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "c2", Name: "q"}}},
		{Role: RoleTool, ToolCallID: "c2", Name: "q", Content: bigText(120_000)},
		{Role: RoleAssistant, Content: "still going"},
	}
	before := estimateContextTokens(msgs)
	out, _ := compactWithSummary(context.Background(), prov, "m", msgs, CompactionSettings{KeepRecentTokens: 3_000})
	after := estimateContextTokens(out)
	if after >= before {
		t.Fatalf("compaction did not shrink an oversized single turn: before=%d after=%d", before, after)
	}
	if prov.calls != 0 {
		t.Fatalf("nothing new to fold — no summarization call expected, got %d", prov.calls)
	}
	// The prior summary must survive the guarded early return.
	found := false
	for _, m := range out {
		if strings.HasPrefix(m.Content, summaryMarker) {
			found = true
		}
	}
	if !found {
		t.Fatal("prior compaction summary was dropped by the tail guard path")
	}
	// The elided transcript must remain provider-valid: every tool result still
	// linked to its call.
	for _, m := range out {
		if m.Role == RoleTool && m.ToolCallID == "" {
			t.Fatalf("orphaned tool result after guard: %+v", m)
		}
	}
}
