package storage

import (
	"errors"
	"testing"
	"time"
)

// metric_catalog_test.go locks the catalog contract: the declaration is
// internally consistent, every declared metric is actually computable by the
// projection, and the projection keeps the honesty rules the overview surface
// promises (no fabricated zeros, no rate smuggled into a count field).

func syntheticOverview() OverviewResult {
	value := func(v uint64, prev *uint64) OverviewMetric {
		return OverviewMetric{State: OverviewStateOK, Value: uint64Ptr(v), Previous: prev}
	}
	prev := uint64(3)
	// The overview read serves return rates as 0–1 fractions; the catalog
	// declares the metric's unit as percent, and the reading is what has to
	// reconcile the two (see the retention case in MetricReadingFor).
	rate := 0.5431
	return OverviewResult{
		Context: OverviewContext{MetricVersion: OverviewMetricVersion, Range: OverviewRange{Days: 7, CompleteDays: true}},
		Metrics: OverviewMetrics{
			ActiveUsers: value(120, &prev),
			NewUsers:    value(9, nil),
			Sessions:    value(200, nil),
			Activation:  OverviewMetric{State: OverviewStateUnconfigured, Definition: metricDefActivation, Notes: []string{metricPrereqActivation}},
			Revenue:     value(4210, nil),
			RevenueDetail: &OverviewRevenueDetail{
				Currency: "VND", Gross: 5000, Reversed: 790, Net: 4210, DedupedRows: 12,
			},
		},
		Trend: []OverviewTrendPoint{{Day: "2026-09-05", ActiveUsers: 10}, {Day: "2026-09-06", ActiveUsers: 0}},
		Retention: OverviewRetention{
			CohortWindow: "lifetime",
			D1:           OverviewRetentionPoint{State: OverviewStateOK, Rate: rate, Returned: 12, Eligible: 22},
			D7:           OverviewRetentionPoint{State: OverviewStateNotReady},
			D30:          OverviewRetentionPoint{State: OverviewStateNotReady},
		},
		Content: OverviewContent{
			TopPages:   OverviewList{Unit: "pageviews", Rows: []PathCount{{Value: "/", Count: 40}}},
			TopSources: OverviewList{Unit: "referrers", Rows: []PathCount{{Value: "unknown", Count: 12}}},
		},
		DataStatus: OverviewDataStatus{EverReceived: true, QualifyingInRange: 42, State: "fresh"},
	}
}

// TestMetricCatalogIsInternallyConsistent fails on a half-declared metric: a key
// that cannot be addressed, a kind nobody can render, a definition that explains
// nothing, or a table count that has drifted from the overview contract.
func TestMetricCatalogIsInternallyConsistent(t *testing.T) {
	catalog := MetricCatalog()
	if len(catalog) == 0 {
		t.Fatal("the catalog is empty")
	}
	seen := map[string]bool{}
	for i, def := range catalog {
		if !boardKeyRe.MatchString(def.Key) {
			t.Errorf("metric %q is not addressable as a key", def.Key)
		}
		if seen[def.Key] {
			t.Errorf("metric %q is declared twice", def.Key)
		}
		seen[def.Key] = true
		if def.SortOrder != i {
			t.Errorf("metric %q sort_order = %d, want %d", def.Key, def.SortOrder, i)
		}
		if def.MetricVersion != OverviewMetricVersion {
			t.Errorf("metric %q version = %q, want the overview contract %q", def.Key, def.MetricVersion, OverviewMetricVersion)
		}
		if def.Label == "" || def.Unit == "" || def.Definition == "" {
			t.Errorf("metric %q must declare a label, a unit and a definition", def.Key)
		}
		switch def.Group {
		case MetricGroupOverview, MetricGroupAcquisition, MetricGroupMonetization, MetricGroupUsage:
		default:
			t.Errorf("metric %q has unknown group %q", def.Key, def.Group)
		}
		if len(def.Displays) == 0 {
			t.Errorf("metric %q declares no display, so no tile could draw it", def.Key)
		}
		for _, display := range def.Displays {
			switch display {
			case DisplayStat:
				if def.Kind != MetricKindValue {
					t.Errorf("metric %q (%s) offers the stat display", def.Key, def.Kind)
				}
			case DisplayLine, DisplayArea:
				if def.Kind != MetricKindSeries {
					t.Errorf("metric %q (%s) offers the %s display", def.Key, def.Kind, display)
				}
			case DisplayBar:
				if def.Kind == MetricKindValue {
					t.Errorf("metric %q (%s) offers the bar display", def.Key, def.Kind)
				}
			case DisplayTable:
				if def.Kind != MetricKindBreakdown {
					t.Errorf("metric %q (%s) offers the table display", def.Key, def.Kind)
				}
			default:
				t.Errorf("metric %q offers unknown display %q", def.Key, display)
			}
		}
		if len(def.Params) == 0 {
			t.Errorf("metric %q declares no params, so a tile cannot say what range it covers", def.Key)
		}
	}
	if entry, ok := MetricCatalogEntry(MetricActiveUsers); !ok || entry.Key != MetricActiveUsers {
		t.Fatalf("MetricCatalogEntry(%q) = %+v, %v", MetricActiveUsers, entry, ok)
	}
}

