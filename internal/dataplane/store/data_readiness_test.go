package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/lohi-ai/agentray/internal/dataplane/connector"
)

func TestDataReadinessRequiresPromotionSandboxAndNoHoles(t *testing.T) {
	d := openTestDuckDB(t)
	ctx := context.Background()
	projectID, connectorID, syncID := uuid.NewString(), uuid.NewString(), uuid.NewString()
	generation, runID := uuid.NewString(), uuid.NewString()
	captured := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	finished := captured.Add(time.Minute)
	batchIndex, expected := uint64(0), uint64(1)
	delivery := DeliveryReceiptMark{StreamID: "stream-a", Subject: "connectors", StreamSeq: 9, PayloadSHA256: strings.Repeat("a", 64), PublishedAt: &finished}
	source := &SourceReceiptMark{ProjectID: projectID, ConnectorID: connectorID, Table: "exports.orders",
		SyncID: syncID, RunID: runID, Generation: generation, GenerationSeq: 1, BatchID: "batch-0",
		BatchIndex: &batchIndex, PayloadSHA256: strings.Repeat("b", 64), CaptureStartedAt: &captured, PublishedAt: &finished}
	if err := d.InsertExternalRows(ctx, projectID, connectorID, source.Table,
		[]connector.LandedRow{{Key: "1", DataJSON: `{"amount":10}`}},
		AppliedMark{Durable: "blue", Seq: 1, Deliveries: []DeliveryReceiptMark{delivery}, Source: source}); err != nil {
		t.Fatal(err)
	}
	complete := *source
	complete.BatchID, complete.BatchIndex, complete.PayloadSHA256 = "", nil, ""
	complete.ExpectedBatches, complete.ExpectedRows, complete.CaptureFinishedAt, complete.Complete = &expected, &expected, &finished, true
	if err := d.RecordPosition(ctx, AppliedMark{Durable: "blue", Seq: 2, Source: &complete}); err != nil {
		t.Fatal(err)
	}
	if err := d.Write(ctx, func(tx *sql.Tx) error {
		return RecordSnapshotPromotionTx(ctx, tx, SnapshotPromotion{SourceReceiptMark: complete, PromotedAt: finished.Add(time.Second)})
	}); err != nil {
		t.Fatal(err)
	}

	pool := newSQLSandboxPool(d)
	t.Cleanup(pool.closeAll)
	s := &Store{duck: d, sandboxes: pool, now: func() time.Time { return finished.Add(2 * time.Minute) }}
	configured := []ReadinessSource{{SyncID: syncID, ConnectorID: connectorID, Table: source.Table, ScheduleCron: "* * * * *", Configured: true}}
	before, err := s.SourceReadiness(ctx, projectID, configured)
	if err != nil {
		t.Fatal(err)
	}
	if got := before[syncID]; got.State != ReadinessSyncing || got.Reason == nil || *got.Reason != "awaiting_query_confirmation" {
		t.Fatalf("before query = %+v", got)
	}
	if _, _, err := s.RunSQLWithMeta(ctx, projectID, `SELECT count(*) AS n FROM events`); err != nil {
		t.Fatal(err)
	}
	after, err := s.SourceReadiness(ctx, projectID, configured)
	if err != nil {
		t.Fatal(err)
	}
	if got := after[syncID]; got.State != ReadinessReady || got.QueryableAt == nil || got.Reason != nil {
		t.Fatalf("after query = %+v", got)
	}
	if err := d.RecordReadinessHole(ctx, delivery, source); err != nil {
		t.Fatal(err)
	}
	withHole, err := s.SourceReadiness(ctx, projectID, configured)
	if err != nil {
		t.Fatal(err)
	}
	if got := withHole[syncID]; got.State != ReadinessIncomplete {
		t.Fatalf("with hole = %+v", got)
	}
	delivery.Replayed = true
	if err := d.RecordPosition(ctx, AppliedMark{Durable: "blue", Seq: 3, Deliveries: []DeliveryReceiptMark{delivery}}); err != nil {
		t.Fatal(err)
	}
	cleared, err := s.SourceReadiness(ctx, projectID, configured)
	if err != nil {
		t.Fatal(err)
	}
	if got := cleared[syncID]; got.State != ReadinessReady {
		t.Fatalf("after exact replay = %+v", got)
	}

	legacy := DeliveryReceiptMark{StreamID: "stream-a", Subject: "events", StreamSeq: 99,
		PayloadSHA256: strings.Repeat("c", 64), Replayed: true, Unverifiable: true}
	if err := d.RecordPosition(ctx, AppliedMark{Durable: "blue", Seq: 4, Deliveries: []DeliveryReceiptMark{legacy}}); err != nil {
		t.Fatal(err)
	}
	legacy.Unverifiable = false
	if err := d.RecordPosition(ctx, AppliedMark{Durable: "blue", Seq: 5, Deliveries: []DeliveryReceiptMark{legacy}}); err != nil {
		t.Fatal(err)
	}
	unverifiable, err := s.SourceReadiness(ctx, projectID, configured)
	if err != nil {
		t.Fatal(err)
	}
	if got := unverifiable[syncID]; got.State != ReadinessIncomplete {
		t.Fatalf("unverifiable legacy replay must remain a hole = %+v", got)
	}
}

