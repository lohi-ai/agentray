package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
)

// metric_catalog.go — the metric catalog: one declaration of what each metric
// means, what it may be drawn as, and what has to be instrumented before it can
// compute at all.
//
// The declaration below is the single authority; `metric_definitions` rows are
// its projection, refreshed on every boot. Storing them in Postgres rather than
// serving the Go slice is what lets every consumer read one contract — the web
// board renderer, an agent over MCP, a Garden preset, and a plain run_sql query
// against the catalog — and it is what a board declaration is validated
// against, so a tile can only reference a metric this server actually computes.
//
// A metric is added here only when a deterministic implementation exists: the
// catalog is a promise that `read_metric` will return a real number, a real
// empty state, or a named prerequisite — never an invented figure. Purchases,
// subscriptions and crashes are therefore absent: no implementation computes
// them today (see docs/ARCHITECTURE-API.md).

// Metric kinds — the shape of the value the metric computes. A tile's display
// is checked against this, so a single number can never be declared as a table.
const (
	MetricKindValue     = "value"     // one scalar, optionally with a comparison
	MetricKindSeries    = "series"    // points over the selected range
	MetricKindBreakdown = "breakdown" // ranked rows with a declared unit
)

// Displays a tile may declare, by metric kind.
const (
	DisplayStat  = "stat"  // value
	DisplayLine  = "line"  // series
	DisplayBar   = "bar"   // series, breakdown
	DisplayArea  = "area"  // series
	DisplayTable = "table" // breakdown
)

// Metric groups — the section a catalog entry belongs to in a board, and the
// way the overview composes its groups.
const (
	MetricGroupOverview     = "overview"
	MetricGroupAcquisition  = "acquisition"
	MetricGroupMonetization = "monetization"
	MetricGroupUsage        = "usage"
)

// Metric keys. They are stable identifiers: a board tile, a finding and a
// saved chart all name a metric by these strings, so renaming one is a
// migration, not a refactor.
const (
	MetricActiveUsers      = "active_users"
	MetricNewUsers         = "new_users"
	MetricSessions         = "sessions"
	MetricActiveUsersDaily = "active_users_daily"
	MetricActivation       = "activation"
	MetricRevenue          = "revenue"
	MetricRetentionD1      = "retention_d1"
	MetricRetentionD7      = "retention_d7"
	MetricRetentionD30     = "retention_d30"
	MetricTopPages         = "top_pages"
	MetricTopSources       = "top_sources"
)

// ErrMetricUnknown is returned when a caller names a metric the catalog does
// not declare — a board tile, a read, or a query. Distinct from a computation
// failure: the caller can list the catalog and correct itself.
var ErrMetricUnknown = errors.New("metric is not in the catalog")

// MetricDefinition is one catalog entry: the served contract for a metric.
// Definition and Prerequisite are the same strings the overview surface prints
// beside a tile, so the number and its explanation cannot drift apart.
type MetricDefinition struct {
	Key           string   `json:"key"`
	MetricVersion string   `json:"metric_version"`
	Label         string   `json:"label"`
	Unit          string   `json:"unit"`
	Kind          string   `json:"kind"`
	Group         string   `json:"group"`
	Definition    string   `json:"definition"`
	Prerequisite  string   `json:"prerequisite,omitempty"`
	Displays      []string `json:"displays"`
	Params        []string `json:"params"`
	SortOrder     int      `json:"sort_order"`
	IsSystem      bool     `json:"is_system"`
}

// AllowsDisplay reports whether a tile may draw this metric that way.
func (d MetricDefinition) AllowsDisplay(display string) bool {
	for _, allowed := range d.Displays {
		if allowed == display {
			return true
		}
	}
	return false
}

// DefaultDisplay is the display a tile gets when it does not declare one.
func (d MetricDefinition) DefaultDisplay() string {
	if len(d.Displays) == 0 {
		return ""
	}
	return d.Displays[0]
}

// Metric parameter names a tile may declare. Period and Platform are the same
// two knobs the overview read takes, with the same values and the same meaning.
const (
	MetricParamPeriod   = "period"
	MetricParamPlatform = "platform"
)

