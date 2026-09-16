package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"math/big"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/duckdb/duckdb-go/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lohi-ai/agentray/internal/shared/config"
)

type Store struct {
	pg *pgxpool.Pool
	// duck is the embedded analytics engine: the event log, alias mirror,
	// person profiles, and connector landing rows. One instance per Store,
	// opened in Open and closed in Close (or CloseDuckDB during shutdown).
	duck *DuckDB
	// sandboxes owns the per-project in-memory DuckDB instances that untrusted
	// SQL (run_sql, saved queries, charts, alerts) executes against. See
	// duckdb_sandbox.go.
	sandboxes *sqlSandboxPool
	resolvers *resolverCache

	// hostModel is the optional hosted default pool. Workspaces without a BYOK
	// key inherit it so the first ask works. Zero-value (empty APIKey) = off.
	hostModel HostModelDefaults

	// The one shared demo (config.DemoProjectID): a REAL project fed by a real
	// site, that every account is added to as a read-only viewer. Both empty
	// means this instance has no demo — the common case, because a self-hosted
	// `docker compose up` operator never sets AGENTRAY_DEMO_PROJECT_ID. Resolved
	// once at boot by migrateDemoWorkspace; read on every signup and on every
	// workspace/project list, so it must not cost a query.
	demoProjectID   string
	demoWorkspaceID string
}

// resolverCache memoizes the per-project identity resolver. Canonical-id
// stitching itself now runs in DuckDB through the resolved_events view (a
// LEFT JOIN on the aliases mirror), but person-scoped filters still need the
// in-memory alias pairs (relatedDistinctIDs). Without this cache every
// analytics call re-reads the whole alias table from Postgres — a hot-path
// round trip an agent firing many queries pays repeatedly. A short TTL bounds
// staleness; new aliases also invalidate the entry explicitly on write, so a
// freshly-identified user stitches immediately for filtering.
type resolverCache struct {
	mu      sync.Mutex
	ttl     time.Duration
	now     func() time.Time
	entries map[string]cachedResolver
}

type cachedResolver struct {
	resolver identityResolver
	expires  time.Time
}

func newResolverCache(ttl time.Duration) *resolverCache {
	return &resolverCache{ttl: ttl, now: time.Now, entries: map[string]cachedResolver{}}
}

func (c *resolverCache) get(projectID string) (identityResolver, bool) {
	if c == nil {
		return identityResolver{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[projectID]
	if !ok || c.now().After(e.expires) {
		return identityResolver{}, false
	}
	return e.resolver, true
}

func (c *resolverCache) put(projectID string, r identityResolver) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[projectID] = cachedResolver{resolver: r, expires: c.now().Add(c.ttl)}
}

func (c *resolverCache) invalidate(projectID string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, projectID)
}

type Project struct {
	ID          string `json:"id"`
	WorkspaceID string `json:"workspace_id,omitempty"`
	Name        string `json:"name"`
	// Timezone is a validated IANA name when set. Empty means an existing
	// nullable row, which Overview reports as its explicit UTC fallback.
	Timezone  string    `json:"timezone,omitempty"`
	// Goal is the owner's answer to "what are you trying to improve?" —
	// activation | retention | revenue | traffic | skipped. A nil Goal means
	// the prompt was never answered (the column is NULL); "skipped" means the
	// owner declined, so the prompt must not reappear. ActivationEvent names
	// the catalog event the owner says counts as "activated"; empty means
	// unset, and it is what the activation overview metric computes against.
	Goal            *string   `json:"goal,omitempty"`
	ActivationEvent string    `json:"activation_event,omitempty"`
	APIKey    string    `json:"api_key"`
	CreatedAt time.Time `json:"created_at"`
	// Role is the requesting user's role in the owning workspace, and IsDemo
	// says the project lives in the shared demo workspace (see demo.go). Both
	// are additive read-only truth for the UI: without them it cannot tell a
	// project the viewer owns from a live demo of someone else's site, and it
	// would offer write affordances that the API will refuse. Empty/false on the
	// api-key path, which has no user to have a role.
	Role   string `json:"role,omitempty"`
	IsDemo bool   `json:"is_demo,omitempty"`
}

type Dashboard struct {
	ID          string     `json:"id"`
	ProjectID   string     `json:"project_id"`
	Name        string     `json:"name"`
	Description string     `json:"description"`
	Revision    int64      `json:"revision"`
	ArchivedAt  *time.Time `json:"archived_at,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
	// BoardKey is the stable name a declaration addresses this board by. It is
	// empty for boards created positionally (create_dashboard) — a key is
	// something a declaration chooses, never something the store invents.
	BoardKey string `json:"board_key,omitempty"`
	// DefinitionUpdatedAt is when the board's declared content was last
	// written. Nil means the board was never declared: it renders from its
	// charts, which is how every board behaved before declarations existed.
	DefinitionUpdatedAt *time.Time `json:"definition_updated_at,omitempty"`
}

// HasDefinition reports whether the board carries a declared composition.
func (d Dashboard) HasDefinition() bool { return d.DefinitionUpdatedAt != nil }

type Chart struct {
	ID          string `json:"id"`
	DashboardID string `json:"dashboard_id"`
	ProjectID   string `json:"project_id"`
	Name        string `json:"name"`
	Kind        string `json:"kind"`
	Metric      string `json:"metric"`
	EventName   string `json:"event_name"`
	EventType   string `json:"event_type"`
	SQL         string `json:"sql"`
	XField      string `json:"x_field"`
	YField      string `json:"y_field"`
	SortOrder   int    `json:"sort_order"`
	ColSpan     int    `json:"col_span"`
	// Revision is the per-chart optimistic-concurrency counter update_chart
	// and archive_chart carry — same contract as dashboards. Board order is
	// fenced by the DASHBOARD's revision (reorder_charts), not this one.
	Revision int64 `json:"revision"`
	// ArchivedAt marks a soft-archived chart — reversible, the row is kept.
	ArchivedAt *time.Time `json:"archived_at,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	UpdatedAt  time.Time  `json:"updated_at"`
}

type Event struct {
	ProjectID       string     `json:"project_id"`
	EventID         string     `json:"event_id"`
	DistinctID      string     `json:"distinct_id"`
	SessionID       string     `json:"session_id"`
	EventName       string     `json:"event_name"`
	EventType       string     `json:"event_type"`
	Properties      string     `json:"properties"`
	AgentID         string     `json:"agent_id,omitempty"`
	ToolName        string     `json:"tool_name,omitempty"`
	ToolInput       string     `json:"tool_input,omitempty"`
	ToolOutput      string     `json:"tool_output,omitempty"`
	TokensInput     *uint32    `json:"tokens_input,omitempty"`
	TokensOutput    *uint32    `json:"tokens_output,omitempty"`
	CostUSD         *float32   `json:"cost_usd,omitempty"`
	LatencyMS       *uint32    `json:"latency_ms,omitempty"`
	ModelName       string     `json:"model_name,omitempty"`
	IsError         bool       `json:"is_error"`
	ErrorMessage    string     `json:"error_message,omitempty"`
	Timestamp       time.Time  `json:"timestamp"`
	InsertedAt      *time.Time `json:"inserted_at,omitempty"`
	VisitorClass    string     `json:"visitor_class,omitempty"`
	BotName         string     `json:"bot_name,omitempty"`
	ReferrerHost    string     `json:"referrer_host,omitempty"`
	ReferrerChannel string     `json:"referrer_channel,omitempty"`
	UserAgent       string     `json:"user_agent,omitempty"`
	// Platform is which app the event came from — web / ios / android / server.
	// A product that ships a site and a native app sends both through one project
	// key, so without this column every visitor, funnel and retention number is
	// two audiences added together. Empty means undetermined; it is rendered as
	// "unknown" rather than folded into web.
	Platform string `json:"platform,omitempty"`
	// InsertID is the caller-supplied idempotency key ($insert_id). Every money
	// read de-duplicates on it — `coalesce(nullif(insert_id, ''), event_id)` in
	// money.go's grid, keeping the greatest `(timestamp, event_id)` per key — so
	// a retried webhook books once and a correction under the same key replaces
	// the row it fixes. The retention "ever paid" flag stays duplicate-safe by
	// construction (it aggregates the paid flag with max()). Cost/token *sums* in
	// the daily rollups and raw agent reads are still not de-duped; the pipeline
	// keeps the practical duplicate rate near zero (ack-after-insert + the
	// JetStream duplicate window), which the data-architecture doc explicitly
	// accepts for count/sum metrics. Do not describe a de-dup guard here that the
	// code does not implement.
	InsertID string `json:"insert_id,omitempty"`
	// IsUnplanned marks an event whose name was not in the project's established
	// catalog when captured (P4 tracking-plan signal). Advisory only.
	IsUnplanned bool `json:"is_unplanned,omitempty"`
}

type Session struct {
	ProjectID      string     `json:"project_id"`
	SessionID      string     `json:"session_id"`
	DistinctID     string     `json:"distinct_id"`
	SessionStart   time.Time  `json:"session_start"`
	SessionEnd     time.Time  `json:"session_end"`
	EventCount     uint64     `json:"event_count"`
	TotalTokensIn  uint64     `json:"total_tokens_in"`
	TotalTokensOut uint64     `json:"total_tokens_out"`
	TotalCostUSD   float64    `json:"total_cost_usd"`
	LastEventAt    *time.Time `json:"last_event_at,omitempty"`
}

type ActivitySummary struct {
	ProjectID       string            `json:"project_id"`
	EventCount      uint64            `json:"event_count"`
	UserEvents      uint64            `json:"user_events"`
	AgentEvents     uint64            `json:"agent_events"`
	SystemEvents    uint64            `json:"system_events"`
	Sessions        uint64            `json:"sessions"`
	DistinctUsers   uint64            `json:"distinct_users"`
	TotalTokensIn   uint64            `json:"total_tokens_in"`
	TotalTokensOut  uint64            `json:"total_tokens_out"`
	TotalCostUSD    float64           `json:"total_cost_usd"`
	EventCounts     []EventCount      `json:"event_counts"`
	Timeline        []TimelinePoint   `json:"timeline"`
	TopAgents       []AgentMetric     `json:"top_agents"`
	RecentEvents    []Event           `json:"recent_events"`
	RecentSessions  []Session         `json:"recent_sessions"`
	GeneratedAt     time.Time         `json:"generated_at"`
	EventsByType    map[string]uint64 `json:"events_by_type"`
	EmptySinceHours int               `json:"empty_since_hours"`
	// Platforms is every app that sent an event in this window ('unknown' for
	// rows that carry no platform). It is deliberately computed *ignoring* the
	// platform filter, so the facet it feeds still lists the other apps once one
	// is selected — a filter you cannot switch back out of is a trap.
	Platforms []string `json:"platforms"`
}

type EventCount struct {
	EventName string `json:"event_name"`
	Count     uint64 `json:"count"`
}

// EventCatalogEntry is one distinct event name in a project, with enough context
// (type, volume, last-seen) for a person to recognise the name they half-remember
// in an autocomplete. The catalog spans all history, not the active time window,
// so picking from it never hides a name just because it was quiet lately.
type EventCatalogEntry struct {
	EventName string `json:"event_name"`
	EventType string `json:"event_type"`
	// Count is event volume; Users is how many stitched, human identities are
	// behind it. They are wildly different numbers — one person can fire a
	// pageview two hundred times — and only Users may be labelled "people".
	// The first-run written opinion reads this catalog, so shipping only Count
	// is what let the headline print event volume under a "people" noun.
	Count    uint64    `json:"count"`
	Users    uint64    `json:"users"`
	LastSeen time.Time `json:"last_seen"`
}

type TimelinePoint struct {
	Hour  time.Time `json:"hour"`
	Count uint64    `json:"count"`
}

type AgentMetric struct {
	AgentID      string  `json:"agent_id"`
	EventCount   uint64  `json:"event_count"`
	TotalCostUSD float64 `json:"total_cost_usd"`
	AvgLatencyMS float64 `json:"avg_latency_ms"`
}

type EventFilter struct {
	From       time.Time `json:"from"`
	To         time.Time `json:"to"`
	EventType  string    `json:"event_type"`
	EventName  string    `json:"event_name"`
	DistinctID string    `json:"distinct_id"`
	SessionID  string    `json:"session_id"`
	AgentID    string    `json:"agent_id"`
	ModelName  string    `json:"model_name"`
	ErrorOnly  bool      `json:"error_only"`
	Search     string    `json:"search"`
	Limit      int       `json:"limit"`
	// HumansOnly drops search-bot and AI-platform crawler traffic
	// (visitor_class != 'human'). Set it for any metric that counts *people* —
	// acquisition, funnel, retention, persons — so a Googlebot or GPTBot crawl
	// is not mistaken for a user. Leave it off for operational/event-volume and
	// agent-cost metrics, which legitimately include non-human rows.
	HumansOnly bool `json:"humans_only"`
	// Platform scopes every metric to one app (web / ios / android / server, or
	// 'unknown' for rows that predate the column or came from a client that says
	// nothing). This is what lets one project answer "does the iOS app convert
	// worse than the site" without a hand-written query.
	Platform string `json:"platform"`
}

type InsightResult struct {
	Type      string           `json:"type"`
	Title     string           `json:"title"`
	Metric    string           `json:"metric"`
	Series    []TimelinePoint  `json:"series"`
	Rows      []map[string]any `json:"rows"`
	Funnel    []FunnelStep     `json:"funnel"`
	Retention []RetentionPoint `json:"retention"`
	Generated time.Time        `json:"generated_at"`
}

type FunnelStep struct {
	Step       int     `json:"step"`
	EventName  string  `json:"event_name"`
	Users      uint64  `json:"users"`
	Conversion float64 `json:"conversion"`
}

type RetentionPoint struct {
	Period string  `json:"period"`
	Users  uint64  `json:"users"`
	Rate   float64 `json:"rate"`
	// Mature is false when this period's window has not finished elapsing, so a
	// 0 here means "nobody could have returned yet", not "nobody returned". The
	// curve used to emit those as a flat 0% and the headline averaged them into
	// an "Avg retention" figure — on the default 24-hour range every weekly
	// period is in the future, so the product reported a precise-looking number
	// for a question the data cannot answer. Renderers must not chart, average,
	// or colour an immature point.
	Mature bool `json:"mature"`
}

// CohortCell is one square in the retention triangle: the audience of a cohort
// that was still active `Period` weeks after acquisition, both as a head count
// and as a share of the cohort's week-0 size.
type CohortCell struct {
	Period int     `json:"period"`
	Users  uint64  `json:"users"`
	Rate   float64 `json:"rate"`
}

// CohortRow is one acquisition cohort — everyone whose first event landed in the
// ISO week beginning CohortStart — with its retention curve across the periods.
type CohortRow struct {
	Cohort      string       `json:"cohort"`
	CohortStart time.Time    `json:"cohort_start"`
	Size        uint64       `json:"size"`
	Cells       []CohortCell `json:"cells"`
}

// AudienceOption is one selectable cohort audience (the segment toggle's items),
// surfaced so the UI renders the available segments from server truth instead of
// a hardcoded list that could drift from audienceSegments.
type AudienceOption struct {
	Key   string `json:"key"`
	Label string `json:"label"`
}

// CohortAnalysis is the cohort retention surface: weekly acquisition cohorts
// (rows) by weeks-since-acquisition (columns), scoped to one audience Segment
// (see audienceSegments — identity, paid, premium, …). Audiences lists every
// selectable segment so the client can build the toggle without duplicating the
// catalog.
type CohortAnalysis struct {
	Segment   string           `json:"segment"`
	Periods   int              `json:"periods"`
	Audiences []AudienceOption `json:"audiences"`
	Rows      []CohortRow      `json:"rows"`
	Generated time.Time        `json:"generated_at"`
}

// ProjectAudience is a user-defined cohort audience scoped to one project. It is
// a structured rule, not raw SQL: Kind is "paid" (anyone who ever paid) or
// "plan" (Plans lists the matching `plan` values). compilePredicate turns it
// into a safe DuckDB boolean — the single place a custom audience becomes
// SQL — so projects can add their own paid/premium-style groups (the planned
// external-DB source plugs in at the same per-person attribute layer).
type ProjectAudience struct {
	ID        string    `json:"id"`
	ProjectID string    `json:"project_id"`
	Key       string    `json:"key"`
	Label     string    `json:"label"`
	Kind      string    `json:"kind"`
	Plans     []string  `json:"plans"`
	CreatedAt time.Time `json:"created_at"`
}

// cohortAudienceOptionsFrom projects a resolved audience-segment list (built-ins
// plus a project's customs) to the wire shape the toggle renders.
func cohortAudienceOptionsFrom(segments []audienceSegment) []AudienceOption {
	out := make([]AudienceOption, len(segments))
	for i, a := range segments {
		out[i] = AudienceOption{Key: a.Key, Label: a.Label}
	}
	return out
}

type TrafficClass struct {
	Class string `json:"class"`
	Count uint64 `json:"count"`
}

type TrafficProvider struct {
	Class     string `json:"class"`
	Provider  string `json:"provider"`
	Visitors  uint64 `json:"visitors"`
	Pageviews uint64 `json:"pageviews"`
}

// PlatformSplit is one app's share of the audience. Visitors is the count that
// answers "how many people", Pageviews the count that answers "how much did they
// look at" — kept separate because a single "count" behind a Visitors header is
// exactly the ambiguity this split exists to remove.
type PlatformSplit struct {
	Platform  string `json:"platform"`
	Visitors  uint64 `json:"visitors"`
	Pageviews uint64 `json:"pageviews"`
	Events    uint64 `json:"events"`
}

type GuestUser struct {
	Guests uint64 `json:"guests"`
	Users  uint64 `json:"users"`
}

type WebAnalytics struct {
	Visitors           uint64            `json:"visitors"`
	Pageviews          uint64            `json:"pageviews"`
	Sessions           uint64            `json:"sessions"`
	Conversions        uint64            `json:"conversions"`
	AvgSessionDuration float64           `json:"avg_session_duration_seconds"`
	BounceRate         float64           `json:"bounce_rate"`
	TopPaths           []PathCount       `json:"top_paths"`
	Referrers          []PathCount       `json:"referrers"`
	TrafficByClass     []TrafficClass    `json:"traffic_by_class"`
	TrafficByPlatform  []PlatformSplit   `json:"traffic_by_platform"`
	TrafficByProvider  []TrafficProvider `json:"traffic_by_provider"`
	AITopPaths         []PathCount       `json:"ai_top_paths"`
	ReferrersByChannel []PathCount       `json:"referrers_by_channel"`
	GuestVsUser        GuestUser         `json:"guest_vs_user"`
	Generated          time.Time         `json:"generated_at"`
}

type PathCount struct {
	Value string `json:"value"`
	Count uint64 `json:"count"`
}

type Person struct {
	DistinctID    string    `json:"distinct_id"`
	Email         string    `json:"email"`
	Name          string    `json:"name"`
	FirstSeen     time.Time `json:"first_seen"`
	LastSeen      time.Time `json:"last_seen"`
	EventCount    uint64    `json:"event_count"`
	Sessions      uint64    `json:"sessions"`
	LastEventName string    `json:"last_event_name"`
	// Platforms is every app this person was seen in, sorted. A reader who used
	// the website and then the app is one row with two platforms — which is the
	// only place identify()/alias stitching is visible as a fact rather than as a
	// number that happens to be smaller than the sum of its parts.
	Platforms []string `json:"platforms"`
	// Traits are the merged $set / $set_once person properties from the profile
	// store (P3). Nil when the profile store has no row yet for this person.
	Traits map[string]json.RawMessage `json:"traits,omitempty"`
}

type PersonsSummary struct {
	Total          uint64          `json:"total"`
	Identified     uint64          `json:"identified"`
	Anonymous      uint64          `json:"anonymous"`
	ActiveTimeline []TimelinePoint `json:"active_timeline"`
	Persons        []Person        `json:"persons"`
	Generated      time.Time       `json:"generated_at"`
}

type Alias struct {
	ProjectID   string    `json:"project_id"`
	AnonymousID string    `json:"anonymous_id"`
	CanonicalID string    `json:"canonical_id"`
	CreatedAt   time.Time `json:"created_at"`
}

type identityResolver struct {
	anonymousIDs []string
	canonicalIDs []string
}

type EventExplorer struct {
	// Schema is the catalog the sample is drawn from, and it is the field an
	// agent actually reads. Events is up to 500 raw rows: enough to see what a
	// payload looks like, and nothing you can safely conclude a shape from — an
	// event that fires once a week is not in a 100-row sample of a busy project.
	// Without Schema every analysis run opened by rebuilding it in SQL (SELECT
	// DISTINCT event_name, SELECT DISTINCT properties, min/max(timestamp)),
	// spending a third of its turn budget rediscovering what one GROUP BY knows.
	Schema    []EventSchemaEntry `json:"schema"`
	Events    []Event            `json:"events"`
	Timeline  []Event            `json:"timeline"`
	Generated time.Time          `json:"generated_at"`
}

// EventSchemaEntry is one event name as the store knows it: how much of it there
// is, how many people are behind it, the span it covers, and the property keys
// it carries.
type EventSchemaEntry struct {
	EventName string `json:"event_name"`
	EventType string `json:"event_type"`
	// Events is volume; People is stitched, human identities. Only People may be
	// reported to anyone as a count of anybody — see EventCatalogEntry.
	Events uint64 `json:"events"`
	People uint64 `json:"people"`
	// FirstSeen/LastSeen bound this event inside the window that was explored,
	// which is not the same as the life of the project: an agent reading a 24h
	// window must not conclude the product launched yesterday.
	FirstSeen time.Time `json:"first_seen"`
	LastSeen  time.Time `json:"last_seen"`
	// PropertyKeys is the union of JSON keys on this event's properties, sorted
	// and capped. This is the part that stops an agent guessing property names.
	PropertyKeys []string `json:"property_keys"`
}

type AgentReplay struct {
	SessionID      string  `json:"session_id"`
	DistinctID     string  `json:"distinct_id"`
	EventCount     uint64  `json:"event_count"`
	TotalTokensIn  uint64  `json:"total_tokens_in"`
	TotalTokensOut uint64  `json:"total_tokens_out"`
	TotalCostUSD   float64 `json:"total_cost_usd"`
	Events         []Event `json:"events"`
}

type SavedQuery struct {
	ID              string          `json:"id"`
	ProjectID       string          `json:"project_id"`
	NaturalLanguage string          `json:"natural_language"`
	GeneratedSQL    string          `json:"generated_sql"`
	Verified        bool            `json:"verified"`
	ResultCache     json.RawMessage `json:"result_cache,omitempty"`
	CreatedAt       time.Time       `json:"created_at"`
}

type SavedQueryResult struct {
	Query     SavedQuery       `json:"query"`
	Rows      []map[string]any `json:"rows"`
	Generated time.Time        `json:"generated_at"`
}

// Well-known UUIDs for the four system templates — stable across restarts.
const (
	ProductOverviewTemplateID = "00000000-0001-0001-0001-000000000001"
	AIAgentOpsTemplateID      = "00000000-0001-0001-0001-000000000002"
	ProductActivityTemplateID = "00000000-0001-0001-0001-000000000003"
	CostControlTemplateID     = "00000000-0001-0001-0001-000000000004"
	GrowthRetentionTemplateID = "00000000-0001-0001-0001-000000000005"
	MarketingFunnelTemplateID = "00000000-0001-0001-0001-000000000006"
)

