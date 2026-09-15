package storage

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"
	"time"
)

// metric_targets_test.go — the target contract: what a declaration must look
// like, how the append-only history versions and dedups, which version a read
// window cites, and what verdict a measured value earns. Live cases need the
// compose Postgres; they skip without one (same convention as boards_test.go).

func TestMetricTargetSpecValidation(t *testing.T) {
	value, _ := MetricCatalogEntry(MetricActiveUsers)
	percent, _ := MetricCatalogEntry(MetricRetentionD7)
	money, _ := MetricCatalogEntry(MetricRevenue)
	series, _ := MetricCatalogEntry(MetricActiveUsersDaily)
	breakdown, _ := MetricCatalogEntry(MetricTopPages)

	cases := []struct {
		name string
		def  MetricDefinition
		spec MetricTargetSpec
		want string // substring of the refusal; empty means accept
	}{
		{"count target", value, MetricTargetSpec{Direction: "gte", Value: 500, Period: "7d"}, ""},
		{"percent target", percent, MetricTargetSpec{Direction: "gte", Value: 40, Period: "7d"}, ""},
		{"money target names its currency", money, MetricTargetSpec{Direction: "gte", Value: 50000, Period: "30d", Currency: "vnd"}, ""},
		{"direction", value, MetricTargetSpec{Direction: "above", Value: 5, Period: "7d"}, "direction"},
		{"negative value", value, MetricTargetSpec{Direction: "gte", Value: -1, Period: "7d"}, "non-negative"},
		{"NaN value", value, MetricTargetSpec{Direction: "gte", Value: nan(), Period: "7d"}, "finite"},
		{"percent over 100", percent, MetricTargetSpec{Direction: "gte", Value: 140, Period: "7d"}, "exceeds 100"},
		{"empty period", value, MetricTargetSpec{Direction: "gte", Value: 5, Period: ""}, "not a target period"},
		{"today is partial", value, MetricTargetSpec{Direction: "gte", Value: 5, Period: "today"}, "partial"},
		{"bad period", value, MetricTargetSpec{Direction: "gte", Value: 5, Period: "weekly"}, "period"},
		{"period over max", value, MetricTargetSpec{Direction: "gte", Value: 5, Period: "91d"}, "period"},
		{"money without currency", money, MetricTargetSpec{Direction: "gte", Value: 50000, Period: "30d"}, "currency"},
		{"currency on a count", value, MetricTargetSpec{Direction: "gte", Value: 5, Period: "7d", Currency: "USD"}, "not currency"},
		{"series metric", series, MetricTargetSpec{Direction: "gte", Value: 5, Period: "7d"}, "single-number"},
		{"breakdown metric", breakdown, MetricTargetSpec{Direction: "gte", Value: 5, Period: "7d"}, "single-number"},
		{"bad effective_at", value, MetricTargetSpec{Direction: "gte", Value: 5, Period: "7d", EffectiveAt: "next week"}, "RFC3339"},
	}
	for _, tc := range cases {
		_, err := normalizeMetricTargetSpec(tc.def, tc.spec)
		if tc.want == "" {
			if err != nil {
				t.Errorf("%s: refused a valid spec: %v", tc.name, err)
			}
			continue
		}
		if err == nil {
			t.Errorf("%s: accepted spec %+v", tc.name, tc.spec)
		} else if !errors.Is(err, ErrMetricTargetInvalid) || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want ErrMetricTargetInvalid naming %q", tc.name, err, tc.want)
		}
	}
}

func nan() float64 { return math.NaN() }

