package ingestion

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"log"
	"strconv"
	"strings"
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
const (
	OriginSubjectHeader      = "AgentRay-Origin-Subject"
	OriginStreamHeader       = "AgentRay-Origin-Stream"
	OriginStreamSeqHeader    = "AgentRay-Origin-Stream-Sequence"
	OriginDigestHeader       = "AgentRay-Origin-Payload-SHA256"
	OriginPublishedAtHeader  = "AgentRay-Origin-Published-At"
	OriginUnverifiableHeader = "AgentRay-Origin-Unverifiable"
	LegacyDLQHeader          = "AgentRay-Legacy-DLQ"
)

// ingestStore is the DuckDB surface the worker needs. *storage.Store is the
// production implementation; *storage.DuckDB stands in for one blue-green colour
// in tests (batcher_ack_test.go already sinks events to it).
//
// The position methods are the readiness half of the same surface: the worker
// binds this store to the durable it consumes under at boot, and every write
// records how far along the stream it carries the file (see duckdb_position.go
// and ReplayStoreBehind).
type ingestStore interface {
	SinkEvents(ctx context.Context, events []storage.Event, mark storage.AppliedMark) error
	InsertExternalRows(ctx context.Context, projectID, connectorID, table string, rows []connector.LandedRow, mark storage.AppliedMark) error
	AppliedPosition(ctx context.Context, durable string) (storage.AppliedPosition, error)
	AdoptPosition(ctx context.Context, durable string, seq uint64) error
	RefusePosition(ctx context.Context, durable string, missing uint64) error
	// RecordPosition covers the settlements that carry no rows to write: an
	// empty batch, a poison payload leaving via the DLQ. See storage.RecordPosition.
	RecordPosition(ctx context.Context, mark storage.AppliedMark) error
	RecordReadinessHole(ctx context.Context, delivery storage.DeliveryReceiptMark, source *storage.SourceReceiptMark) error
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
	nc                  *nats.Conn
	js                  jetstream.JetStream
	subject             string
	connectorSubject    string
	publicationObserver interface {
		RecordPublication(context.Context, storage.PublicationObservation) error
	}
}

// NewEventQueue builds the legacy fire-and-forget publisher.
func NewEventQueue(nc *nats.Conn, subject, connectorSubject string) EventQueue {
	return EventQueue{nc: nc, subject: subject, connectorSubject: connectorSubject}
}

// NewJetStreamQueue builds the durable publisher.
func NewJetStreamQueue(js jetstream.JetStream, subject, connectorSubject string) EventQueue {
	return EventQueue{js: js, subject: subject, connectorSubject: connectorSubject}
}

