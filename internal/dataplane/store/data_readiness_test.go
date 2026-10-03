package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
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

func TestSettlementOnlyReceiptDoesNotClearHole(t *testing.T) {
	d := openTestDuckDB(t)
	ctx := context.Background()
	delivery := DeliveryReceiptMark{StreamID: "stream", Subject: "events", StreamSeq: 5, PayloadSHA256: strings.Repeat("a", 64)}
	if err := d.RecordReadinessHole(ctx, delivery, nil); err != nil {
		t.Fatal(err)
	}
	if err := d.RecordPosition(ctx, AppliedMark{Durable: "blue", Seq: 1, Deliveries: []DeliveryReceiptMark{delivery}}); err != nil {
		t.Fatal(err)
	}
	var holes int
	if err := d.Read(ctx, func(conn *sql.Conn) error {
		return conn.QueryRowContext(ctx, `SELECT count(*) FROM data_receipt_holes WHERE cleared_at IS NULL`).Scan(&holes)
	}); err != nil {
		t.Fatal(err)
	}
	if holes != 1 {
		t.Fatalf("settlement-only receipt cleared holes=%d", holes)
	}
}

func TestLaterIncrementalCompletionPreservesEarlierCoverageHole(t *testing.T) {
	d := openTestDuckDB(t)
	ctx := context.Background()
	projectID, connectorID, syncID := uuid.NewString(), uuid.NewString(), uuid.NewString()
	at := time.Now().UTC().Add(-time.Minute)
	one, zero := uint64(1), uint64(0)
	first := SourceReceiptMark{ProjectID: projectID, ConnectorID: connectorID, Table: "orders", SyncID: syncID,
		RunID: uuid.NewString(), CaptureStartedAt: &at, CaptureFinishedAt: &at, ExpectedBatches: &one, Complete: true, Promoted: true}
	if err := d.RecordPosition(ctx, AppliedMark{Source: &first}); err != nil {
		t.Fatal(err)
	}
	later := at.Add(time.Second)
	second := first
	second.RunID, second.CaptureStartedAt, second.CaptureFinishedAt, second.ExpectedBatches = uuid.NewString(), &later, &later, &zero
	if err := d.RecordPosition(ctx, AppliedMark{Source: &second}); err != nil {
		t.Fatal(err)
	}
	pool := newSQLSandboxPool(d)
	t.Cleanup(pool.closeAll)
	s := &Store{duck: d, sandboxes: pool, now: time.Now, manualFreshness: time.Hour}
	if _, _, err := s.RunSQLWithMeta(ctx, projectID, `SELECT count(*) FROM events`); err != nil {
		t.Fatal(err)
	}
	got, err := s.SourceReadiness(ctx, projectID, []ReadinessSource{{SyncID: syncID, ConnectorID: connectorID, Table: "orders", Configured: true}})
	if err != nil {
		t.Fatal(err)
	}
	if ready := got[syncID]; ready.State != ReadinessIncomplete {
		t.Fatalf("later empty delta forgot earlier coverage hole: %+v", ready)
	}
}

func TestReadinessDoesNotCrossSyncIdentity(t *testing.T) {
	d := openTestDuckDB(t)
	ctx := context.Background()
	projectID, connectorID := uuid.NewString(), uuid.NewString()
	oldSync, newSync, runID := uuid.NewString(), uuid.NewString(), uuid.NewString()
	now := time.Now().UTC()
	zero := uint64(0)
	mark := SourceReceiptMark{ProjectID: projectID, ConnectorID: connectorID, Table: "orders", SyncID: oldSync,
		RunID: runID, CaptureStartedAt: &now, CaptureFinishedAt: &now, ExpectedBatches: &zero, Complete: true, Promoted: true}
	if err := d.RecordPosition(ctx, AppliedMark{Source: &mark}); err != nil {
		t.Fatal(err)
	}
	pool := newSQLSandboxPool(d)
	t.Cleanup(pool.closeAll)
	s := &Store{duck: d, sandboxes: pool, now: func() time.Time { return now }}
	if _, _, err := s.RunSQLWithMeta(ctx, projectID, `SELECT count(*) FROM events`); err != nil {
		t.Fatal(err)
	}
	got, err := s.SourceReadiness(ctx, projectID, []ReadinessSource{{SyncID: newSync, ConnectorID: connectorID, Table: "orders", Configured: true}})
	if err != nil {
		t.Fatal(err)
	}
	if ready := got[newSync]; ready.State == ReadinessReady {
		t.Fatalf("new sync inherited old sync evidence: %+v", ready)
	}
}

