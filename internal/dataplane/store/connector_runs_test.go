package storage

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/lohi-ai/agentray/internal/dataplane/connector"
)

// Live tests for the persistent connector-run contract. Needs the compose
// Postgres; skips without one.

// seedConnectorSync creates a project + connector + sync and returns
// (projectID, syncID).
func seedConnectorSync(t *testing.T, s *Store) (projectID, syncID string) {
	t.Helper()
	t.Setenv("AGENT_KEY_ENC_SECRET", "connector-runs-test-secret")
	ctx := context.Background()
	userID, projectID2 := seedConvProject(t, s)
	projectID = projectID2
	dc, err := s.CreateDataConnector(ctx, userID, projectID, "src", "postgres", "postgres://x")
	if err != nil {
		t.Fatalf("connector: %v", err)
	}
	sync, err := s.CreateConnectorSync(ctx, userID, projectID, dc.ID, ConnectorSyncInput{
		SourceTable: "users", KeyColumn: "id", CursorColumn: "updated_at", Enabled: true,
	})
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	return projectID, sync.ID
}

func TestConnectorRunLifecycle(t *testing.T) {
	s := openConvTestStore(t)
	ctx := context.Background()
	projectID, syncID := seedConnectorSync(t, s)

	// Enqueue → queued row.
	run, enqueued, err := s.EnqueueConnectorRun(ctx, projectID, syncID, "k1")
	if err != nil || !enqueued || run.Status != "queued" {
		t.Fatalf("enqueue = %+v %v %v", run, enqueued, err)
	}
	// Idempotent replay returns the same row.
	replay, enqueued, err := s.EnqueueConnectorRun(ctx, projectID, syncID, "k1")
	if err != nil || enqueued || replay.ID != run.ID {
		t.Fatalf("replay = %+v %v %v", replay, enqueued, err)
	}
	// A second enqueue without a key observes the active run.
	active, enqueued, err := s.EnqueueConnectorRun(ctx, projectID, syncID, "")
	if err != nil || enqueued || active.ID != run.ID {
		t.Fatalf("second enqueue = %+v %v %v", active, enqueued, err)
	}
	// Claim moves queued → running under an owner.
	claimed, ok, err := s.ClaimConnectorRun(ctx, run.ID, "owner-a")
	if err != nil || !ok || claimed.Status != "running" {
		t.Fatalf("claim = %+v %v %v", claimed, ok, err)
	}
	// Double-claim loses.
	if _, ok, err := s.ClaimConnectorRun(ctx, run.ID, "owner-b"); err != nil || ok {
		t.Fatalf("double claim = %v %v", ok, err)
	}
	// Heartbeat keeps the lease and reports no cancel.
	cancelReq, running, err := s.HeartbeatConnectorRun(ctx, run.ID)
	if err != nil || cancelReq || !running {
		t.Fatalf("heartbeat = %v %v %v", cancelReq, running, err)
	}
	// Cancel a running run: flag set, status still running until finish.
	cancelled, err := s.CancelConnectorRun(ctx, projectID, run.ID)
	if err != nil || !cancelled.CancelRequested || cancelled.Status != "running" {
		t.Fatalf("cancel = %+v %v", cancelled, err)
	}
	cancelReq, running, err = s.HeartbeatConnectorRun(ctx, run.ID)
	if err != nil || !cancelReq || !running {
		t.Fatalf("heartbeat after cancel = %v %v %v", cancelReq, running, err)
	}
	// Finish as cancelled.
	if err := s.FinishConnectorRun(ctx, run.ID, syncID, "owner-a", connector.SyncResult{Rows: 3}, true); err != nil {
		t.Fatalf("finish: %v", err)
	}
	got, err := s.ConnectorRunForProject(ctx, projectID, run.ID)
	if err != nil || got.Status != "cancelled" || got.Rows != 3 {
		t.Fatalf("finished run = %+v %v", got, err)
	}
	// Cancelling a terminal run returns it unchanged — no history rewrite.
	again, err := s.CancelConnectorRun(ctx, projectID, run.ID)
	if err != nil || again.Status != "cancelled" {
		t.Fatalf("cancel terminal = %+v %v", again, err)
	}
	// Cross-project read is not-found.
	if _, err := s.ConnectorRunForProject(ctx, "00000000-0000-0000-0000-000000000000", run.ID); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("cross-project run err = %v, want ErrNoRows", err)
	}
}

