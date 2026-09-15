package findings

import (
	"context"
	"testing"
	"time"

	storage "github.com/lohi-ai/agentray/internal/dataplane/store"
)

// fakeStore answers the per-project reads a scan makes from canned fixtures.
// overviews is keyed by the `now` the scanner passes — the current window is
// read at now, the prior window at now-7d. funnels is keyed by the filter's
// To bound for the same reason.
type fakeStore struct {
	overviews map[time.Time]storage.OverviewResult
	funnels   map[time.Time][]storage.FunnelStep
	watches   []storage.FunnelWatch
	recs      []storage.AgentRecommendation
}

func (f *fakeStore) Overview(_ context.Context, _, _, _ string, now time.Time) (storage.OverviewResult, error) {
	if res, ok := f.overviews[now]; ok {
		return res, nil
	}
	return storage.OverviewResult{}, nil
}

func (f *fakeStore) RunInsight(_ context.Context, _, insightType, _ string, _ []string, filter storage.EventFilter) (storage.InsightResult, error) {
	return storage.InsightResult{Type: insightType, Funnel: f.funnels[filter.To]}, nil
}

func (f *fakeStore) FunnelWatchesForProject(_ context.Context, _ string) ([]storage.FunnelWatch, error) {
	return f.watches, nil
}

func (f *fakeStore) CreateRecommendation(_ context.Context, rec storage.AgentRecommendation) (string, error) {
	f.recs = append(f.recs, rec)
	return "rec-" + rec.DedupeKey, nil
}

var testNow = time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)

func okOverview(mut func(*storage.OverviewResult)) storage.OverviewResult {
	res := storage.OverviewResult{
		Context: storage.OverviewContext{
			Timezone:      "UTC",
			MetricVersion: "v-test",
			Range:         storage.OverviewRange{From: testNow.Add(-7 * 24 * time.Hour), To: testNow, Days: 7, CompleteDays: true},
			PreviousRange: storage.OverviewRange{From: testNow.Add(-14 * 24 * time.Hour), To: testNow.Add(-7 * 24 * time.Hour), Days: 7, CompleteDays: true},
		},
		DataStatus: storage.OverviewDataStatus{EverReceived: true, State: "fresh"},
	}
	if mut != nil {
		mut(&res)
	}
	return res
}

func u64(v uint64) *uint64 { return &v }

// A scan over a healthy project with no watches writes nothing — the
// zero-agent, zero-finding baseline the acceptance asks for.
func TestScanHealthyProjectWritesNothing(t *testing.T) {
	st := &fakeStore{overviews: map[time.Time]storage.OverviewResult{
		testNow:                        okOverview(nil),
		testNow.Add(-7 * 24 * time.Hour): okOverview(nil),
	}}
	res, err := ScanProject(context.Background(), st, "p1", testNow)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(res.Findings) != 0 || len(st.recs) != 0 {
		t.Fatalf("healthy project produced findings: %+v", st.recs)
	}
	if len(res.Detectors) != 5 {
		t.Fatalf("detectors = %v, want all five", res.Detectors)
	}
}

func TestWoWDeltaFiresOncePerMetric(t *testing.T) {
	cur := okOverview(func(r *storage.OverviewResult) {
		r.Metrics.ActiveUsers = storage.OverviewMetric{State: storage.OverviewStateOK, Value: u64(40), Previous: u64(200)}
		// Below the count floor — must not fire.
		r.Metrics.NewUsers = storage.OverviewMetric{State: storage.OverviewStateOK, Value: u64(2), Previous: u64(10)}
		// Inside the threshold — must not fire.
		r.Metrics.Sessions = storage.OverviewMetric{State: storage.OverviewStateOK, Value: u64(110), Previous: u64(100)}
	})
	st := &fakeStore{overviews: map[time.Time]storage.OverviewResult{
		testNow:                          cur,
		testNow.Add(-7 * 24 * time.Hour): okOverview(nil),
	}}
	res, err := ScanProject(context.Background(), st, "p1", testNow)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(st.recs) != 1 {
		t.Fatalf("findings = %d, want exactly one (active_users)", len(st.recs))
	}
	rec := st.recs[0]
	if rec.DedupeKey != "wow:active_users" || rec.Source != "engine" {
		t.Fatalf("rec = %+v", rec)
	}
	if len(res.Findings) != 1 {
		t.Fatalf("result findings = %v", res.Findings)
	}
}

func TestOffTrackTargetFires(t *testing.T) {
	cur := okOverview(func(r *storage.OverviewResult) {
		r.Metrics.ActiveUsers = storage.OverviewMetric{
			State: storage.OverviewStateOK, Value: u64(100), Previous: u64(100),
			Target: &storage.MetricTargetView{Version: 2, Label: "≥ 500 people / 7d", Verdict: storage.TargetVerdictOffTrack},
		}
		// at_risk is inside the band — the target working, not a finding.
		r.Metrics.Sessions = storage.OverviewMetric{
			State: storage.OverviewStateOK, Value: u64(460), Previous: u64(460),
			Target: &storage.MetricTargetView{Version: 1, Label: "≥ 450 sessions / 7d", Verdict: storage.TargetVerdictAtRisk},
		}
	})
	st := &fakeStore{overviews: map[time.Time]storage.OverviewResult{
		testNow:                          cur,
		testNow.Add(-7 * 24 * time.Hour): okOverview(nil),
	}}
	if _, err := ScanProject(context.Background(), st, "p1", testNow); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(st.recs) != 1 || st.recs[0].DedupeKey != "target:active_users" {
		t.Fatalf("recs = %+v", st.recs)
	}
}

