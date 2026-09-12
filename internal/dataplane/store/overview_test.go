package storage

import (
	"testing"
	"time"
)

// overview_test.go locks the metric-semantics contract: range boundaries,
// state mapping, and per-cohort-day retention maturity. The live fixture test
// (overview_live_test.go) proves the queries against real ClickHouse.

func TestOverviewRangeCompleteDays(t *testing.T) {
	// 2026-09-12 14:30 UTC: "7d" must end at Sep-12 00:00, covering Sep 5–11.
	now := time.Date(2026, 9, 12, 14, 30, 0, 0, time.UTC)
	r, err := overviewRange("7d", now)
	if err != nil {
		t.Fatalf("7d: %v", err)
	}
	if !r.CompleteDays || r.Days != 7 {
		t.Fatalf("7d: %+v", r)
	}
	if !r.To.Equal(time.Date(2026, 9, 12, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("to = %v, want Sep-12 midnight", r.To)
	}
	if !r.From.Equal(time.Date(2026, 9, 5, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("from = %v, want Sep-5 midnight", r.From)
	}
}

func TestOverviewRangeDefaultAndToday(t *testing.T) {
	now := time.Date(2026, 9, 12, 14, 30, 0, 0, time.UTC)
	r, err := overviewRange("", now)
	if err != nil || r.Days != 7 || !r.CompleteDays {
		t.Fatalf("empty period must default to 7d complete: %+v err=%v", r, err)
	}
	today, err := overviewRange("today", now)
	if err != nil {
		t.Fatalf("today: %v", err)
	}
	if today.CompleteDays || !today.To.Equal(now) {
		t.Fatalf("today must be partial ending at now: %+v", today)
	}
	if !today.From.Equal(time.Date(2026, 9, 12, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("today from = %v, want midnight", today.From)
	}
}

func TestOverviewRangeRejectsBadPeriod(t *testing.T) {
	now := time.Date(2026, 9, 12, 0, 0, 0, 0, time.UTC)
	for _, p := range []string{"0d", "91d", "7", "week", "-3d", "7D", "100d"} {
		if _, err := overviewRange(p, now); err == nil {
			t.Fatalf("period %q must be rejected", p)
		}
	}
	if _, err := overviewRange("90d", now); err != nil {
		t.Fatalf("90d must be accepted: %v", err)
	}
}

func TestOverviewDataState(t *testing.T) {
	if got := overviewDataState(false, 0); got != "no_events" {
		t.Fatalf("never received = %q", got)
	}
	if got := overviewDataState(true, time.Hour); got != "fresh" {
		t.Fatalf("1h old = %q", got)
	}
	if got := overviewDataState(true, 25*time.Hour); got != "quiet" {
		t.Fatalf("25h old = %q", got)
	}
}

func TestOverviewSourceState(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name                string
		configured, enabled bool
		lastStatus          string
		lastRows            int
		want                string
	}{
		{name: "connector without table", want: "not_configured"},
		{name: "paused sync", configured: true, want: "paused"},
		{name: "never run", configured: true, enabled: true, want: "not_ready"},
		{name: "successful run", configured: true, enabled: true, lastStatus: "ok", want: "healthy"},
		{name: "failed run", configured: true, enabled: true, lastStatus: "error", want: "error"},
		{name: "partial failed run", configured: true, enabled: true, lastStatus: "error", lastRows: 3, want: "partial"},
	}
	for _, tc := range cases {
		var lastRunAt *time.Time
		if tc.configured && tc.enabled && tc.name != "never run" {
			lastRunAt = &now
		}
		if got := overviewSourceState(tc.configured, tc.enabled, lastRunAt, tc.lastStatus, tc.lastRows); got != tc.want {
			t.Fatalf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestOverviewMetricStateKeysOffQualifying(t *testing.T) {
	// The receipt-only case: events arrived but none qualify — metrics must
	// report no_data so the UI shows "integrated, no qualifying activity",
	// never a populated chart built on the excluded verification event.
	if got := overviewMetricState(true, 0); got != OverviewStateNoData {
		t.Fatalf("events but none qualifying = %q, want no_data", got)
	}
	if got := overviewMetricState(true, 5); got != OverviewStateOK {
		t.Fatalf("qualifying events = %q, want ok", got)
	}
	if got := overviewMetricState(false, 0); got != OverviewStateNoData {
		t.Fatalf("never received = %q, want no_data", got)
	}
}

func TestOverviewRetentionPointMaturity(t *testing.T) {
	// Zero eligible is "not_ready" — never a 0% rate.
	p := overviewRetentionPoint(0, 0)
	if p.State != OverviewStateNotReady {
		t.Fatalf("zero eligible = %+v, want not_ready", p)
	}
	// A measured 0% with eligible members is a real rate, not not_ready.
	p = overviewRetentionPoint(10, 0)
	if p.State != OverviewStateOK || p.Rate != 0 {
		t.Fatalf("0%% of 10 eligible = %+v, want ok rate 0", p)
	}
	p = overviewRetentionPoint(10, 4)
	if p.State != OverviewStateOK || p.Rate != 0.4 {
		t.Fatalf("4/10 = %+v, want ok rate 0.4", p)
	}
}

func TestOverviewPlatformClauseBound(t *testing.T) {
	// The platform value must always be a bound arg — never inlined into SQL.
	clause, arg := overviewPlatform("ios")
	if clause == "" || arg != "ios" {
		t.Fatalf("ios: clause=%q arg=%v", clause, arg)
	}
	clause, arg = overviewPlatform("")
	if clause != "" || arg != nil {
		t.Fatalf("empty platform must add no clause: %q %v", clause, arg)
	}
	clause, arg = overviewPlatform(PlatformUnknown)
	if arg != nil || clause == "" {
		t.Fatalf("unknown platform must match empty column without a bind: %q %v", clause, arg)
	}
}

func TestFirstPlatformClause(t *testing.T) {
	// New-user eligibility keys off the platform of the FIRST event, so a
	// known web user's first iOS event is not a new iOS user.
	if got := firstPlatformClause("ios"); got != " AND first_platform = ?" {
		t.Fatalf("ios: %q", got)
	}
	if got := firstPlatformClause(""); got != "" {
		t.Fatalf("empty: %q", got)
	}
	if got := firstPlatformClause(PlatformUnknown); got != " AND first_platform = ''" {
		t.Fatalf("unknown: %q", got)
	}
}