// Definition texts, declared once and consumed by both the catalog rows and the
// overview read that computes the numbers. They lived inline in overview.go and
// money.go before this catalog existed; keeping two copies is how a tile ends
// up explaining itself with semantics the server no longer implements.
const (
	metricDefActiveUsers = "Distinct people (canonical identity) with qualifying human product activity per complete project-local calendar-day range."
	metricDefNewUsers    = "People whose first-ever observed qualifying activity falls inside the range. First observed, not signup or download."
	metricDefSessions    = "Distinct session ids on qualifying activity; sessions end after 30 minutes of inactivity (server sessionizer)."
	metricDefDailyActive = "Distinct people with qualifying activity per local calendar day inside the selected range. Daily counts are never summed into a period total — a person active on two days is one person, not two."
	metricDefActivation  = "Share of a cohort completing the project's chosen activation event inside a conversion window."
	metricDefRevenue     = "Deduplicated net revenue per declared currency: rows de-duplicate by $insert_id (last write wins, event_id fallback), refunds and revenue_reversed rows net against bookings, and the headline is the currency with the largest deduplicated gross. No FX — currencies are never summed together."
	metricDefRetention   = "Return rate for mature lifetime first-activity cohorts: those who came back on day N over those whose day N had fully elapsed. Cohorts too young to have reached day N are excluded, not counted as zero."
	metricDefTopPages    = "Pageviews of human product activity grouped by path, ranked. Direct and unattributed traffic is not dropped — it is its own row."
	metricDefTopSources  = "Pageviews of human product activity grouped by referrer channel, with a missing channel reported as unknown rather than inferred."
)

// Prerequisite texts — what must be instrumented before the metric can compute.
// A metric with a prerequisite is not broken; it is waiting for a named input,
// and the state it serves says so ("unconfigured") instead of rendering 0.
const (
	metricPrereqActivation = "no activation condition is stored for projects yet — configure it before this metric can compute"
	metricPrereqRevenue    = "requires a trusted, deduplicated server or billing source that sends " + moneyBookingEvent + " events with a declared currency and gross/net basis"
	metricPrereqRetention  = "needs a first-activity cohort whose day N has fully elapsed; recent cohorts report not_ready, never 0%"
)

// metricCatalogDecl is the catalog, in display order. SortOrder is the slice
// index, so the order a reader sees is the order declared here.
var metricCatalogDecl = []MetricDefinition{
	{
		Key: MetricActiveUsers, Label: "Active people", Unit: "people", Kind: MetricKindValue,
		Group: MetricGroupOverview, Definition: metricDefActiveUsers,
		Displays: []string{DisplayStat},
	},
	{
		Key: MetricNewUsers, Label: "New people", Unit: "people", Kind: MetricKindValue,
		Group: MetricGroupOverview, Definition: metricDefNewUsers,
		Displays: []string{DisplayStat},
	},
	{
		Key: MetricSessions, Label: "Sessions", Unit: "sessions", Kind: MetricKindValue,
		Group: MetricGroupOverview, Definition: metricDefSessions,
		Displays: []string{DisplayStat},
	},
	{
		Key: MetricActiveUsersDaily, Label: "Active people per day", Unit: "people/day", Kind: MetricKindSeries,
		Group: MetricGroupOverview, Definition: metricDefDailyActive,
		Displays: []string{DisplayLine, DisplayArea, DisplayBar},
	},
	{
		Key: MetricActivation, Label: "Activation", Unit: "percent", Kind: MetricKindValue,
		Group: MetricGroupUsage, Definition: metricDefActivation, Prerequisite: metricPrereqActivation,
		Displays: []string{DisplayStat},
	},
	{
		Key: MetricRetentionD1, Label: "D1 retention", Unit: "percent", Kind: MetricKindValue,
		Group: MetricGroupUsage, Definition: metricDefRetention, Prerequisite: metricPrereqRetention,
		Displays: []string{DisplayStat},
	},
	{
		Key: MetricRetentionD7, Label: "D7 retention", Unit: "percent", Kind: MetricKindValue,
		Group: MetricGroupUsage, Definition: metricDefRetention, Prerequisite: metricPrereqRetention,
		Displays: []string{DisplayStat},
	},
	{
		Key: MetricRetentionD30, Label: "D30 retention", Unit: "percent", Kind: MetricKindValue,
		Group: MetricGroupUsage, Definition: metricDefRetention, Prerequisite: metricPrereqRetention,
		Displays: []string{DisplayStat},
	},
	{
		Key: MetricRevenue, Label: "Revenue", Unit: "currency", Kind: MetricKindValue,
		Group: MetricGroupMonetization, Definition: metricDefRevenue, Prerequisite: metricPrereqRevenue,
		Displays: []string{DisplayStat},
	},
	{
		Key: MetricTopPages, Label: "Top pages", Unit: "pageviews", Kind: MetricKindBreakdown,
		Group: MetricGroupAcquisition, Definition: metricDefTopPages,
		Displays: []string{DisplayTable, DisplayBar},
	},
	{
		Key: MetricTopSources, Label: "Top sources", Unit: "pageviews", Kind: MetricKindBreakdown,
		Group: MetricGroupAcquisition, Definition: metricDefTopSources,
		Displays: []string{DisplayTable, DisplayBar},
	},
}