func TestMetricTargetVerdicts(t *testing.T) {
	week := OverviewRange{Days: 7, CompleteDays: true}
	partial := OverviewRange{Days: 1, CompleteDays: false}
	target := MetricTarget{Version: 2, Direction: TargetDirectionGTE, Value: 500, PeriodDays: 7}

	cases := []struct {
		name        string
		target      MetricTarget
		unit        string
		r           OverviewRange
		measured    float64
		currency    string
		wantVerdict string
		wantReason  string
	}{
		{"met", target, "people", week, 500, "", TargetVerdictOnTrack, ""},
		{"exceeded", target, "people", week, 900, "", TargetVerdictOnTrack, ""},
		{"inside the band", target, "people", week, 460, "", TargetVerdictAtRisk, ""},
		{"band edge", target, "people", week, 450, "", TargetVerdictAtRisk, ""},
		{"below the band", target, "people", week, 449, "", TargetVerdictOffTrack, ""},
		{"lte met", MetricTarget{Version: 1, Direction: TargetDirectionLTE, Value: 100, PeriodDays: 7}, "people", week, 80, "", TargetVerdictOnTrack, ""},
		{"lte inside band", MetricTarget{Version: 1, Direction: TargetDirectionLTE, Value: 100, PeriodDays: 7}, "people", week, 105, "", TargetVerdictAtRisk, ""},
		{"lte off", MetricTarget{Version: 1, Direction: TargetDirectionLTE, Value: 100, PeriodDays: 7}, "people", week, 200, "", TargetVerdictOffTrack, ""},
		{"partial window", target, "people", partial, 900, "", "", TargetReasonPartialWindow},
		{"period mismatch", target, "people", OverviewRange{Days: 30, CompleteDays: true}, 900, "", "", TargetReasonPeriodMismatch},
		{"currency match", MetricTarget{Version: 1, Direction: TargetDirectionGTE, Value: 50000, PeriodDays: 7, Currency: "VND"}, "currency", week, 80000, "VND", TargetVerdictOnTrack, ""},
		{"currency mismatch", MetricTarget{Version: 1, Direction: TargetDirectionGTE, Value: 50000, PeriodDays: 7, Currency: "USD"}, "currency", week, 80000, "VND", "", TargetReasonCurrencyMismatch},
		{"negative net judged signed", MetricTarget{Version: 1, Direction: TargetDirectionGTE, Value: 0, PeriodDays: 7, Currency: "VND"}, "currency", week, -5000, "VND", TargetVerdictOffTrack, ""},
	}
	for _, tc := range cases {
		got := judgeMetricTarget(tc.target, tc.unit, tc.r, tc.measured, tc.currency)
		if got.Verdict != tc.wantVerdict || got.VerdictReason != tc.wantReason {
			t.Errorf("%s: verdict = %q reason = %q, want %q / %q", tc.name, got.Verdict, got.VerdictReason, tc.wantVerdict, tc.wantReason)
		}
		if got.Version != tc.target.Version {
			t.Errorf("%s: version = %d, want %d", tc.name, got.Version, tc.target.Version)
		}
	}
}

func TestMetricTargetLabel(t *testing.T) {
	cases := []struct {
		target MetricTarget
		unit   string
		want   string
	}{
		{MetricTarget{Direction: "gte", Value: 40, PeriodDays: 7}, "percent", "≥ 40% weekly"},
		{MetricTarget{Direction: "lte", Value: 500, PeriodDays: 1}, "people", "≤ 500 daily"},
		{MetricTarget{Direction: "gte", Value: 50000, PeriodDays: 30, Currency: "VND"}, "currency", "≥ 50,000 VND monthly"},
		{MetricTarget{Direction: "gte", Value: 12.5, PeriodDays: 14}, "percent", "≥ 12.5% per 14d"},
	}
	for _, tc := range cases {
		if got := metricTargetLabel(tc.target, tc.unit); got != tc.want {
			t.Errorf("label = %q, want %q", got, tc.want)
		}
	}
}

