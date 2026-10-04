package storage

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestQueryEvidenceIsProjectScopedAndReportsServerBound(t *testing.T) {
	d := openTestDuckDB(t)
	ctx := context.Background()
	p1, p2 := uuid.NewString(), uuid.NewString()
	now := time.Now().UTC()
	if err := d.InsertEvents(ctx, []Event{duckEvent(p1, uuid.NewString(), "u1", now), duckEvent(p2, uuid.NewString(), "u2", now)}); err != nil {
		t.Fatal(err)
	}
	pool := newSQLSandboxPool(d)
	t.Cleanup(pool.closeAll)
	s := &Store{duck: d, sandboxes: pool, now: func() time.Time { return now }}
	rows, meta, err := s.RunSQLWithMeta(ctx, p1, `SELECT event_name FROM events`)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || meta.QueryRef == "" || meta.QueryDigest == "" || meta.ServingDataWatermark == nil {
		t.Fatalf("rows/meta = %v %+v", rows, meta)
	}
	if meta.ResultCompleteness != ResultComplete || meta.Truncated != nil {
		t.Fatalf("completeness = %+v", meta)
	}
	_, other, err := s.RunSQLWithMeta(ctx, p2, `SELECT event_name FROM events`)
	if err != nil {
		t.Fatal(err)
	}
	if meta.QueryDigest == other.QueryDigest {
		t.Fatal("query digest crossed project scope")
	}
}

func TestQueryEvidenceReadSnapshotDoesNotHoldWriterGate(t *testing.T) {
	d := openTestDuckDB(t)
	ctx := context.Background()
	projectID := uuid.NewString()
	now := time.Now().UTC()
	if err := d.InsertEvents(ctx, []Event{duckEvent(projectID, uuid.NewString(), "u1", now)}); err != nil {
		t.Fatal(err)
	}

	var first, second int
	err := d.ReadSnapshot(ctx, func(snapshot duckDBSnapshot) error {
		if err := snapshot.QueryRowContext(ctx, `SELECT count(*) FROM events WHERE project_id = ?`, projectID).Scan(&first); err != nil {
			return err
		}
		written := make(chan error, 1)
		go func() {
			written <- d.InsertEvents(ctx, []Event{duckEvent(projectID, uuid.NewString(), "u2", now.Add(time.Second))})
		}()
		select {
		case err := <-written:
			if err != nil {
				return err
			}
		case <-time.After(5 * time.Second):
			return fmt.Errorf("ingest writer was blocked by the sandbox read snapshot")
		}
		return snapshot.QueryRowContext(ctx, `SELECT count(*) FROM events WHERE project_id = ?`, projectID).Scan(&second)
	})
	if err != nil {
		t.Fatal(err)
	}
	if first != 1 || second != first {
		t.Fatalf("snapshot counts changed during refresh: first=%d second=%d", first, second)
	}
}

func TestQueryEvidenceConfirmsAllSourcesWhileCappingPublicDetails(t *testing.T) {
	d := openTestDuckDB(t)
	ctx := context.Background()
	projectID := uuid.NewString()
	now := time.Now().UTC()
	expected := uint64(0)
	for i := 0; i < 65; i++ {
		mark := SourceReceiptMark{ProjectID: projectID, ConnectorID: uuid.NewString(), Table: fmt.Sprintf("table_%02d", i),
			SyncID: uuid.NewString(), RunID: uuid.NewString(), ExpectedBatches: &expected,
			CaptureFinishedAt: &now, Complete: true, Promoted: true}
		if err := d.RecordPosition(ctx, AppliedMark{Source: &mark}); err != nil {
			t.Fatal(err)
		}
	}
	evidence, err := d.projectEvidence(ctx, projectID)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(evidence.Confirmations); got != 65 {
		t.Fatalf("internal confirmations = %d, want 65", got)
	}
	if got := len(evidence.Watermark.Sources); got != 64 || evidence.Watermark.TotalSources != 65 || !evidence.Watermark.SourcesTruncated {
		t.Fatalf("public watermark cap = %+v", evidence.Watermark)
	}

	pool := newSQLSandboxPool(d)
	t.Cleanup(pool.closeAll)
	s := &Store{duck: d, sandboxes: pool, now: func() time.Time { return now }}
	_, meta, err := s.RunSQLWithMeta(ctx, projectID, `SELECT count(*) FROM events`)
	if err != nil {
		t.Fatal(err)
	}
	if meta.AvailabilityReason == nil || *meta.AvailabilityReason != "serving_watermark_sources_summarized" {
		t.Fatalf("overflow description = %+v", meta)
	}
	if got := len(pool.confirmations(projectID)); got != 65 {
		t.Fatalf("live sandbox confirmations = %d, want 65", got)
	}
}