// MetricCatalog returns the declared catalog, in display order, with the
// contract version and sort order filled in.
func MetricCatalog() []MetricDefinition {
	out := make([]MetricDefinition, 0, len(metricCatalogDecl))
	for i, def := range metricCatalogDecl {
		def.MetricVersion = OverviewMetricVersion
		def.SortOrder = i
		def.IsSystem = true
		def.Params = []string{MetricParamPeriod, MetricParamPlatform}
		out = append(out, def)
	}
	return out
}

// MetricCatalogEntry looks up one declared metric.
func MetricCatalogEntry(key string) (MetricDefinition, bool) {
	for _, def := range MetricCatalog() {
		if def.Key == key {
			return def, true
		}
	}
	return MetricDefinition{}, false
}

// MetricKeys returns the declared keys in catalog order — the vocabulary a
// declaration is validated against and the list an error quotes back.
func MetricKeys() []string {
	keys := make([]string, 0, len(metricCatalogDecl))
	for _, def := range metricCatalogDecl {
		keys = append(keys, def.Key)
	}
	return keys
}

// migrateMetricCatalog creates the catalog table and refreshes it from the
// declaration. The refresh is an upsert per row plus a delete of rows the
// declaration dropped: the table is system-owned, so a boot is the only writer
// and a stale row would be a metric the catalog serves but the server cannot
// compute.
func (s *Store) migrateMetricCatalog(ctx context.Context) error {
	if _, err := s.pg.Exec(ctx, `
CREATE TABLE IF NOT EXISTS metric_definitions (
	key VARCHAR(64) PRIMARY KEY,
	metric_version VARCHAR(32) NOT NULL,
	label VARCHAR(120) NOT NULL,
	unit VARCHAR(32) NOT NULL,
	kind VARCHAR(16) NOT NULL,
	metric_group VARCHAR(32) NOT NULL,
	definition TEXT NOT NULL,
	prerequisite TEXT NOT NULL DEFAULT '',
	displays JSONB NOT NULL DEFAULT '[]'::jsonb,
	params JSONB NOT NULL DEFAULT '[]'::jsonb,
	sort_order INT NOT NULL DEFAULT 0,
	is_system BOOLEAN NOT NULL DEFAULT true,
	updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
)`); err != nil {
		return err
	}
	if _, err := s.pg.Exec(ctx, `
CREATE INDEX IF NOT EXISTS metric_definitions_group_idx
ON metric_definitions (metric_group, sort_order)`); err != nil {
		return err
	}

	catalog := MetricCatalog()
	keys := make([]string, 0, len(catalog))
	for _, def := range catalog {
		displays, err := json.Marshal(def.Displays)
		if err != nil {
			return fmt.Errorf("metric catalog %s: displays: %w", def.Key, err)
		}
		params, err := json.Marshal(def.Params)
		if err != nil {
			return fmt.Errorf("metric catalog %s: params: %w", def.Key, err)
		}
		if _, err := s.pg.Exec(ctx, `
INSERT INTO metric_definitions (key, metric_version, label, unit, kind, metric_group, definition, prerequisite, displays, params, sort_order, is_system, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, true, now())
ON CONFLICT (key) DO UPDATE SET
	metric_version = EXCLUDED.metric_version,
	label = EXCLUDED.label,
	unit = EXCLUDED.unit,
	kind = EXCLUDED.kind,
	metric_group = EXCLUDED.metric_group,
	definition = EXCLUDED.definition,
	prerequisite = EXCLUDED.prerequisite,
	displays = EXCLUDED.displays,
	params = EXCLUDED.params,
	sort_order = EXCLUDED.sort_order,
	is_system = true,
	updated_at = now()`,
			def.Key, def.MetricVersion, def.Label, def.Unit, def.Kind, def.Group,
			def.Definition, def.Prerequisite, displays, params, def.SortOrder); err != nil {
			return fmt.Errorf("metric catalog %s: %w", def.Key, err)
		}
		keys = append(keys, def.Key)
	}
	if _, err := s.pg.Exec(ctx,
		`DELETE FROM metric_definitions WHERE is_system AND NOT (key = ANY($1))`, keys); err != nil {
		return err
	}
	return nil
}