// DashboardTemplate is a reusable board preset stored in Postgres.
type DashboardTemplate struct {
	ID          string          `json:"id"`
	ProjectID   *string         `json:"project_id"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	IsSystem    bool            `json:"is_system"`
	Charts      []TemplateChart `json:"charts"`
	CreatedAt   time.Time       `json:"created_at"`
	UpdatedAt   time.Time       `json:"updated_at"`
}

// TemplateChart is a chart definition within a DashboardTemplate.
type TemplateChart struct {
	ID         string `json:"id"`
	TemplateID string `json:"template_id"`
	Name       string `json:"name"`
	Kind       string `json:"kind"`
	Metric     string `json:"metric"`
	EventName  string `json:"event_name"`
	EventType  string `json:"event_type"`
	SQL        string `json:"sql"`
	XField     string `json:"x_field"`
	YField     string `json:"y_field"`
	SortOrder  int    `json:"sort_order"`
}

func Open(ctx context.Context, cfg config.Config) (*Store, error) {
	pgCfg, err := pgxpool.ParseConfig(cfg.PostgresURL)
	if err != nil {
		return nil, err
	}
	// Bound the pool against the shared docai-db instance (100-connection budget
	// split across api, tts-api, translate-api, and the cli). pgxpool otherwise
	// defaults to max(4, numCPU) with no lifetime recycling, and no per-statement
	// cap — so a stuck query would pin a connection indefinitely.
	pgCfg.MaxConns = 6
	pgCfg.MaxConnIdleTime = 30 * time.Second
	pgCfg.MaxConnLifetime = 30 * time.Minute
	if pgCfg.ConnConfig.RuntimeParams == nil {
		pgCfg.ConnConfig.RuntimeParams = map[string]string{}
	}
	// Default the per-statement cap only when the DSN didn't set one
	// (`?options=-c statement_timeout=...` / `statement_timeout=` in the URL),
	// so an operator can override it without a code change.
	if _, ok := pgCfg.ConnConfig.RuntimeParams["statement_timeout"]; !ok {
		pgCfg.ConnConfig.RuntimeParams["statement_timeout"] = "15000"
	}
	pg, err := pgxpool.NewWithConfig(ctx, pgCfg)
	if err != nil {
		return nil, err
	}
	// The embedded analytics engine: one DuckDB file owns the event log,
	// alias mirror, person profiles, and
	// connector landing rows. Opened before the JetStream worker starts so the
	// first consumed batch always has a durable place to land.
	duck, err := OpenDuckDB(ctx, cfg.DuckDBPath)
	if err != nil {
		pg.Close()
		return nil, err
	}
	store := &Store{
		pg:        pg,
		duck:      duck,
		sandboxes: newSQLSandboxPool(duck),
		resolvers: newResolverCache(30 * time.Second),
		hostModel: HostModelDefaultsFromConfig(cfg),
	}
	// Migrations run on their own single-connection pool with the per-statement
	// cap lifted: DDL and one-time backfills on grown production tables can
	// legitimately exceed the 15s runtime cap, and aborting boot mid-migration
	// is strictly worse than a slow one. The runtime pool above keeps its cap.
	migCfg := pgCfg.Copy()
	migCfg.MaxConns = 1
	migCfg.ConnConfig.RuntimeParams["statement_timeout"] = "0"
	migPG, err := pgxpool.NewWithConfig(ctx, migCfg)
	if err != nil {
		store.Close()
		return nil, err
	}
	store.pg = migPG
	err = store.migrate(ctx, cfg)
	migPG.Close()
	store.pg = pg
	if err != nil {
		store.Close()
		return nil, err
	}
	// Reconcile the DuckDB alias mirror against the Postgres source of truth.
	// Non-fatal: CreateAlias upserts keep it current going forward, and a
	// failure here only means historical stitches wait for the next boot —
	// never a startup blocker.
	if err := store.reconcileAliases(ctx); err != nil {
		fmt.Printf("warn: reconcileAliases: %v\n", err)
	}
	if err := store.SeedSystemTemplates(ctx); err != nil {
		// Non-fatal: Templates page shows EmptyState on failure.
		fmt.Printf("warn: SeedSystemTemplates: %v\n", err)
	}
	return store, nil
}

// CloseDuckDB checkpoints and closes the embedded engine while leaving
// Postgres open. Server.Shutdown calls it after the ingest worker drains — so
// the last acked batch is committed and folded out of the WAL — and before
// NATS/Redis/Postgres close.
func (s *Store) CloseDuckDB() {
	if s.sandboxes != nil {
		s.sandboxes.closeAll()
	}
	if s.duck != nil {
		_ = s.duck.Close()
	}
}

func (s *Store) Close() {
	s.CloseDuckDB()
	if s.pg != nil {
		s.pg.Close()
	}
}

// migrate brings the control plane up to schema. The analytics schema lives
// in DuckDB and is created by OpenDuckDB itself, so this is Postgres-only.
func (s *Store) migrate(ctx context.Context, cfg config.Config) error {
	if err := s.migratePostgres(ctx, cfg); err != nil {
		return err
	}
	// These were nested under the old engine bootstrap historically but are
	// all Postgres DDL; they moved here when that bootstrap was removed.
	// migratePostgres already ran migrateAgent (validation_tests references
	// agent_runs), so it is not repeated here.
	if err := s.migrateAgentTrace(ctx); err != nil {
		return err
	}
	if err := s.migrateAgentSessionLog(ctx); err != nil {
		return err
	}
	// Spill hangs off the same runs as the session log and shares its lifetime —
	// a locator only means anything while the log that mentions it exists.
	if err := s.migrateAgentSpill(ctx); err != nil {
		return err
	}
	if err := s.migrateAgentConversations(ctx); err != nil {
		return err
	}
	if err := s.migrateAgentLab(ctx); err != nil {
		return err
	}
	if err := s.migrateTeams(ctx); err != nil {
		return err
	}
	return nil
}

func (s *Store) migratePostgres(ctx context.Context, cfg config.Config) error {
	if _, err := s.pg.Exec(ctx, `CREATE EXTENSION IF NOT EXISTS pgcrypto`); err != nil {
		return err
	}
	// pg_trgm backs the recommendation de-duplication in agent_runtime.go. A
	// scheduled agent re-files the same finding in slightly different words on
	// every cycle, so "have I already said this?" is a fuzzy question, and it
	// has to be answered by an index rather than a scan of every open row.
	if _, err := s.pg.Exec(ctx, `CREATE EXTENSION IF NOT EXISTS pg_trgm`); err != nil {
		return err
	}
	if _, err := s.pg.Exec(ctx, `
CREATE TABLE IF NOT EXISTS users (
	id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
	email VARCHAR(255) UNIQUE NOT NULL,
	name VARCHAR(255) NOT NULL,
	password_hash TEXT NOT NULL,
	created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
)`); err != nil {
		return err
	}
	if _, err := s.pg.Exec(ctx, `
CREATE TABLE IF NOT EXISTS workspaces (
	id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
	name VARCHAR(255) NOT NULL,
	created_by UUID REFERENCES users(id) ON DELETE SET NULL,
	created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
)`); err != nil {
		return err
	}
	if _, err := s.pg.Exec(ctx, `
CREATE TABLE IF NOT EXISTS workspace_members (
	workspace_id UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
	user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	role VARCHAR(32) NOT NULL DEFAULT 'owner',
	created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	PRIMARY KEY (workspace_id, user_id)
)`); err != nil {
		return err
	}
	if _, err := s.pg.Exec(ctx, `
CREATE INDEX IF NOT EXISTS workspace_members_user_idx
ON workspace_members (user_id, workspace_id)`); err != nil {
		return err
	}
	if _, err := s.pg.Exec(ctx, `
UPDATE workspace_members SET role = 'member' WHERE role = 'viewer'`); err != nil {
		return err
	}
	if _, err := s.pg.Exec(ctx, `
CREATE TABLE IF NOT EXISTS workspace_audit_logs (
	id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
	workspace_id UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
	actor_id UUID REFERENCES users(id) ON DELETE SET NULL,
	action VARCHAR(64) NOT NULL,
	target_type VARCHAR(64) NOT NULL,
	target_id UUID,
	target_label TEXT NOT NULL DEFAULT '',
	metadata JSONB NOT NULL DEFAULT '{}'::jsonb,
	created_at TIMESTAMPTZ NOT NULL DEFAULT now()
)`); err != nil {
		return err
	}
	if _, err := s.pg.Exec(ctx, `
CREATE INDEX IF NOT EXISTS workspace_audit_logs_workspace_created_idx
ON workspace_audit_logs (workspace_id, created_at DESC)`); err != nil {
		return err
	}
	if _, err := s.pg.Exec(ctx, `
CREATE TABLE IF NOT EXISTS user_sessions (
	id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
	user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	token_hash VARCHAR(128) UNIQUE NOT NULL,
	expires_at TIMESTAMPTZ NOT NULL,
	created_at TIMESTAMPTZ NOT NULL DEFAULT now()
)`); err != nil {
		return err
	}
	if _, err := s.pg.Exec(ctx, `
CREATE INDEX IF NOT EXISTS user_sessions_user_expires_idx
ON user_sessions (user_id, expires_at DESC)`); err != nil {
		return err
	}
	// The shared demo's per-user daily agent budget (demo_quota.go). It is its
	// own table rather than a column on agent_runs because agent_runs has no
	// user at all — a scheduled run is started by a clock — and because this
	// ledger is written on a path that must stay a single conditional
	// statement. One row per (user, UTC day); the day is part of the key so a
	// new day starts at zero without anything having to reset it.
	if _, err := s.pg.Exec(ctx, `
CREATE TABLE IF NOT EXISTS demo_agent_run_quota (
	user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	day DATE NOT NULL,
	runs INTEGER NOT NULL DEFAULT 0,
	created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	PRIMARY KEY (user_id, day)
)`); err != nil {
		return err
	}
	if _, err := s.pg.Exec(ctx, `
CREATE TABLE IF NOT EXISTS projects (
	id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
	workspace_id UUID REFERENCES workspaces(id) ON DELETE SET NULL,
	name VARCHAR(255) NOT NULL,
	timezone VARCHAR(64),
	api_key VARCHAR(128) UNIQUE NOT NULL,
	owner_id UUID REFERENCES users(id) ON DELETE SET NULL,
	created_at TIMESTAMPTZ NOT NULL DEFAULT now()
)`); err != nil {
		return err
	}
	if _, err := s.pg.Exec(ctx, `ALTER TABLE projects ADD COLUMN IF NOT EXISTS workspace_id UUID REFERENCES workspaces(id) ON DELETE SET NULL`); err != nil {
		return err
	}
	if _, err := s.pg.Exec(ctx, `ALTER TABLE projects ADD COLUMN IF NOT EXISTS owner_id UUID`); err != nil {
		return err
	}
	// Nullable avoids a table rewrite and preserves legacy rows; NULL has the
	// labelled UTC fallback defined by overviewProjectTimezone.
	if _, err := s.pg.Exec(ctx, `ALTER TABLE projects ADD COLUMN IF NOT EXISTS timezone VARCHAR(64)`); err != nil {
		return err
	}
	// goal and activation_event are the onboarding answers (007): NULL goal
	// means the prompt was never answered, 'skipped' means declined.
	// activation_event names the event the owner says counts as "activated" —
	// it is what unlocks the activation overview metric. Both nullable so the
	// ALTER never rewrites the table.
	if _, err := s.pg.Exec(ctx, `ALTER TABLE projects ADD COLUMN IF NOT EXISTS goal VARCHAR(32)`); err != nil {
		return err
	}
	if _, err := s.pg.Exec(ctx, `ALTER TABLE projects ADD COLUMN IF NOT EXISTS activation_event VARCHAR(255)`); err != nil {
		return err
	}
	if _, err := s.pg.Exec(ctx, `
CREATE INDEX IF NOT EXISTS projects_workspace_created_idx
ON projects (workspace_id, created_at DESC)`); err != nil {
		return err
	}
	if _, err := s.pg.Exec(ctx, `
CREATE TABLE IF NOT EXISTS dashboards (
	id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
	project_id UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
	name VARCHAR(255) NOT NULL,
	description TEXT NOT NULL DEFAULT '',
	created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
)`); err != nil {
		return err
	}
	if _, err := s.pg.Exec(ctx, `
CREATE INDEX IF NOT EXISTS dashboards_project_created_idx
ON dashboards (project_id, created_at DESC)`); err != nil {
		return err
	}
	if _, err := s.pg.Exec(ctx, `
CREATE TABLE IF NOT EXISTS charts (
	id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
	dashboard_id UUID NOT NULL REFERENCES dashboards(id) ON DELETE CASCADE,
	project_id UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
	name VARCHAR(255) NOT NULL,
	kind VARCHAR(32) NOT NULL DEFAULT 'line',
	metric VARCHAR(64) NOT NULL DEFAULT 'events',
	event_name VARCHAR(255) NOT NULL DEFAULT '',
	event_type VARCHAR(64) NOT NULL DEFAULT '',
	created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
)`); err != nil {
		return err
	}
	if _, err := s.pg.Exec(ctx, `
CREATE INDEX IF NOT EXISTS charts_dashboard_created_idx
ON charts (dashboard_id, created_at ASC)`); err != nil {
		return err
	}
	if _, err := s.pg.Exec(ctx, `ALTER TABLE charts ADD COLUMN IF NOT EXISTS sql TEXT NOT NULL DEFAULT ''`); err != nil {
		return err
	}
	if _, err := s.pg.Exec(ctx, `ALTER TABLE charts ADD COLUMN IF NOT EXISTS x_field VARCHAR(255) NOT NULL DEFAULT ''`); err != nil {
		return err
	}
	if _, err := s.pg.Exec(ctx, `ALTER TABLE charts ADD COLUMN IF NOT EXISTS y_field VARCHAR(255) NOT NULL DEFAULT ''`); err != nil {
		return err
	}
	// sort_order drives the board layout order; col_span lets a chart occupy 1–3
	// columns of the dashboard grid. Both default so existing charts keep working.
	if _, err := s.pg.Exec(ctx, `ALTER TABLE charts ADD COLUMN IF NOT EXISTS sort_order INT NOT NULL DEFAULT 0`); err != nil {
		return err
	}
	if _, err := s.pg.Exec(ctx, `ALTER TABLE charts ADD COLUMN IF NOT EXISTS col_span INT NOT NULL DEFAULT 1`); err != nil {
		return err
	}
	if _, err := s.pg.Exec(ctx, `
CREATE INDEX IF NOT EXISTS charts_dashboard_sort_idx
ON charts (dashboard_id, sort_order ASC, created_at ASC)`); err != nil {
		return err
	}
	if _, err := s.pg.Exec(ctx, `
CREATE TABLE IF NOT EXISTS saved_queries (
	id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
	project_id UUID NOT NULL REFERENCES projects(id),
	natural_language TEXT NOT NULL,
	generated_sql TEXT NOT NULL,
	verified BOOLEAN NOT NULL DEFAULT false,
	result_cache JSONB,
	created_at TIMESTAMPTZ NOT NULL DEFAULT now()
)`); err != nil {
		return err
	}
	if _, err := s.pg.Exec(ctx, `
CREATE INDEX IF NOT EXISTS saved_queries_project_created_idx
ON saved_queries (project_id, created_at DESC)`); err != nil {
		return err
	}
	// cohort_audiences holds per-project custom audience segments for the cohort
	// retention view. A row is a structured rule (kind + plans), never raw SQL —
	// the predicate is compiled server-side (ProjectAudience.compilePredicate),
	// which is the injection-safe seam that lets users define their own
	// paid/premium-style groups without touching backend code.
	if _, err := s.pg.Exec(ctx, `
CREATE TABLE IF NOT EXISTS cohort_audiences (
	id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
	project_id UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
	key VARCHAR(64) NOT NULL,
	label VARCHAR(120) NOT NULL,
	kind VARCHAR(32) NOT NULL,
	plans JSONB NOT NULL DEFAULT '[]'::jsonb,
	created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	UNIQUE (project_id, key)
)`); err != nil {
		return err
	}
	// Widen kind for tables created before the subscription audience kinds
	// (e.g. 'active_subscriber' is 17 chars, past the original VARCHAR(16)).
	if _, err := s.pg.Exec(ctx, `ALTER TABLE cohort_audiences ALTER COLUMN kind TYPE VARCHAR(32)`); err != nil {
		return err
	}
	if _, err := s.pg.Exec(ctx, `
CREATE INDEX IF NOT EXISTS cohort_audiences_project_created_idx
ON cohort_audiences (project_id, created_at ASC)`); err != nil {
		return err
	}
	// subscription_mappings tells the cohort engine how to read a project's
	// subscription lifecycle off its events (which event = start/renew/cancel,
	// which property = plan/amount/period-end/trial). One row per project; absence
	// means "use defaults / no subscription concept". Config-only, never raw SQL.
	if _, err := s.pg.Exec(ctx, `
CREATE TABLE IF NOT EXISTS subscription_mappings (
	project_id UUID PRIMARY KEY REFERENCES projects(id) ON DELETE CASCADE,
	start_event VARCHAR(80) NOT NULL DEFAULT '',
	renew_event VARCHAR(80) NOT NULL DEFAULT '',
	cancel_event VARCHAR(80) NOT NULL DEFAULT '',
	plan_prop VARCHAR(80) NOT NULL DEFAULT '',
	amount_prop VARCHAR(80) NOT NULL DEFAULT '',
	period_end_prop VARCHAR(80) NOT NULL DEFAULT '',
	trial_prop VARCHAR(80) NOT NULL DEFAULT '',
	grace_days INT NOT NULL DEFAULT 1,
	updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
)`); err != nil {
		return err
	}
	if _, err := s.pg.Exec(ctx, `
CREATE TABLE IF NOT EXISTS dashboard_templates (
	id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
	project_id UUID REFERENCES projects(id) ON DELETE CASCADE,
	name VARCHAR(255) NOT NULL,
	description TEXT NOT NULL DEFAULT '',
	is_system BOOLEAN NOT NULL DEFAULT false,
	created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
)`); err != nil {
		return err
	}
	if _, err := s.pg.Exec(ctx, `
CREATE INDEX IF NOT EXISTS dashboard_templates_system_idx
ON dashboard_templates (is_system, created_at ASC)`); err != nil {
		return err
	}
	if _, err := s.pg.Exec(ctx, `
CREATE INDEX IF NOT EXISTS dashboard_templates_project_idx
ON dashboard_templates (project_id, created_at DESC)`); err != nil {
		return err
	}
	if _, err := s.pg.Exec(ctx, `
CREATE TABLE IF NOT EXISTS template_charts (
	id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
	template_id UUID NOT NULL REFERENCES dashboard_templates(id) ON DELETE CASCADE,
	name VARCHAR(255) NOT NULL,
	kind VARCHAR(32) NOT NULL DEFAULT 'line',
	metric VARCHAR(64) NOT NULL DEFAULT 'events',
	event_name VARCHAR(255) NOT NULL DEFAULT '',
	event_type VARCHAR(64) NOT NULL DEFAULT '',
	sql TEXT NOT NULL DEFAULT '',
	x_field VARCHAR(255) NOT NULL DEFAULT '',
	y_field VARCHAR(255) NOT NULL DEFAULT '',
	sort_order INT NOT NULL DEFAULT 0,
	created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
)`); err != nil {
		return err
	}
	if _, err := s.pg.Exec(ctx, `
CREATE INDEX IF NOT EXISTS template_charts_template_idx
ON template_charts (template_id, sort_order ASC)`); err != nil {
		return err
	}
	if _, err := s.pg.Exec(ctx, `
CREATE TABLE IF NOT EXISTS aliases (
	project_id UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
	anonymous_id VARCHAR(1024) NOT NULL,
	canonical_id VARCHAR(1024) NOT NULL,
	created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	PRIMARY KEY (project_id, anonymous_id)
)`); err != nil {
		return err
	}
	if _, err := s.pg.Exec(ctx, `
CREATE INDEX IF NOT EXISTS aliases_project_canonical_idx
ON aliases (project_id, canonical_id)`); err != nil {
		return err
	}
	if _, err := s.pg.Exec(ctx, `
INSERT INTO projects (name, api_key)
VALUES ($1, $2)
ON CONFLICT (api_key) DO NOTHING`, cfg.DefaultProjectName, cfg.DefaultProjectAPIKey); err != nil {
		return err
	}
	var defaultProjectID string
	if err := s.pg.QueryRow(ctx, `SELECT id::text FROM projects WHERE api_key = $1`, cfg.DefaultProjectAPIKey).Scan(&defaultProjectID); err == nil {
		var dashboardCount int
		if err := s.pg.QueryRow(ctx, `SELECT count(*) FROM dashboards WHERE project_id = $1`, defaultProjectID).Scan(&dashboardCount); err != nil {
			return err
		}
		if dashboardCount == 0 {
			if err := seedStarterDashboard(ctx, s.pg, defaultProjectID); err != nil {
				return err
			}
		}
	}

	if err := s.migrateAlerts(ctx); err != nil {
		return err
	}

	if err := s.migrateConnectors(ctx); err != nil {
		return err
	}

	if err := s.migrateConnectorRuns(ctx); err != nil {
		return err
	}

	if err := s.migrateSourceCredentials(ctx); err != nil {
		return err
	}

	if err := s.migrateCredentials(ctx); err != nil {
		return err
	}

	if err := s.migrateLifecycle(ctx); err != nil {
		return err
	}

	// The metric catalog is the vocabulary a board declaration is validated
	// against, and the contract every metric surface serves — so it is
	// refreshed before the boards that reference it are read.
	if err := s.migrateMetricCatalog(ctx); err != nil {
		return err
	}

	if err := s.migrateBoards(ctx); err != nil {
		return err
	}

	// The project-scoped target history the overview read resolves against.
	if err := s.migrateMetricTargets(ctx); err != nil {
		return err
	}

	// Chart annotations sit beside the boards they mark: a project-scoped
	// record, not a per-chart field, so one deploy shows on every temporal
	// chart that spans it.
	if err := s.migrateAnnotations(ctx); err != nil {
		return err
	}

	// Boards seeded before the "guest vs identified" query was corrected still
	// read `properties.email`; the seed only ever runs once, so they have to be
	// repaired here. Needs the revision column migrateLifecycle just added.
	if charts, projects, err := s.repairSeededCharts(ctx); err != nil {
		return err
	} else if charts > 0 {
		fmt.Printf("seeded chart repair: rewrote the guest-vs-identified query on %d chart(s) across %d project(s)\n", charts, projects)
	}

	// Analysis destinations (Acquisition / Monetization / Usage) are declared
	// boards. Projects created before this slice have none; seed them once the
	// board columns exist. Existing keys are left alone.
	if err := s.EnsureDefaultBoardsForAll(ctx); err != nil {
		return err
	}

	// The seed only writes absent keys, so a project created before a metric
	// existed keeps the old board forever — Monetization saying "Not available"
	// beside a working revenue read. Upgrade the rows that still hold the
	// untouched seed; edited boards stop matching and are left alone.
	if boards, projects, err := s.RepairDefaultBoards(ctx); err != nil {
		return err
	} else if boards > 0 {
		fmt.Printf("default board repair: upgraded %d board(s) across %d project(s)\n", boards, projects)
	}

	// Agent schema (including workspace_providers) lives in Postgres. Run it
	// here so a PG-only boot still creates the tables; the call is idempotent.
	if err := s.migrateAgent(ctx); err != nil {
		return err
	}

	// Findings-engine substrate (finding_scan_state, funnel_watches). New
	// tables only; after migrateAgent so the recommendations table it writes
	// already exists on a fresh boot.
	if err := s.migrateFindings(ctx); err != nil {
		return err
	}

	// The pre-product tables. validation_tests references agent_runs, so this
	// has to follow migrateAgent rather than sit with the other feature
	// migrations above.
	if err := s.migrateValidation(ctx); err != nil {
		return err
	}

	// The plan column and upgrade_requests are Postgres DDL, and the auth path
	// now selects w.plan on every login — so it must not sit behind an engine
	// migration. An outage or schema conflict there returns early, and this
	// column missing turns "analytics are down" into "nobody can log in".
	if err := s.migrateWorkspacePlan(ctx); err != nil {
		return err
	}

	// Last, because it reads projects/workspaces/users and writes memberships —
	// every table it touches has to already exist.
	if err := s.migrateDemoWorkspace(ctx, cfg); err != nil {
		return err
	}

	return nil
}

type pgQuerier interface {
	Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// guestVsIdentifiedSQL builds the seeded "Visitors: guest vs identified" chart
// query: how many visitors were identified, and how many never were.
//
// Identified-ness is a fact about the person, not a property of an event, and
// AgentRay already has one definition of it — DistinctIDLinked (lifecycle.go):
// an explicit identify/alias link exists for the id. The chart states that rule
// in SQL, over the stitched view every person-scoped read already reads:
//
//   - the visitor owns an `$identify` event, or
//   - the aliases mirror folded a different raw id into the visitor
//     (distinct_id <> canonical_id), which is the forward half of the same link.
//
// It used to test `properties.email` / `$."$set".email` instead. That was a PII
// read on AgentRay's own starter board, and a silent lie waiting to happen: a
// customer that stops sending an email trait — LoHi does, on purpose — would
// have every visitor reported as "Guest".
//
// visitorColumn is the id this board counts as a visitor (the starter board
// counts stitched humans; the stock templates count raw ids), so the column is
// the only thing that changes for them. humanOnly keeps crawlers out of a
// visitor count — but it filters the non-human *pageview*, not the markers: a
// visitor whose identify request was classified non-human (a UA change
// mid-session, a client that sends none) still owns the human pageview it came
// with, and dropping the marker would report that visitor as a Guest.
func guestVsIdentifiedSQL(visitorColumn string, humanOnly bool) string {
	scope := `event_name IN ('user.pageview', '$identify')`
	if humanOnly {
		scope = `(event_name = 'user.pageview' AND coalesce(visitor_class, 'human') = 'human') OR event_name = '$identify'`
	}
	return `SELECT if(identified, 'Identified', 'Guest') AS user_type, count(*) AS visitors FROM (
SELECT (max(if(event_name = '$identify', 1, 0)) > 0
        OR max(if(distinct_id <> canonical_id, 1, 0)) > 0) AS identified,
       max(if(event_name = 'user.pageview', 1, 0)) > 0 AS is_pageview
FROM events
WHERE ` + scope + `
GROUP BY ` + visitorColumn + `
) WHERE is_pageview GROUP BY user_type ORDER BY visitors DESC`
}

// The shipped (pre-repair) seeded queries: the exact strings earlier revisions
// wrote into `charts` and `template_charts`. Two eras of the same chart are
// still out there — the DuckDB one, and the ClickHouse one from before the
// engine port, which never translated stored chart SQL and so is both a PII
// read and unrunnable on the current engine.
//
// The repair matches these literally, which is what bounds it to AgentRay's own
// seeded chart — a customer's chart is never rewritten, not even one that reads
// an email property deliberately. The template string covers both the Product
// Overview and the Marketing & Acquisition charts; they shipped identically.
const (
	staleStarterGuestVsIdentifiedSQL = `SELECT if(json_extract_string(properties, '$.email') != '' OR json_extract_string(properties, '$."$set".email') != '', 'Identified', 'Guest') AS user_type, count(DISTINCT canonical_id) AS visitors FROM events WHERE event_name = 'user.pageview' AND coalesce(visitor_class, 'human') = 'human' GROUP BY user_type ORDER BY visitors DESC`
	staleTemplateGuestVsIdentifiedSQL = `SELECT if(json_extract_string(properties, '$.email') != '' OR json_extract_string(properties, '$."$set".email') != '', 'Identified', 'Guest') AS user_type, count(DISTINCT distinct_id) AS visitors FROM events WHERE event_name = 'user.pageview' GROUP BY user_type ORDER BY visitors DESC`

	legacyStarterGuestVsIdentifiedSQL = `SELECT if(JSONExtractString(properties, 'email') != '' OR JSONExtractString(properties, '$set', 'email') != '', 'Identified', 'Guest') AS user_type, uniqExact(canonical_id) AS visitors FROM events WHERE event_name = 'user.pageview' AND ifNull(visitor_class, 'human') = 'human' GROUP BY user_type ORDER BY visitors DESC`
	legacyTemplateGuestVsIdentifiedSQL = `SELECT if(JSONExtractString(properties, 'email') != '' OR JSONExtractString(properties, '$set', 'email') != '', 'Identified', 'Guest') AS user_type, uniqExact(distinct_id) AS visitors FROM events WHERE event_name = 'user.pageview' GROUP BY user_type ORDER BY visitors DESC`
)

// seededChartRepairs pairs each shipped seeded query with its replacement: same
// chart, same visitor column, identified-ness read from identity linkage.
func seededChartRepairs() [][2]string {
	return [][2]string{
		{staleStarterGuestVsIdentifiedSQL, guestVsIdentifiedSQL("canonical_id", true)},
		{staleTemplateGuestVsIdentifiedSQL, guestVsIdentifiedSQL("distinct_id", false)},
		{legacyStarterGuestVsIdentifiedSQL, guestVsIdentifiedSQL("canonical_id", true)},
		{legacyTemplateGuestVsIdentifiedSQL, guestVsIdentifiedSQL("distinct_id", false)},
	}
}

// repairSeededCharts rewrites the seeded "guest vs identified" query on every
// board that already holds a pre-repair copy. The seed runs once — at project
// creation, or when a template is cloned — so without this an existing
// deployment keeps reading `properties.email` forever, and starts calling every
// visitor a "Guest" the moment its events stop carrying an email trait.
// (template_charts itself needs no repair: SeedSystemTemplates rebuilds those
// rows from the current source on every boot.)
//
// Bounded and idempotent by construction. The predicate is an exact match on the
// shipped strings, so it can only ever touch a row that IS the seeded chart, and
// a second run matches nothing because no replacement contains the stale text.
// `charts` is a small table and the update writes only the matched rows' own
// `sql` column — no rewrite, no DDL, no lock on a big table — so none of the
// deploy-time hazards the big-table migration rule guards against apply.
//
// It returns the rows repaired and how many projects own them: the blast radius
// an operator should see, and where the numbers span projects this is the only
// summary of it.
func (s *Store) repairSeededCharts(ctx context.Context) (rows int64, projects int64, err error) {
	repairs := seededChartRepairs()
	args := make([]any, 0, len(repairs)*2)
	values := make([]string, 0, len(repairs))
	for i, pair := range repairs {
		args = append(args, pair[0], pair[1])
		values = append(values, fmt.Sprintf("($%d, $%d)", 2*i+1, 2*i+2))
	}
	// One statement for every pair: the UPDATE's RETURNING feeds the scope
	// counts, so a boot reports rows and projects without a second round trip.
	err = s.pg.QueryRow(ctx, `
WITH stale(stale_sql, fresh_sql) AS (VALUES `+strings.Join(values, ", ")+`),
repaired AS (
	UPDATE charts c
	SET sql = s.fresh_sql, revision = c.revision + 1, updated_at = now()
	FROM stale s
	WHERE c.sql = s.stale_sql
	RETURNING c.project_id
)
SELECT count(*), count(DISTINCT project_id) FROM repaired`, args...).Scan(&rows, &projects)
	return rows, projects, err
}

// starterCharts is the board every new project starts with, in board order.
// Package-level rather than a local of seedStarterDashboard so the seeded
// queries are addressable: the tests that prove what a seeded chart plots run
// the very strings this returns, not a copy of them.
func starterCharts() []Chart {
	return []Chart{
		{Name: "Event trend", Kind: "line", Metric: "events"},
		{Name: "Top events", Kind: "bar", Metric: "event_breakdown"},
		{Name: "Sessions", Kind: "stat", Metric: "sessions"},
		{Name: "Agent cost", Kind: "stat", Metric: "cost", EventType: "agent"},
		{
			Name:   "Traffic: human / bot / AI",
			Kind:   "pie",
			SQL:    `SELECT coalesce(visitor_class, 'human') AS class, count(*) AS count FROM events WHERE event_name = 'user.pageview' GROUP BY class ORDER BY count DESC`,
			XField: "class",
			YField: "count",
		},
		{
			Name:   "Visitors: guest vs identified",
			Kind:   "bar",
			SQL:    guestVsIdentifiedSQL("canonical_id", true),
			XField: "user_type",
			YField: "visitors",
		},
		{
			Name:   "Visitor retention",
			Kind:   "bar",
			SQL:    `SELECT if(visits > 1, 'Returning', 'First-time') AS visitor_type, count(*) AS visitors FROM (SELECT canonical_id, count(*) AS visits FROM events WHERE event_name = 'user.pageview' AND coalesce(visitor_class, 'human') = 'human' GROUP BY canonical_id) GROUP BY visitor_type ORDER BY visitors DESC`,
			XField: "visitor_type",
			YField: "visitors",
		},
	}
}

// seedStarterDashboard gives every new project a ready-use board with
// predefined graphs from base activity, so dashboards are never empty.
func seedStarterDashboard(ctx context.Context, q pgQuerier, projectID string) error {
	var dashboardID string
	if err := q.QueryRow(ctx, `
INSERT INTO dashboards (project_id, name, description)
VALUES ($1, $2, $3)
RETURNING id::text`, projectID, "Product overview", "Starter board auto-created with predefined graphs from base activity.").Scan(&dashboardID); err != nil {
		return err
	}
	for _, chart := range starterCharts() {
		if _, err := q.Exec(ctx, `
INSERT INTO charts (dashboard_id, project_id, name, kind, metric, event_name, event_type, sql, x_field, y_field)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`, dashboardID, projectID, chart.Name, chart.Kind, chart.Metric, chart.EventName, chart.EventType, chart.SQL, chart.XField, chart.YField); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) ProjectByAPIKey(ctx context.Context, apiKey string) (Project, error) {
	if apiKey == "" {
		return Project{}, fmt.Errorf("missing api key")
	}
	var p Project
	err := s.pg.QueryRow(ctx, `SELECT id::text, coalesce(workspace_id::text, ''), name, coalesce(timezone, ''), goal, coalesce(activation_event, ''), api_key, created_at FROM projects WHERE api_key = $1`, apiKey).
		Scan(&p.ID, &p.WorkspaceID, &p.Name, &p.Timezone, &p.Goal, &p.ActivationEvent, &p.APIKey, &p.CreatedAt)
	if err != nil {
		return Project{}, err
	}
	return p, nil
}

// ProjectByID resolves a project without a user check — internal auth paths
// only (the credential IS the authorization). Callers needing membership use
// ProjectByIDForUser.
func (s *Store) ProjectByID(ctx context.Context, projectID string) (Project, error) {
	var p Project
	err := s.pg.QueryRow(ctx, `SELECT id::text, coalesce(workspace_id::text, ''), name, coalesce(timezone, ''), goal, coalesce(activation_event, ''), api_key, created_at FROM projects WHERE id = $1`, projectID).
		Scan(&p.ID, &p.WorkspaceID, &p.Name, &p.Timezone, &p.Goal, &p.ActivationEvent, &p.APIKey, &p.CreatedAt)
	if err != nil {
		return Project{}, err
	}
	return p, nil
}

func (s *Store) CreateProject(ctx context.Context, name string) (Project, error) {
	if name == "" {
		name = "Untitled project"
	}
	apiKey := "agentray_" + uuid.NewString()
	var p Project
	err := s.pg.QueryRow(ctx, `
INSERT INTO projects (name, api_key)
VALUES ($1, $2)
RETURNING id::text, coalesce(workspace_id::text, ''), name, coalesce(timezone, ''), goal, coalesce(activation_event, ''), api_key, created_at`, name, apiKey).
		Scan(&p.ID, &p.WorkspaceID, &p.Name, &p.Timezone, &p.Goal, &p.ActivationEvent, &p.APIKey, &p.CreatedAt)
	return p, err
}

func (s *Store) RotateProjectAPIKey(ctx context.Context, projectID string) (Project, error) {
	apiKey := "agentray_" + uuid.NewString()
	var p Project
	err := s.pg.QueryRow(ctx, `
UPDATE projects
SET api_key = $2
WHERE id = $1
RETURNING id::text, coalesce(workspace_id::text, ''), name, coalesce(timezone, ''), goal, coalesce(activation_event, ''), api_key, created_at`, projectID, apiKey).
		Scan(&p.ID, &p.WorkspaceID, &p.Name, &p.Timezone, &p.Goal, &p.ActivationEvent, &p.APIKey, &p.CreatedAt)
	return p, err
}

func (s *Store) ListDashboards(ctx context.Context, projectID string) ([]Dashboard, error) {
	// Returns every dashboard including archived ones (archived_at marks them);
	// the operation layer filters by caller intent via ListDashboardsFiltered.
	rows, err := s.pg.Query(ctx, `
SELECT `+dashboardColumns+`
FROM dashboards
WHERE project_id = $1
ORDER BY created_at DESC`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	dashboards := []Dashboard{}
	for rows.Next() {
		var d Dashboard
		if err := rows.Scan(dashboardScanDest(&d)...); err != nil {
			return nil, err
		}
		dashboards = append(dashboards, d)
	}
	return dashboards, rows.Err()
}

func (s *Store) CreateDashboard(ctx context.Context, projectID string, name string, description string) (Dashboard, error) {
	if name == "" {
		name = "Untitled dashboard"
	}
	var d Dashboard
	err := s.pg.QueryRow(ctx, `
INSERT INTO dashboards (project_id, name, description)
VALUES ($1, $2, $3)
RETURNING `+dashboardColumns, projectID, name, description).
		Scan(dashboardScanDest(&d)...)
	return d, err
}

func (s *Store) UpdateDashboard(ctx context.Context, projectID string, dashboardID string, name string, description string) (Dashboard, error) {
	if name == "" {
		name = "Untitled dashboard"
	}
	var d Dashboard
	err := s.pg.QueryRow(ctx, `
UPDATE dashboards
SET name = $3, description = $4, revision = revision + 1, updated_at = now()
WHERE project_id = $1 AND id = $2
RETURNING `+dashboardColumns, projectID, dashboardID, name, description).
		Scan(dashboardScanDest(&d)...)
	return d, err
}

func (s *Store) DeleteDashboard(ctx context.Context, projectID string, dashboardID string) error {
	_, err := s.pg.Exec(ctx, `DELETE FROM dashboards WHERE project_id = $1 AND id = $2`, projectID, dashboardID)
	return err
}

func (s *Store) ListCharts(ctx context.Context, projectID string, dashboardID string) ([]Chart, error) {
	// Returns every chart including archived ones (archived_at marks them);
	// the operation layer filters by caller intent via ListChartsFiltered.
	rows, err := s.pg.Query(ctx, `
SELECT `+chartColumns+`
FROM charts
WHERE project_id = $1 AND dashboard_id = $2
ORDER BY sort_order ASC, created_at ASC`, projectID, dashboardID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	charts := []Chart{}
	for rows.Next() {
		var c Chart
		if err := rows.Scan(chartScanDest(&c)...); err != nil {
			return nil, err
		}
		charts = append(charts, c)
	}
	return charts, rows.Err()
}

func (s *Store) CreateChart(ctx context.Context, chart Chart) (Chart, error) {
	if chart.Name == "" {
		chart.Name = "Untitled chart"
	}
	if chart.Kind == "" {
		chart.Kind = "line"
	}
	if chart.Metric == "" {
		chart.Metric = "events"
	}
	chart.ColSpan = clampSpan(chart.ColSpan)
	var c Chart
	// New charts append to the end of the board: sort_order = max(existing)+1.
	err := s.pg.QueryRow(ctx, `
INSERT INTO charts (dashboard_id, project_id, name, kind, metric, event_name, event_type, sql, x_field, y_field, col_span, sort_order)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11,
	COALESCE((SELECT MAX(sort_order) + 1 FROM charts WHERE dashboard_id = $1), 0))