func TestMetricTargetHistoryIsAppendOnly(t *testing.T) {
	s := openConvTestStore(t)
	ctx := context.Background()
	_, projectID := seedConvProject(t, s)

	write := MetricTargetWrite{Metric: MetricActiveUsers, Spec: MetricTargetSpec{Direction: "gte", Value: 500, Period: "7d"}}
	first, err := s.SetMetricTargetIdempotent(ctx, projectID, write, "t1", "h1")
	if err != nil {
		t.Fatalf("set: %v", err)
	}
	if first.Version != 1 || first.Cleared || first.PeriodDays != 7 {
		t.Fatalf("first version = %+v", first)
	}

	// The same declaration restates the latest version instead of appending —
	// this is what keeps an identical board save from growing the history.
	again, err := s.SetMetricTargetIdempotent(ctx, projectID, write, "", "")
	if err != nil {
		t.Fatalf("restated set: %v", err)
	}
	if again.Version != 1 || again.ID != first.ID {
		t.Fatalf("identical write appended a version: %+v", again)
	}

	// The dedup compares the normalized spec: a padded or differently-cased
	// declaration of the same target still restates v1.
	padded := write
	padded.Spec.Direction = " GTE "
	padded.Spec.Period = " 7d "
	deduped, err := s.SetMetricTargetIdempotent(ctx, projectID, padded, "", "")
	if err != nil {
		t.Fatalf("padded set: %v", err)
	}
	if deduped.Version != 1 {
		t.Fatalf("unnormalized-but-identical write appended v%d", deduped.Version)
	}

	// A changed spec appends v2; the history keeps v1.
	write.Spec.Value = 800
	second, err := s.SetMetricTargetIdempotent(ctx, projectID, write, "", "")
	if err != nil {
		t.Fatalf("changed set: %v", err)
	}
	if second.Version != 2 || second.Value != 800 {
		t.Fatalf("second version = %+v", second)
	}

	// Clearing is a version too — a tombstone, not a delete.
	cleared, err := s.SetMetricTargetIdempotent(ctx, projectID, MetricTargetWrite{Metric: MetricActiveUsers, Clear: true}, "", "")
	if err != nil {
		t.Fatalf("clear: %v", err)
	}
	if cleared.Version != 3 || !cleared.Cleared {
		t.Fatalf("cleared version = %+v", cleared)
	}

	// Idempotent replay returns the first receipt; a reused key with a
	// different payload conflicts.
	replay, err := s.SetMetricTargetIdempotent(ctx, projectID, MetricTargetWrite{Metric: MetricSessions, Spec: MetricTargetSpec{Direction: "lte", Value: 100, Period: "1d"}}, "k1", "h1")
	if err != nil {
		t.Fatalf("keyed set: %v", err)
	}
	replayed, err := s.SetMetricTargetIdempotent(ctx, projectID, MetricTargetWrite{Metric: MetricSessions, Spec: MetricTargetSpec{Direction: "lte", Value: 100, Period: "1d"}}, "k1", "h1")
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if replayed.ID != replay.ID {
		t.Fatalf("replay returned a different row: %+v vs %+v", replayed, replay)
	}
	if _, err := s.SetMetricTargetIdempotent(ctx, projectID, MetricTargetWrite{Metric: MetricSessions, Spec: MetricTargetSpec{Direction: "lte", Value: 200, Period: "1d"}}, "k1", "h2"); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("key reuse = %v, want ErrIdempotencyConflict", err)
	}

	// An unknown metric is refused with the catalog's vocabulary.
	if _, err := s.SetMetricTargetIdempotent(ctx, projectID, MetricTargetWrite{Metric: "crashes", Spec: MetricTargetSpec{Direction: "gte", Value: 1, Period: "7d"}}, "", ""); !errors.Is(err, ErrMetricTargetInvalid) || !strings.Contains(err.Error(), "not in the catalog") {
		t.Fatalf("unknown metric = %v, want a catalog refusal", err)
	}
}

func TestMetricTargetInForceResolvesByWindowEnd(t *testing.T) {
	s := openConvTestStore(t)
	ctx := context.Background()
	_, projectID := seedConvProject(t, s)

	now := time.Now().UTC()
	weekAgo := now.Add(-7 * 24 * time.Hour).Format(time.RFC3339)
	tomorrow := now.Add(24 * time.Hour).Format(time.RFC3339)

	// v1 effective a week ago; v2 (a raise) effective tomorrow — a window that
	// ends today must still cite v1.
	if _, err := s.SetMetricTargetIdempotent(ctx, projectID, MetricTargetWrite{
		Metric: MetricNewUsers,
		Spec:   MetricTargetSpec{Direction: "gte", Value: 100, Period: "7d", EffectiveAt: weekAgo},
	}, "", ""); err != nil {
		t.Fatalf("v1: %v", err)
	}
	if _, err := s.SetMetricTargetIdempotent(ctx, projectID, MetricTargetWrite{
		Metric: MetricNewUsers,
		Spec:   MetricTargetSpec{Direction: "gte", Value: 900, Period: "7d", EffectiveAt: tomorrow},
	}, "", ""); err != nil {
		t.Fatalf("v2: %v", err)
	}

	inForce, err := metricTargetsInForce(ctx, s.pg, projectID, now)
	if err != nil {
		t.Fatalf("in force: %v", err)
	}
	got := inForce[MetricNewUsers]
	if got.Version != 1 || got.Value != 100 {
		t.Fatalf("window ending now cites v%d value %v, want v1 = 100", got.Version, got.Value)
	}

	// A window ending after v2 takes force cites v2.
	future, err := metricTargetsInForce(ctx, s.pg, projectID, now.Add(48*time.Hour))
	if err != nil {
		t.Fatalf("future in force: %v", err)
	}
	if got := future[MetricNewUsers]; got.Version != 2 || got.Value != 900 {
		t.Fatalf("later window cites v%d value %v, want v2 = 900", got.Version, got.Value)
	}

	// A cleared version in force means no target.
	if _, err := s.SetMetricTargetIdempotent(ctx, projectID, MetricTargetWrite{Metric: MetricNewUsers, Clear: true}, "", ""); err != nil {
		t.Fatalf("clear: %v", err)
	}
	cleared, err := metricTargetsInForce(ctx, s.pg, projectID, now.Add(96*time.Hour))
	if err != nil {
		t.Fatalf("cleared in force: %v", err)
	}
	if got := cleared[MetricNewUsers]; !got.Cleared || got.Version != 3 {
		t.Fatalf("cleared window cites %+v, want the v3 tombstone", got)
	}
}

