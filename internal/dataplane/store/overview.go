package storage

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"regexp"
	"slices"
	"sort"
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
// v3 is the money release: revenue stops being "unconfigured" and becomes a
// deduplicated, per-currency, signed net (see money.go).
// v4 absorbs the reads the retired /traffic, /web-analytics and /product
// surfaces served — pageviews, conversions, traffic class, AI share, session
// quality, platform split, top events and event volume — so the declared
// boards and read_metric answer them from the same deterministic pass.
// v5 is the activation release: the activation tile stops being permanently
// "unconfigured" and computes against the project's stored activation_event
// over a fixed 7-day window.
// v6 adopts the App Store Connect reads the analysis boards were shaped
// after: sessions per person and its daily trend, paying people, proceeds
// per paying person, and the download→paid cohort conversion (D1/D7/D35).
// v7 adds acquisition detail: utm_source/utm_medium/utm_campaign columns on
// events, plus top_utm_sources, top_campaigns and top_referrers breakdowns
// over the same human pageview population as top_sources.
const OverviewMetricVersion = "overview.v7"

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
// Rate is the float channel for metrics whose honest value is not a count —
// a share, a rate or a duration — served on the scale the catalog unit
// declares (percent for shares, seconds for durations). A metric carries
// Value XOR Rate, never both.
//
// Target is the project-declared target version in force for this window,
// with its verdict when the reading is judgeable; absent when no target is in
// force or the metric's state is an honest empty one (no_data, not_ready).
type OverviewMetric struct {
	State      string            `json:"state"`
	Value      *uint64           `json:"value,omitempty"`
	Previous   *uint64           `json:"previous,omitempty"`
	Rate       *float64          `json:"rate,omitempty"`
	Definition string            `json:"definition"`
	Notes      []string          `json:"notes,omitempty"`
	Target     *MetricTargetView `json:"target,omitempty"`
}

type OverviewTrendPoint struct {
	Day         string `json:"day"` // YYYY-MM-DD in Context.Timezone
	ActiveUsers uint64 `json:"active_users"`
	// Sessions is the distinct qualifying session ids that day — the daily
	// companion to ActiveUsers (App Store Connect's sessions-per-day trend).
	Sessions uint64 `json:"sessions"`
	// Events is every received event that day — the event-volume series the
	// retired Product page's trend question charted. It deliberately counts
	// non-qualifying rows too: volume is an ingestion fact, and a crawler wave
	// IS the answer when the question is "how much arrived".
	Events uint64 `json:"events"`
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
	// Pageviews and Conversions are the retired Traffic page's headline counts.
	// Unlike the people metrics they count every received event — a crawler's
	// pageview is still a pageview, and the traffic-by-class breakdown beside
	// them is where the human/non-human split lives.
	Pageviews   OverviewMetric `json:"pageviews"`
	Conversions OverviewMetric `json:"conversions"`
	// SessionsPerUser is App Store Connect's "sessions per device": distinct
	// session ids over distinct people on qualifying activity, carried on the
	// Rate channel because the honest value is a ratio, not a count.
	SessionsPerUser OverviewMetric `json:"sessions_per_user"`
	// PayingUsers counts the distinct people with a positive deduplicated
	// booking in the window; ProceedsPerPaying is the headline currency's net
	// over the people who paid in it (Rate, smallest unit). Both share the
	// revenue metric's unconfigured/no_data gate.
	PayingUsers       OverviewMetric `json:"paying_users"`
	ProceedsPerPaying OverviewMetric `json:"proceeds_per_paying"`
	// AIShare is the non-human share of classified pageviews, on the percent
	// scale (Rate). Session quality is the retired page's bounce rate and
	// average session duration, computed over every session in the window.
	AIShare            OverviewMetric `json:"ai_share"`
	BounceRate         OverviewMetric `json:"bounce_rate"`
	AvgSessionDuration OverviewMetric `json:"avg_session_duration"`
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
	// UTM-tagged acquisition detail and the external referrer hosts behind it —
	// the same human pageview population as TopSources, grouped by the new
	// columns instead of the classified channel.
	TopUTMSources OverviewList `json:"top_utm_sources"`
	TopCampaigns  OverviewList `json:"top_campaigns"`
	TopReferrers  OverviewList `json:"top_referrers"`
	// FirstReadDiscovery breaks down the discovery surface of readers completing
	// their first activation event (e.g. chapter_view) in the period: classified
	// from the human pageview immediately preceding that first activation into
	// home, search, direct, or communication surfaces.
	FirstReadDiscovery OverviewList `json:"first_read_discovery"`
	// The retired Traffic page's remaining breakdowns. TrafficByClass counts
	// pageviews per visitor class (human / search-bot / ai-platform) — the
	// non-human rows are the point, so this list is not humans-filtered.
	// AITopPaths is the pages AI crawlers and AI referrals actually hit.
	// TrafficByPlatform is pageviews per app; per-platform people stay on the
	// page's platform segment, which scopes every metric on the board.
	TrafficByClass    OverviewList `json:"traffic_by_class"`
	AITopPaths        OverviewList `json:"ai_top_paths"`
	TrafficByPlatform OverviewList `json:"traffic_by_platform"`
	// TopEvents is the retired Product page's raw event ranking — every
	// received event name by volume, no qualifying filter.
	TopEvents OverviewList `json:"top_events"`
}

