package ingestion

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"
	"time"

	storage "github.com/lohi-ai/agentray/internal/dataplane/store"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

func TestDataReadinessConsumesFrozenC1WireFixtures(t *testing.T) {
	body, err := os.ReadFile("testdata/c1-wire-fixtures.json")
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	if got := hex.EncodeToString(sum[:]); got != "01d422ae2b008b5d16490a9dec28ec6077a4fbd95b5074305a93581d0c8a5e58" {
		t.Fatalf("fixture digest = %s", got)
	}
	var fixture struct {
		Batches       []c1ReceiptFixture `json:"batches"`
		Complete      c1ReceiptFixture   `json:"complete"`
		EmptyComplete c1ReceiptFixture   `json:"empty_complete"`
	}
	if err := json.Unmarshal(body, &fixture); err != nil {
		t.Fatal(err)
	}
	if len(fixture.Batches) != 2 || fixture.Complete.Kind != "complete" || fixture.EmptyComplete.ExpectedBatches == nil || *fixture.EmptyComplete.ExpectedBatches != 0 {
		t.Fatalf("fixture shape = %+v", fixture)
	}
	published := time.Date(2026, 10, 1, 0, 5, 1, 0, time.UTC)
	mark := fixture.Complete.receipt(&published)
	if mark == nil || !mark.Complete || mark.Generation == "" || mark.SyncID == "" || mark.ExpectedBatches == nil || *mark.ExpectedBatches != 2 {
		t.Fatalf("completion receipt = %+v", mark)
	}
}

func TestDataReadinessStreamByteCapIsOptInAndPreserved(t *testing.T) {
	url := startBroker(t)
	nc, err := nats.Connect(url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(nc.Close)
	cfg := testConfig("cap-test")
	cfg.IngestStreamMaxBytes = 4096
	if _, err := EnsureStreams(context.Background(), nc, cfg); err != nil {
		t.Fatal(err)
	}
	cfg.IngestStreamMaxBytes = 0
	streams, err := EnsureStreams(context.Background(), nc, cfg)
	if err != nil {
		t.Fatal(err)
	}
	info, err := streams.Ingest.Info(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if info.Config.MaxBytes != 4096 || info.Config.Discard != jetstream.DiscardNew {
		t.Fatalf("startup erased operator byte policy: max=%d discard=%v", info.Config.MaxBytes, info.Config.Discard)
	}
}

type c1ReceiptFixture struct {
	Kind              string     `json:"kind"`
	ProjectID         string     `json:"project_id"`
	ConnectorID       string     `json:"connector_id"`
	Table             string     `json:"table"`
	SyncID            string     `json:"sync_id"`
	RunID             string     `json:"run_id"`
	Generation        string     `json:"generation"`
	BindingDigest     string     `json:"binding_digest"`
	GenerationSeq     uint64     `json:"generation_seq"`
	BatchID           string     `json:"batch_id"`
	BatchIndex        *uint64    `json:"batch_index"`
	PayloadSHA256     string     `json:"payload_sha256"`
	ExpectedBatches   *uint64    `json:"expected_batches"`
	ExpectedRows      *uint64    `json:"expected_rows"`
	CaptureStartedAt  *time.Time `json:"capture_started_at"`
	CaptureFinishedAt *time.Time `json:"capture_finished_at"`
}

func (f c1ReceiptFixture) receipt(published *time.Time) *storage.SourceReceiptMark {
	return &storage.SourceReceiptMark{ProjectID: f.ProjectID, ConnectorID: f.ConnectorID, Table: f.Table,
		SyncID: f.SyncID, RunID: f.RunID, Generation: f.Generation, GenerationSeq: f.GenerationSeq,
		BindingDigest: f.BindingDigest, BatchID: f.BatchID, BatchIndex: f.BatchIndex, PayloadSHA256: f.PayloadSHA256,
		ExpectedBatches: f.ExpectedBatches, ExpectedRows: f.ExpectedRows, CaptureStartedAt: f.CaptureStartedAt,
		CaptureFinishedAt: f.CaptureFinishedAt, PublishedAt: published, Complete: f.Kind == "complete"}
}