const metricDefinitionColumns = `key, metric_version, label, unit, kind, metric_group, definition, prerequisite, displays, params, sort_order, is_system`

func metricDefinitionScanDest(d *MetricDefinition) []any {
	return []any{&d.Key, &d.MetricVersion, &d.Label, &d.Unit, &d.Kind, &d.Group,
		&d.Definition, &d.Prerequisite, &d.Displays, &d.Params, &d.SortOrder, &d.IsSystem}
}

// ListMetricDefinitions reads the served catalog in display order.
func (s *Store) ListMetricDefinitions(ctx context.Context) ([]MetricDefinition, error) {
	rows, err := s.pg.Query(ctx, `SELECT `+metricDefinitionColumns+` FROM metric_definitions ORDER BY sort_order, key`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	defs := []MetricDefinition{}
	for rows.Next() {
		var d MetricDefinition
		if err := rows.Scan(metricDefinitionScanDest(&d)...); err != nil {
			return nil, err
		}
		defs = append(defs, d)
	}
	return defs, rows.Err()
}

// MetricDefinitionByKey reads one catalog entry. An unknown key is
// ErrMetricUnknown, and the error names the keys that do exist.
func (s *Store) MetricDefinitionByKey(ctx context.Context, key string) (MetricDefinition, error) {
	var d MetricDefinition
	err := s.pg.QueryRow(ctx,
		`SELECT `+metricDefinitionColumns+` FROM metric_definitions WHERE key = $1`, key).
		Scan(metricDefinitionScanDest(&d)...)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return MetricDefinition{}, fmt.Errorf("%w: %q (known metrics: %s)", ErrMetricUnknown, key, strings.Join(MetricKeys(), ", "))
		}
		return MetricDefinition{}, err
	}
	return d, nil
}

// MetricPoint is one series point: a local calendar day and its value.
type MetricPoint struct {
	Label string `json:"label"`
	Value uint64 `json:"value"`
}

// MetricReading is one metric computed over one range, with everything needed
// to render it honestly: the catalog contract, the state, the value or series,
// and the range's evidence (timezone, range, metric version, capture status).
// Value is nil unless State is "ok" — a metric that cannot be computed carries
// no number, never a fabricated zero.
type MetricReading struct {
	Key           string        `json:"key"`
	MetricVersion string        `json:"metric_version"`
	Label         string        `json:"label"`
	Unit          string        `json:"unit"`
	Kind          string        `json:"kind"`
	Definition    string        `json:"definition"`
	Prerequisite  string        `json:"prerequisite,omitempty"`
	State         string        `json:"state"`
	Value         *uint64       `json:"value,omitempty"`
	Previous      *uint64       `json:"previous,omitempty"`
	Rate          *float64      `json:"rate,omitempty"`
	Notes         []string      `json:"notes,omitempty"`
	Series        []MetricPoint `json:"series,omitempty"`
	Rows          []PathCount   `json:"rows,omitempty"`
	// Revenue carries the signed arithmetic behind the revenue metric: the
	// headline Value is unsigned and shared by every tile, so a negative net
	// exists only here.
	Revenue *OverviewRevenueDetail `json:"revenue,omitempty"`
	Context OverviewContext        `json:"context"`
}