func TestLatestConnectorRunsForProject(t *testing.T) {
	s := openConvTestStore(t)
	ctx := context.Background()
	t.Setenv("AGENT_KEY_ENC_SECRET", "connector-runs-batch-test-secret")
	userID, projectID := seedConvProject(t, s)
	dc, err := s.CreateDataConnector(ctx, userID, projectID, "src", "postgres", "postgres://x")
	if err != nil {
		t.Fatalf("connector: %v", err)
	}
	firstSync, err := s.CreateConnectorSync(ctx, userID, projectID, dc.ID, ConnectorSyncInput{
		SourceTable: "users", KeyColumn: "id", CursorColumn: "updated_at", Enabled: true,
	})
	if err != nil {
		t.Fatalf("first sync: %v", err)
	}
	secondSync, err := s.CreateConnectorSync(ctx, userID, projectID, dc.ID, ConnectorSyncInput{
		SourceTable: "orders", KeyColumn: "id", CursorColumn: "updated_at", Enabled: true,
	})
	if err != nil {
		t.Fatalf("second sync: %v", err)
	}
	firstRun, _, err := s.EnqueueConnectorRun(ctx, projectID, firstSync.ID, "first-1")
	if err != nil {
		t.Fatalf("first run: %v", err)
	}
	if _, err := s.CancelConnectorRun(ctx, projectID, firstRun.ID); err != nil {
		t.Fatalf("cancel first run: %v", err)
	}
	newestFirst, _, err := s.EnqueueConnectorRun(ctx, projectID, firstSync.ID, "first-2")
	if err != nil {
		t.Fatalf("newest first run: %v", err)
	}
	secondRun, _, err := s.EnqueueConnectorRun(ctx, projectID, secondSync.ID, "second-1")
	if err != nil {
		t.Fatalf("second run: %v", err)
	}

	runs, err := s.LatestConnectorRunsForProject(ctx, projectID, []string{firstSync.ID, secondSync.ID, "00000000-0000-0000-0000-000000000000"})
	if err != nil {
		t.Fatalf("latest batch: %v", err)
	}
	if runs[firstSync.ID].ID != newestFirst.ID || runs[secondSync.ID].ID != secondRun.ID {
		t.Fatalf("latest batch = %+v, want first=%s second=%s", runs, newestFirst.ID, secondRun.ID)
	}
	if _, found := runs["00000000-0000-0000-0000-000000000000"]; found {
		t.Fatalf("missing sync unexpectedly has a run: %+v", runs)
	}
}

func TestConnectorRunPauseOrdering(t *testing.T) {
	s := openConvTestStore(t)
	ctx := context.Background()
	projectID, syncID := seedConnectorSync(t, s)

	// Pause the sync (revision 1 → 2), then enqueue must refuse.
	if _, err := s.SetConnectorSyncEnabled(ctx, projectID, syncID, false, 1); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if _, _, err := s.EnqueueConnectorRun(ctx, projectID, syncID, ""); !errors.Is(err, ErrSyncPaused) {
		t.Fatalf("enqueue on paused = %v, want ErrSyncPaused", err)
	}
	// Stale revision conflicts.
	if _, err := s.SetConnectorSyncEnabled(ctx, projectID, syncID, true, 1); !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("stale resume = %v, want ErrRevisionConflict", err)
	}
	// Correct revision resumes; enqueue works again.
	if _, err := s.SetConnectorSyncEnabled(ctx, projectID, syncID, true, 2); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if _, enqueued, err := s.EnqueueConnectorRun(ctx, projectID, syncID, ""); err != nil || !enqueued {
		t.Fatalf("enqueue after resume = %v %v", enqueued, err)
	}
}

