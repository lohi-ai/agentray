package ingestion

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"log"
	"strconv"
	"time"

	"github.com/lohi-ai/agentray/internal/dataplane/connector"
	"github.com/lohi-ai/agentray/internal/dataplane/store"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// OriginSubjectHeader names the subject a dead-lettered body came from, so
// `replay-dlq` puts it back on the subject whose decoder understands it.
// Without it a replayed connector batch would land on the events subject and be
// dead-lettered straight back.
const OriginSubjectHeader = "AgentRay-Origin-Subject"

// eventSink is the DuckDB write surface the worker needs. *storage.Store is the
// production implementation; *storage.DuckDB stands in for one blue-green colour
// in tests (batcher_ack_test.go already sinks events to it).
type eventSink interface {
	SinkEvents(ctx context.Context, events []storage.Event) error
	InsertExternalRows(ctx context.Context, projectID, connectorID, table string, rows []connector.LandedRow) error
}

// EventQueue is the ingestion publisher. With a JetStream context it publishes
// durably — the call waits for a broker ack, so an HTTP 200 to the SDK means the
// batch is safely stored and will be delivered to the worker even across a
// crash. Without one it falls back to fire-and-forget core NATS (dev/tests).
//
// It publishes two kinds of batch to one stream: event batches (subject) and
// connector sync batches (connectorSubject). Both ride the same per-colour
// durable, so a blue-green colour switch replays landed connector rows exactly
// like events — the fix for "connector rows bypass the durable stream".
type EventQueue struct {
	nc               *nats.Conn
	js               jetstream.JetStream
	subject          string
	connectorSubject string
}

// NewEventQueue builds the legacy fire-and-forget publisher.
func NewEventQueue(nc *nats.Conn, subject, connectorSubject string) EventQueue {
	return EventQueue{nc: nc, subject: subject, connectorSubject: connectorSubject}
}

