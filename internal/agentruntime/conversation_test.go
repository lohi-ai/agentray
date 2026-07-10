package agentruntime

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/lohi-ai/agentray/agentcore"
	"github.com/lohi-ai/agentray/internal/storage"
)

func msgEntry(id, role, text string) storage.AgentConversationEntry {
	p, _ := json.Marshal(convMessagePayload{Text: text})
	return storage.AgentConversationEntry{ID: id, Kind: ConvKindMessage, Role: role, PayloadJSON: string(p)}
}

func TestFoldHistoryEmitsMessagesInOrder(t *testing.T) {
	entries := []storage.AgentConversationEntry{
		msgEntry("1", "user", "hello"),
		msgEntry("2", "assistant", "hi there"),
		msgEntry("3", "user", "how are you"),
	}
	got := foldHistory(entries)
	if len(got) != 3 {
		t.Fatalf("want 3 messages, got %d", len(got))
	}
	if got[0].Role != agentcore.RoleUser || got[0].Content != "hello" {
		t.Fatalf("first message wrong: %+v", got[0])
	}
	if got[1].Role != agentcore.RoleAssistant || got[1].Content != "hi there" {
		t.Fatalf("second message wrong: %+v", got[1])
	}
}

func TestFoldHistorySkipsNonMessageKinds(t *testing.T) {
	entries := []storage.AgentConversationEntry{
		msgEntry("1", "user", "run a report"),
		{ID: "2", Kind: ConvKindToolTrace, PayloadJSON: `{"tool":"query"}`},
		{ID: "3", Kind: ConvKindStep, PayloadJSON: `{"note":"working"}`},
		msgEntry("4", "assistant", "done"),
	}
	got := foldHistory(entries)
	if len(got) != 2 {
		t.Fatalf("want 2 messages (tool/step skipped), got %d: %+v", len(got), got)
	}
	if got[1].Content != "done" {
		t.Fatalf("want assistant 'done', got %q", got[1].Content)
	}
}

func TestFoldHistorySkipsInvalidRoleAndEmptyText(t *testing.T) {
	entries := []storage.AgentConversationEntry{
		msgEntry("1", "user", "keep"),
		msgEntry("2", "tool", "drop-bad-role"),
		msgEntry("3", "assistant", ""),
	}
	got := foldHistory(entries)
	if len(got) != 1 || got[0].Content != "keep" {
		t.Fatalf("want only the valid user message, got %+v", got)
	}
}

func TestFoldHistoryCompactionDropsPrefixAndEmitsSummary(t *testing.T) {
	comp, _ := json.Marshal(convCompactionPayload{
		Summary:          "Earlier: user asked about retention; agent pulled the cohort.",
		FirstKeptEntryID: "4",
	})
	entries := []storage.AgentConversationEntry{
		msgEntry("1", "user", "old turn 1"),
		msgEntry("2", "assistant", "old answer 1"),
		{ID: "3", Kind: ConvKindCompaction, PayloadJSON: string(comp)},
		msgEntry("4", "user", "recent question"),
		msgEntry("5", "assistant", "recent answer"),
	}
	got := foldHistory(entries)
	if len(got) != 3 {
		t.Fatalf("want summary + 2 kept messages, got %d: %+v", len(got), got)
	}
	if got[0].Role != agentcore.RoleSystem || got[0].Content == "" {
		t.Fatalf("first message should be the compaction summary, got %+v", got[0])
	}
	if got[1].Content != "recent question" || got[2].Content != "recent answer" {
		t.Fatalf("kept messages wrong: %+v", got[1:])
	}
}

func TestFoldHistoryCompactionFallsBackWhenFirstKeptMissing(t *testing.T) {
	comp, _ := json.Marshal(convCompactionPayload{Summary: "summary", FirstKeptEntryID: "nonexistent"})
	entries := []storage.AgentConversationEntry{
		msgEntry("1", "user", "old"),
		{ID: "2", Kind: ConvKindCompaction, PayloadJSON: string(comp)},
		msgEntry("3", "assistant", "after compaction"),
	}
	got := foldHistory(entries)
	// Summary + the single post-compaction message (prefix represented by summary).
	if len(got) != 2 || got[0].Role != agentcore.RoleSystem || got[1].Content != "after compaction" {
		t.Fatalf("fallback to just-after-compaction failed: %+v", got)
	}
}