func TestSnapshotToIncrementalTransitionAppliesNewerRow(t *testing.T) {
	d := openTestDuckDB(t)
	ctx := context.Background()
	projectID, connectorID := uuid.NewString(), uuid.NewString()
	now := time.Now().UTC()
	index := uint64(0)
	snapshot := SourceReceiptMark{ProjectID: projectID, ConnectorID: connectorID, Table: "orders", SyncID: uuid.NewString(),
		RunID: uuid.NewString(), Generation: uuid.NewString(), GenerationSeq: 1, BatchID: "snapshot", BatchIndex: &index,
		PayloadSHA256: strings.Repeat("a", 64), CaptureStartedAt: &now, Promoted: true}
	if err := d.InsertExternalRows(ctx, projectID, connectorID, "orders", []connector.LandedRow{{Key: "same", DataJSON: `{"n":1}`}}, AppliedMark{Source: &snapshot}); err != nil {
		t.Fatal(err)
	}
	later := now.Add(time.Minute)
	incremental := snapshot
	incremental.Generation, incremental.GenerationSeq, incremental.RunID, incremental.BatchID = "", 0, uuid.NewString(), "incremental"
	incremental.CaptureStartedAt, incremental.PayloadSHA256 = &later, strings.Repeat("b", 64)
	if err := d.InsertExternalRows(ctx, projectID, connectorID, "orders", []connector.LandedRow{{Key: "same", DataJSON: `{"n":2}`}}, AppliedMark{Source: &incremental}); err != nil {
		t.Fatal(err)
	}
	var value int
	if err := d.Read(ctx, func(conn *sql.Conn) error {
		return conn.QueryRowContext(ctx, `SELECT CAST(json_extract(data,'$.n') AS INTEGER) FROM external_rows WHERE project_id=?`, projectID).Scan(&value)
	}); err != nil {
		t.Fatal(err)
	}
	if value != 2 {
		t.Fatalf("snapshot-to-incremental update stored value=%d", value)
	}
}