// NewJetStreamQueue builds the durable publisher.
func NewJetStreamQueue(js jetstream.JetStream, subject, connectorSubject string) EventQueue {
	return EventQueue{js: js, subject: subject, connectorSubject: connectorSubject}
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

// ExternalRowsBatch is one connector sync batch in flight on the durable stream.
// Row bodies ride as json.RawMessage so neither side re-encodes the row JSON —
// the engine already holds it as a string and the landing table stores a string.
type ExternalRowsBatch struct {
	ProjectID   string        `json:"project_id"`
	ConnectorID string        `json:"connector_id"`
	Table       string        `json:"table"`
	Rows        []ExternalRow `json:"rows"`
}

// ExternalRow is one landed source row on the wire. The split from
// connector.LandedRow is deliberate — Data is json.RawMessage so the body
// neither gets re-encoded on the way out nor re-parsed on the way in — which
// means a field added to LandedRow must be added HERE too or it vanishes on the
// wire with no compile error and no test failure.
type ExternalRow struct {
	Key    string          `json:"key"`
	Cursor string          `json:"cursor"`
	Data   json.RawMessage `json:"data"`
}

// wireBytes estimates the row's contribution to a marshaled batch: the payload
// plus its JSON framing (the fixed keys, quotes and separators).
func (r ExternalRow) wireBytes() int {
	return len(r.Data) + len(r.Key) + len(r.Cursor) + 48
}

// LandedRows converts the wire form back into the engine's landing form.
func (b ExternalRowsBatch) LandedRows() []connector.LandedRow {
	out := make([]connector.LandedRow, 0, len(b.Rows))
	for _, r := range b.Rows {
		out = append(out, connector.LandedRow{Key: r.Key, Cursor: r.Cursor, DataJSON: string(r.Data)})
	}
	return out
}

// publishBudget is how large one connector message may get, taken from the
// broker's own advertised payload limit (an eighth left for framing, headers and
// the subject) rather than a fixed guess, so raising the limit raises the budget.
const publishFallbackBudget = 512 << 10

func (q EventQueue) publishBudget() int {
	nc := q.nc
	if q.js != nil {
		nc = q.js.Conn()
	}
	if nc == nil {
		return publishFallbackBudget
	}
	if max := int(nc.MaxPayload()); max > 0 {
		if budget := max - max/8; budget > 0 {
			return budget
		}
	}
	return publishFallbackBudget
}

// PublishExternalRows hands one pulled connector batch to the stream instead of
// writing it into this process's DuckDB. Both colours' durables receive it, each
// applying it to its own file, so the colour that did not run the sync is not
// stale after a switch — and because this call ack-waits before the caller
// advances the shared source cursor, the cursor can never move past a batch the
// stream did not accept.
//
// The landing key makes re-apply idempotent, so a redelivery (or a second
// colour applying the same rows) is a replace, not a duplicate.
//
// A row too large for one message fails the sync with the row named: the cursor
// holds, so both colours stay without it together (a loud, repairable stall)
// rather than one colour quietly holding a row the other never got.
func (q EventQueue) PublishExternalRows(ctx context.Context, projectID, connectorID, table string, rows []connector.LandedRow) error {
	if len(rows) == 0 {
		return nil
	}
	budget := q.publishBudget()
	for _, chunk := range chunkRows(rows, budget) {
		if len(chunk) == 1 && chunk[0].wireBytes() > budget {
			return fmt.Errorf("connector row %s is %d bytes, over the %d-byte publish budget: raise the broker's max_payload before this source can sync",
				chunk[0].Key, chunk[0].wireBytes(), budget)
		}
		body, err := json.Marshal(ExternalRowsBatch{
			ProjectID:   projectID,
			ConnectorID: connectorID,
			Table:       table,
			Rows:        chunk,
		})
		if err != nil {
			return fmt.Errorf("encode connector batch: %w", err)
		}
		if q.js != nil {
			// Same dedup key as the event path: a publish whose broker ack was
			// lost to a timeout is retried by the engine, and the stream should
			// collapse the identical body instead of storing a second copy that
			// every colour has to re-apply.
			if _, err := q.js.Publish(ctx, q.connectorSubject, body, jetstream.WithMsgID(bodyMsgID(body))); err != nil {
				return fmt.Errorf("publish connector batch: %w", err)
			}
			continue
		}
		if err := q.nc.Publish(q.connectorSubject, body); err != nil {
			return err
		}
		if err := q.nc.FlushTimeout(2 * time.Second); err != nil {
			return err
		}
	}
	return nil
}

// chunkRows splits a batch so no single message approaches the payload limit.
// The size is estimated per row rather than measured, so a chunk can overshoot
// by at most one row — which the eighth of headroom absorbs.
func chunkRows(rows []connector.LandedRow, maxBytes int) [][]ExternalRow {
	var out [][]ExternalRow
	var cur []ExternalRow
	size := 0
	for _, r := range rows {
		row := ExternalRow{Key: r.Key, Cursor: r.Cursor, Data: json.RawMessage(r.DataJSON)}
		rowBytes := row.wireBytes()
		if len(cur) > 0 && size+rowBytes > maxBytes {
			out = append(out, cur)
			cur, size = nil, 0
		}
		cur = append(cur, row)
		size += rowBytes
	}
	if len(cur) > 0 {
		out = append(out, cur)
	}
	return out
}

// bodyMsgID is a stable id for a marshaled batch, used as the JetStream
// deduplication key when a batch is first published.
func bodyMsgID(body []byte) string {
	h := fnv.New64a()
	_, _ = h.Write(body)
	return strconv.FormatUint(h.Sum64(), 16)
}

// EventWorker consumes queued events and writes them to DuckDB via the
// batcher. It holds either a legacy core-NATS subscription or a JetStream consume
// context, plus the metrics emitter on the durable path. Connector batches are
// settled inline (they arrive already batched) rather than through the batcher.
type EventWorker struct {
	sub     *nats.Subscription
	csub    *nats.Subscription
	consume jetstream.ConsumeContext
	batcher *EventBatcher
	metrics *PipelineMetrics
}

// StartEventWorker wires the legacy fire-and-forget consumer (core NATS). Used
// only when INGEST_JETSTREAM=false; a failed insert is logged and dropped.
func StartEventWorker(nc *nats.Conn, subject, connectorSubject string, sink eventSink) (*EventWorker, error) {
	ch := make(chan *nats.Msg, 1024)
	sub, err := nc.ChanQueueSubscribe(subject, "agentray-ingestors", ch)
	if err != nil {
		return nil, err
	}
	// Connector batches ride the same fire-and-forget path: no durability here,
	// but a self-hosted `docker compose up` must still land connector rows.
	csub, err := nc.ChanQueueSubscribe(connectorSubject, "agentray-ingestors", ch)
	if err != nil {
		_ = sub.Unsubscribe()
		return nil, err
	}
	if err := nc.FlushTimeout(2 * time.Second); err != nil {
		_ = sub.Unsubscribe()
		_ = csub.Unsubscribe()
		return nil, err
	}

	// Coalesce events across messages into larger DuckDB inserts instead of
	// one insert per message (which explodes the part count under load).
	batcher := NewEventBatcher(sink.SinkEvents, EventBatcherConfig{})

	go func() {
		for msg := range ch {
			if msg.Subject == connectorSubject {
				var batch ExternalRowsBatch
				if err := json.Unmarshal(msg.Data, &batch); err != nil {
					log.Printf("ingestion worker: decode connector batch: %v", err)
					continue
				}
				// Bounded like the durable path's insert: this goroutine also
				// drains event messages, so an unbounded write would stall all
				// ingestion behind one connector batch. Failure is still logged
				// and dropped — that is this mode's contract (see
				// StartEventWorker), and it is why the durable path exists.
				insertCtx, cancel := context.WithTimeout(context.Background(), connectorInsertTimeout)
				insertErr := sink.InsertExternalRows(insertCtx, batch.ProjectID, batch.ConnectorID, batch.Table, batch.LandedRows())
				cancel()
				if insertErr != nil {
					log.Printf("ingestion worker: insert connector batch: %v", insertErr)
				}
				continue
			}
			var events []storage.Event
			if err := json.Unmarshal(msg.Data, &events); err != nil {
				log.Printf("ingestion worker: decode event batch: %v", err)
				continue
			}
			batcher.Add(events)
		}
	}()

	return &EventWorker{sub: sub, csub: csub, batcher: batcher}, nil
}

// StartJetStreamWorker wires the durable consumer: a durable, explicit-ack
// consumer feeds the batcher, which acks each message only after its DuckDB
// insert lands and NAKs / dead-letters it otherwise. metrics may be nil.
//
// One consumer covers both subjects, so a single ack floor is this colour's
// applied mark for events and connector rows alike (see ReplayStatus).
func StartJetStreamWorker(ctx context.Context, ss *StreamSet, sink eventSink, metrics *PipelineMetrics) (*EventWorker, error) {
	dlqPublish := func(origin string, body []byte) error {
		pubCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		// PublishMsg, not Publish: the origin subject rides a header so
		// `replay-dlq` can put each body back on the subject that decodes it.
		_, err := ss.JS.PublishMsg(pubCtx, &nats.Msg{
			Subject: ss.DLQSubj,
			Data:    body,
			Header:  nats.Header{OriginSubjectHeader: []string{origin}},
		})
		return err
	}
	batcher := NewEventBatcher(sink.SinkEvents, EventBatcherConfig{
		MaxDeliver: ss.MaxDeliv,
		DeadLetter: func(body []byte) error { return dlqPublish(ss.Subject, body) },
		Metrics:    metrics,
	})
	settler := externalRowsSettler{
		sink:       sink,
		deadLetter: func(body []byte) error { return dlqPublish(ss.ConnectorSubject, body) },
		maxDeliver: ss.MaxDeliv,
		nakDelay:   connectorNakDelay,
		metrics:    metrics,
	}

	// FilterSubject is exclusive with FilterSubjects, so only the latter is set;
	// a broker too old for multiple filters fails here, loudly, instead of the
	// two DuckDB files drifting apart while the deploy thinks it succeeded.
	cons, err := ss.Ingest.CreateOrUpdateConsumer(ctx, jetstream.ConsumerConfig{
		Durable:        ss.durableName(),
		AckPolicy:      jetstream.AckExplicitPolicy,
		AckWait:        120 * time.Second,
		MaxAckPending:  8192,
		FilterSubjects: []string{ss.Subject, ss.ConnectorSubject},
	})
	if err != nil {
		batcher.Stop()
		return nil, fmt.Errorf("create ingest consumer: %w", err)
	}
	// Sample the retention gap now, before this process consumes anything: once
	// the replay advances the applied mark past the purge frontier the loss is no
	// longer visible in the broker's state (see latchBootGap).
	ss.latchBootGap(ctx, cons)
	consume, err := cons.Consume(func(msg jetstream.Msg) {
		if msg.Subject() == ss.ConnectorSubject {
			settler.settle(msg)
			return
		}
		var events []storage.Event
		if err := json.Unmarshal(msg.Data(), &events); err != nil {
			// Undecodable payload is poison — it will never insert. Dead-letter the
			// raw body (so an operator can inspect/replay it) and terminate so it
			// leaves the stream instead of redelivering forever. If the DLQ is
			// unreachable, NAK instead: one more redelivery beats losing the body.
			log.Printf("ingestion worker: decode event batch (dead-lettering): %v", err)
			if derr := dlqPublish(ss.Subject, msg.Data()); derr != nil {
				log.Printf("ingestion worker: dead-letter undecodable batch: %v", derr)
				_ = msg.NakWithDelay(5 * time.Second)
				return
			}
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

// externalRowsSettler applies a connector batch and settles its message with the
// batcher's policy, which it mirrors deliberately: ack only after the DuckDB
// write lands (that is what makes the consumer's ack floor the applied mark),
// a few bounded in-process retries for a blip, then NAK, then dead-letter once
// the message has exhausted its redeliveries. Connector messages need no
// coalescing — the engine already sends pullBatchSize rows per message.
type externalRowsSettler struct {
	sink       eventSink
	deadLetter func(body []byte) error
	maxDeliver int
	// nakDelay is the redelivery delay asked of JetStream. Zero means
	// connectorNakDelay; tests shorten it.
	nakDelay time.Duration
	metrics  *PipelineMetrics
}

// connectorInsertAttempts / connectorInsertTimeout / connectorNakDelay mirror
// EventBatcherConfig's defaults for the same failure.
const (
	connectorInsertAttempts = 3
	connectorInsertTimeout  = 10 * time.Second
	connectorNakDelay       = 5 * time.Second
)

func (s externalRowsSettler) settle(msg jetstream.Msg) {
	handle := jsMsgHandle{msg: msg}
	var batch ExternalRowsBatch
	if err := json.Unmarshal(msg.Data(), &batch); err != nil {
		log.Printf("ingestion worker: decode connector batch (dead-lettering): %v", err)
		s.poison(handle, msg.Data(), err)
		return
	}

	// Decoded once: the batch is immutable from here, and the retry path should
	// not re-allocate and re-copy every row payload per attempt.
	landed := batch.LandedRows()
	var err error
	for attempt := range connectorInsertAttempts {
		insertCtx, cancel := context.WithTimeout(context.Background(), connectorInsertTimeout)
		err = s.sink.InsertExternalRows(insertCtx, batch.ProjectID, batch.ConnectorID, batch.Table, landed)
		cancel()
		if err == nil {
			if ackErr := handle.ack(); ackErr != nil {
				log.Printf("ingestion worker: ack after connector insert: %v", ackErr)
			}
			return
		}
		if attempt < connectorInsertAttempts-1 {
			s.metrics.recordRetry()
			time.Sleep(backoff(attempt))
		}
	}

	s.metrics.recordInsertFailure()
	if handle.deliveries() >= uint64(s.maxDeliver) && s.deadLetter != nil {
		if derr := s.deadLetter(msg.Data()); derr != nil {
			log.Printf("ingestion worker: dead-letter connector batch failed, will retry: %v", derr)
			s.nak(handle)
			return
		}
		s.metrics.recordDeadLetter()
		_ = handle.term()
		log.Printf("ingestion worker: dead-lettered connector batch after %d deliveries: %v", handle.deliveries(), err)
		return
	}
	log.Printf("ingestion worker: connector insert failed, redelivering: %v", err)
	s.nak(handle)
}

// poison settles a connector batch that can never insert, with the same
// DLQ-then-terminate contract the batcher uses for events.
func (s externalRowsSettler) poison(handle jsMsgHandle, body []byte, cause error) {
	if s.deadLetter == nil {
		log.Printf("ingestion worker: connector poison batch has no DLQ, will retry: %v", cause)
		s.nak(handle)
		return
	}
	if err := s.deadLetter(body); err != nil {
		log.Printf("ingestion worker: dead-letter failed, will retry: %v", err)
		s.nak(handle)
		return
	}
	s.metrics.recordDeadLetter()
	_ = handle.term()
	log.Printf("ingestion worker: terminated poison connector batch: %v", cause)
}

func (s externalRowsSettler) nak(handle jsMsgHandle) {
	delay := s.nakDelay
	if delay <= 0 {
		delay = connectorNakDelay
	}
	_ = handle.nak(delay)
	s.metrics.recordNak()
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
	if w.csub != nil {
		if err := w.csub.Drain(); err != nil {
			return fmt.Errorf("drain connector worker: %w", err)
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
