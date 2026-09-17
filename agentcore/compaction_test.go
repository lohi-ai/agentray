package agentcore

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"testing"
)

// bigText returns a string of n bytes so token estimates cross thresholds.
func bigText(n int) string { return strings.Repeat("x", n) }

// scriptedSummaryProvider returns a fixed summary for the summarization call.
type scriptedSummaryProvider struct {
	summary string
	err     error
	calls   int
}

func (p *scriptedSummaryProvider) Name() string        { return "summary" }
func (p *scriptedSummaryProvider) SupportsTools() bool { return false }
func (p *scriptedSummaryProvider) Stream(context.Context, ChatRequest) (<-chan ChatDelta, error) {
	return nil, errors.New("no stream")
}
func (p *scriptedSummaryProvider) Chat(_ context.Context, _ ChatRequest) (ChatResponse, error) {
	p.calls++
	if p.err != nil {
		return ChatResponse{}, p.err
	}
	return ChatResponse{Message: Message{Role: RoleAssistant, Content: p.summary}, StopReason: "stop"}, nil
}

// longTranscript builds [system, then many user/assistant/tool turns] whose
// older portion exceeds keepRecent so a cut point exists.
func longTranscript() []Message {
	msgs := []Message{{Role: RoleSystem, Content: "you are an agent"}}
	for i := 0; i < 8; i++ {
		msgs = append(msgs,
			Message{Role: RoleUser, Content: "question " + bigText(2000)},
			Message{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "c", Name: "q", Arguments: "{}"}}},
			Message{Role: RoleTool, Name: "q", Content: "result " + bigText(2000)},
			Message{Role: RoleAssistant, Content: "answer " + bigText(500)},
		)
	}
	return msgs
}

func TestCompactWithSummary_ReplacesOlderSpan(t *testing.T) {
	prov := &scriptedSummaryProvider{summary: "## Goal\nfinish the task\n## Next Steps\n1. continue"}
	msgs := longTranscript()
	out, _ := compactWithSummary(context.Background(), prov, "m", msgs, CompactionSettings{KeepRecentTokens: 3000})

	if prov.calls != 1 {
		t.Fatalf("expected 1 summarization call, got %d", prov.calls)
	}
	if len(out) >= len(msgs) {
		t.Fatalf("compaction should shrink transcript: before=%d after=%d", len(msgs), len(out))
	}
	// Leading real system prompt preserved first.
	if out[0].Role != RoleSystem || out[0].Content != "you are an agent" {
		t.Fatalf("system header not preserved: %+v", out[0])
	}
	// A summary message with the marker is present.
	var sawSummary bool
	for _, m := range out {
		if m.Role == RoleSystem && strings.HasPrefix(m.Content, summaryMarker) {
			sawSummary = true
		}
	}
	if !sawSummary {
		t.Fatalf("expected a summary message, got %+v", out)
	}
	// Retained tail must not begin on a tool-result (would break the provider).
	for i, m := range out {
		if m.Role == RoleSystem && strings.HasPrefix(m.Content, summaryMarker) {
			if i+1 < len(out) && out[i+1].Role == RoleTool {
				t.Fatalf("tail begins on a tool-result message")
			}
		}
	}
}

// capturingSummaryProvider records the last summarization request so a test can
// assert how the prompt was built (fresh vs iterative update).
type capturingSummaryProvider struct {
	summary string
	lastReq ChatRequest
	calls   int
}

func (p *capturingSummaryProvider) Name() string        { return "capture" }
func (p *capturingSummaryProvider) SupportsTools() bool { return false }
func (p *capturingSummaryProvider) Stream(context.Context, ChatRequest) (<-chan ChatDelta, error) {
	return nil, errors.New("no stream")
}
func (p *capturingSummaryProvider) Chat(_ context.Context, req ChatRequest) (ChatResponse, error) {
	p.calls++
	p.lastReq = req
	return ChatResponse{Message: Message{Role: RoleAssistant, Content: p.summary}, StopReason: "stop"}, nil
}

// TestCompactWithSummary_IterativeUpdate verifies that when the body already
// begins with a prior summary (a second+ compaction), the older span is folded
// into that summary via the UPDATE prompt instead of being re-summarized raw.
func TestCompactWithSummary_IterativeUpdate(t *testing.T) {
	prov := &capturingSummaryProvider{summary: "## Goal\nupdated goal\n## Next Steps\n1. go"}

	// Transcript: real system header, a PRIOR summary, then many fresh turns.
	msgs := []Message{
		{Role: RoleSystem, Content: "you are an agent"},
		{Role: RoleSystem, Content: summaryMarker + "\nPRIOR-SUMMARY-SENTINEL: earlier facts"},
	}
	for i := 0; i < 8; i++ {
		msgs = append(msgs,
			Message{Role: RoleUser, Content: "fresh question " + bigText(2000)},
			Message{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "c", Name: "q", Arguments: "{}"}}},
			Message{Role: RoleTool, Name: "q", Content: "result " + bigText(2000)},
			Message{Role: RoleAssistant, Content: "answer " + bigText(500)},
		)
	}

	out, _ := compactWithSummary(context.Background(), prov, "m", msgs, CompactionSettings{KeepRecentTokens: 3000})

	if prov.calls != 1 {
		t.Fatalf("expected 1 summarization call, got %d", prov.calls)
	}
	user := prov.lastReq.Messages[len(prov.lastReq.Messages)-1].Content
	// The prior summary must be handed back to the model as the base to update,
	// and the UPDATE prompt (not the from-scratch prompt) must be used.
	if !strings.Contains(user, "PRIOR-SUMMARY-SENTINEL") {
		t.Fatalf("update prompt must carry the previous summary; got:\n%s", user)
	}
	if !strings.Contains(user, "Previous summary") || !strings.Contains(user, "New messages since that summary") {
		t.Fatalf("expected iterative-update framing; got:\n%s", user)
	}
	// Exactly one summary marker survives (the new one replaces the prior).
	markers := 0
	for _, m := range out {
		if m.Role == RoleSystem && strings.HasPrefix(m.Content, summaryMarker) {
			markers++
		}
	}
	if markers != 1 {
		t.Fatalf("expected exactly one summary marker after compaction, got %d", markers)
	}
}

