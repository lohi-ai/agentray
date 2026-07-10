package alerting

import (
	"testing"
	"time"

	"github.com/lohi-ai/agentray/internal/storage"
)

func TestApplyConditionGtLt(t *testing.T) {
	cases := []struct {
		name   string
		cond   storage.AlertCondition
		series []float64
		fire   bool
	}{
		{"gt fires", storage.AlertCondition{Op: "gt", Value: 10}, []float64{5, 12}, true},
		{"gt quiet", storage.AlertCondition{Op: "gt", Value: 10}, []float64{5, 8}, false},
		{"gt boundary not strict", storage.AlertCondition{Op: "gt", Value: 10}, []float64{5, 10}, false},
		{"lt fires", storage.AlertCondition{Op: "lt", Value: 3}, []float64{5, 1}, true},
		{"lt quiet", storage.AlertCondition{Op: "lt", Value: 3}, []float64{5, 4}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := applyCondition(tc.cond, tc.series)
			if got != tc.fire {
				t.Fatalf("applyCondition(%v, %v) = %v, want %v", tc.cond, tc.series, got, tc.fire)
			}
		})
	}
}

func TestZScoreFires(t *testing.T) {
	// Flat baseline of 100s with one spike to 400 → far beyond 3σ.
	spike := []float64{100, 100, 101, 99, 100, 400}
	cond := storage.AlertCondition{Op: "z_score", Value: 3, MinEvents: 10}
	if fire, _ := applyCondition(cond, spike); !fire {
		t.Fatal("expected z_score to fire on a clear spike")
	}
	// A steady series should not fire.
	steady := []float64{100, 102, 98, 101, 99, 100}
	if fire, _ := applyCondition(cond, steady); fire {
		t.Fatal("z_score fired on a steady series")
	}
	// MinEvents suppresses low-volume noise even with a proportionally large jump.
	lowVol := []float64{0, 1, 0, 0, 1, 5}
	if fire, _ := applyCondition(storage.AlertCondition{Op: "z_score", Value: 3, MinEvents: 100}, lowVol); fire {
		t.Fatal("z_score fired despite MinEvents guard on a low-volume series")
	}
	// A flat baseline (zero variance) must not divide by zero / fire.
	flat := []float64{50, 50, 50, 90}
	if fire, _ := applyCondition(storage.AlertCondition{Op: "z_score", Value: 3, MinEvents: 1}, flat); fire {
		t.Fatal("z_score fired on a zero-variance baseline (div-by-zero path)")
	}
}

func TestExtractSeries(t *testing.T) {
	rows := []map[string]any{
		{"bucket": "2026-01-01T00:00:00Z", "value": uint64(10)},
		{"bucket": "2026-01-01T01:00:00Z", "value": uint64(20)},
	}
	got := extractSeries(rows)
	if len(got) != 2 || got[0] != 10 || got[1] != 20 {
		t.Fatalf("extractSeries = %v, want [10 20]", got)
	}
	// Falls back to the first numeric column when there is no "value".
	rows2 := []map[string]any{{"bucket": "x", "cnt": int64(7)}}
	if got := extractSeries(rows2); len(got) != 1 || got[0] != 7 {
		t.Fatalf("extractSeries fallback = %v, want [7]", got)
	}
}

func TestCronMatches(t *testing.T) {
	at := time.Date(2026, 7, 2, 9, 5, 0, 0, time.UTC) // Thu 09:05
	if !CronMatches("*/5 * * * *", at) {
		t.Fatal("*/5 should match minute 5")
	}
	if CronMatches("*/5 * * * *", at.Add(time.Minute)) {
		t.Fatal("*/5 should not match minute 6")
	}
	if !CronMatches("5 9 * * *", at) {
		t.Fatal("5 9 * * * should match 09:05")
	}
	if CronMatches("bad expr", at) {
		t.Fatal("malformed expr should not match")
	}
}

func TestOpsMetricSQLReadsEventsOnce(t *testing.T) {
	// Each canned metric must reference the events table exactly once so RunSQL's
	// project-scoping accepts it (the scoped_events rewrite requires a single source).
	for name, sql := range opsMetricSQL {
		if got := countOccurrences(sql, "FROM events"); got != 1 {
			t.Fatalf("metric %q references events %d times, want 1", name, got)
		}
	}
}

func countOccurrences(haystack, needle string) int {
	count := 0
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			count++
		}
	}
	return count
}
