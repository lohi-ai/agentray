package agentruntime

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/lohi-ai/agentray/agentcore"
	"github.com/lohi-ai/agentray/internal/dataplane/store"
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
	if plan := planCompaction(entries, 0, false); plan.ok {
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
	plan := planCompaction(entries, 0, false)
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
	if plan := planCompaction(entries, 0, false); plan.ok {
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
	plan := planCompaction(entries, 0, false)
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

// The conversation trigger and the in-run compaction budget shrink the same
// thread for the same model, so they must be capped by the same window. Before
// this was a parameter it was a hardcoded 128k that disagreed with whatever the
// run was actually pointed at: on a smaller model the thread replays a history
// the model rejects, on a larger one it summarizes long before it needed to.
func TestPlanCompactionUsesTheModelsWindow(t *testing.T) {
	// A live window of ~60k tokens: comfortably inside 128k, far past 32k.
	var entries []storage.AgentConversationEntry
	for i := 0; i < 20; i++ {
		role := string(agentcore.RoleUser)
		if i%2 == 1 {
			role = string(agentcore.RoleAssistant)
		}
		entries = append(entries, storage.AgentConversationEntry{
			Kind: ConvKindMessage, Role: role, TokenEstimate: 3000,
		})
	}

	if plan := planCompaction(entries, 128_000, false); plan.ok {
		t.Fatal("a 60k thread compacted on a 128k model; the window is being ignored")
	}
	if plan := planCompaction(entries, 32_000, false); !plan.ok {
		t.Fatal("a 60k thread did not compact on a 32k model; it would replay a history the model cannot accept")
	}
}

// 0 means "the caller could not find out", and every caller is on a
// must-not-fail path. It has to mean the conservative default, not a window of
// zero (which would compact every thread on its first turn).
func TestPlanCompactionFallsBackWhenTheWindowIsUnknown(t *testing.T) {
	entries := []storage.AgentConversationEntry{
		{Kind: ConvKindMessage, Role: string(agentcore.RoleUser), TokenEstimate: 100},
	}
	if plan := planCompaction(entries, 0, false); plan.ok {
		t.Fatal("a tiny thread compacted with an unknown window; 0 was read as a zero-sized window")
	}
}

// --- /clear seam + forced compaction -------------------------------------

func compEntry(id, summary, firstKept string) storage.AgentConversationEntry {
	p, _ := json.Marshal(convCompactionPayload{Summary: summary, FirstKeptEntryID: firstKept})
	return storage.AgentConversationEntry{ID: id, Kind: ConvKindCompaction, PayloadJSON: string(p)}
}

// A /clear drops everything above it out of the model's context — and unlike a
// compaction, leaves nothing standing in for it.
func TestFoldHistoryClearDropsEverythingAbove(t *testing.T) {
	entries := []storage.AgentConversationEntry{
		msgEntry("1", "user", "old question"),
		msgEntry("2", "assistant", "old answer"),
		{ID: "3", Kind: ConvKindClear, PayloadJSON: "{}"},
		msgEntry("4", "user", "fresh question"),
	}
	got := foldHistory(entries)
	if len(got) != 1 || got[0].Content != "fresh question" {
		t.Fatalf("want only the post-clear turn, got %+v", got)
	}
}

// A clear after a compaction must drop that compaction's summary too: handing
// the model a summary of what it was just told to forget is not a fresh start.
func TestFoldHistoryClearSupersedesEarlierCompaction(t *testing.T) {
	entries := []storage.AgentConversationEntry{
		msgEntry("1", "user", "ancient"),
		compEntry("2", "summary of the ancient part", "3"),
		msgEntry("3", "user", "middle"),
		{ID: "4", Kind: ConvKindClear, PayloadJSON: "{}"},
		msgEntry("5", "user", "fresh"),
	}
	got := foldHistory(entries)
	if len(got) != 1 || got[0].Content != "fresh" {
		t.Fatalf("clear did not supersede the compaction summary: %+v", got)
	}
}

// A compaction after a clear is the ordinary case again: the clear bounds the
// live window, the compaction summarizes inside it.
func TestFoldHistoryCompactionAfterClearWins(t *testing.T) {
	entries := []storage.AgentConversationEntry{
		msgEntry("1", "user", "forgotten"),
		{ID: "2", Kind: ConvKindClear, PayloadJSON: "{}"},
		msgEntry("3", "user", "since the clear"),
		compEntry("4", "summary since the clear", "5"),
		msgEntry("5", "user", "latest"),
	}
	got := foldHistory(entries)
	if len(got) != 2 || got[0].Content != "summary since the clear" || got[1].Content != "latest" {
		t.Fatalf("want summary + latest, got %+v", got)
	}
}

// Forced (/compact) folds a thread that the automatic trigger would leave alone,
// keeping the most recent user turn verbatim.
func TestPlanCompactionForcedBelowThreshold(t *testing.T) {
	entries := []storage.AgentConversationEntry{
		msgEntry("1", "user", "first"),
		msgEntry("2", "assistant", "answer"),
		msgEntry("3", "user", "second"),
	}
	if plan := planCompaction(entries, 128_000, false); plan.ok {
		t.Fatal("automatic compaction fired on a tiny thread")
	}
	plan := planCompaction(entries, 128_000, true)
	if !plan.ok {
		t.Fatal("forced compaction did not fire")
	}
	if plan.firstKept != 2 {
		t.Fatalf("forced cut at %d, want the last user turn (2)", plan.firstKept)
	}
}

// Nothing to fold is not a failure: a thread whose only user turn is the /compact
// message itself has no earlier turn to summarize.
func TestPlanCompactionForcedNoOpOnSingleTurn(t *testing.T) {
	entries := []storage.AgentConversationEntry{msgEntry("1", "user", "/compact")}
	if plan := planCompaction(entries, 128_000, true); plan.ok {
		t.Fatal("forced compaction fired with nothing above the cut")
	}
}

// Compaction never reaches back across a clear: the entries above it are gone
// from context by the user's instruction, and summarizing them would put them
// straight back.
func TestPlanCompactionStopsAtClear(t *testing.T) {
	entries := []storage.AgentConversationEntry{
		msgEntry("1", "user", "forgotten"),
		msgEntry("2", "assistant", "forgotten answer"),
		{ID: "3", Kind: ConvKindClear, PayloadJSON: "{}"},
		msgEntry("4", "user", "after"),
		msgEntry("5", "assistant", "reply"),
		msgEntry("6", "user", "latest"),
	}
	plan := planCompaction(entries, 128_000, true)
	if !plan.ok {
		t.Fatal("forced compaction did not fire")
	}
	if plan.liveStart != 3 {
		t.Fatalf("live window starts at %d, want just after the clear (3)", plan.liveStart)
	}
	if plan.firstKept != 5 {
		t.Fatalf("cut at %d, want the last user turn (5)", plan.firstKept)
	}
}

// The plan renderer is what /plan and the chat surface both read, so its three
// states have to be distinguishable at a glance.
func TestRenderPlanMarksTheStates(t *testing.T) {
	out := RenderPlan([]PlanItem{
		{Content: "read the schema", Status: "completed"},
		{Content: "write the query", Status: "in_progress"},
		{Content: "verify the numbers", Status: "pending"},
	})
	for _, want := range []string{"[x]", "~~read the schema~~", "**write the query**", "- [ ] verify the numbers"} {
		if !strings.Contains(out, want) {
			t.Fatalf("rendered plan missing %q:\n%s", want, out)
		}
	}
	if RenderPlan(nil) != "" {
		t.Fatal("an empty plan must render as nothing")
	}
}

// /compact twice in a row, or /compact right after /clear, leaves one lonely
// assistant reply above the newest user turn. Folding that costs a summary call
// and reports "Compacted" for a thread that did not change.
func TestForcedCompactionNeedsAnExchangeAbove(t *testing.T) {
	lonely := []storage.AgentConversationEntry{
		tokEntry("1", "user", "earlier question", 200),
		tokEntry("2", "assistant", "earlier answer", 200),
		{ID: "3", Kind: ConvKindClear, PayloadJSON: "{}"},
		tokEntry("4", "assistant", "Cleared.", 30),
		tokEntry("5", "user", "/compact", 5),
	}
	if plan := planCompaction(lonely, 8000, true); plan.ok {
		t.Fatalf("compacted a lone reply above the cut: firstKept=%d", plan.firstKept)
	}

	// One real exchange above the cut and it proceeds.
	withExchange := append(lonely[:4:4],
		tokEntry("5", "user", "a real question", 100),
		tokEntry("6", "assistant", "a real answer", 100),
		tokEntry("7", "user", "/compact", 5),
	)
	plan := planCompaction(withExchange, 8000, true)
	if !plan.ok {
		t.Fatal("refused to compact a thread with a real exchange above the cut")
	}
	if plan.firstKept != 6 {
		t.Fatalf("cut should land on the newest user turn (index 6), got %d", plan.firstKept)
	}
}

// cmdEntry is a control-plane turn: a handled slash command or the server's reply
// to one. It is in the transcript but must never be in the model's context.
func cmdEntry(id, role, text string) storage.AgentConversationEntry {
	p, _ := json.Marshal(convMessagePayload{Text: text, Command: true})
	return storage.AgentConversationEntry{ID: id, Kind: ConvKindMessage, Role: role, PayloadJSON: string(p)}
}

// Replaying a command turn as history hands the model the server's own words
// about the conversation. A run whose most recent "assistant" message reads
// "Cleared. I've forgotten everything above this line" will echo it back as its
// answer — which is exactly what happened before this was skipped.
func TestFoldHistorySkipsCommandTurns(t *testing.T) {
	entries := []storage.AgentConversationEntry{
		msgEntry("1", "user", "how did last week go?"),
		msgEntry("2", "assistant", "signups were up 12%"),
		cmdEntry("3", "user", "/compact"),
		cmdEntry("4", "assistant", "Compacted. I've summarized the earlier part of this chat…"),
		msgEntry("5", "user", "and this week?"),
	}
	got := foldHistory(entries)
	if len(got) != 3 {
		t.Fatalf("want 3 real messages, got %d: %+v", len(got), got)
	}
	for _, m := range got {
		if strings.Contains(m.Content, "Compacted") || m.Content == "/compact" {
			t.Fatalf("a command turn leaked into context: %+v", m)
		}
	}
}

// The transcript handed to the summarizer is model context too — a command turn
// summarized into the running summary comes back on every later turn.
func TestRenderTranscriptSkipsCommandTurns(t *testing.T) {
	entries := []storage.AgentConversationEntry{
		msgEntry("1", "user", "old q"),
		cmdEntry("2", "user", "/help"),
		cmdEntry("3", "assistant", "Here's what you can type in this box."),
		msgEntry("4", "assistant", "old a"),
	}
	if got := renderTranscript(entries); got != "user: old q\nassistant: old a\n" {
		t.Fatalf("transcript = %q", got)
	}
}
