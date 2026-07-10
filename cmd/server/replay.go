package main

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/lohi-ai/agentray/internal/config"
	"github.com/lohi-ai/agentray/internal/ingestion"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// replayDLQ drains the dead-letter stream and republishes each batch onto the
// ingest subject so the durable worker retries it. Run it as an operator after
// fixing whatever made a batch poison (e.g. a ClickHouse schema mismatch):
//
//	agentray-server replay-dlq
//
// It lives in the server binary, not the client CLI, because it needs direct
// queue access — which the analytics CLI is forbidden to hold (AGENT-GOVERNANCE).
// Ack-after-republish means an interrupted replay is safe to re-run: only
// batches confirmed re-queued are removed from the DLQ.
func replayDLQ(cfg config.Config) error {
	if !cfg.IngestJetStream {
		return fmt.Errorf("replay-dlq requires INGEST_JETSTREAM=true (no durable stream otherwise)")
	}
	ctx := context.Background()
	nc, err := nats.Connect(cfg.NATSURL, nats.Name("AgentRay dlq-replay"))
	if err != nil {
		return fmt.Errorf("connect nats: %w", err)
	}
	defer nc.Close()

	ss, err := ingestion.EnsureStreams(ctx, nc, cfg)
	if err != nil {
		return err
	}
	cons, err := ss.DLQ.CreateOrUpdateConsumer(ctx, jetstream.ConsumerConfig{
		Durable:   "agentray-dlq-replay",
		AckPolicy: jetstream.AckExplicitPolicy,
		AckWait:   30 * time.Second,
	})
	if err != nil {
		return fmt.Errorf("create dlq consumer: %w", err)
	}

	replayed := 0
	for {
		batch, err := cons.Fetch(100, jetstream.FetchMaxWait(2*time.Second))
		if err != nil {
			return fmt.Errorf("fetch dlq: %w", err)
		}
		got := 0
		for msg := range batch.Messages() {
			got++
			if _, perr := ss.JS.Publish(ctx, ss.Subject, msg.Data(), jetstream.WithMsgID(ingestion.BodyMsgID(msg.Data()))); perr != nil {
				// Leave it in the DLQ (do not ack) so a later run retries it.
				log.Printf("replay-dlq: republish failed, leaving in DLQ: %v", perr)
				continue
			}
			if aerr := msg.Ack(); aerr != nil {
				log.Printf("replay-dlq: ack after republish: %v", aerr)
			}
			replayed++
		}
		if err := batch.Error(); err != nil {
			return fmt.Errorf("dlq batch: %w", err)
		}
		if got == 0 {
			break // stream drained
		}
	}
	log.Printf("replay-dlq: republished %d dead-lettered batch(es) to %s", replayed, ss.Subject)
	return nil
}