// TestSplitPriorSummary covers the head-detection helper directly.
func TestSplitPriorSummary(t *testing.T) {
	prev, rest := splitPriorSummary([]Message{
		{Role: RoleSystem, Content: summaryMarker + "\nkept facts"},
		{Role: RoleUser, Content: "new"},
	})
	if prev != "kept facts" || len(rest) != 1 || rest[0].Role != RoleUser {
		t.Fatalf("split failed: prev=%q rest=%+v", prev, rest)
	}
	// No prior summary -> empty prev, original slice returned.
	prev2, rest2 := splitPriorSummary([]Message{{Role: RoleUser, Content: "hi"}})
	if prev2 != "" || len(rest2) != 1 {
		t.Fatalf("expected passthrough, got prev=%q rest=%+v", prev2, rest2)
	}
}

func TestCompactWithSummary_FallsBackOnError(t *testing.T) {
	prov := &scriptedSummaryProvider{err: errors.New("boom")}
	msgs := longTranscript()
	out, _ := compactWithSummary(context.Background(), prov, "m", msgs, CompactionSettings{KeepRecentTokens: 3000})
	// Falls back to deterministic elide: no summary marker, still shrinks/holds.
	for _, m := range out {
		if strings.HasPrefix(m.Content, summaryMarker) {
			t.Fatalf("error path must not produce a model summary")
		}
	}
	if len(out) == 0 {
		t.Fatalf("fallback produced empty transcript")
	}
}

func TestEstimateContextTokens_PrefersUsage(t *testing.T) {
	// No usage anywhere -> byte heuristic over the whole transcript.
	plain := []Message{
		{Role: RoleUser, Content: string(make([]byte, 400))},
	}
	if got := estimateContextTokens(plain); got != 100 {
		t.Fatalf("byte fallback: want 100, got %d", got)
	}

	// Assistant carries usage -> use input+output + trailing byte estimate.
	withUsage := []Message{
		{Role: RoleUser, Content: "hi"},
		{Role: RoleAssistant, Content: "ok", Usage: &Usage{InputTokens: 5000, OutputTokens: 200}},
		{Role: RoleTool, Content: string(make([]byte, 400))}, // trailing 400 bytes ≈ 100 tokens
	}
	if got := estimateContextTokens(withUsage); got != 5300 {
		t.Fatalf("usage-based: want 5300, got %d", got)
	}
}

func TestEstimateContextTokens_UsesLatestUsage(t *testing.T) {
	msgs := []Message{
		{Role: RoleAssistant, Content: "a", Usage: &Usage{InputTokens: 1000}},
		{Role: RoleUser, Content: "more"},
		{Role: RoleAssistant, Content: "b", Usage: &Usage{InputTokens: 8000, OutputTokens: 100}},
	}
	// Latest usage wins; nothing trails it.
	if got := estimateContextTokens(msgs); got != 8100 {
		t.Fatalf("want 8100, got %d", got)
	}
}

// TestPinnedRequirementIsBounded drives the pin's accumulation directly rather
// than through a run, because the number of corrections it takes to overflow it
// is larger than a readable end-to-end test should need.
//
// The pin is preserved verbatim by every compaction and never summarized, so an
// unbounded pin is a ratchet in the one message nothing can shrink. Bounding it
// is easy; bounding it without breaking it is the point of this test. Two things
// the ceiling must never cost: the task at the top, which every correction is
// relative to, and the newest instruction at the bottom, which is the one
// actually in force.
func TestPinnedRequirementIsBounded(t *testing.T) {
	const maxTokens = 256
	pin := ""
	for i := range 400 {
		pin = composePin(pin, []string{
			fmt.Sprintf("correction %d: reconcile region %d before filing", i, i),
		}, maxTokens)
	}
	t.Logf("400 corrections render to %d B (ceiling %d B):\n%s", len(pin), maxTokens*4, pin)

	if len(pin) > maxTokens*bytesPerTokenEstimate {
		t.Fatalf("the pin is %d B against a %d B ceiling: corrections accumulate unbounded",
			len(pin), maxTokens*bytesPerTokenEstimate)
	}
	if !strings.Contains(pin, "correction 0:") {
		t.Fatalf("the pin dropped its first entry — the task — so later corrections have nothing "+
			"to correct:\n%s", pin)
	}
	if !strings.Contains(pin, "correction 399:") {
		t.Fatalf("the pin dropped the NEWEST correction, which is the one currently in force:\n%s", pin)
	}
	if !strings.Contains(pin, "omitted for space") {
		t.Fatalf("the pin dropped corrections silently: a model told nothing proceeds on a partial "+
			"requirement, where one told something was dropped can go and read it:\n%s", pin)
	}
}

// A correction steered in twice in a row is one requirement, not two.
func TestPinDoesNotRepeatAnIdenticalCorrection(t *testing.T) {
	pin := composePin("", []string{"audit the ledger"}, 256)
	pin = composePin(pin, []string{"switch to payroll"}, 256)
	again := composePin(pin, []string{"switch to payroll"}, 256)
	if again != pin {
		t.Fatalf("re-steering the same instruction grew the pin:\nbefore %q\nafter  %q", pin, again)
	}
}

