package storage

import (
	"context"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lohi-ai/agentray/internal/shared/config"
)

// The deploy-time repair, against a real Postgres. Skipped when no test
// database is reachable, like the other *_live_test.go files:
//
//	AGENTRAY_TEST_DATABASE_URL=postgres://lohi:lohi@localhost:5434/lohi_analytics?sslmode=disable \
//	go test ./internal/dataplane/store/ -run SeededChart -v
func TestRepairSeededChartsRewritesOnlyTheSeededQuery(t *testing.T) {
	s, ctx := seededChartRepairStore(t)

	project, err := s.CreateProject(ctx, "seeded-chart-repair-"+uuid.NewString())
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	t.Cleanup(func() {
		if _, err := s.pg.Exec(context.Background(), `DELETE FROM projects WHERE id = $1`, project.ID); err != nil {
			t.Errorf("cleanup: %v", err)
		}
	})
	dashboard, err := s.CreateDashboard(ctx, project.ID, "board", "")
	if err != nil {
		t.Fatalf("CreateDashboard: %v", err)
	}

	newChart := func(chart Chart) Chart {
		t.Helper()
		chart.DashboardID = dashboard.ID
		chart.ProjectID = project.ID
		chart.XField = "user_type"
		chart.YField = "visitors"
		created, err := s.CreateChart(ctx, chart)
		if err != nil {
			t.Fatalf("CreateChart(%s): %v", chart.Name, err)
		}
		return created
	}
	starter := newChart(Chart{Name: "Visitors: guest vs identified", SQL: staleStarterGuestVsIdentifiedSQL})
	template := newChart(Chart{Name: "Guest vs identified", SQL: staleTemplateGuestVsIdentifiedSQL})
	// A customer's own chart that reads an email property on purpose. The repair
	// is scoped to AgentRay's seeded queries and must leave it alone.
	ownSQL := `SELECT count(*) AS visitors FROM events WHERE event_name = 'user.pageview' AND json_extract_string(properties, '$.email') <> ''`
	own := newChart(Chart{Name: "Readers who left an email", SQL: ownSQL})

	repaired, projects, err := s.repairSeededCharts(ctx)
	if err != nil {
		t.Fatalf("repairSeededCharts: %v", err)
	}
	// Other projects on a shared test database may hold the same seeded rows, so
	// the total is a lower bound — the point is the scope is reported at all.
	if repaired < 2 || projects < 1 {
		t.Errorf("repair reported %d chart(s) in %d project(s), want at least this test's two", repaired, projects)
	}

	for _, want := range []struct {
		name   string
		before Chart
		want   string
	}{
		{"starter board", starter, guestVsIdentifiedSQL("canonical_id", true)},
		{"cloned template", template, guestVsIdentifiedSQL("distinct_id", false)},
	} {
		got, err := s.ChartForProject(ctx, project.ID, want.before.ID)
		if err != nil {
			t.Fatalf("ChartForProject(%s): %v", want.name, err)
		}
		if got.SQL != want.want {
			t.Errorf("%s chart kept the stale query:\n got %s\nwant %s", want.name, got.SQL, want.want)
		}
		if got.Revision != want.before.Revision+1 {
			t.Errorf("%s chart revision = %d, want %d — a content change must fence a stale edit", want.name, got.Revision, want.before.Revision+1)
		}
	}

	got, err := s.ChartForProject(ctx, project.ID, own.ID)
	if err != nil {
		t.Fatalf("ChartForProject(own): %v", err)
	}
	if got.SQL != ownSQL || got.Revision != own.Revision {
		t.Errorf("the repair touched a customer chart: sql=%q revision=%d", got.SQL, got.Revision)
	}

	// Nothing stale is left anywhere, so the next boot repairs nothing.
	repaired, projects, err = s.repairSeededCharts(ctx)
	if err != nil {
		t.Fatalf("second repairSeededCharts: %v", err)
	}
	if repaired != 0 || projects != 0 {
		t.Errorf("second repair rewrote %d chart(s) in %d project(s), want none — it is not idempotent", repaired, projects)
	}
}

// seededChartRepairStore opens the shared test database and brings it to
// schema, mirroring the other live store tests.
func seededChartRepairStore(t *testing.T) (*Store, context.Context) {
	t.Helper()
	url := os.Getenv("AGENTRAY_TEST_DATABASE_URL")
	if url == "" {
		url = "postgres://lohi:lohi@localhost:5434/lohi_analytics?sslmode=disable"
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Skipf("no test database (%v)", err)
	}
	t.Cleanup(pool.Close)
	if err := pool.Ping(ctx); err != nil {
		t.Skipf("test database unreachable (%v)", err)
	}
	s := &Store{pg: pool}
	if err := s.migratePostgres(ctx, config.Config{
		PostgresURL:           url,
		DefaultProjectName:    "seeded-chart-repair",
		DefaultProjectAPIKey:  "seeded_chart_repair_test_key",
	}); err != nil {
		t.Fatalf("migratePostgres: %v", err)
	}
	return s, ctx
}