func TestReconcileFencesOnlyStale(t *testing.T) {
	s := openConvTestStore(t)
	ctx := context.Background()
	projectID, syncID := seedConnectorSync(t, s)

	// A live run with a fresh heartbeat survives reconcile.
	live, _, err := s.EnqueueConnectorRun(ctx, projectID, syncID, "")
	if err != nil {
		t.Fatalf("enqueue live: %v", err)
	}
	if _, ok, err := s.ClaimConnectorRun(ctx, live.ID, "owner-live"); err != nil || !ok {
		t.Fatalf("claim live: %v %v", ok, err)
	}
	if _, _, err := s.HeartbeatConnectorRun(ctx, live.ID); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	// Reconcile may fence stale rows left by earlier tests in this persistent
	// dev DB — what matters is the live run survives.
	if _, err := s.ReconcileConnectorRuns(ctx, time.Now().Add(-2*time.Minute)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	got, _ := s.ConnectorRunForProject(ctx, projectID, live.ID)
	if got.Status != "running" {
		t.Fatalf("live run status = %q, want running", got.Status)
	}

	// A stale running row (old heartbeat) is fenced.
	if _, err := s.pg.Exec(ctx, `UPDATE connector_runs SET heartbeat_at = now() - interval '10 minutes' WHERE id = $1`, live.ID); err != nil {
		t.Fatalf("age heartbeat: %v", err)
	}
	if _, err := s.ReconcileConnectorRuns(ctx, time.Now().Add(-2*time.Minute)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	got, _ = s.ConnectorRunForProject(ctx, projectID, live.ID)
	if got.Status != "failed" {
		t.Fatalf("stale run status = %q, want failed", got.Status)
	}
}

// The engine's capacity path asks this read-only question instead of running
// the enqueue transaction, so it must resolve both the run's own stamp and the
// alias a retry bound to an already-active run — and must not invent a receipt
// for a key it never saw.
func TestConnectorRunByIdempotencyKeyResolvesReceipt(t *testing.T) {
	s := openConvTestStore(t)
	ctx := context.Background()
	projectID, syncID := seedConnectorSync(t, s)

	run, enqueued, err := s.EnqueueConnectorRun(ctx, projectID, syncID, "k1")
	if err != nil || !enqueued {
		t.Fatalf("enqueue = %+v %v %v", run, enqueued, err)
	}
	found, ok, err := s.ConnectorRunByIdempotencyKey(ctx, projectID, syncID, "k1")
	if err != nil || !ok || found.ID != run.ID {
		t.Fatalf("resolve stamped key = %+v %v %v", found, ok, err)
	}
	// A retry under a fresh key observes the active run and binds its key to
	// that run; resolution must follow the alias.
	active, enqueued, err := s.EnqueueConnectorRun(ctx, projectID, syncID, "k2")
	if err != nil || enqueued || active.ID != run.ID {
		t.Fatalf("aliased enqueue = %+v %v %v", active, enqueued, err)
	}
	found, ok, err = s.ConnectorRunByIdempotencyKey(ctx, projectID, syncID, "k2")
	if err != nil || !ok || found.ID != run.ID {
		t.Fatalf("resolve aliased key = %+v %v %v", found, ok, err)
	}
	if _, ok, err := s.ConnectorRunByIdempotencyKey(ctx, projectID, syncID, "never-seen"); err != nil || ok {
		t.Fatalf("unknown key = %v %v", ok, err)
	}
	if _, ok, err := s.ConnectorRunByIdempotencyKey(ctx, projectID, syncID, "   "); err != nil || ok {
		t.Fatalf("blank key = %v %v", ok, err)
	}
}