func TestMessageEntryText(t *testing.T) {
	// A message entry yields its text (regenerate resends this verbatim).
	if got := MessageEntryText(msgEntry("1", "user", "resend me")); got != "resend me" {
		t.Fatalf("want %q, got %q", "resend me", got)
	}
	// Non-message kinds and unparsable payloads yield "".
	if got := MessageEntryText(storage.AgentConversationEntry{Kind: ConvKindToolTrace, PayloadJSON: `{"tool":"q"}`}); got != "" {
		t.Fatalf("non-message should yield empty, got %q", got)
	}
	if got := MessageEntryText(storage.AgentConversationEntry{Kind: ConvKindMessage, PayloadJSON: "not-json"}); got != "" {
		t.Fatalf("unparsable should yield empty, got %q", got)
	}
}

func TestEstimateTokensCharsOverFour(t *testing.T) {
	if got := estimateTokens("abcd"); got != 1 {
		t.Fatalf("want 1 token for 4 chars, got %d", got)
	}
	if got := estimateTokens(""); got != 0 {
		t.Fatalf("want 0 tokens for empty, got %d", got)
	}
}

// tokEntry is a message entry with an explicit token estimate, so the compaction
// planner's threshold/cut logic can be driven without megabytes of real text.
func tokEntry(id, role, text string, tokens int) storage.AgentConversationEntry {
	e := msgEntry(id, role, text)
	e.TokenEstimate = tokens
	return e
}

// Below the compaction threshold, the planner does nothing — the whole live
// window stays in the model context. This is the common case for short threads.
func TestPlanCompactionBelowThresholdIsNoOp(t *testing.T) {
	entries := []storage.AgentConversationEntry{
		tokEntry("1", "user", "hi", 100),
		tokEntry("2", "assistant", "hello", 100),
	}
	if plan := planCompaction(entries); plan.ok {
		t.Fatalf("short thread should not compact, got %+v", plan)
	}
}

// Over the threshold, the planner cuts at the next-older user turn boundary past
// the keep-recent window: older turns are summarized away, recent turns survive
// verbatim, and the cut never lands mid-turn. This is the core context-management
// behavior the model relies on for long threads.
func TestPlanCompactionCutsAtUserTurnBoundary(t *testing.T) {
	// total = 240k > 128k-16384; walking back, the recent asst(20k)+user(15k)
	// reaches keepRecent(20k) at the user message, so that user turn is the cut.
	entries := []storage.AgentConversationEntry{
		tokEntry("1", "user", "old q1", 100000),
		tokEntry("2", "assistant", "old a1", 100000),
		tokEntry("3", "user", "recent q", 15000),
		tokEntry("4", "assistant", "recent a", 25000),
	}
	plan := planCompaction(entries)
	if !plan.ok {
		t.Fatalf("long thread should compact, got %+v", plan)
	}
	if plan.firstKept != 2 {
		t.Fatalf("first kept should be the recent user turn (index 2), got %d", plan.firstKept)
	}
	// Transcript to summarize is everything before the cut; kept entries (incl. the
	// boundary user turn) are not in it.
	if got := renderTranscript(entries[plan.liveStart:plan.firstKept]); got != "user: old q1\nassistant: old a1\n" {
		t.Fatalf("transcript = %q", got)
	}
}

// When the only turn boundary past keepRecent is the very first entry, cutting
// there would drop the entire window — so the planner declines rather than
// summarize everything (the recent window must keep at least one real turn).
func TestPlanCompactionDeclinesWhenNoCleanCut(t *testing.T) {
	entries := []storage.AgentConversationEntry{
		tokEntry("1", "user", "one giant turn", 200000),
		tokEntry("2", "assistant", "answer", 5000),
	}
	if plan := planCompaction(entries); plan.ok {
		t.Fatalf("no clean cut should decline, got %+v", plan)
	}
}

