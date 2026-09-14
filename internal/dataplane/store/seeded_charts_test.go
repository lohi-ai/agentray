package storage

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
)

// The seeded "Visitors: guest vs identified" chart classifies by identity
// linkage, not by a property. Nothing in these events carries an email or a
// name, so the property-based query this replaced would report both visitors as
// "Guest" — the failure mode is the second half of this test.
func TestSeededGuestVsIdentifiedChartSplitsByIdentityLinkage(t *testing.T) {
	d := openTestDuckDB(t)
	ctx := context.Background()
	project := uuid.NewString()
	start := time.Now().UTC().Add(-time.Hour)

	pageview := func(distinctID string, at time.Time) Event {
		e := duckEvent(project, uuid.NewString(), distinctID, at)
		e.EventName = "user.pageview"
		e.Properties = `{}`
		return e
	}
	identify := func(distinctID string, at time.Time) Event {
		e := pageview(distinctID, at)
		e.EventName = "$identify"
		return e
	}
	// One reader browses anonymously and then logs in (the SDK's identify()
	// writes the alias and the $identify event); a second reader never signs in;
	// a third identified but never produced a pageview.
	if err := d.InsertEvents(ctx, []Event{
		pageview("anon-1", start),
		identify("user-1", start.Add(time.Minute)),
		pageview("user-1", start.Add(2*time.Minute)),
		pageview("never-1", start.Add(3*time.Minute)),
		identify("user-2", start.Add(4*time.Minute)),
	}); err != nil {
		t.Fatalf("InsertEvents: %v", err)
	}
	if err := d.UpsertAliases(ctx, [][3]string{{project, "anon-1", "user-1"}}); err != nil {
		t.Fatalf("UpsertAliases: %v", err)
	}

	s := &Store{duck: d, sandboxes: newSQLSandboxPool(d)}
	t.Cleanup(s.sandboxes.closeAll)

	rows, err := s.RunSQL(ctx, project, seededChartSQL(t, "Visitors: guest vs identified"))
	if err != nil {
		t.Fatalf("RunSQL: %v", err)
	}
	visitors := map[string]string{}
	for _, row := range rows {
		visitors[fmt.Sprint(row["user_type"])] = fmt.Sprint(row["visitors"])
	}
	if len(rows) != 2 || visitors["Identified"] != "1" || visitors["Guest"] != "1" {
		t.Fatalf("seeded chart classified %v, want exactly one Identified and one Guest visitor", rows)
	}
}

// seededChartSQL returns the query the starter board seeds under name: the
// string the board actually runs, not a copy of it.
func seededChartSQL(t *testing.T, name string) string {
	t.Helper()
	for _, chart := range starterCharts() {
		if chart.Name == name {
			return chart.SQL
		}
	}
	t.Fatalf("the starter board seeds no chart named %q", name)
	return ""
}
