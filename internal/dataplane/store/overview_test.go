package storage

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// overview_test.go locks the metric-semantics contract: range boundaries,
// state mapping, and per-cohort-day retention maturity. The live fixture test
// (overview_live_test.go) proves the queries against real DuckDB.

func TestOverviewRangeCompleteDays(t *testing.T) {
	// 2026-09-12 14:30 UTC: "7d" must end at Sep-12 00:00, covering Sep 5–11.
	now := time.Date(2026, 9, 12, 14, 30, 0, 0, time.UTC)
	r, err := overviewRange("7d", now, time.UTC)
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

func TestOverviewRangeProjectTimezoneDST(t *testing.T) {
	loc, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Fatal(err)
	}
	// Mar 9 is the first full local day after DST begins. The seven-day range
	// crosses the offset change, so its bounds must be local midnights, not
	// fixed 24-hour subtraction.
	now := time.Date(2026, 3, 9, 10, 30, 0, 0, time.UTC)
	r, err := overviewRange("7d", now, loc)
	if err != nil {
		t.Fatal(err)
	}
	if !r.From.Equal(time.Date(2026, 3, 2, 0, 0, 0, 0, loc)) || !r.To.Equal(time.Date(2026, 3, 9, 0, 0, 0, 0, loc)) {
		t.Fatalf("DST range = %+v, want Mar 2–9 local midnights", r)
	}
	today, err := overviewRange("today", now, loc)
	if err != nil {
		t.Fatal(err)
	}
	if today.CompleteDays || !today.From.Equal(r.To) || !today.To.Equal(now) {
		t.Fatalf("DST today = %+v, want partial range from Mar-9 local midnight", today)
	}
}

