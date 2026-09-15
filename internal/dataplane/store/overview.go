package storage

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// overview.go — the deterministic Product Overview read. One Store method
// serves the shared `overview` opcore operation (REST /api/op/overview, MCP
// tools/call, and the GET /api/overview convenience adapter all run this), so
// web and external agents can never see two different metric contracts.
//
// Semantics are locked by the slice-1 contract (see the ticket's
// overview-contract.md): qualifying activity is event_type='user' AND
// visitor_class='human' AND event_name != 'onboarding_verified'. The
// verification event proves receipt only and must never count as product
// activity; broader internal/test exclusion needs provenance fields that do
// not exist yet, so metrics carry that limit as a note rather than claiming it.

// OverviewMetricVersion is bumped when any metric definition changes, so a
// saved chart or agent finding can name the semantics it was computed under.
// v4 is the activation release: the activation tile stops being permanently
// "unconfigured" and computes against the project's stored activation_event
// over a fixed 7-day window. v3 was the money release.
const OverviewMetricVersion = "overview.v4"

// overviewVerificationEvent is the canonical first-event check the onboarding
// flow asks the SDK to send. It is excluded from every qualifying-activity
// metric by stable name predicate — receipt proof, never a user.
const overviewVerificationEvent = "onboarding_verified"

// overviewMaxDays bounds the complete-day window a caller can ask for.
const overviewMaxDays = 90

// overviewQuietAfter is the age past which data_status reports "quiet" — the
// project simply has not received events lately. It is NOT processing lag: no
// pipeline watermark exists, so lag is reported as unavailable, never inferred.
const overviewQuietAfter = 24 * time.Hour

// Metric states shared by every overview block. "unconfigured" means the
// prerequisite (activation mapping, trusted revenue source) does not exist;
// "no_data" means the metric is defined but the range holds no qualifying
// activity; "not_ready" means the cohort window has not elapsed.
const (
	OverviewStateOK           = "ok"
	OverviewStateNoData       = "no_data"
	OverviewStateUnconfigured = "unconfigured"
	OverviewStateNotReady     = "not_ready"
)

type OverviewInput struct {
	// Period is "Nd" (N complete project-local days ending at the last local
	// midnight, 1..90) or "today" (the partial current project-local day).
	// Empty means "7d".
	Period   string `json:"period"`
	Platform string `json:"platform"`
}

type OverviewRange struct {
	From         time.Time `json:"from"`
	To           time.Time `json:"to"`
	Days         int       `json:"days"`
	CompleteDays bool      `json:"complete_days"`
}

type OverviewContext struct {
	ProjectID string `json:"project_id"`
	Timezone  string `json:"timezone"`
	// TimezoneSource is "project" when a validated project setting drove the
	// result, or "fallback" when a legacy nullable row used UTC.
	TimezoneSource string        `json:"timezone_source"`
	Range          OverviewRange `json:"range"`
	PreviousRange  OverviewRange `json:"previous_range"`
	Platform       string        `json:"platform"`
	GeneratedAt    time.Time     `json:"generated_at"`
	MetricVersion  string        `json:"metric_version"`
}

// OverviewMetric is one headline number plus the trust metadata the UI must
// render beside it. Value/Previous are nil unless State is "ok" — a metric
// that cannot be computed honestly carries no number, never a fabricated zero.
// Target is the project-declared target version in force for this window,
// with its verdict when the reading is judgeable; absent when no target is in
// force or the metric's state is an honest empty one (no_data, not_ready).
type OverviewMetric struct {
	State      string            `json:"state"`
	Value      *uint64           `json:"value,omitempty"`
	Previous   *uint64           `json:"previous,omitempty"`
	Definition string            `json:"definition"`
	Notes      []string          `json:"notes,omitempty"`
	Target     *MetricTargetView `json:"target,omitempty"`
}

type OverviewTrendPoint struct {
	Day         string `json:"day"` // YYYY-MM-DD in Context.Timezone
	ActiveUsers uint64 `json:"active_users"`
}

// OverviewRetentionPoint is one daily-cohort return rate. Eligible counts only
// cohort members whose own day N has fully elapsed (per-cohort-day maturity —
// a blended denominator must never include members who could not have
// returned). Eligible == 0 reports State "not_ready" with no rate.
type OverviewRetentionPoint struct {
	State string `json:"state"` // ok | not_ready
	// Rate is serialized even when 0 — a measured 0% is a fact, not a missing
	// value; omitempty would silently drop it.
	Rate     float64           `json:"rate"`
	Returned uint64            `json:"returned"`
	Eligible uint64            `json:"eligible"`
	Target   *MetricTargetView `json:"target,omitempty"`
}

// OverviewList is a ranked breakdown with its unit declared, so a pageview
// count is never rendered under a "people" label.
type OverviewList struct {
	Unit string      `json:"unit"` // "pageviews"
	Rows []PathCount `json:"rows"`
}