func TestMetricCatalogDoesNotAliasDeclarationSlices(t *testing.T) {
	a := MetricCatalog()
	a[0].Displays[0] = "mutated"
	b := MetricCatalog()
	if b[0].Displays[0] == "mutated" {
		t.Fatal("MetricCatalog aliases the declaration's Displays slice")
	}
}

// TestEveryCatalogMetricIsComputable is the drift guard that matters: a metric
// added to the catalog without a projection branch would otherwise be served as
// a definition no read can fill.
func TestEveryCatalogMetricIsComputable(t *testing.T) {
	res := syntheticOverview()
	for _, def := range MetricCatalog() {
		reading, err := MetricReadingFor(def, res)
		if err != nil {
			t.Errorf("metric %q has no projection: %v", def.Key, err)
			continue
		}
		if reading.Key != def.Key || reading.Definition != def.Definition || reading.MetricVersion != OverviewMetricVersion {
			t.Errorf("metric %q served as %+v", def.Key, reading)
		}
	}
	if _, err := MetricReadingFor(MetricDefinition{Key: "no_such_metric"}, res); !errors.Is(err, ErrMetricUnknown) {
		t.Fatalf("unknown metric err = %v, want ErrMetricUnknown", err)
	}
}

func TestMetricReadingCarriesValueAndComparison(t *testing.T) {
	res := syntheticOverview()
	def, _ := MetricCatalogEntry(MetricActiveUsers)
	reading, err := MetricReadingFor(def, res)
	if err != nil {
		t.Fatal(err)
	}
	if reading.State != OverviewStateOK || reading.Value == nil || *reading.Value != 120 {
		t.Fatalf("active_users = %+v", reading)
	}
	if reading.Previous == nil || *reading.Previous != 3 {
		t.Fatalf("active_users previous = %v, want 3", reading.Previous)
	}
	if reading.Rate != nil || len(reading.Series) != 0 || len(reading.Rows) != 0 {
		t.Fatalf("a value metric must not carry a rate, series or rows: %+v", reading)
	}
}

// TestUnconfiguredMetricNamesItsPrerequisite: a metric the project has not
// instrumented reports the missing input and carries no number — never 0.
func TestUnconfiguredMetricNamesItsPrerequisite(t *testing.T) {
	res := syntheticOverview()
	def, _ := MetricCatalogEntry(MetricActivation)
	reading, err := MetricReadingFor(def, res)
	if err != nil {
		t.Fatal(err)
	}
	if reading.State != OverviewStateUnconfigured {
		t.Fatalf("activation state = %q, want unconfigured", reading.State)
	}
	if reading.Value != nil || reading.Previous != nil || reading.Rate != nil {
		t.Fatalf("an unconfigured metric carried a number: %+v", reading)
	}
	if reading.Prerequisite == "" || len(reading.Notes) == 0 || reading.Notes[0] != reading.Prerequisite {
		t.Fatalf("the prerequisite must be the note served beside the tile: prereq=%q notes=%v", reading.Prerequisite, reading.Notes)
	}
}

func TestRevenueKeepsSignedDetail(t *testing.T) {
	res := syntheticOverview()
	def, _ := MetricCatalogEntry(MetricRevenue)
	reading, err := MetricReadingFor(def, res)
	if err != nil {
		t.Fatal(err)
	}
	if reading.Revenue == nil || reading.Revenue.Net != 4210 || reading.Revenue.Reversed != 790 {
		t.Fatalf("revenue detail = %+v", reading.Revenue)
	}
	if reading.Value == nil || *reading.Value != 4210 {
		t.Fatalf("revenue value = %v, want the unsigned magnitude", reading.Value)
	}
}

