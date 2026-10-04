package ingestion

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lohi-ai/agentray/internal/dataplane/connector"
	storage "github.com/lohi-ai/agentray/internal/dataplane/store"
	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

type snapshotAdmissionProbe struct {
	admissions   int
	publications int
}

func (p *snapshotAdmissionProbe) AdmitDataPublication() error {
	p.admissions++
	return storage.ErrDataCapacity
}
func (p *snapshotAdmissionProbe) RecordPublication(context.Context, storage.PublicationObservation) error {
	p.publications++
	return nil
}

func TestSnapshotPublicationRespectsCapacityAdmission(t *testing.T) {
	ctx := context.Background()
	url := startBroker(t)
	nc, err := nats.Connect(url)
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	streams, err := EnsureStreams(ctx, nc, testConfig("snapshot-admission"))
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Batches []connector.SnapshotEnvelope `json:"batches"`
	}
	raw, err := os.ReadFile(filepath.Join("testdata", "c1-wire-v1.json"))
	if err != nil || json.Unmarshal(raw, &fixture) != nil || len(fixture.Batches) == 0 {
		t.Fatalf("fixture err=%v batches=%d", err, len(fixture.Batches))
	}
	probe := &snapshotAdmissionProbe{}
	queue := NewJetStreamQueue(streams.JS, streams.Subject, streams.ConnectorSubject).WithPublicationObserver(probe)
	if err := queue.PublishSnapshotEnvelope(ctx, fixture.Batches[0]); !errors.Is(err, storage.ErrDataCapacity) {
		t.Fatalf("snapshot admission error=%v", err)
	}
	info, err := streams.Ingest.Info(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if probe.admissions != 1 || probe.publications != 0 || info.State.Msgs != 0 {
		t.Fatalf("admissions=%d publications=%d broker_messages=%d", probe.admissions, probe.publications, info.State.Msgs)
	}
}

func TestSnapshotFrozenWireFixture(t *testing.T) {
	localPath := filepath.Join("testdata", "c1-wire-v1.json")
	ticketPath := "/Users/long/.babysit/projects/lohi-ai-agentray/tickets/bs-ordhkfdt/c1-wire-fixtures.json"
	local, err := os.ReadFile(localPath)
	if err != nil {
		t.Fatal(err)
	}
	ticket, err := os.ReadFile(ticketPath)
	if err == nil && !bytes.Equal(local, ticket) {
		t.Fatal("runtime fixture drifted from frozen ticket fixture")
	}
	var fixture struct {
		Batches  []connector.SnapshotEnvelope `json:"batches"`
		Complete connector.SnapshotEnvelope   `json:"complete"`
		Empty    connector.SnapshotEnvelope   `json:"empty_complete"`
	}
	if err := json.Unmarshal(local, &fixture); err != nil {
		t.Fatal(err)
	}
	for _, env := range append(append(fixture.Batches, fixture.Complete), fixture.Empty) {
		if err := env.Validate(); err != nil {
			t.Fatalf("fixture invalid: %v", err)
		}
		raw, err := json.Marshal(env)
		if err != nil {
			t.Fatal(err)
		}
		legacy, snapshot, err := decodeConnectorEnvelope(raw)
		if err != nil || legacy != nil || snapshot == nil {
			t.Fatalf("dispatch legacy=%v snapshot=%v err=%v", legacy, snapshot, err)
		}
	}
	first, err := json.Marshal(fixture.Batches[0])
	if err != nil {
		t.Fatal(err)
	}
	var firstFields map[string]json.RawMessage
	if err := json.Unmarshal(first, &firstFields); err != nil {
		t.Fatal(err)
	}
	if _, ok := firstFields["batch_index"]; !ok {
		t.Fatalf("zero batch_index omitted: %s", first)
	}
	empty, err := json.Marshal(fixture.Empty)
	if err != nil {
		t.Fatal(err)
	}
	var emptyFields map[string]json.RawMessage
	if err := json.Unmarshal(empty, &emptyFields); err != nil {
		t.Fatal(err)
	}
	if _, ok := emptyFields["expected_batches"]; !ok {
		t.Fatalf("zero expected_batches omitted: %s", empty)
	}
	if _, ok := emptyFields["rows"]; ok {
		t.Fatalf("completion carried rows: %s", empty)
	}
}

