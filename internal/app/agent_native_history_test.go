package app

import (
	"context"
	"fmt"
	"testing"

	storage "github.com/lohi-ai/agentray/internal/dataplane/store"
	agentruntime "github.com/lohi-ai/agentray/internal/runtime"
)

type nativeHistoryEntries map[string]storage.AgentConversationEntry

func TestNativeConversationDisplayPreservesBranchWithoutReplayPayload(t *testing.T) {
	for _, kind := range []string{agentruntime.ConvKindPiHistory, agentruntime.ConvKindPiCompaction} {
		entry := storage.AgentConversationEntry{ID: "entry", ParentID: "parent", Seq: 3, Kind: kind, PayloadJSON: `{"private":"native replay"}`, TokenEstimate: 123}
		display := conversationDisplayEntry(entry)
		if display.ID != entry.ID || display.ParentID != entry.ParentID || display.Seq != entry.Seq || display.TokenEstimate != entry.TokenEstimate || display.PayloadJSON != "{}" {
			t.Fatalf("lost branch or exposed replay: %+v", display)
		}
		if kind == agentruntime.ConvKindPiCompaction && display.Kind != agentruntime.ConvKindCompaction {
			t.Fatal("native compaction lost existing UI marker")
		}
		if entry.PayloadJSON == "{}" || entry.Kind != kind {
			t.Fatal("display changed stored replay data")
		}
	}
}

func (entries nativeHistoryEntries) GetConversationEntry(_ context.Context, _ string, id string) (storage.AgentConversationEntry, error) {
	if entry, ok := entries[id]; ok {
		return entry, nil
	}
	return storage.AgentConversationEntry{}, fmt.Errorf("missing entry %s", id)
}
func TestNativeRegenerateUsesConsumedInputBeforePendingCorrection(t *testing.T) {
	entries := nativeHistoryEntries{
		"user":    {ID: "user", Kind: agentruntime.ConvKindMessage, Role: "user"},
		"pending": {ID: "pending", ParentID: "user", Kind: agentruntime.ConvKindMessage, Role: "user"},
		"native":  {ID: "native", ParentID: "pending", Kind: agentruntime.ConvKindPiHistory, PayloadJSON: `{"base_entry_id":"","revision":"r","messages":[{"role":"user","content":"question","agentrayInputId":"user"},{"role":"assistant","content":[]}]}`},
		"trace":   {ID: "trace", ParentID: "native", Kind: agentruntime.ConvKindToolTrace},
	}
	user, err := userTurnAbove(context.Background(), entries, "conversation", "trace")
	if err != nil || user.ID != "user" {
		t.Fatalf("regenerate selected an unconsumed correction: %+v %v", user, err)
	}
	// Old transcripts still walk ordinary parents through human-only entries.
	user, err = userTurnAbove(context.Background(), entries, "conversation", "pending")
	if err != nil || user.ID != "pending" {
		t.Fatalf("legacy parent walk changed: %+v %v", user, err)
	}
}