func TestBoardDeclarationAppliesTargets(t *testing.T) {
	s := openConvTestStore(t)
	ctx := context.Background()
	_, projectID := seedConvProject(t, s)

	name := "Targeted board"
	def := twoTileBoard()
	def.Sections[0].Tiles[0].Target = &MetricTargetSpec{Direction: "gte", Value: 500, Period: "7d"}
	content, err := s.SaveBoardDefinition(ctx, projectID, BoardDefinitionWrite{
		BoardKey: "targeted", Name: &name, Definition: def,
	}, "", "")
	if err != nil {
		t.Fatalf("declare: %v", err)
	}
	// The served content names the version the declaration wrote.
	got, ok := content.Targets[MetricActiveUsers]
	if !ok || got.Version != 1 || got.Value != 500 || got.Cleared {
		t.Fatalf("served targets = %+v", content.Targets)
	}

	// Re-saving the identical document appends no second version.
	redeclared, err := s.SaveBoardDefinition(ctx, projectID, BoardDefinitionWrite{
		BoardID: content.Board.ID, Definition: def, ExpectedRevision: content.Board.Revision,
	}, "", "")
	if err != nil {
		t.Fatalf("redeclare: %v", err)
	}
	if got := redeclared.Targets[MetricActiveUsers]; got.Version != 1 {
		t.Fatalf("identical save appended target v%d", got.Version)
	}

	// A changed target in the document appends v2.
	def.Sections[0].Tiles[0].Target.Value = 800
	raised, err := s.SaveBoardDefinition(ctx, projectID, BoardDefinitionWrite{
		BoardID: content.Board.ID, Definition: def, ExpectedRevision: redeclared.Board.Revision,
	}, "", "")
	if err != nil {
		t.Fatalf("raise: %v", err)
	}
	if got := raised.Targets[MetricActiveUsers]; got.Version != 2 || got.Value != 800 {
		t.Fatalf("raised target = %+v, want v2 = 800", got)
	}

	// A document that drops the target field leaves the history alone — a
	// board never clears by omission.
	def.Sections[0].Tiles[0].Target = nil
	dropped, err := s.SaveBoardDefinition(ctx, projectID, BoardDefinitionWrite{
		BoardID: content.Board.ID, Definition: def, ExpectedRevision: raised.Board.Revision,
	}, "", "")
	if err != nil {
		t.Fatalf("drop target field: %v", err)
	}
	if got := dropped.Targets[MetricActiveUsers]; got.Version != 2 || got.Cleared {
		t.Fatalf("omitted target cleared the history: %+v", got)
	}

	// Two tiles in one document cannot disagree about one metric's target.
	conflict := twoTileBoard()
	conflict.Sections[0].Tiles[0].Target = &MetricTargetSpec{Direction: "gte", Value: 1, Period: "7d"}
	conflict.Sections[0].Tiles = append(conflict.Sections[0].Tiles, BoardTile{
		Key: "people-again", Kind: TileKindMetric, Metric: MetricActiveUsers, Display: DisplayStat,
		Target: &MetricTargetSpec{Direction: "gte", Value: 2, Period: "7d"},
	})
	if _, err := s.SaveBoardDefinition(ctx, projectID, BoardDefinitionWrite{
		BoardKey: "conflicting", Definition: conflict,
	}, "", ""); !errors.Is(err, ErrBoardDefinitionInvalid) || !strings.Contains(err.Error(), "two different targets") {
		t.Fatalf("conflicting targets = %v, want a declaration refusal", err)
	}
}
