package storage

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lohi-ai/agentray/internal/shared/config"
)

func strptr(s string) *string { return &s }

// Live tests for dashboard revision/archive and idempotency receipts. Needs
// the compose Postgres; skips without one.

func TestDashboardRevisionAndArchive(t *testing.T) {
	s := openConvTestStore(t)
	ctx := context.Background()
	_, projectID := seedConvProject(t, s)

	d, err := s.CreateDashboard(ctx, projectID, "Board A", "first")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if d.Revision != 1 || d.ArchivedAt != nil {
		t.Fatalf("new dashboard = rev %d archived %v", d.Revision, d.ArchivedAt)
	}

	// Revision-checked update: correct revision applies and bumps once.
	d2, err := s.UpdateDashboardRevision(ctx, projectID, d.ID, strptr("Board A2"), strptr("renamed"), 1)
	if err != nil {
		t.Fatalf("update rev1: %v", err)
	}
	if d2.Revision != 2 || d2.Name != "Board A2" {
		t.Fatalf("updated = rev %d name %q", d2.Revision, d2.Name)
	}

	// Stale revision conflicts; the row is unchanged.
	if _, err := s.UpdateDashboardRevision(ctx, projectID, d.ID, strptr("sneaky"), nil, 1); !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("stale update err = %v, want ErrRevisionConflict", err)
	}
	cur, err := s.ListDashboards(ctx, projectID)
	if err != nil || len(cur) != 1 || cur[0].Name != "Board A2" || cur[0].Revision != 2 {
		t.Fatalf("row changed under conflict: %+v", cur)
	}

	// Unknown id is not-found, not conflict.
	if _, err := s.UpdateDashboardRevision(ctx, projectID, "00000000-0000-0000-0000-000000000000", strptr("x"), nil, 1); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("missing dashboard err = %v, want ErrNoRows", err)
	}

	// Archive: soft, revision-checked, and a repeat does not mutate.
	arch, err := s.ArchiveDashboard(ctx, projectID, d.ID, 2)
	if err != nil {
		t.Fatalf("archive: %v", err)
	}
	if arch.ArchivedAt == nil || arch.Revision != 3 {
		t.Fatalf("archived = %+v", arch)
	}
	// A repeat — even with a stale revision — returns the row unchanged.
	again, err := s.ArchiveDashboard(ctx, projectID, d.ID, 1)
	if err != nil {
		t.Fatalf("re-archive: %v", err)
	}
	if again.ArchivedAt == nil || again.Revision != 3 {
		t.Fatalf("re-archive mutated the row: %+v", again)
	}
	// Filtered list omits archived; unfiltered keeps it.
	active, err := s.ListDashboardsFiltered(ctx, projectID, false)
	if err != nil || len(active) != 0 {
		t.Fatalf("active list = %v", active)
	}
	all, err := s.ListDashboardsFiltered(ctx, projectID, true)
	if err != nil || len(all) != 1 {
		t.Fatalf("all list = %v", all)
	}

	// A stale revision on a LIVE dashboard conflicts — no bypass.
	d3, err := s.CreateDashboard(ctx, projectID, "Board B", "")
	if err != nil {
		t.Fatalf("create B: %v", err)
	}
	if _, err := s.ArchiveDashboard(ctx, projectID, d3.ID, 99); !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("stale archive err = %v, want ErrRevisionConflict", err)
	}

	// Cross-project isolation: another project's id cannot be touched.
	_, otherProject := seedConvProject(t, s)
	if _, err := s.UpdateDashboardRevision(ctx, otherProject, d.ID, strptr("nope"), nil, 3); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("cross-project update err = %v, want ErrNoRows", err)
	}
	if _, err := s.ArchiveDashboard(ctx, otherProject, d.ID, 3); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("cross-project archive err = %v, want ErrNoRows", err)
	}
}