type OverviewDataStatus struct {
	LastEventAt    *time.Time `json:"last_event_at,omitempty"`
	LastReceivedAt *time.Time `json:"last_received_at,omitempty"`
	// AgeSeconds is now − last_received_at: how quiet the project is. It is
	// deliberately not called lag — no ingest watermark exists to measure
	// processing delay against.
	AgeSeconds   int64  `json:"age_seconds,omitempty"`
	PipelineLag  string `json:"pipeline_lag"`  // always "unavailable" today
	SchemaStatus string `json:"schema_status"` // always "unavailable" today
	// EventsInRange counts ALL events in the window (any type/class) so the UI
	// can tell "integrated but nothing qualifying" from "nothing arrived".
	EventsInRange uint64 `json:"events_in_range"`
	// QualifyingInRange counts only qualifying-activity events — the
	// discriminator between "integrated, no qualifying activity yet" and
	// "no completed-day data".
	QualifyingInRange uint64                 `json:"qualifying_in_range"`
	EverReceived      bool                   `json:"ever_received"`
	State             string                 `json:"state"` // fresh | quiet | no_events
	Sources           []OverviewSourceStatus `json:"sources"`
	SourcesTruncated  bool                   `json:"sources_truncated"`
}

// OverviewSourceStatus is the bounded, project-scoped source health record
// displayed beside capture freshness. It reports only persisted connector-sync
// facts; it never infers schema health or processing lag.
type OverviewSourceStatus struct {
	ConnectorID    string     `json:"connector_id"`
	ConnectorName  string     `json:"connector_name"`
	ConnectorKind  string     `json:"connector_kind"`
	SyncID         string     `json:"sync_id,omitempty"`
	SourceTable    string     `json:"source_table,omitempty"`
	SyncConfigured bool       `json:"sync_configured"`
	Enabled        bool       `json:"enabled"`
	State          string     `json:"state"` // not_configured | paused | not_ready | healthy | partial | error
	Cursor         string     `json:"cursor,omitempty"`
	CursorKey      string     `json:"cursor_key,omitempty"`
	LastRunAt      *time.Time `json:"last_run_at,omitempty"`
	LastSuccessAt  *time.Time `json:"last_success_at,omitempty"`
	LastStatus     string     `json:"last_status,omitempty"`
	LastError      string     `json:"last_error,omitempty"`
	LastRows       int        `json:"last_rows"`
	TotalRows      int64      `json:"total_rows"`
	SchemaStatus   string     `json:"schema_status"` // always "unavailable" today
}

type OverviewMetrics struct {
	ActiveUsers OverviewMetric `json:"active_users"`
	NewUsers    OverviewMetric `json:"new_users"`
	Sessions    OverviewMetric `json:"sessions"`
	Activation  OverviewMetric `json:"activation"`
	Revenue     OverviewMetric `json:"revenue"`
	// RevenueDetail carries the signed arithmetic behind Revenue — the
	// per-currency gross/reversed/net, the exclusions, and the previous
	// window's net. Revenue.Value is unsigned (the field is shared with every
	// other tile), so a negative net can only be read from here. Additive: a
	// client that does not know the block ignores it.
	RevenueDetail *OverviewRevenueDetail `json:"revenue_detail,omitempty"`
	// ActivationDetail carries the cohort arithmetic behind Activation — the
	// matured cohort size, how many fired the activation event inside the
	// window, and the 0–1 rate. Activation.Value stays nil (a percent is not
	// a count), so the tile and the catalog read the rate from here.
	ActivationDetail *OverviewActivationDetail `json:"activation_detail,omitempty"`
}

// OverviewActivationDetail is the arithmetic behind the activation metric:
// of the people whose first-ever qualifying activity matured past the
// conversion window, how many fired the project's chosen activation event
// inside it. Rate is the 0–1 fraction; the catalog rescales it to the
// declared percent unit, the same convention retention uses.
type OverviewActivationDetail struct {
	Event      string  `json:"event"`
	WindowDays int     `json:"window_days"`
	Eligible   uint64  `json:"eligible"`
	Activated  uint64  `json:"activated"`
	Rate       float64 `json:"rate"`
}

type OverviewRetention struct {
	// CohortWindow declares which cohorts feed the rates: "lifetime" means
	// every first-seen cohort in project history, not the selected range —
	// a 7-day range cannot contain a mature D30 cohort, so scoping cohorts to
	// the range would make D7/D30 permanently not_ready.
	CohortWindow string                 `json:"cohort_window"`
	D1           OverviewRetentionPoint `json:"d1"`
	D7           OverviewRetentionPoint `json:"d7"`
	D30          OverviewRetentionPoint `json:"d30"`
}

type OverviewContent struct {
	TopPages   OverviewList `json:"top_pages"`
	TopSources OverviewList `json:"top_sources"`
}

type OverviewResult struct {
	Context    OverviewContext      `json:"context"`
	Metrics    OverviewMetrics      `json:"metrics"`
	Trend      []OverviewTrendPoint `json:"trend"`
	Retention  OverviewRetention    `json:"retention"`
	Content    OverviewContent      `json:"content"`
	DataStatus OverviewDataStatus   `json:"data_status"`
}

var overviewPeriodRe = regexp.MustCompile(`^(\d{1,2})d$`)

