package storage

import (
	"strings"
	"testing"
	"time"
)

// A funnel that reports more people at step 2 than at step 1 is the bug this
// rollup exists to make impossible. The old implementation counted each step
// independently, so subscription_started(2) -> subscription_activated(4)
// rendered 200% conversion. windowFunnel hands back per-user depths instead, and
// summing the tail makes the series monotone by construction.
func TestFunnelStepsFromDepthsCannotExceedTheFirstStep(t *testing.T) {
	steps := []string{"subscription_started", "subscription_activated"}
	// 2 users reached step 1 only, 2 users reached both. The 2 people who did
	// step 2 without ever doing step 1 are not in the funnel at all.
	depths := []uint64{0, 2, 2}

	out := funnelStepsFromDepths(steps, depths)

	if len(out) != 2 {
		t.Fatalf("want 2 steps, got %d", len(out))
	}
	if out[0].Users != 4 {
		t.Errorf("step 1 users = %d, want 4 (everyone at depth >= 1)", out[0].Users)
	}
	if out[1].Users != 2 {
		t.Errorf("step 2 users = %d, want 2 (only those at depth >= 2)", out[1].Users)
	}
	if out[1].Conversion != 0.5 {
		t.Errorf("step 2 conversion = %v, want 0.5", out[1].Conversion)
	}
}

func TestFunnelStepsFromDepthsAreMonotoneAndBounded(t *testing.T) {
	cases := []struct {
		name   string
		steps  []string
		depths []uint64
	}{
		{"perfectly nested", []string{"a", "b", "c"}, []uint64{0, 61, 18, 11}},
		{"everyone drops at step 1", []string{"a", "b"}, []uint64{0, 90, 0}},
		{"everyone completes", []string{"a", "b", "c"}, []uint64{0, 0, 0, 40}},
		{"nobody enters", []string{"a", "b"}, []uint64{0, 0, 0}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := funnelStepsFromDepths(tc.steps, tc.depths)
			for i, step := range out {
				if step.Step != i+1 {
					t.Errorf("step %d numbered %d; the API contract is 1-based", i, step.Step)
				}
				if step.Conversion > 1.0 {
					t.Errorf("step %d conversion %v exceeds 100%%", step.Step, step.Conversion)
				}
				if i > 0 && step.Users > out[i-1].Users {
					t.Errorf("step %d has %d users, more than step %d's %d", step.Step, step.Users, i, out[i-1].Users)
				}
			}
		})
	}
}

// ClickHouse binds ? by position, so a mismatch between the argument slice and
// the order the placeholders appear in the SQL text does not error — it funnels
// the wrong events. This pins the correspondence.
func TestBuildFunnelQueryBindsArgumentsInSQLTextOrder(t *testing.T) {
	steps := []string{"signup", "checkout", "paid"}
	where := "project_id = ? AND timestamp >= ? AND timestamp <= ?"
	whereArgs := []any{"proj-1", "from", "to"}

	query, args := buildFunnelQuery(steps, 3600, where, whereArgs, "canonical(distinct_id)", nil)

	if got := strings.Count(query, "?"); got != len(args) {
		t.Fatalf("query has %d placeholders but %d args\n%s", got, len(args), query)
	}
	want := []any{
		// windowFunnel conditions, in the SELECT
		"signup", "checkout", "paid",
		// the WHERE
		"proj-1", "from", "to",
		// the event_name IN list
		"signup", "checkout", "paid",
	}
	if len(args) != len(want) {
		t.Fatalf("got %d args, want %d: %v", len(args), len(want), args)
	}
	for i := range want {
		if args[i] != want[i] {
			t.Errorf("arg %d = %v, want %v", i, args[i], want[i])
		}
	}
	if !strings.Contains(query, "windowFunnel(3600)(toDateTime(timestamp)") {
		t.Errorf("window seconds or the DateTime64 cast is missing:\n%s", query)
	}
	if !strings.Contains(query, "GROUP BY canonical(distinct_id)") {
		t.Errorf("funnel must group by the stitched identity, not the raw distinct_id:\n%s", query)
	}
}

func TestBuildFunnelQueryAppendsCanonicalArgsLast(t *testing.T) {
	_, args := buildFunnelQuery([]string{"a"}, 60, "project_id = ?", []any{"p"}, "dictGet(?, distinct_id)", []any{"dict"})
	if got, want := args[len(args)-1], any("dict"); got != want {
		t.Errorf("last arg = %v, want %v — the GROUP BY placeholder is last in the SQL text", got, want)
	}
}

// The window is the analysis range itself: a 30-day funnel asks who converted
// during those 30 days. A stricter window would silently drop real conversions.
func TestFunnelWindowSecondsTracksTheAnalysisRange(t *testing.T) {
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		f    EventFilter
		want int64
	}{
		{"unset falls back to 24h", EventFilter{}, 86400},
		{"30 day range", EventFilter{From: now.AddDate(0, 0, -30), To: now}, 30 * 86400},
		{"missing To falls back", EventFilter{From: now.AddDate(0, 0, -30)}, 86400},
		{"inverted range falls back", EventFilter{From: now, To: now.AddDate(0, 0, -1)}, 86400},
		{"zero-length range falls back", EventFilter{From: now, To: now}, 86400},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := funnelWindowSeconds(tc.f); got != tc.want {
				t.Errorf("got %d, want %d", got, tc.want)
			}
		})
	}
}