func TestSourceShiftFiresOnTopChangeAndShareMove(t *testing.T) {
	cur := okOverview(func(r *storage.OverviewResult) {
		r.Content.TopSources = storage.OverviewList{Rows: []storage.PathCount{
			{Value: "organic", Count: 400}, {Value: "social", Count: 350}, {Value: "direct", Count: 250},
		}}
	})
	prior := okOverview(func(r *storage.OverviewResult) {
		r.Content.TopSources = storage.OverviewList{Rows: []storage.PathCount{
			{Value: "direct", Count: 500}, {Value: "organic", Count: 300}, {Value: "social", Count: 200},
		}}
	})
	st := &fakeStore{overviews: map[time.Time]storage.OverviewResult{
		testNow:                          cur,
		testNow.Add(-7 * 24 * time.Hour): prior,
	}}
	if _, err := ScanProject(context.Background(), st, "p1", testNow); err != nil {
		t.Fatalf("scan: %v", err)
	}
	keys := map[string]bool{}
	for _, rec := range st.recs {
		keys[rec.DedupeKey] = true
	}
	// Top channel changed (direct → organic) and social moved +15pp.
	if !keys["source_shift:top"] || !keys["source_shift:social"] {
		t.Fatalf("keys = %v, want source_shift:top and source_shift:social", keys)
	}
}

func TestStaleDataFires(t *testing.T) {
	lastSuccess := testNow.Add(-72 * time.Hour)
	cur := okOverview(func(r *storage.OverviewResult) {
		r.DataStatus.State = "quiet"
		r.DataStatus.AgeSeconds = 30 * 3600
		r.DataStatus.Sources = []storage.OverviewSourceStatus{
			{ConnectorID: "c-err", ConnectorName: "Stripe", ConnectorKind: "stripe", State: "error", LastError: "auth failed"},
			{ConnectorID: "c-stale", ConnectorName: "Postgres", ConnectorKind: "postgres", State: "healthy",
				SyncConfigured: true, Enabled: true, LastSuccessAt: &lastSuccess},
			{ConnectorID: "c-ok", ConnectorName: "Fine", ConnectorKind: "postgres", State: "healthy",
				SyncConfigured: true, Enabled: true, LastSuccessAt: &testNow},
		}
	})
	st := &fakeStore{overviews: map[time.Time]storage.OverviewResult{
		testNow:                          cur,
		testNow.Add(-7 * 24 * time.Hour): okOverview(nil),
	}}
	if _, err := ScanProject(context.Background(), st, "p1", testNow); err != nil {
		t.Fatalf("scan: %v", err)
	}
	keys := map[string]bool{}
	for _, rec := range st.recs {
		keys[rec.DedupeKey] = true
	}
	for _, want := range []string{"stale:capture", "stale:source:c-err", "stale:source:c-stale"} {
		if !keys[want] {
			t.Fatalf("missing %q in %v", want, keys)
		}
	}
	if keys["stale:source:c-ok"] {
		t.Fatalf("healthy source fired: %v", keys)
	}
}

func TestFunnelDropFiresOnWorstStep(t *testing.T) {
	watch := storage.FunnelWatch{ID: "w1", ProjectID: "p1", Name: "signup", Steps: []string{"user.pageview", "user.signup"}}
	curTo := testNow
	priorTo := testNow.Add(-7 * 24 * time.Hour)
	st := &fakeStore{
		overviews: map[time.Time]storage.OverviewResult{
			testNow: okOverview(nil),
			priorTo: okOverview(nil),
		},
		watches: []storage.FunnelWatch{watch},
		funnels: map[time.Time][]storage.FunnelStep{
			curTo: {
				{Step: 1, EventName: "user.pageview", Users: 1000, Conversion: 1},
				{Step: 2, EventName: "user.signup", Users: 100, Conversion: 0.10},
			},
			priorTo: {
				{Step: 1, EventName: "user.pageview", Users: 1000, Conversion: 1},
				{Step: 2, EventName: "user.signup", Users: 300, Conversion: 0.30},
			},
		},
	}
	if _, err := ScanProject(context.Background(), st, "p1", testNow); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(st.recs) != 1 || st.recs[0].DedupeKey != "funnel:w1" {
		t.Fatalf("recs = %+v", st.recs)
	}
}

// A funnel whose conversion holds inside the threshold stays silent.
func TestFunnelDropSilentWhenStable(t *testing.T) {
	watch := storage.FunnelWatch{ID: "w1", ProjectID: "p1", Name: "signup", Steps: []string{"a", "b"}}
	steps := []storage.FunnelStep{
		{Step: 1, EventName: "a", Users: 1000, Conversion: 1},
		{Step: 2, EventName: "b", Users: 300, Conversion: 0.30},
	}
	st := &fakeStore{
		overviews: map[time.Time]storage.OverviewResult{
			testNow:                          okOverview(nil),
			testNow.Add(-7 * 24 * time.Hour): okOverview(nil),
		},
		watches: []storage.FunnelWatch{watch},
		funnels: map[time.Time][]storage.FunnelStep{
			testNow:                          steps,
			testNow.Add(-7 * 24 * time.Hour): steps,
		},
	}
	if _, err := ScanProject(context.Background(), st, "p1", testNow); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(st.recs) != 0 {
		t.Fatalf("stable funnel fired: %+v", st.recs)
	}
}
