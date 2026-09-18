package agentcore

import (
	"context"
	"errors"
	"testing"
	"time"
)

type countingLeaseStore struct {
	*MemorySessionStore
	acquires int
}

type leaseLossStore struct {
	*MemorySessionStore
	cancel context.CancelCauseFunc
}

func (s *leaseLossStore) AcquireSessionLease(ctx context.Context, _ string) (context.Context, func() error, error) {
	leaseCtx, cancel := context.WithCancelCause(ctx)
	s.cancel = cancel
	return leaseCtx, func() error { return nil }, nil
}

func (s *leaseLossStore) AppendBatch(ctx context.Context, id string, entries []SessionEntry) error {
	for _, entry := range entries {
		if entry.Kind == EntryLeaf {
			s.cancel(ErrSessionLeaseLost)
			return ErrSessionLeaseLost
		}
	}
	return s.MemorySessionStore.AppendBatch(ctx, id, entries)
}

func (s *countingLeaseStore) AcquireSessionLease(ctx context.Context, id string) (context.Context, func() error, error) {
	s.acquires++
	return s.MemorySessionStore.AcquireSessionLease(ctx, id)
}

func TestMemorySessionLeaseSerializesOneSession(t *testing.T) {
	store := NewMemorySessionStore()
	_, releaseFirst, err := AcquireSessionLease(context.Background(), store, "shared")
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}

	acquired := make(chan func() error, 1)
	go func() {
		_, release, err := AcquireSessionLease(context.Background(), store, "shared")
		if err == nil {
			acquired <- release
		}
	}()
	select {
	case <-acquired:
		t.Fatal("second owner acquired the same live session")
	case <-time.After(25 * time.Millisecond):
	}

	// A different session is independent and must not queue behind shared.
	_, releaseOther, err := AcquireSessionLease(context.Background(), store, "other")
	if err != nil {
		t.Fatalf("independent acquire: %v", err)
	}
	if err := releaseOther(); err != nil {
		t.Fatalf("release independent: %v", err)
	}

	if err := releaseFirst(); err != nil {
		t.Fatalf("release first: %v", err)
	}
	var releaseSecond func() error
	select {
	case releaseSecond = <-acquired:
	case <-time.After(time.Second):
		t.Fatal("second owner did not acquire after release")
	}
	if err := releaseSecond(); err != nil {
		t.Fatalf("release second: %v", err)
	}
	if err := releaseSecond(); err != nil { // idempotent
		t.Fatalf("second release: %v", err)
	}
}

func TestMemorySessionLeaseCanceledWaiter(t *testing.T) {
	store := NewMemorySessionStore()
	_, release, err := AcquireSessionLease(context.Background(), store, "shared")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = release() }()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, _, err = AcquireSessionLease(ctx, store, "shared")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waiter error = %v, want deadline exceeded", err)
	}
}

func TestAcquireSessionLeaseNestedContextDoesNotDeadlock(t *testing.T) {
	store := NewMemorySessionStore()
	ctx, release, err := AcquireSessionLease(context.Background(), store, "shared")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = release() }()

	_, nestedRelease, err := AcquireSessionLease(ctx, store, "shared")
	if err != nil {
		t.Fatalf("nested acquire: %v", err)
	}
	if err := nestedRelease(); err != nil {
		t.Fatalf("nested release: %v", err)
	}
}

func TestRecordSessionAnswerIsIdempotentAndRejectsConflict(t *testing.T) {
	ctx := context.Background()
	store := NewMemorySessionStore()
	if err := store.Append(ctx, "s", SessionEntry{Kind: EntryQuestion, CallID: "call-1"}); err != nil {
		t.Fatal(err)
	}
	appended, err := RecordSessionAnswer(ctx, store, "s", "call-1", "yes")
	if err != nil || !appended {
		t.Fatalf("first answer: appended=%v err=%v", appended, err)
	}
	appended, err = RecordSessionAnswer(ctx, store, "s", "call-1", "yes")
	if err != nil || appended {
		t.Fatalf("exact retry: appended=%v err=%v", appended, err)
	}
	if _, err := RecordSessionAnswer(ctx, store, "s", "call-1", "no"); !errors.Is(err, ErrAnswerConflict) {
		t.Fatalf("conflicting retry error = %v, want ErrAnswerConflict", err)
	}
	log, err := store.Log(ctx, "s")
	if err != nil {
		t.Fatal(err)
	}
	answers := 0
	for _, entry := range log {
		if entry.Kind == EntryAnswer {
			answers++
		}
	}
	if answers != 1 {
		t.Fatalf("answer entries = %d, want 1", answers)
	}
}

func TestResumeAcquiresOnceAndReusesOuterLease(t *testing.T) {
	store := &countingLeaseStore{MemorySessionStore: NewMemorySessionStore()}
	agent, err := New(Config{
		Provider:      NewFauxProvider(AssistantText("done")),
		Model:         "test",
		Tools:         NewToolSet(),
		Policy:        DenyAll{},
		Session:       store,
		SessionID:     "resume-once",
		ResumeSession: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, release, err := AcquireSessionLease(context.Background(), store, "resume-once")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = release() }()
	if _, err := agent.Prompt(ctx, "continue"); err != nil {
		t.Fatal(err)
	}
	if store.acquires != 1 {
		t.Fatalf("backend lease acquisitions = %d, want outer acquisition only", store.acquires)
	}
}

func TestResumePromotesLeaseLossFromFinalSavePoint(t *testing.T) {
	store := &leaseLossStore{MemorySessionStore: NewMemorySessionStore()}
	agent, err := New(Config{
		Provider:      NewFauxProvider(AssistantText("done")),
		Model:         "test",
		Tools:         NewToolSet(),
		Policy:        DenyAll{},
		Session:       store,
		SessionID:     "lease-loss",
		ResumeSession: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = agent.Prompt(context.Background(), "continue")
	if !errors.Is(err, ErrSessionLeaseLost) {
		t.Fatalf("Prompt error = %v, want ErrSessionLeaseLost", err)
	}
}