RETURNING `+chartColumns,
		chart.DashboardID, chart.ProjectID, chart.Name, chart.Kind, chart.Metric, chart.EventName, chart.EventType, chart.SQL, chart.XField, chart.YField, chart.ColSpan).
		Scan(chartScanDest(&c)...)
	return c, err
}

// clampSpan keeps a chart's column span within the 1–3 column grid; a 0 (unset)
// value becomes a single column.
func clampSpan(span int) int {
	if span < 1 {
		return 1
	}
	if span > 3 {
		return 3
	}
	return span
}

func (s *Store) UpdateChart(ctx context.Context, chart Chart) (Chart, error) {
	if chart.Name == "" {
		chart.Name = "Untitled chart"
	}
	if chart.Kind == "" {
		chart.Kind = "line"
	}
	if chart.Metric == "" {
		chart.Metric = "events"
	}
	chart.ColSpan = clampSpan(chart.ColSpan)
	var c Chart
	// Legacy unfenced write: still bumps revision so a stale operation-layer
	// caller cannot silently overwrite what this write changed.
	err := s.pg.QueryRow(ctx, `
UPDATE charts
SET name = $3, kind = $4, metric = $5, event_name = $6, event_type = $7, sql = $8, x_field = $9, y_field = $10, col_span = $11,
    revision = revision + 1, updated_at = now()
WHERE project_id = $1 AND id = $2
RETURNING `+chartColumns,
		chart.ProjectID, chart.ID, chart.Name, chart.Kind, chart.Metric, chart.EventName, chart.EventType, chart.SQL, chart.XField, chart.YField, chart.ColSpan).
		Scan(chartScanDest(&c)...)
	return c, err
}

// ReorderCharts persists a new board order: each chart id in `chartIDs` gets its
// sort_order set to its index. Scoped to the dashboard + project so a caller can
// only reorder charts it owns. Runs in one transaction so the board never reads
// a half-applied order. The dashboard revision is bumped — it is the optimistic
// fence reorder_charts and dashboard writes share, so even this legacy unfenced
// path cannot leave a stale fence value behind.
func (s *Store) ReorderCharts(ctx context.Context, projectID string, dashboardID string, chartIDs []string) error {
	tx, err := s.pg.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `
UPDATE dashboards SET revision = revision + 1, updated_at = now()
WHERE project_id = $1 AND id = $2`, projectID, dashboardID); err != nil {
		return err
	}
	for i, id := range chartIDs {
		if _, err := tx.Exec(ctx, `
UPDATE charts SET sort_order = $4, updated_at = now()
WHERE project_id = $1 AND dashboard_id = $2 AND id = $3`, projectID, dashboardID, id, i); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func (s *Store) DeleteChart(ctx context.Context, projectID string, chartID string) error {
	_, err := s.pg.Exec(ctx, `DELETE FROM charts WHERE project_id = $1 AND id = $2`, projectID, chartID)
	return err
}

// SeedSystemTemplates upserts the four built-in system templates on startup.
// It is idempotent — safe to call on every boot.
func (s *Store) SeedSystemTemplates(ctx context.Context) error {
	type seedTemplate struct {
		id          string
		name        string
		description string
		charts      []TemplateChart
	}

	templates := []seedTemplate{
		{
			id:          ProductOverviewTemplateID,
			name:        "Product Overview",
			description: "Starter board with DAU, MAU, traffic breakdowns, and visitor identification.",
			charts: []TemplateChart{
				{Name: "DAU", Kind: "line", SQL: `SELECT CAST("timestamp" AS DATE) AS day, count(DISTINCT distinct_id) AS dau FROM events GROUP BY day ORDER BY day ASC LIMIT 30`, XField: "day", YField: "dau", SortOrder: 0},
				{Name: "MAU", Kind: "line", SQL: `SELECT date_trunc('month', "timestamp") AS month, count(DISTINCT distinct_id) AS mau FROM events GROUP BY month ORDER BY month ASC LIMIT 12`, XField: "month", YField: "mau", SortOrder: 1},
				{Name: "Event trend", Kind: "line", Metric: "events", SortOrder: 2},
				{Name: "Sessions", Kind: "stat", Metric: "sessions", SortOrder: 3},
				{Name: "Top events", Kind: "bar", Metric: "event_breakdown", SortOrder: 4},
				{Name: "AI cost", Kind: "stat", Metric: "cost", EventType: "agent", SortOrder: 5},
				{Name: "Traffic by class", Kind: "pie", SQL: `SELECT coalesce(visitor_class, 'human') AS class, count(*) AS count FROM events WHERE event_name = 'user.pageview' GROUP BY class ORDER BY count DESC`, XField: "class", YField: "count", SortOrder: 6},
				{Name: "Visitors: guest vs identified", Kind: "bar", SQL: guestVsIdentifiedSQL("distinct_id", false), XField: "user_type", YField: "visitors", SortOrder: 7},
			},
		},
		{
			id:          AIAgentOpsTemplateID,
			name:        "AI Agent Ops",
			description: "Tool calls, failures, latency, and token cost for AI-agent workflows.",
			charts: []TemplateChart{
				{Name: "Agent event timeline", Kind: "line", Metric: "events", EventType: "agent", SortOrder: 0},
				{Name: "Tool call breakdown", Kind: "bar", Metric: "event_breakdown", EventName: "agent.tool_call", EventType: "agent", SortOrder: 1},
				{Name: "Token usage", Kind: "bar", Metric: "tokens", EventType: "agent", SortOrder: 2},
				{Name: "Agent cost", Kind: "stat", Metric: "cost", EventType: "agent", SortOrder: 3},
			},
		},
		{
			id:          ProductActivityTemplateID,
			name:        "Product Activity",
			description: "Pageviews, signups, conversions, and product activity health.",
			charts: []TemplateChart{
				{Name: "Product event trend", Kind: "line", Metric: "events", EventType: "user", SortOrder: 0},
				{Name: "Top product events", Kind: "bar", Metric: "event_breakdown", EventType: "user", SortOrder: 1},
				{Name: "Sessions", Kind: "stat", Metric: "sessions", SortOrder: 2},
				{Name: "Conversions", Kind: "bar", Metric: "event_breakdown", EventName: "user.conversion", SortOrder: 3},
			},
		},
		{
			id:          CostControlTemplateID,
			name:        "Cost Control",
			description: "Cost, tokens, and model usage by agent session.",
			charts: []TemplateChart{
				{Name: "Total cost", Kind: "stat", Metric: "cost", EventType: "agent", SortOrder: 0},
				{Name: "Token usage", Kind: "bar", Metric: "tokens", EventType: "agent", SortOrder: 1},
				{Name: "Cost events", Kind: "line", Metric: "events", EventType: "agent", SortOrder: 2},
			},
		},
		{
			id:          GrowthRetentionTemplateID,
			name:        "Growth & Retention",
			description: "Acquisition, activation, and how well readers come back week over week.",
			charts: []TemplateChart{
				{Name: "New vs returning (daily)", Kind: "line", SQL: `SELECT CAST("timestamp" AS DATE) AS day, count(DISTINCT distinct_id) FILTER (WHERE is_first) AS new_readers, count(DISTINCT distinct_id) FILTER (WHERE NOT is_first) AS returning_readers FROM (SELECT distinct_id, "timestamp", min("timestamp") OVER (PARTITION BY distinct_id) = "timestamp" AS is_first FROM events) GROUP BY day ORDER BY day ASC LIMIT 30`, XField: "day", YField: "returning_readers", SortOrder: 0},
				{Name: "WAU", Kind: "line", SQL: `SELECT date_trunc('week', "timestamp") AS week, count(DISTINCT distinct_id) AS wau FROM events GROUP BY week ORDER BY week ASC LIMIT 12`, XField: "week", YField: "wau", SortOrder: 1},
				{Name: "Active readers (7d)", Kind: "stat", Metric: "sessions", SortOrder: 2},
				{Name: "Reading depth (events/reader)", Kind: "bar", SQL: `SELECT CAST("timestamp" AS DATE) AS day, round(count(*) / count(DISTINCT distinct_id), 1) AS events_per_reader FROM events GROUP BY day ORDER BY day ASC LIMIT 30`, XField: "day", YField: "events_per_reader", SortOrder: 3},
				{Name: "Top events", Kind: "bar", Metric: "event_breakdown", SortOrder: 4},
			},
		},
		{
			id:          MarketingFunnelTemplateID,
			name:        "Marketing & Acquisition",
			description: "Traffic sources, the visit→read→subscribe funnel, and guest-to-identified conversion.",
			charts: []TemplateChart{
				{Name: "Traffic by class", Kind: "pie", SQL: `SELECT coalesce(visitor_class, 'human') AS class, count(*) AS count FROM events WHERE event_name = 'user.pageview' GROUP BY class ORDER BY count DESC`, XField: "class", YField: "count", SortOrder: 0},
				{Name: "Top referrers", Kind: "bar", SQL: `SELECT coalesce(nullif(json_extract_string(properties, '$.referrer'), ''), 'direct') AS referrer, count(*) AS visits FROM events WHERE event_name = 'user.pageview' GROUP BY referrer ORDER BY visits DESC LIMIT 10`, XField: "referrer", YField: "visits", SortOrder: 1},
				{Name: "Guest vs identified", Kind: "bar", SQL: guestVsIdentifiedSQL("distinct_id", false), XField: "user_type", YField: "visitors", SortOrder: 2},
				{Name: "Pageviews trend", Kind: "line", Metric: "events", EventName: "user.pageview", SortOrder: 3},
				{Name: "Conversions", Kind: "bar", Metric: "event_breakdown", EventName: "user.conversion", SortOrder: 4},
			},
		},
	}

	for _, tmpl := range templates {
		if _, err := s.pg.Exec(ctx, `
INSERT INTO dashboard_templates (id, name, description, is_system)
VALUES ($1, $2, $3, true)
ON CONFLICT (id) DO UPDATE SET name = EXCLUDED.name, description = EXCLUDED.description, updated_at = now()`,
			tmpl.id, tmpl.name, tmpl.description); err != nil {
			return err
		}
		// Re-seed charts: delete existing and re-insert.
		if _, err := s.pg.Exec(ctx, `DELETE FROM template_charts WHERE template_id = $1`, tmpl.id); err != nil {
			return err
		}
		for _, chart := range tmpl.charts {
			if _, err := s.pg.Exec(ctx, `