// TestRetentionRateIsNotACount: a mature cohort carries its rate on the percent
// scale its declared unit promises (the overview read's fraction is scaled,
// not copied), and an immature one carries nothing at all.
func TestRetentionRateIsNotACount(t *testing.T) {
	res := syntheticOverview()
	d1, _ := MetricCatalogEntry(MetricRetentionD1)
	reading, err := MetricReadingFor(d1, res)
	if err != nil {
		t.Fatal(err)
	}
	if reading.State != OverviewStateOK || reading.Unit != "percent" {
		t.Fatalf("d1 = %+v", reading)
	}
	if reading.Rate == nil || *reading.Rate != 54.31 {
		t.Fatalf("d1 rate = %v, want 54.31 percent for a 0.5431 fraction", reading.Rate)
	}
	if reading.Value != nil {
		t.Fatalf("a percentage must not be carried in the unsigned count field: %v", *reading.Value)
	}
	d7, _ := MetricCatalogEntry(MetricRetentionD7)
	reading, err = MetricReadingFor(d7, res)
	if err != nil {
		t.Fatal(err)
	}
	if reading.State != OverviewStateNotReady || reading.Rate != nil {
		t.Fatalf("d7 = %+v, want not_ready with no rate", reading)
	}
}

func TestDailySeriesKeepsEveryDay(t *testing.T) {
	res := syntheticOverview()
	def, _ := MetricCatalogEntry(MetricActiveUsersDaily)
	reading, err := MetricReadingFor(def, res)
	if err != nil {
		t.Fatal(err)
	}
	if len(reading.Series) != 2 || reading.Series[0].Label != "2026-09-05" || reading.Series[1].Value != 0 {
		t.Fatalf("series = %+v, want both days including the measured zero", reading.Series)
	}

	// Receipt-only: events arrived, none qualifying. The series is empty and
	// the state says why.
	res.DataStatus.QualifyingInRange = 0
	reading, err = MetricReadingFor(def, res)
	if err != nil {
		t.Fatal(err)
	}
	if reading.State != OverviewStateNoData || len(reading.Series) != 0 {
		t.Fatalf("receipt-only series = %+v", reading)
	}
}

// TestBreakdownServesTheComputedUnit: the unit comes from the list the
// computation produced, so a metric whose rows are counted in referrers can
// never be labelled pageviews.
func TestBreakdownServesTheComputedUnit(t *testing.T) {
	res := syntheticOverview()
	def, _ := MetricCatalogEntry(MetricTopSources)
	reading, err := MetricReadingFor(def, res)
	if err != nil {
		t.Fatal(err)
	}
	if reading.Unit != "referrers" || len(reading.Rows) != 1 || reading.Rows[0].Value != "unknown" {
		t.Fatalf("top_sources = %+v", reading)
	}
	pages, _ := MetricCatalogEntry(MetricTopPages)
	reading, err = MetricReadingFor(pages, res)
	if err != nil {
		t.Fatal(err)
	}
	if reading.Unit != "pageviews" || len(reading.Rows) != 1 {
		t.Fatalf("top_pages = %+v", reading)
	}
}

// TestNoEventsIsNoDataForEveryMetric: a project that has never sent an event
// reports the same named state for every metric, including the ones whose
// computation would otherwise always produce a number.
func TestNoEventsIsNoDataForEveryMetric(t *testing.T) {
	res := syntheticOverview()
	res.DataStatus = OverviewDataStatus{EverReceived: false}
	for _, def := range MetricCatalog() {
		reading, err := MetricReadingFor(def, res)
		if err != nil {
			t.Fatalf("metric %q: %v", def.Key, err)
		}
		if reading.State != OverviewStateNoData {
			t.Errorf("metric %q state = %q, want no_data", def.Key, reading.State)
		}
		if reading.Value != nil || reading.Rate != nil || len(reading.Series) != 0 {
			t.Errorf("metric %q served a number with no events: %+v", def.Key, reading)
		}
	}
}

func TestValidPeriodMatchesTheRangeContract(t *testing.T) {
	for _, ok := range []string{"", "today", "1d", "7d", "90d"} {
		if err := validPeriod(ok); err != nil {
			t.Errorf("validPeriod(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"0d", "91d", "7", "7D", "-1d", "week"} {
		if err := validPeriod(bad); err == nil {
			t.Errorf("validPeriod(%q) accepted an out-of-contract period", bad)
		}
	}
	// The range builder and the tile validator must accept the same strings.
	now := time.Date(2026, 9, 12, 14, 30, 0, 0, time.UTC)
	for _, p := range []string{"", "today", "1d", "7d", "90d"} {
		period := p
		if period == "" {
			period = "7d"
		}
		if _, err := overviewRange(period, now, time.UTC); err != nil {
			t.Errorf("overviewRange(%q) = %v, want nil", period, err)
		}
	}
}
