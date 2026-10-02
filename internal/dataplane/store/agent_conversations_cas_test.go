package storage

import (
	"context"
	"errors"
	"sync"
	"testing"
)

func TestConversationConditionalAppendRejectsStaleLeaf(t *testing.T) {
	s := openConvTestStore(t)
	user, project := seedConvProject(t, s)
	ctx := context.Background()
	conv, err := s.CreateConversation(ctx, user, project, "", "CAS")
	if err != nil {
		t.Fatal(err)
	}
	first, err := s.AppendConversationEntryAtLeaf(ctx, AgentConversationEntry{ConversationID: conv.ID, Kind: "message", PayloadJSON: msg("first")}, "")
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.AppendConversationEntryAtLeaf(ctx, AgentConversationEntry{ConversationID: conv.ID, Kind: "message", PayloadJSON: msg("stale")}, "")
	if !errors.Is(err, ErrConversationLeafChanged) {
		t.Fatalf("stale write was accepted: %v", err)
	}
	path, err := s.PathToLeaf(ctx, conv.ID)
	if err != nil || len(path) != 1 || path[0].ID != first.ID {
		t.Fatalf("stale append changed path: %+v %v", path, err)
	}
	all, err := s.ConversationEntries(ctx, user, project, conv.ID, 0)
	if err != nil || len(all) != 1 {
		t.Fatalf("stale append left orphan: %+v %v", all, err)
	}
	_, err = s.AppendConversationEntryAtLeaf(ctx, AgentConversationEntry{ConversationID: conv.ID, Kind: "message", PayloadJSON: msg("next")}, first.ID)
	if err != nil {
		t.Fatal(err)
	}
}

func TestConversationConcurrentAppendsAreSerialized(t *testing.T) {
	s := openConvTestStore(t)
	user, project := seedConvProject(t, s)
	ctx := context.Background()
	conv, err := s.CreateConversation(ctx, user, project, "", "parallel")
	if err != nil {
		t.Fatal(err)
	}
	const count = 24
	var wg sync.WaitGroup
	start := make(chan struct{})
	failures := make(chan error, count)
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := s.AppendConversationEntry(ctx, AgentConversationEntry{ConversationID: conv.ID, Kind: "message", PayloadJSON: msg("parallel")})
			failures <- err
		}()
	}
	close(start)
	wg.Wait()
	close(failures)
	for err := range failures {
		if err != nil {
			t.Fatal(err)
		}
	}
	path, err := s.PathToLeaf(ctx, conv.ID)
	if err != nil || len(path) != count {
		t.Fatalf("lost concurrent append: %d %v", len(path), err)
	}
	parent := ""
	for i, entry := range path {
		if entry.Seq != int64(i+1) || entry.ParentID != parent {
			t.Fatalf("broken sequence/chain: %+v", entry)
		}
		parent = entry.ID
	}
}