// Iterative compaction: a thread that already has a compaction entry only counts
// the live window after it, and carries that compaction's summary forward — so the
// next summary extends the prior one instead of silently dropping it (pi's
// update-summary). This is what keeps very old context represented across repeated
// compactions of one long conversation.
func TestPlanCompactionCarriesPriorSummaryForward(t *testing.T) {
	comp, _ := json.Marshal(convCompactionPayload{Summary: "EARLIER: onboarding discussion."})
	entries := []storage.AgentConversationEntry{
		tokEntry("1", "user", "ancient", 90000),
		{ID: "2", Kind: ConvKindCompaction, PayloadJSON: string(comp), TokenEstimate: 50},
		tokEntry("3", "user", "post q1", 100000),
		tokEntry("4", "assistant", "post a1", 100000),
		tokEntry("5", "user", "recent q", 15000),
		tokEntry("6", "assistant", "recent a", 25000),
	}
	plan := planCompaction(entries)
	if !plan.ok {
		t.Fatalf("should compact, got %+v", plan)
	}
	if plan.liveStart != 2 {
		t.Fatalf("live window should start after the compaction entry (index 2), got %d", plan.liveStart)
	}
	if plan.prevSummary != "EARLIER: onboarding discussion." {
		t.Fatalf("prior summary not carried forward: %q", plan.prevSummary)
	}
	// The pre-compaction "ancient" entry (index 0) is below liveStart, so it is
	// never re-summarized — it lives only in prevSummary now.
	if got := renderTranscript(entries[plan.liveStart:plan.firstKept]); strings.Contains(got, "ancient") {
		t.Fatalf("pre-compaction prefix must not be re-summarized: %q", got)
	}
}

// renderTranscript flattens only message entries into "role: text" lines; tool
// traces, steps, and compaction brackets are not part of the text handed to the
// summarizer (their effect shows in the surrounding messages).
func TestRenderTranscriptMessagesOnly(t *testing.T) {
	entries := []storage.AgentConversationEntry{
		msgEntry("1", "user", "question"),
		{ID: "2", Kind: ConvKindToolTrace, PayloadJSON: `{"tool":"q"}`},
		msgEntry("3", "assistant", "answer"),
	}
	if got := renderTranscript(entries); got != "user: question\nassistant: answer\n" {
		t.Fatalf("transcript = %q", got)
	}
}

// The two-projection invariant the store is built on: one entry log feeds both the
// human FE view (every entry, verbatim) and the model context (messages only, with
// the compacted prefix replaced by its summary). This asserts they diverge exactly
// as designed from a single mixed log — the FE keeps the tool trace and the old
// turns; the model sees only the summary plus the kept messages.
func TestModelAndHumanProjectionsDivergeFromOneLog(t *testing.T) {
	comp, _ := json.Marshal(convCompactionPayload{Summary: "Earlier work summarized.", FirstKeptEntryID: "4"})
	log := []storage.AgentConversationEntry{
		msgEntry("1", "user", "old turn"),
		{ID: "2", Kind: ConvKindToolTrace, PayloadJSON: `{"tool":"run_sql"}`},
		{ID: "3", Kind: ConvKindCompaction, PayloadJSON: string(comp)},
		msgEntry("4", "user", "recent question"),
		msgEntry("5", "assistant", "recent answer"),
	}

	// Human/FE projection = the raw log (storage.ConversationEntries returns every
	// entry of every kind, unfolded). Nothing is dropped for display.
	if len(log) != 5 {
		t.Fatalf("FE projection should keep all 5 entries, got %d", len(log))
	}

	// Model projection = folded: summary system message + the two kept turns; the
	// old turn and the tool trace are gone from the model's context.
	model := foldHistory(log)
	if len(model) != 3 || model[0].Role != agentcore.RoleSystem {
		t.Fatalf("model projection should be summary + 2 kept turns, got %+v", model)
	}
	for _, m := range model {
		if strings.Contains(m.Content, "old turn") {
			t.Fatalf("compacted prefix leaked into model context: %+v", model)
		}
	}
}