func TestSnapshotDispatchFailsClosed(t *testing.T) {
	for _, raw := range [][]byte{
		[]byte(`{"protocol":"unknown.v1","project_id":"p"}`),
		[]byte(`{"project_id":"p","connector_id":"c","table":"t","generation":"g","rows":[]}`),
	} {
		if _, _, err := decodeConnectorEnvelope(raw); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
	legacyRaw := []byte(`{"project_id":"p","connector_id":"c","table":"t","rows":[]}`)
	legacy, snapshot, err := decodeConnectorEnvelope(legacyRaw)
	if err != nil || legacy == nil || snapshot != nil {
		t.Fatalf("legacy dispatch failed: %v", err)
	}
}

func TestSnapshotDispatchRejectsMissingCompletionCounts(t *testing.T) {
	raw := []byte(`{"protocol":"agentray.connector.snapshot.v1","project_id":"p","connector_id":"c","table":"t","sync_id":"s","run_id":"r","generation":"g","generation_seq":1,"binding_digest":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","capture_started_at":"2026-10-01T00:00:00Z","capture_finished_at":"2026-10-01T00:00:01Z","kind":"complete","batch_manifest_sha256":"e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"}`)
	if _, _, err := decodeConnectorEnvelope(raw); err == nil {
		t.Fatal("completion without explicit expected counts was accepted")
	}
}

func TestLegacyConnectorBatchDecodeUnchanged(t *testing.T) {
	want := ExternalRowsBatch{ProjectID: "00000000-0000-4000-8000-000000000001", ConnectorID: "00000000-0000-4000-8000-000000000002", Table: "users", Rows: []ExternalRow{{Key: "k1", Cursor: "1", Data: json.RawMessage(`{"id":"k1","kind":"legacy","generation":"customer-value"}`)}}}
	raw, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	batch, snapshot, err := decodeConnectorEnvelope(raw)
	if err != nil || snapshot != nil || batch == nil {
		t.Fatalf("batch=%+v snapshot=%+v err=%v raw=%s", batch, snapshot, err, raw)
	}
	landed := batch.LandedRows()
	if len(landed) != 1 || landed[0].Key != "k1" {
		t.Fatalf("landed=%+v", landed)
	}
}

func TestRepairG7OversizedSnapshotRejectedBeforePreparation(t *testing.T) {
	url := startBrokerWith(t, func(o *natsserver.Options) { o.MaxPayload = 8 << 10 })
	nc, err := nats.Connect(url)
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatal(err)
	}
	cfg := testConfig("snapshot-payload")
	if _, err := EnsureStreams(context.Background(), nc, cfg); err != nil {
		t.Fatal(err)
	}
	q := NewJetStreamQueue(js, cfg.IngestSubject, cfg.IngestConnectorSubject)
	common := connector.SnapshotEnvelope{ProjectID: "p", ConnectorID: "c", Table: "t", SyncID: "s", RunID: "r",
		Generation: "g", GenerationSeq: 1, BindingDigest: strings.Repeat("a", 64), CaptureStartedAt: time.Now().UTC()}
	sensitiveKey := "fixture.patient.0042@example.test"
	_, err = q.BuildSnapshotBatches(common, []connector.LandedRow{{Key: sensitiveKey, DataJSON: `{"blob":"` + strings.Repeat("x", 32<<10) + `"}`}}, 7)
	if err == nil {
		t.Fatal("oversized snapshot envelope was accepted for persistence")
	}
	if strings.Contains(err.Error(), sensitiveKey) {
		t.Fatalf("oversized snapshot error exposed source row key: %v", err)
	}
	if !strings.Contains(err.Error(), "snapshot batch 7 is") || !strings.Contains(err.Error(), "publish budget") {
		t.Fatalf("oversized snapshot error lost actionable batch and budget details: %v", err)
	}
}

func TestRepairR5SnapshotRowsSplitForEnvelopeOverhead(t *testing.T) {
	url := startBrokerWith(t, func(o *natsserver.Options) { o.MaxPayload = 8 << 10 })
	nc, err := nats.Connect(url)
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatal(err)
	}
	cfg := testConfig("snapshot-envelope-overhead")
	if _, err := EnsureStreams(context.Background(), nc, cfg); err != nil {
		t.Fatal(err)
	}
	q := NewJetStreamQueue(js, cfg.IngestSubject, cfg.IngestConnectorSubject)
	common := connector.SnapshotEnvelope{ProjectID: "p", ConnectorID: "c", Table: "t", SyncID: "s", RunID: "r",
		Generation: "g", GenerationSeq: 1, BindingDigest: strings.Repeat("a", 64), CaptureStartedAt: time.Now().UTC()}
	rows := []connector.LandedRow{
		{Key: "1", DataJSON: `{"blob":"` + strings.Repeat("x", 3400) + `"}`},
		{Key: "2", DataJSON: `{"blob":"` + strings.Repeat("x", 3400) + `"}`},
	}
	for _, row := range rows {
		if _, err := q.BuildSnapshotBatches(common, []connector.LandedRow{row}, 0); err != nil {
			t.Fatalf("singleton must fit: %v", err)
		}
	}
	envelopes, err := q.BuildSnapshotBatches(common, rows, 0)
	if err != nil {
		t.Fatalf("individually valid rows were rejected instead of split: %v", err)
	}
	if len(envelopes) < 2 {
		t.Fatalf("rows requiring envelope-aware splitting produced %d envelope(s)", len(envelopes))
	}
	for _, envelope := range envelopes {
		if err := q.PublishSnapshotEnvelope(context.Background(), envelope); err != nil {
			t.Fatal(err)
		}
	}
}
