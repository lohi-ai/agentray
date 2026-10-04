package agentruntime

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/lohi-ai/agentray/agentcore"
)

func ladderJournalEntry(t *testing.T, selection nativeLadderSelection) agentcore.SessionEntry {
	t.Helper()
	raw, err := json.Marshal(selection)
	if err != nil {
		t.Fatal(err)
	}
	return agentcore.SessionEntry{Kind: agentcore.EntryPiModelSelection, Content: string(raw)}
}

func TestNativeLadderJournalCrashRecovery(t *testing.T) {
	for _, failApply := range []bool{false, true} {
		name := "before-checkpoint"
		if failApply {
			name = "before-state-apply"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			store := agentcore.NewMemorySessionStore()
			ladder := testNativeLadder(t)
			binding, _, stream := ladder.binding()
			session, err := NewPiSession(ctx, PiSessionConfig{Pi: binding, nativeLadder: ladder, NativeStream: stream, Store: store, SessionID: name})
			if err != nil {
				t.Fatal(err)
			}
			defer session.Close()
			if failApply {
				session.agent.Call = func(context.Context, string, json.RawMessage) (json.RawMessage, error) {
					return nil, errors.New("process stopped")
				}
			}
			err = session.selectNativeRung(ctx, 0, 1)
			if failApply {
				if err == nil || session.failure() == nil {
					t.Fatal("state apply failure did not fence session")
				}
			} else if err != nil {
				t.Fatal(err)
			}
			entries, err := store.Log(ctx, name)
			if err != nil {
				t.Fatal(err)
			}
			if entries[len(entries)-1].Kind != agentcore.EntryPiModelSelection {
				t.Fatal("selection was not durable before state/checkpoint")
			}
			recovered, err := recoverPiState(entries)
			if err != nil {
				t.Fatal(err)
			}
			var state map[string]json.RawMessage
			if err = json.Unmarshal(recovered, &state); err != nil {
				t.Fatal(err)
			}
			if !nativeModelIdentityEqual(state["model"], ladder.selection().Model) {
				t.Fatal("recovery lost committed model")
			}
			fresh := testNativeLadder(t)
			if err = fresh.restoreJournal(entries); err != nil {
				t.Fatal(err)
			}
			if fresh.selection().Generation != 1 || fresh.selection().Rung != 1 {
				t.Fatal("recovery lost generation or rung")
			}
			restored, _, _ := fresh.binding()
			key, err := restored.Callback(ctx, "getApiKey", json.RawMessage(`"openai"`), nil)
			if err != nil || string(key) != `"secret-b-fresh"` {
				t.Fatal("recovery selected wrong provider row")
			}
			if !failApply {
				if err = session.checkpoint(ctx); err != nil {
					t.Fatal(err)
				}
				entries, err = store.Log(ctx, name)
				if err != nil {
					t.Fatal(err)
				}
				if _, err = recoverPiState(entries); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestNativeLadderJournalRejectsCorruptionAtomically(t *testing.T) {
	ladder := testNativeLadder(t)
	first := nativeLadderSelection{Version: 1, Generation: 1, Rung: 1, ProviderID: ladder.rungs[1].providerID, Model: ladder.rungs[1].model}
	next := nativeLadderSelection{Version: 1, Generation: 2, Rung: 0, ProviderID: ladder.rungs[0].providerID, Model: ladder.rungs[0].model}
	for _, tc := range []struct {
		name   string
		change func(*nativeLadderSelection)
	}{
		{"generation-gap", func(r *nativeLadderSelection) { r.Generation = 3 }},
		{"repeated-generation", func(r *nativeLadderSelection) { r.Generation = 1 }},
		{"unknown-version", func(r *nativeLadderSelection) { r.Version = 2 }},
		{"repeated-rung", func(r *nativeLadderSelection) { r.Rung = 1 }},
		{"wrong-row", func(r *nativeLadderSelection) { r.ProviderID = "replaced" }},
		{"changed-model", func(r *nativeLadderSelection) {
			r.Model = json.RawMessage(`{"id":"changed","api":"openai-completions","provider":"openai"}`)
		}},
		{"invalid-model", func(r *nativeLadderSelection) { r.Model = json.RawMessage(`{}`) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fresh := testNativeLadder(t)
			bad := next
			tc.change(&bad)
			entries := []agentcore.SessionEntry{ladderJournalEntry(t, first), ladderJournalEntry(t, bad)}
			if err := fresh.restoreJournal(entries); err == nil {
				t.Fatal("corruption accepted")
			}
			if fresh.selection().Generation != 0 || fresh.selection().Rung != 0 {
				t.Fatal("partial restore escaped")
			}
		})
	}
	initial, _ := json.Marshal(map[string]any{"model": ladder.rungs[0].model, "messages": []any{}})
	checkpoint := agentcore.SessionEntry{Kind: piStateEntry, Content: string(initial)}
	if _, err := recoverPiState([]agentcore.SessionEntry{ladderJournalEntry(t, first), checkpoint}); err == nil {
		t.Fatal("selection before initial state accepted")
	}
	if _, err := recoverPiState([]agentcore.SessionEntry{checkpoint, ladderJournalEntry(t, first), checkpoint}); err == nil {
		t.Fatal("stale checkpoint accepted")
	}
	valid := ladderJournalEntry(t, first)
	for _, field := range []string{"version", "generation", "rung", "providerId", "model"} {
		var fields map[string]json.RawMessage
		_ = json.Unmarshal([]byte(valid.Content), &fields)
		delete(fields, field)
		raw, _ := json.Marshal(fields)
		if _, err := parseNativeLadderSelection(string(raw), nil); err == nil {
			t.Fatalf("missing %s accepted", field)
		}
	}
	if strings.Contains(valid.Content, "secret-") {
		t.Fatal("credential entered journal")
	}
}

type nativeSelectionFailStore struct {
	*agentcore.MemorySessionStore
	selectionError error
}

func (s *nativeSelectionFailStore) Append(ctx context.Context, id string, entry agentcore.SessionEntry) error {
	if entry.Kind == agentcore.EntryPiModelSelection {
		return s.selectionError
	}
	return s.MemorySessionStore.Append(ctx, id, entry)
}
func TestNativeLadderJournalWriteFailureFencesSession(t *testing.T) {
	for _, failure := range []error{errors.New("disk unavailable"), agentcore.ErrSessionLeaseLost} {
		t.Run(failure.Error(), func(t *testing.T) {
			ctx := context.Background()
			store := &nativeSelectionFailStore{MemorySessionStore: agentcore.NewMemorySessionStore(), selectionError: failure}
			ladder := testNativeLadder(t)
			binding, _, stream := ladder.binding()
			session, err := NewPiSession(ctx, PiSessionConfig{Pi: binding, nativeLadder: ladder, NativeStream: stream, Store: store, SessionID: "write-failure"})
			if err != nil {
				t.Fatal(err)
			}
			defer session.Close()
			before, err := session.State(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if err = session.selectNativeRung(session.ctx, 0, 1); !errors.Is(err, failure) {
				t.Fatalf("write failure lost: %v", err)
			}
			if ladder.selection().Generation != 0 || ladder.selection().Rung != 0 {
				t.Fatal("failed write published selection")
			}
			after, err := session.agent.State(ctx)
			if err != nil || string(before) != string(after) {
				t.Fatal("failed write changed agent state")
			}
			if !errors.Is(session.failure(), failure) {
				t.Fatal("write failure did not fence session")
			}
			entries, err := store.Log(ctx, "write-failure")
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range entries {
				if entry.Kind == agentcore.EntryPiModelSelection {
					t.Fatal("failed write entered journal")
				}
			}
		})
	}
}

func TestNativeLadderJournalConcurrentCheckpoints(t *testing.T) {
	ctx := context.Background()
	store := agentcore.NewMemorySessionStore()
	ladder := testNativeLadder(t)
	binding, _, stream := ladder.binding()
	session, err := NewPiSession(ctx, PiSessionConfig{Pi: binding, nativeLadder: ladder, NativeStream: stream, Store: store, SessionID: "checkpoint-race"})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 50; i++ {
			if err := session.selectNativeRung(session.ctx, uint64(i), (i+1)%2); err != nil {
				errs <- err
				return
			}
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 50; i++ {
			if err := session.checkpoint(session.ctx); err != nil {
				errs <- err
				return
			}
		}
	}()
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	entries, err := store.Log(ctx, "checkpoint-race")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = recoverPiState(entries); err != nil {
		t.Fatal(err)
	}
	restored := testNativeLadder(t)
	if err = restored.restoreJournal(entries); err != nil {
		t.Fatal(err)
	}
	if restored.selection().Generation != 50 || restored.selection().Rung != 0 {
		t.Fatal("concurrent checkpoint lost selection")
	}
}
