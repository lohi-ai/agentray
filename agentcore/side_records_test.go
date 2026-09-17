package agentcore

import (
	"context"
	"strings"
	"sync"
	"testing"
)

// TestSteerIsDurableAndDelivered verifies the durable inbox contract: Steer
// writes an EntryInbox side record immediately (intent), the loop delivers it
// as an EntryMessage on the next turn (effect), and settles it with an
// EntryInboxDone chain entry (settlement) so a later resume never re-delivers
// it.
func TestSteerIsDurableAndDelivered(t *testing.T) {
	ctx := context.Background()
	store := newMemSessionStore()
	faux := NewFauxProvider(
		AssistantToolCall("c1", "echo", `{}`),
		AssistantText("saw the steer"),
	)
	agent, err := New(Config{
		Provider:  faux,
		Model:     "test",
		Tools:     NewToolSet(&echoTool{name: "echo"}),
		Policy:    NewAllowList("echo"),
		Session:   store,
		SessionID: "s-inbox",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Queue the steer before the run starts (same as queuing it mid-turn).
	if err := agent.Steer(ctx, Message{Role: RoleUser, Content: "focus on X"}); err != nil {
		t.Fatalf("Steer: %v", err)
	}

	res, err := agent.Prompt(ctx, "hello")
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if res.Final != "saw the steer" {
		t.Fatalf("final = %q", res.Final)
	}

	// The steer was delivered into the conversation before the second turn.
	var sawSteer bool
	for _, m := range res.Messages {
		if m.Role == RoleUser && m.Content == "focus on X" && m.Directive {
			sawSteer = true
		}
	}
	if !sawSteer {
		t.Fatal("steer was not delivered into the message history")
	}

	// The log carries all three steps of the op: intent -> message -> settlement.
	log, _ := store.Log(ctx, "s-inbox")
	var hasInbox, hasDone bool
	var inboxID string
	for _, e := range log {
		switch e.Kind {
		case EntryInbox:
			hasInbox = true
			inboxID = e.ID
		case EntryInboxDone:
			if e.Target == inboxID && inboxID != "" {
				hasDone = true
			}
		}
	}
	if !hasInbox {
		t.Fatal("EntryInbox was not written")
	}
	if !hasDone {
		t.Fatalf("EntryInboxDone was not written for inbox entry %q", inboxID)
	}

	// Re-reducing the log reports the inbox as settled (rs.Inbox is empty).
	rs := ReduceSession(log)
	if len(rs.Inbox) != 0 {
		t.Fatalf("reduced inbox must be empty after settlement, got %+v", rs.Inbox)
	}
}

// TestResumeDeliversPendingInbox verifies an inbox entry queued before a crash
// survives: the resumed run reads it from the log, delivers it on turn 1, and
// settles it.
func TestResumeDeliversPendingInbox(t *testing.T) {
	ctx := context.Background()
	store := newMemSessionStore()
	// Seed a crashed log: user prompt, an unfinished assistant turn, and an
	// EntryInbox that was queued before the crash but never settled.
	for _, e := range []SessionEntry{
		{Kind: EntryMessage, Message: &Message{Role: RoleUser, Content: "start"}},
		{Kind: EntryMessage, Message: &Message{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "c1", Name: "echo", Arguments: "{}"}}}},
		{Kind: EntryInbox, ID: "inbox-1", Lane: InboxSteer, Message: &Message{Role: RoleUser, Content: "queued before crash"}},
	} {
		if err := store.Append(ctx, "r-inbox", e); err != nil {
			t.Fatal(err)
		}
	}

	// Recovery sees the pending inbox.
	log, _ := store.Log(ctx, "r-inbox")
	plan := RecoverSession(log, NewToolSet(&echoTool{name: "echo"}), RecoveryMarkInterrupted)
	if len(plan.Inbox) != 1 || plan.Inbox[0].Message.Content != "queued before crash" {
		t.Fatalf("recovered plan.Inbox = %+v, want the queued message", plan.Inbox)
	}

	// Resuming the agent delivers the queued message.
	faux := NewFauxProvider(AssistantText("resumed and handled inbox"))
	agent, err := New(Config{
		Provider:      faux,
		Model:         "test",
		Tools:         NewToolSet(&echoTool{name: "echo"}),
		Policy:        NewAllowList("echo"),
		Session:       store,
		SessionID:     "r-inbox",
		ResumeSession: true,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	res, err := agent.Prompt(ctx, "resume")
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if res.Final != "resumed and handled inbox" {
		t.Fatalf("final = %q", res.Final)
	}

	// Check settlement in the store.
	logAfter, _ := store.Log(ctx, "r-inbox")
	rs := ReduceSession(logAfter)
	if len(rs.Inbox) != 0 {
		t.Fatalf("inbox must be settled after resume, got %+v", rs.Inbox)
	}
}

// TestStreamingWritesPartialFrames verifies streamed turns write throttled
// EntryAssistantFrame side records, that buildChain skips them (they do not
// fork the chain), and that a trailing frame is recovered as a draft.
func TestStreamingWritesPartialFrames(t *testing.T) {
	ctx := context.Background()
	store := newMemSessionStore()
	// An assistant turn with enough text to cross the frame throttle, then a
	// crash (no leaf).
	faux := NewFauxProvider(
		AssistantText("first chunk of answer and then some more words to make it long enough"),
	)
	agent, err := New(Config{
		Provider:  faux,
		Model:     "test",
		Session:   store,
		SessionID: "s-frames",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// Run streamed so streamTurn is exercised.
	_, err = agent.PromptStream(ctx, "hi", func(StreamEvent) {})
	if err != nil {
		t.Fatalf("PromptStream: %v", err)
	}

	log, _ := store.Log(ctx, "s-frames")
	var frames int
	for _, e := range log {
		if e.Kind == EntryAssistantFrame {
			frames++
		}
	}
	if frames == 0 {
		t.Fatal("no EntryAssistantFrame records were written during streaming")
	}

	// Side records are NOT in the tree.
	nodes := SessionTree(log)
	for _, n := range nodes {
		if n.Entry.Kind == EntryAssistantFrame {
			t.Fatalf("SessionTree must not include side records, found frame %q", n.ID)
		}
	}

	// A settled turn's frames are superseded by its assistant message, so Draft is empty.
	rs := ReduceSession(log)
	if rs.Draft != "" {
		t.Fatalf("settled run must have no draft, got %q", rs.Draft)
	}

	// Simulate a crash mid-generation: append a trailing frame after a user prompt.
	crashedStore := newMemSessionStore()
	for _, e := range []SessionEntry{
		{Kind: EntryMessage, Message: &Message{Role: RoleUser, Content: "write a poem"}},
		{Kind: EntryAssistantFrame, Turn: 1, Content: "the rose was red"},
	} {
		_ = crashedStore.Append(ctx, "crashed", e)
	}
	cLog, _ := crashedStore.Log(ctx, "crashed")
	cPlan := RecoverSession(cLog, nil, RecoveryMarkInterrupted)
	if cPlan.Draft != "the rose was red" {
		t.Fatalf("recovered draft = %q, want 'the rose was red'", cPlan.Draft)
	}
}

// progressTool is a StreamingTool that emits partial output before returning.
type progressTool struct {
	called int
}

func (p *progressTool) Name() string { return "progress_tool" }
func (p *progressTool) Schema() ToolSchema {
	return ToolSchema{Name: "progress_tool", Description: "streaming tool"}
}
func (p *progressTool) Run(context.Context, string) (string, error) {
	return "authoritative final", nil
}
func (p *progressTool) RunStreaming(_ context.Context, _ string, emit func(string)) (string, error) {
	p.called++
	emit("fetched 50/100 records")
	emit("fetched 100/100 records")
	return "authoritative final", nil
}

// TestToolProgressRecordedAndRecovered verifies streaming tool partials write
// EntryToolProgress side records, and that an interrupted call's note carries
// its last reported progress.
func TestToolProgressRecordedAndRecovered(t *testing.T) {
	ctx := context.Background()
	store := newMemSessionStore()
	pt := &progressTool{}
	faux := NewFauxProvider(
		AssistantToolCall("c1", "progress_tool", `{}`),
		AssistantText("done"),
	)
	agent, err := New(Config{
		Provider:  faux,
		Model:     "test",
		Tools:     NewToolSet(pt),
		Policy:    NewAllowList("progress_tool"),
		Session:   store,
		SessionID: "s-progress",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, err = agent.PromptStream(ctx, "fetch", func(StreamEvent) {})
	if err != nil {
		t.Fatalf("PromptStream: %v", err)
	}

	log, _ := store.Log(ctx, "s-progress")
	var progressRecords int
	for _, e := range log {
		if e.Kind == EntryToolProgress {
			progressRecords++
			if e.CallID != "c1" {
				t.Fatalf("progress CallID = %q, want c1", e.CallID)
			}
		}
	}
	if progressRecords == 0 {
		t.Fatal("no EntryToolProgress records were written")
	}

	// Simulate an interrupted run whose tool reported progress before the crash.
	crashedStore := newMemSessionStore()
	for _, e := range []SessionEntry{
		{Kind: EntryMessage, Message: &Message{Role: RoleUser, Content: "fetch"}},
		{Kind: EntryMessage, Message: &Message{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "c1", Name: "progress_tool", Arguments: "{}"}}}},
		{Kind: EntryToolProgress, Turn: 1, CallID: "c1", Content: "fetched 50/100"},
	} {
		_ = crashedStore.Append(ctx, "crashed-tool", e)
	}
	// progressTool does not implement RetrySafeTool, so it is dropped and gets
	// an interrupted note.
	resumeFaux := NewFauxProvider(AssistantText("recovered"))
	resumedAgent, err := New(Config{
		Provider:      resumeFaux,
		Model:         "test",
		Tools:         NewToolSet(&echoTool{name: "progress_tool"}), // plain, not retry-safe
		Policy:        NewAllowList("progress_tool"),
		Session:       crashedStore,
		SessionID:     "crashed-tool",
		ResumeSession: true,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	res, err := resumedAgent.Prompt(ctx, "resume")
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	// The interrupted note carries the last reported progress.
	var sawProgressInNote bool
	for _, m := range res.Messages {
		if m.Role == RoleTool && m.ToolCallID == "c1" && strings.Contains(m.Content, "Last reported progress: fetched 50/100") {
			sawProgressInNote = true
		}
	}
	if !sawProgressInNote {
		t.Fatalf("interrupted note did not carry progress: %+v", res.Messages)
	}
}

// TestFollowUpIsDurableAndDelivered verifies FollowUp queues work that drains
// after the agent would stop, restarts the loop, and settles its inbox entry.
func TestFollowUpIsDurableAndDelivered(t *testing.T) {
	ctx := context.Background()
	store := newMemSessionStore()
	faux := NewFauxProvider(
		AssistantText("first answer"),
		AssistantText("second answer after follow-up"),
	)
	agent, err := New(Config{
		Provider:  faux,
		Model:     "test",
		Session:   store,
		SessionID: "s-follow-durable",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Queue the follow-up before the run starts.
	if err := agent.FollowUp(ctx, Message{Role: RoleUser, Content: "also do Y"}); err != nil {
		t.Fatalf("FollowUp: %v", err)
	}

	res, err := agent.Prompt(ctx, "do X")
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if res.Final != "second answer after follow-up" {
		t.Fatalf("final = %q", res.Final)
	}
	if res.Turns != 2 {
		t.Fatalf("turns = %d, want 2", res.Turns)
	}

	// Check settlement.
	log, _ := store.Log(ctx, "s-follow-durable")
	rs := ReduceSession(log)
	if len(rs.Inbox) != 0 {
		t.Fatalf("follow-up inbox must be settled, got %+v", rs.Inbox)
	}
}

// TestSideRecordsConcurrentWrites ensures direct appends from tool goroutines
// and the stream frame writer do not race with the save-point flush.
func TestSideRecordsConcurrentWrites(t *testing.T) {
	ctx := context.Background()
	store := newMemSessionStore()
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_ = store.Append(ctx, "concurrent", SessionEntry{
				Kind:    EntryToolProgress,
				CallID:  "c",
				Content: "progress",
			})
		}(i)
	}
	wg.Wait()
	log, _ := store.Log(ctx, "concurrent")
	if len(log) != 20 {
		t.Fatalf("expected 20 entries, got %d", len(log))
	}
}

// TestFoldSeesFramesBehindAFlushedTurn covers the ordering a graceful
// interruption produces: mid-turn side records land first, then the deferred
// flush commits the turn's buffered chain entries AFTER them. A tail scan that
// stops at the first chain entry loses the draft; the fold must bound the
// in-flight window by the last SETTLING entry (assistant/tool message), not
// the last chain entry.
func TestFoldSeesFramesBehindAFlushedTurn(t *testing.T) {
	ctx := context.Background()
	store := newMemSessionStore()
	user := Message{Role: RoleUser, Content: "go"}
	steer := Message{Role: RoleUser, Content: "mid-turn steer"}
	for _, e := range []SessionEntry{
		{Kind: EntryMessage, Turn: 1, Message: &user},
		// Turn 2 streams a draft and tool progress, delivers a steer (buffered
		// chain entries), then is interrupted — the deferred flush commits the
		// buffered entries AFTER the side records.
		{Kind: EntryAssistantFrame, Turn: 2, Content: "half-written answer"},
		{Kind: EntryToolProgress, Turn: 2, CallID: "c1", Content: "tool got this far"},
		{Kind: EntryMessage, Turn: 2, Message: &steer},
		{Kind: EntryInboxDone, Turn: 2, Target: "inbox-9"},
	} {
		if err := store.Append(ctx, "s", e); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	log, _ := store.Log(ctx, "s")
	rs := ReduceSession(log)
	if rs.Draft != "half-written answer" {
		t.Fatalf("draft behind flushed chain entries must be recovered, got %q", rs.Draft)
	}
	if rs.ToolProgress["c1"] != "tool got this far" {
		t.Fatalf("tool progress behind flushed chain entries must be recovered, got %v", rs.ToolProgress)
	}
}

// TestInboxDoesNotLeakAcrossRewind covers both directions of branch leakage:
// an intent settled on an abandoned branch is pending again after the rewind
// (its delivered message is gone), and an intent queued on the abandoned
// branch is not delivered into the new one.
func TestInboxDoesNotLeakAcrossRewind(t *testing.T) {
	ctx := context.Background()
	store := newMemSessionStore()
	q := Message{Role: RoleUser, Content: "question"}
	a1 := Message{Role: RoleAssistant, Content: "first answer"}
	steer := Message{Role: RoleUser, Content: "delivered steer"}
	a2 := Message{Role: RoleAssistant, Content: "steered answer"}
	stale := Message{Role: RoleUser, Content: "queued on the old branch"}
	entries := []SessionEntry{
		{Kind: EntryMessage, Turn: 1, Message: &q},                               // seq 0
		{Kind: EntryMessage, Turn: 1, Message: &a1},                              // seq 1
		{Kind: EntryInbox, ID: "i-delivered", Lane: InboxSteer, Message: &steer}, // seq 2, anchored to seq 1
		{Kind: EntryMessage, Turn: 2, Message: &steer},                           // seq 3 (delivered)
		{Kind: EntryInboxDone, Turn: 2, Target: "i-delivered"},                   // seq 4
		{Kind: EntryMessage, Turn: 2, Message: &a2},                              // seq 5
		{Kind: EntryInbox, ID: "i-stale", Lane: InboxSteer, Message: &stale},     // seq 6, anchored to seq 5
	}
	for _, e := range entries {
		if err := store.Append(ctx, "s", e); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	// Rewind to just after the first answer: the delivered steer, its
	// settlement, the second answer, and the stale intent all sit on the
	// abandoned branch.
	if _, err := Rewind(ctx, store, "s", "#1", BranchOptions{}); err != nil {
		t.Fatalf("Rewind: %v", err)
	}
	log, _ := store.Log(ctx, "s")
	rs := ReduceSession(log)

	var ids []string
	for _, it := range rs.Inbox {
		ids = append(ids, it.ID)
	}
	if len(ids) != 1 || ids[0] != "i-delivered" {
		t.Fatalf("after rewind the rewound-away delivery must be pending again and the stale intent dropped, got %v", ids)
	}
}
