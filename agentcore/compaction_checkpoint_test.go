package agentcore

import (
	"context"
	"strings"
	"testing"
)

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