func TestIncompleteCompletionPreservesLastCompleteAt(t *testing.T) {
	d := openTestDuckDB(t)
	ctx := context.Background()
	projectID, connectorID, syncID := uuid.NewString(), uuid.NewString(), uuid.NewString()
	oldTime := time.Now().UTC().Add(-time.Hour)
	zero, two, index := uint64(0), uint64(2), uint64(0)
	old := SourceReceiptMark{ProjectID: projectID, ConnectorID: connectorID, Table: "orders", SyncID: syncID,
		RunID: uuid.NewString(), CaptureStartedAt: &oldTime, CaptureFinishedAt: &oldTime, ExpectedBatches: &zero, Complete: true, Promoted: true}
	if err := d.Write(ctx, func(tx *sql.Tx) error {
		return recordAppliedReceiptsTx(ctx, tx, AppliedMark{Source: &old}, oldTime)
	}); err != nil {
		t.Fatal(err)
	}
	started := time.Now().UTC().Add(-time.Minute)
	batch := SourceReceiptMark{ProjectID: projectID, ConnectorID: connectorID, Table: "orders", SyncID: syncID,
		RunID: uuid.NewString(), CaptureStartedAt: &started, BatchID: "one", BatchIndex: &index, PayloadSHA256: strings.Repeat("a", 64), Promoted: true}
	if err := d.InsertExternalRows(ctx, projectID, connectorID, "orders", []connector.LandedRow{{Key: "1", DataJSON: `{}`}}, AppliedMark{Source: &batch}); err != nil {
		t.Fatal(err)
	}
	finished := time.Now().UTC()
	complete := batch
	complete.BatchID, complete.BatchIndex, complete.PayloadSHA256 = "", nil, ""
	complete.ExpectedBatches, complete.Complete, complete.CaptureFinishedAt = &two, true, &finished
	if err := d.RecordPosition(ctx, AppliedMark{Source: &complete}); err != nil {
		t.Fatal(err)
	}
	local, err := d.localReadiness(ctx, projectID, []ReadinessSource{{SyncID: syncID, ConnectorID: connectorID, Table: "orders", Configured: true}})
	if err != nil {
		t.Fatal(err)
	}
	if got := local[syncID].LastCompleteAt; got == nil || !got.Equal(oldTime) {
		t.Fatalf("incomplete run advanced last_complete_at: old=%s got=%v", oldTime, got)
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

func TestOrdinaryDeliveryCoverageAndJournalBounds(t *testing.T) {
	d := openTestDuckDB(t)
	ctx := context.Background()
	ordinary := DeliveryReceiptMark{StreamID: "events@ordinary", Subject: "events", StreamSeq: 42, PayloadSHA256: strings.Repeat("a", 64)}
	if err := d.RecordPosition(ctx, AppliedMark{Durable: "blue", Seq: 7, Deliveries: []DeliveryReceiptMark{ordinary}}); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := d.Read(ctx, func(conn *sql.Conn) error {
		return conn.QueryRowContext(ctx, `SELECT count(*) FROM data_receipt_deliveries WHERE stream_id=?`, ordinary.StreamID).Scan(&count)
	}); err != nil || count != 1 {
		t.Fatalf("ordinary delivery receipts = %d err=%v, want 1", count, err)
	}

	for seq := uint64(1); seq <= 10; seq++ {
		replay := DeliveryReceiptMark{StreamID: "events@replay", Subject: "events", StreamSeq: seq,
			PayloadSHA256: fmt.Sprintf("%064x", seq), Replayed: true}
		if err := d.RecordReadinessHole(ctx, replay, nil); err != nil {
			t.Fatal(err)
		}
		if err := d.RecordPosition(ctx, AppliedMark{Deliveries: []DeliveryReceiptMark{replay}}); err != nil {
			t.Fatal(err)
		}
	}
	var holes, replayed int
	if err := d.Read(ctx, func(conn *sql.Conn) error {
		return conn.QueryRowContext(ctx, `SELECT
		(SELECT count(*) FROM data_receipt_holes WHERE stream_id='events@replay'),
		(SELECT count(*) FROM data_receipt_deliveries WHERE stream_id='events@replay')`).Scan(&holes, &replayed)
	}); err != nil || holes != 0 || replayed != 0 {
		t.Fatalf("resolved replay journals = holes %d deliveries %d err=%v, want 0/0", holes, replayed, err)
	}

	if err := d.Write(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO data_receipt_deliveries
(stream_id,subject,stream_seq,payload_sha256,applied_at)
SELECT 'events@bounded','events',seq,lpad(CAST(seq AS VARCHAR),64,'0'),now()
FROM range(1, ?) AS generated(seq)`, deliveryReceiptSuffixLimit+2); err != nil {
			return err
		}
		return recordAppliedReceiptsTx(ctx, tx, AppliedMark{}, time.Now().UTC())
	}); err != nil {
		t.Fatal(err)
	}
	var minimum uint64
	if err := d.Read(ctx, func(conn *sql.Conn) error {
		return conn.QueryRowContext(ctx, `SELECT count(*),min(stream_seq) FROM data_receipt_deliveries WHERE stream_id='events@bounded'`).Scan(&count, &minimum)
	}); err != nil || count != deliveryReceiptSuffixLimit || minimum != 2 {
		t.Fatalf("bounded receipt suffix = count %d min %d err=%v", count, minimum, err)
	}
}

func TestIncrementalReadinessRequiresAppliedCompletionAndFreshQuery(t *testing.T) {
	s := openConvTestStore(t)
	_, projectID := seedConvProject(t, s)
	ctx := context.Background()
	var connectorID, syncID string
	if err := s.pg.QueryRow(ctx, `INSERT INTO data_connectors(project_id,name,kind) VALUES($1,'incremental-ready','postgres') RETURNING id::text`, projectID).Scan(&connectorID); err != nil {
		t.Fatal(err)
	}
	if err := s.pg.QueryRow(ctx, `INSERT INTO connector_syncs(connector_id,project_id,source_table,key_column) VALUES($1,$2,'orders','id') RETURNING id::text`, connectorID, projectID).Scan(&syncID); err != nil {
		t.Fatal(err)
	}
	s.duck = openTestDuckDB(t)
	s.sandboxes = newSQLSandboxPool(s.duck)
	t.Cleanup(s.sandboxes.closeAll)
	now := time.Now().UTC()
	s.now = func() time.Time { return now }
	runID := uuid.NewString()
	index0 := uint64(0)
	batch := SourceReceiptMark{ProjectID: projectID, ConnectorID: connectorID, Table: "orders", SyncID: syncID,
		RunID: runID, BatchID: "batch-0", BatchIndex: &index0, PayloadSHA256: strings.Repeat("a", 64), CaptureStartedAt: &now, Promoted: true}
	if err := s.duck.InsertExternalRows(ctx, projectID, connectorID, "orders", []connector.LandedRow{{Key: "1", DataJSON: `{"n":1}`}}, AppliedMark{Source: &batch}); err != nil {
		t.Fatal(err)
	}
	expected := uint64(2)
	complete := batch
	complete.BatchID, complete.BatchIndex, complete.PayloadSHA256 = "", nil, ""
	complete.ExpectedBatches, complete.CaptureFinishedAt, complete.Complete = &expected, &now, true
	if err := s.RecordPublication(ctx, PublicationObservation{SourceReceiptMark: complete, StableBatchID: runID + ":complete",
		PayloadSHA256: strings.Repeat("b", 64), PublishedAt: now}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.RunSQLWithMeta(ctx, projectID, `SELECT count(*) FROM external_rows`); err != nil {
		t.Fatal(err)
	}
	sources := []ReadinessSource{{SyncID: syncID, ConnectorID: connectorID, Table: "orders", ScheduleCron: "* * * * *", Configured: true}}
	readiness, err := s.SourceReadiness(ctx, projectID, sources)
	if err != nil {
		t.Fatal(err)
	}
	if got := readiness[syncID]; got.State != ReadinessSyncing || got.Reason == nil || *got.Reason != "awaiting_completion" || got.LastCompleteAt != nil {
		t.Fatalf("partial incremental run = %+v", got)
	}
	if err := s.duck.RecordPosition(ctx, AppliedMark{Source: &complete}); err != nil {
		t.Fatal(err)
	}
	readiness, err = s.SourceReadiness(ctx, projectID, sources)
	if err != nil || readiness[syncID].State != ReadinessIncomplete {
		t.Fatalf("completion with missing batch = %+v err=%v", readiness[syncID], err)
	}
	index1 := uint64(1)
	second := batch
	second.BatchID, second.BatchIndex, second.PayloadSHA256 = "batch-1", &index1, strings.Repeat("c", 64)
	if err := s.duck.InsertExternalRows(ctx, projectID, connectorID, "orders", []connector.LandedRow{{Key: "2", DataJSON: `{"n":2}`}}, AppliedMark{Source: &second}); err != nil {
		t.Fatal(err)
	}
	readiness, err = s.SourceReadiness(ctx, projectID, sources)
	if err != nil || readiness[syncID].State != ReadinessSyncing || readiness[syncID].Reason == nil || *readiness[syncID].Reason != "awaiting_query_confirmation" {
		t.Fatalf("completed mutation reused partial query proof = %+v err=%v", readiness[syncID], err)
	}
	if _, _, err := s.RunSQLWithMeta(ctx, projectID, `SELECT count(*) FROM external_rows`); err != nil {
		t.Fatal(err)
	}
	readiness, err = s.SourceReadiness(ctx, projectID, sources)
	if err != nil || readiness[syncID].State != ReadinessReady {
		t.Fatalf("freshly confirmed completion = %+v err=%v", readiness[syncID], err)
	}
}

func TestOlderIncrementalReplayIsFencedBeforeRowMutation(t *testing.T) {
	d := openTestDuckDB(t)
	ctx := context.Background()
	projectID, connectorID, syncID := uuid.NewString(), uuid.NewString(), uuid.NewString()
	now := time.Now().UTC()
	index, expected := uint64(0), uint64(1)
	current := SourceReceiptMark{ProjectID: projectID, ConnectorID: connectorID, Table: "orders", SyncID: syncID,
		RunID: uuid.NewString(), BatchID: "batch-0", BatchIndex: &index, PayloadSHA256: strings.Repeat("a", 64), CaptureStartedAt: &now, Promoted: true}
	if err := d.InsertExternalRows(ctx, projectID, connectorID, "orders", []connector.LandedRow{{Key: "1", DataJSON: `{"n":2}`}}, AppliedMark{Source: &current}); err != nil {
		t.Fatal(err)
	}
	complete := current
	complete.BatchID, complete.BatchIndex, complete.PayloadSHA256 = "", nil, ""
	complete.ExpectedBatches, complete.CaptureFinishedAt, complete.Complete = &expected, &now, true
	if err := d.RecordPosition(ctx, AppliedMark{Source: &complete}); err != nil {
		t.Fatal(err)
	}
	var mutationBefore uint64
	if err := d.Read(ctx, func(conn *sql.Conn) error {
		return conn.QueryRowContext(ctx, `SELECT mutation_seq FROM data_receipt_sources WHERE project_id=? AND connector_id=? AND table_name='orders'`, projectID, connectorID).Scan(&mutationBefore)
	}); err != nil {
		t.Fatal(err)
	}
	olderAt := now.Add(-time.Hour)
	older := current
	older.RunID, older.CaptureStartedAt, older.PayloadSHA256 = uuid.NewString(), &olderAt, strings.Repeat("b", 64)
	if err := d.InsertExternalRows(ctx, projectID, connectorID, "orders", []connector.LandedRow{{Key: "1", DataJSON: `{"n":1}`}}, AppliedMark{Source: &older}); err != nil {
		t.Fatal(err)
	}
	var n int
	var mutationAfter uint64
	if err := d.Read(ctx, func(conn *sql.Conn) error {
		if err := conn.QueryRowContext(ctx, `SELECT CAST(json_extract(data,'$.n') AS INTEGER) FROM external_rows WHERE project_id=?`, projectID).Scan(&n); err != nil {
			return err
		}
		return conn.QueryRowContext(ctx, `SELECT mutation_seq FROM data_receipt_sources WHERE project_id=? AND connector_id=? AND table_name='orders'`, projectID, connectorID).Scan(&mutationAfter)
	}); err != nil {
		t.Fatal(err)
	}
	if n != 2 || mutationAfter != mutationBefore {
		t.Fatalf("older replay changed rows/proof token: n=%d mutation=%d->%d", n, mutationBefore, mutationAfter)
	}
}

func TestOlderIncrementalReplayMergesOnlyMissingKeys(t *testing.T) {
	d := openTestDuckDB(t)
	ctx := context.Background()
	projectID, connectorID, syncID := uuid.NewString(), uuid.NewString(), uuid.NewString()
	now := time.Now().UTC()
	index, expected := uint64(0), uint64(1)
	current := SourceReceiptMark{ProjectID: projectID, ConnectorID: connectorID, Table: "orders", SyncID: syncID,
		RunID: uuid.NewString(), BatchID: "batch-0", BatchIndex: &index, PayloadSHA256: strings.Repeat("a", 64), CaptureStartedAt: &now, Promoted: true}
	if err := d.InsertExternalRows(ctx, projectID, connectorID, "orders", []connector.LandedRow{
		{Key: "shared", DataJSON: `{"n":2}`},
		{Key: "new-only", DataJSON: `{"n":3}`},
	}, AppliedMark{Source: &current}); err != nil {
		t.Fatal(err)
	}
	complete := current
	complete.BatchID, complete.BatchIndex, complete.PayloadSHA256 = "", nil, ""
	complete.ExpectedBatches, complete.CaptureFinishedAt, complete.Complete = &expected, &now, true
	if err := d.RecordPosition(ctx, AppliedMark{Source: &complete}); err != nil {
		t.Fatal(err)
	}
	var mutationBefore uint64
	if err := d.Read(ctx, func(conn *sql.Conn) error {
		return conn.QueryRowContext(ctx, `SELECT mutation_seq FROM data_receipt_sources WHERE project_id=? AND connector_id=? AND table_name='orders'`, projectID, connectorID).Scan(&mutationBefore)
	}); err != nil {
		t.Fatal(err)
	}
	olderAt := now.Add(-time.Hour)
	older := current
	older.RunID, older.CaptureStartedAt, older.PayloadSHA256 = uuid.NewString(), &olderAt, strings.Repeat("b", 64)
	rows := []connector.LandedRow{
		{Key: "shared", DataJSON: `{"n":1}`},
		{Key: "old-only", DataJSON: `{"n":1}`},
	}
	if err := d.InsertExternalRows(ctx, projectID, connectorID, "orders", rows, AppliedMark{Source: &older}); err != nil {
		t.Fatal(err)
	}
	var shared, oldOnly int
	var runID string
	var mutationAfter uint64
	if err := d.Read(ctx, func(conn *sql.Conn) error {
		if err := conn.QueryRowContext(ctx, `SELECT CAST(json_extract(data,'$.n') AS INTEGER) FROM external_rows WHERE project_id=? AND row_key='shared'`, projectID).Scan(&shared); err != nil {
			return err
		}
		if err := conn.QueryRowContext(ctx, `SELECT CAST(json_extract(data,'$.n') AS INTEGER) FROM external_rows WHERE project_id=? AND row_key='old-only'`, projectID).Scan(&oldOnly); err != nil {
			return err
		}
		return conn.QueryRowContext(ctx, `SELECT run_id::VARCHAR,mutation_seq FROM data_receipt_sources WHERE project_id=? AND connector_id=? AND table_name='orders'`, projectID, connectorID).Scan(&runID, &mutationAfter)
	}); err != nil {
		t.Fatal(err)
	}
	if shared != 2 || oldOnly != 1 || runID != current.RunID || mutationAfter != mutationBefore+1 {
		t.Fatalf("older merge shared=%d old-only=%d run=%s mutation=%d->%d", shared, oldOnly, runID, mutationBefore, mutationAfter)
	}
	// Once the missing key has landed, an exact replay is a no-op and must not
	// invalidate a query proof again.
	if err := d.InsertExternalRows(ctx, projectID, connectorID, "orders", rows, AppliedMark{Source: &older}); err != nil {
		t.Fatal(err)
	}
	var mutationReplay uint64
	if err := d.Read(ctx, func(conn *sql.Conn) error {
		return conn.QueryRowContext(ctx, `SELECT mutation_seq FROM data_receipt_sources WHERE project_id=? AND connector_id=? AND table_name='orders'`, projectID, connectorID).Scan(&mutationReplay)
	}); err != nil {
		t.Fatal(err)
	}
	if mutationReplay != mutationAfter {
		t.Fatalf("idempotent older replay changed proof token: %d -> %d", mutationAfter, mutationReplay)
	}
}

func TestOlderIncrementalReplayRemainsIncompleteAfterFreshQuery(t *testing.T) {
	s := openConvTestStore(t)
	_, projectID := seedConvProject(t, s)
	ctx := context.Background()
	var connectorID, syncID string
	if err := s.pg.QueryRow(ctx, `INSERT INTO data_connectors(project_id,name,kind) VALUES($1,'older-delta','postgres') RETURNING id::text`, projectID).Scan(&connectorID); err != nil {
		t.Fatal(err)
	}
	if err := s.pg.QueryRow(ctx, `INSERT INTO connector_syncs(connector_id,project_id,source_table,key_column) VALUES($1,$2,'orders','id') RETURNING id::text`, connectorID, projectID).Scan(&syncID); err != nil {
		t.Fatal(err)
	}
	s.duck = openTestDuckDB(t)
	s.sandboxes = newSQLSandboxPool(s.duck)
	t.Cleanup(s.sandboxes.closeAll)
	now := time.Now().UTC()
	s.now = func() time.Time { return now }
	index, expected := uint64(0), uint64(1)
	current := SourceReceiptMark{ProjectID: projectID, ConnectorID: connectorID, Table: "orders", SyncID: syncID,
		RunID: uuid.NewString(), BatchID: "batch-0", BatchIndex: &index, PayloadSHA256: strings.Repeat("a", 64), CaptureStartedAt: &now, Promoted: true}
	if err := s.duck.InsertExternalRows(ctx, projectID, connectorID, "orders", []connector.LandedRow{{Key: "shared", DataJSON: `{"n":2}`}}, AppliedMark{Source: &current}); err != nil {
		t.Fatal(err)
	}
	complete := current
	complete.BatchID, complete.BatchIndex, complete.PayloadSHA256 = "", nil, ""
	complete.ExpectedBatches, complete.CaptureFinishedAt, complete.Complete = &expected, &now, true
	if err := s.duck.RecordPosition(ctx, AppliedMark{Source: &complete}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.RunSQLWithMeta(ctx, projectID, `SELECT count(*) FROM external_rows`); err != nil {
		t.Fatal(err)
	}
	sources := []ReadinessSource{{SyncID: syncID, ConnectorID: connectorID, Table: "orders", ScheduleCron: "* * * * *", Configured: true}}
	if got, err := s.SourceReadiness(ctx, projectID, sources); err != nil || got[syncID].State != ReadinessReady {
		t.Fatalf("current run readiness=%+v err=%v", got[syncID], err)
	}
	olderAt := now.Add(-time.Hour)
	older := current
	older.RunID, older.CaptureStartedAt, older.PayloadSHA256 = uuid.NewString(), &olderAt, strings.Repeat("b", 64)
	if err := s.duck.InsertExternalRows(ctx, projectID, connectorID, "orders", []connector.LandedRow{
		{Key: "shared", DataJSON: `{"n":1}`},
		{Key: "old-only", DataJSON: `{"n":1}`},
	}, AppliedMark{Source: &older}); err != nil {
		t.Fatal(err)
	}
	got, err := s.SourceReadiness(ctx, projectID, sources)
	if err != nil || got[syncID].State != ReadinessIncomplete || got[syncID].Reason == nil || *got[syncID].Reason != "incremental_ordering_ambiguous" {
		t.Fatalf("merged older delta readiness=%+v err=%v", got[syncID], err)
	}
	var shared, oldOnly int
	if err := s.duck.Read(ctx, func(conn *sql.Conn) error {
		if err := conn.QueryRowContext(ctx, `SELECT CAST(json_extract(data,'$.n') AS INTEGER) FROM external_rows WHERE project_id=? AND row_key='shared'`, projectID).Scan(&shared); err != nil {
			return err
		}
		return conn.QueryRowContext(ctx, `SELECT CAST(json_extract(data,'$.n') AS INTEGER) FROM external_rows WHERE project_id=? AND row_key='old-only'`, projectID).Scan(&oldOnly)
	}); err != nil {
		t.Fatal(err)
	}
	if shared != 2 || oldOnly != 1 {
		t.Fatalf("merged rows shared=%d old-only=%d", shared, oldOnly)
	}
	if _, _, err := s.RunSQLWithMeta(ctx, projectID, `SELECT count(*) FROM external_rows`); err != nil {
		t.Fatal(err)
	}
	if got, err = s.SourceReadiness(ctx, projectID, sources); err != nil || got[syncID].State != ReadinessIncomplete || got[syncID].Reason == nil || *got[syncID].Reason != "incremental_ordering_ambiguous" {
		t.Fatalf("fresh query cleared ordering ambiguity: readiness=%+v err=%v", got[syncID], err)
	}
}

func TestDelayedIncrementalOrderingAmbiguityNeverReportsReady(t *testing.T) {
	type step struct {
		capture time.Duration
		key     string
		value   int
	}
	cases := map[string][]step{
		// The first two cases flank the smallest delayed delivery: whether the
		// ignored key overlaps or is absent, source-wide ordering cannot prove
		// which incremental run owns every applied key.
		"overlap-after-newer":  {{capture: -time.Minute, key: "shared", value: 2}, {capture: -2 * time.Minute, key: "shared", value: 1}},
		"disjoint-after-newer": {{capture: 0, key: "unrelated", value: 3}, {capture: -time.Minute, key: "shared", value: 2}},
		// These adjacent controls exercise both orders for two delayed runs. The
		// stored value happens to be correct in reverse order, but readiness must
		// still be conservative because that outcome is not provable per key.
		"two-delayed-forward": {{capture: 0, key: "unrelated", value: 3}, {capture: -2 * time.Minute, key: "shared", value: 1}, {capture: -time.Minute, key: "shared", value: 2}},
		"two-delayed-reverse": {{capture: 0, key: "unrelated", value: 3}, {capture: -time.Minute, key: "shared", value: 2}, {capture: -2 * time.Minute, key: "shared", value: 1}},
	}
	for name, steps := range cases {
		t.Run(name, func(t *testing.T) {
			s := openConvTestStore(t)
			_, projectID := seedConvProject(t, s)
			ctx := context.Background()
			var connectorID, syncID string
			if err := s.pg.QueryRow(ctx, `INSERT INTO data_connectors(project_id,name,kind) VALUES($1,'ordering-ambiguity','postgres') RETURNING id::text`, projectID).Scan(&connectorID); err != nil {
				t.Fatal(err)
			}
			if err := s.pg.QueryRow(ctx, `INSERT INTO connector_syncs(connector_id,project_id,source_table,key_column) VALUES($1,$2,'orders','id') RETURNING id::text`, connectorID, projectID).Scan(&syncID); err != nil {
				t.Fatal(err)
			}
			s.duck = openTestDuckDB(t)
			s.sandboxes = newSQLSandboxPool(s.duck)
			t.Cleanup(s.sandboxes.closeAll)
			now := time.Now().UTC()
			s.now = func() time.Time { return now }
			index, expected := uint64(0), uint64(1)
			for i, item := range steps {
				captured := now.Add(item.capture)
				batch := SourceReceiptMark{ProjectID: projectID, ConnectorID: connectorID, Table: "orders", SyncID: syncID,
					RunID: uuid.NewString(), BatchID: "batch-0", BatchIndex: &index, PayloadSHA256: fmt.Sprintf("%064d", i+1), CaptureStartedAt: &captured, Promoted: true}
				if err := s.duck.InsertExternalRows(ctx, projectID, connectorID, "orders", []connector.LandedRow{{Key: item.key, DataJSON: fmt.Sprintf(`{"n":%d}`, item.value)}}, AppliedMark{Source: &batch}); err != nil {
					t.Fatal(err)
				}
				complete := batch
				complete.BatchID, complete.BatchIndex, complete.PayloadSHA256 = "", nil, ""
				complete.ExpectedBatches, complete.CaptureFinishedAt, complete.Complete = &expected, &captured, true
				if err := s.duck.RecordPosition(ctx, AppliedMark{Source: &complete}); err != nil {
					t.Fatal(err)
				}
			}
			if _, _, err := s.RunSQLWithMeta(ctx, projectID, `SELECT count(*) FROM external_rows`); err != nil {
				t.Fatal(err)
			}
			readiness, err := s.SourceReadiness(ctx, projectID, []ReadinessSource{{SyncID: syncID, ConnectorID: connectorID, Table: "orders", ScheduleCron: "* * * * *", Configured: true}})
			if err != nil {
				t.Fatal(err)
			}
			got := readiness[syncID]
			if got.State != ReadinessIncomplete || got.Reason == nil || *got.Reason != "incremental_ordering_ambiguous" {
				t.Fatalf("delayed order became ready: %+v", got)
			}
		})
	}
}

func TestLegacyPublicationJournalRetainsBoundedSuffix(t *testing.T) {
	s := openConvTestStore(t)
	ctx := context.Background()
	projectID, syncID := seedConnectorSync(t, s)
	sync, err := s.ConnectorSyncForProject(ctx, projectID, syncID)
	if err != nil {
		t.Fatal(err)
	}
	base := time.Now().UTC()
	for i := 0; i < legacyPublicationReceiptLimit+5; i++ {
		if err := s.RecordPublication(ctx, PublicationObservation{ProjectID: projectID, ConnectorID: sync.ConnectorID, Table: sync.SourceTable,
			StableBatchID: fmt.Sprintf("legacy-%06d", i), PayloadSHA256: strings.Repeat("d", 64), PublishedAt: base.Add(time.Duration(i) * time.Millisecond)}); err != nil {
			t.Fatal(err)
		}
	}
	var count int
	var oldest string
	if err := s.pg.QueryRow(ctx, `SELECT count(*),min(stable_batch_id) FROM source_publication_receipts
WHERE project_id=$1 AND connector_id=$2 AND table_name=$3 AND run_id IS NULL`, projectID, sync.ConnectorID, sync.SourceTable).Scan(&count, &oldest); err != nil {
		t.Fatal(err)
	}
	if count != legacyPublicationReceiptLimit || oldest != "legacy-000005" {
		t.Fatalf("legacy publication suffix = count %d oldest %q", count, oldest)
	}
}