// OverviewPaidConversion is the download→paid cohort read: D1/D7/D35 points
// with the same maturity contract as retention.
type OverviewPaidConversion struct {
	CohortWindow string                 `json:"cohort_window"`
	D1           OverviewRetentionPoint `json:"d1"`
	D7           OverviewRetentionPoint `json:"d7"`
	D35          OverviewRetentionPoint `json:"d35"`
}

type OverviewResult struct {
	Context    OverviewContext      `json:"context"`
	Metrics    OverviewMetrics      `json:"metrics"`
	Trend      []OverviewTrendPoint `json:"trend"`
	Retention  OverviewRetention    `json:"retention"`
	// PaidConversion is App Store Connect's download→paid cohort read: the
	// share of a lifetime first-activity cohort that booked positive revenue
	// within N local days. Same point shape as retention — eligible is the
	// matured denominator, never a blended one.
	PaidConversion OverviewPaidConversion `json:"paid_conversion"`
	Content        OverviewContent        `json:"content"`
	DataStatus     OverviewDataStatus     `json:"data_status"`
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

func float64Ptr(v float64) *float64 { return &v }

// overviewEventState is the no-data gate for metrics whose population is every
// received event, not qualifying activity — a project that only ever received
// crawler pageviews still has pageviews to report.
func overviewEventState(everReceived bool, eventsInRange uint64) string {
	if !everReceived || eventsInRange == 0 {
		return OverviewStateNoData
	}
	return OverviewStateOK
}

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
		// Sessions per person is the same two counts divided — it exists only
		// when both halves are measured, so it inherits their state and never
		// divides by an empty population.
		res.Metrics.SessionsPerUser = OverviewMetric{
			State:      metricState,
			Definition: metricDefSessionsPerUser,
			Notes:      []string{exclusionNote},
		}
		if metricState == OverviewStateOK && active > 0 {
			res.Metrics.SessionsPerUser.Rate = float64Ptr(float64(sessions) / float64(active))
			if r.CompleteDays && activePrev > 0 {
				prev := float64(sessionsPrev) / float64(activePrev)
				res.Metrics.SessionsPerUser.Notes = append(res.Metrics.SessionsPerUser.Notes,
					fmt.Sprintf("previous window: %.2f sessions per person", prev))
			}
		}
	}

	// --- pageviews + conversions: the retired Traffic page's headline counts ---
	// Every received event counts — the class split below is where the
	// human/non-human question is answered, so filtering here would make the
	// headline disagree with the breakdown under it.
	{
		platClause, platArg := overviewPlatform(platform)
		var pageviews, pageviewsPrev, conversions, conversionsPrev uint64
		qargs := []any{r.From, r.To, prev.From, prev.To, r.From, r.To, prev.From, prev.To,
			projectID, prev.From, r.To}
		if platArg != nil {
			qargs = append(qargs, platArg)
		}
		err = s.duckQueryRow(ctx, `
SELECT
	count(*) FILTER (WHERE event_name = 'user.pageview' AND "timestamp" >= ? AND "timestamp" < ?),
	count(*) FILTER (WHERE event_name = 'user.pageview' AND "timestamp" >= ? AND "timestamp" < ?),
	count(*) FILTER (WHERE event_name IN ('user.conversion', 'user.signup') AND "timestamp" >= ? AND "timestamp" < ?),
	count(*) FILTER (WHERE event_name IN ('user.conversion', 'user.signup') AND "timestamp" >= ? AND "timestamp" < ?)
FROM events
WHERE project_id = ? AND "timestamp" >= ? AND "timestamp" < ?`+platClause,
			qargs,
			&pageviews, &pageviewsPrev, &conversions, &conversionsPrev)
		if err != nil {
			return res, err
		}
		eventState := overviewEventState(res.DataStatus.EverReceived, res.DataStatus.EventsInRange)
		res.Metrics.Pageviews = OverviewMetric{State: eventState, Definition: metricDefPageviews}
		res.Metrics.Conversions = OverviewMetric{State: eventState, Definition: metricDefConversions}
		if eventState == OverviewStateOK {
			res.Metrics.Pageviews.Value = uint64Ptr(pageviews)
			res.Metrics.Conversions.Value = uint64Ptr(conversions)
			if r.CompleteDays {
				res.Metrics.Pageviews.Previous = uint64Ptr(pageviewsPrev)
				res.Metrics.Conversions.Previous = uint64Ptr(conversionsPrev)
			}
		}
	}

	// --- session quality: bounce rate + average duration over every session ---
	{
		duration, bounce, sessionCount, err := s.sessionQualityWhere(ctx, where, args)
		if err != nil {
			return res, err
		}
		if math.IsNaN(duration) || math.IsInf(duration, 0) {
			duration = 0
		}
		if math.IsNaN(bounce) || math.IsInf(bounce, 0) {
			bounce = 0
		}
		// Zero sessions is no_data, not a 0% bounce — a rate over an empty
		// population is a fabricated measurement.
		sessionState := OverviewStateNoData
		if res.DataStatus.EverReceived && sessionCount > 0 {
			sessionState = OverviewStateOK
		}
		res.Metrics.BounceRate = OverviewMetric{State: sessionState, Definition: metricDefBounceRate}
		res.Metrics.AvgSessionDuration = OverviewMetric{State: sessionState, Definition: metricDefAvgSession}
		if sessionState == OverviewStateOK {
			res.Metrics.BounceRate.Rate = float64Ptr(bounce * 100)
			res.Metrics.AvgSessionDuration.Rate = float64Ptr(duration)
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
		arg_min(coalesce(platform, ''), ("timestamp", event_id)) AS first_platform
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

		// Paying users and proceeds-per-paying-person ride the revenue gate:
		// a project that never sent a money row is unconfigured, one whose
		// window holds none is no_data. The payer counts live on the detail
		// the same pass already computed — no second query.
		payingNotes := []string{"a refund does not un-pay a person; the count is distinct people with a positive deduplicated booking"}
		if metric.State == OverviewStateUnconfigured {
			payingNotes = []string{metricPrereqRevenue}
		}
		res.Metrics.PayingUsers = OverviewMetric{
			State:      metric.State,
			Definition: metricDefPayingUsers,
			Notes:      payingNotes,
		}
		res.Metrics.ProceedsPerPaying = OverviewMetric{
			State:      metric.State,
			Definition: metricDefProceedsPerPaying,
			Notes:      payingNotes,
		}
		if metric.State == OverviewStateOK {
			res.Metrics.PayingUsers.Value = uint64Ptr(detail.PayingUsers)
			for _, row := range detail.ByCurrency {
				if row.Currency == detail.Currency && row.Payers > 0 {
					res.Metrics.ProceedsPerPaying.Rate = float64Ptr(float64(row.Net) / float64(row.Payers))
					res.Metrics.ProceedsPerPaying.Notes = []string{fmt.Sprintf(
						"net %d over %d paying people in %s", row.Net, row.Payers, row.Currency)}
					break
				}
			}
			if res.Metrics.ProceedsPerPaying.Rate == nil {
				// Money arrived but nobody booked positively in the headline
				// currency — a real no_data, not a 0.
				res.Metrics.ProceedsPerPaying.State = OverviewStateNoData
				res.Metrics.ProceedsPerPaying.Notes = []string{"no positive booking in the headline currency this window"}
			}
		}
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

	// --- daily trend: active people + raw event volume in one pass ---
	// Events counts every received row (resolved_events is events LEFT JOIN
	// aliases, so count(*) is still one per event); ActiveUsers stays the
	// qualifying-people count. One query, two populations, each labelled.
	{
		byDay := map[string]OverviewTrendPoint{}
		err := s.duckQuery(ctx, `
SELECT CAST(timezone(?, "timestamp") AS DATE) AS day,
	count(DISTINCT `+canonicalID+`) FILTER (WHERE `+overviewQualifying+`) AS users,
	count(DISTINCT session_id) FILTER (WHERE session_id <> '' AND `+overviewQualifying+`) AS sessions,
	count(*) AS events
FROM resolved_events
WHERE `+where+`
GROUP BY day
ORDER BY day`, append([]any{timezone}, args...), func(rows *sql.Rows) error {
			var day time.Time
			var point OverviewTrendPoint
			if err := rows.Scan(&day, &point.ActiveUsers, &point.Sessions, &point.Events); err != nil {
				return err
			}
			point.Day = day.Format("2006-01-02")
			byDay[point.Day] = point
			return nil
		})
		if err != nil {
			return res, err
		}
		// Emit every local calendar day in the range, including zero days — a
		// gap in the series must read as a zero day, not a missing interpolation.
		for d := r.From.In(loc); d.Before(r.To); d = d.AddDate(0, 0, 1) {
			key := d.Format("2006-01-02")
			point := byDay[key]
			point.Day = key
			res.Trend = append(res.Trend, point)
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

	// --- download→paid: the same cohorts, converted by the money grid ---
	{
		paid, err := s.overviewPaidConversion(ctx, projectID, platform, timezone, r.To, moneySeen > 0)
		if err != nil {
			return res, err
		}
		res.PaidConversion = paid
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
		utmSources, err := s.acquisitionBreakdown(ctx, qualWhere, args, "utm_source", "")
		if err != nil {
			return res, err
		}
		res.Content.TopUTMSources = OverviewList{Unit: "pageviews", Rows: utmSources}

		campaigns, err := s.acquisitionBreakdown(ctx, qualWhere, args, "utm_campaign", "")
		if err != nil {
			return res, err
		}
		res.Content.TopCampaigns = OverviewList{Unit: "pageviews", Rows: campaigns}

		referrers, err := s.acquisitionBreakdown(ctx, qualWhere, args, "referrer_host",
			"referrer_channel NOT IN ('', 'direct', 'internal') AND referrer_host IS NOT NULL AND referrer_host <> ''")
		if err != nil {
			return res, err
		}
		res.Content.TopReferrers = OverviewList{Unit: "pageviews", Rows: referrers}

		discoveryRows, err := s.overviewFirstReadDiscovery(ctx, projectID, platform, r.From, r.To, activationEvent)
		if err != nil {
			return res, err
		}
		res.Content.FirstReadDiscovery = OverviewList{Unit: "people", Rows: discoveryRows}
	}

	// --- content: the retired Traffic/Product breakdowns ---
	// These reuse the legacy reads verbatim, scoped to the overview's own
	// window fragment: traffic class and AI-cited pages keep their all-classes
	// population (the non-human rows are the answer), the platform split keeps
	// its per-app pageview count, and top events ranks every received name.
	{
		byClass, err := s.trafficByClass(ctx, where, args)
		if err != nil {
			return res, err
		}
		classRows := make([]PathCount, 0, len(byClass))
		var totalClass, nonHuman uint64
		for _, c := range byClass {
			classRows = append(classRows, PathCount{Value: c.Class, Count: c.Count})
			totalClass += c.Count
			if c.Class != "human" {
				nonHuman += c.Count
			}
		}
		res.Content.TrafficByClass = OverviewList{Unit: "pageviews", Rows: classRows}

		// AI share is the non-human share of classified pageviews — the same
		// population the breakdown above serves, so the headline and the rows
		// can never disagree about the denominator.
		aiState := OverviewStateNoData
		if res.DataStatus.EverReceived && totalClass > 0 {
			aiState = OverviewStateOK
		}
		res.Metrics.AIShare = OverviewMetric{State: aiState, Definition: metricDefAIShare}
		if aiState == OverviewStateOK {
			res.Metrics.AIShare.Rate = float64Ptr(float64(nonHuman) / float64(totalClass) * 100)
		}

		aiPaths, err := s.aiTopPaths(ctx, where, args)
		if err != nil {
			return res, err
		}
		res.Content.AITopPaths = OverviewList{Unit: "pageviews", Rows: aiPaths}

		resolver, err := s.identityResolver(ctx, projectID)
		if err != nil {
			return res, err
		}
		byPlatform, err := s.trafficByPlatform(ctx, resolver, where, args)
		if err != nil {
			return res, err
		}
		platformRows := make([]PathCount, 0, len(byPlatform))
		for _, p := range byPlatform {
			platformRows = append(platformRows, PathCount{Value: p.Platform, Count: p.Pageviews})
		}
		sort.Slice(platformRows, func(i, j int) bool { return platformRows[i].Count > platformRows[j].Count })
		res.Content.TrafficByPlatform = OverviewList{Unit: "pageviews", Rows: platformRows}

		topEvents, err := s.eventCounts(ctx, where, args)
		if err != nil {
			return res, err
		}
		eventRows := make([]PathCount, 0, len(topEvents))
		for _, e := range topEvents {
			eventRows = append(eventRows, PathCount{Value: e.EventName, Count: e.Count})
		}
		res.Content.TopEvents = OverviewList{Unit: "events", Rows: eventRows}
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
	// measured reads whichever channel the metric's honest value lives on —
	// Value for counts, Rate for shares/durations. When the state is not ok
	// the value is ignored anyway (attach serves the target with no verdict),
	// so a nil channel collapses to 0 rather than needing a branch per metric.
	measured := func(m OverviewMetric) float64 {
		if m.Value != nil {
			return float64(*m.Value)
		}
		if m.Rate != nil {
			return *m.Rate
		}
		return 0
	}
	attach(MetricActiveUsers, &res.Metrics.ActiveUsers, "people", measured(res.Metrics.ActiveUsers), "")
	attach(MetricNewUsers, &res.Metrics.NewUsers, "people", measured(res.Metrics.NewUsers), "")
	attach(MetricSessions, &res.Metrics.Sessions, "sessions", measured(res.Metrics.Sessions), "")
	// The retired-surface metrics are judged on the same scale the catalog
	// declares: counts on their own unit, shares on percent, duration in
	// seconds — the same scale read_metric serves.
	attach(MetricPageviews, &res.Metrics.Pageviews, "pageviews", measured(res.Metrics.Pageviews), "")
	attach(MetricConversions, &res.Metrics.Conversions, "events", measured(res.Metrics.Conversions), "")
	attach(MetricSessionsPerUser, &res.Metrics.SessionsPerUser, "sessions/person", measured(res.Metrics.SessionsPerUser), "")
	attach(MetricPayingUsers, &res.Metrics.PayingUsers, "people", measured(res.Metrics.PayingUsers), "")
	attach(MetricAIShare, &res.Metrics.AIShare, "percent", measured(res.Metrics.AIShare), "")
	attach(MetricBounceRate, &res.Metrics.BounceRate, "percent", measured(res.Metrics.BounceRate), "")
	attach(MetricAvgSession, &res.Metrics.AvgSessionDuration, "seconds", measured(res.Metrics.AvgSessionDuration), "")
	// Activation is a percent metric with no measured value yet — the target
	// still serves (unconfigured), just never a verdict.
	attach(MetricActivation, &res.Metrics.Activation, "percent", 0, "")
	// Revenue is judged on the signed net in the headline currency — the
	// unsigned headline Value cannot express a net reversal.
	if d := res.Metrics.RevenueDetail; d != nil {
		attach(MetricRevenue, &res.Metrics.Revenue, "currency", float64(d.Net), d.Currency)
		// Proceeds per paying person is judged in the headline currency's
		// smallest unit — the same scale the currency unit declares.
		attach(MetricProceedsPerPaying, &res.Metrics.ProceedsPerPaying, "currency", measured(res.Metrics.ProceedsPerPaying), d.Currency)
	} else {
		attach(MetricRevenue, &res.Metrics.Revenue, "currency", 0, "")
		attach(MetricProceedsPerPaying, &res.Metrics.ProceedsPerPaying, "currency", 0, "")
	}
	// Retention rates are judged on the percent scale the catalog declares —
	// the same scale read_metric serves — never the 0-1 fraction.
	for key, point := range map[string]*OverviewRetentionPoint{
		MetricRetentionD1:       &res.Retention.D1,
		MetricRetentionD7:       &res.Retention.D7,
		MetricRetentionD30:      &res.Retention.D30,
		MetricDownloadToPaidD1:  &res.PaidConversion.D1,
		MetricDownloadToPaidD7:  &res.PaidConversion.D7,
		MetricDownloadToPaidD35: &res.PaidConversion.D35,
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
		arg_min(coalesce(platform, ''), ("timestamp", event_id)) AS first_platform
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

// overviewPaidConversion computes App Store Connect's download→paid read:
// the share of a lifetime first-activity cohort that made a deduplicated
// positive revenue booking within N local days of first activity. Cohorts
// are the same firsts subquery retention uses; the conversion event is the
// money grid's positive-booking predicate, so "paid" can never disagree with
// the revenue tile. instrumented is the lifetime money signal — a project
// that never sent a money row reports unconfigured, not not_ready.
func (s *Store) overviewPaidConversion(ctx context.Context, projectID, platform, timezone string, to time.Time, instrumented bool) (OverviewPaidConversion, error) {
	out := OverviewPaidConversion{CohortWindow: "lifetime"}
	if !instrumented {
		out.D1 = OverviewRetentionPoint{State: OverviewStateUnconfigured}
		out.D7 = OverviewRetentionPoint{State: OverviewStateUnconfigured}
		out.D35 = OverviewRetentionPoint{State: OverviewStateUnconfigured}
		return out, nil
	}
	platClause, platArg := overviewPlatform(platform)
	firstClause := firstPlatformClause(platform)
	canonicalID := "canonical_distinct_id"

	firsts := `
SELECT cid, CAST(timezone(?, first_ts) AS DATE) AS cohort_day
FROM (
	SELECT ` + canonicalID + ` AS cid, min("timestamp") AS first_ts,
		arg_min(coalesce(platform, ''), ("timestamp", event_id)) AS first_platform
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
	// by `to` — the same per-cohort-day maturity retention uses.
	var elig [3]uint64
	err := s.duckQueryRow(ctx, `
SELECT
	count(*) FILTER (WHERE cohort_day + INTERVAL '2 days' <= CAST(timezone(?, ?) AS DATE)),
	count(*) FILTER (WHERE cohort_day + INTERVAL '8 days' <= CAST(timezone(?, ?) AS DATE)),
	count(*) FILTER (WHERE cohort_day + INTERVAL '36 days' <= CAST(timezone(?, ?) AS DATE))
FROM (`+firsts+`)`, append([]any{timezone, to, timezone, to, timezone, to}, firstArgs...), &elig[0], &elig[1], &elig[2])
	if err != nil {
		return out, err
	}

	// paid_N = eligible members with a deduplicated positive booking on or
	// before cohort_day + N. The money grid is scoped by project and platform
	// only — the window is the cohort's own day range, not the read's range.
	var paid [3]uint64
	moneyWhere := "project_id = ?" + platClause
	moneyArgs := append([]any{projectID}, platformArgs(platArg)...)
	err = s.duckQueryRow(ctx, `
WITH `+moneyRowsCTE(moneyWhere)+`
SELECT
	count(DISTINCT m.person_id) FILTER (WHERE CAST(timezone(?, m.occurred_at) AS DATE) <= f.cohort_day + INTERVAL '1 day' AND f.cohort_day + INTERVAL '2 days' <= CAST(timezone(?, ?) AS DATE)),
	count(DISTINCT m.person_id) FILTER (WHERE CAST(timezone(?, m.occurred_at) AS DATE) <= f.cohort_day + INTERVAL '7 days' AND f.cohort_day + INTERVAL '8 days' <= CAST(timezone(?, ?) AS DATE)),
	count(DISTINCT m.person_id) FILTER (WHERE CAST(timezone(?, m.occurred_at) AS DATE) <= f.cohort_day + INTERVAL '35 days' AND f.cohort_day + INTERVAL '36 days' <= CAST(timezone(?, ?) AS DATE))
FROM money_rows m
INNER JOIN (`+firsts+`) f ON m.person_id = f.cid
WHERE m.write_rank = 1
  AND m.event_name = '`+moneyBookingEvent+`'
  AND NOT `+moneyReverses+`
  AND m.amount > 0
  AND m.currency <> ''
  AND m.currency <> '`+moneyNonCurrency+`'
  AND CAST(timezone(?, m.occurred_at) AS DATE) >= f.cohort_day`,
		append(append(moneyArgs, timezone, timezone, to, timezone, timezone, to, timezone, timezone, to), append(firstArgs, timezone)...),
		&paid[0], &paid[1], &paid[2])
	if err != nil {
		return out, err
	}

	out.D1 = overviewRetentionPoint(elig[0], paid[0])
	out.D7 = overviewRetentionPoint(elig[1], paid[1])
	out.D35 = overviewRetentionPoint(elig[2], paid[2])
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
		arg_min(coalesce(platform, ''), ("timestamp", event_id)) AS first_platform
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

// overviewFirstReadDiscovery classifies the human pageview immediately preceding
// each person's first activation event (e.g. chapter_view) into its discovery
// surface: home, search, direct, or communication.
//
// The cohort is people whose lifetime first activation event occurred within the
// selected analysis range [from, to). For each qualifying person, we find the
// latest human user.pageview event that occurred before (or at) that first activation.
// If no preceding pageview exists, the entry is classified as "direct". Otherwise,
// the preceding pageview's path determines the discovery surface:
//   - "/" -> "home"
//   - contains "tim-kiem" or "search" -> "search"
//   - starts with "/community", "/messages", "/messenger", or "/s/" -> "communication"
//   - other paths -> "direct" (or "other" if distinct from direct)
//
// To ensure the 4 surfaces requested (home, search, direct, communication) are
// always present and comparable, any surface with 0 users is populated with 0.
func (s *Store) overviewFirstReadDiscovery(ctx context.Context, projectID, platform string, from, to time.Time, activationEvent string) ([]PathCount, error) {
	activationEvent = strings.TrimSpace(activationEvent)
	if activationEvent == "" {
		return []PathCount{}, nil
	}

	platClause, platArg := overviewPlatform(platform)
	args := []any{projectID, activationEvent, from, to}
	if platArg != nil {
		args = append(args, platArg)
	}

	// Find each person's first activation event in the project, restricted to
	// those whose first activation falls within [from, to).
	query := `
WITH first_activations AS (
	SELECT
		canonical_distinct_id AS person_id,
		min("timestamp") AS first_act_ts
	FROM resolved_events
	WHERE project_id = ?
	  AND event_name = ?
	  AND coalesce(visitor_class, 'human') = 'human'` + platClause + `
	GROUP BY canonical_distinct_id
	HAVING min("timestamp") >= ? AND min("timestamp") < ?
),
preceding_pvs AS (
	SELECT
		fa.person_id,
		coalesce(json_extract_string(e.properties, '$.path'), '') AS pv_path,
		e."timestamp" AS pv_ts,
		row_number() OVER (PARTITION BY fa.person_id ORDER BY e."timestamp" DESC, e.event_id DESC) AS rn
	FROM first_activations fa
	INNER JOIN resolved_events e
		ON e.project_id = ?
	   AND e.canonical_distinct_id = fa.person_id
	   AND e.event_name = 'user.pageview'
	   AND coalesce(e.visitor_class, 'human') = 'human'
	   AND e."timestamp" <= fa.first_act_ts
),
attributed AS (
	SELECT
		fa.person_id,
		CASE
			WHEN p.pv_path IS NULL OR p.pv_path = '' THEN 'direct'
			WHEN p.pv_path = '/' THEN 'home'
			WHEN p.pv_path LIKE '%tim-kiem%' OR p.pv_path LIKE '%search%' THEN 'search'
			WHEN p.pv_path LIKE '/community%' OR p.pv_path LIKE '/messages%' OR p.pv_path LIKE '/messenger%' OR p.pv_path LIKE '/s/%' THEN 'communication'
			ELSE 'direct'
		END AS surface
	FROM first_activations fa
	LEFT JOIN preceding_pvs p
		ON p.person_id = fa.person_id AND p.rn = 1
)
SELECT surface, count(*) AS count
FROM attributed
GROUP BY surface
ORDER BY count DESC, surface ASC`

	fullArgs := append(args, projectID)
	surfaceCounts := map[string]uint64{
		"home":          0,
		"search":        0,
		"direct":        0,
		"communication": 0,
	}

	var hasRows bool
	err := s.duckQuery(ctx, query, fullArgs, func(rows *sql.Rows) error {
		hasRows = true
		var surface string
		var count uint64
		if err := rows.Scan(&surface, &count); err != nil {
			return err
		}
		surfaceCounts[surface] = count
		return nil
	})
	if err != nil {
		return nil, err
	}
	if !hasRows {
		return []PathCount{}, nil
	}

	// Return in fixed, canonical order: home, search, direct, communication
	// or ordered by count DESC with fixed ties.
	orderedSurfaces := []string{"home", "search", "direct", "communication"}
	slices.SortFunc(orderedSurfaces, func(a, b string) int {
		ca, cb := surfaceCounts[a], surfaceCounts[b]
		if ca != cb {
			if cb > ca {
				return 1
			}
			return -1
		}
		return strings.Compare(a, b)
	})

	result := make([]PathCount, 0, len(orderedSurfaces))
	for _, s := range orderedSurfaces {
		result = append(result, PathCount{
			Value: s,
			Count: surfaceCounts[s],
		})
	}
	return result, nil
}
