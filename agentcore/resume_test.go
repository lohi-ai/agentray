package agentcore

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// seedCrashedLog writes a minimal crashed-run log: the user task, then an
// assistant turn whose tool call never got a result (the process died mid-tool).
func seedCrashedLog(t *testing.T, store SessionStore, sessionID, callID, toolName string) {
	t.Helper()
	ctx := context.Background()
	for _, e := range []SessionEntry{
		{Kind: EntryMessage, Message: &Message{Role: RoleUser, Content: "count the beans"}},
		{Kind: EntryMessage, Message: &Message{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: callID, Name: toolName, Arguments: "{}"}}}},
	} {
		if err := store.Append(ctx, sessionID, e); err != nil {
			t.Fatal(err)
		}
	}
}

// TestResumeSessionReplaysRetrySafeCall pins the drive-level resume contract:
// a dangling call whose tool is retry-safe is re-issued before the first model
// turn, with its ORIGINAL call id, and its real result — not an interrupted
// note — reaches the model and the durable log.
func TestResumeSessionReplaysRetrySafeCall(t *testing.T) {
	ctx := context.Background()
	store := newMemSessionStore()
	seedCrashedLog(t, store, "r1", "c9", "safe_read")

	provider := NewFauxProvider(AssistantText("done after replay"))
	agent, err := New(Config{
		Provider:      provider,
		Model:         "test",
		Tools:         NewToolSet(retrySafeProbe{}),
		Policy:        NewAllowList("safe_read"),
		Session:       store,
		SessionID:     "r1",
		ResumeSession: true,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	res, err := agent.Prompt(ctx, "resume")
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if res.Final != "done after replay" {
		t.Fatalf("final = %q", res.Final)
	}

	// The model's first request saw the replayed result, not an interrupted note.
	var sawResult, sawNote bool
	for _, m := range provider.Recorded[0].Messages {
		if m.Role == RoleTool && m.ToolCallID == "c9" {
			if m.Content == "ok" {
				sawResult = true
			}
			if strings.Contains(m.Content, "[interrupted:") {
				sawNote = true
			}
		}
	}
	if !sawResult || sawNote {
		t.Fatalf("replay must feed the real result (result=%v note=%v): %+v", sawResult, sawNote, provider.Recorded[0].Messages)
	}
	// The replay is traced like a live execution.
	if len(res.Tools) == 0 || res.Tools[0].Tool != "safe_read" {
		t.Fatalf("replayed call missing from traces: %+v", res.Tools)
	}

	// The log completes, holds the replayed result exactly once, and does not
	// duplicate the recovered history.
	log, _ := store.Log(ctx, "r1")
	rs := ReduceSession(log)
	if !rs.Completed {
		t.Fatal("resumed log must reduce Completed")
	}
	results, seeds := 0, 0
	for _, e := range log {
		if e.Kind != EntryMessage || e.Message == nil {
			continue
		}
		if e.Message.Role == RoleTool && e.Message.ToolCallID == "c9" {
			results++
		}
		if e.Message.Role == RoleUser && e.Message.Content == "count the beans" {
			seeds++
		}
	}
	if results != 1 || seeds != 1 {
		t.Fatalf("log shape wrong: results=%d seeds=%d (want 1/1)", results, seeds)
	}
}

// TestResumeSessionClosesUnsafeCall verifies a dangling call whose tool is NOT
// retry-safe is never re-run: it is closed with an interrupted note that both
// reaches the model and is persisted, so the model decides what to do next.
func TestResumeSessionClosesUnsafeCall(t *testing.T) {
	ctx := context.Background()
	store := newMemSessionStore()
	seedCrashedLog(t, store, "r2", "c9", "noop")

	tool := noopTool{}
	provider := NewFauxProvider(AssistantText("acknowledged the interruption"))
	agent, err := New(Config{
		Provider:      provider,
		Model:         "test",
		Tools:         NewToolSet(tool),
		Policy:        NewAllowList("noop"),
		Session:       store,
		SessionID:     "r2",
		ResumeSession: true,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := agent.Prompt(ctx, "resume"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}

	var sawNote bool
	for _, m := range provider.Recorded[0].Messages {
		if m.Role == RoleTool && m.ToolCallID == "c9" && strings.Contains(m.Content, "[interrupted:") {
			sawNote = true
		}
	}
	if !sawNote {
		t.Fatalf("non-retry-safe dangling call must be closed with a note: %+v", provider.Recorded[0].Messages)
	}
	// The note is durable: a second resume reduces a transcript with no dangling call.
	log, _ := store.Log(ctx, "r2")
	plan := RecoverSession(log, nil, RecoveryMarkInterrupted)
	if len(plan.DroppedCalls) != 0 || len(plan.RetryCalls) != 0 {
		t.Fatalf("persisted note must satisfy the call: %+v", plan)
	}
}

// TestResumeSessionReattachesCompletedLog verifies a log that already reached
// its leaf returns the recorded final answer without a single provider call.
func TestResumeSessionReattachesCompletedLog(t *testing.T) {
	ctx := context.Background()
	store := newMemSessionStore()
	for _, e := range []SessionEntry{
		{Kind: EntryMessage, Message: &Message{Role: RoleUser, Content: "count the beans"}},
		{Kind: EntryMessage, Message: &Message{Role: RoleAssistant, Content: "42 beans"}},
		{Kind: EntryLeaf},
	} {
		if err := store.Append(ctx, "r3", e); err != nil {
			t.Fatal(err)
		}
	}

	provider := NewFauxProvider(AssistantText("should never be produced"))
	agent, err := New(Config{
		Provider:      provider,
		Model:         "test",
		Session:       store,
		SessionID:     "r3",
		ResumeSession: true,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	res, err := agent.Prompt(ctx, "resume")
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if res.Final != "42 beans" {
		t.Fatalf("reattach must return the recorded answer, got %q", res.Final)
	}
	if res.StopReason != "reattached" {
		t.Fatalf("stop reason = %q", res.StopReason)
	}
	if len(provider.Recorded) != 0 {
		t.Fatalf("reattach must not call the provider (%d calls)", len(provider.Recorded))
	}
}

// TestResumeSessionEmptyLogRunsFresh verifies ResumeSession degrades cleanly:
// with nothing in the log the run behaves exactly like a fresh one, persisting
// its seed exactly once.
func TestResumeSessionEmptyLogRunsFresh(t *testing.T) {
	ctx := context.Background()
	store := newMemSessionStore()
	agent, err := New(Config{
		Provider:      NewFauxProvider(AssistantText("fresh answer")),
		Model:         "test",
		Session:       store,
		SessionID:     "r4",
		ResumeSession: true,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	res, err := agent.Prompt(ctx, "hello")
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if res.Final != "fresh answer" {
		t.Fatalf("final = %q", res.Final)
	}
	log, _ := store.Log(ctx, "r4")
	seeds := 0
	for _, e := range log {
		if e.Kind == EntryMessage && e.Message != nil && e.Message.Role == RoleUser && e.Message.Content == "hello" {
			seeds++
		}
	}
	if seeds != 1 {
		t.Fatalf("seed persisted %d times, want 1", seeds)
	}
}

// TestResumeRestoresActiveToolsAndModel verifies the durable run state beyond
// the transcript is honored on resume: a crashed run that had swapped its tool
// set (EntryActiveToolsChange) and escalated to another model
// (EntryModelChange) comes back on THAT set and THAT rung — not the registry
// and primary model it was configured with.
func TestResumeRestoresActiveToolsAndModel(t *testing.T) {
	ctx := context.Background()
	store := newMemSessionStore()
	// A crashed run: user task, a mid-run tool swap to just "b", an escalation
	// to model "big", then an assistant turn that died mid-work.
	for _, e := range []SessionEntry{
		{Kind: EntryMessage, Message: &Message{Role: RoleUser, Content: "count the beans"}},
		{Kind: EntryActiveToolsChange, Tools: []string{"b"}},
		{Kind: EntryModelChange, Model: "big"},
		{Kind: EntryMessage, Message: &Message{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "c1", Name: "b", Arguments: "{}"}}}},
	} {
		if err := store.Append(ctx, "r5", e); err != nil {
			t.Fatal(err)
		}
	}

	primary := NewFauxProvider(AssistantText("primary must not be called"))
	escalated := NewFauxProvider(AssistantText("resumed on the recorded rung"))
	agent, err := New(Config{
		Provider:      primary,
		Model:         "small",
		Escalation:    []ModelRung{{Provider: escalated, Model: "big"}},
		Tools:         NewToolSet(&echoTool{name: "a"}, &echoTool{name: "b"}),
		Policy:        NewAllowList("a", "b"),
		Session:       store,
		SessionID:     "r5",
		ResumeSession: true,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	res, err := agent.Prompt(ctx, "resume")
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if res.Final != "resumed on the recorded rung" {
		t.Fatalf("final = %q", res.Final)
	}
	if len(primary.Recorded) != 0 {
		t.Fatal("resume must start on the recorded rung, not re-pay the failed primary")
	}
	if len(escalated.Recorded) != 1 {
		t.Fatalf("escalated provider calls = %d, want 1", len(escalated.Recorded))
	}
	// The recorded active set is advertised, not the full registry.
	req := escalated.Recorded[0]
	if len(req.Tools) != 1 || req.Tools[0].Name != "b" {
		t.Fatalf("resumed run must advertise the recorded tool set [b], got %+v", req.Tools)
	}
}

// TestPrepareNextTurnToolSwapIsDurable verifies a save-point tool swap writes
// EntryActiveToolsChange — the entry a resume reads to rebuild the set — and
// that a hook returning the same set every turn writes nothing.
func TestPrepareNextTurnToolSwapIsDurable(t *testing.T) {
	ctx := context.Background()
	store := newMemSessionStore()
	narrow := NewToolSet(&echoTool{name: "b"})
	faux := NewFauxProvider(
		AssistantToolCall("c1", "a", `{}`),
		AssistantText("done"),
	)
	agent, err := New(Config{
		Provider:  faux,
		Model:     "test",
		Tools:     NewToolSet(&echoTool{name: "a"}, &echoTool{name: "b"}),
		Policy:    NewAllowList("a", "b"),
		Session:   store,
		SessionID: "r6",
		PrepareNextTurn: func(_ context.Context, s TurnState) TurnState {
			s.Tools = narrow
			return s
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := agent.Prompt(ctx, "go"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	// Turn 2 advertised only the swapped-in set.
	if len(faux.Recorded) != 2 {
		t.Fatalf("provider calls = %d, want 2", len(faux.Recorded))
	}
	if got := faux.Recorded[1].Tools; len(got) != 1 || got[0].Name != "b" {
		t.Fatalf("turn 2 must advertise only [b], got %+v", got)
	}
	// The swap is in the durable log exactly once.
	log, _ := store.Log(ctx, "r6")
	changes := 0
	for _, e := range log {
		if e.Kind == EntryActiveToolsChange {
			changes++
			if len(e.Tools) != 1 || e.Tools[0] != "b" {
				t.Fatalf("recorded tool set = %v, want [b]", e.Tools)
			}
		}
	}
	if changes != 1 {
		t.Fatalf("EntryActiveToolsChange written %d times, want 1", changes)
	}
}

// The windowed resume rests on one claim: reading a SUFFIX of a log gives the
// same answer as reading all of it. Every test here attacks that claim, because
// a window that folds to a different state is not a slower resume — it is a run
// that silently forgets work it already did.

// logBuilder writes a session log the way the loop does, so the fixtures below
// exercise the real entry shapes rather than a convenient approximation.
type logBuilder struct {
	entries []SessionEntry
	seq     int
	state   CheckpointState
}

func (b *logBuilder) add(e SessionEntry) {
	b.seq++
	e.Seq = b.seq
	b.entries = append(b.entries, e)
}

func (b *logBuilder) message(turn int, role Role, content string) {
	b.add(SessionEntry{Kind: EntryMessage, Turn: turn, Message: &Message{Role: role, Content: content}})
}

func (b *logBuilder) goal(g string) {
	b.add(SessionEntry{Kind: EntryGoal, Goal: g})
	b.state.Goal = g
}

func (b *logBuilder) modelChange(turn int, model string) {
	b.add(SessionEntry{Kind: EntryModelChange, Turn: turn, Model: model})
	b.state.Model = model
}

func (b *logBuilder) disable(turn int, tool string) {
	b.add(SessionEntry{Kind: EntryToolDisabled, Turn: turn, Tool: tool})
	b.state.DisabledTools = append(b.state.DisabledTools, tool)
}

// compact writes the durable bracket the loop writes: a start entry, then a
// completion carrying the retained transcript and the mirrored run state.
func (b *logBuilder) compact(turn int, retained []Message) {
	b.add(SessionEntry{Kind: EntryCompaction, Turn: turn})
	b.add(SessionEntry{
		Kind: EntryCompaction, Turn: turn, Final: true,
		Retained: retained,
		State:    b.state.clone(),
	})
}

// checkpointSeq is the seq the store would hand LoadResumeLog.
func (b *logBuilder) checkpointSeq() int {
	seq := 0
	for _, e := range b.entries {
		if e.Kind == EntryCompaction && e.Final && e.Retained != nil && e.State != nil {
			seq = e.Seq
		}
	}
	return seq
}

// window is the suffix beginning at the newest checkpoint.
func (b *logBuilder) window() []SessionEntry {
	seq := b.checkpointSeq()
	var out []SessionEntry
	for _, e := range b.entries {
		if e.Seq >= seq {
			out = append(out, e)
		}
	}
	return out
}

// longRun builds a log shaped like a run that compacted several times: a goal,
// escalation, a disabled tool, and message traffic between checkpoints.
func longRun() *logBuilder {
	b := &logBuilder{}
	b.message(1, RoleUser, "audit every shard")
	b.goal("all shards reconciled")
	for turn := 1; turn <= 30; turn++ {
		b.message(turn, RoleAssistant, fmt.Sprintf("working on shard %d", turn))
		b.message(turn, RoleTool, fmt.Sprintf("shard %d ok", turn))
		switch turn {
		case 8:
			b.modelChange(turn, "claude-opus-5")
		case 12:
			b.disable(turn, "flaky_probe")
		case 21:
			b.disable(turn, "slow_probe")
		}
		if turn%10 == 0 {
			b.compact(turn, []Message{
				{Role: RoleSystem, Content: fmt.Sprintf("[checkpoint] shards 1-%d done", turn)},
				{Role: RoleAssistant, Content: fmt.Sprintf("continuing from shard %d", turn)},
			})
		}
	}
	b.message(31, RoleAssistant, "final stretch")
	return b
}

// TestWindowReducesLikeTheWholeLog is the property the whole feature rests on.
func TestWindowReducesLikeTheWholeLog(t *testing.T) {
	b := longRun()
	full := ReduceSession(b.entries)
	windowed := ReduceSession(b.window())

	if !reflect.DeepEqual(full, windowed) {
		t.Fatalf("window folded to a different state\n full: %+v\n window: %+v", full, windowed)
	}
	// A guard on the fixture itself: if the window were the whole log the
	// equality above would be trivially true and would prove nothing.
	if len(b.window()) >= len(b.entries) {
		t.Fatalf("fixture does not window: %d of %d entries", len(b.window()), len(b.entries))
	}
}

// TestWindowRecoversLikeTheWholeLog carries the property up one level, to the
// resume plan the loop actually acts on.
func TestWindowRecoversLikeTheWholeLog(t *testing.T) {
	b := longRun()
	// Leave a dangling tool call in the tail so recovery has real work to do —
	// a window that agrees only on empty plans agrees about nothing.
	b.add(SessionEntry{Kind: EntryMessage, Turn: 32, Message: &Message{
		Role:      RoleAssistant,
		ToolCalls: []ToolCall{{ID: "call-1", Name: "read_ledger", Arguments: "{}"}},
	}})

	full := RecoverSession(b.entries, nil, RecoveryMarkInterrupted)
	windowed := RecoverSession(b.window(), nil, RecoveryMarkInterrupted)
	if !reflect.DeepEqual(full, windowed) {
		t.Fatalf("window recovered a different plan\n full: %+v\n window: %+v", full, windowed)
	}
	if !full.Interrupted || len(full.DroppedCalls) != 1 {
		t.Fatalf("fixture lost its dangling call: %+v", full)
	}
}

// TestWindowKeepsStateWrittenBeforeTheCheckpoint is the specific way a naive
// suffix read goes wrong: the transcript restarts at the checkpoint, but the
// goal, the escalated model, and the disabled tools were all recorded BEFORE it
// and would simply vanish.
func TestWindowKeepsStateWrittenBeforeTheCheckpoint(t *testing.T) {
	b := longRun()
	rs := ReduceSession(b.window())

	if rs.Goal != "all shards reconciled" {
		t.Fatalf("window lost the run's goal: %q", rs.Goal)
	}
	if rs.Model != "claude-opus-5" {
		t.Fatalf("window lost the escalated model: %q", rs.Model)
	}
	if !reflect.DeepEqual(rs.DisabledTools, []string{"flaky_probe", "slow_probe"}) {
		t.Fatalf("window lost disabled tools: %v", rs.DisabledTools)
	}
	// Every one of those was written before the newest checkpoint — that is what
	// makes this test about the checkpoint's state and not about the tail.
	seq := b.checkpointSeq()
	for _, e := range b.entries {
		switch e.Kind {
		case EntryGoal, EntryModelChange, EntryToolDisabled:
			if e.Seq >= seq {
				t.Fatalf("fixture writes %s at seq %d, at or after the checkpoint at %d", e.Kind, e.Seq, seq)
			}
		}
	}
}

// TestCheckpointStateIsOwnedByItsEntry pins the clone.
//
// The loop keeps ONE running mirror of the state and stamps it on every
// checkpoint. A written entry is immutable — that is the log's whole premise —
// so it must own its copy, and the mirror must be free to keep changing without
// rewriting history that has already been recorded.
//
// The mutation is in-place on purpose. Appending to a shared slice usually
// reallocates and hides the aliasing, which is exactly what makes it a bug that
// surfaces later and at random rather than one a growing fixture would catch.
func TestCheckpointStateIsOwnedByItsEntry(t *testing.T) {
	mirror := CheckpointState{
		Model:         "claude-opus-5",
		ActiveTools:   []string{"run_sql", "create_chart"},
		DisabledTools: []string{"flaky_probe"},
		Goal:          "all shards reconciled",
	}
	stamped := mirror.clone()

	mirror.DisabledTools[0] = "something_else"
	mirror.ActiveTools[0] = "something_else"
	mirror.Model = "claude-haiku-4-5"

	if got := stamped.DisabledTools[0]; got != "flaky_probe" {
		t.Fatalf("a written checkpoint changed when the mirror did: disabled = %q", got)
	}
	if got := stamped.ActiveTools[0]; got != "run_sql" {
		t.Fatalf("a written checkpoint changed when the mirror did: active = %q", got)
	}
	if stamped.Model != "claude-opus-5" {
		t.Fatalf("a written checkpoint changed when the mirror did: model = %q", stamped.Model)
	}
}

// TestLoadResumeLogFallsBack covers every way a window can be unavailable or
// unsafe. Each case must end in the FULL log — the failure mode of reading too
// much is slowness, and of reading too little is lost work.
func TestLoadResumeLogFallsBack(t *testing.T) {
	base := longRun()

	cases := []struct {
		name  string
		store SessionStore
		want  int // entries expected back
	}{
		{
			name:  "a store without the capability",
			store: &plainStore{entries: base.entries},
			want:  len(base.entries),
		},
		{
			name:  "a store whose checkpoint lookup fails",
			store: &windowStore{entries: base.entries, seq: base.checkpointSeq(), checkErr: errors.New("db down")},
			want:  len(base.entries),
		},
		{
			name:  "a log with no self-contained checkpoint",
			store: &windowStore{entries: base.entries, seq: 0},
			want:  len(base.entries),
		},
		{
			name:  "a log that branched",
			store: &windowStore{entries: base.entries, seq: base.checkpointSeq(), branched: true},
			want:  len(base.entries),
		},
		{
			name:  "a window read that fails",
			store: &windowStore{entries: base.entries, seq: base.checkpointSeq(), fromErr: errors.New("db down")},
			want:  len(base.entries),
		},
		{
			name:  "a window that does not start at the checkpoint",
			store: &windowStore{entries: base.entries, seq: base.checkpointSeq(), skew: 1},
			want:  len(base.entries),
		},
		{
			name:  "a healthy window",
			store: &windowStore{entries: base.entries, seq: base.checkpointSeq()},
			want:  len(base.window()),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := LoadResumeLog(context.Background(), tc.store, "s1")
			if err != nil {
				t.Fatalf("LoadResumeLog: %v", err)
			}
			if len(got) != tc.want {
				t.Fatalf("read %d entries, want %d", len(got), tc.want)
			}
			// Whatever was read must fold to the same state. That is the only
			// thing a fallback is for.
			if !reflect.DeepEqual(ReduceSession(got), ReduceSession(base.entries)) {
				t.Fatal("the log that was read folds to a different state than the whole log")
			}
		})
	}
}

// TestMemorySessionStoreServesAWindow checks the in-process store against the
// same contract, since it is what every test and local run resumes through.
func TestMemorySessionStoreServesAWindow(t *testing.T) {
	ctx := context.Background()
	m := NewMemorySessionStore()
	b := longRun()
	for _, e := range b.entries {
		if err := m.Append(ctx, "s1", e); err != nil {
			t.Fatalf("append: %v", err)
		}
	}

	seq, branched, err := m.CheckpointSeq(ctx, "s1")
	if err != nil || branched {
		t.Fatalf("CheckpointSeq = %d, %v, %v", seq, branched, err)
	}
	got, err := LoadResumeLog(ctx, m, "s1")
	if err != nil {
		t.Fatalf("LoadResumeLog: %v", err)
	}
	full, _ := m.Log(ctx, "s1")
	if len(got) >= len(full) {
		t.Fatalf("read %d of %d entries — no window was served", len(got), len(full))
	}
	if !reflect.DeepEqual(ReduceSession(got), ReduceSession(full)) {
		t.Fatal("the window folds to a different state than the whole log")
	}

	// A leaf move means the newest checkpoint may sit on an abandoned branch, so
	// the store must stop offering a window at all.
	if err := m.Append(ctx, "s1", SessionEntry{Kind: EntryLeafMove, Target: "#3"}); err != nil {
		t.Fatalf("append: %v", err)
	}
	if _, branched, _ := m.CheckpointSeq(ctx, "s1"); !branched {
		t.Fatal("a log with a leaf move must report itself branched")
	}
	got, _ = LoadResumeLog(ctx, m, "s1")
	if len(got) != len(full)+1 {
		t.Fatalf("a branched log must be read whole, got %d of %d entries", len(got), len(full)+1)
	}
}

// TestUnsettledInboxDefeatsTheWindow covers the third window-safety rule: an
// EntryInbox queued before the newest checkpoint and never settled is invisible
// to a suffix read — the fold scans the raw log for pending inbox items — so
// the store must report no checkpoint and force the full read. Settling it
// restores the window.
func TestUnsettledInboxDefeatsTheWindow(t *testing.T) {
	ctx := context.Background()
	m := NewMemorySessionStore()
	b := longRun()
	for _, e := range b.entries {
		if err := m.Append(ctx, "s1", e); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	full, _ := m.Log(ctx, "s1")

	// A steer queued mid-run and never delivered sits before every checkpoint.
	msg := Message{Role: RoleUser, Content: "also check shard 7"}
	if err := m.Append(ctx, "s1", SessionEntry{Kind: EntryInbox, ID: "inbox-1", Lane: InboxSteer, Message: &msg}); err != nil {
		t.Fatalf("append inbox: %v", err)
	}
	if seq, _, _ := m.CheckpointSeq(ctx, "s1"); seq != 0 {
		t.Fatalf("an unsettled inbox must report no checkpoint, got seq %d", seq)
	}
	got, err := LoadResumeLog(ctx, m, "s1")
	if err != nil {
		t.Fatalf("LoadResumeLog: %v", err)
	}
	if len(got) != len(full)+1 {
		t.Fatalf("an unsettled inbox must force the full read, got %d of %d entries", len(got), len(full)+1)
	}
	if rs := ReduceSession(got); len(rs.Inbox) != 1 || rs.Inbox[0].ID != "inbox-1" {
		t.Fatalf("the pending steer must survive the fold, got %+v", rs.Inbox)
	}

	// Settling it makes the window safe again.
	if err := m.Append(ctx, "s1", SessionEntry{Kind: EntryInboxDone, Target: "inbox-1"}); err != nil {
		t.Fatalf("append done: %v", err)
	}
	got, err = LoadResumeLog(ctx, m, "s1")
	if err != nil {
		t.Fatalf("LoadResumeLog: %v", err)
	}
	if len(got) >= len(full)+2 {
		t.Fatalf("a settled inbox must not defeat the window, got %d of %d entries", len(got), len(full)+2)
	}
}

// TestARealRunWritesResumableCheckpoints closes the loop between the two halves
// of this feature. Everything above proves a window folds correctly GIVEN a
// checkpoint that carries state; this proves the loop actually writes one, on a
// run driven through real compaction with a real provider seam.
//
// Without it the window is dead code in production: every checkpoint would have
// a nil State, LoadResumeLog would fall back on every session, and no test above
// would notice.
func TestARealRunWritesResumableCheckpoints(t *testing.T) {
	ctx := context.Background()
	store := NewMemorySessionStore()
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
		SessionID:  "real",
		Goal:       "finish the blob run",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := agent.Prompt(ctx, "go"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if prov.Summaries < 2 {
		t.Fatalf("run did not compact enough to be a useful fixture: %d summaries", prov.Summaries)
	}

	full, err := store.Log(ctx, "real")
	if err != nil {
		t.Fatalf("Log: %v", err)
	}
	var finals, stamped int
	for _, e := range full {
		if e.Kind == EntryCompaction && e.Final {
			finals++
			if e.State != nil {
				stamped++
			}
		}
	}
	if finals == 0 || stamped != finals {
		t.Fatalf("%d/%d compaction completions carry the run state a resume needs", stamped, finals)
	}

	window, err := LoadResumeLog(ctx, store, "real")
	if err != nil {
		t.Fatalf("LoadResumeLog: %v", err)
	}
	if len(window) >= len(full) {
		t.Fatalf("resume read %d of %d entries — the checkpoint bought nothing", len(window), len(full))
	}
	if !reflect.DeepEqual(ReduceSession(window), ReduceSession(full)) {
		t.Fatal("the window a real run produced folds to a different state than its whole log")
	}
	// The goal is the state most easily lost: it is written once, at the very
	// start of the run, so every checkpoint after it is a chance to drop it and a
	// window that did would resume an ungated run.
	//
	// It is read off the checkpoint rather than off the fold because this run
	// FINISHED — a leaf clears the goal, correctly, since it belonged to the run
	// that just ended. The state a crash would resume from is the one stamped on
	// the checkpoint.
	for _, e := range window {
		if e.Kind == EntryCompaction && e.Final && e.State.Goal != "finish the blob run" {
			t.Fatalf("checkpoint at seq %d lost the run's goal: %q", e.Seq, e.State.Goal)
		}
	}
}

// --- stores ---

// plainStore implements only SessionStore, standing in for a backend that has
// not adopted the windowing capability.
type plainStore struct{ entries []SessionEntry }

func (p *plainStore) Append(context.Context, string, SessionEntry) error { return nil }
func (p *plainStore) Log(context.Context, string) ([]SessionEntry, error) {
	return p.entries, nil
}

// windowStore implements the capability and can fail or misbehave on demand.
type windowStore struct {
	entries  []SessionEntry
	seq      int
	branched bool
	checkErr error
	fromErr  error
	skew     int // start the window this many entries late
}

func (w *windowStore) Append(context.Context, string, SessionEntry) error { return nil }
func (w *windowStore) Log(context.Context, string) ([]SessionEntry, error) {
	return w.entries, nil
}
func (w *windowStore) CheckpointSeq(context.Context, string) (int, bool, error) {
	return w.seq, w.branched, w.checkErr
}
func (w *windowStore) LogFrom(_ context.Context, _ string, sinceSeq int) ([]SessionEntry, error) {
	if w.fromErr != nil {
		return nil, w.fromErr
	}
	var out []SessionEntry
	for _, e := range w.entries {
		if e.Seq >= sinceSeq+w.skew {
			out = append(out, e)
		}
	}
	return out, nil
}

// unreadableLogStore accepts appends but cannot read its log back.
type unreadableLogStore struct{ *memSessionStore }

func (unreadableLogStore) Log(context.Context, string) ([]SessionEntry, error) {
	return nil, errors.New("disk exploded")
}

// TestResumeFailsOnUnreadableLog verifies a resume whose durable log cannot be
// read fails loudly instead of silently degrading to a from-scratch run that
// would splice fresh seeds onto (and redo work recorded in) the crashed log.
func TestResumeFailsOnUnreadableLog(t *testing.T) {
	agent, err := New(Config{
		Provider:      NewFauxProvider(AssistantText("should never run")),
		Model:         "test",
		Session:       unreadableLogStore{newMemSessionStore()},
		SessionID:     "r5",
		ResumeSession: true,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = agent.Prompt(context.Background(), "resume")
	if err == nil || !strings.Contains(err.Error(), "resume: reading durable session log") {
		t.Fatalf("unreadable log must fail the resume, got %v", err)
	}
}

// TestTerminalToolWritesLeaf verifies a run ended by a terminal tool (an After
// hook returning terminate=true) still records its leaf, so the durable log
// reduces to Completed=true and a later resume reattaches instead of replaying
// the terminal call.
func TestTerminalToolWritesLeaf(t *testing.T) {
	ctx := context.Background()
	store := newMemSessionStore()
	terminal := func(_ context.Context, _ ToolCall, result string, _ error) (string, bool) {
		return result, true
	}
	provider := NewFauxProvider(
		AssistantToolCall("c1", "noop", `{}`),
		AssistantText("should never be produced"),
	)
	agent, err := New(Config{
		Provider:  provider,
		Model:     "test",
		Tools:     NewToolSet(noopTool{}),
		Policy:    NewAllowList("noop"),
		Hooks:     Hooks{After: []AfterToolCall{terminal}},
		Session:   store,
		SessionID: "r6",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := agent.Prompt(ctx, "finish it"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if len(provider.Recorded) != 1 {
		t.Fatalf("terminal tool must end the run after one turn (%d provider calls)", len(provider.Recorded))
	}
	log, _ := store.Log(ctx, "r6")
	if rs := ReduceSession(log); !rs.Completed {
		t.Fatalf("terminal exit must reduce Completed=true: %+v", rs)
	}
}

// TestReplayHonorsMaxToolCalls verifies resume replay runs under the run's tool
// budget: with MaxToolCalls=1 and two dangling retry-safe calls, exactly one is
// re-executed and the other is closed with an interrupted note.
func TestReplayHonorsMaxToolCalls(t *testing.T) {
	ctx := context.Background()
	store := newMemSessionStore()
	for _, e := range []SessionEntry{
		{Kind: EntryMessage, Message: &Message{Role: RoleUser, Content: "count the beans"}},
		{Kind: EntryMessage, Message: &Message{Role: RoleAssistant, ToolCalls: []ToolCall{
			{ID: "c1", Name: "safe_read", Arguments: "{}"},
			{ID: "c2", Name: "safe_read", Arguments: "{}"},
		}}},
	} {
		if err := store.Append(ctx, "r7", e); err != nil {
			t.Fatal(err)
		}
	}

	provider := NewFauxProvider(AssistantText("done"))
	agent, err := New(Config{
		Provider:      provider,
		Model:         "test",
		Tools:         NewToolSet(retrySafeProbe{}),
		Policy:        NewAllowList("safe_read"),
		Limits:        &Limits{MaxTurns: 4, MaxToolCalls: 1, MaxToolResultLen: 1 << 16, MaxContextTokens: 1 << 20},
		Session:       store,
		SessionID:     "r7",
		ResumeSession: true,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := agent.Prompt(ctx, "resume"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}

	replayed, interrupted := 0, 0
	for _, m := range provider.Recorded[0].Messages {
		if m.Role != RoleTool {
			continue
		}
		switch {
		case m.Content == "ok":
			replayed++
		case strings.Contains(m.Content, "[interrupted:"):
			interrupted++
		}
	}
	if replayed != 1 || interrupted != 1 {
		t.Fatalf("budget must cap replay (replayed=%d interrupted=%d, want 1/1): %+v",
			replayed, interrupted, provider.Recorded[0].Messages)
	}
}
