package ingestion

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/lohi-ai/agentray/internal/storage"
)

// recordingSink captures every flushed batch so tests can assert on coalescing.
type recordingSink struct {
	mu      sync.Mutex
	batches [][]storage.Event
	flushed chan struct{}
}

func newRecordingSink() *recordingSink {
	return &recordingSink{flushed: make(chan struct{}, 64)}
}

func (r *recordingSink) insert(_ context.Context, events []storage.Event) error {
	r.mu.Lock()
	cp := append([]storage.Event(nil), events...)
	r.batches = append(r.batches, cp)
	r.mu.Unlock()
	r.flushed <- struct{}{}
	return nil
}

func (r *recordingSink) snapshot() [][]storage.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([][]storage.Event(nil), r.batches...)
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
	for i := range out {
		out[i] = storage.Event{EventName: "user.pageview"}
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
