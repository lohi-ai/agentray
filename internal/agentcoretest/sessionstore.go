// Package agentcoretest contains reusable conformance checks for agentcore
// boundary implementations. Keeping the checks outside the kernel lets every
// host backend prove the same semantics without adding test machinery to the
// runtime package itself.
package agentcoretest

import (
	"context"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/lohi-ai/agentray/agentcore"
)

// SessionStoreHarness supplies one backend and a factory for fresh, valid
// session IDs. Durable backends may use NewSessionID to create required parent
// rows; in-memory backends can simply return unique strings.
type SessionStoreHarness struct {
	Store        agentcore.SessionStore
	NewSessionID func(t *testing.T) string
}

// RunSessionStoreConformance exercises the observable SessionStore contract,
// including optional batch and window capabilities. Backends should invoke it
// unchanged so local and server persistence cannot drift semantically.
func RunSessionStoreConformance(t *testing.T, harness SessionStoreHarness) {
	t.Helper()
	if harness.Store == nil {
		t.Fatal("SessionStoreHarness.Store is nil")
	}
	if harness.NewSessionID == nil {
		t.Fatal("SessionStoreHarness.NewSessionID is nil")
	}

	t.Run("snapshot_and_typed_round_trip", func(t *testing.T) {
		id := harness.NewSessionID(t)
		ctx := context.Background()
		created := time.Date(2026, 9, 18, 12, 30, 0, 123_000_000, time.UTC)
		message := agentcore.Message{
			Role: agentcore.RoleAssistant, Content: "calling", Directive: true,
			ToolCalls: []agentcore.ToolCall{{ID: "call-1", Name: "read", Arguments: `{"path":"README.md"}`}},
			Usage:     &agentcore.Usage{InputTokens: 11, OutputTokens: 7, CacheReadTokens: 3, CostUSD: 0.125},
		}
		first := agentcore.SessionEntry{
			Kind: agentcore.EntryMessage, ID: "assistant-1", ParentID: "root", Turn: 2,
			Message: &message, Tools: []string{"read", "write"}, Question: []byte(`{"prompt":"continue?"}`),
			CreatedAt: created,
		}
		outcome := agentcore.SessionEntry{
			Kind: agentcore.EntryToolOutcome, ID: "outcome-1", ParentID: "assistant-1", Turn: 2, CallID: "call-1",
			Outcome: &agentcore.ToolOutcomeRecord{
				Message: agentcore.Message{Role: agentcore.RoleTool, ToolCallID: "call-1", Name: "read", Content: "contents"},
				Trace: agentcore.ToolTrace{
					CallID: "call-1", Tool: "read", Args: `{"path":"README.md"}`, Allowed: true,
					ResultMeta: "17 bytes", LatencyMS: 9, IdempotencyKey: "idem-1", SpillLocator: "spill://one",
				},
				Extra: []agentcore.Message{{Role: agentcore.RoleSystem, Content: "extra"}}, Terminate: true, Executed: true,
			},
			CreatedAt: created.Add(time.Second),
		}
		if err := harness.Store.Append(ctx, id, first); err != nil {
			t.Fatalf("Append(first): %v", err)
		}
		if err := harness.Store.Append(ctx, id, outcome); err != nil {
			t.Fatalf("Append(outcome): %v", err)
		}

		// A store owns an immutable snapshot after Append, not aliases into the
		// caller's structs.
		first.Message.Content = "mutated at source"
		first.Message.ToolCalls[0].Name = "mutated"
		first.Message.Usage.InputTokens = 999
		first.Tools[0] = "mutated"
		first.Question[0] = 'X'
		outcome.Outcome.Message.Content = "mutated outcome"
		outcome.Outcome.Extra[0].Content = "mutated extra"

		got := mustLog(t, harness.Store, id)
		assertTypedEntries(t, got, created)

		// Reads are snapshots too: mutating one result must not rewrite the log.
		got[0].Message.Content = "mutated through read"
		got[0].Message.ToolCalls[0].Name = "mutated through read"
		got[1].Outcome.Extra[0].Content = "mutated through read"
		assertTypedEntries(t, mustLog(t, harness.Store, id), created)
	})

	t.Run("session_isolation", func(t *testing.T) {
		ctx := context.Background()
		left, right := harness.NewSessionID(t), harness.NewSessionID(t)
		appendMessage(t, ctx, harness.Store, left, "left")
		appendMessage(t, ctx, harness.Store, right, "right")
		if got := mustLog(t, harness.Store, left); len(got) != 1 || got[0].Message.Content != "left" {
			t.Fatalf("left session leaked or lost data: %+v", got)
		}
		if got := mustLog(t, harness.Store, right); len(got) != 1 || got[0].Message.Content != "right" {
			t.Fatalf("right session leaked or lost data: %+v", got)
		}
	})

	t.Run("optional_lease_serializes_one_session", func(t *testing.T) {
		leases, ok := harness.Store.(agentcore.SessionLeaseStore)
		if !ok {
			t.Skip("backend does not implement SessionLeaseStore")
		}
		id := harness.NewSessionID(t)
		other := harness.NewSessionID(t)
		_, release, err := leases.AcquireSessionLease(context.Background(), id)
		if err != nil {
			t.Fatalf("first acquire: %v", err)
		}
		defer func() { _ = release() }()

		// A different durable session must not queue behind this one.
		_, releaseOther, err := leases.AcquireSessionLease(context.Background(), other)
		if err != nil {
			t.Fatalf("independent acquire: %v", err)
		}
		if err := releaseOther(); err != nil {
			t.Fatalf("independent release: %v", err)
		}

		waitCtx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		if _, _, err := leases.AcquireSessionLease(waitCtx, id); err == nil {
			t.Fatal("contending owner acquired a live session")
		}
		if err := release(); err != nil {
			t.Fatalf("first release: %v", err)
		}
		_, releaseAgain, err := leases.AcquireSessionLease(context.Background(), id)
		if err != nil {
			t.Fatalf("acquire after release: %v", err)
		}
		if err := releaseAgain(); err != nil {
			t.Fatalf("second release: %v", err)
		}
	})

	t.Run("concurrent_append_has_total_order", func(t *testing.T) {
		const writers = 32
		id := harness.NewSessionID(t)
		ctx := context.Background()
		errs := make(chan error, writers)
		var wg sync.WaitGroup
		for i := 0; i < writers; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				content := fmt.Sprintf("writer-%02d", i)
				msg := agentcore.Message{Role: agentcore.RoleUser, Content: content}
				errs <- harness.Store.Append(ctx, id, agentcore.SessionEntry{Kind: agentcore.EntryMessage, Message: &msg})
			}(i)
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatalf("concurrent Append: %v", err)
			}
		}
		log := mustLog(t, harness.Store, id)
		if len(log) != writers {
			t.Fatalf("len(Log) = %d, want %d", len(log), writers)
		}
		seen := make(map[string]bool, writers)
		assertStrictlyIncreasingSeq(t, log)
		for _, entry := range log {
			if entry.Message == nil {
				t.Fatalf("entry %d has no message", entry.Seq)
			}
			seen[entry.Message.Content] = true
		}
		for i := 0; i < writers; i++ {
			if !seen[fmt.Sprintf("writer-%02d", i)] {
				t.Errorf("writer-%02d was lost", i)
			}
		}
	})

	t.Run("batch_is_atomic_and_contiguous", func(t *testing.T) {
		batches, ok := harness.Store.(agentcore.SessionBatchStore)
		if !ok {
			t.Skip("backend does not implement SessionBatchStore")
		}
		const batchCount, batchSize, singles = 16, 3, 16
		id := harness.NewSessionID(t)
		ctx := context.Background()
		if err := batches.AppendBatch(ctx, id, nil); err != nil {
			t.Fatalf("empty AppendBatch: %v", err)
		}
		errs := make(chan error, batchCount+singles)
		var wg sync.WaitGroup
		for batch := 0; batch < batchCount; batch++ {
			wg.Add(1)
			go func(batch int) {
				defer wg.Done()
				entries := make([]agentcore.SessionEntry, batchSize)
				for item := range entries {
					content := fmt.Sprintf("batch-%02d-%d", batch, item)
					msg := agentcore.Message{Role: agentcore.RoleUser, Content: content}
					entries[item] = agentcore.SessionEntry{Kind: agentcore.EntryMessage, Message: &msg}
				}
				errs <- batches.AppendBatch(ctx, id, entries)
			}(batch)
		}
		for single := 0; single < singles; single++ {
			wg.Add(1)
			go func(single int) {
				defer wg.Done()
				content := fmt.Sprintf("single-%02d", single)
				msg := agentcore.Message{Role: agentcore.RoleUser, Content: content}
				errs <- harness.Store.Append(ctx, id, agentcore.SessionEntry{Kind: agentcore.EntryMessage, Message: &msg})
			}(single)
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatalf("concurrent batch/append: %v", err)
			}
		}

		log := mustLog(t, harness.Store, id)
		if want := batchCount*batchSize + singles; len(log) != want {
			t.Fatalf("len(Log) = %d, want %d", len(log), want)
		}
		assertStrictlyIncreasingSeq(t, log)
		positions := make(map[string]int, len(log))
		for position, entry := range log {
			if entry.Message == nil {
				t.Fatalf("entry at position %d has no message", position)
			}
			positions[entry.Message.Content] = position
		}
		for batch := 0; batch < batchCount; batch++ {
			start := positions[fmt.Sprintf("batch-%02d-0", batch)]
			for item := 1; item < batchSize; item++ {
				if got := positions[fmt.Sprintf("batch-%02d-%d", batch, item)]; got != start+item {
					t.Errorf("batch %d was interleaved: item %d at %d, want %d", batch, item, got, start+item)
				}
			}
		}
	})

	t.Run("branch_and_side_record_semantics", func(t *testing.T) {
		id := harness.NewSessionID(t)
		ctx := context.Background()
		user := agentcore.Message{Role: agentcore.RoleUser, Content: "root"}
		assistant := agentcore.Message{Role: agentcore.RoleAssistant, Content: "", ToolCalls: []agentcore.ToolCall{{ID: "call-branch", Name: "write", Arguments: `{}`}}}
		entries := []agentcore.SessionEntry{
			{Kind: agentcore.EntryMessage, ID: "root", Turn: 1, Message: &user},
			{Kind: agentcore.EntryMessage, ID: "calls", ParentID: "root", Turn: 1, Message: &assistant},
			{Kind: agentcore.EntryToolOutcome, ID: "outcome", ParentID: "calls", Turn: 1, CallID: "call-branch", Outcome: &agentcore.ToolOutcomeRecord{
				Message: agentcore.Message{Role: agentcore.RoleTool, ToolCallID: "call-branch", Name: "write", Content: "done"},
				Trace:   agentcore.ToolTrace{CallID: "call-branch", Tool: "write", Args: `{}`, Allowed: true}, Executed: true,
			}},
			{Kind: agentcore.EntryLeafMove, Target: "root"},
		}
		appendBatchOrEntries(t, ctx, harness.Store, id, entries)
		log := mustLog(t, harness.Store, id)
		state := agentcore.ReduceSession(log)
		if len(state.Messages) != 1 || state.Messages[0].Content != "root" {
			t.Fatalf("active branch messages = %+v, want only root", state.Messages)
		}
		if state.ToolOutcomes != nil {
			t.Fatalf("abandoned branch outcome remained active: %+v", state.ToolOutcomes)
		}
	})

	t.Run("completed_tool_outcome_is_recoverable", func(t *testing.T) {
		id := harness.NewSessionID(t)
		ctx := context.Background()
		user := agentcore.Message{Role: agentcore.RoleUser, Content: "do it"}
		assistant := agentcore.Message{Role: agentcore.RoleAssistant, ToolCalls: []agentcore.ToolCall{{ID: "call-recover", Name: "write", Arguments: `{}`}}}
		entries := []agentcore.SessionEntry{
			{Kind: agentcore.EntryMessage, ID: "recover-root", Message: &user},
			{Kind: agentcore.EntryMessage, ID: "recover-call", ParentID: "recover-root", Message: &assistant},
			{Kind: agentcore.EntryToolOutcome, ParentID: "recover-call", CallID: "call-recover", Outcome: &agentcore.ToolOutcomeRecord{
				Message: agentcore.Message{Role: agentcore.RoleTool, ToolCallID: "call-recover", Name: "write", Content: "done"},
				Trace:   agentcore.ToolTrace{CallID: "call-recover", Tool: "write", Allowed: true}, Executed: true,
			}},
		}
		appendBatchOrEntries(t, ctx, harness.Store, id, entries)
		plan := agentcore.RecoverSession(mustLog(t, harness.Store, id), nil, agentcore.RecoveryMarkInterrupted)
		if plan.ToolOutcomes["call-recover"].Message.Content != "done" {
			t.Fatalf("completed outcome not recovered: %+v", plan.ToolOutcomes)
		}
		if len(plan.RetryCalls) != 0 || len(plan.DroppedCalls) != 0 {
			t.Fatalf("completed physical call was retried/dropped: retry=%+v dropped=%+v", plan.RetryCalls, plan.DroppedCalls)
		}
	})

	t.Run("reverse_completion_recovers_once_in_source_order", func(t *testing.T) {
		id := harness.NewSessionID(t)
		ctx := context.Background()
		user := agentcore.Message{Role: agentcore.RoleUser, Content: "run both"}
		assistant := agentcore.Message{Role: agentcore.RoleAssistant, ToolCalls: []agentcore.ToolCall{
			{ID: "slow-call", Name: "slow", Arguments: `{}`},
			{ID: "fast-call", Name: "fast", Arguments: `{}`},
		}}
		// Persist one intent, then physical completions in the opposite order.
		// They are deliberately separate appends: this is the crash-recovery shape,
		// not a settled transcript batch.
		entries := []agentcore.SessionEntry{
			{Kind: agentcore.EntryMessage, ID: "parallel-root", Message: &user},
			{Kind: agentcore.EntryMessage, ID: "parallel-intent", ParentID: "parallel-root", Message: &assistant},
			{Kind: agentcore.EntryToolOutcome, ParentID: "parallel-intent", CallID: "fast-call", Outcome: &agentcore.ToolOutcomeRecord{
				Message: agentcore.Message{Role: agentcore.RoleTool, ToolCallID: "fast-call", Name: "fast", Content: "fast-result"},
				Trace:   agentcore.ToolTrace{CallID: "fast-call", Tool: "fast", Args: `{}`, Allowed: true}, Executed: true,
			}},
			{Kind: agentcore.EntryToolOutcome, ParentID: "parallel-intent", CallID: "slow-call", Outcome: &agentcore.ToolOutcomeRecord{
				Message: agentcore.Message{Role: agentcore.RoleTool, ToolCallID: "slow-call", Name: "slow", Content: "slow-result"},
				Trace:   agentcore.ToolTrace{CallID: "slow-call", Tool: "slow", Args: `{}`, Allowed: true}, Executed: true,
			}},
		}
		for _, entry := range entries {
			if err := harness.Store.Append(ctx, id, entry); err != nil {
				t.Fatalf("Append(%s/%s): %v", entry.Kind, entry.CallID, err)
			}
		}
		plan := agentcore.RecoverSession(mustLog(t, harness.Store, id), nil, agentcore.RecoveryMarkInterrupted)
		if len(plan.ToolOutcomes) != 2 || len(plan.RetryCalls) != 0 || len(plan.DroppedCalls) != 0 {
			t.Fatalf("reverse completions produced wrong recovery plan: %+v", plan)
		}

		provider := agentcore.NewFauxProvider(agentcore.AssistantText("continued"))
		agent, err := agentcore.New(agentcore.Config{
			Provider: provider, Model: "test", Session: harness.Store, SessionID: id, ResumeSession: true,
		})
		if err != nil {
			t.Fatalf("New(resumer): %v", err)
		}
		if _, err := agent.Prompt(ctx, "resume"); err != nil {
			t.Fatalf("first resume: %v", err)
		}
		if len(provider.Recorded) != 1 {
			t.Fatalf("provider calls = %d, want 1", len(provider.Recorded))
		}
		var ordered []string
		for _, message := range provider.Recorded[0].Messages {
			if message.Role == agentcore.RoleTool {
				ordered = append(ordered, message.ToolCallID+":"+message.Content)
			}
		}
		want := []string{"slow-call:slow-result", "fast-call:fast-result"}
		if !reflect.DeepEqual(ordered, want) {
			t.Fatalf("recovered provider order = %v, want %v", ordered, want)
		}
		assertCanonicalToolResultsOnce(t, mustLog(t, harness.Store, id), "slow-call", "fast-call")

		// The first resume materialized both canonical messages and completed the
		// log. A second recovery must reattach without calling a provider or
		// appending another result for either physical execution.
		secondProvider := agentcore.NewFauxProvider(agentcore.AssistantText("must not run"))
		second, err := agentcore.New(agentcore.Config{
			Provider: secondProvider, Model: "test", Session: harness.Store, SessionID: id, ResumeSession: true,
		})
		if err != nil {
			t.Fatalf("New(second resumer): %v", err)
		}
		result, err := second.Prompt(ctx, "resume again")
		if err != nil {
			t.Fatalf("second resume: %v", err)
		}
		if result.StopReason != "reattached" || len(secondProvider.Recorded) != 0 {
			t.Fatalf("second resume replayed work: stop=%q provider_calls=%d", result.StopReason, len(secondProvider.Recorded))
		}
		assertCanonicalToolResultsOnce(t, mustLog(t, harness.Store, id), "slow-call", "fast-call")
	})

	t.Run("checkpoint_window_matches_full_fold", func(t *testing.T) {
		windows, ok := harness.Store.(agentcore.SessionWindowStore)
		if !ok {
			t.Skip("backend does not implement SessionWindowStore")
		}
		id := harness.NewSessionID(t)
		ctx := context.Background()
		old := agentcore.Message{Role: agentcore.RoleUser, Content: "old"}
		tail := agentcore.Message{Role: agentcore.RoleAssistant, Content: "new"}
		entries := []agentcore.SessionEntry{
			{Kind: agentcore.EntryMessage, ID: "old", Turn: 1, Message: &old},
			{Kind: agentcore.EntryModelChange, ID: "old-model", ParentID: "old", Turn: 1, Model: "superseded"},
			{Kind: agentcore.EntryCompaction, ID: "checkpoint", ParentID: "old-model", Turn: 4, Final: true, Summary: "summary",
				Retained: []agentcore.Message{{Role: agentcore.RoleSystem, Content: "summary"}},
				State:    &agentcore.CheckpointState{Model: "model-v2", ActiveTools: []string{"read"}, DisabledTools: []string{"write"}, Goal: "finish"}},
			{Kind: agentcore.EntryMessage, ID: "tail", ParentID: "checkpoint", Turn: 5, Message: &tail},
		}
		appendBatchOrEntries(t, ctx, harness.Store, id, entries)
		seq, branched, err := windows.CheckpointSeq(ctx, id)
		if err != nil {
			t.Fatalf("CheckpointSeq: %v", err)
		}
		if seq <= 0 || branched {
			t.Fatalf("CheckpointSeq = (%d, %v), want positive/unbranched", seq, branched)
		}
		window, err := windows.LogFrom(ctx, id, seq)
		if err != nil {
			t.Fatalf("LogFrom: %v", err)
		}
		if len(window) == 0 || window[0].Seq != seq || window[0].Kind != agentcore.EntryCompaction {
			t.Fatalf("window starts incorrectly: %+v", window)
		}
		fullState := agentcore.ReduceSession(mustLog(t, harness.Store, id))
		windowState := agentcore.ReduceSession(window)
		if !reflect.DeepEqual(fullState, windowState) {
			t.Fatalf("window fold differs from full fold:\nfull=%+v\nwindow=%+v", fullState, windowState)
		}
	})

	t.Run("unsafe_windows_are_rejected", func(t *testing.T) {
		windows, ok := harness.Store.(agentcore.SessionWindowStore)
		if !ok {
			t.Skip("backend does not implement SessionWindowStore")
		}
		ctx := context.Background()

		t.Run("branch", func(t *testing.T) {
			id := harness.NewSessionID(t)
			root := agentcore.Message{Role: agentcore.RoleUser, Content: "root"}
			appendBatchOrEntries(t, ctx, harness.Store, id, []agentcore.SessionEntry{
				{Kind: agentcore.EntryMessage, ID: "root", Message: &root},
				{Kind: agentcore.EntryCompaction, ID: "cp", ParentID: "root", Final: true, Retained: []agentcore.Message{}, State: &agentcore.CheckpointState{}},
				{Kind: agentcore.EntryLeafMove, Target: "root"},
			})
			seq, branched, err := windows.CheckpointSeq(ctx, id)
			if err != nil {
				t.Fatalf("CheckpointSeq: %v", err)
			}
			if !branched {
				t.Fatalf("CheckpointSeq = (%d, false), want branched", seq)
			}
		})

		t.Run("unsettled_inbox", func(t *testing.T) {
			id := harness.NewSessionID(t)
			root := agentcore.Message{Role: agentcore.RoleUser, Content: "root"}
			queued := agentcore.Message{Role: agentcore.RoleUser, Content: "do not lose me"}
			appendBatchOrEntries(t, ctx, harness.Store, id, []agentcore.SessionEntry{
				{Kind: agentcore.EntryMessage, ID: "root", Message: &root},
				{Kind: agentcore.EntryInbox, ID: "inbox-1", Lane: agentcore.InboxSteer, Message: &queued},
				{Kind: agentcore.EntryCompaction, ID: "cp", ParentID: "root", Final: true, Retained: []agentcore.Message{}, State: &agentcore.CheckpointState{}},
			})
			seq, _, err := windows.CheckpointSeq(ctx, id)
			if err != nil {
				t.Fatalf("CheckpointSeq: %v", err)
			}
			if seq != 0 {
				t.Fatalf("CheckpointSeq = %d with pending pre-checkpoint inbox, want 0", seq)
			}
		})
	})
}