// validPeriod reports whether a period string belongs to the range contract:
// "" (the default 7d), "today" (the partial current day), or "Nd" with
// 1 <= N <= overviewMaxDays. It exists as its own function because a board tile
// declares the same period a read takes, and the two must accept exactly the
// same strings.
func validPeriod(period string) error {
	if period == "" || period == "today" {
		return nil
	}
	m := overviewPeriodRe.FindStringSubmatch(period)
	if m == nil {
		return fmt.Errorf("period must be \"Nd\" (1-%d) or \"today\", got %q", overviewMaxDays, period)
	}
	days, _ := strconv.Atoi(m[1])
	if days < 1 || days > overviewMaxDays {
		return fmt.Errorf("period must be \"Nd\" (1-%d) or \"today\", got %q", overviewMaxDays, period)
	}
	return nil
}

// overviewRange resolves the period string into half-open UTC instants [from,
// to), whose boundaries are midnight in loc. Complete-day ranges exclude the
// current local day; "today" is the explicit partial-period escape.
func overviewRange(period string, now time.Time, loc *time.Location) (OverviewRange, error) {
	if loc == nil {
		return OverviewRange{}, fmt.Errorf("overview: timezone is required")
	}
	localNow := now.In(loc)
	midnight := time.Date(localNow.Year(), localNow.Month(), localNow.Day(), 0, 0, 0, 0, loc)
	if period == "" {
		period = "7d"
	}
	if err := validPeriod(period); err != nil {
		return OverviewRange{}, fmt.Errorf("overview: %w", err)
	}
	if period == "today" {
		return OverviewRange{From: midnight, To: now, Days: 1, CompleteDays: false}, nil
	}
	m := overviewPeriodRe.FindStringSubmatch(period)
	days, _ := strconv.Atoi(m[1])
	return OverviewRange{
		From:         midnight.AddDate(0, 0, -days),
		To:           midnight,
		Days:         days,
		CompleteDays: true,
	}, nil
}

// overviewWindowWhere is the fragment every Overview query starts from — the
// project, the half-open instant range, and the optional platform filter — with
// its bind args in the same order. It is one function rather than a copied
// string so a metric added later cannot quietly widen the window or drop the
// platform filter.
func overviewWindowWhere(projectID string, r OverviewRange, platform string) (string, []any) {
	where := "project_id = ? AND timestamp >= ? AND timestamp < ?"
	args := []any{projectID, r.From, r.To}
	if clause, arg, ok := platformClause(platform); ok {
		where += " AND " + clause
		if arg != nil {
			args = append(args, arg)
		}
	}
	return where, args
}

// overviewQualifying is the WHERE fragment every people/activity metric
// shares: real user events from humans, minus the onboarding verification
// event. It is a fragment (not a filter flag) because it must compose with the
// half-open range bounds this package builds itself.
const overviewQualifying = `event_type = 'user' AND coalesce(visitor_class, 'human') = 'human' AND event_name != '` + overviewVerificationEvent + `'`

// overviewDataState maps receipt facts to the data-status label. "quiet" is an
// age statement (nothing received lately), never a pipeline-lag claim.
func overviewDataState(everReceived bool, age time.Duration) string {
	if !everReceived {
		return "no_events"
	}
	if age > overviewQuietAfter {
		return "quiet"
	}
	return "fresh"
}

// overviewMetricState decides whether a computed count may be shown. A project
// that has never received an event, or whose range holds no qualifying
// activity, reports no_data — the UI renders the honest empty state instead of
// a zero that looks like a measurement.
func overviewMetricState(everReceived bool, eventsInRange uint64) string {
	if !everReceived || eventsInRange == 0 {
		return OverviewStateNoData
	}
	return OverviewStateOK
}

// overviewRetentionPoint builds one D{N} point: eligible is the matured
// denominator, returned the qualifying returns inside it. Zero eligible is
// "not_ready", not 0%.
func overviewRetentionPoint(eligible, returned uint64) OverviewRetentionPoint {
	p := OverviewRetentionPoint{State: OverviewStateNotReady, Returned: returned, Eligible: eligible}
	if eligible == 0 {
		return p
	}
	p.State = OverviewStateOK
	p.Rate = float64(returned) / float64(eligible)
	return p
}

func uint64Ptr(v uint64) *uint64 { return &v }