// MetricReadingFor projects one catalog metric out of an Overview result. It is
// a pure function of the two — no query of its own — because the overview read
// is the single deterministic computation for every metric in the catalog, and
// a second implementation beside it is how two surfaces end up disagreeing
// about the same number.
func MetricReadingFor(def MetricDefinition, res OverviewResult) (MetricReading, error) {
	reading := MetricReading{
		Key:           def.Key,
		MetricVersion: def.MetricVersion,
		Label:         def.Label,
		Unit:          def.Unit,
		Kind:          def.Kind,
		Definition:    def.Definition,
		Prerequisite:  def.Prerequisite,
		Context:       res.Context,
	}
	// Capture status first: a project with no events reports no_data for every
	// metric, including the ones that are otherwise always computable.
	if !res.DataStatus.EverReceived {
		reading.State = OverviewStateNoData
		reading.Notes = []string{"no events have arrived for this project yet — the metric is defined, nothing is measured"}
		return reading, nil
	}

	// Value metrics carry the overview's own state, value, comparison and
	// notes: those are the fields the overview tile renders, and re-deriving
	// them here would be a second opinion about the same number.
	valueMetrics := map[string]func() OverviewMetric{
		MetricActiveUsers: func() OverviewMetric { return res.Metrics.ActiveUsers },
		MetricNewUsers:    func() OverviewMetric { return res.Metrics.NewUsers },
		MetricSessions:    func() OverviewMetric { return res.Metrics.Sessions },
		MetricActivation:  func() OverviewMetric { return res.Metrics.Activation },
		MetricRevenue:     func() OverviewMetric { return res.Metrics.Revenue },
	}
	if get, ok := valueMetrics[def.Key]; ok {
		m := get()
		reading.State = m.State
		reading.Value = m.Value
		reading.Previous = m.Previous
		reading.Notes = m.Notes
		if def.Key == MetricRevenue {
			reading.Revenue = res.Metrics.RevenueDetail
		}
		return reading, nil
	}

	switch def.Key {
	case MetricActiveUsersDaily:
		reading.State = overviewMetricState(res.DataStatus.EverReceived, res.DataStatus.QualifyingInRange)
		if reading.State == OverviewStateOK {
			reading.Series = make([]MetricPoint, 0, len(res.Trend))
			for _, p := range res.Trend {
				reading.Series = append(reading.Series, MetricPoint{Label: p.Day, Value: p.ActiveUsers})
			}
		}
		return reading, nil
	case MetricRetentionD1, MetricRetentionD7, MetricRetentionD30:
		point := res.Retention.D1
		switch def.Key {
		case MetricRetentionD7:
			point = res.Retention.D7
		case MetricRetentionD30:
			point = res.Retention.D30
		}
		reading.State = point.State
		reading.Notes = []string{fmt.Sprintf("cohort window: %s; %d returned of %d eligible", res.Retention.CohortWindow, point.Returned, point.Eligible)}
		if point.State == OverviewStateOK {
			// The overview read serves the return rate as a 0–1 fraction
			// because its own renderer scales it (the web tile prints
			// rate*100). The catalog declares this metric's unit as percent, so
			// the reading is served on the percent scale the unit promises —
			// a consumer must never have to know which surface's convention it
			// happens to be holding.
			rate := point.Rate * 100
			reading.Rate = &rate
		}
		return reading, nil
	case MetricTopPages, MetricTopSources:
		list := res.Content.TopPages
		if def.Key == MetricTopSources {
			list = res.Content.TopSources
		}
		reading.State = overviewMetricState(res.DataStatus.EverReceived, res.DataStatus.QualifyingInRange)
		reading.Unit = list.Unit
		reading.Rows = list.Rows
		return reading, nil
	}
	return MetricReading{}, fmt.Errorf("%w: %q (known metrics: %s)", ErrMetricUnknown, def.Key, strings.Join(MetricKeys(), ", "))
}