// TestCompactCollapsesOldToolResults verifies compaction elides bulky old tool
// results, preserves the system prompt and recent tail verbatim, and inserts a
// single breadcrumb after the system message.
func TestCompactCollapsesOldToolResults(t *testing.T) {
	big := make([]byte, 512)
	for i := range big {
		big[i] = 'x'
	}
	msgs := []Message{
		{Role: RoleSystem, Content: "you are an analyst"},
		{Role: RoleUser, Content: "find anomalies"},
		{Role: RoleTool, Name: "run_sql", ToolCallID: "1", Content: string(big)},
		{Role: RoleTool, Name: "run_sql", ToolCallID: "2", Content: string(big)},
		{Role: RoleAssistant, Content: "thinking"},
		{Role: RoleTool, Name: "run_sql", ToolCallID: "3", Content: string(big)},
		{Role: RoleAssistant, Content: "recent enough"},
	}
	out := compact(msgs, 2)

	if out[0].Role != RoleSystem {
		t.Fatalf("system prompt must stay first, got %v", out[0].Role)
	}
	if out[1].Role != RoleSystem {
		t.Fatalf("breadcrumb must follow system prompt, got %v", out[1].Role)
	}
	// The recent tail (last 2) must be preserved verbatim.
	last := out[len(out)-1]
	if last.Content != "recent enough" {
		t.Errorf("recent tail mutated: %q", last.Content)
	}
	// At least one older bulky tool result must be elided.
	elided := 0
	for _, m := range out {
		if m.Role == RoleTool && m.Content == "[older tool result elided to fit context]" {
			elided++
		}
	}
	if elided == 0 {
		t.Error("expected at least one elided tool result")
	}
}

// TestShouldCompactBudget checks the heuristic boundary and the zero-budget
// fallback to the default ceiling.
func TestShouldCompactBudget(t *testing.T) {
	small := []Message{{Role: RoleUser, Content: "hi"}}
	if shouldCompact(small, 1000) {
		t.Error("tiny context must not trigger compaction")
	}
	body := make([]byte, 4096)
	msgs := []Message{{Role: RoleUser, Content: string(body)}}
	if !shouldCompact(msgs, 100) { // ~1024 est tokens > 100
		t.Error("large context must trigger compaction at low budget")
	}
	if shouldCompact(msgs, 0) { // 0 -> default 96k budget, 1024 est < that
		t.Error("zero budget must fall back to the default ceiling")
	}
}

// A completed compaction is a checkpoint in the durable log.
//
// Compaction shrinks the live transcript, but for a long time the log recorded
// only that it HAPPENED — an empty start/completion bracket. Reduce therefore
// replayed the whole pre-compaction span, so a resumed run rebuilt a history
// the live run had already thrown away, then paid to summarize it a second
// time. Worse, the second summary is a different summary: models are not
// deterministic, so the resumed run reasoned over a conversation that never
// took place — exactly the divergence the log-invariant plugin exists to catch,
// arriving through the one path it cannot see.
//
// The completion entry now carries what compaction LEFT (Retained), and reduce
// restarts history there. These tests hold that property, its backward
// compatibility, and the two ways it could be silently wrong: dropping a system
// message that belongs to the conversation, and swallowing a compaction that
// never finished.