// Overview computes the whole Product Overview in one deterministic pass over
// the current DuckDB store. now is injectable so tests can pin the
// project-local calendar boundary; production callers pass time.Now().
func (s *Store) Overview(ctx context.Context, projectID, period, platform string, now time.Time) (OverviewResult, error) {
	res := OverviewResult{Trend: []OverviewTrendPoint{}}
	var storedTimezone, activationEvent string
	if err := s.pg.QueryRow(ctx, `SELECT coalesce(timezone, ''), coalesce(activation_event, '') FROM projects WHERE id = $1`, projectID).Scan(&storedTimezone, &activationEvent); err != nil {
		return res, err
	}
	timezone, timezoneSource, loc, err := overviewProjectTimezone(storedTimezone)
	if err != nil {
		return res, err
	}
	r, err := overviewRange(period, now, loc)
	if err != nil {
		return res, err
	}
	prev := OverviewRange{
		From:         r.From.AddDate(0, 0, -r.Days),
		To:           r.From,
		Days:         r.Days,
		CompleteDays: r.CompleteDays,
	}
	res.Context = OverviewContext{
		ProjectID:      projectID,
		Timezone:       timezone,
		TimezoneSource: timezoneSource,
		Range:          r,
		PreviousRange:  prev,
		Platform:       platform,
		GeneratedAt:    now.UTC(),
		MetricVersion:  OverviewMetricVersion,
	}

	// resolved_events supplies the stitched canonical id (the aliases_dict job).
	canonicalID := "canonical_distinct_id"

	// One WHERE fragment for the whole read: project + half-open range +
	// optional platform. Qualifying-activity clauses are added per query so
	// data_status can still see non-qualifying arrivals.
	where, args := overviewWindowWhere(projectID, r, platform)
	qualWhere := where + " AND " + overviewQualifying

	// --- data status (all events, no qualifying clause) ---
	var moneySeen uint64
	{
		var total uint64
		var lastEvent, lastReceived sql.NullTime
		err = s.duckQueryRow(ctx, `
SELECT count(*), max("timestamp"), max(coalesce(inserted_at, "timestamp")),
	count(*) FILTER (WHERE event_name IN ('`+moneyBookingEvent+`', '`+moneyReversalEvent+`'))
FROM events
WHERE project_id = ?`, []any{projectID}, &total, &lastEvent, &lastReceived, &moneySeen)
		if err != nil {
			return res, err
		}
		res.DataStatus.EverReceived = total > 0
		if res.DataStatus.EverReceived {
			lastEventAt := lastEvent.Time
			lastReceivedAt := lastReceived.Time
			res.DataStatus.LastEventAt = &lastEventAt
			res.DataStatus.LastReceivedAt = &lastReceivedAt
			age := now.UTC().Sub(lastReceivedAt)
			if age < 0 {
				age = 0
			}
			res.DataStatus.AgeSeconds = int64(age.Seconds())
		}
		res.DataStatus.State = overviewDataState(res.DataStatus.EverReceived, now.UTC().Sub(lastReceived.Time))
		res.DataStatus.PipelineLag = "unavailable"
		res.DataStatus.SchemaStatus = "unavailable"

		sources, truncated, sourceErr := s.overviewSources(ctx, projectID)
		if sourceErr != nil {
			return res, sourceErr
		}
		res.DataStatus.Sources = sources
		res.DataStatus.SourcesTruncated = truncated

		var inRange, qualifying uint64
		err = s.duckQueryRow(ctx, `
SELECT count(*), count(*) FILTER (WHERE `+overviewQualifying+`)
FROM events
WHERE `+where, args, &inRange, &qualifying)
		if err != nil {
			return res, err
		}
		res.DataStatus.EventsInRange = inRange
		res.DataStatus.QualifyingInRange = qualifying
	}

	// Metric visibility keys off QUALIFYING events, not raw arrivals — a
	// project whose only event is the onboarding verification read is
	// "integrated, no qualifying activity", not a populated overview.
	metricState := overviewMetricState(res.DataStatus.EverReceived, res.DataStatus.QualifyingInRange)
	exclusionNote := "excludes bots and the onboarding verification event; other internal/test traffic not separable yet"

	// --- headline metrics: current + previous range in one pass ---
	{
		platClause, platArg := overviewPlatform(platform)
		var active, activePrev, sessions, sessionsPrev uint64
		qargs := []any{r.From, r.To, prev.From, prev.To, r.From, r.To, prev.From, prev.To,
			projectID, prev.From, r.To}
		if platArg != nil {
			qargs = append(qargs, platArg)
		}
		err = s.duckQueryRow(ctx, `
SELECT
	count(DISTINCT `+canonicalID+`) FILTER (WHERE "timestamp" >= ? AND "timestamp" < ?),
	count(DISTINCT `+canonicalID+`) FILTER (WHERE "timestamp" >= ? AND "timestamp" < ?),
	count(DISTINCT session_id) FILTER (WHERE session_id <> '' AND "timestamp" >= ? AND "timestamp" < ?),
	count(DISTINCT session_id) FILTER (WHERE session_id <> '' AND "timestamp" >= ? AND "timestamp" < ?)
FROM resolved_events
WHERE project_id = ? AND "timestamp" >= ? AND "timestamp" < ? AND `+overviewQualifying+platClause,
			qargs,
			&active, &activePrev, &sessions, &sessionsPrev)
		if err != nil {
			return res, err
		}
		res.Metrics.ActiveUsers = OverviewMetric{
			State:      metricState,
			Definition: metricDefActiveUsers,
			Notes:      []string{exclusionNote, "anonymous people are approximate until an explicit identify link exists"},
		}
		res.Metrics.Sessions = OverviewMetric{
			State:      metricState,
			Definition: metricDefSessions,
			Notes:      []string{exclusionNote},
		}
		if metricState == OverviewStateOK {
			res.Metrics.ActiveUsers.Value = uint64Ptr(active)
			res.Metrics.Sessions.Value = uint64Ptr(sessions)
			// A partial range ("today") gets no comparison — a full previous
			// day against a few elapsed hours is a misleading delta.
			if r.CompleteDays {
				res.Metrics.ActiveUsers.Previous = uint64Ptr(activePrev)
				res.Metrics.Sessions.Previous = uint64Ptr(sessionsPrev)
			}
		}
	}

	// --- new users: first-ever qualifying event inside the range ---
	{
		_, platArg := overviewPlatform(platform)
		var newUsers, newUsersPrev uint64
		qargs := []any{r.From, r.To, prev.From, prev.To, projectID}
		if platArg != nil {
			qargs = append(qargs, platArg)
		}
		err = s.duckQueryRow(ctx, `
SELECT
	count(*) FILTER (WHERE first_ts >= ? AND first_ts < ?),
	count(*) FILTER (WHERE first_ts >= ? AND first_ts < ?)
FROM (
	SELECT `+canonicalID+` AS cid, min("timestamp") AS first_ts,
		(array_agg(coalesce(platform, '') ORDER BY "timestamp" ASC, event_id ASC))[1] AS first_platform
	FROM resolved_events
	WHERE project_id = ? AND `+overviewQualifying+`
	GROUP BY cid
)
WHERE 1 = 1`+firstPlatformClause(platform), qargs, &newUsers, &newUsersPrev)
		if err != nil {
			return res, err
		}
		res.Metrics.NewUsers = OverviewMetric{
			State:      metricState,
			Definition: metricDefNewUsers,
			Notes:      []string{exclusionNote},
		}
		if metricState == OverviewStateOK {
			res.Metrics.NewUsers.Value = uint64Ptr(newUsers)
			if r.CompleteDays {
				res.Metrics.NewUsers.Previous = uint64Ptr(newUsersPrev)
			}
		}
	}
	// --- money: one deduplicated, per-currency, signed net ---
	{
		metric, detail, err := s.overviewRevenue(ctx, projectID, r, prev, platform, moneySeen > 0)
		if err != nil {
			return res, err
		}
		res.Metrics.Revenue = metric
		res.Metrics.RevenueDetail = detail
	}

	// --- activation: configured event over a fixed 7-day cohort window ---
	{
		metric, detail, err := s.overviewActivation(ctx, projectID, platform, timezone, r.To, activationEvent)
		if err != nil {
			return res, err
		}
		res.Metrics.Activation = metric
		res.Metrics.ActivationDetail = detail
	}

	// --- daily active-user trend ---
	{
		byDay := map[string]uint64{}
		err := s.duckQuery(ctx, `
SELECT CAST(timezone(?, "timestamp") AS DATE) AS day, count(DISTINCT `+canonicalID+`) AS users
FROM resolved_events
WHERE `+qualWhere+`
GROUP BY day
ORDER BY day`, append([]any{timezone}, args...), func(rows *sql.Rows) error {
			var day time.Time
			var users uint64
			if err := rows.Scan(&day, &users); err != nil {
				return err
			}
			byDay[day.Format("2006-01-02")] = users
			return nil
		})
		if err != nil {
			return res, err
		}
		// Emit every local calendar day in the range, including zero days — a
		// gap in the series must read as a zero day, not a missing interpolation.
		for d := r.From.In(loc); d.Before(r.To); d = d.AddDate(0, 0, 1) {
			key := d.Format("2006-01-02")
			res.Trend = append(res.Trend, OverviewTrendPoint{Day: key, ActiveUsers: byDay[key]})
		}
	}

	// --- D1/D7/D30 retention: per-cohort-day maturity ---
	{
		ret, err := s.overviewRetention(ctx, projectID, platform, timezone, r.To)
		if err != nil {
			return res, err
		}
		res.Retention = ret
	}

	// --- content: top pages + sources (pageview units, declared) ---
	//
	// Acquisition is the same population as every people metric: real user
	// pageviews from humans — see overviewAcquisitionFilter and qualWhere.
	{
		pages, err := s.propertyCounts(ctx, projectID, overviewAcquisitionFilter(r, platform), "path", "user.pageview")
		if err != nil {
			return res, err
		}
		res.Content.TopPages = OverviewList{Unit: "pageviews", Rows: pages}

		sources := []PathCount{}
		err = s.duckQuery(ctx, `
SELECT if(coalesce(referrer_channel, '') = '', 'unknown', referrer_channel) AS channel, count(*) AS count
FROM events
WHERE `+qualWhere+` AND event_name = 'user.pageview'
GROUP BY channel
ORDER BY count DESC
LIMIT 20`, args, func(rows *sql.Rows) error {
			var item PathCount
			if err := rows.Scan(&item.Value, &item.Count); err != nil {
				return err
			}
			sources = append(sources, item)
			return nil
		})
		if err != nil {
			return res, err
		}
		res.Content.TopSources = OverviewList{Unit: "pageviews", Rows: sources}
	}

	// --- declared targets: the version in force at the window's end, judged
	// against the measured value when the reading can be judged ---
	if err := s.attachMetricTargets(ctx, projectID, &res); err != nil {
		return res, err
	}

	return res, nil
}