func appendMessage(t *testing.T, ctx context.Context, store agentcore.SessionStore, id, content string) {
	t.Helper()
	message := agentcore.Message{Role: agentcore.RoleUser, Content: content}
	if err := store.Append(ctx, id, agentcore.SessionEntry{Kind: agentcore.EntryMessage, Message: &message}); err != nil {
		t.Fatalf("Append(%q): %v", content, err)
	}
}

func appendBatchOrEntries(t *testing.T, ctx context.Context, store agentcore.SessionStore, id string, entries []agentcore.SessionEntry) {
	t.Helper()
	if batches, ok := store.(agentcore.SessionBatchStore); ok {
		if err := batches.AppendBatch(ctx, id, entries); err != nil {
			t.Fatalf("AppendBatch: %v", err)
		}
		return
	}
	for _, entry := range entries {
		if err := store.Append(ctx, id, entry); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
}

func mustLog(t *testing.T, store agentcore.SessionStore, id string) []agentcore.SessionEntry {
	t.Helper()
	log, err := store.Log(context.Background(), id)
	if err != nil {
		t.Fatalf("Log: %v", err)
	}
	return log
}

func assertStrictlyIncreasingSeq(t *testing.T, log []agentcore.SessionEntry) {
	t.Helper()
	for i := 1; i < len(log); i++ {
		if log[i].Seq <= log[i-1].Seq {
			t.Fatalf("sequence is not strictly increasing at %d: %d then %d", i, log[i-1].Seq, log[i].Seq)
		}
	}
}

func assertTypedEntries(t *testing.T, got []agentcore.SessionEntry, created time.Time) {
	t.Helper()
	if len(got) != 2 {
		t.Fatalf("len(Log) = %d, want 2", len(got))
	}
	assertStrictlyIncreasingSeq(t, got)
	first := got[0]
	if first.Kind != agentcore.EntryMessage || first.ID != "assistant-1" || first.ParentID != "root" || first.Turn != 2 || !first.CreatedAt.Equal(created) {
		t.Fatalf("message envelope did not round-trip: %+v", first)
	}
	if first.Message == nil || first.Message.Content != "calling" || len(first.Message.ToolCalls) != 1 || first.Message.ToolCalls[0].Name != "read" || first.Message.Usage == nil || first.Message.Usage.InputTokens != 11 {
		t.Fatalf("typed message did not round-trip: %+v", first.Message)
	}
	if !reflect.DeepEqual(first.Tools, []string{"read", "write"}) || string(first.Question) != `{"prompt":"continue?"}` {
		t.Fatalf("slice/raw fields did not round-trip: tools=%v question=%s", first.Tools, first.Question)
	}
	second := got[1]
	if second.Kind != agentcore.EntryToolOutcome || second.CallID != "call-1" || second.Outcome == nil {
		t.Fatalf("outcome envelope did not round-trip: %+v", second)
	}
	if second.Outcome.Message.Content != "contents" || second.Outcome.Trace.SpillLocator != "spill://one" || len(second.Outcome.Extra) != 1 || second.Outcome.Extra[0].Content != "extra" || !second.Outcome.Terminate || !second.Outcome.Executed {
		t.Fatalf("tool outcome did not round-trip: %+v", second.Outcome)
	}
}

func assertCanonicalToolResultsOnce(t *testing.T, log []agentcore.SessionEntry, callIDs ...string) {
	t.Helper()
	counts := make(map[string]int, len(callIDs))
	for _, entry := range log {
		if entry.Kind == agentcore.EntryMessage && entry.Message != nil && entry.Message.Role == agentcore.RoleTool {
			counts[entry.Message.ToolCallID]++
		}
	}
	for _, callID := range callIDs {
		if counts[callID] != 1 {
			t.Errorf("canonical result count for %q = %d, want 1 (all counts: %v)", callID, counts[callID], counts)
		}
	}
}
