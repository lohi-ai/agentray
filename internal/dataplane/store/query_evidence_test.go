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
