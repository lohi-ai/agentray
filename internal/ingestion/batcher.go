package ingestion

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/lohi-ai/agentray/internal/storage"
)

func logBatchError(err error) {
	log.Printf("ingestion batcher: insert event batch: %v", err)
}

// msgHandle is the acknowledgement surface the batcher needs from a durable
// (JetStream) message. The JetStream worker supplies a real one so a batch is
// acked only after its ClickHouse insert succeeds and NAK'd (redelivered) or
// dead-lettered otherwise. The legacy core-NATS path supplies no handle
// (fire-and-forget: a failed insert is logged and dropped, as before).
type msgHandle interface {
	ack() error
	nak(delay time.Duration) error
	term() error
	deliveries() uint64
	body() []byte
}

// queued pairs a decoded message's events with its (optional) ack handle so the
// batcher can coalesce events from many messages into one ClickHouse insert yet
// still acknowledge each source message correctly.
type queued struct {
	events []storage.Event
	msg    msgHandle // nil on the legacy fire-and-forget path
}

// EventBatcher coalesces events arriving from many small NATS messages into
// larger inserts before they reach ClickHouse. ClickHouse is an OLAP store that
// wants few, large inserts: every INSERT creates a part, and a flood of
// single-row inserts (one per browser `capture`) explodes the part count and
// the background-merge load. The worker hands every decoded message to Add/AddMsg;
// the batcher flushes a combined slice either when it reaches maxBatch rows or
// when flushEvery elapses, whichever comes first — so latency stays bounded even
// when traffic is light.
//
// On the durable path each flush acknowledges its source messages only after the
// insert succeeds. A transient failure gets a few quick in-process retries; if it
// still fails the messages are NAK'd so JetStream redelivers them later (bounded
// backoff), and a message that has exhausted maxDeliver attempts is dead-lettered
// so one poison batch never wedges the stream.
type EventBatcher struct {
	sink       func(ctx context.Context, events []storage.Event) error
	deadLetter func(body []byte) error
	metrics    *PipelineMetrics
	maxBatch   int
	flushEvery time.Duration
	insertTO   time.Duration
	maxRetries int
	maxDeliver int
	nakDelay   time.Duration

	in   chan queued
	done chan struct{}
	wg   sync.WaitGroup
}

// EventBatcherConfig tunes the batcher. Zero values fall back to sensible
// defaults so callers only set what they care about.
type EventBatcherConfig struct {
	MaxBatch      int           // flush once this many rows are buffered (default 500)
	FlushEvery    time.Duration // flush at least this often (default 1s)
	InsertTimeout time.Duration // per-flush context timeout (default 10s)
	QueueDepth    int           // pending-message buffer (default 1024)
	MaxRetries    int           // quick in-process insert retries before NAK/drop (default 3)
	MaxDeliver    int           // redelivery attempts before dead-lettering (default 5)
	NakDelay      time.Duration // delay asked of JetStream on NAK (default 5s)

	// DeadLetter republishes a poison batch's raw body to the DLQ. Nil disables
	// dead-lettering (the batch is NAK'd indefinitely instead).
	DeadLetter func(body []byte) error
	// Metrics, when set, counts flush/failure/retry/nak/dead-letter activity.
	Metrics *PipelineMetrics
}

// NewEventBatcher constructs a batcher around a sink (typically
// store.InsertEvents) and starts its flush loop. Call Stop to drain.
func NewEventBatcher(sink func(ctx context.Context, events []storage.Event) error, cfg EventBatcherConfig) *EventBatcher {
	if cfg.MaxBatch <= 0 {
		cfg.MaxBatch = 500
	}
	if cfg.FlushEvery <= 0 {
		cfg.FlushEvery = time.Second
	}
	if cfg.InsertTimeout <= 0 {
		cfg.InsertTimeout = 10 * time.Second
	}
	if cfg.QueueDepth <= 0 {
		cfg.QueueDepth = 1024
	}
	if cfg.MaxRetries <= 0 {
		cfg.MaxRetries = 3
	}
	if cfg.MaxDeliver <= 0 {
		cfg.MaxDeliver = 5
	}
	if cfg.NakDelay <= 0 {
		cfg.NakDelay = 5 * time.Second
	}
	b := &EventBatcher{
		sink:       sink,
		deadLetter: cfg.DeadLetter,
		metrics:    cfg.Metrics,
		maxBatch:   cfg.MaxBatch,
		flushEvery: cfg.FlushEvery,
		insertTO:   cfg.InsertTimeout,
		maxRetries: cfg.MaxRetries,
		maxDeliver: cfg.MaxDeliver,
		nakDelay:   cfg.NakDelay,
		in:         make(chan queued, cfg.QueueDepth),
		done:       make(chan struct{}),
	}
	b.wg.Add(1)
	go b.loop()
	return b
}

// Add hands a decoded message's events to the batcher on the legacy fire-and-
// forget path (no acknowledgement). It never blocks the caller on the flush
// itself — only on the bounded queue when truly saturated, which is the back-
// pressure we want.
func (b *EventBatcher) Add(events []storage.Event) {
	if len(events) == 0 {
		return
	}
	b.in <- queued{events: events}
}