func TestDataReadinessUnknownFieldsMarshalNull(t *testing.T) {
	body, err := json.Marshal(Readiness{State: ReadinessSyncing})
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{`"published_at":null`, `"landed_at":null`, `"queryable_at":null`, `"generation":null`, `"reason":null`} {
		if !strings.Contains(string(body), field) {
			t.Fatalf("%s missing %s", body, field)
		}
	}
}

func TestDataReadinessDoesNotInheritOrRegressPromotedGeneration(t *testing.T) {
	d := openTestDuckDB(t)
	ctx := context.Background()
	projectID, connectorID, syncID := uuid.NewString(), uuid.NewString(), uuid.NewString()
	firstGeneration, secondGeneration := uuid.NewString(), uuid.NewString()
	now := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	expected := uint64(0)
	first := SourceReceiptMark{ProjectID: projectID, ConnectorID: connectorID, Table: "exports.orders",
		SyncID: syncID, Generation: firstGeneration, GenerationSeq: 1, ExpectedBatches: &expected,
		CaptureFinishedAt: &now, Complete: true}
	if err := d.Write(ctx, func(tx *sql.Tx) error {
		return RecordSnapshotPromotionTx(ctx, tx, SnapshotPromotion{SourceReceiptMark: first, PromotedAt: now})
	}); err != nil {
		t.Fatal(err)
	}

	second := first
	second.Generation, second.GenerationSeq = secondGeneration, 2
	second.CaptureFinishedAt = nil
	if err := d.RecordPosition(ctx, AppliedMark{Source: &second}); err != nil {
		t.Fatal(err)
	}
	pool := newSQLSandboxPool(d)
	t.Cleanup(pool.closeAll)
	s := &Store{duck: d, sandboxes: pool, now: func() time.Time { return now.Add(time.Minute) }}
	if _, _, err := s.RunSQLWithMeta(ctx, projectID, `SELECT count(*) AS n FROM events`); err != nil {
		t.Fatal(err)
	}
	configured := []ReadinessSource{{SyncID: syncID, ConnectorID: connectorID, Table: first.Table, ScheduleCron: "* * * * *", Configured: true}}
	readiness, err := s.SourceReadiness(ctx, projectID, configured)
	if err != nil {
		t.Fatal(err)
	}
	if got := readiness[syncID]; got.State != ReadinessSyncing || got.LandedAt != nil || got.Generation == nil || *got.Generation != secondGeneration {
		t.Fatalf("new unpromoted generation inherited old landing evidence: %+v", got)
	}

	late := first
	if err := d.Write(ctx, func(tx *sql.Tx) error {
		return RecordSnapshotPromotionTx(ctx, tx, SnapshotPromotion{SourceReceiptMark: late, PromotedAt: now.Add(2 * time.Minute)})
	}); err != nil {
		t.Fatal(err)
	}
	readiness, err = s.SourceReadiness(ctx, projectID, configured)
	if err != nil {
		t.Fatal(err)
	}
	if got := readiness[syncID]; got.Generation == nil || *got.Generation != secondGeneration || got.LandedAt != nil {
		t.Fatalf("late obsolete promotion regressed generation fence: %+v", got)
	}
}
