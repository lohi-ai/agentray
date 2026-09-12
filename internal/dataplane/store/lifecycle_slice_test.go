package storage

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
)

// Live tests for the slice-2 chart/source lifecycle contract: per-chart
// revision + archive, the dashboard-revision reorder fence, and reversible
// source archive with transactional sync pause/resume. Needs the compose
// Postgres; skips without one.

func TestChartRevisionAndArchive(t *testing.T) {
	s := openConvTestStore(t)
	ctx := context.Background()
	_, projectID := seedConvProject(t, s)

	d, err := s.CreateDashboard(ctx, projectID, "Board", "")
	if err != nil {
		t.Fatalf("create dashboard: %v", err)
	}
	c1, err := s.CreateChart(ctx, Chart{DashboardID: d.ID, ProjectID: projectID, Name: "one"})
	if err != nil {
		t.Fatalf("create chart: %v", err)
	}
	if c1.Revision != 1 || c1.ArchivedAt != nil {
		t.Fatalf("new chart = %+v, want revision 1 active", c1)
	}

	// Revision-checked update applies once; a stale revision conflicts.
	u1, err := s.UpdateChartIdempotent(ctx, Chart{ID: c1.ID, ProjectID: projectID, Name: "renamed", Kind: "bar"}, 1, "cu1", "h1")
	if err != nil || u1.Revision != 2 || u1.Name != "renamed" || u1.Kind != "bar" {
		t.Fatalf("update = %+v %v", u1, err)
	}
	if _, err := s.UpdateChartIdempotent(ctx, Chart{ID: c1.ID, ProjectID: projectID, Name: "stale"}, 1, "cu2", "h2"); !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("stale update = %v, want ErrRevisionConflict", err)
	}
	// Identical replay returns the receipt without re-mutating.
	replay, err := s.UpdateChartIdempotent(ctx, Chart{ID: c1.ID, ProjectID: projectID, Name: "renamed", Kind: "bar"}, 1, "cu1", "h1")
	if err != nil || replay.Revision != 2 {
		t.Fatalf("replay = %+v %v", replay, err)
	}
	// Same key, different payload → conflict.
	if _, err := s.UpdateChartIdempotent(ctx, Chart{ID: c1.ID, ProjectID: projectID, Name: "other"}, 1, "cu1", "hX"); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("key reuse = %v, want ErrIdempotencyConflict", err)
	}

	// Archive is soft and idempotent; the row is kept.
	a1, err := s.ArchiveChartIdempotent(ctx, projectID, c1.ID, 2, "ca1", "h3")
	if err != nil || a1.ArchivedAt == nil || a1.Revision != 3 {
		t.Fatalf("archive = %+v %v", a1, err)
	}
	a2, err := s.ArchiveChartIdempotent(ctx, projectID, c1.ID, 2, "ca1", "h3")
	if err != nil || a2.Revision != 3 {
		t.Fatalf("archive replay = %+v %v", a2, err)
	}
	// Filtered list hides it; includeArchived returns it.
	active, err := s.ListChartsFiltered(ctx, projectID, d.ID, false)
	if err != nil || len(active) != 0 {
		t.Fatalf("active list = %+v %v", active, err)
	}
	all, err := s.ListChartsFiltered(ctx, projectID, d.ID, true)
	if err != nil || len(all) != 1 {
		t.Fatalf("all list = %+v %v", all, err)
	}
	// Unarchive restores under the same revision contract.
	u2, err := s.UnarchiveChartIdempotent(ctx, projectID, c1.ID, 3, "cua1", "h4")
	if err != nil || u2.ArchivedAt != nil || u2.Revision != 4 {
		t.Fatalf("unarchive = %+v %v", u2, err)
	}

	// Cross-project isolation: another project cannot touch the chart.
	_, otherProject := seedConvProject(t, s)
	if _, err := s.ArchiveChartIdempotent(ctx, otherProject, c1.ID, 4, "cx1", "h5"); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("foreign archive = %v, want ErrNoRows", err)
	}
	if _, err := s.UpdateChartIdempotent(ctx, Chart{ID: c1.ID, ProjectID: otherProject, Name: "x"}, 4, "cx2", "h6"); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("foreign update = %v, want ErrNoRows", err)
	}
}