// AddMsg hands a durable message's events plus its ack handle to the batcher. The
// handle is acked after a successful insert, or NAK'd / dead-lettered on failure.
func (b *EventBatcher) AddMsg(events []storage.Event, msg msgHandle) {
	if len(events) == 0 {
		// Nothing to insert, but the message must still be acked or it redelivers
		// forever.
		if msg != nil {
			_ = msg.ack()
		}
		return
	}
	b.in <- queued{events: events, msg: msg}
}

func (b *EventBatcher) loop() {
	defer b.wg.Done()
	ticker := time.NewTicker(b.flushEvery)
	defer ticker.Stop()

	var buf []queued
	count := 0
	flush := func() {
		if len(buf) == 0 {
			return
		}
		b.flush(buf)
		buf = nil
		count = 0
	}

	for {
		select {
		case q := <-b.in:
			buf = append(buf, q)
			count += len(q.events)
			if count >= b.maxBatch {
				flush()
			}
		case <-ticker.C:
			flush()
		case <-b.done:
			// Drain anything still queued, then the buffer, before returning.
			for {
				select {
				case q := <-b.in:
					buf = append(buf, q)
				default:
					flush()
					return
				}
			}
		}
	}
}

// flush inserts every buffered message's events in one ClickHouse write and then
// settles each source message (ack on success; NAK or dead-letter on failure).
func (b *EventBatcher) flush(items []queued) {
	total := 0
	for _, it := range items {
		total += len(it.events)
	}
	all := make([]storage.Event, 0, total)
	for _, it := range items {
		all = append(all, it.events...)
	}

	if err := b.sinkWithRetry(all); err == nil {
		b.metrics.recordFlush(len(all), ingestLagMS(all))
		for _, it := range items {
			if it.msg != nil {
				if ackErr := it.msg.ack(); ackErr != nil {
					// Insert succeeded but the ack didn't land; the message will
					// redeliver and re-insert. The JetStream duplicate window absorbs a
					// redelivery of the identical body, and the only money-critical read
					// (the retention "ever paid" flag) aggregates with max(), so a rare
					// surviving duplicate can't corrupt it. Count/sum rollups tolerate the
					// near-zero residual dup rate per the data-architecture doc; there is
					// no read-time insert_id de-dup, so do not claim one here.
					log.Printf("ingestion batcher: ack after insert: %v", ackErr)
				}
			}
		}
		return
	} else {
		b.metrics.recordInsertFailure()
		b.settleFailure(items, err)
	}
}

// settleFailure decides, per source message, whether to redeliver (NAK) or
// dead-letter the batch after the insert exhausted its in-process retries.
func (b *EventBatcher) settleFailure(items []queued, cause error) {
	for _, it := range items {
		if it.msg == nil {
			// Legacy fire-and-forget path: no durability, drop as before.
			logBatchError(cause)
			continue
		}
		if it.msg.deliveries() >= uint64(b.maxDeliver) && b.deadLetter != nil {
			if derr := b.deadLetter(it.msg.body()); derr != nil {
				// Couldn't dead-letter (DLQ unreachable); keep the message alive by
				// asking for another redelivery rather than losing it.
				log.Printf("ingestion batcher: dead-letter failed, will retry: %v", derr)
				_ = it.msg.nak(b.nakDelay)
				b.metrics.recordNak()
				continue
			}
			_ = it.msg.term()
			b.metrics.recordDeadLetter()
			log.Printf("ingestion batcher: dead-lettered poison batch after %d deliveries: %v", it.msg.deliveries(), cause)
			continue
		}
		_ = it.msg.nak(b.nakDelay)
		b.metrics.recordNak()
	}
}

// sinkWithRetry does a few quick, bounded retries with exponential backoff to ride
// out a transient ClickHouse blip without a full redelivery cycle. On a longer
// outage it gives up and returns the error so the caller NAKs (JetStream then owns
// the slower redelivery/backoff).
func (b *EventBatcher) sinkWithRetry(events []storage.Event) error {
	var err error
	for attempt := 0; attempt < b.maxRetries; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), b.insertTO)
		err = b.sink(ctx, events)
		cancel()
		if err == nil {
			return nil
		}
		if attempt < b.maxRetries-1 {
			b.metrics.recordRetry()
			time.Sleep(backoff(attempt))
		}
	}
	return err
}

// backoff is the in-process retry schedule: 200ms, 400ms, 800ms, capped at 2s.
func backoff(attempt int) time.Duration {
	d := 200 * time.Millisecond << attempt
	if d > 2*time.Second {
		d = 2 * time.Second
	}
	return d
}

// ingestLagMS is now minus the oldest event's origin timestamp in the batch — how
// far behind real time the pipeline is running, in milliseconds. Reported as the
// pipeline's headline lag gauge.
func ingestLagMS(events []storage.Event) int {
	if len(events) == 0 {
		return 0
	}
	oldest := events[0].Timestamp
	for _, e := range events[1:] {
		if e.Timestamp.Before(oldest) {
			oldest = e.Timestamp
		}
	}
	lag := time.Since(oldest).Milliseconds()
	if lag < 0 {
		return 0
	}
	return int(lag)
}

// Stop signals the loop to drain and waits for the final flush to complete.
func (b *EventBatcher) Stop() {
	close(b.done)
	b.wg.Wait()
}
