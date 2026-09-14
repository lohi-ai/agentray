package ingestion

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/lohi-ai/agentray/internal/dataplane/store"
)

// recordingSink captures every flushed batch so tests can assert on coalescing.
// It also captures the position mark each flush carries, which is what the
// store records for the readiness binding.
type recordingSink struct {
	mu      sync.Mutex
	batches [][]storage.Event
	marks   []storage.AppliedMark
	flushed chan struct{}
}

func newRecordingSink() *recordingSink {
	return &recordingSink{flushed: make(chan struct{}, 64)}
}

func (r *recordingSink) insert(_ context.Context, events []storage.Event, mark storage.AppliedMark) error {
	r.mu.Lock()
	cp := append([]storage.Event(nil), events...)
	r.batches = append(r.batches, cp)
	r.marks = append(r.marks, mark)
	r.mu.Unlock()
	r.flushed <- struct{}{}
	return nil
}

func (r *recordingSink) snapshot() [][]storage.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([][]storage.Event(nil), r.batches...)
}

// markSnapshot is the position mark of every flush, in flush order.
func (r *recordingSink) markSnapshot() []storage.AppliedMark {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]storage.AppliedMark(nil), r.marks...)
}

func (r *recordingSink) totalRows() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, b := range r.batches {
		n += len(b)
	}
	return n
}

func ev(n int) []storage.Event {
	out := make([]storage.Event, n)
	projectID := uuid.NewString()
	for i := range out {
		out[i] = storage.Event{
			ProjectID:  projectID,
			EventID:    uuid.NewString(),
			EventName:  "user.pageview",
			DistinctID: "test-user",
			Timestamp:  time.Now().UTC(),
		}
	}
	return out
}

func waitFor(t *testing.T, ch chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for a flush")
	}
}

// A burst of single-event messages must be coalesced into one insert once the
// size threshold is reached — that is the whole point versus inserting per msg.
func TestEventBatcherFlushesBySize(t *testing.T) {
	sink := newRecordingSink()
	b := NewEventBatcher(sink.insert, EventBatcherConfig{MaxBatch: 5, FlushEvery: time.Hour})
	defer b.Stop()

	for i := 0; i < 5; i++ {
		b.Add(ev(1))
	}
	waitFor(t, sink.flushed)

	got := sink.snapshot()
	if len(got) != 1 {
		t.Fatalf("want 1 coalesced insert, got %d", len(got))
	}
	if len(got[0]) != 5 {
		t.Fatalf("want 5 rows in the insert, got %d", len(got[0]))
	}
}

// Below the size threshold, the time trigger must still drain the buffer so
// low-traffic events are not stranded.
func TestEventBatcherFlushesByTime(t *testing.T) {
	sink := newRecordingSink()
	b := NewEventBatcher(sink.insert, EventBatcherConfig{MaxBatch: 1000, FlushEvery: 20 * time.Millisecond})
	defer b.Stop()

	b.Add(ev(3))
	waitFor(t, sink.flushed)

	if rows := sink.totalRows(); rows != 3 {
		t.Fatalf("want 3 rows flushed by timer, got %d", rows)
	}
}

// Stop must drain whatever is still buffered before returning.
func TestEventBatcherDrainsOnStop(t *testing.T) {
	sink := newRecordingSink()
	b := NewEventBatcher(sink.insert, EventBatcherConfig{MaxBatch: 1000, FlushEvery: time.Hour})

	b.Add(ev(7))
	b.Stop()

	if rows := sink.totalRows(); rows != 7 {
		t.Fatalf("want 7 rows drained on stop, got %d", rows)
	}
}

// The position a flush records under is the HIGHEST consumer sequence among its
// messages: the store's record has to dominate every message the batch is about
// to acknowledge, or the readiness binding would refuse a colour for messages
// that are in its file.
func TestEventBatcherRecordsHighestDeliveryPosition(t *testing.T) {
	sink := newRecordingSink()
	b := NewEventBatcher(sink.insert, EventBatcherConfig{
		MaxBatch: 1000, FlushEvery: time.Hour, Durable: "colour-mark",
	})

	b.AddMsg(ev(1), &fakeMsg{deliv: 1, seqN: 4})
	b.AddMsg(ev(1), &fakeMsg{deliv: 1, seqN: 9})
	b.AddMsg(ev(1), &fakeMsg{deliv: 2, seqN: 6})
	b.Stop()

	marks := sink.markSnapshot()
	if len(marks) != 1 {
		t.Fatalf("flush marks = %v, want one coalesced flush", marks)
	}
	if marks[0].Durable != "colour-mark" || marks[0].Seq != 9 {
		t.Fatalf("flush mark = %+v, want colour-mark at the highest delivery 9", marks[0])
	}
}

// A single message's events insert atomically — never split across inserts — so
// the durable path can ack that message on exactly one successful flush. A
// message that alone exceeds maxBatch simply triggers an immediate one-shot
// flush of its events (bounded by the SDK/HTTP payload size, not left unbounded).
func TestEventBatcherFlushesOversizedMessageAtomically(t *testing.T) {
	sink := newRecordingSink()
	b := NewEventBatcher(sink.insert, EventBatcherConfig{MaxBatch: 10, FlushEvery: time.Hour})

	b.Add(ev(25))
	waitFor(t, sink.flushed)
	b.Stop()

	got := sink.snapshot()
	if len(got) != 1 {
		t.Fatalf("want 1 atomic insert, got %d: %v", len(got), got)
	}
	if len(got[0]) != 25 {
		t.Fatalf("want 25 rows in the single insert, got %d", len(got[0]))
	}
}
