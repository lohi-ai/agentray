package main

import (
	"context"
	"testing"
	"time"

	"github.com/lohi-ai/agentray/internal/dataplane/ingest"
	"github.com/lohi-ai/agentray/internal/shared/config"
	server "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
)

func TestReplayDLQPreservesIdentityAndMarksLegacyUnknown(t *testing.T) {
	dir := t.TempDir()
	options := &server.Options{JetStream: true, StoreDir: dir, Port: -1}
	srv, err := server.NewServer(options)
	if err != nil {
		t.Fatal(err)
	}
	srv.Start()
	t.Cleanup(srv.Shutdown)
	if !srv.ReadyForConnections(10 * time.Second) {
		t.Fatal("nats server did not become ready")
	}

	cfg := config.Config{NATSURL: srv.ClientURL(), IngestJetStream: true,
		IngestStreamName: "REPLAY_TEST", IngestSubject: "replay.events", IngestConnectorSubject: "replay.connectors",
		IngestDLQSubject: "replay.dlq", IngestDurable: "replay-worker", IngestMaxDeliver: 3}
	nc, err := nats.Connect(cfg.NATSURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(nc.Close)
	streams, err := ingestion.EnsureStreams(context.Background(), nc, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := streams.JS.Publish(context.Background(), cfg.IngestDLQSubject, []byte("legacy")); err != nil {
		t.Fatal(err)
	}
	modern := &nats.Msg{Subject: cfg.IngestDLQSubject, Data: []byte("modern"), Header: nats.Header{}}
	modern.Header.Set(ingestion.OriginSubjectHeader, cfg.IngestConnectorSubject)
	modern.Header.Set(ingestion.OriginStreamHeader, "OLD_STREAM")
	modern.Header.Set(ingestion.OriginStreamSeqHeader, "42")
	modern.Header.Set(ingestion.OriginDigestHeader, "original-digest")
	modern.Header.Set(ingestion.OriginUnverifiableHeader, "true")
	if _, err := streams.JS.PublishMsg(context.Background(), modern); err != nil {
		t.Fatal(err)
	}

	if err := replayDLQ(cfg); err != nil {
		t.Fatal(err)
	}
	legacy, err := streams.Ingest.GetMsg(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if legacy.Subject != cfg.IngestSubject || legacy.Header.Get(ingestion.LegacyDLQHeader) != "true" {
		t.Fatalf("legacy replay = subject %q headers %v", legacy.Subject, legacy.Header)
	}
	replayed, err := streams.Ingest.GetMsg(context.Background(), 2)
	if err != nil {
		t.Fatal(err)
	}
	if replayed.Subject != cfg.IngestConnectorSubject || replayed.Header.Get(ingestion.OriginStreamSeqHeader) != "42" ||
		replayed.Header.Get(ingestion.OriginDigestHeader) != "original-digest" || replayed.Header.Get(ingestion.OriginUnverifiableHeader) != "true" {
		t.Fatalf("identity-bearing replay = subject %q headers %v", replayed.Subject, replayed.Header)
	}
}