// attachMetricTargets resolves each catalog metric's target version in force
// at the read window's end and hangs it on the metric it judges. The verdict
// is computed here — the one place the measured value exists — so a board
// tile, the overview headline and read_metric can never disagree about the
// same number's standing.
//
// A metric in an honest empty state (no_data, not_ready) serves no target at
// all: the tile's state already says what is true, and a target line beside
// "No data" would pretend a measurement exists. A metric that is defined but
// not measurable (unconfigured) serves the target with no verdict — the
// declaration is real, the reading is not.
func (s *Store) attachMetricTargets(ctx context.Context, projectID string, res *OverviewResult) error {
	targets, err := metricTargetsInForce(ctx, s.pg, projectID, res.Context.Range.To)
	if err != nil {
		return err
	}
	attach := func(key string, m *OverviewMetric, unit string, measured float64, currency string) {
		t, ok := targets[key]
		if !ok || t.Cleared {
			return
		}
		if m.State == OverviewStateNoData || m.State == OverviewStateNotReady {
			return
		}
		if m.State != OverviewStateOK {
			m.Target = &MetricTargetView{
				Version: t.Version, Direction: t.Direction, Value: t.Value,
				PeriodDays: t.PeriodDays, Currency: t.Currency, EffectiveAt: t.EffectiveAt,
				Label:         metricTargetLabel(t, unit),
				VerdictReason: TargetReasonMetricUnavailable,
			}
			return
		}
		view := judgeMetricTarget(t, unit, res.Context.Range, measured, currency)
		m.Target = &view
	}
	if v := res.Metrics.ActiveUsers.Value; v != nil {
		attach(MetricActiveUsers, &res.Metrics.ActiveUsers, "people", float64(*v), "")
	} else {
		attach(MetricActiveUsers, &res.Metrics.ActiveUsers, "people", 0, "")
	}
	if v := res.Metrics.NewUsers.Value; v != nil {
		attach(MetricNewUsers, &res.Metrics.NewUsers, "people", float64(*v), "")
	} else {
		attach(MetricNewUsers, &res.Metrics.NewUsers, "people", 0, "")
	}
	if v := res.Metrics.Sessions.Value; v != nil {
		attach(MetricSessions, &res.Metrics.Sessions, "sessions", float64(*v), "")
	} else {
		attach(MetricSessions, &res.Metrics.Sessions, "sessions", 0, "")
	}
	// Activation is a percent metric with no measured value yet — the target
	// still serves (unconfigured), just never a verdict.
	attach(MetricActivation, &res.Metrics.Activation, "percent", 0, "")
	// Revenue is judged on the signed net in the headline currency — the
	// unsigned headline Value cannot express a net reversal.
	if d := res.Metrics.RevenueDetail; d != nil {
		attach(MetricRevenue, &res.Metrics.Revenue, "currency", float64(d.Net), d.Currency)
	} else {
		attach(MetricRevenue, &res.Metrics.Revenue, "currency", 0, "")
	}
	// Retention rates are judged on the percent scale the catalog declares —
	// the same scale read_metric serves — never the 0-1 fraction.
	for key, point := range map[string]*OverviewRetentionPoint{
		MetricRetentionD1:  &res.Retention.D1,
		MetricRetentionD7:  &res.Retention.D7,
		MetricRetentionD30: &res.Retention.D30,
	} {
		t, ok := targets[key]
		if !ok || t.Cleared || point.State != OverviewStateOK {
			continue
		}
		view := judgeMetricTarget(t, "percent", res.Context.Range, point.Rate*100, "")
		point.Target = &view
	}
	return nil
}