// TestIdempotentWrites exercises the atomic claim+mutation+receipt: a replay
// returns the first result without re-mutating, a reused key with a different
// payload conflicts, and concurrent claimants never double-apply.
func TestIdempotentWrites(t *testing.T) {
	s := openConvTestStore(t)
	ctx := context.Background()
	_, projectID := seedConvProject(t, s)

	d, err := s.CreateDashboard(ctx, projectID, "Board", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// First call claims, mutates, and records in one transaction.
	d1, err := s.UpdateDashboardIdempotent(ctx, projectID, d.ID, strptr("Renamed"), nil, 1, "k1", "hash-a")
	if err != nil {
		t.Fatalf("first update: %v", err)
	}
	if d1.Revision != 2 || d1.Name != "Renamed" {
		t.Fatalf("first = %+v", d1)
	}
	// Identical replay returns the receipt — revision does NOT bump again.
	d2, err := s.UpdateDashboardIdempotent(ctx, projectID, d.ID, strptr("Renamed"), nil, 1, "k1", "hash-a")
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if d2.Revision != 2 || d2.Name != "Renamed" {
		t.Fatalf("replay mutated: %+v", d2)
	}
	// Same key, different payload → conflict, no mutation.
	if _, err := s.UpdateDashboardIdempotent(ctx, projectID, d.ID, strptr("Other"), nil, 2, "k1", "hash-b"); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("reuse err = %v, want ErrIdempotencyConflict", err)
	}
	cur, _ := s.ListDashboards(ctx, projectID)
	if len(cur) != 1 || cur[0].Name != "Renamed" || cur[0].Revision != 2 {
		t.Fatalf("conflict mutated the row: %+v", cur)
	}
	// A failed mutation rolls back claim AND write — the key is free again.
	if _, err := s.UpdateDashboardIdempotent(ctx, projectID, d.ID, strptr("X"), nil, 99, "k2", "hash-c"); !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("failed mutation err = %v, want ErrRevisionConflict", err)
	}
	if _, err := s.UpdateDashboardIdempotent(ctx, projectID, d.ID, strptr("Y"), nil, 2, "k2", "hash-c"); err != nil {
		t.Fatalf("key not freed after rollback: %v", err)
	}
	// Concurrent identical claims: both succeed with the same result and the
	// mutation ran exactly once (revision moved once, not twice).
	const n = 8
	errs := make(chan error, n)
	revs := make(chan int64, n)
	for i := 0; i < n; i++ {
		go func() {
			got, err := s.ArchiveDashboardIdempotent(ctx, projectID, d.ID, 3, "k3", "hash-d")
			if err != nil {
				errs <- err
				return
			}
			revs <- got.Revision
		}()
	}
	for i := 0; i < n; i++ {
		select {
		case err := <-errs:
			t.Fatalf("concurrent archive: %v", err)
		case rev := <-revs:
			if rev != 4 {
				t.Fatalf("revision = %d, want 4 (exactly one mutation)", rev)
			}
		}
	}
}

// TestIdempotentReplaySingleConn proves the replay path cannot deadlock a
// one-connection pool: the losing claimant reads the winner's receipt through
// its own transaction, not a second pool connection.
func TestIdempotentReplaySingleConn(t *testing.T) {
	url := os.Getenv("AGENTRAY_TEST_DATABASE_URL")
	if url == "" {
		url = "postgres://lohi:lohi@localhost:5434/lohi_analytics?sslmode=disable"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Skipf("no test database (%v)", err)
	}
	cfg.MaxConns = 1
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Skipf("no test database (%v)", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Skipf("test database unreachable (%v)", err)
	}
	defer pool.Close()
	s := &Store{pg: pool}
	if err := s.migratePostgres(ctx, config.Config{PostgresURL: url, DefaultProjectName: "idem-test", DefaultProjectAPIKey: "idem_test_key"}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	_, projectID := seedConvProject(t, s)

	d, err := s.CreateDashboard(ctx, projectID, "Board", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.UpdateDashboardIdempotent(ctx, projectID, d.ID, strptr("Once"), nil, 1, "k1", "hash-a"); err != nil {
		t.Fatalf("first: %v", err)
	}
	// Replay on the same single connection: must return the receipt, not hang.
	got, err := s.UpdateDashboardIdempotent(ctx, projectID, d.ID, strptr("Once"), nil, 1, "k1", "hash-a")
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if got.Name != "Once" || got.Revision != 2 {
		t.Fatalf("replay = %+v", got)
	}
}