func TestReorderChartsFenced(t *testing.T) {
	s := openConvTestStore(t)
	ctx := context.Background()
	_, projectID := seedConvProject(t, s)

	d, err := s.CreateDashboard(ctx, projectID, "Board", "")
	if err != nil {
		t.Fatalf("create dashboard: %v", err)
	}
	var ids []string
	for _, name := range []string{"a", "b", "c"} {
		c, err := s.CreateChart(ctx, Chart{DashboardID: d.ID, ProjectID: projectID, Name: name})
		if err != nil {
			t.Fatalf("create chart: %v", err)
		}
		ids = append(ids, c.ID)
	}

	// Reorder under the dashboard revision fence: applies, bumps the fence.
	d2, err := s.ReorderChartsIdempotent(ctx, projectID, d.ID, []string{ids[2], ids[0], ids[1]}, 1, "r1", "h1")
	if err != nil || d2.Revision != 2 {
		t.Fatalf("reorder = %+v %v", d2, err)
	}
	charts, err := s.ListChartsFiltered(ctx, projectID, d.ID, false)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(charts) != 3 || charts[0].ID != ids[2] || charts[1].ID != ids[0] || charts[2].ID != ids[1] {
		t.Fatalf("order = %v", charts)
	}
	// Identical replay returns the receipt — the fence does not bump twice.
	d3, err := s.ReorderChartsIdempotent(ctx, projectID, d.ID, []string{ids[2], ids[0], ids[1]}, 1, "r1", "h1")
	if err != nil || d3.Revision != 2 {
		t.Fatalf("reorder replay = %+v %v", d3, err)
	}
	// A stale fence conflicts and leaves the order untouched.
	if _, err := s.ReorderChartsIdempotent(ctx, projectID, d.ID, []string{ids[0], ids[1], ids[2]}, 1, "r2", "h2"); !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("stale reorder = %v, want ErrRevisionConflict", err)
	}
	charts, _ = s.ListChartsFiltered(ctx, projectID, d.ID, false)
	if charts[0].ID != ids[2] {
		t.Fatalf("stale reorder mutated the board: %v", charts)
	}
	// A dashboard update moves the same fence, so a reorder issued against
	// the pre-update revision conflicts.
	if _, err := s.UpdateDashboardIdempotent(ctx, projectID, d.ID, strptr("Renamed"), nil, 2, "ud1", "h3"); err != nil {
		t.Fatalf("dashboard update: %v", err)
	}
	if _, err := s.ReorderChartsIdempotent(ctx, projectID, d.ID, ids, 2, "r3", "h4"); !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("post-update reorder = %v, want ErrRevisionConflict", err)
	}

	// Concurrent reorders on the same revision: exactly one wins.
	const n = 6
	var wg sync.WaitGroup
	wins := make(chan error, n)
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			order := []string{ids[i%3], ids[(i+1)%3], ids[(i+2)%3]}
			_, err := s.ReorderChartsIdempotent(ctx, projectID, d.ID, order, 3, "", "")
			wins <- err
		}(i)
	}
	wg.Wait()
	close(wins)
	succeeded, conflicts := 0, 0
	for err := range wins {
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, ErrRevisionConflict):
			conflicts++
		default:
			t.Fatalf("concurrent reorder err = %v", err)
		}
	}
	if succeeded != 1 || conflicts != n-1 {
		t.Fatalf("concurrent reorders: %d succeeded, %d conflicts — want 1 and %d", succeeded, conflicts, n-1)
	}
}

