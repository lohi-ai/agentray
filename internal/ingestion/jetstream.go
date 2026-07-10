package ingestion

import (
	"context"
	"fmt"
	"time"

	"github.com/lohi-ai/agentray/internal/config"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// StreamSet holds the JetStream handles the durable pipeline needs: the ingest
// stream (events awaiting a ClickHouse write) and the dead-letter stream (batches
// that exhausted redelivery). Both are file-backed so they survive a broker
// restart.
type StreamSet struct {
	JS       jetstream.JetStream
	Ingest   jetstream.Stream
	DLQ      jetstream.Stream
	Subject  string
	DLQSubj  string
	MaxDeliv int
}

// EnsureStreams connects a JetStream context on nc and idempotently provisions
// the ingest + DLQ streams. Safe to call on every boot: CreateOrUpdateStream
// reconciles config without dropping stored messages. The ingest stream keeps a
// short duplicate window so an accidental client re-publish of the identical
// body (same Nats-Msg-Id) collapses to one stored message.
func EnsureStreams(ctx context.Context, nc *nats.Conn, cfg config.Config) (*StreamSet, error) {
	js, err := jetstream.New(nc)
	if err != nil {
		return nil, fmt.Errorf("jetstream context: %w", err)
	}
	ingest, err := js.CreateOrUpdateStream(ctx, jetstream.StreamConfig{
		Name:      cfg.IngestStreamName,
		Subjects:  []string{cfg.IngestSubject},
		Storage:   jetstream.FileStorage,
		Retention: jetstream.LimitsPolicy,
		// A LimitsPolicy stream purges by age regardless of ack state, so MaxAge is
		// the outage window we can survive without losing un-processed events. NAK'd
		// messages dead-letter after MaxDeliver attempts (minutes), so the stream only
		// accumulates unacked messages during a *total* worker/ClickHouse outage —
		// give that a month of recovery slack (matching the DLQ retention) rather than
		// a week, so a long incident degrades to backlog, not data loss.
		MaxAge:     30 * 24 * time.Hour,
		Duplicates: 2 * time.Minute,
	})
	if err != nil {
		return nil, fmt.Errorf("ensure ingest stream %q: %w", cfg.IngestStreamName, err)
	}
	dlq, err := js.CreateOrUpdateStream(ctx, jetstream.StreamConfig{
		Name:      cfg.IngestStreamName + "_DLQ",
		Subjects:  []string{cfg.IngestDLQSubject},
		Storage:   jetstream.FileStorage,
		Retention: jetstream.LimitsPolicy,
		MaxAge:    30 * 24 * time.Hour,
	})
	if err != nil {
		return nil, fmt.Errorf("ensure dlq stream: %w", err)
	}
	maxDeliv := cfg.IngestMaxDeliver
	if maxDeliv <= 0 {
		maxDeliv = 5
	}
	return &StreamSet{
		JS:       js,
		Ingest:   ingest,
		DLQ:      dlq,
		Subject:  cfg.IngestSubject,
		DLQSubj:  cfg.IngestDLQSubject,
		MaxDeliv: maxDeliv,
	}, nil
}