func (q EventQueue) WithPublicationObserver(observer interface {
	RecordPublication(context.Context, storage.PublicationObservation) error
}) EventQueue {
	q.publicationObserver = observer
	return q
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
	ProjectID   string                     `json:"project_id"`
	ConnectorID string                     `json:"connector_id"`
	Table       string                     `json:"table"`
	Rows        []ExternalRow              `json:"rows"`
	Source      *storage.SourceReceiptMark `json:"source_receipt,omitempty"`
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

// wireBytes is an UPPER bound on the row's contribution to a marshaled batch:
// the payload plus its JSON framing (the fixed keys, quotes and separators).
//
// An upper bound, not len(): json.Marshal ESCAPES what these fields carry, and
// an escaped byte costs up to six. Counting raw lengths let a chunk whose keys,
// cursors or payload were escape-heavy exceed the broker's payload limit, which
// fails the publish and stalls the sync on every retry of the same batch. The
// row-level guard below reads the same number, so it stays an over-estimate —
// the safe direction for a message that must fit.
func (r ExternalRow) wireBytes() int {
	return rawJSONBytes(r.Data) + jsonStringBytes(r.Key) + jsonStringBytes(r.Cursor) + wireFramingBytes
}

// wireFramingBytes is everything a row's JSON costs beyond the three field
// bodies: `{"key":,"cursor":,"data":},` and the batch's own separator.
const wireFramingBytes = 44

// jsonStringBytes is an upper bound on the bytes json.Marshal writes for s,
// including its two quotes: one per plain ASCII character, six for everything
// the encoder escapes — a quote, a backslash, `<`, `>`, `&` (which it escapes
// by default), any control byte (`\u0000`), and any non-ASCII rune, which it
// may emit as `\uXXXX`.
func jsonStringBytes(s string) int {
	n := 2
	for _, r := range s {
		if r >= 0x20 && r < 0x80 && r != '"' && r != '\\' && r != '<' && r != '>' && r != '&' {
			n++
			continue
		}
		n += 6
	}
	return n
}

// rawJSONBytes is an upper bound on what a RawMessage contributes: itself,
// re-emitted rather than re-encoded, except that the encoder's HTML escaping
// still expands each `<`, `>` and `&` to six bytes. Bytes ≥ 0x80 pass through.
func rawJSONBytes(raw []byte) int {
	n := len(raw)
	for _, b := range raw {
		if b == '<' || b == '>' || b == '&' {
			n += 5
		}
	}
	return n
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
// A row too large for one message fails the sync with its size and the broker
// budget, but never its source key. The cursor holds, so both colours stay
// without it together rather than one colour quietly holding a row the other
// never got.
func (q EventQueue) PublishExternalRows(ctx context.Context, projectID, connectorID, table string, rows []connector.LandedRow) error {
	if len(rows) == 0 {
		return nil
	}
	budget := q.publishBudget()
	for _, chunk := range chunkRows(rows, budget) {
		if len(chunk) == 1 && chunk[0].wireBytes() > budget {
			return fmt.Errorf("connector row is %d bytes, over the %d-byte publish budget: raise the broker's max_payload before this source can sync",
				chunk[0].wireBytes(), budget)
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
			if admission, ok := q.publicationObserver.(interface{ AdmitDataPublication() error }); ok {
				if err := admission.AdmitDataPublication(); err != nil {
					return fmt.Errorf("admit connector publication: %w", err)
				}
			}
			// Same dedup key as the event path: a publish whose broker ack was
			// lost to a timeout is retried by the engine, and the stream should
			// collapse the identical body instead of storing a second copy that
			// every colour has to re-apply.
			if _, err := q.js.Publish(ctx, q.connectorSubject, body, jetstream.WithMsgID(bodyMsgID(body))); err != nil {
				return fmt.Errorf("publish connector batch: %w", err)
			}
			if q.publicationObserver != nil {
				digest := fmt.Sprintf("%x", sha256.Sum256(body))
				if err := q.publicationObserver.RecordPublication(ctx, storage.PublicationObservation{
					ProjectID: projectID, ConnectorID: connectorID, Table: table,
					StableBatchID: digest, PayloadSHA256: digest, PublishedAt: time.Now().UTC(),
				}); err != nil {
					return fmt.Errorf("record accepted connector publication: %w", err)
				}
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

func (q EventQueue) PublishIncrementalBatch(ctx context.Context, projectID, connectorID, table string, receipt connector.IncrementalReceipt, rows []connector.LandedRow) (int64, error) {
	chunks := chunkRows(rows, q.publishBudget())
	for i, chunk := range chunks {
		index := uint64(receipt.BatchIndex + int64(i))
		digestBody, err := json.Marshal(chunk)
		if err != nil {
			return int64(i), err
		}
		digest := fmt.Sprintf("%x", sha256.Sum256(digestBody))
		mark := &storage.SourceReceiptMark{ProjectID: projectID, ConnectorID: connectorID, Table: table,
			SyncID: receipt.SyncID, RunID: receipt.RunID, BatchID: fmt.Sprintf("batch-%06d", index),
			BatchIndex: &index, PayloadSHA256: digest, CaptureStartedAt: &receipt.CaptureStartedAt}
		if err := q.publishExternalEnvelope(ctx, ExternalRowsBatch{ProjectID: projectID, ConnectorID: connectorID, Table: table, Rows: chunk, Source: mark}); err != nil {
			return int64(i), err
		}
	}
	return int64(len(chunks)), nil
}

func (q EventQueue) PublishIncrementalComplete(ctx context.Context, projectID, connectorID, table string, receipt connector.IncrementalReceipt) error {
	expectedBatches, expectedRows := uint64(receipt.ExpectedBatches), uint64(receipt.ExpectedRows)
	mark := &storage.SourceReceiptMark{ProjectID: projectID, ConnectorID: connectorID, Table: table,
		SyncID: receipt.SyncID, RunID: receipt.RunID, ExpectedBatches: &expectedBatches, ExpectedRows: &expectedRows,
		CaptureStartedAt: &receipt.CaptureStartedAt, CaptureFinishedAt: receipt.CaptureFinishedAt, Complete: true}
	return q.publishExternalEnvelope(ctx, ExternalRowsBatch{ProjectID: projectID, ConnectorID: connectorID, Table: table, Rows: []ExternalRow{}, Source: mark})
}

func (q EventQueue) publishExternalEnvelope(ctx context.Context, batch ExternalRowsBatch) error {
	body, err := json.Marshal(batch)
	if err != nil {
		return fmt.Errorf("encode connector batch: %w", err)
	}
	published := time.Now().UTC()
	if q.js != nil {
		if admission, ok := q.publicationObserver.(interface{ AdmitDataPublication() error }); ok {
			if err := admission.AdmitDataPublication(); err != nil {
				return fmt.Errorf("admit connector publication: %w", err)
			}
		}
		if _, err := q.js.Publish(ctx, q.connectorSubject, body, jetstream.WithMsgID(bodyMsgID(body))); err != nil {
			return fmt.Errorf("publish connector batch: %w", err)
		}
	} else {
		if err := q.nc.Publish(q.connectorSubject, body); err != nil {
			return err
		}
		if err := q.nc.FlushTimeout(2 * time.Second); err != nil {
			return err
		}
	}
	if q.publicationObserver != nil && batch.Source != nil {
		mark := *batch.Source
		mark.PublishedAt = &published
		stableID := mark.RunID + ":" + mark.BatchID
		payloadDigest := mark.PayloadSHA256
		if mark.Complete {
			stableID = mark.RunID + ":complete"
			payloadDigest = fmt.Sprintf("%x", sha256.Sum256(body))
		}
		if err := q.publicationObserver.RecordPublication(ctx, storage.PublicationObservation{
			SourceReceiptMark: mark, StableBatchID: stableID, PayloadSHA256: payloadDigest, PublishedAt: published,
		}); err != nil {
			return fmt.Errorf("record accepted connector publication: %w", err)
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
func StartEventWorker(nc *nats.Conn, subject, connectorSubject string, sink ingestStore) (*EventWorker, error) {
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
				batch, snapshot, err := decodeConnectorEnvelope(msg.Data)
				if err != nil {
					log.Printf("ingestion worker: decode connector batch: %v", err)
					continue
				}
				if snapshot != nil {
					log.Printf("ingestion worker: snapshot envelope refused: durable JetStream is required")
					continue
				}
				// Bounded like the durable path's insert: this goroutine also
				// drains event messages, so an unbounded write would stall all
				// ingestion behind one connector batch. Failure is still logged
				// and dropped — that is this mode's contract (see
				// StartEventWorker), and it is why the durable path exists.
				insertCtx, cancel := context.WithTimeout(context.Background(), connectorInsertTimeout)
				insertErr := sink.InsertExternalRows(insertCtx, batch.ProjectID, batch.ConnectorID, batch.Table, batch.LandedRows(), storage.AppliedMark{})
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
func StartJetStreamWorker(ctx context.Context, ss *StreamSet, sink ingestStore, metrics *PipelineMetrics) (*EventWorker, error) {
	dlqPublish := func(origin string, delivery storage.DeliveryReceiptMark, body []byte) error {
		pubCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		// PublishMsg, not Publish: the origin subject rides a header so
		// `replay-dlq` can put each body back on the subject that decodes it.
		headers := nats.Header{OriginSubjectHeader: []string{origin}}
		if delivery.StreamSeq != 0 {
			headers.Set(OriginStreamHeader, delivery.StreamID)
			headers.Set(OriginStreamSeqHeader, strconv.FormatUint(delivery.StreamSeq, 10))
			headers.Set(OriginDigestHeader, delivery.PayloadSHA256)
			if delivery.PublishedAt != nil {
				headers.Set(OriginPublishedAtHeader, delivery.PublishedAt.UTC().Format(time.RFC3339Nano))
			}
			if delivery.Unverifiable {
				headers.Set(OriginUnverifiableHeader, "true")
			}
		}
		_, err := ss.JS.PublishMsg(pubCtx, &nats.Msg{
			Subject: ss.DLQSubj,
			Data:    body,
			Header:  headers,
		})
		return err
	}
	batcher := NewEventBatcher(sink.SinkEvents, EventBatcherConfig{
		MaxDeliver: ss.MaxDeliv,
		Durable:    ss.durableName(),
		DeadLetterWithReceipt: func(delivery storage.DeliveryReceiptMark, body []byte) error {
			return dlqPublish(ss.Subject, delivery, body)
		},
		RecordPosition: sink.RecordPosition,
		RecordHole:     sink.RecordReadinessHole,
		Metrics:        metrics,
	})
	settler := externalRowsSettler{
		sink: sink,
		deadLetterWithReceipt: func(delivery storage.DeliveryReceiptMark, body []byte) error {
			return dlqPublish(ss.ConnectorSubject, delivery, body)
		},
		recordHole: sink.RecordReadinessHole,
		maxDeliver: ss.MaxDeliv,
		durable:    ss.durableName(),
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
	// Sample the broker now, before this process consumes anything: the store
	// binding and the retention gap are both questions about what this colour
	// had applied before it started applying, and the replay advancing is what
	// makes them unanswerable (see bindStore, latchBootGap). A boot that cannot
	// answer them does not start consuming: every ack would move the durable's
	// floor over a file whose provenance was never established, and the write
	// would hide the hole for good.
	ss.Positions = sink
	cinfo, streamInfo, err := ss.bootSample(ctx, cons)
	if err != nil {
		batcher.Stop()
		return nil, fmt.Errorf("boot replay sample unreadable: %w", err)
	}
	if err := ss.bindStore(ctx, cinfo); err != nil {
		batcher.Stop()
		return nil, fmt.Errorf("bind this colour's store to durable %q: %w", ss.durableName(), err)
	}
	ss.latchBootGap(cinfo, streamInfo)
	streamID := streamIncarnation(streamInfo.Config.Name, streamInfo.Created)
	if missing := ss.bootGap.Load(); missing > 0 || ss.bootUnverified.Load() {
		seq := missing
		if seq == 0 {
			seq = 1
		}
		digest := fmt.Sprintf("%x", sha256.Sum256([]byte("retention-loss\x00"+streamID)))
		if err := sink.RecordReadinessHole(ctx, storage.DeliveryReceiptMark{StreamID: streamID,
			Subject: "__retention_loss__", StreamSeq: seq, PayloadSHA256: digest, Unverifiable: true}, nil); err != nil {
			batcher.Stop()
			return nil, fmt.Errorf("record retention-loss readiness evidence: %w", err)
		}
	}
	settler.streamID = streamID
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
			// Recorded before the dead-letter and the terminate: terminating the
			// delivery moves the consumer's ack floor, so the store has to cover
			// the position or its next boot reads the difference as a lost range.
			// If the record will not land, the message is retried instead.
			if recErr := recordSettled(sink, ss.durableName(), jsMsgHandle{msg: msg, streamID: streamID}.seq()); recErr != nil {
				log.Printf("ingestion worker: %v; redelivering the undecodable batch instead of settling over it", recErr)
				_ = msg.NakWithDelay(5 * time.Second)
				return
			}
			delivery := jsMsgHandle{msg: msg, streamID: streamID}.delivery()
			if derr := dlqPublish(ss.Subject, delivery, msg.Data()); derr != nil {
				log.Printf("ingestion worker: dead-letter undecodable batch: %v", derr)
				_ = msg.NakWithDelay(5 * time.Second)
				return
			}
			if herr := sink.RecordReadinessHole(context.Background(), delivery, nil); herr != nil {
				log.Printf("ingestion worker: record undecodable readiness hole: %v", herr)
				_ = msg.NakWithDelay(5 * time.Second)
				return
			}
			_ = msg.Term()
			return
		}
		batcher.AddMsg(events, jsMsgHandle{msg: msg, streamID: streamID})
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
	sink                  ingestStore
	deadLetter            func([]byte) error
	deadLetterWithReceipt func(storage.DeliveryReceiptMark, []byte) error
	recordHole            func(context.Context, storage.DeliveryReceiptMark, *storage.SourceReceiptMark) error
	maxDeliver            int
	// durable names the consumer these batches arrive under; connector rows are
	// recorded against it exactly as events are, because one consumer carries
	// both subjects (see storage.AppliedMark).
	durable  string
	streamID string
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
	handle := jsMsgHandle{msg: msg, streamID: s.streamID}
	batch, snapshot, err := decodeConnectorEnvelope(msg.Data())
	if err != nil {
		log.Printf("ingestion worker: decode connector batch (dead-lettering): %v", err)
		s.poison(handle, msg.Data(), err)
		return
	}
	delivery := handle.delivery()
	var source *storage.SourceReceiptMark
	if snapshot != nil {
		source = sourceReceiptFromSnapshotEnvelope(*snapshot, delivery.PublishedAt)
		delivery.ProjectID, delivery.ConnectorID, delivery.Table = snapshot.ProjectID, snapshot.ConnectorID, snapshot.Table
	} else {
		delivery.ProjectID, delivery.ConnectorID, delivery.Table = batch.ProjectID, batch.ConnectorID, batch.Table
		if batch.Source != nil {
			copy := *batch.Source
			copy.Promoted = true
			if copy.PublishedAt == nil {
				copy.PublishedAt = delivery.PublishedAt
			}
			source = &copy
		}
	}
	deliveries := []storage.DeliveryReceiptMark{delivery}
	var landed []connector.LandedRow
	if batch != nil {
		landed = batch.LandedRows()
	}
	for attempt := range connectorInsertAttempts {
		insertCtx, cancel := context.WithTimeout(context.Background(), connectorInsertTimeout)
		if snapshot != nil {
			if snapshotSink, ok := s.sink.(interface {
				ApplySnapshotEnvelope(context.Context, connector.SnapshotEnvelope, storage.AppliedMark) (*connector.SnapshotPromotion, error)
			}); ok {
				_, err = snapshotSink.ApplySnapshotEnvelope(insertCtx, *snapshot, storage.AppliedMark{Durable: s.durable, Seq: handle.seq(), Deliveries: deliveries})
			} else {
				err = fmt.Errorf("snapshot landing is unavailable")
			}
		} else {
			err = s.sink.InsertExternalRows(insertCtx, batch.ProjectID, batch.ConnectorID, batch.Table, landed, storage.AppliedMark{Durable: s.durable, Seq: handle.seq(), Deliveries: deliveries, Source: source})
		}
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
	if errors.Is(err, storage.ErrDataCapacity) {
		log.Printf("ingestion worker: connector capacity unavailable, redelivering: %v", err)
		s.nak(handle)
		return
	}
	if handle.deliveries() >= uint64(s.maxDeliver) && (s.deadLetter != nil || s.deadLetterWithReceipt != nil) {
		// Recorded before the dead-letter and the terminate: settling moves the
		// ack floor, so a store that cannot take the record leaves the batch to be
		// retried instead of being left behind the floor.
		if err := recordSettled(s.sink, s.durable, handle.seq()); err != nil {
			log.Printf("ingestion worker: %v; redelivering the connector batch instead of settling over it", err)
			s.nak(handle)
			return
		}
		if derr := s.publishDeadLetter(delivery, msg.Data()); derr != nil {
			log.Printf("ingestion worker: dead-letter connector batch failed, will retry: %v", derr)
			s.nak(handle)
			return
		}
		if s.recordHole != nil {
			if herr := s.recordHole(context.Background(), delivery, source); herr != nil {
				log.Printf("ingestion worker: record connector readiness hole: %v", herr)
				s.nak(handle)
				return
			}
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
	if s.deadLetter == nil && s.deadLetterWithReceipt == nil {
		log.Printf("ingestion worker: connector poison batch has no DLQ, will retry: %v", cause)
		s.nak(handle)
		return
	}
	if err := recordSettled(s.sink, s.durable, handle.seq()); err != nil {
		log.Printf("ingestion worker: %v; redelivering the poison connector batch instead of settling over it", err)
		s.nak(handle)
		return
	}
	delivery := handle.delivery()
	if err := s.publishDeadLetter(delivery, body); err != nil {
		log.Printf("ingestion worker: dead-letter failed, will retry: %v", err)
		s.nak(handle)
		return
	}
	if s.recordHole != nil {
		if err := s.recordHole(context.Background(), delivery, nil); err != nil {
			log.Printf("ingestion worker: record connector readiness hole: %v", err)
			s.nak(handle)
			return
		}
	}
	s.metrics.recordDeadLetter()
	_ = handle.term()
	log.Printf("ingestion worker: terminated poison connector batch: %v", cause)
}

func (s externalRowsSettler) publishDeadLetter(delivery storage.DeliveryReceiptMark, body []byte) error {
	if s.deadLetterWithReceipt != nil {
		return s.deadLetterWithReceipt(delivery, body)
	}
	return s.deadLetter(body)
}

// recordSettled moves a store's applied position over a delivery that settles
// without a row write — an empty batch, a poison payload dead-lettered and
// terminated. Both acking and terminating advance the consumer's ack floor, so
// skipping the record here leaves the store behind its own floor and the next
// boot refuses a colour that lost nothing (see storage.RecordPosition).
//
// The error is returned and the callers do not settle without it: retrying the
// delivery costs one redelivery, while settling over a failed record would
// advance the ack floor past a position the store never wrote down.
func recordSettled(sink ingestStore, durable string, seq uint64) error {
	if durable == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), connectorInsertTimeout)
	defer cancel()
	if err := sink.RecordPosition(ctx, storage.AppliedMark{Durable: durable, Seq: seq}); err != nil {
		return fmt.Errorf("record applied position %d for durable %q: %w", seq, durable, err)
	}
	return nil
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
type jsMsgHandle struct {
	msg      jetstream.Msg
	streamID string
}

func (h jsMsgHandle) ack() error                { return h.msg.Ack() }
func (h jsMsgHandle) nak(d time.Duration) error { return h.msg.NakWithDelay(d) }
func (h jsMsgHandle) term() error               { return h.msg.Term() }
func (h jsMsgHandle) body() []byte              { return h.msg.Data() }
func (h jsMsgHandle) delivery() storage.DeliveryReceiptMark {
	data := h.msg.Data()
	digest := fmt.Sprintf("%x", sha256.Sum256(data))
	headers := h.msg.Headers()
	if seq, err := strconv.ParseUint(headers.Get(OriginStreamSeqHeader), 10, 64); err == nil && seq > 0 {
		published, _ := time.Parse(time.RFC3339Nano, headers.Get(OriginPublishedAtHeader))
		if original := strings.TrimSpace(headers.Get(OriginDigestHeader)); original != "" {
			digest = original
		}
		return storage.DeliveryReceiptMark{StreamID: headers.Get(OriginStreamHeader), Subject: headers.Get(OriginSubjectHeader),
			StreamSeq: seq, PayloadSHA256: digest, PublishedAt: timePtr(published), Replayed: true,
			Unverifiable: headers.Get(OriginUnverifiableHeader) == "true"}
	}
	md, err := h.msg.Metadata()
	if err != nil {
		return storage.DeliveryReceiptMark{Subject: h.msg.Subject(), PayloadSHA256: digest}
	}
	published := md.Timestamp.UTC()
	legacy := headers.Get(LegacyDLQHeader) == "true"
	streamID := h.streamID
	if streamID == "" {
		streamID = md.Stream
	}
	return storage.DeliveryReceiptMark{StreamID: streamID, Subject: h.msg.Subject(), StreamSeq: md.Sequence.Stream,
		PayloadSHA256: digest, PublishedAt: &published, Replayed: legacy, Unverifiable: legacy}
}

func streamIncarnation(name string, created time.Time) string {
	if created.IsZero() {
		return name
	}
	return name + "@" + created.UTC().Format(time.RFC3339Nano)
}

func timePtr(v time.Time) *time.Time {
	if v.IsZero() {
		return nil
	}
	v = v.UTC()
	return &v
}
func (h jsMsgHandle) deliveries() uint64 {
	md, err := h.msg.Metadata()
	if err != nil {
		return 0
	}
	return md.NumDelivered
}

// seq is this delivery's consumer sequence — the total number of deliveries
// this consumer has made, redeliveries included. It is the position the DuckDB
// write records, and the same space AckFloor.Consumer reports in, so the two are
// comparable however wide the stream is: a stream shared with another
// environment interleaves its sequences with ours, and its traffic never
// advances our consumer's delivery counter.
func (h jsMsgHandle) seq() uint64 {
	md, err := h.msg.Metadata()
	if err != nil {
		return 0
	}
	return md.Sequence.Consumer
}