func TestSourceArchivePausesSyncs(t *testing.T) {
	s := openConvTestStore(t)
	t.Setenv("AGENT_KEY_ENC_SECRET", "source-archive-test-secret")
	ctx := context.Background()
	userID, projectID := seedConvProject(t, s)

	cred, err := s.CreateSourceCredential(ctx, userID, projectID, "prod-pg", "postgres://u:p@h:5432/db")
	if err != nil {
		t.Fatalf("create credential: %v", err)
	}
	dc, err := s.CreateDataConnectorIdempotent(ctx, projectID, "warehouse", "postgres", cred.ID, "ck1", "h1")
	if err != nil {
		t.Fatalf("create connector: %v", err)
	}
	enabledSync, err := s.CreateConnectorSync(ctx, userID, projectID, dc.ID, ConnectorSyncInput{
		SourceTable: "orders", KeyColumn: "id", ScheduleCron: "*/5 * * * *", Enabled: true,
	})
	if err != nil {
		t.Fatalf("create enabled sync: %v", err)
	}
	pausedSync, err := s.CreateConnectorSync(ctx, userID, projectID, dc.ID, ConnectorSyncInput{
		SourceTable: "customers", KeyColumn: "id", Enabled: false,
	})
	if err != nil {
		t.Fatalf("create paused sync: %v", err)
	}

	// Archive: connector keeps its row + credential; the enabled sync is
	// disabled and marked, the operator-paused one stays unmarked.
	arch, err := s.ArchiveDataConnectorIdempotent(ctx, projectID, dc.ID, 1, "sa1", "h2")
	if err != nil || arch.ArchivedAt == nil || arch.Revision != 2 {
		t.Fatalf("archive = %+v %v", arch, err)
	}
	syncs, err := s.ListConnectorSyncsForProject(ctx, projectID, dc.ID)
	if err != nil || len(syncs) != 2 {
		t.Fatalf("syncs = %+v %v", syncs, err)
	}
	for _, cs := range syncs {
		switch cs.ID {
		case enabledSync.ID:
			if cs.Enabled || !cs.DisabledByArchive {
				t.Fatalf("enabled sync after archive = %+v", cs)
			}
		case pausedSync.ID:
			if cs.Enabled || cs.DisabledByArchive {
				t.Fatalf("paused sync after archive = %+v", cs)
			}
		}
	}
	// The engine tick and the job resolver both exclude the archived source.
	enabled, err := s.ListEnabledConnectorSyncs(ctx)
	if err != nil {
		t.Fatalf("enabled syncs: %v", err)
	}
	for _, ss := range enabled {
		if ss.ID == enabledSync.ID {
			t.Fatal("archived connector's sync still scheduled")
		}
	}
	if _, err := s.ConnectorSyncJob(ctx, enabledSync.ID); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("sync job under archived connector = %v, want ErrNoRows", err)
	}
	// Probes fail closed; the credential reference is preserved.
	if _, _, err := s.ConnectorDSNForProject(ctx, projectID, dc.ID); !errors.Is(err, ErrSourceArchived) {
		t.Fatalf("dsn under archived connector = %v, want ErrSourceArchived", err)
	}
	// A manual resume under an archived connector is rejected — the resume
	// path is unarchive, not a per-sync enable.
	cur, err := s.ConnectorSyncForProject(ctx, projectID, enabledSync.ID)
	if err != nil {
		t.Fatalf("read sync: %v", err)
	}
	// Run admission rejects inside its locked transaction — no run row is
	// persisted for a sync under an archived connector.
	if _, _, err := s.EnqueueConnectorRun(ctx, projectID, enabledSync.ID, "run1"); !errors.Is(err, ErrSourceArchived) {
		t.Fatalf("enqueue under archived connector = %v, want ErrSourceArchived", err)
	}
	if _, err := s.SetConnectorSyncEnabledIdempotent(ctx, projectID, enabledSync.ID, true, cur.Revision, "pe1", "h3"); !errors.Is(err, ErrSourceArchived) {
		t.Fatalf("enable under archived connector = %v, want ErrSourceArchived", err)
	}
	// Idempotent archive replay returns the receipt.
	replay, err := s.ArchiveDataConnectorIdempotent(ctx, projectID, dc.ID, 1, "sa1", "h2")
	if err != nil || replay.Revision != 2 {
		t.Fatalf("archive replay = %+v %v", replay, err)
	}

	// Unarchive resumes exactly the syncs the archive paused.
	un, err := s.UnarchiveDataConnectorIdempotent(ctx, projectID, dc.ID, 2, "su1", "h4")
	if err != nil || un.ArchivedAt != nil || un.Revision != 3 {
		t.Fatalf("unarchive = %+v %v", un, err)
	}
	syncs, err = s.ListConnectorSyncsForProject(ctx, projectID, dc.ID)
	if err != nil {
		t.Fatalf("syncs after unarchive: %v", err)
	}
	for _, cs := range syncs {
		switch cs.ID {
		case enabledSync.ID:
			if !cs.Enabled || cs.DisabledByArchive {
				t.Fatalf("archive-paused sync after unarchive = %+v", cs)
			}
		case pausedSync.ID:
			if cs.Enabled {
				t.Fatalf("operator-paused sync resumed by unarchive: %+v", cs)
			}
		}
	}
	// The credential still resolves after restore.
	if _, dsn, err := s.ConnectorDSNForProject(ctx, projectID, dc.ID); err != nil || dsn != "postgres://u:p@h:5432/db" {
		t.Fatalf("dsn after unarchive = %q %v", dsn, err)
	}

	// Cross-project isolation: another project cannot archive the connector.
	_, otherProject := seedConvProject(t, s)
	if _, err := s.ArchiveDataConnectorIdempotent(ctx, otherProject, dc.ID, 3, "sx1", "h5"); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("foreign archive = %v, want ErrNoRows", err)
	}
}

func TestLegacyInlineConnectorStillResolves(t *testing.T) {
	s := openConvTestStore(t)
	t.Setenv("AGENT_KEY_ENC_SECRET", "legacy-inline-test-secret")
	ctx := context.Background()
	userID, projectID := seedConvProject(t, s)

	// A connector carrying legacy inline ciphertext (pre-credential rows)
	// still resolves its DSN through the same read path.
	dc, err := s.CreateDataConnector(ctx, userID, projectID, "legacy", "postgres", "postgres://legacy:p@h:5432/db")
	if err != nil {
		t.Fatalf("create legacy connector: %v", err)
	}
	kind, dsn, err := s.ConnectorDSNForProject(ctx, projectID, dc.ID)
	if err != nil || kind != "postgres" || dsn != "postgres://legacy:p@h:5432/db" {
		t.Fatalf("legacy dsn resolve = %q %q %v", kind, dsn, err)
	}
}
