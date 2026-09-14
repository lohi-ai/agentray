package storage

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

// The deploy-time repair, against a real Postgres: it rewrites every shipped
// version of the seeded query on the boards that already hold it, leaves every
// other chart byte-identical, and is a no-op on the second run. Skipped when no
// test database is reachable — openConvTestStore is the package's live-store
// opener, used well beyond the conversation tests it was named for.
func TestRepairSeededChartsRewritesOnlyTheSeededQuery(t *testing.T) {
	s := openConvTestStore(t)
	ctx := context.Background()

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

	// Each shipped version of the seeded chart, with the query it must become.
	// The two ClickHouse-era strings are still on boards seeded before the
	// engine port, which never translated stored chart SQL.
	cases := []struct {
		name  string
		stale string
		want  string
	}{
		{"DuckDB starter", staleStarterGuestVsIdentifiedSQL, guestVsIdentifiedSQL("canonical_id", true)},
		{"DuckDB template", staleTemplateGuestVsIdentifiedSQL, guestVsIdentifiedSQL("distinct_id", false)},
		{"ClickHouse starter", legacyStarterGuestVsIdentifiedSQL, guestVsIdentifiedSQL("canonical_id", true)},
		{"ClickHouse template", legacyTemplateGuestVsIdentifiedSQL, guestVsIdentifiedSQL("distinct_id", false)},
	}
	// A customer's own chart that reads an email property on purpose. The repair
	// is scoped to AgentRay's seeded queries and must leave it alone.
	ownSQL := `SELECT count(*) AS visitors FROM events WHERE event_name = 'user.pageview' AND json_extract_string(properties, '$.email') <> ''`

	seeded := make([]Chart, len(cases))
	for i, c := range cases {
		seeded[i], err = s.CreateChart(ctx, Chart{
			DashboardID: dashboard.ID, ProjectID: project.ID, Name: c.name,
			SQL: c.stale, XField: "user_type", YField: "visitors",
		})
		if err != nil {
			t.Fatalf("CreateChart(%s): %v", c.name, err)
		}
	}
	own, err := s.CreateChart(ctx, Chart{
		DashboardID: dashboard.ID, ProjectID: project.ID, Name: "Readers who left an email",
		SQL: ownSQL, XField: "visitors",
	})
	if err != nil {
		t.Fatalf("CreateChart(own): %v", err)
	}

	repaired, projects, err := s.repairSeededCharts(ctx)
	if err != nil {
		t.Fatalf("repairSeededCharts: %v", err)
	}
	// Other projects on a shared test database may hold the same seeded rows, so
	// the total is a lower bound — the point is that the scope is reported at all.
	if repaired < int64(len(cases)) || projects < 1 {
		t.Errorf("repair reported %d chart(s) in %d project(s), want at least this test's %d", repaired, projects, len(cases))
	}

	for i, c := range cases {
		got, err := s.ChartForProject(ctx, project.ID, seeded[i].ID)
		if err != nil {
			t.Fatalf("ChartForProject(%s): %v", c.name, err)
		}
		if got.SQL != c.want {
			t.Errorf("%s chart kept the stale query:\n got %s\nwant %s", c.name, got.SQL, c.want)
		}
		if got.Revision != seeded[i].Revision+1 {
			t.Errorf("%s chart revision = %d, want %d — a content change must fence a stale edit", c.name, got.Revision, seeded[i].Revision+1)
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
