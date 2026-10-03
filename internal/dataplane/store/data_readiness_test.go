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
	if got := cleared[syncID]; got.State != ReadinessSyncing || got.Reason == nil || *got.Reason != "awaiting_query_confirmation" {
		t.Fatalf("repaired hole reused pre-repair confirmation = %+v", got)
	}
	if _, _, err := s.RunSQLWithMeta(ctx, projectID, `SELECT count(*) AS n FROM events`); err != nil {
		t.Fatal(err)
	}
	cleared, err = s.SourceReadiness(ctx, projectID, configured)
	if err != nil {
		t.Fatal(err)
	}
	if got := cleared[syncID]; got.State != ReadinessReady {
		t.Fatalf("after repair confirmation = %+v", got)
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

func TestPublicationTargetRejectsOlderServingRun(t *testing.T) {
	local := localSourceReceipt{RunID: "run-old", GenerationSeq: 3}
	if !publicationRequiresNewerLocal(publicationTarget{RunID: "run-new", GenerationSeq: 3}, local) {
		t.Fatal("newer published run was allowed to inherit an older local receipt")
	}
	generation := "generation-4"
	local.Generation = &generation
	if !publicationRequiresNewerLocal(publicationTarget{Generation: "generation-5", GenerationSeq: 5}, local) {
		t.Fatal("newer published generation was allowed to inherit an older local receipt")
	}
	if publicationRequiresNewerLocal(publicationTarget{Generation: generation, GenerationSeq: 3}, local) {
		t.Fatal("matching publication identity was treated as newer")
	}
	if publicationRequiresNewerLocal(publicationTarget{Generation: "generation-2", GenerationSeq: 2}, local) {
		t.Fatal("older published generation was allowed to supersede a newer local receipt")
	}
	newerCapture := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	olderCapture := newerCapture.Add(-time.Hour)
	local = localSourceReceipt{RunID: "run-new", CaptureStartedAt: &newerCapture}
	if publicationRequiresNewerLocal(publicationTarget{RunID: "run-old", CaptureStarted: &olderCapture}, local) {
		t.Fatal("older incremental publication was allowed to supersede a newer local receipt")
	}
}

func TestIncrementalReceiptDoesNotRegressToOlderRun(t *testing.T) {
	d := openTestDuckDB(t)
	ctx := context.Background()
	projectID, connectorID, syncID := uuid.NewString(), uuid.NewString(), uuid.NewString()
	newerRun, olderRun := uuid.NewString(), uuid.NewString()
	base := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	expected := uint64(0)
	newerCapture := base.Add(time.Hour)
	newer := SourceReceiptMark{ProjectID: projectID, ConnectorID: connectorID, Table: "orders", SyncID: syncID,
		RunID: newerRun, CaptureStartedAt: &newerCapture, CaptureFinishedAt: &newerCapture,
		ExpectedBatches: &expected, Complete: true, Promoted: true}
	if err := d.RecordPosition(ctx, AppliedMark{Source: &newer}); err != nil {
		t.Fatal(err)
	}
	older := newer
	older.RunID, older.CaptureStartedAt, older.CaptureFinishedAt = olderRun, &base, &base
	if err := d.RecordPosition(ctx, AppliedMark{Source: &older}); err != nil {
		t.Fatal(err)
	}
	var gotRun string
	if err := d.Read(ctx, func(conn *sql.Conn) error {
		return conn.QueryRowContext(ctx, `SELECT run_id::VARCHAR FROM data_receipt_sources
WHERE project_id=? AND connector_id=? AND table_name=?`, projectID, connectorID, newer.Table).Scan(&gotRun)
	}); err != nil {
		t.Fatal(err)
	}
	if gotRun != newerRun {
		t.Fatalf("late older receipt regressed run to %s, want %s", gotRun, newerRun)
	}
}

func TestDataReadinessUsesCaptureBoundaryNotLateLanding(t *testing.T) {
	d := openTestDuckDB(t)
	ctx := context.Background()
	projectID, connectorID, syncID := uuid.NewString(), uuid.NewString(), uuid.NewString()
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	captured := now.Add(-24 * time.Hour)
	expected := uint64(0)
	mark := SourceReceiptMark{ProjectID: projectID, ConnectorID: connectorID, Table: "exports.orders",
		SyncID: syncID, RunID: uuid.NewString(), ExpectedBatches: &expected, CaptureFinishedAt: &captured, Complete: true}
	if err := d.Write(ctx, func(tx *sql.Tx) error {
		return RecordSnapshotPromotionTx(ctx, tx, SnapshotPromotion{SourceReceiptMark: mark, PromotedAt: now})
	}); err != nil {
		t.Fatal(err)
	}
	pool := newSQLSandboxPool(d)
	t.Cleanup(pool.closeAll)
	s := &Store{duck: d, sandboxes: pool, now: func() time.Time { return now }}
	if _, _, err := s.RunSQLWithMeta(ctx, projectID, `SELECT count(*) FROM events`); err != nil {
		t.Fatal(err)
	}
	got, err := s.SourceReadiness(ctx, projectID, []ReadinessSource{{SyncID: syncID, ConnectorID: connectorID, Table: mark.Table, ScheduleCron: "* * * * *", Configured: true}})
	if err != nil {
		t.Fatal(err)
	}
	if ready := got[syncID]; ready.State != ReadinessStale || ready.Reason == nil || *ready.Reason != "freshness_deadline_missed" || ready.LandedAt == nil || !ready.LandedAt.Equal(now) {
		t.Fatalf("late landing hid old capture: %+v", ready)
	}
}

func TestDataReadinessBridgesRefusedMissingAndScopesDLQHoles(t *testing.T) {
	d := openTestDuckDB(t)
	ctx := context.Background()
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	projectA, projectB := uuid.NewString(), uuid.NewString()
	connectorA, connectorB := uuid.NewString(), uuid.NewString()
	syncA, syncB := uuid.NewString(), uuid.NewString()
	expected := uint64(0)
	for _, mark := range []SourceReceiptMark{
		{ProjectID: projectA, ConnectorID: connectorA, Table: "orders", SyncID: syncA, RunID: uuid.NewString(), ExpectedBatches: &expected, CaptureFinishedAt: &now, Complete: true},
		{ProjectID: projectB, ConnectorID: connectorB, Table: "orders", SyncID: syncB, RunID: uuid.NewString(), ExpectedBatches: &expected, CaptureFinishedAt: &now, Complete: true},
	} {
		mark := mark
		if err := d.Write(ctx, func(tx *sql.Tx) error {
			return RecordSnapshotPromotionTx(ctx, tx, SnapshotPromotion{SourceReceiptMark: mark, PromotedAt: now})
		}); err != nil {
			t.Fatal(err)
		}
	}
	pool := newSQLSandboxPool(d)
	t.Cleanup(pool.closeAll)
	s := &Store{duck: d, sandboxes: pool, now: func() time.Time { return now }}
	if _, _, err := s.RunSQLWithMeta(ctx, projectA, `SELECT count(*) FROM events`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.RunSQLWithMeta(ctx, projectB, `SELECT count(*) FROM events`); err != nil {
		t.Fatal(err)
	}
	hole := DeliveryReceiptMark{StreamID: "stream@one", Subject: "connectors", StreamSeq: 4,
		PayloadSHA256: strings.Repeat("d", 64), ProjectID: projectA, ConnectorID: connectorA, Table: "orders"}
	if err := d.RecordReadinessHole(ctx, hole, nil); err != nil {
		t.Fatal(err)
	}
	sources := []ReadinessSource{{SyncID: syncA, ConnectorID: connectorA, Table: "orders", ScheduleCron: "* * * * *", Configured: true}}
	a, err := s.SourceReadiness(ctx, projectA, sources)
	if err != nil || a[syncA].State != ReadinessIncomplete {
		t.Fatalf("attributed project hole = %+v err=%v", a[syncA], err)
	}
	b, err := s.SourceReadiness(ctx, projectB, []ReadinessSource{{SyncID: syncB, ConnectorID: connectorB, Table: "orders", ScheduleCron: "* * * * *", Configured: true}})
	if err != nil || b[syncB].State != ReadinessReady {
		t.Fatalf("unrelated project inherited hole = %+v err=%v", b[syncB], err)
	}
	if err := d.RefusePosition(ctx, "durable", 2); err != nil {
		t.Fatal(err)
	}
	b, err = s.SourceReadiness(ctx, projectB, []ReadinessSource{{SyncID: syncB, ConnectorID: connectorB, Table: "orders", ScheduleCron: "* * * * *", Configured: true}})
	if err != nil || b[syncB].State != ReadinessIncomplete {
		t.Fatalf("refused_missing was not bridged = %+v err=%v", b[syncB], err)
	}
}

func TestReceiptJournalCompactsCompletedGenerations(t *testing.T) {
	d := openTestDuckDB(t)
	ctx := context.Background()
	projectID, connectorID, syncID := uuid.NewString(), uuid.NewString(), uuid.NewString()
	now := time.Now().UTC()
	for seq := uint64(1); seq <= 2; seq++ {
		generation := uuid.NewString()
		index, expected := uint64(0), uint64(1)
		batch := SourceReceiptMark{ProjectID: projectID, ConnectorID: connectorID, Table: "orders", SyncID: syncID,
			Generation: generation, GenerationSeq: seq, BatchID: "batch-0", BatchIndex: &index,
			PayloadSHA256: strings.Repeat(string(rune('a'+int(seq))), 64), CaptureStartedAt: &now}
		if err := d.RecordPosition(ctx, AppliedMark{Source: &batch}); err != nil {
			t.Fatal(err)
		}
		batch.BatchID, batch.BatchIndex, batch.PayloadSHA256 = "", nil, ""
		batch.ExpectedBatches, batch.CaptureFinishedAt, batch.Complete, batch.Promoted = &expected, &now, true, true
		if err := d.RecordPosition(ctx, AppliedMark{Source: &batch}); err != nil {
			t.Fatal(err)
		}
	}
	var batches int
	if err := d.Read(ctx, func(conn *sql.Conn) error {
		return conn.QueryRowContext(ctx, `SELECT count(*) FROM data_receipt_batches WHERE project_id=? AND connector_id=?`, projectID, connectorID).Scan(&batches)
	}); err != nil {
		t.Fatal(err)
	}
	if batches != 1 {
		t.Fatalf("completed generation batch journals = %d, want only latest", batches)
	}
}

func TestPublicationJournalSelectsNewRunAndCompactsResolvedHistory(t *testing.T) {
	s := openConvTestStore(t)
	_, projectID := seedConvProject(t, s)
	ctx := context.Background()
	var connectorID, syncID string
	if err := s.pg.QueryRow(ctx, `INSERT INTO data_connectors(project_id,name,kind) VALUES($1,'readiness','postgres') RETURNING id::text`, projectID).Scan(&connectorID); err != nil {
		t.Fatal(err)
	}
	if err := s.pg.QueryRow(ctx, `INSERT INTO connector_syncs(connector_id,project_id,source_table,key_column) VALUES($1,$2,'orders','id') RETURNING id::text`, connectorID, projectID).Scan(&syncID); err != nil {
		t.Fatal(err)
	}
	oldRun, newRun := uuid.NewString(), uuid.NewString()
	base := time.Now().UTC().Add(-time.Minute)
	old := PublicationObservation{SourceReceiptMark: SourceReceiptMark{ProjectID: projectID, ConnectorID: connectorID, Table: "orders", SyncID: syncID, RunID: oldRun, CaptureStartedAt: &base, Complete: true},
		StableBatchID: oldRun + ":complete", PayloadSHA256: strings.Repeat("a", 64), PublishedAt: base}
	if err := s.RecordPublication(ctx, old); err != nil {
		t.Fatal(err)
	}
	newCapture := base.Add(time.Minute)
	newer := PublicationObservation{SourceReceiptMark: SourceReceiptMark{ProjectID: projectID, ConnectorID: connectorID, Table: "orders", SyncID: syncID, RunID: newRun, CaptureStartedAt: &newCapture},
		StableBatchID: newRun + ":batch-0", PayloadSHA256: strings.Repeat("b", 64), PublishedAt: base.Add(time.Minute)}
	if err := s.RecordPublication(ctx, newer); err != nil {
		t.Fatal(err)
	}
	targets, err := s.publicationTargets(ctx, projectID, []ReadinessSource{{SyncID: syncID}})
	if err != nil || targets[syncID].RunID != newRun {
		t.Fatalf("latest publication target = %+v err=%v", targets[syncID], err)
	}
	newer.Complete = true
	newer.StableBatchID = newRun + ":complete"
	newer.PayloadSHA256 = strings.Repeat("c", 64)
	if err := s.RecordPublication(ctx, newer); err != nil {
		t.Fatal(err)
	}
	var oldRows int
	if err := s.pg.QueryRow(ctx, `SELECT count(*) FROM source_publication_receipts WHERE project_id=$1 AND run_id=$2`, projectID, oldRun).Scan(&oldRows); err != nil {
		t.Fatal(err)
	}
	if oldRows != 0 {
		t.Fatalf("resolved publication rows retained = %d", oldRows)
	}
	old.PublishedAt = base.Add(2 * time.Minute)
	if err := s.RecordPublication(ctx, old); err != nil {
		t.Fatal(err)
	}
	targets, err = s.publicationTargets(ctx, projectID, []ReadinessSource{{SyncID: syncID}})
	if err != nil || targets[syncID].RunID != newRun {
		t.Fatalf("late old completion replaced newer publication target = %+v err=%v", targets[syncID], err)
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