const overviewSourceLimit = 20

// overviewSources reads enough rows to say when the Overview's bounded source
// list has been truncated. It uses the persisted sync summary rather than
// performing one latest-run lookup per row.
func (s *Store) overviewSources(ctx context.Context, projectID string) ([]OverviewSourceStatus, bool, error) {
	rows, err := s.pg.Query(ctx, `
SELECT c.id::text, c.name, c.kind,
       COALESCE(cs.id::text, ''), COALESCE(cs.source_table, ''),
       cs.id IS NOT NULL, COALESCE(cs.enabled, false),
       COALESCE(cs.cursor, ''), COALESCE(cs.cursor_key, ''),
       cs.last_run_at, cs.last_success_at, COALESCE(cs.last_status, ''),
       COALESCE(cs.last_error, ''), COALESCE(cs.last_rows, 0), COALESCE(cs.total_rows, 0)
FROM data_connectors c
LEFT JOIN connector_syncs cs
  ON cs.connector_id = c.id AND cs.project_id = c.project_id
WHERE c.project_id = $1 AND c.archived_at IS NULL
ORDER BY c.created_at DESC, c.id, cs.created_at ASC NULLS LAST, cs.id
LIMIT $2`, projectID, overviewSourceLimit+1)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()

	sources := make([]OverviewSourceStatus, 0, overviewSourceLimit)
	for rows.Next() {
		var source OverviewSourceStatus
		if err := rows.Scan(
			&source.ConnectorID, &source.ConnectorName, &source.ConnectorKind,
			&source.SyncID, &source.SourceTable,
			&source.SyncConfigured, &source.Enabled,
			&source.Cursor, &source.CursorKey,
			&source.LastRunAt, &source.LastSuccessAt, &source.LastStatus,
			&source.LastError, &source.LastRows, &source.TotalRows,
		); err != nil {
			return nil, false, err
		}
		source.State = overviewSourceState(source.SyncConfigured, source.Enabled, source.LastRunAt, source.LastStatus, source.LastRows)
		source.SchemaStatus = "unavailable"
		sources = append(sources, source)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	truncated := len(sources) > overviewSourceLimit
	if truncated {
		sources = sources[:overviewSourceLimit]
	}
	return sources, truncated, nil
}

func overviewSourceState(configured, enabled bool, lastRunAt *time.Time, lastStatus string, lastRows int) string {
	if !configured {
		return "not_configured"
	}
	if !enabled {
		return "paused"
	}
	if lastRunAt == nil {
		return "not_ready"
	}
	if lastStatus == "error" {
		if lastRows > 0 {
			return "partial"
		}
		return "error"
	}
	return "healthy"
}

// firstPlatformClause filters a first-seen subquery by the platform OF THE
// FIRST EVENT, not by the platform of any event — a known web user whose first
// iOS event arrives today is not a new iOS user. "" = all, "unknown" = the
// first event carried no platform.
func firstPlatformClause(platform string) string {
	platform = strings.ToLower(strings.TrimSpace(platform))
	switch platform {
	case "":
		return ""
	case PlatformUnknown:
		return " AND first_platform = ''"
	default:
		return " AND first_platform = ?"
	}
}

// overviewPlatform returns the platform WHERE fragment and its bind arg for
// hand-built queries, matching platformClause's semantics (empty = all,
// 'unknown' = empty column). The value is always bound, never inlined.
func overviewPlatform(platform string) (string, any) {
	clause, arg, ok := platformClause(platform)
	if !ok {
		return "", nil
	}
	return " AND " + clause, arg
}

// overviewRetention computes D1/D7/D30 return rates over project-local daily
// cohorts. A cohort is first-ever qualifying activity, then optionally filtered
// by the platform of that first event — the same first-platform contract as
// new users. Returns themselves remain scoped to the selected platform.
func (s *Store) overviewRetention(ctx context.Context, projectID, platform, timezone string, to time.Time) (OverviewRetention, error) {
	out := OverviewRetention{CohortWindow: "lifetime"}
	platClause, platArg := overviewPlatform(platform)
	firstClause := firstPlatformClause(platform)
	// resolved_events carries the stitched canonical id (the aliases_dict job).
	// The join aliases it as e, so the canonical column is e.canonical_distinct_id.
	canonicalID := "canonical_distinct_id"
	canonicalE := "e.canonical_distinct_id"

	firsts := `
SELECT cid, CAST(timezone(?, first_ts) AS DATE) AS cohort_day
FROM (
	SELECT ` + canonicalID + ` AS cid, min("timestamp") AS first_ts,
		(array_agg(coalesce(platform, '') ORDER BY "timestamp" ASC, event_id ASC))[1] AS first_platform
	FROM resolved_events
	WHERE project_id = ? AND ` + overviewQualifying + `
	GROUP BY cid
)
WHERE 1 = 1` + firstClause
	firstArgs := []any{timezone, projectID}
	if platArg != nil {
		firstArgs = append(firstArgs, platArg)
	}

	// eligible_N = members whose local calendar day-N window has fully closed
	// by `to`; a partial Today window cannot mature an in-progress local day.
	var elig [3]uint64
	err := s.duckQueryRow(ctx, `
SELECT
	count(*) FILTER (WHERE cohort_day + INTERVAL '2 days' <= CAST(timezone(?, ?) AS DATE)),
	count(*) FILTER (WHERE cohort_day + INTERVAL '8 days' <= CAST(timezone(?, ?) AS DATE)),
	count(*) FILTER (WHERE cohort_day + INTERVAL '31 days' <= CAST(timezone(?, ?) AS DATE))
FROM (`+firsts+`)`, append([]any{timezone, to, timezone, to, timezone, to}, firstArgs...), &elig[0], &elig[1], &elig[2])
	if err != nil {
		return out, err
	}

	// returned_N = eligible members with a qualifying event on the Nth local
	// calendar day. Both the cohort subquery and return events obey the same
	// first-platform/platform-filter semantics as new users and active users.
	var ret [3]uint64
	err = s.duckQueryRow(ctx, `
SELECT
	count(DISTINCT `+canonicalE+`) FILTER (WHERE CAST(timezone(?, e."timestamp") AS DATE) = f.cohort_day + INTERVAL '1 day' AND f.cohort_day + INTERVAL '2 days' <= CAST(timezone(?, ?) AS DATE)),
	count(DISTINCT `+canonicalE+`) FILTER (WHERE CAST(timezone(?, e."timestamp") AS DATE) = f.cohort_day + INTERVAL '7 days' AND f.cohort_day + INTERVAL '8 days' <= CAST(timezone(?, ?) AS DATE)),
	count(DISTINCT `+canonicalE+`) FILTER (WHERE CAST(timezone(?, e."timestamp") AS DATE) = f.cohort_day + INTERVAL '30 days' AND f.cohort_day + INTERVAL '31 days' <= CAST(timezone(?, ?) AS DATE))
FROM resolved_events e
INNER JOIN (`+firsts+`) f ON `+canonicalE+` = f.cid
WHERE e.project_id = ? AND `+overviewQualifying+platClause,
		append(append([]any{timezone, timezone, to, timezone, timezone, to, timezone, timezone, to}, firstArgs...), append([]any{projectID}, platformArgs(platArg)...)...),
		&ret[0], &ret[1], &ret[2])
	if err != nil {
		return out, err
	}

	out.D1 = overviewRetentionPoint(elig[0], ret[0])
	out.D7 = overviewRetentionPoint(elig[1], ret[1])
	out.D30 = overviewRetentionPoint(elig[2], ret[2])
	return out, nil
}

func platformArgs(arg any) []any {
	if arg == nil {
		return nil
	}
	return []any{arg}
}

// overviewActivation computes the project's activation metric. It is
// unconfigured when the project carries no activation_event, not_ready when
// no first-seen cohort has reached the 7-day conversion window, and ok once
// mature cohort members exist. Like retention, cohorts are lifetime
// first-qualifying activity, scoped to the first event's platform.
func (s *Store) overviewActivation(ctx context.Context, projectID, platform, timezone string, to time.Time, activationEvent string) (OverviewMetric, *OverviewActivationDetail, error) {
	activationEvent = strings.TrimSpace(activationEvent)
	if activationEvent == "" {
		return OverviewMetric{
			State:      OverviewStateUnconfigured,
			Definition: metricDefActivation,
			Notes:      []string{metricPrereqActivation},
		}, nil, nil
	}

	platClause, platArg := overviewPlatform(platform)
	firstClause := firstPlatformClause(platform)
	canonicalID := "canonical_distinct_id"
	canonicalE := "e.canonical_distinct_id"

	firsts := `
SELECT cid, CAST(timezone(?, first_ts) AS DATE) AS cohort_day
FROM (
	SELECT ` + canonicalID + ` AS cid, min("timestamp") AS first_ts,
		(array_agg(coalesce(platform, '') ORDER BY "timestamp" ASC, event_id ASC))[1] AS first_platform
	FROM resolved_events
	WHERE project_id = ? AND ` + overviewQualifying + `
	GROUP BY cid
)
WHERE 1 = 1` + firstClause
	firstArgs := []any{timezone, projectID}
	if platArg != nil {
		firstArgs = append(firstArgs, platArg)
	}

	// Eligible: members whose local calendar 7-day window has fully closed by `to`
	var eligible uint64
	err := s.duckQueryRow(ctx, `
SELECT count(*) FILTER (WHERE cohort_day + INTERVAL '8 days' <= CAST(timezone(?, ?) AS DATE))
FROM (`+firsts+`)`, append([]any{timezone, to}, firstArgs...), &eligible)
	if err != nil {
		return OverviewMetric{}, nil, err
	}

	detail := &OverviewActivationDetail{
		Event:      activationEvent,
		WindowDays: 7,
		Eligible:   eligible,
	}

	note := fmt.Sprintf("event: %s; 7-day conversion window; %d activated of %d eligible", activationEvent, 0, eligible)

	if eligible == 0 {
		return OverviewMetric{
			State:      OverviewStateNotReady,
			Definition: metricDefActivation,
			Notes:      []string{note, "no mature 7-day cohort yet"},
		}, detail, nil
	}

	// Activated: eligible members who fired the activation_event within 7 local
	// days of their cohort day. Placeholder order follows the SQL text: the
	// FROM-clause `firsts` subquery binds first (timezone, projectID, platform),
	// then the WHERE clause (timezone, to, timezone, timezone, projectID,
	// event name, platform).
	var activated uint64
	qargs := append([]any{}, firstArgs...)
	qargs = append(qargs, timezone, to, timezone, timezone, projectID, activationEvent)
	if platArg != nil {
		qargs = append(qargs, platArg)
	}

	err = s.duckQueryRow(ctx, `
SELECT count(DISTINCT `+canonicalE+`)
FROM resolved_events e
INNER JOIN (`+firsts+`) f ON `+canonicalE+` = f.cid
WHERE f.cohort_day + INTERVAL '8 days' <= CAST(timezone(?, ?) AS DATE)
  AND CAST(timezone(?, e."timestamp") AS DATE) >= f.cohort_day
  AND CAST(timezone(?, e."timestamp") AS DATE) <= f.cohort_day + INTERVAL '7 days'
  AND coalesce(e.visitor_class, 'human') = 'human'
  AND e.project_id = ? AND e.event_name = ?`+platClause,
		qargs, &activated)
	if err != nil {
		return OverviewMetric{}, nil, err
	}

	rate := float64(activated) / float64(eligible)
	detail.Activated = activated
	detail.Rate = rate

	note = fmt.Sprintf("event: %s; 7-day conversion window; %d activated of %d eligible", activationEvent, activated, eligible)
	return OverviewMetric{
		State:      OverviewStateOK,
		Definition: metricDefActivation,
		Notes:      []string{note},
	}, detail, nil
}
