package storage

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
)

// The seeded "Visitors: guest vs identified" chart classifies by identity
// linkage, not by a property. Nothing here carries an email or a name, so the
// property-based query this replaced would report every visitor as "Guest".
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
	// Three readers, none of whom ever sends a trait:
	//   user-1  browsed anonymously, then identified (alias + $identify);
	//   user-2  was linked to an anonymous id by alias alone;
	//   never-1 never linked to anything.
	// Plus an identify with no pageview, which is not a visitor.
	if err := d.InsertEvents(ctx, []Event{
		pageview("anon-1", start),
		identify("user-1", start.Add(time.Minute)),
		pageview("user-1", start.Add(2*time.Minute)),
		pageview("anon-2", start.Add(3*time.Minute)),
		pageview("never-1", start.Add(4*time.Minute)),
		identify("user-3", start.Add(5*time.Minute)),
	}); err != nil {
		t.Fatalf("InsertEvents: %v", err)
	}
	if err := d.UpsertAliases(ctx, [][3]string{
		{project, "anon-1", "user-1"},
		{project, "anon-2", "user-2"},
	}); err != nil {
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
	if len(rows) != 2 || visitors["Identified"] != "2" || visitors["Guest"] != "1" {
		t.Fatalf("seeded chart classified %v, want two Identified and one Guest visitor", rows)
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
