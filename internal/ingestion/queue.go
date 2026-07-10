package ingestion

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"log"
	"strconv"
	"time"

	"github.com/lohi-ai/agentray/internal/storage"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// EventQueue is the ingestion publisher. With a JetStream context it publishes
// durably — the call waits for a broker ack, so an HTTP 200 to the SDK means the
// batch is safely stored and will be delivered to the worker even across a
// crash. Without one it falls back to fire-and-forget core NATS (dev/tests).
type EventQueue struct {
	nc      *nats.Conn
	js      jetstream.JetStream
	subject string
}

// NewEventQueue builds the legacy fire-and-forget publisher.
func NewEventQueue(nc *nats.Conn, subject string) EventQueue {
	return EventQueue{nc: nc, subject: subject}
}

// NewJetStreamQueue builds the durable publisher.
func NewJetStreamQueue(js jetstream.JetStream, subject string) EventQueue {
	return EventQueue{js: js, subject: subject}
}

func (q EventQueue) InsertEvents(ctx context.Context, events []storage.Event) error {
	body, err := json.Marshal(events)
	if err != nil {
		return err
	}
	if q.js != nil {
		// Publish-with-ack: the deduplication id collapses an accidental identical
		// re-publish within the stream's Duplicates window to one stored message.
		if _, err := q.js.Publish(ctx, q.subject, body, jetstream.WithMsgID(bodyMsgID(body))); err != nil {
			return err
		}
		return nil
	}
	if err := q.nc.Publish(q.subject, body); err != nil {
		return err
	}
	return q.nc.FlushTimeout(2 * time.Second)
}

// bodyMsgID is a stable id for a marshaled batch, used as the JetStream
// deduplication key.
func bodyMsgID(body []byte) string {
	h := fnv.New64a()
	_, _ = h.Write(body)
	return strconv.FormatUint(h.Sum64(), 16)
}

// BodyMsgID exposes the same batch-body dedup key to the DLQ replay path (in the
// server binary) so a re-queued dead-letter carries the original message's
// deduplication id — making an interrupted or repeated replay idempotent within
// the ingest stream's Duplicates window instead of re-inserting the batch.
func BodyMsgID(body []byte) string { return bodyMsgID(body) }

// EventWorker consumes queued events and writes them to ClickHouse via the
// batcher. It holds either a legacy core-NATS subscription or a JetStream consume
// context, plus the metrics emitter on the durable path.
type EventWorker struct {
	sub     *nats.Subscription
	consume jetstream.ConsumeContext
	batcher *EventBatcher
	metrics *PipelineMetrics
}

// StartEventWorker wires the legacy fire-and-forget consumer (core NATS). Used
// only when INGEST_JETSTREAM=false; a failed insert is logged and dropped.
func StartEventWorker(nc *nats.Conn, subject string, store *storage.Store) (*EventWorker, error) {
	ch := make(chan *nats.Msg, 1024)
	sub, err := nc.ChanQueueSubscribe(subject, "agentray-ingestors", ch)
	if err != nil {
		return nil, err
	}
	if err := nc.FlushTimeout(2 * time.Second); err != nil {
		_ = sub.Unsubscribe()
		return nil, err
	}

	// Coalesce events across messages into larger ClickHouse inserts instead of
	// one insert per message (which explodes the part count under load).
	batcher := NewEventBatcher(store.SinkEvents, EventBatcherConfig{})

	go func() {
		for msg := range ch {
			var events []storage.Event
			if err := json.Unmarshal(msg.Data, &events); err != nil {
				log.Printf("ingestion worker: decode event batch: %v", err)
				continue
			}
			batcher.Add(events)
		}
	}()

	return &EventWorker{sub: sub, batcher: batcher}, nil
}

// StartJetStreamWorker wires the durable consumer: a durable, explicit-ack
// consumer feeds the batcher, which acks each message only after its ClickHouse
// insert lands and NAKs / dead-letters it otherwise. metrics may be nil.
func StartJetStreamWorker(ctx context.Context, ss *StreamSet, store *storage.Store, metrics *PipelineMetrics) (*EventWorker, error) {
	dlqPublish := func(body []byte) error {
		pubCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, err := ss.JS.Publish(pubCtx, ss.DLQSubj, body)
		return err
	}
	batcher := NewEventBatcher(store.SinkEvents, EventBatcherConfig{
		MaxDeliver: ss.MaxDeliv,
		DeadLetter: dlqPublish,
		Metrics:    metrics,
	})

	cons, err := ss.Ingest.CreateOrUpdateConsumer(ctx, jetstream.ConsumerConfig{
		Durable:       "agentray-ingestors",
		AckPolicy:     jetstream.AckExplicitPolicy,
		AckWait:       120 * time.Second,
		MaxAckPending: 8192,
		FilterSubject: ss.Subject,
	})
	if err != nil {
		batcher.Stop()
		return nil, fmt.Errorf("create ingest consumer: %w", err)
	}
	consume, err := cons.Consume(func(msg jetstream.Msg) {
		var events []storage.Event
		if err := json.Unmarshal(msg.Data(), &events); err != nil {
			// Undecodable payload is poison — it will never insert. Terminate so it
			// leaves the stream instead of redelivering forever.
			log.Printf("ingestion worker: decode event batch (terminating): %v", err)
			_ = msg.Term()
			return
		}
		batcher.AddMsg(events, jsMsgHandle{msg: msg})
	})
	if err != nil {
		batcher.Stop()
		return nil, fmt.Errorf("consume ingest stream: %w", err)
	}
	metrics.Start()
	return &EventWorker{consume: consume, batcher: batcher, metrics: metrics}, nil
}

func (w *EventWorker) Stop() error {
	if w == nil {
		return nil
	}
	// Stop pulling new work first so the batcher drains a fixed set.
	if w.consume != nil {
		w.consume.Stop()
	}
	if w.sub != nil {
		if err := w.sub.Drain(); err != nil {
			return fmt.Errorf("drain event worker: %w", err)
		}
	}
	if w.batcher != nil {
		w.batcher.Stop()
	}
	if w.metrics != nil {
		w.metrics.Stop()
	}
	return nil
}

// jsMsgHandle adapts a JetStream message to the batcher's msgHandle contract.
type jsMsgHandle struct{ msg jetstream.Msg }

func (h jsMsgHandle) ack() error                { return h.msg.Ack() }
func (h jsMsgHandle) nak(d time.Duration) error { return h.msg.NakWithDelay(d) }
func (h jsMsgHandle) term() error               { return h.msg.Term() }
func (h jsMsgHandle) body() []byte              { return h.msg.Data() }
func (h jsMsgHandle) deliveries() uint64 {
	md, err := h.msg.Metadata()
	if err != nil {
		return 0
	}
	return md.NumDelivered
}
