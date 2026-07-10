package ingestion

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lohi-ai/agentray/internal/storage"
)

// fakeMsg is a test double for a durable (JetStream) message: it records how it
// was settled (ack / nak / term) and reports a fixed delivery count.
type fakeMsg struct {
	deliv   uint64
	payload []byte

	mu      sync.Mutex
	acked   bool
	nakked  bool
	termed  bool
}

func (m *fakeMsg) ack() error                { m.mu.Lock(); m.acked = true; m.mu.Unlock(); return nil }
func (m *fakeMsg) nak(time.Duration) error   { m.mu.Lock(); m.nakked = true; m.mu.Unlock(); return nil }
func (m *fakeMsg) term() error               { m.mu.Lock(); m.termed = true; m.mu.Unlock(); return nil }
func (m *fakeMsg) deliveries() uint64        { return m.deliv }
func (m *fakeMsg) body() []byte              { return m.payload }
func (m *fakeMsg) state() (bool, bool, bool) { m.mu.Lock(); defer m.mu.Unlock(); return m.acked, m.nakked, m.termed }

// A message whose insert succeeds must be acked (never redelivered).
func TestBatcherAcksOnSuccessfulInsert(t *testing.T) {
	sink := newRecordingSink()
	b := NewEventBatcher(sink.insert, EventBatcherConfig{MaxBatch: 1, FlushEvery: time.Hour})
	defer b.Stop()

	msg := &fakeMsg{deliv: 1}
	b.AddMsg(ev(1), msg)
	waitFor(t, sink.flushed)
	waitForState(t, msg, func() bool { a, _, _ := msg.state(); return a })

	if a, n, term := msg.state(); !a || n || term {
		t.Fatalf("want acked only; got ack=%v nak=%v term=%v", a, n, term)
	}
}

// A transient insert failure that recovers within the retry budget must still end
// in an ack — no redelivery, no data loss.
func TestBatcherRetriesThenAcks(t *testing.T) {
	var calls atomic.Int32
	sink := func(_ context.Context, _ []storage.Event) error {
		if calls.Add(1) < 3 {
			return errors.New("clickhouse blip")
		}
		return nil
	}
	metrics := NewPipelineMetrics(nil, "", time.Hour)
	b := NewEventBatcher(sink, EventBatcherConfig{MaxBatch: 1, FlushEvery: time.Hour, MaxRetries: 3, Metrics: metrics})
	defer b.Stop()

	msg := &fakeMsg{deliv: 1}
	b.AddMsg(ev(1), msg)
	waitForState(t, msg, func() bool { a, _, _ := msg.state(); return a })

	if a, n, term := msg.state(); !a || n || term {
		t.Fatalf("want acked after retry; got ack=%v nak=%v term=%v", a, n, term)
	}
	if metrics.retries.Load() == 0 {
		t.Fatal("want retries recorded")
	}
}

// A persistent failure below the redelivery ceiling must NAK (JetStream will
// redeliver), never drop.
func TestBatcherNaksBelowMaxDeliver(t *testing.T) {
	failing := func(_ context.Context, _ []storage.Event) error { return errors.New("clickhouse down") }
	b := NewEventBatcher(failing, EventBatcherConfig{MaxBatch: 1, FlushEvery: time.Hour, MaxRetries: 1, MaxDeliver: 5})
	defer b.Stop()

	msg := &fakeMsg{deliv: 2}
	b.AddMsg(ev(1), msg)
	waitForState(t, msg, func() bool { _, n, _ := msg.state(); return n })

	if a, n, term := msg.state(); a || !n || term {
		t.Fatalf("want nak only; got ack=%v nak=%v term=%v", a, n, term)
	}
}

// A persistent failure at the redelivery ceiling must be dead-lettered: the body
// is republished to the DLQ and the original terminated so it leaves the stream.
func TestBatcherDeadLettersAtMaxDeliver(t *testing.T) {
	failing := func(_ context.Context, _ []storage.Event) error { return errors.New("poison") }
	var dlq [][]byte
	var dlqMu sync.Mutex
	deadLetter := func(body []byte) error {
		dlqMu.Lock()
		dlq = append(dlq, body)
		dlqMu.Unlock()
		return nil
	}
	b := NewEventBatcher(failing, EventBatcherConfig{
		MaxBatch: 1, FlushEvery: time.Hour, MaxRetries: 1, MaxDeliver: 5, DeadLetter: deadLetter,
	})
	defer b.Stop()

	msg := &fakeMsg{deliv: 5, payload: []byte("poison-body")}
	b.AddMsg(ev(1), msg)
	waitForState(t, msg, func() bool { _, _, term := msg.state(); return term })

	if a, n, term := msg.state(); a || n || !term {
		t.Fatalf("want term only; got ack=%v nak=%v term=%v", a, n, term)
	}
	dlqMu.Lock()
	defer dlqMu.Unlock()
	if len(dlq) != 1 || string(dlq[0]) != "poison-body" {
		t.Fatalf("want body dead-lettered once, got %v", dlq)
	}
}

func waitForState(t *testing.T, _ *fakeMsg, ready func() bool) {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		if ready() {
			return
		}
		select {
		case <-deadline:
			t.Fatal("timed out waiting for message to settle")
		case <-time.After(5 * time.Millisecond):
		}
	}
}