INSERT INTO template_charts (template_id, name, kind, metric, event_name, event_type, sql, x_field, y_field, sort_order)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
				tmpl.id, chart.Name, chart.Kind, chart.Metric, chart.EventName, chart.EventType, chart.SQL, chart.XField, chart.YField, chart.SortOrder); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Store) ListTemplates(ctx context.Context, projectID string) ([]DashboardTemplate, error) {
	rows, err := s.pg.Query(ctx, `
SELECT id::text, coalesce(project_id::text, ''), name, description, is_system, created_at, updated_at
FROM dashboard_templates
WHERE project_id IS NULL OR project_id = $1
ORDER BY is_system DESC, created_at ASC`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	templates := []DashboardTemplate{}
	for rows.Next() {
		var t DashboardTemplate
		var pid string
		if err := rows.Scan(&t.ID, &pid, &t.Name, &t.Description, &t.IsSystem, &t.CreatedAt, &t.UpdatedAt); err != nil {
			return nil, err
		}
		if pid != "" {
			t.ProjectID = &pid
		}
		templates = append(templates, t)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	if len(templates) == 0 {
		return templates, nil
	}

	ids := make([]string, len(templates))
	for i, t := range templates {
		ids[i] = t.ID
	}
	placeholders := make([]string, len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		placeholders[i] = fmt.Sprintf("$%d", i+1)
		args[i] = id
	}
	chartRows, err := s.pg.Query(ctx,
		fmt.Sprintf(`SELECT id::text, template_id::text, name, kind, metric, event_name, event_type, sql, x_field, y_field, sort_order FROM template_charts WHERE template_id IN (%s) ORDER BY sort_order ASC`, strings.Join(placeholders, ",")),
		args...)
	if err != nil {
		return nil, err
	}
	defer chartRows.Close()

	chartsByTemplate := map[string][]TemplateChart{}
	for chartRows.Next() {
		var tc TemplateChart
		if err := chartRows.Scan(&tc.ID, &tc.TemplateID, &tc.Name, &tc.Kind, &tc.Metric, &tc.EventName, &tc.EventType, &tc.SQL, &tc.XField, &tc.YField, &tc.SortOrder); err != nil {
			return nil, err
		}
		chartsByTemplate[tc.TemplateID] = append(chartsByTemplate[tc.TemplateID], tc)
	}
	if err := chartRows.Err(); err != nil {
		return nil, err
	}

	for i := range templates {
		templates[i].Charts = chartsByTemplate[templates[i].ID]
		if templates[i].Charts == nil {
			templates[i].Charts = []TemplateChart{}
		}
	}
	return templates, nil
}

func (s *Store) GetTemplate(ctx context.Context, id string) (DashboardTemplate, error) {
	var t DashboardTemplate
	var pid string
	err := s.pg.QueryRow(ctx, `
SELECT id::text, coalesce(project_id::text, ''), name, description, is_system, created_at, updated_at
FROM dashboard_templates WHERE id = $1`, id).
		Scan(&t.ID, &pid, &t.Name, &t.Description, &t.IsSystem, &t.CreatedAt, &t.UpdatedAt)
	if err != nil {
		return DashboardTemplate{}, err
	}
	if pid != "" {
		t.ProjectID = &pid
	}

	chartRows, err := s.pg.Query(ctx, `
SELECT id::text, template_id::text, name, kind, metric, event_name, event_type, sql, x_field, y_field, sort_order
FROM template_charts WHERE template_id = $1 ORDER BY sort_order ASC`, id)
	if err != nil {
		return DashboardTemplate{}, err
	}
	defer chartRows.Close()
	for chartRows.Next() {
		var tc TemplateChart
		if err := chartRows.Scan(&tc.ID, &tc.TemplateID, &tc.Name, &tc.Kind, &tc.Metric, &tc.EventName, &tc.EventType, &tc.SQL, &tc.XField, &tc.YField, &tc.SortOrder); err != nil {
			return DashboardTemplate{}, err
		}
		t.Charts = append(t.Charts, tc)
	}
	if t.Charts == nil {
		t.Charts = []TemplateChart{}
	}
	return t, chartRows.Err()
}

func (s *Store) CloneTemplate(ctx context.Context, templateID string, projectID string) (Dashboard, []Chart, error) {
	tmpl, err := s.GetTemplate(ctx, templateID)
	if err != nil {
		return Dashboard{}, nil, err
	}
	dashboard, err := s.CreateDashboard(ctx, projectID, tmpl.Name, tmpl.Description)
	if err != nil {
		return Dashboard{}, nil, err
	}
	charts := make([]Chart, 0, len(tmpl.Charts))
	for _, tc := range tmpl.Charts {
		created, err := s.CreateChart(ctx, Chart{
			DashboardID: dashboard.ID,
			ProjectID:   projectID,
			Name:        tc.Name,
			Kind:        tc.Kind,
			Metric:      tc.Metric,
			EventName:   tc.EventName,
			EventType:   tc.EventType,
			SQL:         tc.SQL,
			XField:      tc.XField,
			YField:      tc.YField,
		})
		if err != nil {
			return dashboard, charts, err
		}
		charts = append(charts, created)
	}
	return dashboard, charts, nil
}

func (s *Store) CloneTemplateChart(ctx context.Context, templateChartID string, dashboardID string, projectID string) (Chart, error) {
	var dashProjectID string
	err := s.pg.QueryRow(ctx, `SELECT project_id::text FROM dashboards WHERE id = $1`, dashboardID).Scan(&dashProjectID)
	if err != nil {
		return Chart{}, err
	}
	if dashProjectID != projectID {
		return Chart{}, fmt.Errorf("forbidden")
	}

	var tc TemplateChart
	err = s.pg.QueryRow(ctx, `
SELECT id::text, template_id::text, name, kind, metric, event_name, event_type, sql, x_field, y_field, sort_order
FROM template_charts WHERE id = $1`, templateChartID).
		Scan(&tc.ID, &tc.TemplateID, &tc.Name, &tc.Kind, &tc.Metric, &tc.EventName, &tc.EventType, &tc.SQL, &tc.XField, &tc.YField, &tc.SortOrder)
	if err != nil {
		return Chart{}, err
	}

	return s.CreateChart(ctx, Chart{
		DashboardID: dashboardID,
		ProjectID:   projectID,
		Name:        tc.Name,
		Kind:        tc.Kind,
		Metric:      tc.Metric,
		EventName:   tc.EventName,
		EventType:   tc.EventType,
		SQL:         tc.SQL,
		XField:      tc.XField,
		YField:      tc.YField,
	})
}

// SeedProjectFromTemplate seeds a new project from the "Product Overview" system template.
// Falls back to seedStarterDashboard if the template is missing.
func (s *Store) SeedProjectFromTemplate(ctx context.Context, projectID string) error {
	// Seed a capable default agent so a new workspace is productive immediately;
	// non-fatal so a seeding hiccup never blocks signup.
	if err := s.SeedDefaultFoundationAgent(ctx, projectID); err != nil {
		fmt.Printf("warn: SeedDefaultFoundationAgent(%s): %v\n", projectID, err)
	}
	_, _, err := s.CloneTemplate(ctx, ProductOverviewTemplateID, projectID)
	if err != nil {
		if seedErr := seedStarterDashboard(ctx, s.pg, projectID); seedErr != nil {
			return seedErr
		}
	}
	return s.EnsureDefaultBoards(ctx, projectID)
}

// InsertEvents durably stores a raw event batch in DuckDB (no person
// projection). Used by pipeline self-metrics; the ingest worker's path is
// SinkEvents. Idempotent on (project_id, event_id).
func (s *Store) InsertEvents(ctx context.Context, events []Event) error {
	if s.duck == nil {
		return errors.New("storage: duckdb not open")
	}
	return s.duck.InsertEvents(ctx, events)
}

// SinkEvents is the ingest worker's write path: the batch's events and their
// person-profile projection commit in ONE DuckDB transaction, and the error
// drives the JetStream ack/nak/dead-letter decision. Commit-before-ack plus
// the (project_id, event_id) dedup key makes redelivery a no-op, and the
// profile commits atomically with the batch. mark is the durable position the
// batch lands, recorded in that same transaction (see duckdb_position.go).
func (s *Store) SinkEvents(ctx context.Context, events []Event, mark AppliedMark) error {
	if s.duck == nil {
		return errors.New("storage: duckdb not open")
	}
	return s.duck.SinkEvents(ctx, events, mark)
}

// The position record is the store-side half of the readiness contract: it is
// what lets /readyz answer for THIS DuckDB file rather than for the durable's
// own arithmetic. The ingestion worker reads and binds it at boot (see
// internal/dataplane/ingest/readiness.go); the record itself lives in
// duckdb_position.go.
func (s *Store) AppliedPosition(ctx context.Context, durable string) (AppliedPosition, error) {
	if s.duck == nil {
		return AppliedPosition{}, errors.New("storage: duckdb not open")
	}
	return s.duck.AppliedPosition(ctx, durable)
}

// AdoptPosition records the starting position of a file that predates the
// record; RefusePosition writes down a gap a boot proved, so restarting the
// colour does not forget it.
func (s *Store) AdoptPosition(ctx context.Context, durable string, seq uint64) error {
	if s.duck == nil {
		return errors.New("storage: duckdb not open")
	}
	return s.duck.AdoptPosition(ctx, durable, seq)
}

func (s *Store) RefusePosition(ctx context.Context, durable string, missing uint64) error {
	if s.duck == nil {
		return errors.New("storage: duckdb not open")
	}
	return s.duck.RefusePosition(ctx, durable, missing)
}

// RecordPosition advances the applied position for a delivery that settled
// without a row write; see duckdb_position.go.
func (s *Store) RecordPosition(ctx context.Context, mark AppliedMark) error {
	if s.duck == nil {
		return errors.New("storage: duckdb not open")
	}
	return s.duck.RecordPosition(ctx, mark)
}

func (s *Store) CreateAlias(ctx context.Context, projectID, anonymousID, canonicalID string) error {
	_, err := s.pg.Exec(ctx, `
	INSERT INTO aliases (project_id, anonymous_id, canonical_id)
	VALUES ($1, $2, $3)
	ON CONFLICT (project_id, anonymous_id) DO NOTHING`,
		projectID, anonymousID, canonicalID)
	if err != nil {
		return err
	}
	// Postgres is the source of truth; the cached resolver and the DuckDB
	// aliases mirror are derived. Invalidate the cache so a person-scoped
	// filter sees the new alias immediately, and upsert into the mirror (best
	// effort — the boot reconcile re-reads Postgres, so a dropped write
	// self-heals).
	s.resolvers.invalidate(projectID)
	if s.duck != nil {
		if err := s.duck.UpsertAliases(ctx, [][3]string{{projectID, anonymousID, canonicalID}}); err != nil {
			log.Printf("storage: mirror alias to duckdb: %v", err)
		}
	}
	return nil
}

// reconcileAliases loads every Postgres alias and makes the DuckDB mirror
// match it exactly. Runs at boot after OpenDuckDB; CreateAlias keeps the
// mirror current between boots.
func (s *Store) reconcileAliases(ctx context.Context) error {
	if s.duck == nil {
		return nil
	}
	rows, err := s.pg.Query(ctx, `SELECT project_id::text, anonymous_id, canonical_id FROM aliases`)
	if err != nil {
		return err
	}
	defer rows.Close()

	var triples [][3]string
	for rows.Next() {
		var projectID, anonymousID, canonicalID string
		if err := rows.Scan(&projectID, &anonymousID, &canonicalID); err != nil {
			return err
		}
		triples = append(triples, [3]string{projectID, anonymousID, canonicalID})
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return s.duck.ReconcileAliases(ctx, triples)
}

func (s *Store) identityResolver(ctx context.Context, projectID string) (identityResolver, error) {
	if r, ok := s.resolvers.get(projectID); ok {
		return r, nil
	}
	rows, err := s.pg.Query(ctx, `SELECT anonymous_id, canonical_id FROM aliases WHERE project_id = $1`, projectID)
	if err != nil {
		return identityResolver{}, err
	}
	defer rows.Close()

	resolver := identityResolver{}
	for rows.Next() {
		var anonymousID, canonicalID string
		if err := rows.Scan(&anonymousID, &canonicalID); err != nil {
			return identityResolver{}, err
		}
		resolver.anonymousIDs = append(resolver.anonymousIDs, anonymousID)
		resolver.canonicalIDs = append(resolver.canonicalIDs, canonicalID)
	}
	if err := rows.Err(); err != nil {
		return identityResolver{}, err
	}
	s.resolvers.put(projectID, resolver)
	return resolver, nil
}

// canonicalExpr returns the resolved_events column that carries the stitched
// canonical id for a distinct-id column. The DuckDB resolved_events view does
// the job aliases_dict did: a LEFT JOIN on the aliases mirror keyed by
// (project_id, anonymous_id), falling back to the raw distinct id. Callers
// must therefore read FROM resolved_events (or an equivalent aliases join),
// not bare events. Returns no bind args.
func (r identityResolver) canonicalExpr(column string) (string, []any) {
	// The view exposes exactly one stitched column; the argument only carries
	// the caller's table alias (e.g. "e.distinct_id" -> "e.canonical_distinct_id").
	return strings.ReplaceAll(column, "distinct_id", "canonical_distinct_id"), nil
}

// workspaceCanonicalExpr is canonicalExpr for a query that spans a whole
// workspace, where there is no single project to resolve. It is safe because
// the resolved_events view resolves through the aliases mirror keyed on
// (project_id, distinct_id) — identity namespaces stay per-project. The
// Postgres alias list identityResolver loads serves the *other* resolver
// methods (relatedDistinctIDs, canonicalID), which a workspace-wide query
// does not use.
func (s *Store) workspaceCanonicalExpr(column string) string {
	expr, _ := identityResolver{}.canonicalExpr(column)
	return expr
}

// canonicalID resolves a raw distinct id to its stitched canonical id, mirroring
// the read path's resolved_events view (coalesce(aliases.canonical_id, distinct_id)):
// an id that appears as an anonymous alias maps to its canonical, everything else
// (including canonical ids themselves) maps to itself. Single-hop, matching the
// view; used by the person-profile write path to key by canonical id.
func (r identityResolver) canonicalID(distinctID string) string {
	for i, anonymousID := range r.anonymousIDs {
		if anonymousID == distinctID {
			return r.canonicalIDs[i]
		}
	}
	return distinctID
}

func (r identityResolver) relatedDistinctIDs(distinctID string) []string {
	if distinctID == "" {
		return nil
	}
	ids := []string{distinctID}
	seen := map[string]bool{distinctID: true}
	for i, anonymousID := range r.anonymousIDs {
		canonicalID := r.canonicalIDs[i]
		if canonicalID == distinctID && !seen[anonymousID] {
			ids = append(ids, anonymousID)
			seen[anonymousID] = true
		}
		if anonymousID == distinctID && !seen[canonicalID] {
			ids = append(ids, canonicalID)
			seen[canonicalID] = true
		}
	}
	return ids
}

// EventNames returns the distinct event-name catalog for a project, most active
// first, so the UI can offer an autocomplete instead of asking a person to recall
// an exact name. It scans all history (event names are low-cardinality) and is
// safe to cache client-side. limit is capped to keep the payload small.
func (s *Store) EventNames(ctx context.Context, projectID string, limit int) ([]EventCatalogEntry, error) {
	if limit <= 0 || limit > 1000 {
		limit = 500
	}
	// The catalog is read by autocomplete *and* by the first-run written opinion,
	// which speaks in people. So it carries both numbers, and the people number
	// is stitched (one human who logged in mid-session is one person) and
	// human-only (a crawler is not a person). resolved_events supplies the
	// stitched canonical_distinct_id.
	entries := []EventCatalogEntry{}
	err := s.duckQuery(ctx, `
SELECT event_name,
       any_value(event_type) AS event_type,
       count(*) AS cnt,
       count(DISTINCT canonical_distinct_id) FILTER (WHERE coalesce(visitor_class, 'human') = 'human') AS users,
       max("timestamp") AS last_seen
FROM resolved_events
WHERE project_id = ? AND event_name <> ''
GROUP BY event_name
ORDER BY cnt DESC
LIMIT ?`, []any{projectID, limit}, func(rows *sql.Rows) error {
		var e EventCatalogEntry
		if err := rows.Scan(&e.EventName, &e.EventType, &e.Count, &e.Users, &e.LastSeen); err != nil {
			return err
		}
		entries = append(entries, e)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return entries, nil
}

// EventNameSet returns every distinct event name for a project with no volume cap,
// for callers that need the *complete* catalog rather than a ranked top-N. The
// tracking-plan guard uses it: a top-N (EventNames) would misflag a genuinely
// established but low-volume name that fell outside the slice as "unplanned"
// forever. Event names are low-cardinality, so the full set stays small.
func (s *Store) EventNameSet(ctx context.Context, projectID string) (map[string]struct{}, error) {
	out := map[string]struct{}{}
	err := s.duckQuery(ctx, `
SELECT DISTINCT event_name
FROM events
WHERE project_id = ? AND event_name <> ''`, []any{projectID}, func(rows *sql.Rows) error {
		var name string
		if err := rows.Scan(&name); err != nil {
			return err
		}
		out[name] = struct{}{}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (s *Store) RecentEvents(ctx context.Context, projectID string, limit int) ([]Event, error) {
	events := []Event{}
	err := s.duckQuery(ctx, `
SELECT
	project_id::VARCHAR, event_id::VARCHAR, distinct_id, session_id, event_name,
	event_type, properties, is_error, "timestamp", inserted_at, platform
FROM events
WHERE project_id = ?
ORDER BY inserted_at DESC
LIMIT ?`, []any{projectID, limit}, func(rows *sql.Rows) error {
		var event Event
		var isError bool
		var inserted time.Time
		if err := rows.Scan(
			&event.ProjectID,
			&event.EventID,
			&event.DistinctID,
			&event.SessionID,
			&event.EventName,
			&event.EventType,
			&event.Properties,
			&isError,
			&event.Timestamp,
			&inserted,
			&event.Platform,
		); err != nil {
			return err
		}
		event.IsError = isError
		event.InsertedAt = &inserted
		events = append(events, event)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return events, nil
}

func (s *Store) RecentSessions(ctx context.Context, projectID string, limit int) ([]Session, error) {
	// sessions is the ingest-derived view (the sessions_mv replacement); the
	// canonical id stitches through the aliases mirror, same as resolved_events.
	sessions := []Session{}
	err := s.duckQuery(ctx, `
SELECT
	s.project_id::VARCHAR,
	s.session_id,
	coalesce(a.canonical_id, s.distinct_id) AS canonical_distinct_id,
	min(s.session_start) AS session_start,
	max(s.session_end) AS session_end,
	sum(s.event_count) AS event_count,
	sum(s.total_tokens_in) AS total_tokens_in,
	sum(s.total_tokens_out) AS total_tokens_out,
	sum(s.total_cost_usd) AS total_cost_usd,
	max(s.last_event_at) AS last_event_at
FROM sessions s
LEFT JOIN aliases a
	ON a.project_id = s.project_id AND a.anonymous_id = s.distinct_id
WHERE s.project_id = ?
GROUP BY s.project_id, s.session_id, canonical_distinct_id
ORDER BY last_event_at DESC
LIMIT ?`, []any{projectID, limit}, func(rows *sql.Rows) error {
		var session Session
		var lastEventAt time.Time
		if err := rows.Scan(
			&session.ProjectID,
			&session.SessionID,
			&session.DistinctID,
			&session.SessionStart,
			&session.SessionEnd,
			&session.EventCount,
			&session.TotalTokensIn,
			&session.TotalTokensOut,
			&session.TotalCostUSD,
			&lastEventAt,
		); err != nil {
			return err
		}
		session.LastEventAt = &lastEventAt
		sessions = append(sessions, session)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return sessions, nil
}

// distinctPlatforms lists the apps that sent events in the window, busiest
// first. Bounded by the column's small value set (a handful of apps), so no
// LIMIT is needed for it to stay cheap.
func (s *Store) distinctPlatforms(ctx context.Context, where string, args []any) ([]string, error) {
	result := []string{}
	err := s.duckQuery(ctx, `
SELECT if(coalesce(platform, '') = '', 'unknown', platform) AS platform, count(*) AS events
FROM events
WHERE `+where+`
GROUP BY platform
ORDER BY events DESC`, args, func(rows *sql.Rows) error {
		var platform string
		var events uint64
		if err := rows.Scan(&platform, &events); err != nil {
			return err
		}
		result = append(result, platform)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func (s *Store) ActivitySummary(ctx context.Context, projectID string, filter EventFilter) (ActivitySummary, error) {
	summary := ActivitySummary{
		ProjectID:       projectID,
		GeneratedAt:     time.Now().UTC(),
		EventsByType:    map[string]uint64{},
		EmptySinceHours: emptySinceHours(filter),
	}
	resolver, err := s.identityResolver(ctx, projectID)
	if err != nil {
		return summary, err
	}
	where, args := filteredWhereWithDistinctIDs(projectID, filter, true, resolver.relatedDistinctIDs(filter.DistinctID))

	// DistinctUsers renders as "People" on the dashboard, so it counts humans —
	// the same rule Persons and the funnel already apply. It is filtered inside
	// the aggregate rather than through filter.HumansOnly because the sibling
	// numbers on this row (EventCount, the per-type breakdowns, tokens, cost) are
	// ingestion and spend figures: a crawler's events were still ingested and
	// still cost money, and hiding them would make the dashboard disagree with
	// the bill. resolved_events supplies the stitched canonical_distinct_id.
	err = s.duckQueryRow(ctx, `
	SELECT
		count(*),
		count(*) FILTER (WHERE event_type = 'user'),
		count(*) FILTER (WHERE event_type = 'agent'),
		count(*) FILTER (WHERE event_type = 'system'),
		count(DISTINCT session_id) FILTER (WHERE session_id <> ''),
		count(DISTINCT canonical_distinct_id) FILTER (WHERE coalesce(visitor_class, 'human') = 'human'),
		coalesce(sum(tokens_input), 0),
		coalesce(sum(tokens_output), 0),
		coalesce(sum(cost_usd), 0)
	FROM resolved_events
	WHERE `+where, args,
		&summary.EventCount,
		&summary.UserEvents,
		&summary.AgentEvents,
		&summary.SystemEvents,
		&summary.Sessions,
		&summary.DistinctUsers,
		&summary.TotalTokensIn,
		&summary.TotalTokensOut,
		&summary.TotalCostUSD,
	)
	if err != nil {
		return summary, err
	}
	summary.EventsByType["user"] = summary.UserEvents
	summary.EventsByType["agent"] = summary.AgentEvents
	summary.EventsByType["system"] = summary.SystemEvents

	unscoped := filter
	unscoped.Platform = ""
	platformWhere, platformArgs := filteredWhereWithDistinctIDs(projectID, unscoped, true, resolver.relatedDistinctIDs(unscoped.DistinctID))
	platforms, err := s.distinctPlatforms(ctx, platformWhere, platformArgs)
	if err != nil {
		return summary, err
	}
	summary.Platforms = platforms

	eventCounts, err := s.eventCounts(ctx, where, args)
	if err != nil {
		return summary, err
	}
	summary.EventCounts = eventCounts

	timeline, err := s.timeline(ctx, where, args)
	if err != nil {
		return summary, err
	}
	summary.Timeline = timeline

	topAgents, err := s.topAgents(ctx, where, args)
	if err != nil {
		return summary, err
	}
	summary.TopAgents = topAgents

	recentFilter := filter
	recentFilter.Limit = 25
	explorer, err := s.ExploreEvents(ctx, projectID, recentFilter)
	if err != nil {
		return summary, err
	}
	summary.RecentEvents = explorer.Events

	recentSessions, err := s.recentSessions(ctx, resolver, where, args, 25)
	if err != nil {
		return summary, err
	}
	summary.RecentSessions = recentSessions

	return summary, nil
}

func (s *Store) eventCounts(ctx context.Context, where string, args []any) ([]EventCount, error) {
	counts := []EventCount{}
	err := s.duckQuery(ctx, `
	SELECT event_name, count(*) AS count
	FROM events
	WHERE `+where+`
	GROUP BY event_name
	ORDER BY count DESC
	LIMIT 20`, args, func(rows *sql.Rows) error {
		var item EventCount
		if err := rows.Scan(&item.EventName, &item.Count); err != nil {
			return err
		}
		counts = append(counts, item)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return counts, nil
}

func (s *Store) timeline(ctx context.Context, where string, args []any) ([]TimelinePoint, error) {
	points := []TimelinePoint{}
	err := s.duckQuery(ctx, `
	SELECT date_trunc('hour', "timestamp") AS hour, count(*) AS count
	FROM events
	WHERE `+where+`
	GROUP BY hour
	ORDER BY hour ASC`, args, func(rows *sql.Rows) error {
		var point TimelinePoint
		if err := rows.Scan(&point.Hour, &point.Count); err != nil {
			return err
		}
		points = append(points, point)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return points, nil
}

func (s *Store) topAgents(ctx context.Context, where string, args []any) ([]AgentMetric, error) {
	agents := []AgentMetric{}
	err := s.duckQuery(ctx, `
	SELECT
		coalesce(agent_id, 'unknown') AS agent_id,
		count(*) AS event_count,
		coalesce(sum(cost_usd), 0) AS total_cost_usd,
		coalesce(avg(coalesce(latency_ms, 0)::DOUBLE), 0) AS avg_latency_ms
	FROM events
	WHERE `+where+` AND event_type = 'agent'
	GROUP BY agent_id
	ORDER BY total_cost_usd DESC, event_count DESC
	LIMIT 10`, args, func(rows *sql.Rows) error {
		var agent AgentMetric
		if err := rows.Scan(&agent.AgentID, &agent.EventCount, &agent.TotalCostUSD, &agent.AvgLatencyMS); err != nil {
			return err
		}
		agents = append(agents, agent)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return agents, nil
}

func (s *Store) recentSessions(ctx context.Context, resolver identityResolver, where string, args []any, limit int) ([]Session, error) {
	queryArgs := append(append([]any{}, args...), limit)
	sessions := []Session{}
	err := s.duckQuery(ctx, `
	SELECT
		project_id::VARCHAR,
		session_id,
		any_value(canonical_distinct_id) AS session_distinct_id,
		min("timestamp") AS session_start,
		max("timestamp") AS session_end,
		count(*) AS event_count,
		coalesce(sum(tokens_input), 0) AS total_tokens_in,
		coalesce(sum(tokens_output), 0) AS total_tokens_out,
		coalesce(sum(cost_usd), 0) AS total_cost_usd,
		max("timestamp") AS last_event_at
	FROM resolved_events
	WHERE `+where+` AND session_id <> ''
	GROUP BY project_id, session_id
	ORDER BY last_event_at DESC
	LIMIT ?`, queryArgs, func(rows *sql.Rows) error {
		var session Session
		var lastEventAt time.Time
		if err := rows.Scan(
			&session.ProjectID,
			&session.SessionID,
			&session.DistinctID,
			&session.SessionStart,
			&session.SessionEnd,
			&session.EventCount,
			&session.TotalTokensIn,
			&session.TotalTokensOut,
			&session.TotalCostUSD,
			&lastEventAt,
		); err != nil {
			return err
		}
		session.LastEventAt = &lastEventAt
		sessions = append(sessions, session)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return sessions, nil
}

func (s *Store) RunInsight(ctx context.Context, projectID string, insightType string, metric string, steps []string, filter EventFilter) (InsightResult, error) {
	// "timeseries" is the name the agent tool and its docs use; "trend" is the
	// name the REST/UI path uses. They mean the same insight — accept either (and
	// an empty type) so a documented call never falls through to "unsupported".
	if insightType == "" || insightType == "timeseries" {
		insightType = "trend"
	}
	result := InsightResult{
		Type:      insightType,
		Metric:    metric,
		Generated: time.Now().UTC(),
	}
	switch insightType {
	case "trend":
		series, err := s.filteredTimeline(ctx, projectID, filter)
		result.Title = "Trend"
		result.Series = series
		return result, err
	case "funnel":
		funnel, err := s.funnel(ctx, projectID, steps, filter)
		result.Title = "Funnel"
		result.Funnel = funnel
		return result, err
	case "retention":
		// The anchor is an *event name*. `metric` is one of events/users/sessions,
		// so feeding it in here made every unfiltered retention query ask about an
		// event literally named "events" — which nothing emits, so the curve was
		// structurally 0% while the UI still printed "Week 0: 100%". Steps are the
		// caller's event names, so the first one is the cohort-defining event.
		retention, err := s.retention(ctx, projectID, firstNonEmpty(filter.EventName, firstStep(steps), "user.pageview"), filter)
		result.Title = "Retention"
		result.Retention = retention
		return result, err
	case "agent":
		rows, err := s.agentInsightRows(ctx, projectID, filter)
		result.Title = "Agent cost and latency"
		result.Rows = rows
		return result, err
	case "table":
		events, err := s.ExploreEvents(ctx, projectID, filter)
		result.Title = "Event table"
		for _, event := range events.Events {
			result.Rows = append(result.Rows, eventToMap(event))
		}
		return result, err
	default:
		return result, fmt.Errorf("unsupported insight type: %s", insightType)
	}
}

func (s *Store) WebAnalytics(ctx context.Context, projectID string, filter EventFilter) (WebAnalytics, error) {
	web := WebAnalytics{Generated: time.Now().UTC()}
	resolver, err := s.identityResolver(ctx, projectID)
	if err != nil {
		return web, err
	}
	where, args := filteredWhereWithDistinctIDs(projectID, filter, true, resolver.relatedDistinctIDs(filter.DistinctID))
	err = s.duckQueryRow(ctx, `
SELECT
	count(DISTINCT canonical_distinct_id),
	count(*) FILTER (WHERE event_name = 'user.pageview'),
	count(DISTINCT session_id) FILTER (WHERE session_id <> ''),
	count(*) FILTER (WHERE event_name IN ('user.conversion', 'user.signup'))
FROM resolved_events
WHERE `+where, args, &web.Visitors, &web.Pageviews, &web.Sessions, &web.Conversions)
	if err != nil {
		return web, err
	}
	if web.Visitors == 0 && web.Pageviews == 0 && web.Sessions == 0 && web.Conversions == 0 {
		return web, nil
	}

	duration, bounce, err := s.sessionQuality(ctx, projectID, filter)
	if err != nil {
		return web, err
	}
	web.AvgSessionDuration = duration
	web.BounceRate = bounce
	if math.IsNaN(web.AvgSessionDuration) || math.IsInf(web.AvgSessionDuration, 0) {
		web.AvgSessionDuration = 0
	}
	if math.IsNaN(web.BounceRate) || math.IsInf(web.BounceRate, 0) {
		web.BounceRate = 0
	}

	paths, err := s.propertyCounts(ctx, projectID, filter, "path", "user.pageview")
	if err != nil {
		return web, err
	}
	web.TopPaths = paths

	referrers, err := s.externalReferrers(ctx, where, args)
	if err != nil {
		return web, err
	}
	web.Referrers = referrers

	trafficByClass, err := s.trafficByClass(ctx, where, args)
	if err != nil {
		return web, err
	}
	web.TrafficByClass = trafficByClass

	trafficByPlatform, err := s.trafficByPlatform(ctx, resolver, where, args)
	if err != nil {
		return web, err
	}
	web.TrafficByPlatform = trafficByPlatform

	trafficByProvider, err := s.trafficByProvider(ctx, resolver, where, args)
	if err != nil {
		return web, err
	}
	web.TrafficByProvider = trafficByProvider

	aiTopPaths, err := s.aiTopPaths(ctx, where, args)
	if err != nil {
		return web, err
	}
	web.AITopPaths = aiTopPaths

	refByChannel, err := s.referrersByChannel(ctx, where, args)
	if err != nil {
		return web, err
	}
	web.ReferrersByChannel = refByChannel

	guestVsUser, err := s.guestVsUser(ctx, resolver, where, args)
	if err != nil {
		return web, err
	}
	web.GuestVsUser = guestVsUser

	return web, nil
}

func (s *Store) trafficByClass(ctx context.Context, where string, args []any) ([]TrafficClass, error) {
	result := []TrafficClass{}
	err := s.duckQuery(ctx, `
SELECT coalesce(visitor_class, 'human') AS class, count(*) AS count
FROM events
WHERE `+where+` AND event_name = 'user.pageview'
GROUP BY class
ORDER BY count DESC`, args, func(rows *sql.Rows) error {
		var item TrafficClass
		if err := rows.Scan(&item.Class, &item.Count); err != nil {
			return err
		}
		result = append(result, item)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// trafficByPlatform splits the audience by which app the events came from. It
// counts people by canonical id, so one human who uses the site and the app is
// one visitor on each platform and not two humans overall — the same identity
// resolution every other people metric uses.
func (s *Store) trafficByPlatform(ctx context.Context, resolver identityResolver, where string, args []any) ([]PlatformSplit, error) {
	result := []PlatformSplit{}
	err := s.duckQuery(ctx, `
SELECT
	if(coalesce(platform, '') = '', 'unknown', platform) AS platform,
	count(DISTINCT canonical_distinct_id) AS visitors,
	count(*) FILTER (WHERE event_name = 'user.pageview') AS pageviews,
	count(*) AS events
FROM resolved_events
WHERE `+where+`
GROUP BY platform
ORDER BY visitors DESC, events DESC`, args, func(rows *sql.Rows) error {
		var item PlatformSplit
		if err := rows.Scan(&item.Platform, &item.Visitors, &item.Pageviews, &item.Events); err != nil {
			return err
		}
		result = append(result, item)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func (s *Store) trafficByProvider(ctx context.Context, resolver identityResolver, where string, args []any) ([]TrafficProvider, error) {
	result := []TrafficProvider{}
	err := s.duckQuery(ctx, `
SELECT
	class,
	provider,
	count(DISTINCT canonical_distinct_id) AS visitors,
	count(*) AS pageviews
FROM (
	SELECT
		project_id,
		canonical_distinct_id,
		coalesce(visitor_class, 'human') AS class,
		CASE
			WHEN coalesce(bot_name, '') <> '' THEN coalesce(bot_name, '')
			WHEN referrer_channel = 'ai-referral' AND coalesce(referrer_host, '') <> '' THEN coalesce(referrer_host, '')
			ELSE coalesce(visitor_class, 'human')
		END AS provider
	FROM resolved_events
	WHERE `+where+` AND event_name = 'user.pageview'
)
GROUP BY class, provider
ORDER BY pageviews DESC, visitors DESC
LIMIT 20`, args, func(rows *sql.Rows) error {
		var item TrafficProvider
		if err := rows.Scan(&item.Class, &item.Provider, &item.Visitors, &item.Pageviews); err != nil {
			return err
		}
		result = append(result, item)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func (s *Store) aiTopPaths(ctx context.Context, where string, args []any) ([]PathCount, error) {
	result := []PathCount{}
	err := s.duckQuery(ctx, `
SELECT value, count(*) AS count
FROM (
	SELECT coalesce(json_extract_string(properties, '$.path'), '') AS value
	FROM events
	WHERE `+where+` AND event_name = 'user.pageview'
		AND (visitor_class = 'ai-platform' OR referrer_channel = 'ai-referral')
)
WHERE value <> ''
GROUP BY value
ORDER BY count DESC
LIMIT 10`, args, func(rows *sql.Rows) error {
		var item PathCount
		if err := rows.Scan(&item.Value, &item.Count); err != nil {
			return err
		}
		result = append(result, item)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func (s *Store) externalReferrers(ctx context.Context, where string, args []any) ([]PathCount, error) {
	result := []PathCount{}
	err := s.duckQuery(ctx, `
SELECT coalesce(referrer_host, '') AS value, count(*) AS count
FROM events
WHERE `+where+` AND event_name = 'user.pageview'
	AND referrer_channel NOT IN ('', 'direct', 'internal')
	AND referrer_host IS NOT NULL AND referrer_host <> ''
GROUP BY value
ORDER BY count DESC
LIMIT 20`, args, func(rows *sql.Rows) error {
		var item PathCount
		if err := rows.Scan(&item.Value, &item.Count); err != nil {
			return err
		}
		result = append(result, item)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func (s *Store) referrersByChannel(ctx context.Context, where string, args []any) ([]PathCount, error) {
	result := []PathCount{}
	err := s.duckQuery(ctx, `
SELECT coalesce(referrer_channel, '') AS channel, count(*) AS count
FROM events
WHERE `+where+` AND event_name = 'user.pageview'
	AND referrer_channel <> '' AND referrer_channel <> 'direct' AND referrer_channel <> 'internal'
GROUP BY channel
ORDER BY count DESC`, args, func(rows *sql.Rows) error {
		var item PathCount
		if err := rows.Scan(&item.Value, &item.Count); err != nil {
			return err
		}
		result = append(result, item)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func (s *Store) guestVsUser(ctx context.Context, resolver identityResolver, where string, args []any) (GuestUser, error) {
	var gv GuestUser
	err := s.duckQueryRow(ctx, `
SELECT
	count(*) FILTER (WHERE has_traits) AS users,
	count(*) FILTER (WHERE NOT has_traits) AS guests
FROM (
	SELECT
		canonical_distinct_id,
		max(email_trait <> '' OR name_trait <> '') AS has_traits
	FROM (
		SELECT
			canonical_distinct_id,
			coalesce(nullif(json_extract_string(properties, '$.email'), ''), json_extract_string(properties, '$."$set".email'), '') AS email_trait,
			coalesce(nullif(json_extract_string(properties, '$.name'), ''), json_extract_string(properties, '$."$set".name'), '') AS name_trait
		FROM resolved_events
		WHERE `+where+`
	)
	GROUP BY canonical_distinct_id
)`, args, &gv.Users, &gv.Guests)
	return gv, err
}

// Persons aggregates distinct users from raw events. Identity traits (email,
// name) come from $identify events and event properties: top-level keys first,
// then the PostHog-style $set payload.
func (s *Store) Persons(ctx context.Context, projectID string, filter EventFilter) (PersonsSummary, error) {
	if filter.Limit <= 0 {
		filter.Limit = 100
	}
	if filter.Limit > 500 {
		filter.Limit = 500
	}
	summary := PersonsSummary{Persons: []Person{}, ActiveTimeline: []TimelinePoint{}, Generated: time.Now().UTC()}
	// Persons answers "who are my customers"; crawlers are not people.
	filter.HumansOnly = true
	resolver, err := s.identityResolver(ctx, projectID)
	if err != nil {
		return summary, err
	}
	where, args := filteredWhereWithDistinctIDs(projectID, filter, true, resolver.relatedDistinctIDs(filter.DistinctID))

	traitSource := `
SELECT
	canonical_distinct_id, session_id, event_name, "timestamp", platform,
	coalesce(nullif(json_extract_string(properties, '$.email'), ''), json_extract_string(properties, '$."$set".email'), '') AS email_trait,
	coalesce(nullif(json_extract_string(properties, '$.name'), ''), json_extract_string(properties, '$."$set".name'), '') AS name_trait
FROM resolved_events
WHERE ` + where

	if err := s.duckQueryRow(ctx, `
SELECT
	count(*),
	count(*) FILTER (WHERE has_traits)
FROM (
	SELECT
		canonical_distinct_id,
		max(email_trait <> '' OR name_trait <> '') AS has_traits
	FROM (`+traitSource+`)
	GROUP BY canonical_distinct_id
)`, args, &summary.Total, &summary.Identified); err != nil {
		return summary, err
	}
	summary.Anonymous = summary.Total - summary.Identified

	err = s.duckQuery(ctx, `
SELECT date_trunc('hour', "timestamp") AS hour, count(DISTINCT canonical_distinct_id) AS users
FROM resolved_events
WHERE `+where+`
GROUP BY hour
ORDER BY hour ASC`, args, func(rows *sql.Rows) error {
		var point TimelinePoint
		if err := rows.Scan(&point.Hour, &point.Count); err != nil {
			return err
		}
		summary.ActiveTimeline = append(summary.ActiveTimeline, point)
		return nil
	})
	if err != nil {
		return summary, err
	}

	personArgs := append(append([]any{}, args...), filter.Limit)
	err = s.duckQuery(ctx, `
SELECT
	canonical_distinct_id,
	coalesce(arg_max(email_trait, "timestamp") FILTER (WHERE email_trait <> ''), '') AS email,
	coalesce(arg_max(name_trait, "timestamp") FILTER (WHERE name_trait <> ''), '') AS name,
	min("timestamp") AS first_seen,
	max("timestamp") AS last_seen,
	count(*) AS event_count,
	count(DISTINCT session_id) FILTER (WHERE session_id <> '') AS sessions,
	coalesce(arg_max(event_name, "timestamp"), '') AS last_event_name,
	list_sort(list_distinct(list_filter(array_agg(platform), p -> p <> ''))) AS platforms
FROM (`+traitSource+`)
GROUP BY canonical_distinct_id
ORDER BY (email <> '' OR name <> '') DESC, last_seen DESC
LIMIT ?`, personArgs, func(rows *sql.Rows) error {
		var person Person
		var platforms any
		if err := rows.Scan(
			&person.DistinctID, &person.Email, &person.Name,
			&person.FirstSeen, &person.LastSeen,
			&person.EventCount, &person.Sessions, &person.LastEventName,
			&platforms,
		); err != nil {
			return err
		}
		person.Platforms = duckList(platforms)
		summary.Persons = append(summary.Persons, person)
		return nil
	})
	if err != nil {
		return summary, err
	}

	// Enrich the returned page (bounded ≤500) with merged profile traits from the
	// person store so custom $set/$set_once properties are visible, not just the
	// hard-coded email/name. Best-effort: a profile read failure leaves the list
	// intact with nil Traits.
	if len(summary.Persons) > 0 {
		ids := make([]string, len(summary.Persons))
		for i, p := range summary.Persons {
			ids[i] = p.DistinctID
		}
		if s.duck != nil {
			if profiles, err := s.duck.PersonProfilesByKeys(ctx, projectID, ids); err == nil {
				for i := range summary.Persons {
					prof := profiles[summary.Persons[i].DistinctID]
					if prof == nil {
						continue
					}
					traits := map[string]json.RawMessage{}
					for k, v := range prof.OnceProps {
						traits[k] = v
					}
					for k, v := range prof.SetProps {
						traits[k] = v
					}
					if len(traits) > 0 {
						summary.Persons[i].Traits = traits
					}
				}
			}
		}
	}

	return summary, nil
}

func (s *Store) ExploreEvents(ctx context.Context, projectID string, filter EventFilter) (EventExplorer, error) {
	return s.exploreEvents(ctx, projectID, filter, true)
}

func (s *Store) exploreEvents(ctx context.Context, projectID string, filter EventFilter, defaultTimeWindow bool) (EventExplorer, error) {
	if filter.Limit <= 0 {
		filter.Limit = 100
	}
	if filter.Limit > 500 {
		filter.Limit = 500
	}
	resolver, err := s.identityResolver(ctx, projectID)
	if err != nil {
		return EventExplorer{}, err
	}
	where, whereArgs := filteredWhereWithDistinctIDs(projectID, filter, defaultTimeWindow, resolver.relatedDistinctIDs(filter.DistinctID))
	// Copied, not appended in place: whereArgs is reused verbatim by the schema
	// query below, and appending the row limit onto its backing array would bind
	// that limit as a WHERE argument there.
	args := append(append([]any{}, whereArgs...), filter.Limit)
	explorer := EventExplorer{Schema: []EventSchemaEntry{}, Events: []Event{}, Timeline: []Event{}, Generated: time.Now().UTC()}
	err = s.duckQuery(ctx, `
SELECT
	project_id::VARCHAR, event_id::VARCHAR, distinct_id, session_id, event_name,
	event_type, properties, coalesce(agent_id, ''), coalesce(tool_name, ''),
	coalesce(tool_input, ''), coalesce(tool_output, ''),
	coalesce(tokens_input, 0), coalesce(tokens_output, 0),
	coalesce(cost_usd, 0)::DOUBLE, coalesce(latency_ms, 0),
	coalesce(model_name, ''), is_error, coalesce(error_message, ''), "timestamp", inserted_at, is_unplanned,
	platform
FROM events
WHERE `+where+`
ORDER BY "timestamp" DESC
LIMIT ?`, args, func(rows *sql.Rows) error {
		event, err := scanEvent(rows)
		if err != nil {
			return err
		}
		explorer.Events = append(explorer.Events, event)
		return nil
	})
	if err != nil {
		return explorer, err
	}

	schema, err := s.eventSchema(ctx, resolver, where, whereArgs)
	if err != nil {
		return explorer, err
	}
	explorer.Schema = schema

	if filter.SessionID != "" || filter.DistinctID != "" {
		timelineFilter := filter
		timelineFilter.Limit = 200
		timelineFilter.Search = ""
		timelineWhere, timelineArgs := filteredWhereWithDistinctIDs(projectID, timelineFilter, defaultTimeWindow, resolver.relatedDistinctIDs(timelineFilter.DistinctID))
		timelineArgs = append(timelineArgs, timelineFilter.Limit)
		err := s.duckQuery(ctx, `
SELECT
	project_id::VARCHAR, event_id::VARCHAR, distinct_id, session_id, event_name,
	event_type, properties, coalesce(agent_id, ''), coalesce(tool_name, ''),
	coalesce(tool_input, ''), coalesce(tool_output, ''),
	coalesce(tokens_input, 0), coalesce(tokens_output, 0),
	coalesce(cost_usd, 0)::DOUBLE, coalesce(latency_ms, 0),
	coalesce(model_name, ''), is_error, coalesce(error_message, ''), "timestamp", inserted_at, is_unplanned,
	platform
FROM events
WHERE `+timelineWhere+`
ORDER BY "timestamp" ASC
LIMIT ?`, timelineArgs, func(rows *sql.Rows) error {
			event, err := scanEvent(rows)
			if err != nil {
				return err
			}
			explorer.Timeline = append(explorer.Timeline, event)
			return nil
		})
		if err != nil {
			return explorer, err
		}
	}
	return explorer, nil
}

// eventSchemaLimit caps the catalog one explore returns. A project with more
// distinct event names than this has a tracking-plan problem rather than a
// schema, and the tail is noise to anything reading it.
const eventSchemaLimit = 100

// eventSchemaKeyLimit caps the property keys listed per event. The point of the
// list is to stop a reader guessing key names, which the first two dozen do;
// past that it is one event's payload crowding out every other event's.
const eventSchemaKeyLimit = 25

// eventSchema summarizes the same window the explore sampled: one row per event
// name, with volume, people, span, and property keys. It runs over the caller's
// WHERE clause so the summary describes exactly the slice the sample came from —
// a schema for a different window would be worse than none, because it would be
// believed.
func (s *Store) eventSchema(ctx context.Context, resolver identityResolver, where string, whereArgs []any) ([]EventSchemaEntry, error) {
	args := append(append([]any{}, whereArgs...), eventSchemaLimit)
	out := []EventSchemaEntry{}
	err := s.duckQuery(ctx, `
SELECT
	event_name,
	any_value(event_type) AS event_type,
	count(*) AS events,
	count(DISTINCT canonical_distinct_id) FILTER (WHERE coalesce(visitor_class, 'human') = 'human') AS people,
	min("timestamp") AS first_seen,
	max("timestamp") AS last_seen,
	list_sort(list_distinct(flatten(array_agg(json_keys(properties)))))[1:`+strconv.Itoa(eventSchemaKeyLimit)+`] AS property_keys
FROM resolved_events
WHERE `+where+`
GROUP BY event_name
ORDER BY events DESC
LIMIT ?`, args, func(rows *sql.Rows) error {
		var e EventSchemaEntry
		var keys any
		if err := rows.Scan(&e.EventName, &e.EventType, &e.Events, &e.People, &e.FirstSeen, &e.LastSeen, &keys); err != nil {
			return err
		}
		e.PropertyKeys = duckList(keys)
		out = append(out, e)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (s *Store) AgentReplay(ctx context.Context, projectID string, sessionID string) (AgentReplay, error) {
	filter := EventFilter{SessionID: sessionID, Limit: 500}
	explorer, err := s.exploreEvents(ctx, projectID, filter, false)
	if err != nil {
		return AgentReplay{}, err
	}
	replay := AgentReplay{SessionID: sessionID}
	for i := len(explorer.Events) - 1; i >= 0; i-- {
		event := explorer.Events[i]
		replay.Events = append(replay.Events, event)
		replay.EventCount++
		replay.DistinctID = event.DistinctID
		if event.TokensInput != nil {
			replay.TotalTokensIn += uint64(*event.TokensInput)
		}
		if event.TokensOutput != nil {
			replay.TotalTokensOut += uint64(*event.TokensOutput)
		}
		if event.CostUSD != nil {
			replay.TotalCostUSD += float64(*event.CostUSD)
		}
	}
	return replay, nil
}

func (s *Store) ListSavedQueries(ctx context.Context, projectID string) ([]SavedQuery, error) {
	rows, err := s.pg.Query(ctx, `
SELECT id::text, project_id::text, natural_language, generated_sql, verified, COALESCE(result_cache, 'null'::jsonb), created_at
FROM saved_queries
WHERE project_id = $1
ORDER BY created_at DESC`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	queries := []SavedQuery{}
	for rows.Next() {
		var q SavedQuery
		if err := rows.Scan(&q.ID, &q.ProjectID, &q.NaturalLanguage, &q.GeneratedSQL, &q.Verified, &q.ResultCache, &q.CreatedAt); err != nil {
			return nil, err
		}
		queries = append(queries, q)
	}
	return queries, rows.Err()
}

func (s *Store) CreateSavedQuery(ctx context.Context, projectID string, naturalLanguage string, sqlText string, verified bool) (SavedQuery, error) {
	sqlText = strings.TrimRight(strings.TrimSpace(sqlText), ";")
	if err := validateReadonlySQL(sqlText); err != nil {
		return SavedQuery{}, err
	}
	var q SavedQuery
	err := s.pg.QueryRow(ctx, `
INSERT INTO saved_queries (project_id, natural_language, generated_sql, verified)
VALUES ($1, $2, $3, $4)
RETURNING id::text, project_id::text, natural_language, generated_sql, verified, COALESCE(result_cache, 'null'::jsonb), created_at`,
		projectID, naturalLanguage, sqlText, verified).
		Scan(&q.ID, &q.ProjectID, &q.NaturalLanguage, &q.GeneratedSQL, &q.Verified, &q.ResultCache, &q.CreatedAt)
	return q, err
}

// RunSavedQuery executes a stored query and returns its rows.
//
// cacheResult decides whether the run also refreshes the query's memoized
// result. It is the CALLER's decision because running a saved query is a read
// for the person doing it and a write to the owner's row — and in the shared
// demo those are two different people. A read-only member gets the rows and
// leaves the owner's cache alone.
func (s *Store) RunSavedQuery(ctx context.Context, projectID string, queryID string, cacheResult bool) (SavedQueryResult, error) {
	var q SavedQuery
	err := s.pg.QueryRow(ctx, `
SELECT id::text, project_id::text, natural_language, generated_sql, verified, COALESCE(result_cache, 'null'::jsonb), created_at
FROM saved_queries
WHERE project_id = $1 AND id = $2`, projectID, queryID).
		Scan(&q.ID, &q.ProjectID, &q.NaturalLanguage, &q.GeneratedSQL, &q.Verified, &q.ResultCache, &q.CreatedAt)
	if err != nil {
		return SavedQueryResult{}, err
	}
	rows, err := s.RunSQL(ctx, projectID, q.GeneratedSQL)
	if err != nil {
		return SavedQueryResult{}, err
	}
	if cacheResult {
		cache, _ := json.Marshal(rows)
		_, _ = s.pg.Exec(ctx, `UPDATE saved_queries SET result_cache = $3 WHERE project_id = $1 AND id = $2`, projectID, queryID, cache)
	}
	return SavedQueryResult{Query: q, Rows: rows, Generated: time.Now().UTC()}, nil
}

func (s *Store) DeleteSavedQuery(ctx context.Context, projectID string, queryID string) error {
	_, err := s.pg.Exec(ctx, `DELETE FROM saved_queries WHERE project_id = $1 AND id = $2`, projectID, queryID)
	return err
}

// RenameSavedQuery updates only the human label (natural_language); the SQL body
// is immutable here — re-saving from the editor is the way to change the query.
func (s *Store) RenameSavedQuery(ctx context.Context, projectID string, queryID string, naturalLanguage string) (SavedQuery, error) {
	var q SavedQuery
	err := s.pg.QueryRow(ctx, `
UPDATE saved_queries SET natural_language = $3
WHERE project_id = $1 AND id = $2
RETURNING id::text, project_id::text, natural_language, generated_sql, verified, COALESCE(result_cache, 'null'::jsonb), created_at`,
		projectID, queryID, naturalLanguage).
		Scan(&q.ID, &q.ProjectID, &q.NaturalLanguage, &q.GeneratedSQL, &q.Verified, &q.ResultCache, &q.CreatedAt)
	return q, err
}

func (s *Store) RunSQL(ctx context.Context, projectID string, sqlText string) ([]map[string]any, error) {
	// Soft-delete rules only matter when the query reads external_rows — skip
	// the Postgres round-trip for the common events-only query.
	var rules []softDeleteRule
	if externalSourcePattern.MatchString(sqlText) {
		var err error
		if rules, err = s.softDeleteRulesForProject(ctx, projectID); err != nil {
			return nil, err
		}
	}
	query, args, err := scopedReadonlySQL(sqlText, projectID, rules)
	if err != nil {
		return nil, err
	}
	if !strings.Contains(strings.ToLower(query), "limit") {
		query += " LIMIT 100"
	}
	// Untrusted SQL runs inside the project's sandbox: an in-memory DuckDB
	// holding only this project's rows, on a locked-down connection. See
	// duckdb_sandbox.go.
	return s.sandboxes.query(ctx, projectID, query, args)
}

func (s *Store) filteredTimeline(ctx context.Context, projectID string, filter EventFilter) ([]TimelinePoint, error) {
	resolver, err := s.identityResolver(ctx, projectID)
	if err != nil {
		return nil, err
	}
	where, args := filteredWhereWithDistinctIDs(projectID, filter, true, resolver.relatedDistinctIDs(filter.DistinctID))
	points := []TimelinePoint{}
	err = s.duckQuery(ctx, `
SELECT date_trunc('hour', "timestamp") AS hour, count(*) AS count
FROM events
WHERE `+where+`
GROUP BY hour
ORDER BY hour ASC`, args, func(rows *sql.Rows) error {
		var point TimelinePoint
		if err := rows.Scan(&point.Hour, &point.Count); err != nil {
			return err
		}
		points = append(points, point)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return points, nil
}

// funnel computes a genuinely sequential funnel: the users counted at step N are
// the users who did steps 1..N *in order*, within the analysis window.
//
// It used to run one independent uniqExact per step and divide each by step 1,
// which is not a funnel — it is a per-event unique-user bar chart with a
// division. Nothing required a step-N user to have done step N-1, and nothing
// required the events to happen in order, so a user who only ever did the last
// step still counted as converted and a later step could report more users than
// an earlier one (conversion above 100%). Every surface that says "step-by-step
// conversion" — the Product page, run_insight/run_funnel, and the agents that
// pick a weakest link from them — was reading that number as passage.
//
// DuckDB has no windowFunnel; the ordering runs as a correlated earliest-match
// chain (the shape proven in storage-evaluation/harness/queries.py): per user,
// t1 is each step-1 event and tN is the earliest step-N event at or after
// t(N-1) and inside t1 + window. The user's depth is the longest prefix whose
// chain completed. Users at step N are then everyone whose depth is >= N,
// which is monotonically non-increasing by construction.
func (s *Store) funnel(ctx context.Context, projectID string, steps []string, filter EventFilter) ([]FunnelStep, error) {
	resolver, err := s.identityResolver(ctx, projectID)
	if err != nil {
		return nil, err
	}
	// A funnel measures people converting; crawler hits are not conversions.
	filter.HumansOnly = true
	cleanSteps := []string{}
	for _, step := range steps {
		step = strings.TrimSpace(step)
		if step != "" {
			cleanSteps = append(cleanSteps, step)
		}
	}
	if len(cleanSteps) == 0 {
		cleanSteps = []string{"user.pageview", "user.signup", "user.conversion"}
	}

	// The step list scopes the scan; the per-step EventName filter must not, or
	// every step but one would be filtered away before windowFunnel sees it.
	scanFilter := filter
	scanFilter.EventName = ""
	canonicalID, canonicalArgs := resolver.canonicalExpr("distinct_id")
	where, whereArgs := filteredWhereWithDistinctIDs(projectID, scanFilter, true, resolver.relatedDistinctIDs(scanFilter.DistinctID))

	query, args := buildFunnelQuery(cleanSteps, funnelWindowSeconds(filter), where, whereArgs, canonicalID, canonicalArgs)

	// depthCounts[d] = users whose longest in-order prefix was exactly d steps.
	depthCounts := make([]uint64, len(cleanSteps)+1)
	err = s.duckQuery(ctx, query, args, func(rows *sql.Rows) error {
		var level int
		var people uint64
		if err := rows.Scan(&level, &people); err != nil {
			return err
		}
		if level < len(depthCounts) {
			depthCounts[level] += people
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	return funnelStepsFromDepths(cleanSteps, depthCounts), nil
}

// buildFunnelQuery assembles the ordered-funnel query and its arguments.
// DuckDB binds ? by position, so the args are appended in the order the
// placeholders appear in the SQL *text*: the WHERE clause inside the ev CTE,
// then the event_name IN list, then the anchor step, then one bind per
// subsequent step's correlated subquery. Getting that order wrong does not
// error, it silently funnels the wrong events, which is why this is a
// separate function with a test rather than inline.
//
// The chain is greedy-earliest: tN is the earliest step-N event at or after
// t(N-1) within t1 + windowSeconds of the anchor. That matches windowFunnel's
// reachability semantics — the earliest achievable prefix is the longest one.
func buildFunnelQuery(cleanSteps []string, windowSeconds int64, where string, whereArgs []any, canonicalID string, canonicalArgs []any) (string, []any) {
	args := make([]any, 0, len(whereArgs)+2*len(cleanSteps)+len(canonicalArgs))
	args = append(args, whereArgs...)
	inList := strings.TrimSuffix(strings.Repeat("?, ", len(cleanSteps)), ", ")
	for _, eventName := range cleanSteps {
		args = append(args, eventName)
	}
	args = append(args, cleanSteps[0])
	for _, eventName := range cleanSteps[1:] {
		args = append(args, eventName)
	}
	args = append(args, canonicalArgs...)

	var b strings.Builder
	b.WriteString(`WITH ev AS (
	SELECT ` + canonicalID + ` AS cid, event_name, "timestamp" FROM resolved_events
	WHERE ` + where + ` AND event_name IN (` + inList + `)),
anchors AS (
	SELECT cid, "timestamp" AS t1 FROM ev WHERE event_name = ?)`)
	// Each step is its own CTE so the correlated subquery can reference the
	// previous step's column — DuckDB does not let a SELECT expression read a
	// sibling alias (a.t2 inside t3's subquery is a binder error).
	prev := "anchors"
	for i := 2; i <= len(cleanSteps); i++ {
		fmt.Fprintf(&b, `,
step%d AS (
	SELECT p.*,
		(SELECT min("timestamp") FROM ev e%d
			WHERE e%d.cid = p.cid AND e%d.event_name = ?
			  AND e%d."timestamp" >= p.t%d
			  AND e%d."timestamp" <= p.t1 + INTERVAL '%d seconds') AS t%d
	FROM %s p)`,
			i, i, i, i, i, i-1, i, windowSeconds, i, prev)
		prev = fmt.Sprintf("step%d", i)
	}
	if len(cleanSteps) == 1 {
		// A bare CASE with no WHEN is invalid; one step means every anchor is
		// depth 1 by definition.
		b.WriteString(`,
lvl AS (SELECT cid, 1 AS level FROM anchors)
SELECT level, count(*) AS people FROM lvl GROUP BY level`)
		return b.String(), args
	}
	b.WriteString(`,
lvl AS (
	SELECT cid, max(CASE`)
	for i := 2; i <= len(cleanSteps); i++ {
		fmt.Fprintf(&b, ` WHEN t%d IS NULL THEN %d`, i, i-1)
	}
	fmt.Fprintf(&b, ` ELSE %d END) AS level
	FROM %s GROUP BY cid)
SELECT level, count(*) AS people FROM lvl WHERE level > 0 GROUP BY level`, len(cleanSteps), prev)
	return b.String(), args
}

// funnelStepsFromDepths turns windowFunnel's per-user depth histogram into the
// step rows the API returns. depthCounts[d] is the number of users whose longest
// in-order prefix was exactly d steps, so the users who reached step N are
// everyone at depth >= N. Summing the tail is what makes the series monotonically
// non-increasing and conversion incapable of exceeding 100%.
func funnelStepsFromDepths(cleanSteps []string, depthCounts []uint64) []FunnelStep {
	out := make([]FunnelStep, 0, len(cleanSteps))
	var firstCount uint64
	for i, eventName := range cleanSteps {
		var count uint64
		for depth := i + 1; depth < len(depthCounts); depth++ {
			count += depthCounts[depth]
		}
		if i == 0 {
			firstCount = count
		}
		conversion := 0.0
		if firstCount > 0 {
			conversion = float64(count) / float64(firstCount)
		}
		out = append(out, FunnelStep{Step: i + 1, EventName: eventName, Users: count, Conversion: conversion})
	}
	return out
}

// firstStep is the first non-blank entry of a step list, or "".
func firstStep(steps []string) string {
	for _, step := range steps {
		if trimmed := strings.TrimSpace(step); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

// funnelWindowSeconds is how long a person has to complete the whole sequence.
// It is the analysis window itself: a funnel over "the last 30 days" asks who
// converted during those 30 days, so a stricter window would silently drop real
// conversions. Mirrors the 24h default filteredWhereWithDistinctIDs applies when
// the caller gave no range.
func funnelWindowSeconds(filter EventFilter) int64 {
	from, to := filter.From, filter.To
	if from.IsZero() || to.IsZero() || !to.After(from) {
		return int64((24 * time.Hour).Seconds())
	}
	return int64(to.Sub(from).Seconds())
}

func (s *Store) retention(ctx context.Context, projectID string, firstEvent string, filter EventFilter) ([]RetentionPoint, error) {
	resolver, err := s.identityResolver(ctx, projectID)
	if err != nil {
		return nil, err
	}
	// Retention is a people metric: a returning crawler is not a retained user.
	filter.HumansOnly = true
	// A weekly curve needs a window that can hold at least one full week. The
	// default analytics range is 24 hours, in which "weekly retention" is not a
	// narrower question but an unanswerable one — every period boundary is in the
	// future. Widen the cohort scan so the curve has room, and only when the
	// caller's range is too short to contain a single period; a range of two
	// weeks or more is a deliberate choice and is left exactly as given.
	if filter.To.IsZero() {
		filter.To = time.Now().UTC()
	}
	if filter.From.IsZero() || filter.To.Sub(filter.From) < 2*retentionPeriod {
		filter.From = filter.To.Add(-time.Duration(retentionWeeks+1) * retentionPeriod)
	}
	where, args := filteredWhereWithDistinctIDs(projectID, filter, true, resolver.relatedDistinctIDs(filter.DistinctID))
	firstWhere := where + " AND event_name = ?"
	firstArgs := append(append([]any{}, args...), firstEvent)
	// cohortStart is when the *earliest* member of this cohort was acquired. It
	// decides which periods are old enough to have a rate, and it must come from
	// the data, not from the window: a nine-week window over a project with two
	// days of events still has a two-day-old cohort, and every weekly period is
	// in the future for all of them. min() over an empty set is NULL in DuckDB,
	// so cohortStart scans into a nullable time.
	var base uint64
	var cohortStart sql.NullTime
	if err := s.duckQueryRow(ctx, `SELECT count(DISTINCT canonical_distinct_id), min("timestamp") FROM resolved_events WHERE `+firstWhere, firstArgs, &base, &cohortStart); err != nil {
		return nil, err
	}
	// Bracketed weekly cohort retention: week W counts cohort users active during
	// the half-open window [first_ts + W*7d, first_ts + (W+1)*7d). Week 0 is the
	// acquisition week (the base, 100%). A *periodic* curve — not the old rolling
	// "returned ever after X" — is what lets the PMF verdict read a plateau: a
	// curve that flattens to a stable floor is fit; one decaying toward zero is not.
	// Week 0 is the acquisition week — 100% by definition, not by measurement.
	points := []RetentionPoint{{Period: "Week 0", Users: base, Rate: 1, Mature: true}}
	for week := 1; week <= retentionWeeks; week++ {
		lowerHours := week * retentionPeriodHours
		upperHours := (week + 1) * retentionPeriodHours
		query := `
WITH firsts AS (
	SELECT canonical_distinct_id, min("timestamp") AS first_ts
	FROM resolved_events
	WHERE ` + firstWhere + `
	GROUP BY canonical_distinct_id
)
SELECT count(DISTINCT e.canonical_distinct_id)
FROM resolved_events e
INNER JOIN firsts f ON e.canonical_distinct_id = f.canonical_distinct_id
WHERE e.project_id = ?
	AND e."timestamp" >= f.first_ts + INTERVAL '` + fmt.Sprint(lowerHours) + ` hours'
	AND e."timestamp" <  f.first_ts + INTERVAL '` + fmt.Sprint(upperHours) + ` hours'`
		queryArgs := append(append([]any{}, firstArgs...), projectID)
		var retained uint64
		if err := s.duckQueryRow(ctx, query, queryArgs, &retained); err != nil {
			return nil, err
		}
		rate := 0.0
		if base > 0 {
			rate = float64(retained) / float64(base)
		}
		points = append(points, RetentionPoint{
			Period: fmt.Sprintf("Week %d", week),
			Users:  retained,
			Rate:   rate,
			Mature: retentionPeriodMature(cohortStart.Time, filter.To, week),
		})
	}
	return points, nil
}

// retentionWeeks / retentionPeriodHours define the cohort retention curve. Eight
// weekly periods is enough to see whether the curve plateaus (the PMF tell)
// rather than decaying to zero.
const (
	retentionWeeks       = 8
	retentionPeriodHours = 24 * 7
	retentionPeriod      = retentionPeriodHours * time.Hour
)

// retentionPeriodMature reports whether week N has finished elapsing for anyone
// in the cohort, given when its earliest member was acquired.
//
// The curve blends every cohort in the range into one series, so it uses the
// same standard the per-cohort triangle does (web/modules/cohorts/page.tsx):
// measure from the cohort's start. "Mature" therefore means the *earliest*
// members have had a full week N, not that every member has — a blended curve
// cannot promise the stronger thing without splitting into rows, which is what
// the Cohorts page is for. What it does guarantee is the thing that was broken:
// a period that nobody could possibly have reached is never reported as 0%.
func retentionPeriodMature(cohortStart, to time.Time, week int) bool {
	if cohortStart.IsZero() || to.IsZero() {
		return false
	}
	return !cohortStart.Add(time.Duration(week+1) * retentionPeriod).After(to)
}

// cohortWindowWeeks is how far back Cohorts reaches to assemble cohort rows. It
// spans the full retention curve (retentionWeeks) plus a few extra weeks so the
// most recent cohorts are visible alongside older, fully-matured ones.
const cohortWindowWeeks = retentionWeeks + 4

// maxCohortRows caps the triangle height so a wide custom range can't return an
// unbounded number of weekly rows; rows are emitted newest-first, so the cap
// keeps the most recent cohorts.
const maxCohortRows = 16

// audienceSegment is one named slice of the audience. Predicate is a DuckDB
// boolean over the per-person aggregate columns the cohort rollup exposes under
// the `f.` alias (has_traits, is_paid, plan); "" means the whole population. New
// audiences are added here as config rows — no new query branch — which is the
// extension seam the product asked for ("add a group like paid/premium user").
//
// Detection is event-derived today: `is_paid` flags anyone who ever fired a
// `revenue` event with a positive `amount`, and `plan` is the latest `plan`
// property off that event. The planned future input is an external attributes
// source — LEFT JOIN a `person_attributes` table keyed by canonical id and
// reference its columns the same way (`f.plan`, `f.is_paid`) so a paid/premium
// flag synced from a billing DB drops in without touching the query shape.
type audienceSegment struct {
	Key       string
	Label     string
	Predicate string
}

// Audience rule kinds for a custom ProjectAudience. These are the structured
// inputs compilePredicate understands — deliberately not raw SQL.
//
// Static-trait kinds (v1) read all-time event aggregates:
//   - "paid": ever fired a positive `revenue` event.
//   - "plan": latest `plan` value ∈ a configured set.
//
// Subscription-state kinds (v2) read the per-person subscription projection
// (f.sub_status / f.sub_plan), which is point-in-time aware via the project's
// subscription mapping (see SubscriptionMapping + DESIGN-SUBSCRIPTION-AUDIENCES.md):
//   - "active_subscriber": currently active or trialing.
//   - "trialing": currently on a trial.
//   - "churned": was a subscriber, now cancelled/expired.
//   - "plan_active": on a configured plan AND currently active/trialing.
const (
	audienceKindPaid       = "paid"
	audienceKindPlan       = "plan"
	audienceKindActive     = "active_subscriber"
	audienceKindTrialing   = "trialing"
	audienceKindChurned    = "churned"
	audienceKindPlanActive = "plan_active"
)

// subStatusActive is the SQL set treated as "a current customer" — active plus
// trialing (the doc's default: active ∪ trialing). "paid" vs "trialing" remain
// separable through the dedicated trialing kind.
const subStatusActive = "('active', 'trialing')"

// premiumPlans is the recognized set of premium-tier `plan` values. It is
// case-folded (the column is lower()'d at read time) and lives here so widening
// "what counts as premium" is a one-line edit, not a query change.
var premiumPlans = []string{"premium", "pro", "vip", "enterprise"}

var audienceSegments = []audienceSegment{
	{Key: "all", Label: "Everyone", Predicate: ""},
	{Key: "user", Label: "Users", Predicate: "f.has_traits = 1"},
	{Key: "guest", Label: "Guests", Predicate: "f.has_traits = 0"},
	{Key: "paid", Label: "Paid", Predicate: "f.is_paid = 1"},
	{Key: "premium", Label: "Premium", Predicate: "f.plan IN (" + quotedList(premiumPlans) + ")"},
}

// audiencePredicateFrom resolves an audience key to its canonical key + filter
// predicate against a resolved segment list (built-ins plus project customs). An
// unknown/empty key falls back to the whole population ("all").
func audiencePredicateFrom(segments []audienceSegment, segment string) (key, predicate string) {
	for _, a := range segments {
		if a.Key == segment {
			return a.Key, a.Predicate
		}
	}
	return "all", ""
}

// buildAudienceSegments merges the built-in audiences with a project's custom
// audiences. Each custom predicate is *compiled* from its structured rule
// (ProjectAudience.compilePredicate), never accepted as raw SQL — this is the
// injection-safe extension seam. A custom whose key collides with a built-in (or
// that compiles to nothing) is skipped, so the built-in catalog stays
// authoritative and a malformed row can never widen the query.
func buildAudienceSegments(custom []ProjectAudience, mapping SubscriptionMapping) []audienceSegment {
	out := append([]audienceSegment{}, audienceSegments...)
	// Subscription-state built-ins appear only once a project has configured a
	// status-capable mapping (start event + period-end property), so a product
	// with no subscription concept never sees empty status toggles. Trialing is
	// gated further on a trial property being mapped.
	if mapping.Configured && mapping.statusCapable() {
		out = append(out, audienceSegment{Key: audienceKindActive, Label: "Active subs", Predicate: "f.sub_status IN " + subStatusActive})
		if mapping.trialCapable() {
			out = append(out, audienceSegment{Key: audienceKindTrialing, Label: "Trialing", Predicate: "f.sub_status = 'trialing'"})
		}
		out = append(out, audienceSegment{Key: audienceKindChurned, Label: "Churned", Predicate: "f.sub_status = 'churned'"})
	}
	seen := make(map[string]bool, len(out))
	for _, a := range out {
		seen[a.Key] = true
	}
	for _, c := range custom {
		if seen[c.Key] {
			continue
		}
		pred := c.compilePredicate()
		if pred == "" {
			continue
		}
		out = append(out, audienceSegment{Key: c.Key, Label: c.Label, Predicate: pred})
		seen[c.Key] = true
	}
	return out
}

// SubscriptionMapping tells the cohort engine how to read a project's
// subscription lifecycle off its events: which event names open/renew/cancel a
// subscription and which properties carry the plan, amount, period-end and trial
// flag. It is per-project config (not code), the "signal differs per product"
// seam from DESIGN-SUBSCRIPTION-AUDIENCES.md. Configured is false when the
// project has never saved one (the returned value is then defaults).
type SubscriptionMapping struct {
	ProjectID     string `json:"project_id"`
	StartEvent    string `json:"start_event"`
	RenewEvent    string `json:"renew_event"`
	CancelEvent   string `json:"cancel_event"`
	PlanProp      string `json:"plan_prop"`
	AmountProp    string `json:"amount_prop"`
	PeriodEndProp string `json:"period_end_prop"`
	TrialProp     string `json:"trial_prop"`
	GraceDays     int    `json:"grace_days"`
	Configured    bool   `json:"configured"`
}

// defaultSubscriptionMapping is Stripe-shaped: a project emitting these standard
// event/property names gets subscription status with no configuration. It is
// returned (Configured=false) when no row exists.
func defaultSubscriptionMapping(projectID string) SubscriptionMapping {
	return SubscriptionMapping{
		ProjectID:     projectID,
		StartEvent:    "subscription_started",
		RenewEvent:    "subscription_renewed",
		CancelEvent:   "subscription_cancelled",
		PlanProp:      "plan",
		AmountProp:    "amount",
		PeriodEndProp: "current_period_end",
		TrialProp:     "is_trial",
		GraceDays:     1,
	}
}

// paidEventExpr builds the "this row is a payment" predicate for person
// flattening. It recognises the SDK convention (`revenue` carrying `amount`) and,
// additionally, the project's own subscription events carrying the amount
// property the mapping configures.
//
// Before the second half existed, paid detection recognised exactly one shape, so
// a product whose money rides on its own subscription events had an empty Paid
// audience while having paying subscribers — and the "Amount property" the
// mapping UI collects (audience-manager.tsx) was stored, validated and
// round-tripped without ever reaching a query.
//
// Strictly additive: it can only recognise a payer the SDK-shaped half missed,
// never un-pay one. An amount property the product does not actually emit
// extracts 0 and matches nothing, which is the honest answer — a booking event
// carrying no amount is a tracking defect to report, not a number to invent.
//
// openExpr is the caller's already-validated "subscription opened" predicate;
// the amount property is interpolated as a quoted JSON path like every other
// mapped token, so none of this is a raw-SQL surface.
func paidEventExpr(mapping SubscriptionMapping, openExpr string) string {
	// Historical, pre-DuckDB: JSONExtractFloat(p,'k') > 0 -> a missing/non-numeric
	// key extracts 0 and never matches; try_cast + coalesce reproduces that exactly.
	sdkShaped := "(event_name = 'revenue' AND coalesce(try_cast(json_extract_string(properties, '$.amount') AS DOUBLE), 0) > 0)"
	amountProp := strings.TrimSpace(mapping.AmountProp)
	if amountProp == "" || strings.TrimSpace(openExpr) == "" {
		return sdkShaped
	}
	// The property name lands inside a single-quoted JSON path literal —
	// escape it like every other interpolated token.
	escaped := strings.ReplaceAll(strings.ReplaceAll(amountProp, `\`, `\\`), `'`, `''`)
	return "(" + sdkShaped + " OR (" + openExpr +
		" AND coalesce(try_cast(json_extract_string(properties, '$.\"" + escaped + "\"') AS DOUBLE), 0) > 0))"
}

// statusCapable reports whether the mapping can assert active/expired status —
// which needs both a start event and a period-end property. Without a period end
// the engine refuses to call anyone "active" (it would silently regress to the
// v1 ever-paid flaw), so status stays "none". See decision #2 in the design doc.
func (m SubscriptionMapping) statusCapable() bool {
	return strings.TrimSpace(m.StartEvent) != "" && strings.TrimSpace(m.PeriodEndProp) != ""
}

// trialCapable reports whether a trial property is mapped (so trialing is
// distinguishable from active).
func (m SubscriptionMapping) trialCapable() bool {
	return m.statusCapable() && strings.TrimSpace(m.TrialProp) != ""
}

// subscriptionTokenRe validates an event/property name destined for the cohort
// SQL. Tokens are also escaped before interpolation; this is the first gate so a
// nonsense name never reaches the query at all.
var subscriptionTokenRe = regexp.MustCompile(`^[A-Za-z0-9_.$:-]*$`)

func validSubscriptionToken(s string) bool {
	return len(s) <= 64 && subscriptionTokenRe.MatchString(s)
}

// GetSubscriptionMapping returns the project's saved mapping, or Stripe-shaped
// defaults (Configured=false) when none exists.
func (s *Store) GetSubscriptionMapping(ctx context.Context, projectID string) (SubscriptionMapping, error) {
	m := defaultSubscriptionMapping(projectID)
	var got SubscriptionMapping
	err := s.pg.QueryRow(ctx, `
SELECT project_id::text, start_event, renew_event, cancel_event, plan_prop, amount_prop, period_end_prop, trial_prop, grace_days
FROM subscription_mappings WHERE project_id = $1`, projectID).
		Scan(&got.ProjectID, &got.StartEvent, &got.RenewEvent, &got.CancelEvent, &got.PlanProp, &got.AmountProp, &got.PeriodEndProp, &got.TrialProp, &got.GraceDays)
	if errors.Is(err, pgx.ErrNoRows) {
		return m, nil
	}
	if err != nil {
		return m, err
	}
	got.Configured = true
	return got, nil
}

// UpsertSubscriptionMapping validates and saves a project's subscription mapping.
func (s *Store) UpsertSubscriptionMapping(ctx context.Context, projectID string, in SubscriptionMapping) (SubscriptionMapping, error) {
	in.StartEvent = strings.TrimSpace(in.StartEvent)
	in.RenewEvent = strings.TrimSpace(in.RenewEvent)
	in.CancelEvent = strings.TrimSpace(in.CancelEvent)
	in.PlanProp = strings.TrimSpace(in.PlanProp)
	in.AmountProp = strings.TrimSpace(in.AmountProp)
	in.PeriodEndProp = strings.TrimSpace(in.PeriodEndProp)
	in.TrialProp = strings.TrimSpace(in.TrialProp)
	for _, tok := range []string{in.StartEvent, in.RenewEvent, in.CancelEvent, in.PlanProp, in.AmountProp, in.PeriodEndProp, in.TrialProp} {
		if !validSubscriptionToken(tok) {
			return SubscriptionMapping{}, fmt.Errorf("invalid event/property name %q (letters, numbers, . _ $ : - only)", tok)
		}
	}
	if in.StartEvent == "" {
		return SubscriptionMapping{}, fmt.Errorf("a subscription start event is required")
	}
	if in.GraceDays < 0 || in.GraceDays > 90 {
		return SubscriptionMapping{}, fmt.Errorf("grace days must be between 0 and 90")
	}
	var got SubscriptionMapping
	err := s.pg.QueryRow(ctx, `
INSERT INTO subscription_mappings (project_id, start_event, renew_event, cancel_event, plan_prop, amount_prop, period_end_prop, trial_prop, grace_days, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, now())
ON CONFLICT (project_id) DO UPDATE SET
	start_event = EXCLUDED.start_event, renew_event = EXCLUDED.renew_event, cancel_event = EXCLUDED.cancel_event,
	plan_prop = EXCLUDED.plan_prop, amount_prop = EXCLUDED.amount_prop, period_end_prop = EXCLUDED.period_end_prop,
	trial_prop = EXCLUDED.trial_prop, grace_days = EXCLUDED.grace_days, updated_at = now()
RETURNING project_id::text, start_event, renew_event, cancel_event, plan_prop, amount_prop, period_end_prop, trial_prop, grace_days`,
		projectID, in.StartEvent, in.RenewEvent, in.CancelEvent, in.PlanProp, in.AmountProp, in.PeriodEndProp, in.TrialProp, in.GraceDays).
		Scan(&got.ProjectID, &got.StartEvent, &got.RenewEvent, &got.CancelEvent, &got.PlanProp, &got.AmountProp, &got.PeriodEndProp, &got.TrialProp, &got.GraceDays)
	if err != nil {
		return SubscriptionMapping{}, err
	}
	got.Configured = true
	return got, nil
}

// compilePredicate turns a structured audience rule into a DuckDB boolean
// over the per-person aggregate columns the cohort rollup exposes (f.is_paid,
// f.plan). It is the only place a custom audience becomes SQL: plan values are
// lower-cased (the column is lower()'d at read time) and single-quote escaped,
// so no user input reaches the query unescaped. An unknown kind or empty value
// set yields "" and the audience is dropped by buildAudienceSegments.
func (a ProjectAudience) compilePredicate() string {
	switch a.Kind {
	case audienceKindPaid:
		return "f.is_paid = 1"
	case audienceKindActive:
		return "f.sub_status IN " + subStatusActive
	case audienceKindTrialing:
		return "f.sub_status = 'trialing'"
	case audienceKindChurned:
		return "f.sub_status = 'churned'"
	case audienceKindPlan, audienceKindPlanActive:
		list := quotedPlanList(a.Plans)
		if list == "" {
			return ""
		}
		if a.Kind == audienceKindPlanActive {
			// On a configured plan AND a current customer — the point-in-time
			// version of "plan", which "ever paid on plan X" cannot express.
			return "f.sub_plan IN (" + list + ") AND f.sub_status IN " + subStatusActive
		}
		return "f.plan IN (" + list + ")"
	}
	return ""
}

// quotedPlanList lower-cases, dedups, escapes and single-quotes a set of plan
// values into a SQL value list, or "" if nothing usable remains. The plan column
// is lower()'d at read time; escaping keeps user input off the injection surface.
func quotedPlanList(plans []string) string {
	vals := make([]string, 0, len(plans))
	seen := map[string]bool{}
	for _, p := range plans {
		p = strings.ToLower(strings.TrimSpace(p))
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		vals = append(vals, "'"+strings.ReplaceAll(p, "'", "''")+"'")
	}
	return strings.Join(vals, ", ")
}

// quotedList renders a string slice as a SQL value list (single-quoted, comma
// separated). The values are an internal allow-list (premiumPlans), never user
// input, so this is not a SQL-injection surface.
func quotedList(values []string) string {
	out := make([]string, len(values))
	for i, v := range values {
		out[i] = "'" + v + "'"
	}
	return strings.Join(out, ", ")
}

// maxAudiencePlans caps how many plan values one custom audience may list — a
// generous bound that keeps the compiled IN (...) clause sane.
const maxAudiencePlans = 32

// isUniqueViolation reports whether err is a Postgres unique-constraint
// violation (SQLSTATE 23505), so callers can return a friendly conflict message.
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// slugifyAudienceKey derives a stable, URL/query-safe key from a human label
// (lower-case, alnum runs joined by single dashes). The key is what the segment
// toggle and the `?segment=` param carry.
func slugifyAudienceKey(label string) string {
	var b strings.Builder
	prevDash := false
	for _, r := range strings.ToLower(strings.TrimSpace(label)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			prevDash = false
		default:
			if b.Len() > 0 && !prevDash {
				b.WriteByte('-')
				prevDash = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}

// validateProjectAudience normalizes and checks a custom audience definition,
// returning the cleaned key/label/kind/plans or a user-facing error. It rejects
// labels that collide with a built-in audience so the built-in catalog stays
// authoritative, and requires a non-empty plan set for the "plan" kind.
func validateProjectAudience(label, kind string, plans []string) (ProjectAudience, error) {
	label = strings.TrimSpace(label)
	if label == "" {
		return ProjectAudience{}, fmt.Errorf("audience name is required")
	}
	if len([]rune(label)) > 60 {
		return ProjectAudience{}, fmt.Errorf("audience name is too long")
	}
	key := slugifyAudienceKey(label)
	if key == "" {
		return ProjectAudience{}, fmt.Errorf("audience name must contain letters or numbers")
	}
	for _, b := range audienceSegments {
		if b.Key == key {
			return ProjectAudience{}, fmt.Errorf("%q is a built-in audience — choose another name", label)
		}
	}
	clean := []string{}
	switch kind {
	case audienceKindPaid, audienceKindActive, audienceKindTrialing, audienceKindChurned:
		// These match by status/ever-paid — no plan values needed.
	case audienceKindPlan, audienceKindPlanActive:
		seen := map[string]bool{}
		for _, p := range plans {
			p = strings.TrimSpace(p)
			if p == "" || seen[strings.ToLower(p)] {
				continue
			}
			seen[strings.ToLower(p)] = true
			clean = append(clean, p)
		}
		if len(clean) == 0 {
			return ProjectAudience{}, fmt.Errorf("a plan audience needs at least one plan value")
		}
		if len(clean) > maxAudiencePlans {
			return ProjectAudience{}, fmt.Errorf("too many plan values (max %d)", maxAudiencePlans)
		}
	default:
		return ProjectAudience{}, fmt.Errorf("unknown audience kind %q", kind)
	}
	return ProjectAudience{Key: key, Label: label, Kind: kind, Plans: clean}, nil
}

// ListProjectAudiences returns a project's custom cohort audiences, oldest first
// (the order they appear after the built-ins in the toggle).
func (s *Store) ListProjectAudiences(ctx context.Context, projectID string) ([]ProjectAudience, error) {
	rows, err := s.pg.Query(ctx, `
SELECT id::text, project_id::text, key, label, kind, COALESCE(plans, '[]'::jsonb), created_at
FROM cohort_audiences
WHERE project_id = $1
ORDER BY created_at ASC`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ProjectAudience{}
	for rows.Next() {
		var a ProjectAudience
		var plans []byte
		if err := rows.Scan(&a.ID, &a.ProjectID, &a.Key, &a.Label, &a.Kind, &plans, &a.CreatedAt); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(plans, &a.Plans); err != nil || a.Plans == nil {
			a.Plans = []string{}
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// CreateProjectAudience validates and inserts a custom audience. A name whose
// slug already exists in the project surfaces as a friendly conflict error.
func (s *Store) CreateProjectAudience(ctx context.Context, projectID, label, kind string, plans []string) (ProjectAudience, error) {
	def, err := validateProjectAudience(label, kind, plans)
	if err != nil {
		return ProjectAudience{}, err
	}
	planJSON, _ := json.Marshal(def.Plans)
	var out ProjectAudience
	var got []byte
	err = s.pg.QueryRow(ctx, `
INSERT INTO cohort_audiences (project_id, key, label, kind, plans)
VALUES ($1, $2, $3, $4, $5::jsonb)
RETURNING id::text, project_id::text, key, label, kind, COALESCE(plans, '[]'::jsonb), created_at`,
		projectID, def.Key, def.Label, def.Kind, string(planJSON)).
		Scan(&out.ID, &out.ProjectID, &out.Key, &out.Label, &out.Kind, &got, &out.CreatedAt)
	if err != nil {
		if isUniqueViolation(err) {
			return ProjectAudience{}, fmt.Errorf("an audience named %q already exists", label)
		}
		return ProjectAudience{}, err
	}
	if json.Unmarshal(got, &out.Plans) != nil || out.Plans == nil {
		out.Plans = []string{}
	}
	return out, nil
}

// UpdateProjectAudience re-validates and replaces a custom audience in place
// (identified by id); the slug may change if the name changed.
func (s *Store) UpdateProjectAudience(ctx context.Context, projectID, id, label, kind string, plans []string) (ProjectAudience, error) {
	def, err := validateProjectAudience(label, kind, plans)
	if err != nil {
		return ProjectAudience{}, err
	}
	planJSON, _ := json.Marshal(def.Plans)
	var out ProjectAudience
	var got []byte
	err = s.pg.QueryRow(ctx, `
UPDATE cohort_audiences SET key = $3, label = $4, kind = $5, plans = $6::jsonb
WHERE project_id = $1 AND id = $2
RETURNING id::text, project_id::text, key, label, kind, COALESCE(plans, '[]'::jsonb), created_at`,
		projectID, id, def.Key, def.Label, def.Kind, string(planJSON)).
		Scan(&out.ID, &out.ProjectID, &out.Key, &out.Label, &out.Kind, &got, &out.CreatedAt)
	if err != nil {
		if isUniqueViolation(err) {
			return ProjectAudience{}, fmt.Errorf("an audience named %q already exists", label)
		}
		return ProjectAudience{}, err
	}
	if json.Unmarshal(got, &out.Plans) != nil || out.Plans == nil {
		out.Plans = []string{}
	}
	return out, nil
}

// DeleteProjectAudience removes a custom audience by id (scoped to the project).
func (s *Store) DeleteProjectAudience(ctx context.Context, projectID, id string) error {
	_, err := s.pg.Exec(ctx, `DELETE FROM cohort_audiences WHERE project_id = $1 AND id = $2`, projectID, id)
	return err
}

// Cohorts builds a weekly acquisition-cohort retention triangle, optionally
// scoped to one audience segment. Each row is the set of people whose first
// event landed in a given ISO week; each cell is the share of that cohort still
// active in week N after acquisition. `segment` selects the population through
// audienceSegments — "user"/"guest" by identity trait, "paid"/"premium" by
// event-derived billing attributes — projecting the People split onto retention.
//
// explicitRange is true when the caller chose a concrete from/to window (vs the
// preset hours range); it suppresses the default look-back widening so an
// explicit narrow range is honored exactly instead of being silently expanded.
func (s *Store) Cohorts(ctx context.Context, projectID string, filter EventFilter, segment string, explicitRange bool) (CohortAnalysis, error) {
	// Built-in audiences plus this project's custom ones; the toggle catalog and
	// the active predicate both resolve from this one merged list so they cannot
	// drift.
	custom, err := s.ListProjectAudiences(ctx, projectID)
	if err != nil {
		return CohortAnalysis{}, err
	}
	mapping, err := s.GetSubscriptionMapping(ctx, projectID)
	if err != nil {
		return CohortAnalysis{}, err
	}
	segments := buildAudienceSegments(custom, mapping)
	key, predicate := audiencePredicateFrom(segments, segment)
	result := CohortAnalysis{Segment: key, Periods: retentionWeeks, Audiences: cohortAudienceOptionsFrom(segments), Rows: []CohortRow{}, Generated: time.Now().UTC()}
	// Cohort retention is a people metric — a returning crawler is not a user.
	filter.HumansOnly = true
	// Weekly cohorts need a window wide enough to see a full retention curve; the
	// default analytics range (hours) is far too short. Widen From so the earliest
	// cohort still has room to mature — but only for the preset range; an explicit
	// custom from/to is the user's deliberate choice and is left untouched.
	if !explicitRange {
		if filter.To.IsZero() {
			filter.To = time.Now().UTC()
		}
		minFrom := filter.To.Add(-time.Duration(cohortWindowWeeks*retentionPeriodHours) * time.Hour)
		if filter.From.IsZero() || filter.From.After(minFrom) {
			filter.From = minFrom
		}
	}

	resolver, err := s.identityResolver(ctx, projectID)
	if err != nil {
		return result, err
	}
	where, args := filteredWhereWithDistinctIDs(projectID, filter, true, resolver.relatedDistinctIDs(filter.DistinctID))

	segmentClause := ""
	if predicate != "" {
		segmentClause = " AND " + predicate
	}

	// base is scanned twice (firsts derives the cohort anchor + per-person
	// attributes from it, the outer query measures activity against it) — two
	// event scans, the same trade-off Persons accepts. The paid / plan
	// attributes come off the `revenue` event (amount/plan in properties) —
	// the documented revenue convention — and aggregate to one flag per person.
	// resolved_events supplies the stitched canonical id (the aliases_dict job).
	periodExpr := "(date_diff('hour', f.first_ts, b.\"timestamp\") // " + fmt.Sprint(retentionPeriodHours) + ")::INTEGER"

	// Subscription projection literals, compiled from the per-project mapping. The
	// tokens are validated (validSubscriptionToken) and escaped (sqlQuote) so
	// none of this is a raw-SQL surface. An event a product never emits simply
	// matches nothing, leaving sub_status = 'none'. See DESIGN-SUBSCRIPTION-AUDIENCES.md.
	startLit := sqlQuote(mapping.StartEvent)
	renewLit := sqlQuote(mapping.RenewEvent)
	cancelLit := sqlQuote(mapping.CancelEvent)
	openExpr := "(event_name = " + startLit + " OR event_name = " + renewLit + ")"
	// JSON paths use the quoted-key form ($."prop") so a mapped property whose
	// name contains a dot still reads one top-level key — DuckDB json_extract_string
	// treats the quoted segment as a literal key. The token charset
	// (validSubscriptionToken) excludes quotes, so the path cannot break out.
	trialExpr := "false"
	if strings.TrimSpace(mapping.TrialProp) != "" {
		trialExpr = "coalesce(try_cast(json_extract(properties, '$.\"" + mapping.TrialProp + "\"') AS BOOLEAN), false)"
	}
	paidExpr := paidEventExpr(mapping, openExpr)
	grace := fmt.Sprint(mapping.GraceDays)

	query := `
WITH base AS (
	SELECT
		canonical_distinct_id AS cid,
		"timestamp",
		(coalesce(nullif(json_extract_string(properties, '$.email'), ''), json_extract_string(properties, '$."$set".email'), '') <> ''
		 OR coalesce(nullif(json_extract_string(properties, '$.name'), ''), json_extract_string(properties, '$."$set".name'), '') <> '') AS identified,
		` + paidExpr + ` AS paid_event,
		if(event_name = 'revenue', lower(json_extract_string(properties, '$.plan')), '') AS plan_value,
		` + openExpr + ` AS is_sub_open,
		(event_name = ` + cancelLit + `) AS is_sub_cancel,
		if(` + openExpr + `, lower(json_extract_string(properties, '$."` + mapping.PlanProp + `"')), '') AS sub_plan_value,
		if(` + openExpr + `, coalesce(
			try_cast(json_extract_string(properties, '$."` + mapping.PeriodEndProp + `"') AS TIMESTAMPTZ),
			to_timestamp(try_cast(json_extract_string(properties, '$."` + mapping.PeriodEndProp + `"') AS DOUBLE))), NULL) AS sub_period_end,
		if(` + openExpr + `, ` + trialExpr + `, false) AS sub_is_trial
	FROM resolved_events
	WHERE ` + where + `
),
firsts AS (
	SELECT
		cid,
		min("timestamp") AS first_ts,
		max(identified) AS has_traits,
		max(paid_event) AS is_paid,
		arg_max(plan_value, "timestamp") FILTER (WHERE plan_value <> '') AS plan,
		count(*) FILTER (WHERE is_sub_open) AS sub_open_count,
		max("timestamp") FILTER (WHERE is_sub_open) AS last_open_ts,
		max("timestamp") FILTER (WHERE is_sub_cancel) AS last_cancel_ts,
		arg_max(sub_period_end, "timestamp") FILTER (WHERE is_sub_open) AS sub_period_end,
		arg_max(sub_plan_value, "timestamp") FILTER (WHERE is_sub_open AND sub_plan_value <> '') AS sub_plan,
		arg_max(sub_is_trial, "timestamp") FILTER (WHERE is_sub_open) AS sub_is_trial
	FROM base
	GROUP BY cid
),
people AS (
	SELECT
		*,
		CASE
			WHEN sub_open_count = 0 THEN 'none'
			WHEN last_cancel_ts > last_open_ts THEN 'churned'
			WHEN sub_period_end IS NULL THEN 'none'
			WHEN sub_period_end + INTERVAL '` + grace + ` days' < now() THEN 'churned'
			WHEN sub_is_trial THEN 'trialing'
			ELSE 'active' END AS sub_status
	FROM firsts
)
SELECT
	date_trunc('week', timezone('UTC', f.first_ts)) AS cohort_week,
	` + periodExpr + ` AS period,
	count(DISTINCT b.cid) AS users
FROM base b
INNER JOIN people f ON b.cid = f.cid
WHERE b."timestamp" >= f.first_ts` + segmentClause + `
	AND ` + periodExpr + ` <= ?
GROUP BY cohort_week, period
ORDER BY cohort_week DESC, period ASC`

	queryArgs := append(append([]any{}, args...), retentionWeeks)
	// Bucket cells by cohort week, preserving the newest-first scan order so the
	// row cap keeps the most recent cohorts.
	byCohort := map[string]*CohortRow{}
	order := []string{}
	err = s.duckQuery(ctx, query, queryArgs, func(rows *sql.Rows) error {
		var cohortWeek time.Time
		var period int32
		var users uint64
		if err := rows.Scan(&cohortWeek, &period, &users); err != nil {
			return err
		}
		key := cohortWeek.Format("2006-01-02")
		row := byCohort[key]
		if row == nil {
			row = &CohortRow{Cohort: key, CohortStart: cohortWeek, Cells: []CohortCell{}}
			byCohort[key] = row
			order = append(order, key)
		}
		if period == 0 {
			row.Size = users
		}
		row.Cells = append(row.Cells, CohortCell{Period: int(period), Users: users})
		return nil
	})
	if err != nil {
		return result, err
	}

	if len(order) > maxCohortRows {
		order = order[:maxCohortRows]
	}
	for _, key := range order {
		row := byCohort[key]
		for i := range row.Cells {
			if row.Size > 0 {
				row.Cells[i].Rate = float64(row.Cells[i].Users) / float64(row.Size)
			}
		}
		result.Rows = append(result.Rows, *row)
	}
	return result, nil
}

func (s *Store) agentInsightRows(ctx context.Context, projectID string, filter EventFilter) ([]map[string]any, error) {
	filter.EventType = "agent"
	resolver, err := s.identityResolver(ctx, projectID)
	if err != nil {
		return nil, err
	}
	where, args := filteredWhereWithDistinctIDs(projectID, filter, true, resolver.relatedDistinctIDs(filter.DistinctID))
	out := []map[string]any{}
	err = s.duckQuery(ctx, `
SELECT
	coalesce(agent_id, 'unknown') AS agent_id,
	coalesce(model_name, 'unknown') AS model_name,
	count(*) AS events,
	coalesce(sum(tokens_input), 0) AS tokens_in,
	coalesce(sum(tokens_output), 0) AS tokens_out,
	coalesce(sum(cost_usd), 0) AS cost_usd,
	coalesce(avg(coalesce(latency_ms, 0)::DOUBLE), 0) AS avg_latency_ms,
	count(*) FILTER (WHERE is_error) AS errors
FROM events
WHERE `+where+`
GROUP BY agent_id, model_name
ORDER BY cost_usd DESC, events DESC
LIMIT 50`, args, func(rows *sql.Rows) error {
		var agentID, modelName string
		var events, tokensIn, tokensOut, errors uint64
		var cost, latency float64
		if err := rows.Scan(&agentID, &modelName, &events, &tokensIn, &tokensOut, &cost, &latency, &errors); err != nil {
			return err
		}
		out = append(out, map[string]any{
			"agent_id":       agentID,
			"model_name":     modelName,
			"events":         events,
			"tokens_in":      tokensIn,
			"tokens_out":     tokensOut,
			"cost_usd":       cost,
			"avg_latency_ms": latency,
			"errors":         errors,
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// overviewAcquisitionFilter is the population the ranked acquisition lists
// count: real user pageviews from humans, the same qualifying activity every
// people metric uses. A crawler that walks the site is not a landing page or a
// source, and counting it here while New people excludes it would put two
// different populations under one group heading.
func overviewAcquisitionFilter(r OverviewRange, platform string) EventFilter {
	return EventFilter{
		From:       r.From,
		To:         r.To.Add(-time.Nanosecond),
		EventType:  "user",
		HumansOnly: true,
		Platform:   platform,
	}
}

func (s *Store) sessionQuality(ctx context.Context, projectID string, filter EventFilter) (float64, float64, error) {
	resolver, err := s.identityResolver(ctx, projectID)
	if err != nil {
		return 0, 0, err
	}
	where, args := filteredWhereWithDistinctIDs(projectID, filter, true, resolver.relatedDistinctIDs(filter.DistinctID))
	duration, bounce, _, err := s.sessionQualityWhere(ctx, where, args)
	return duration, bounce, err
}

// sessionQualityWhere runs the session-quality aggregate over a caller-built
// window. The session count comes back beside the rates so a caller can tell
// "every session bounced" from "there were no sessions" — the second is
// no_data, never a 0% measurement.
func (s *Store) sessionQualityWhere(ctx context.Context, where string, args []any) (float64, float64, uint64, error) {
	var duration float64
	var bounceRate float64
	var sessions uint64
	err := s.duckQueryRow(ctx, `
WITH per_session AS (
	SELECT
		session_id,
		date_diff('second', min("timestamp"), max("timestamp")) AS duration_seconds,
		count(*) AS events
	FROM events
	WHERE `+where+` AND session_id <> ''
	GROUP BY session_id
)
SELECT coalesce(avg(duration_seconds), 0), coalesce(avg(if(events <= 1, 1, 0)), 0), count(*)
FROM per_session`, args, &duration, &bounceRate, &sessions)
	return duration, bounceRate, sessions, err
}

func (s *Store) propertyCounts(ctx context.Context, projectID string, filter EventFilter, property string, eventName string) ([]PathCount, error) {
	filter.EventName = eventName
	resolver, err := s.identityResolver(ctx, projectID)
	if err != nil {
		return nil, err
	}
	where, args := filteredWhereWithDistinctIDs(projectID, filter, true, resolver.relatedDistinctIDs(filter.DistinctID))
	out := []PathCount{}
	err = s.duckQuery(ctx, `
SELECT coalesce(json_extract_string(properties, '$.' || ?), '') AS value, count(*) AS count
FROM events
WHERE `+where+` AND value <> ''
GROUP BY value
ORDER BY count DESC
LIMIT 20`, append([]any{property}, args...), func(rows *sql.Rows) error {
		var item PathCount
		if err := rows.Scan(&item.Value, &item.Count); err != nil {
			return err
		}
		out = append(out, item)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func filteredWhereWithDefault(projectID string, filter EventFilter, defaultTimeWindow bool) (string, []any) {
	return filteredWhereWithDistinctIDs(projectID, filter, defaultTimeWindow, nil)
}

func filteredWhereWithDistinctIDs(projectID string, filter EventFilter, defaultTimeWindow bool, relatedDistinctIDs []string) (string, []any) {
	clauses := []string{"project_id = ?"}
	args := []any{projectID}
	from := filter.From
	if from.IsZero() && defaultTimeWindow {
		from = time.Now().UTC().Add(-24 * time.Hour)
	}
	to := filter.To
	if to.IsZero() && defaultTimeWindow {
		to = time.Now().UTC()
	}
	if !from.IsZero() {
		clauses = append(clauses, `"timestamp" >= ?`)
		args = append(args, from)
	}
	if !to.IsZero() {
		clauses = append(clauses, `"timestamp" <= ?`)
		args = append(args, to)
	}
	if filter.EventType != "" {
		clauses = append(clauses, "event_type = ?")
		args = append(args, filter.EventType)
	}
	if filter.EventName != "" {
		clauses = append(clauses, "event_name = ?")
		args = append(args, filter.EventName)
	}
	if filter.DistinctID != "" {
		if len(relatedDistinctIDs) <= 1 {
			clauses = append(clauses, "distinct_id = ?")
			args = append(args, filter.DistinctID)
		} else {
			clauses = append(clauses, "distinct_id IN "+placeholders(len(relatedDistinctIDs)))
			for _, id := range relatedDistinctIDs {
				args = append(args, id)
			}
		}
	}
	if filter.SessionID != "" {
		clauses = append(clauses, "session_id = ?")
		args = append(args, filter.SessionID)
	}
	if filter.AgentID != "" {
		clauses = append(clauses, "coalesce(agent_id, '') = ?")
		args = append(args, filter.AgentID)
	}
	if filter.ModelName != "" {
		clauses = append(clauses, "coalesce(model_name, '') = ?")
		args = append(args, filter.ModelName)
	}
	if filter.ErrorOnly {
		clauses = append(clauses, "is_error")
	}
	if filter.HumansOnly {
		clauses = append(clauses, "coalesce(visitor_class, 'human') = 'human'")
	}
	if clause, arg, ok := platformClause(filter.Platform); ok {
		clauses = append(clauses, clause)
		if arg != nil {
			args = append(args, arg)
		}
	}
	if filter.Search != "" {
		clauses = append(clauses, "(strpos(lower(event_name), lower(?)) > 0 OR strpos(lower(distinct_id), lower(?)) > 0 OR strpos(lower(session_id), lower(?)) > 0 OR strpos(lower(properties), lower(?)) > 0)")
		args = append(args, filter.Search, filter.Search, filter.Search, filter.Search)
	}
	return strings.Join(clauses, " AND "), args
}

func workspaceFilteredWhere(projectIDs []string, filter EventFilter, defaultTimeWindow bool) (string, []any) {
	clauses := []string{"project_id IN " + placeholders(len(projectIDs))}
	args := make([]any, 0, len(projectIDs)+8)
	for _, projectID := range projectIDs {
		args = append(args, projectID)
	}
	from := filter.From
	to := filter.To
	if from.IsZero() && defaultTimeWindow {
		from = time.Now().UTC().Add(-24 * time.Hour)
	}
	if to.IsZero() && defaultTimeWindow {
		to = time.Now().UTC()
	}
	if !from.IsZero() {
		clauses = append(clauses, `"timestamp" >= ?`)
		args = append(args, from)
	}
	if !to.IsZero() {
		clauses = append(clauses, `"timestamp" <= ?`)
		args = append(args, to)
	}
	if filter.EventType != "" {
		clauses = append(clauses, "event_type = ?")
		args = append(args, filter.EventType)
	}
	if filter.EventName != "" {
		clauses = append(clauses, "event_name = ?")
		args = append(args, filter.EventName)
	}
	if filter.DistinctID != "" {
		clauses = append(clauses, "distinct_id = ?")
		args = append(args, filter.DistinctID)
	}
	if filter.SessionID != "" {
		clauses = append(clauses, "session_id = ?")
		args = append(args, filter.SessionID)
	}
	if filter.AgentID != "" {
		clauses = append(clauses, "agent_id = ?")
		args = append(args, filter.AgentID)
	}
	if filter.ModelName != "" {
		clauses = append(clauses, "model_name = ?")
		args = append(args, filter.ModelName)
	}
	if filter.ErrorOnly {
		clauses = append(clauses, "is_error")
	}
	if filter.HumansOnly {
		clauses = append(clauses, "coalesce(visitor_class, 'human') = 'human'")
	}
	if clause, arg, ok := platformClause(filter.Platform); ok {
		clauses = append(clauses, clause)
		if arg != nil {
			args = append(args, arg)
		}
	}
	if filter.Search != "" {
		clauses = append(clauses, "(event_name ILIKE ? OR properties ILIKE ? OR distinct_id ILIKE ?)")
		search := "%" + filter.Search + "%"
		args = append(args, search, search, search)
	}
	return strings.Join(clauses, " AND "), args
}

// anySlice boxes a string slice for variadic arg lists.
func anySlice(values []string) []any {
	out := make([]any, len(values))
	for i, v := range values {
		out[i] = v
	}
	return out
}

// PlatformUnknown is the filter value that selects rows whose platform could not
// be determined — events captured before the column existed, or from a client
// that sends neither the property nor a recognisable user agent. It is a filter
// token only; the stored value for those rows is ”.
const PlatformUnknown = "unknown"

// platformClause turns a platform filter into SQL. 'unknown' matches the empty
// stored value, anything else matches exactly, and an empty filter adds nothing.
func platformClause(platform string) (string, any, bool) {
	platform = strings.ToLower(strings.TrimSpace(platform))
	switch platform {
	case "":
		return "", nil, false
	case PlatformUnknown:
		return "coalesce(platform, '') = ''", nil, true
	default:
		return "coalesce(platform, '') = ?", platform, true
	}
}

// placeholders renders a parenthesized IN-list of ? binds — "(?, ?, ?)".
func placeholders(count int) string {
	out := make([]string, count)
	for i := range out {
		out[i] = "?"
	}
	return "(" + strings.Join(out, ", ") + ")"
}

func emptySinceHours(filter EventFilter) int {
	from := filter.From
	to := filter.To
	if from.IsZero() {
		from = time.Now().UTC().Add(-24 * time.Hour)
	}
	if to.IsZero() {
		to = time.Now().UTC()
	}
	hours := int(to.Sub(from).Hours())
	if hours <= 0 {
		return 1
	}
	if hours > 24*90 {
		return 24 * 90
	}
	return hours
}

type eventScanner interface {
	Scan(dest ...any) error
}

func scanEvent(rows eventScanner) (Event, error) {
	var event Event
	var inserted time.Time
	var isError bool
	var isUnplanned bool
	var tokensIn uint32
	var tokensOut uint32
	var cost float64
	var latency uint32
	if err := rows.Scan(
		&event.ProjectID,
		&event.EventID,
		&event.DistinctID,
		&event.SessionID,
		&event.EventName,
		&event.EventType,
		&event.Properties,
		&event.AgentID,
		&event.ToolName,
		&event.ToolInput,
		&event.ToolOutput,
		&tokensIn,
		&tokensOut,
		&cost,
		&latency,
		&event.ModelName,
		&isError,
		&event.ErrorMessage,
		&event.Timestamp,
		&inserted,
		&isUnplanned,
		&event.Platform,
	); err != nil {
		return event, err
	}
	event.IsError = isError
	event.IsUnplanned = isUnplanned
	event.InsertedAt = &inserted
	if tokensIn > 0 {
		event.TokensInput = &tokensIn
	}
	if tokensOut > 0 {
		event.TokensOutput = &tokensOut
	}
	if cost > 0 {
		cost32 := float32(cost)
		event.CostUSD = &cost32
	}
	if latency > 0 {
		event.LatencyMS = &latency
	}
	return event, nil
}

func eventToMap(event Event) map[string]any {
	return map[string]any{
		"event_id":      event.EventID,
		"event_name":    event.EventName,
		"event_type":    event.EventType,
		"distinct_id":   event.DistinctID,
		"session_id":    event.SessionID,
		"agent_id":      event.AgentID,
		"tool_name":     event.ToolName,
		"model_name":    event.ModelName,
		"is_error":      event.IsError,
		"timestamp":     event.Timestamp,
		"tokens_input":  event.TokensInput,
		"tokens_output": event.TokensOutput,
		"cost_usd":      event.CostUSD,
	}
}

var (
	eventsSourcePattern = regexp.MustCompile(`(?i)\bfrom\s+events\b`)
	eventsJoinPattern   = regexp.MustCompile(`(?i)\bjoin\s+events\b`)
	// external_rows (data-connector landing table) may be read via FROM or
	// JOIN — its project scope is a plain filter with no identity-stitching
	// args, so joining it against events is safe to rewrite.
	externalSourcePattern = regexp.MustCompile(`(?i)\b(from|join)\s+external_rows\b`)
	// After rewriting, no bare reference to either tenant table may remain:
	// comma joins (`FROM external_rows x, events e`), quoted identifiers
	// (`FROM "events"`), or any other unrecognized form would read the raw
	// multi-tenant table on a role with database-wide SELECT. String literals
	// are stripped before this check so `WHERE name = 'events'` stays legal.
	residualSourcePattern = regexp.MustCompile(`(?i)\b(events|external_rows)\b`)
	sqlStringLiteral      = regexp.MustCompile(`'(?:[^'\\]|\\.|'')*'`)
	// A caller's own CTE list. Its leading `WITH [RECURSIVE]` has to give way to
	// ours — `WITH scoped_events AS (…) WITH money_raw AS (…)` is a parser error,
	// and a multi-step query (a de-dup grid, a cohort) is written as a CTE by
	// every agent and every analyst.
	leadingWithPattern = regexp.MustCompile(`(?is)^with\s+(recursive\s+)?`)
)

func scopedReadonlySQL(sqlText string, projectID string, rules []softDeleteRule) (string, []any, error) {
	// Normalize: strip trailing semicolons before validation and query building.
	sqlText = strings.TrimRight(strings.TrimSpace(sqlText), ";")
	if err := validateReadonlySQL(sqlText); err != nil {
		return "", nil, err
	}
	if strings.Contains(sqlText, "?") {
		return "", nil, fmt.Errorf("SQL parameters are not supported; use {project_id}")
	}
	// JOIN events is still rejected — the contract predates the sandbox and
	// stays so saved queries and agent SQL keep one supported shape.
	if eventsJoinPattern.MatchString(sqlText) {
		return "", nil, fmt.Errorf("SQL-lite does not support joining the events table")
	}
	hasExternal := externalSourcePattern.MatchString(sqlText)
	eventsMatches := eventsSourcePattern.FindAllStringIndex(sqlText, -1)
	hasEvents := len(eventsMatches) > 0
	if hasEvents && len(eventsMatches) != 1 {
		return "", nil, fmt.Errorf("SQL must read from the events table exactly once")
	}
	if !hasEvents && !hasExternal {
		return "", nil, fmt.Errorf("SQL must read from the events table exactly once (or from external_rows)")
	}

	query := sqlText
	if hasEvents {
		query = eventsSourcePattern.ReplaceAllString(query, "FROM scoped_events")
	}
	if hasExternal {
		query = externalSourcePattern.ReplaceAllString(query, "${1} scoped_external_rows")
	}
	// Fail closed: any reference the rewrite did not catch (comma join, quoted
	// identifier, second occurrence) is rejected rather than rewritten —
	// inside the sandbox it could only ever see this project's rows, but the
	// contract is that the two names appear exactly where the rewrite expects.
	if residualSourcePattern.MatchString(sqlStringLiteral.ReplaceAllString(query, "''")) {
		return "", nil, fmt.Errorf("the events and external_rows tables may only be referenced directly after FROM or JOIN (comma joins and quoted table names are not supported)")
	}
	projectPlaceholders := strings.Count(query, "{project_id}")
	query = strings.ReplaceAll(query, "{project_id}", "?")

	// CTE args come first (the CTEs precede the user query), in CTE order;
	// {project_id} placeholder args follow.
	var ctes []string
	args := []any{}
	if hasEvents {
		// scoped_events exposes a stitched `canonical_id` column: anonymous events are
		// folded onto the identified user they later aliased to. Raw `distinct_id` is
		// left untouched for exact-match filters; counts of unique users / retention
		// should read `canonical_id` so a visitor who later logs in is one person, not
		// two. The stitch is the resolved_events view's LEFT JOIN on the aliases mirror.
		args = append(args, projectID)
		ctes = append(ctes, "scoped_events AS (SELECT *, canonical_distinct_id AS canonical_id FROM resolved_events WHERE project_id = ?)")
	}
	if hasExternal {
		// external_rows is keyed by (project, connector, table, row_key) with
		// INSERT OR REPLACE on write, so the newest sync's row is the only row —
		// there is no duplicate left for a read to collapse. The per-connector/table
		// soft-delete predicate rides the same CTE so a row the source marked
		// deleted reads as gone here exactly as it does in dataset_preview —
		// one contract, one predicate (softDeleteCondition). Hard deletes are
		// still invisible: a row removed in the source without a deletion mark
		// stays, which the preview warnings state.
		externalFilter := ""
		for _, r := range rules {
			if cond := softDeleteCondition(r.Column, r.Semantics); cond != "" {
				externalFilter += ` AND NOT (connector_id = ` + sqlQuote(r.ConnectorID) + ` AND table_name = ` + sqlQuote(r.Table) + ` AND ` + cond + `)`
			}
		}
		args = append(args, projectID)
		ctes = append(ctes, "scoped_external_rows AS (SELECT * FROM external_rows WHERE project_id = ?"+externalFilter+")")
	}
	for range projectPlaceholders {
		args = append(args, projectID)
	}
	// Our scoped CTEs go first, then the caller's own list if it has one: a
	// second `WITH` keyword would not parse, so a query that opens with one has
	// its keyword replaced and its CTEs appended to ours.
	if m := leadingWithPattern.FindStringIndex(query); m != nil {
		prefix := "WITH "
		if strings.Contains(strings.ToUpper(query[m[0]:m[1]]), "RECURSIVE") {
			prefix = "WITH RECURSIVE "
		}
		return prefix + strings.Join(ctes, ", ") + ", " + query[m[1]:], args, nil
	}
	query = "WITH " + strings.Join(ctes, ", ") + " " + query
	return query, args, nil
}

func validateReadonlySQL(sqlText string) error {
	// Strip trailing semicolons — LLMs routinely end SQL with one, and
	// single-statement SQL is safe without them.
	trimmed := strings.TrimRight(strings.TrimSpace(sqlText), ";")
	upper := strings.ToUpper(trimmed)
	if trimmed == "" {
		return fmt.Errorf("SQL is required")
	}
	if strings.Contains(trimmed, ";") {
		return fmt.Errorf("SQL must be a single statement")
	}
	if !(strings.HasPrefix(upper, "SELECT") || strings.HasPrefix(upper, "WITH")) {
		return fmt.Errorf("only SELECT queries are allowed")
	}
	// Match keywords as whole tokens, over SQL whose comments and quoted spans
	// have been blanked. A raw substring test rejects the ordinary vocabulary of
	// commerce -- `created_at` contains CREATE, `insert_id` contains INSERT,
	// `subscription_granted` contains GRANT -- and run_sql is the only path in
	// the product to a revenue number, so that guard denied the money questions
	// it was never aimed at. It even rejected the de-dup recipe the Data Analyst
	// preset teaches for money totals (`GROUP BY insert_id`). Blanking literals
	// first means an event *named* 'order_created' is data, not a statement.
	// The security property is unchanged: a real DDL/DML statement still has its
	// keyword as a bare token, and the SELECT/WITH prefix plus the single-
	// statement rule already forbid appending one.
	if keyword := forbiddenKeyword(maskSQLLiterals(stripSQLComments(trimmed))); keyword != "" {
		return fmt.Errorf("forbidden SQL keyword: %s", keyword)
	}
	// Defense-in-depth against the table-function bypass. The primary security
	// property is the sandbox itself — a per-project in-memory instance whose
	// runner connection has enable_external_access=false and a locked
	// configuration — but the denylist stays so a known SSRF / file-read
	// function name fails with a clear error instead of an engine one, and so
	// the guard still holds if the query ever runs outside the sandbox.
	// Comments are stripped first, because a comment is whitespace between the
	// name and its paren (e.g. `read_csv/**/('…')`), which would otherwise slip
	// past the `name(` match. Matched as `name(` so a column literally named
	// `url` is unaffected.
	//
	// Both checks run on literal-masked SQL: a string literal is data, so
	// `WHERE event_name = 'query(foo)'` must not trip the table-function
	// check, and `FROM 'checkout'` inside a literal must not trip the
	// file-read check. The mask keeps the quote characters, so a real
	// `FROM 'x.csv'` still matches `from\s*'`.
	masked := maskSQLLiterals(stripSQLComments(sqlText))
	if fn := forbiddenTableFunction(masked); fn != "" {
		return fmt.Errorf("forbidden table function: %s", fn)
	}
	// DuckDB reads a bare string after FROM/JOIN as a file path ('data.csv',
	// 's3://…'). The sandbox's enable_external_access=false would already refuse
	// it; rejecting here returns the clearer error and keeps the guard honest
	// outside the sandbox too. Single quotes only — a double-quoted token is
	// an identifier, not a path.
	if fromStringLiteralPattern.MatchString(masked) {
		return fmt.Errorf("reading a file or URL as a table is not allowed")
	}
	return nil
}

// sqlCommentPattern matches SQL comments: `/* … */` blocks (including
// across newlines) and `-- …` / `# …` line comments to end of line.
var sqlCommentPattern = regexp.MustCompile(`(?s)/\*.*?\*/|--[^\n]*|#[^\n]*`)

// stripSQLComments replaces every comment with a single space so comment text can
// neither hide a forbidden token nor bridge a function name to its `(`. Replacing
// with a space (not "") keeps adjacent tokens separated so no false call is
// synthesized. It is intentionally string-literal-naive: a comment sequence
// inside a quoted string is also blanked, which can only make the downstream
// denylist stricter (a forbidden name inside a literal isn't a call anyway).
func stripSQLComments(sqlText string) string {
	return sqlCommentPattern.ReplaceAllString(sqlText, " ")
}

// tableFunctionPattern matches a table function call — an identifier
// immediately followed by `(`, tolerant of whitespace between name and paren.
var tableFunctionPattern = regexp.MustCompile(`(?i)\b([a-z_][a-z0-9_]*)\s*\(`)

// forbiddenTableFunctions are table functions that read outside the sandbox:
// network egress (SSRF), file reads, other-database scans, and dynamic
// SQL/table access. run_sql must never reach them; names from other engines
// stay listed so a query written for a different dialect fails loudly here
// rather than being reinterpreted.
var forbiddenTableFunctions = map[string]bool{
	"url": true, "urlcluster": true, "remote": true, "remotesecure": true,
	"mysql": true, "postgresql": true, "mongodb": true, "redis": true,
	"file": true, "s3": true, "s3cluster": true, "hdfs": true, "hdfscluster": true,
	"jdbc": true, "odbc": true, "sqlite": true, "azureblobstorage": true, "deltalake": true,
	"iceberg": true, "gcs": true, "dictionary": true, "cluster": true, "clusterallreplicas": true,
	// DuckDB file/network readers and dynamic-SQL escapes.
	"read_csv": true, "read_csv_auto": true, "read_parquet": true, "read_json": true,
	"read_json_auto": true, "read_ndjson": true, "read_text": true, "read_blob": true,
	"parquet_scan": true, "csv_scan": true, "json_scan": true, "ndjson_scan": true,
	"glob": true, "sqlite_scan": true, "postgres_scan": true, "mysql_scan": true,
	"query": true, "query_table": true, "parquet_metadata": true, "parquet_schema": true,
	"parquet_file_metadata": true, "parquet_kv_metadata": true, "parquet_bloom_probe": true,
	"delta_scan": true, "iceberg_scan": true, "iceberg_metadata": true, "iceberg_snapshots": true,
	"excel_text": true, "read_xlsx": true, "st_read": true, "http_request": true,
}

func forbiddenTableFunction(sqlText string) string {
	for _, m := range tableFunctionPattern.FindAllStringSubmatch(sqlText, -1) {
		if forbiddenTableFunctions[strings.ToLower(m[1])] {
			return strings.ToLower(m[1])
		}
	}
	return ""
}

// forbiddenKeywordPattern matches a statement keyword only as a whole token, so
// an identifier that merely contains one (created_at, insert_id, updated_at,
// subscription_granted) is left alone. Go's \b breaks on [0-9A-Za-z_], which is
// exactly the SQL identifier alphabet. The DuckDB additions (ATTACH, SET,
// PRAGMA, COPY, INSTALL, LOAD, CALL, PREPARE, EXECUTE, USE, CHECKPOINT,
// VACUUM, ANALYZE, EXPORT, IMPORT, PIVOT is allowed) cover the statements a
// SELECT/WITH prefix would already reject at position 0 — the list is
// belt-and-suspenders for a keyword smuggled inside a subquery or CTE body.
var forbiddenKeywordPattern = regexp.MustCompile(`(?i)\b(DROP|DELETE|INSERT|UPDATE|ALTER|CREATE|TRUNCATE|SYSTEM|GRANT|REVOKE|ATTACH|DETACH|USE|SET|PRAGMA|INSTALL|LOAD|COPY|EXPORT|IMPORT|CALL|PREPARE|EXECUTE|CHECKPOINT|VACUUM|ANALYZE|RESET)\b`)

// fromStringLiteralPattern catches DuckDB's file-read sugar — `FROM 'x.csv'` —
// which needs no function name to escape the sandbox's data. Single-quoted
// only: a double-quoted token is an identifier, not a path.
var fromStringLiteralPattern = regexp.MustCompile(`(?i)\b(from|join)\s*'`)

// forbiddenKeyword returns the first statement keyword appearing as a bare token
// in sqlText, or "". Callers pass SQL with comments and quoted spans already
// blanked so neither can carry a false positive.
func forbiddenKeyword(sqlText string) string {
	if m := forbiddenKeywordPattern.FindString(sqlText); m != "" {
		return strings.ToUpper(m)
	}
	return ""
}

// maskSQLLiterals blanks the inside of quoted strings and quoted identifiers,
// keeping the quotes and the original length so offsets and token boundaries
// still line up. Data is not a statement: an event named 'order_created' or a
// property named 'updated_at' must not read as DDL to the denylist. Handles the
// two escape forms accepted inside a literal -- a doubled quote and a
// backslash escape.
func maskSQLLiterals(sqlText string) string {
	var b strings.Builder
	b.Grow(len(sqlText))
	runes := []rune(sqlText)
	for i := 0; i < len(runes); {
		quote := runes[i]
		if quote != '\'' && quote != '"' && quote != '`' {
			b.WriteRune(runes[i])
			i++
			continue
		}
		b.WriteRune(quote)
		i++
		for i < len(runes) {
			if runes[i] == '\\' && i+1 < len(runes) {
				b.WriteString("  ")
				i += 2
				continue
			}
			if runes[i] == quote {
				if i+1 < len(runes) && runes[i+1] == quote {
					b.WriteString("  ")
					i += 2
					continue
				}
				break
			}
			b.WriteRune(' ')
			i++
		}
		if i < len(runes) {
			b.WriteRune(quote)
			i++
		}
	}
	return b.String()
}

func normalizeSQLValue(value any) any {
	if value == nil {
		return nil
	}
	reflected := reflect.ValueOf(value)
	for reflected.Kind() == reflect.Ptr {
		if reflected.IsNil() {
			return nil
		}
		reflected = reflected.Elem()
	}
	switch v := reflected.Interface().(type) {
	case []byte:
		return string(v)
	case time.Time:
		return v.UTC()
	case duckdb.Interval:
		// INTERVAL scans as a struct; render it as a duration string so the
		// JSON answer is readable.
		return fmt.Sprintf("%d months %d days %d µs", v.Months, v.Days, v.Micros)
	case duckdb.Decimal:
		// DECIMAL scans as a struct holding a *big.Int. Render the exact
		// decimal text: money and cost columns live here, and a struct is
		// neither readable nor (before this) encodable across the sandbox.
		return v.String()
	case *big.Int:
		return v.String()
	case duckdb.Bit:
		// The driver rewrites a TOP-LEVEL BIT to its string form in rows.Next,
		// but not one nested in a LIST or STRUCT, and gob cannot carry the
		// named type: `SELECT ['101'::BIT]` would fail the whole query.
		return v.String()
	case big.Int:
		return v.String()
	case duckdb.Map:
		// MAP scans as a Go map with non-string keys, which neither JSON nor
		// the frame codec carries; keys become their text form.
		return stringKeyedMap(v)
	case duckdb.OrderedMap:
		return orderedStringKeyedMap(&v)
	case duckdb.Union:
		return map[string]any{"tag": v.Tag, "value": normalizeSQLValue(v.Value)}
	case []any:
		// LIST/ARRAY results: recurse, because the elements are driver values
		// too — a LIST of DECIMAL is as unrenderable as a bare DECIMAL, and the
		// frame codec carries neither.
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = normalizeSQLValue(item)
		}
		return out
	case map[string]any:
		// STRUCT results: same, one level down.
		out := make(map[string]any, len(v))
		for k, item := range v {
			out[k] = normalizeSQLValue(item)
		}
		return out
	default:
		return v
	}
}

// stringKeyedMap renders a DuckDB MAP as a JSON-ready object.
func stringKeyedMap(m duckdb.Map) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[fmt.Sprint(normalizeSQLValue(k))] = normalizeSQLValue(v)
	}
	return out
}

// orderedStringKeyedMap is the same for the order-preserving form the driver
// returns by default. Key order is lost, which is inherent to a JSON object.
func orderedStringKeyedMap(m *duckdb.OrderedMap) map[string]any {
	keys, values := m.Keys(), m.Values()
	out := make(map[string]any, len(keys))
	for i, k := range keys {
		if i >= len(values) {
			break
		}
		out[fmt.Sprint(normalizeSQLValue(k))] = normalizeSQLValue(values[i])
	}
	return out
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func nullableString(value string) sql.NullString {
	return sql.NullString{String: value, Valid: value != ""}
}