// compactingRun drives a durable run that compacts several times and returns
// the run result, the log, and the summarization count.
func compactingRun(t *testing.T, sessionID string) (RunResult, []SessionEntry, *memSessionStore, int) {
	t.Helper()
	ctx := context.Background()
	store := newMemSessionStore()
	prov := &stressProvider{target: 60, uniqueArgs: true}

	limits := DefaultLimits()
	limits.MaxTurns = 400
	limits.MaxToolCalls = 500
	limits.MaxContextTokens = 4000
	cs := DefaultCompactionSettings()
	cs.KeepRecentTokens = 1500

	agent, err := New(Config{
		Provider:   prov,
		Model:      "stress",
		Tools:      NewToolSet(&blobTool{size: 900}, newPlanTool(newPlanStore())),
		Policy:     NewAllowList("blob", planToolName),
		Limits:     &limits,
		Compaction: &cs,
		Session:    store,
		SessionID:  sessionID,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	res, err := agent.Prompt(ctx, "go")
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if prov.Summaries < 2 {
		t.Fatalf("run did not compact enough to be a useful fixture: %d summaries", prov.Summaries)
	}
	log, err := store.Log(ctx, sessionID)
	if err != nil {
		t.Fatalf("Log: %v", err)
	}
	return res, log, store, prov.Summaries
}

// TestCompletedCompactionIsACheckpoint is the headline property: after a run
// that compacted repeatedly, the log reduces to the transcript the run actually
// ended with — not to the full history those compactions discarded.
//
// The assertion is equality against the live transcript rather than a size
// bound, because "smaller" would also be satisfied by a reduce that truncated
// to the wrong place. The system prompt is the one legitimate difference: it is
// never logged, since every run rebuilds it and prepends it itself.
func TestCompletedCompactionIsACheckpoint(t *testing.T) {
	res, log, _, summaries := compactingRun(t, "s")
	rs := ReduceSession(log)

	if len(res.Messages) == 0 || res.Messages[0].Role != RoleSystem {
		t.Fatalf("expected the live transcript to open with the system prompt: %+v", res.Messages[0])
	}
	live := res.Messages[1:] // drop the system prompt

	if len(rs.Messages) != len(live) {
		t.Fatalf("reduced %d messages, live transcript has %d (after %d compactions) — "+
			"reduce is replaying history the run had already compacted away",
			len(rs.Messages), len(live), summaries)
	}
	for i := range live {
		if rs.Messages[i].Content != live[i].Content || rs.Messages[i].Role != live[i].Role {
			t.Fatalf("message %d diverges:\n reduced = %s %.100q\n live    = %s %.100q",
				i, rs.Messages[i].Role, rs.Messages[i].Content, live[i].Role, live[i].Content)
		}
	}

	// The checkpoint must also fit the budget the live run was held to; that is
	// the whole point (an over-budget resume compacts again before its first
	// turn, paying twice for the same shrink).
	if got, budget := estimateContextTokens(rs.Messages), 4000; got > budget {
		t.Fatalf("reduced context is %d tokens, over the run's %d budget", got, budget)
	}

	// Every completed compaction should carry its retained transcript.
	var finals, retained int
	for _, e := range log {
		if e.Kind == EntryCompaction && e.Final {
			finals++
			if e.Retained != nil {
				retained++
			}
		}
	}
	if finals == 0 || retained != finals {
		t.Fatalf("%d/%d compaction completions carry Retained", retained, finals)
	}
}

// TestResumeAfterCompactionStartsFromCheckpoint is the property that actually
// costs money: an interrupted run resumes on the compacted history, so its
// first provider call is the size the live run was paying, not the size of the
// span every compaction had already folded away.
func TestResumeAfterCompactionStartsFromCheckpoint(t *testing.T) {
	ctx := context.Background()
	res, log, _, _ := compactingRun(t, "s")

	// Simulate a crash after the last compaction: drop the terminating leaf so
	// the log looks interrupted rather than completed (a completed log would
	// reattach and never call the provider at all).
	trimmed := make([]SessionEntry, 0, len(log))
	for _, e := range log {
		if e.Kind == EntryLeaf {
			continue
		}
		trimmed = append(trimmed, e)
	}
	crashed := newMemSessionStore()
	for _, e := range trimmed {
		if err := crashed.Append(ctx, "s", e); err != nil {
			t.Fatal(err)
		}
	}

	// A provider that records the size of the first request it is handed.
	probe := &firstRequestProbe{inner: &stressProvider{target: 1}}
	agent, err := New(Config{
		Provider:      probe,
		Model:         "stress",
		Tools:         NewToolSet(&blobTool{size: 900}, newPlanTool(newPlanStore())),
		Policy:        NewAllowList("blob", planToolName),
		Session:       crashed,
		SessionID:     "s",
		ResumeSession: true,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := agent.Prompt(ctx, "go"); err != nil {
		t.Fatalf("resume Prompt: %v", err)
	}
	if probe.first == 0 {
		t.Fatal("resumed run never called the provider")
	}

	// The resumed request should be the size of the compacted transcript, not of
	// the full log. Compare against the live run's own transcript, allowing a
	// small margin for the resume's own bookkeeping messages.
	if probe.first > len(res.Messages)+8 {
		t.Fatalf("resumed run rebuilt %d messages; the live run finished on %d — "+
			"the compaction checkpoint was not honored", probe.first, len(res.Messages))
	}
}

// firstRequestProbe records how many messages the first Chat request carried.
type firstRequestProbe struct {
	inner *stressProvider
	first int
}

func (p *firstRequestProbe) Name() string        { return "probe" }
func (p *firstRequestProbe) SupportsTools() bool { return true }

func (p *firstRequestProbe) Chat(ctx context.Context, req ChatRequest) (ChatResponse, error) {
	// Ignore the summarization call: it is not the run's own turn.
	isSummary := len(req.Messages) > 0 && strings.HasPrefix(req.Messages[0].Content, "You are a context summarization")
	if p.first == 0 && !isSummary {
		p.first = len(req.Messages)
	}
	return p.inner.Chat(ctx, req)
}

func (p *firstRequestProbe) Stream(ctx context.Context, req ChatRequest) (<-chan ChatDelta, error) {
	return p.inner.Stream(ctx, req)
}

// TestLegacyCompactionEntryReplaysFully pins backward compatibility: a log
// written before the retained transcript existed has no Retained on its
// completion entries, and must still reduce to its full history rather than
// truncating to nothing. Old sessions stay resumable.
func TestLegacyCompactionEntryReplaysFully(t *testing.T) {
	log := []SessionEntry{
		{Kind: EntryMessage, Message: &Message{Role: RoleUser, Content: "task"}},
		{Kind: EntryMessage, Message: &Message{Role: RoleAssistant, Content: "old work"}},
		{Kind: EntryCompaction},              // start
		{Kind: EntryCompaction, Final: true}, // completion, no Retained (legacy)
		{Kind: EntryMessage, Message: &Message{Role: RoleAssistant, Content: "new work"}},
	}
	rs := ReduceSession(log)
	want := []string{"task", "old work", "new work"}
	if len(rs.Messages) != len(want) {
		t.Fatalf("legacy log reduced to %d messages, want %d: %+v", len(rs.Messages), len(want), rs.Messages)
	}
	for i, w := range want {
		if rs.Messages[i].Content != w {
			t.Fatalf("message[%d] = %q, want %q", i, rs.Messages[i].Content, w)
		}
	}
	if rs.PendingCompaction {
		t.Fatal("a closed bracket must not leave PendingCompaction set")
	}
}

// TestCheckpointResetsThenAccumulates verifies the fold's shape directly: the
// most recent checkpoint replaces history, and entries appended after it chain
// onto that — including a later compaction, which resets again.
func TestCheckpointResetsThenAccumulates(t *testing.T) {
	log := []SessionEntry{
		{Kind: EntryMessage, Message: &Message{Role: RoleUser, Content: "discarded by checkpoint 1"}},
		{Kind: EntryCompaction},
		{Kind: EntryCompaction, Final: true, Retained: []Message{
			{Role: RoleSystem, Content: "summary 1"},
			{Role: RoleAssistant, Content: "tail 1"},
		}},
		{Kind: EntryMessage, Message: &Message{Role: RoleAssistant, Content: "after checkpoint 1"}},
		{Kind: EntryCompaction},
		{Kind: EntryCompaction, Final: true, Retained: []Message{
			{Role: RoleSystem, Content: "summary 2"},
			{Role: RoleAssistant, Content: "tail 2"},
		}},
		{Kind: EntryMessage, Message: &Message{Role: RoleAssistant, Content: "after checkpoint 2"}},
	}
	rs := ReduceSession(log)
	want := []string{"summary 2", "tail 2", "after checkpoint 2"}
	if len(rs.Messages) != len(want) {
		t.Fatalf("reduced to %d messages, want %d: %+v", len(rs.Messages), len(want), rs.Messages)
	}
	for i, w := range want {
		if rs.Messages[i].Content != w {
			t.Fatalf("message[%d] = %q, want %q", i, rs.Messages[i].Content, w)
		}
	}
}

// TestPendingCompactionAfterCheckpointStillReruns covers the crash-mid-compaction
// case on top of an existing checkpoint: the unfinished start must still ask
// recovery to re-run compaction, and the history it hands over must be the last
// checkpoint plus the work done since — not the whole log.
func TestPendingCompactionAfterCheckpointStillReruns(t *testing.T) {
	log := []SessionEntry{
		{Kind: EntryMessage, Message: &Message{Role: RoleUser, Content: "discarded"}},
		{Kind: EntryCompaction},
		{Kind: EntryCompaction, Final: true, Retained: []Message{
			{Role: RoleSystem, Content: "summary"},
		}},
		{Kind: EntryMessage, Message: &Message{Role: RoleAssistant, Content: "since"}},
		{Kind: EntryCompaction}, // crash here: started, never completed
	}
	rs := ReduceSession(log)
	if !rs.PendingCompaction {
		t.Fatal("an unfinished compaction must reduce to PendingCompaction")
	}
	want := []string{"summary", "since"}
	if len(rs.Messages) != len(want) {
		t.Fatalf("reduced to %d messages, want %d: %+v", len(rs.Messages), len(want), rs.Messages)
	}
	for i, w := range want {
		if rs.Messages[i].Content != w {
			t.Fatalf("message[%d] = %q, want %q", i, rs.Messages[i].Content, w)
		}
	}
}

// TestRetainedTranscriptKeepsConversationalSystemMessages guards the subtle half
// of the write path. Only the run's OWN system prompt is excluded, because the
// run rebuilds it; a system message that is part of the conversation — a goal
// pin promoted into the head by an earlier compaction, or a caller's seed
// system message — has no other home and must survive.
func TestRetainedTranscriptKeepsConversationalSystemMessages(t *testing.T) {
	const system = "You are a helpful agent."

	t.Run("drops the run's own prompt", func(t *testing.T) {
		got := retainedTranscript([]Message{
			{Role: RoleSystem, Content: system},
			{Role: RoleSystem, Content: goalMarker + "\nship it"},
			{Role: RoleAssistant, Content: "work"},
		}, system)
		if len(got) != 2 || got[0].Content != goalMarker+"\nship it" {
			t.Fatalf("expected the prompt dropped and the goal pin kept: %+v", got)
		}
	})

	t.Run("keeps a leading system message that is not the prompt", func(t *testing.T) {
		got := retainedTranscript([]Message{
			{Role: RoleSystem, Content: goalMarker + "\nship it"},
			{Role: RoleAssistant, Content: "work"},
		}, system)
		if len(got) != 2 || got[0].Content != goalMarker+"\nship it" {
			t.Fatalf("a conversational system message was dropped: %+v", got)
		}
	})

	t.Run("snapshots rather than aliasing", func(t *testing.T) {
		live := []Message{
			{Role: RoleSystem, Content: system},
			{Role: RoleAssistant, Content: "original"},
		}
		got := retainedTranscript(live, system)
		live[1].Content = "mutated after the entry was written"
		if got[0].Content != "original" {
			t.Fatal("Retained aliases the live transcript; a durable entry must be a snapshot")
		}
	})
}

// These tests pin the keep-recent clamp (effectiveCompaction). The wedge it
// fixes, found by the live collision-sim benchmark (bench/collision_test.go):
// with Limits.MaxContextTokens set BELOW the default KeepRecentTokens (20k),
// shouldCompact fires every turn but findCutPoint sees the whole transcript
// inside the "recent" window (cut 0), and the deterministic-elide fallback only
// collapses bulky TOOL RESULTS — so a run whose bulk lives in assistant
// tool-call ARGUMENTS (the write-a-full-file shape) never shrinks: compaction
// runs forever and changes nothing.

func TestEffectiveCompactionClampsKeepRecentToHalfBudget(t *testing.T) {
	// Small budget, default settings: clamp to budget/2.
	got := effectiveCompaction(CompactionSettings{}, 6000)
	if got.KeepRecentTokens != 3000 {
		t.Fatalf("KeepRecentTokens = %d, want 3000 (budget/2)", got.KeepRecentTokens)
	}
	// Explicit setting below the clamp is honored untouched.
	got = effectiveCompaction(CompactionSettings{KeepRecentTokens: 1500}, 6000)
	if got.KeepRecentTokens != 1500 {
		t.Fatalf("KeepRecentTokens = %d, want 1500 (explicit, under clamp)", got.KeepRecentTokens)
	}
	// Default budget: default settings pass through unchanged.
	got = effectiveCompaction(CompactionSettings{}, 0)
	if got.KeepRecentTokens != defaultKeepRecentTokens {
		t.Fatalf("KeepRecentTokens = %d, want default %d", got.KeepRecentTokens, defaultKeepRecentTokens)
	}
}

// sinkTool accepts a large content argument and returns a tiny confirmation —
// the bulk stays in the CALL, mirroring a full-file write tool.
type sinkTool struct{ calls int }

func (s *sinkTool) Name() string { return "write_file" }
func (s *sinkTool) Schema() ToolSchema {
	return ToolSchema{Name: "write_file", Description: "write a file", Parameters: map[string]any{"type": "object"}}
}
func (s *sinkTool) Run(_ context.Context, _ string) (string, error) {
	s.calls++
	return fmt.Sprintf("wrote #%d", s.calls), nil
}

// bigArgsProvider emits tool calls whose ARGUMENTS carry the bulk (unique per
// call so no editing rule applies), and doubles as the compaction summarizer.
type bigArgsProvider struct {
	target    int
	calls     int
	Summaries int
	argSize   int
}

func (p *bigArgsProvider) Name() string        { return "bigargs" }
func (p *bigArgsProvider) SupportsTools() bool { return true }
func (p *bigArgsProvider) Chat(_ context.Context, req ChatRequest) (ChatResponse, error) {
	if len(req.Messages) > 0 && strings.HasPrefix(req.Messages[0].Content, "You are a context summarization") {
		p.Summaries++
		return AssistantText("## Goal\nKeep writing the file\n## Next Steps\n1. next milestone"), nil
	}
	p.calls++
	if p.calls >= p.target {
		return AssistantText("DONE"), nil
	}
	args := fmt.Sprintf(`{"path":"page.html","content":"%s-%d"}`, bigText(p.argSize), p.calls)
	return AssistantToolCall(fmt.Sprintf("w%d", p.calls), "write_file", args), nil
}
func (p *bigArgsProvider) Stream(ctx context.Context, req ChatRequest) (<-chan ChatDelta, error) {
	ch := make(chan ChatDelta, 4)
	go func() {
		defer close(ch)
		resp, _ := p.Chat(ctx, req)
		if resp.Message.Content != "" {
			ch <- ChatDelta{ContentDelta: resp.Message.Content}
		}
		for i := range resp.Message.ToolCalls {
			tc := resp.Message.ToolCalls[i]
			ch <- ChatDelta{ToolCall: &tc}
		}
		ch <- ChatDelta{Done: true, StopReason: resp.StopReason}
	}()
	return ch, nil
}

func TestCompactionFiresWhenBulkIsInToolCallArguments(t *testing.T) {
	prov := &bigArgsProvider{target: 20, argSize: 3000} // ~750 est. tokens per call

	limits := DefaultLimits()
	limits.MaxTurns = 60
	limits.MaxToolCalls = 80
	limits.MaxContextTokens = 4000 // deliberately below default KeepRecentTokens

	agent, err := New(Config{
		Provider: prov,
		Model:    "bigargs",
		Tools:    NewToolSet(&sinkTool{}),
		Policy:   NewAllowList("write_file"),
		Limits:   &limits,
		// No Compaction override: the default 20k keep-recent window must be
		// clamped to the 4k budget or this run wedges (the pre-fix behavior).
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	res, err := agent.Prompt(context.Background(), "build the page milestone by milestone")
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}

	if res.Final != "DONE" {
		t.Fatalf("run did not finish (stop=%q final=%q turns=%d)", res.StopReason, res.Final, res.Turns)
	}
	if prov.Summaries == 0 {
		t.Fatalf("compaction never summarized: big tool-call arguments under a small budget must compact (turns=%d)", res.Turns)
	}
	var sawSummary bool
	for _, m := range res.Messages {
		if m.Role == RoleSystem && strings.HasPrefix(m.Content, summaryMarker) {
			sawSummary = true
			break
		}
	}
	if !sawSummary {
		t.Fatal("no summary checkpoint in the final transcript")
	}
	// Bounded: 20 turns of ~750-token calls is ~15k tokens uncompacted; the
	// transcript must have been folded down, not merely marked.
	if est := estimateContextTokens(res.Messages); est > 3*limits.MaxContextTokens {
		t.Fatalf("transcript not bounded: estimated %d tokens against a %d budget", est, limits.MaxContextTokens)
	}
}

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

// The checkpoint ceiling, and what it is allowed to destroy.
//
// A checkpoint that overshoots its budget has to lose something. The question is
// what, and the answer has to be "whole facts", because the alternative is not a
// smaller checkpoint — it is a wrong one. This document is handed to the NEXT
// fold as the previous summary with instructions to carry it forward, so
// whatever survives the cut is what the run believes for the rest of its life.

// shardCheckpoint is a realistic over-budget checkpoint: a Done list of
// identifier-bearing facts between a Goal and a Next Steps section.
func shardCheckpoint(n int) string {
	var b strings.Builder
	b.WriteString("## Goal\nAudit every shard and report each shard's code.\n## Progress\n### Done\n")
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&b, "- [x] shard %d inspected, code FINDING-%d=SHARD%03dOK\n", i, i, i)
	}
	b.WriteString("## Next Steps\n1. Inspect the remaining shards\n2. Report every code\n")
	return b.String()
}

var shardCodeRe = regexp.MustCompile(`FINDING-(\d+)=(\S*)`)

// The regression. A byte-exact cut through the Done list produced
// "code FINDING-4=SHARD00" for a code that ends 002OK — a fact that is wrong
// while reading as complete, which the next fold then carries forward as truth.
func TestClampedCheckpointNeverCutsAFactInHalf(t *testing.T) {
	got := clampSummary(shardCheckpoint(24), 120)

	for _, m := range shardCodeRe.FindAllStringSubmatch(got, -1) {
		var n int
		if _, err := fmt.Sscanf(m[1], "%d", &n); err != nil {
			t.Fatalf("unparseable finding number %q", m[1])
		}
		want := fmt.Sprintf("SHARD%03dOK", n)
		if m[2] != want {
			t.Fatalf("finding %d survived the clamp as %q, want %q. A truncated identifier is "+
				"worse than a dropped one: this checkpoint is handed to the next fold as the "+
				"previous summary, so the run carries the mutilated value forward and reports "+
				"it as a finding, with nothing anywhere marking it damaged.\n---\n%s",
				n, m[2], want, got)
		}
	}
}

// What is lost has to be countable, because the next fold reads this document
// and a stated count is something it can act on ("20 items were dropped");
// a bare ellipsis is not.
func TestClampedCheckpointSaysHowMuchItDropped(t *testing.T) {
	got := clampSummary(shardCheckpoint(24), 120)
	if !regexp.MustCompile(`\[\d+ earlier lines dropped`).MatchString(got) {
		t.Fatalf("the clamp dropped content without saying how much:\n---\n%s", got)
	}
}

// The middle goes, not the end: Next Steps and Critical Context sit last in the
// format and are the sections a resuming agent actually reads.
func TestClampedCheckpointKeepsBothEnds(t *testing.T) {
	got := clampSummary(shardCheckpoint(24), 120)
	if !strings.HasPrefix(got, "## Goal") {
		t.Fatalf("the goal did not survive the clamp:\n---\n%s", got)
	}
	if !strings.Contains(got, "## Next Steps") || !strings.Contains(got, "2. Report every code") {
		t.Fatalf("the clamp ate Next Steps, the one section a resuming agent reads:\n---\n%s", got)
	}
}

// Whole-line cutting must not become an excuse to overshoot: an over-budget
// checkpoint is fed back into every window that follows it.
func TestClampedCheckpointRespectsTheCeiling(t *testing.T) {
	for _, tokens := range []int{40, 120, 200, 400} {
		got := clampSummary(shardCheckpoint(60), tokens)
		if max := tokens * bytesPerTokenEstimate; len(got) > max {
			t.Fatalf("clamp at %d tokens returned %d bytes, over the %d-byte ceiling",
				tokens, len(got), max)
		}
	}
}

// A checkpoint that fits is returned untouched — no marker, no reflow.
func TestClampSummaryLeavesAFittingCheckpointAlone(t *testing.T) {
	in := shardCheckpoint(3)
	got := clampSummary(in, 2048)
	if got != strings.TrimSpace(in) {
		t.Fatalf("a checkpoint inside its budget was rewritten:\n---\n%s", got)
	}
}

// Degenerate input still has to be bounded. One enormous line has no structure
// to respect, and an unbounded checkpoint would become the permanent floor of
// every window after it — so a byte cut is correct here, and the point of the
// test is that the ceiling still holds.
func TestClampSummaryBoundsAStructurelessReply(t *testing.T) {
	got := clampSummary(strings.Repeat("word ", 4000), 100)
	if len(got) > 100*bytesPerTokenEstimate {
		t.Fatalf("a single-line reply escaped the ceiling: %d bytes", len(got))
	}
}

// Clamping is applied once per fold, and the fold's own output is clamped again
// next time. Markers must not nest into a document made mostly of markers.
func TestClampingAnAlreadyClampedCheckpointDoesNotNestMarkers(t *testing.T) {
	got := clampSummary(shardCheckpoint(24), 120)
	for i := 0; i < 5; i++ {
		got = clampSummary(got, 120)
	}
	if n := strings.Count(got, "earlier lines dropped"); n > 1 {
		t.Fatalf("%d drop markers accumulated across folds:\n---\n%s", n, got)
	}
}

// The bug this file exists for: before the window capped it, the loop compacted
// at a fixed 200k regardless of the model. Point a run at anything smaller and
// compaction never fires — the transcript grows past what the model can hold and
// the provider rejects it, which no retry or escalation can rescue because the
// transcript is simply too big.
func TestBudgetIsCappedByTheModelWindow(t *testing.T) {
	// A 32k model with the default (300k) configured ceiling must compact well
	// inside 32k, not at 300k.
	got := effectiveBudget(defaultContextTokenBudget, 32_000)
	if got >= 32_000 {
		t.Fatalf("budget %d does not fit a 32000-token window", got)
	}
	if !shouldCompact(msgsOfTokens(33_000), got) {
		t.Fatal("a transcript larger than the model window did not trigger compaction")
	}
}

// The other direction: the configured ceiling is a ceiling. A 1M-token model
// must not license a 1M-token transcript when the operator asked for less —
// that is the cost control.
func TestConfiguredBudgetStillCapsALargeWindow(t *testing.T) {
	if got := effectiveBudget(120_000, 1_000_000); got != 120_000 {
		t.Fatalf("effectiveBudget(120000, 1000000) = %d, want the configured 120000", got)
	}
}

// An unknown window (0) is "nobody could tell us", not "a window of zero". It
// must leave the configured budget alone rather than collapsing it, or every
// self-hosted endpoint would compact on the first turn.
func TestUnknownWindowLeavesTheConfiguredBudgetAlone(t *testing.T) {
	if got := effectiveBudget(150_000, 0); got != 150_000 {
		t.Fatalf("effectiveBudget(150000, 0) = %d, want 150000", got)
	}
	if got := effectiveBudget(0, 0); got != defaultContextTokenBudget {
		t.Fatalf("effectiveBudget(0, 0) = %d, want the default %d", got, defaultContextTokenBudget)
	}
}

// A context window holds the answer as well as the prompt. Budgeting the whole
// window means the loop only compacts once the input alone has filled it, with
// no room left to reply in — so the cap must reserve output headroom.
func TestBudgetReservesRoomToAnswerIn(t *testing.T) {
	const window = 200_000
	got := effectiveBudget(defaultContextTokenBudget, window)
	if got > window-outputHeadroomTokens {
		t.Fatalf("budget %d leaves less than %d tokens to answer in", got, outputHeadroomTokens)
	}
}

// A window smaller than the headroom must not produce a zero or negative
// budget: shouldCompact reads 0 as "use the default 300k", so collapsing to
// zero would disable compaction on the *smallest* window — the exact inversion
// of what the cap is for.
func TestATinyWindowStillProducesAWorkingBudget(t *testing.T) {
	for _, window := range []int{1_000, 8_192, 16_385, 32_000} {
		got := effectiveBudget(defaultContextTokenBudget, window)
		if got <= 0 {
			t.Fatalf("window %d produced budget %d, which disables compaction", window, got)
		}
		if got >= window {
			t.Fatalf("window %d produced budget %d, which does not fit", window, got)
		}
	}
}

// The ladder is the reason the window lives on the rung. A run that escalates
// from a large-window model to a small-window one must re-derive its budget, or
// it carries the first rung's headroom onto a model that cannot hold it.
func TestEscalationRederivesTheBudgetForTheNewRung(t *testing.T) {
	big := effectiveBudget(defaultContextTokenBudget, 1_000_000)
	small := effectiveBudget(defaultContextTokenBudget, 32_000)
	if big <= small {
		t.Fatalf("a 1M-token rung (%d) must budget more than a 32k one (%d)", big, small)
	}
	// The transcript that was comfortable on the big rung must trigger
	// compaction on the small one.
	msgs := msgsOfTokens(40_000)
	if shouldCompact(msgs, big) {
		t.Fatal("a 40k transcript should be fine on a 1M-token model")
	}
	if !shouldCompact(msgs, small) {
		t.Fatal("a 40k transcript must compact on a 32k model")
	}
}

// End to end through a real run, and the pair below is the point: the SAME
// transcript must compact on a small-window model and not on an undeclared one.
// Without the wiring (Config → registry → agent → ladder → budget) every unit
// test above can pass while the loop still reads the raw configured ceiling, so
// only a run proves it.
func TestARunWithASmallWindowActuallyCompacts(t *testing.T) {
	if n := windowRunCompactions(t, 32_000); n == 0 {
		t.Fatal("a run on a 32k model never compacted; the window did not reach the loop")
	}
}

func TestTheSameRunWithNoDeclaredWindowDoesNotCompact(t *testing.T) {
	if n := windowRunCompactions(t, 0); n != 0 {
		t.Fatalf("a run with no declared window compacted %d times; the budget is not the configured one", n)
	}
}

// windowRunCompactions drives an identical bulky run at a given declared window
// and reports how many compactions the durable log recorded. Holding everything
// but the window fixed is what makes the pair above a controlled comparison
// rather than two independent assertions.
func windowRunCompactions(t *testing.T, window int) int {
	t.Helper()

	work := bulkTool{size: 60_000}
	call := func(id string) ChatResponse {
		return ChatResponse{Message: Message{
			Role:      RoleAssistant,
			ToolCalls: []ToolCall{{ID: id, Name: "work", Arguments: "{}"}},
		}}
	}
	fp := &FauxProvider{Responses: []ChatResponse{
		call("c1"), call("c2"), call("c3"),
		{Message: Message{Role: RoleAssistant, Content: "done"}},
	}}

	store := NewMemorySessionStore()
	agent, err := New(Config{
		Provider:      fp,
		Model:         "m",
		ContextWindow: window,
		Tools:         NewToolSet(work),
		Policy:        NewAllowList("work"),
		Session:       store,
		SessionID:     "s1",
		Limits: &Limits{
			MaxTurns: 8, MaxToolCalls: 8,
			MaxToolResultLen: 64 * 1024,
			MaxContextTokens: defaultContextTokenBudget,
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := agent.Prompt(context.Background(), "go"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}

	log, err := store.Log(context.Background(), "s1")
	if err != nil {
		t.Fatalf("Log: %v", err)
	}
	n := 0
	for _, e := range log {
		if e.Kind == EntryCompaction && e.Final {
			n++
		}
	}
	return n
}

// bulkTool returns a payload large enough that a few calls cross a small
// window, so compaction is exercised for real rather than merely configured.
type bulkTool struct{ size int }

func (bulkTool) Name() string { return "work" }
func (bulkTool) Schema() ToolSchema {
	return ToolSchema{Name: "work", Description: "does work", Parameters: map[string]any{"type": "object"}}
}
func (b bulkTool) Run(context.Context, string) (string, error) {
	return strings.Repeat("x", b.size), nil
}

// msgsOfTokens builds a transcript whose byte estimate is roughly n tokens, for
// driving shouldCompact without a provider.
func msgsOfTokens(n int) []Message {
	return []Message{{Role: RoleUser, Content: strings.Repeat("x", n*4)}}
}

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

	out, _ := compactWithSummary(context.Background(), prov, "m", msgs, CompactionSettings{KeepRecentTokens: 3000})

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
		msgs, _ = compactWithSummary(context.Background(), prov, "m", msgs, CompactionSettings{KeepRecentTokens: 3000})
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

type prunerCompactor struct{ compacted *int }

func (prunerCompactor) Name() string { return "pruner" }

func (prunerCompactor) ShouldCompact(messages []Message, _ int) bool {
	return len(messages) > 4
}

func (p prunerCompactor) Compact(_ context.Context, req CompactionRequest) (CompactionResult, error) {
	*p.compacted++
	out := make([]Message, 0, len(req.Messages))
	for i, m := range req.Messages {
		if m.Role == RoleTool && i < len(req.Messages)-2 {
			out = append(out, Message{Role: RoleTool, ToolCallID: m.ToolCallID, Content: "[pruned]"})
			continue
		}
		out = append(out, m)
	}
	return CompactionResult{Messages: out}, nil
}

func TestCustomCompactionStrategyReplacesTheBuiltIn(t *testing.T) {
	calls := 0
	faux := NewFauxProvider(
		AssistantToolCall("c1", "echo", `{"v":"1"}`),
		AssistantToolCall("c2", "echo", `{"v":"2"}`),
		AssistantText("done"),
	)
	agent, err := New(Config{
		Provider:  faux,
		Model:     "test",
		Tools:     NewToolSet(&echoToolCompaction{}),
		Policy:    NewAllowList("echo"),
		Compactor: prunerCompactor{compacted: &calls},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := agent.Prompt(context.Background(), "go"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if calls == 0 {
		t.Fatal("the custom compactor never ran — the loop is still calling its own strategy")
	}
	for _, req := range faux.Recorded {
		for _, m := range req.Messages {
			if strings.Contains(m.Content, "structured context checkpoint") {
				t.Fatal("the built-in summarizer ran despite a replacement being installed")
			}
		}
	}
	if desc := agent.Describe(); !strings.Contains(desc, "compactor:") || !strings.Contains(desc, "pruner") {
		t.Fatalf("Describe() does not report the installed strategy:\n%s", desc)
	}
}

func TestTwoCompactionStrategiesIsABuildError(t *testing.T) {
	calls := 0
	_, err := Build(
		ModelPlugin{Provider: NewFauxProvider(AssistantText("ok")), Model: "m"},
		CompactionPlugin{Strategy: prunerCompactor{compacted: &calls}},
		CompactionPlugin{Strategy: prunerCompactor{compacted: &calls}},
	)
	if err == nil {
		t.Fatal("two compaction strategies composed without complaint")
	}
	if !strings.Contains(err.Error(), "compact") {
		t.Fatalf("error should name the contested seam, got: %v", err)
	}
}

type echoToolCompaction struct{}

func (*echoToolCompaction) Name() string { return "echo" }
func (*echoToolCompaction) Schema() ToolSchema {
	return ToolSchema{Name: "echo", Description: "echo", Parameters: map[string]any{"type": "object"}}
}
func (*echoToolCompaction) Run(context.Context, string) (string, error) {
	return strings.Repeat("payload ", 64), nil
}

// TestDefaultCompactionStrategyStaysInstalled pins the other half: a
// composition that says nothing about compaction still gets the built-in, so
// extracting the seam did not quietly turn compaction off.
func TestDefaultCompactionStrategyStaysInstalled(t *testing.T) {
	agent, err := New(Config{
		Provider: NewFauxProvider(AssistantText("ok")),
		Model:    "test",
		Policy:   DenyAll{},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if desc := agent.Describe(); !strings.Contains(desc, "summary") {
		t.Fatalf("default compactor missing:\n%s", desc)
	}
}