func TestOverviewRangeDefaultAndToday(t *testing.T) {
	now := time.Date(2026, 9, 12, 14, 30, 0, 0, time.UTC)
	r, err := overviewRange("", now, time.UTC)
	if err != nil || r.Days != 7 || !r.CompleteDays {
		t.Fatalf("empty period must default to 7d complete: %+v err=%v", r, err)
	}
	today, err := overviewRange("today", now, time.UTC)
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
		if _, err := overviewRange(p, now, time.UTC); err == nil {
			t.Fatalf("period %q must be rejected", p)
		}
	}
	if _, err := overviewRange("90d", now, time.UTC); err != nil {
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

// The ranked acquisition lists count pageviews, but a crawler walking the site
// is not a landing page or a source. They therefore compile the same qualifying
// population every people metric uses — event_type='user' and the human visitor
// class — so Top pages and Top sources can never be two populations under one
// heading.
func TestOverviewAcquisitionCountsHumanUserPageviews(t *testing.T) {
	r, err := overviewRange("7d", time.Date(2026, 9, 12, 0, 0, 0, 0, time.UTC), time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	where, _ := filteredWhereWithDefault("project-1", overviewAcquisitionFilter(r, ""), false)
	if !strings.Contains(where, "event_type = ?") {
		t.Fatalf("acquisition must count user events only: %s", where)
	}
	if !strings.Contains(where, "coalesce(visitor_class, 'human') = 'human'") {
		t.Fatalf("acquisition must exclude crawler traffic: %s", where)
	}
	// The pageview window is half-open like every other Overview range: an
	// inclusive upper bound would pull the next local midnight into the range.
	if !strings.Contains(where, `"timestamp" <= ?`) || !strings.Contains(where, `"timestamp" >= ?`) {
		t.Fatalf("acquisition window must stay bounded: %s", where)
	}
}

func TestOverviewActivationUnconfigured(t *testing.T) {
	s := &Store{}
	m, detail, err := s.overviewActivation(context.Background(), "p1", "", "UTC", time.Now(), "")
	if err != nil {
		t.Fatal(err)
	}
	if m.State != OverviewStateUnconfigured {
		t.Fatalf("state = %q, want unconfigured", m.State)
	}
	if detail != nil {
		t.Fatalf("detail = %+v, want nil", detail)
	}
	if len(m.Notes) == 0 || m.Notes[0] != metricPrereqActivation {
		t.Fatalf("notes = %v, want %q", m.Notes, metricPrereqActivation)
	}
}

// TestOverviewActivationCohortShare proves the activation read against real
// DuckDB: eligibility is a matured 7-day window from first qualifying
// activity, the chosen event counts inside that window only, and a bot firing
// it under a human's id does not activate them.
func TestOverviewActivationCohortShare(t *testing.T) {
	d := openTestDuckDB(t)
	s := &Store{duck: d}
	day := func(d int) time.Time { return time.Date(2026, 9, d, 12, 0, 0, 0, time.UTC) }
	ev := func(person, name string, at time.Time, class string) Event {
		return Event{
			ProjectID: "aaaaaaaa-1111-2222-3333-444444444444", EventID: uuid.NewString(), EventName: name,
			EventType: "user", DistinctID: person, VisitorClass: class, Timestamp: at,
		}
	}
	to := time.Date(2026, 9, 12, 0, 0, 0, 0, time.UTC)
	events := []Event{
		// u1: cohort Sep 1 (mature), activation Sep 3 — inside window.
		ev("u1", "user.pageview", day(1), "human"),
		ev("u1", "onboarding.completed", day(3), "human"),
		// u2: cohort Sep 2 (mature), activation Sep 10 — day 8, outside window.
		ev("u2", "user.pageview", day(2), "human"),
		ev("u2", "onboarding.completed", day(10), "human"),
		// u3: cohort Sep 2 (mature), activation Sep 9 — exactly day 7, inside.
		ev("u3", "user.pageview", day(2), "human"),
		ev("u3", "onboarding.completed", day(9), "human"),
		// u4: cohort Sep 10 — window has not closed by `to`, not eligible.
		ev("u4", "user.pageview", day(10), "human"),
		ev("u4", "onboarding.completed", day(10), "human"),
		// u5: human cohort Sep 1, but the activation event arrived as a bot —
		// a crawler completing onboarding is not an activated person.
		ev("u5", "user.pageview", day(1), "human"),
		ev("u5", "onboarding.completed", day(3), "bot"),
	}
	if err := d.SinkEvents(context.Background(), events, AppliedMark{}); err != nil {
		t.Fatalf("SinkEvents: %v", err)
	}

	metric, detail, err := s.overviewActivation(context.Background(), "aaaaaaaa-1111-2222-3333-444444444444", "", "UTC", to, "onboarding.completed")
	if err != nil {
		t.Fatalf("overviewActivation: %v", err)
	}
	if metric.State != OverviewStateOK {
		t.Fatalf("state = %q, want ok (notes: %v)", metric.State, metric.Notes)
	}
	if detail == nil {
		t.Fatal("activation_detail is nil — the tile cannot render the rate without it")
	}
	if detail.Eligible != 4 {
		t.Fatalf("eligible = %d, want 4 (u4's window has not closed)", detail.Eligible)
	}
	if detail.Activated != 2 {
		t.Fatalf("activated = %d, want 2 (u2 fired on day 8, u5's event was a bot)", detail.Activated)
	}
	if detail.Rate != 0.5 {
		t.Fatalf("rate = %v, want 0.5", detail.Rate)
	}
	if detail.Event != "onboarding.completed" || detail.WindowDays != 7 {
		t.Fatalf("detail = %+v, want event onboarding.completed window 7", detail)
	}
}

// TestOverviewActivationNoMatureCohort: a configured event with only young
// cohorts reports not_ready, never a fabricated 0%.
func TestOverviewActivationNoMatureCohort(t *testing.T) {
	d := openTestDuckDB(t)
	s := &Store{duck: d}
	now := time.Date(2026, 9, 12, 0, 0, 0, 0, time.UTC)
	events := []Event{{
		ProjectID: "bbbbbbbb-1111-2222-3333-444444444444", EventID: uuid.NewString(), EventName: "user.pageview",
		EventType: "user", DistinctID: "fresh", VisitorClass: "human",
		Timestamp: now.Add(-24 * time.Hour),
	}}
	if err := d.SinkEvents(context.Background(), events, AppliedMark{}); err != nil {
		t.Fatalf("SinkEvents: %v", err)
	}
	metric, detail, err := s.overviewActivation(context.Background(), "bbbbbbbb-1111-2222-3333-444444444444", "", "UTC", now, "onboarding.completed")
	if err != nil {
		t.Fatalf("overviewActivation: %v", err)
	}
	if metric.State != OverviewStateNotReady {
		t.Fatalf("state = %q, want not_ready", metric.State)
	}
	if detail == nil || detail.Eligible != 0 {
		t.Fatalf("detail = %+v, want eligible 0", detail)
	}
}
