package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lohi-ai/agentray/internal/config"
)

type Store struct {
	pg *pgxpool.Pool
	ch clickhouse.Conn
	// chRO is a least-privilege ClickHouse connection (readonly=2, GRANT SELECT on
	// the project database only, no table-function grants) used for every
	// user-/agent-authored SELECT so a table-function bypass cannot exfiltrate.
	// nil when no RO account is configured (dev), in which case readConn() falls
	// back to the privileged ch. See migrateClickHouse / provisionReadonlyRole.
	chRO       clickhouse.Conn
	chDatabase string
	resolvers  *resolverCache

	// personUpdates carries durably-stored batches to a single background applier
	// goroutine that maintains the persons profile table off the ingest ack path.
	// A single consumer preserves the single-writer read-merge-write invariant the
	// profile merge relies on; nil when the applier isn't running (tests), in which
	// case SinkEvents falls back to applying inline. See startPersonApplier.
	personUpdates chan []Event
	personDone    chan struct{}
	personWG      sync.WaitGroup
}

// readConn returns the connection that untrusted SELECTs (run_sql, saved
// queries, agent SQL) must use: the least-privilege RO connection when
// configured, otherwise the primary connection.
func (s *Store) readConn() clickhouse.Conn {
	if s.chRO != nil {
		return s.chRO
	}
	return s.ch
}

// resolverCache memoizes the per-project identity resolver. canonical-id
// stitching itself now runs in ClickHouse via the aliases_dict dictionary, but
// person-scoped filters still need the in-memory alias pairs
// (relatedDistinctIDs). Without this cache every analytics call and every
// run_sql re-read the whole alias table from Postgres — a hot-path round trip an
// agent firing many queries pays repeatedly. A short TTL bounds staleness; new
// aliases also invalidate the entry explicitly on write, so a freshly-identified
// user stitches immediately for filtering.
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
	ID          string    `json:"id"`
	WorkspaceID string    `json:"workspace_id,omitempty"`
	Name        string    `json:"name"`
	APIKey      string    `json:"api_key"`
	CreatedAt   time.Time `json:"created_at"`
}

type Dashboard struct {
	ID          string    `json:"id"`
	ProjectID   string    `json:"project_id"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

type Chart struct {
	ID          string    `json:"id"`
	DashboardID string    `json:"dashboard_id"`
	ProjectID   string    `json:"project_id"`
	Name        string    `json:"name"`
	Kind        string    `json:"kind"`
	Metric      string    `json:"metric"`
	EventName   string    `json:"event_name"`
	EventType   string    `json:"event_type"`
	SQL         string    `json:"sql"`
	XField      string    `json:"x_field"`
	YField      string    `json:"y_field"`
	SortOrder   int       `json:"sort_order"`
	ColSpan     int       `json:"col_span"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
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
	// InsertID is the caller-supplied idempotency key ($insert_id). It is captured
	// and stored so a future read-time de-dup (argMax(...) GROUP BY insert_id) can be
	// layered on if a money path ever needs one. NOTE: no read path de-dups on it
	// today. The one money-adjacent read — the retention "ever paid" flag — is
	// duplicate-safe by construction (it aggregates with max()/argMaxIf, so a
	// re-inserted `revenue` event can't change the boolean). Cost/token *sums* in the
	// daily rollups and raw agent reads are not de-duped; the pipeline keeps the
	// practical duplicate rate near zero (ack-after-insert + the JetStream duplicate
	// window), which the data-architecture doc explicitly accepts for count/sum
	// metrics. Do not describe a de-dup guard here that the code does not implement.
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
	EventName string    `json:"event_name"`
	EventType string    `json:"event_type"`
	Count     uint64    `json:"count"`
	LastSeen  time.Time `json:"last_seen"`
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
// into a safe ClickHouse boolean — the single place a custom audience becomes
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
	database     string // ClickHouse database that holds aliases_dict
	anonymousIDs []string
	canonicalIDs []string
}

type EventExplorer struct {
	Events    []Event   `json:"events"`
	Timeline  []Event   `json:"timeline"`
	Generated time.Time `json:"generated_at"`
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
	pg, err := pgxpool.New(ctx, cfg.PostgresURL)
	if err != nil {
		return nil, err
	}
	ch, err := clickhouse.Open(&clickhouse.Options{
		Addr: []string{cfg.ClickHouseAddr},
		Auth: clickhouse.Auth{
			Database: cfg.ClickHouseDatabase,
			Username: cfg.ClickHouseUser,
			Password: cfg.ClickHousePassword,
		},
		DialTimeout: 10 * time.Second,
	})
	if err != nil {
		pg.Close()
		return nil, err
	}
	store := &Store{
		pg:         pg,
		ch:         ch,
		chDatabase: cfg.ClickHouseDatabase,
		resolvers:  newResolverCache(30 * time.Second),
	}
	if err := store.migrate(ctx, cfg); err != nil {
		store.Close()
		return nil, err
	}
	// Maintain person profiles off the ingest ack path: a single background writer
	// keeps ingest throughput independent of the per-batch profile read-merge-write.
	store.startPersonApplier()
	// Open the least-privilege read connection once the RO role exists (created in
	// migrateClickHouse). Non-fatal: if the account can't be opened we log and fall
	// back to the privileged conn so a misconfiguration degrades to today's
	// behavior rather than taking analytics down.
	if cfg.ClickHouseROUser != "" {
		ro, err := clickhouse.Open(&clickhouse.Options{
			Addr: []string{cfg.ClickHouseAddr},
			Auth: clickhouse.Auth{
				Database: cfg.ClickHouseDatabase,
				Username: cfg.ClickHouseROUser,
				Password: cfg.ClickHouseROPassword,
			},
			DialTimeout: 10 * time.Second,
		})
		if err != nil {
			fmt.Printf("warn: open ClickHouse read-only conn: %v\n", err)
		} else if err := ro.Ping(ctx); err != nil {
			fmt.Printf("warn: ping ClickHouse read-only conn: %v\n", err)
			_ = ro.Close()
		} else {
			store.chRO = ro
		}
	}
	// Seed the alias dictionary from existing Postgres aliases. Non-fatal:
	// dual-writes keep it current going forward, and a failure here only means
	// historical stitches wait for the next boot — never a startup blocker.
	if err := store.backfillAliasDictionary(ctx); err != nil {
		fmt.Printf("warn: backfillAliasDictionary: %v\n", err)
	}
	if err := store.SeedSystemTemplates(ctx); err != nil {
		// Non-fatal: Templates page shows EmptyState on failure.
		fmt.Printf("warn: SeedSystemTemplates: %v\n", err)
	}
	return store, nil
}

func (s *Store) Close() {
	// Drain in-flight person updates before the ClickHouse conn they write through
	// is closed, so a shutdown doesn't lose the profile side of already-acked events.
	s.stopPersonApplier()
	if s.pg != nil {
		s.pg.Close()
	}
	if s.ch != nil {
		_ = s.ch.Close()
	}
	if s.chRO != nil {
		_ = s.chRO.Close()
	}
}

// migrate brings both stores up to schema: the relational data in Postgres and
// the event table in ClickHouse. Split into migratePostgres / migrateClickHouse so
// each backend can be migrated on its own (a Postgres-only integration test runs
// the relational half without a ClickHouse connection).
func (s *Store) migrate(ctx context.Context, cfg config.Config) error {
	if err := s.migratePostgres(ctx, cfg); err != nil {
		return err
	}
	return s.migrateClickHouse(ctx, cfg)
}

func (s *Store) migratePostgres(ctx context.Context, cfg config.Config) error {
	if _, err := s.pg.Exec(ctx, `CREATE EXTENSION IF NOT EXISTS pgcrypto`); err != nil {
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
	if _, err := s.pg.Exec(ctx, `
CREATE TABLE IF NOT EXISTS projects (
	id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
	workspace_id UUID REFERENCES workspaces(id) ON DELETE SET NULL,
	name VARCHAR(255) NOT NULL,
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
CREATE TABLE IF NOT EXISTS query_feedback (
	id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
	query_id UUID NOT NULL REFERENCES saved_queries(id),
	rating SMALLINT NOT NULL,
	correction TEXT,
	created_at TIMESTAMPTZ NOT NULL DEFAULT now()
)`); err != nil {
		return err
	}
	if _, err := s.pg.Exec(ctx, `
CREATE INDEX IF NOT EXISTS query_feedback_query_created_idx
ON query_feedback (query_id, created_at DESC)`); err != nil {
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
		// #3b: on the compose quickstart, fill the default project with a couple of
		// days of synthetic events so a first-time visitor sees populated dashboards
		// instead of empty states. Opt-in and idempotent; a seeding hiccup must never
		// block boot, so it is best-effort.
		if cfg.SeedDemo {
			if err := s.SeedDemoEvents(ctx, defaultProjectID); err != nil {
				fmt.Printf("warn: SeedDemoEvents(%s): %v\n", defaultProjectID, err)
			}
		}
	}

	if err := s.migrateAlerts(ctx); err != nil {
		return err
	}

	if err := s.migrateConnectors(ctx); err != nil {
		return err
	}

	return nil
}

// chIdentLiteral escapes a value for inclusion inside a single-quoted ClickHouse
// string literal (used when building the aliases_dict DDL from config-supplied
// credentials). Backslash first, then the quote.
func chIdentLiteral(v string) string {
	v = strings.ReplaceAll(v, `\`, `\\`)
	return strings.ReplaceAll(v, `'`, `\'`)
}

// chIdentBacktick quotes a ClickHouse identifier (database/table/user) with
// backticks, doubling any embedded backtick. Use for object names in DDL where a
// string literal would be wrong (e.g. GRANT SELECT ON `db`.*).
func chIdentBacktick(v string) string {
	return "`" + strings.ReplaceAll(v, "`", "``") + "`"
}

func (s *Store) migrateClickHouse(ctx context.Context, cfg config.Config) error {
	if err := s.ch.Exec(ctx, `
CREATE TABLE IF NOT EXISTS events (
	project_id UUID,
	event_id UUID DEFAULT generateUUIDv4(),
	distinct_id String,
	session_id String,
	event_name LowCardinality(String),
	event_type LowCardinality(String),
	properties String,
	agent_id Nullable(String),
	tool_name Nullable(String),
	tool_input Nullable(String),
	tool_output Nullable(String),
	tokens_input Nullable(UInt32),
	tokens_output Nullable(UInt32),
	cost_usd Nullable(Float32),
	latency_ms Nullable(UInt32),
	model_name Nullable(String),
	is_error UInt8 DEFAULT 0,
	error_message Nullable(String),
	timestamp DateTime64(3, 'UTC'),
	inserted_at DateTime64(3, 'UTC') DEFAULT now64()
)
ENGINE = MergeTree()
PARTITION BY toYYYYMM(timestamp)
ORDER BY (project_id, event_name, timestamp, distinct_id)
TTL toDateTime(timestamp) + INTERVAL 1 YEAR`); err != nil {
		return err
	}
	if err := s.ch.Exec(ctx, `ALTER TABLE events ADD COLUMN IF NOT EXISTS visitor_class LowCardinality(String) DEFAULT 'human'`); err != nil {
		return err
	}
	if err := s.ch.Exec(ctx, `ALTER TABLE events ADD COLUMN IF NOT EXISTS bot_name Nullable(String)`); err != nil {
		return err
	}
	if err := s.ch.Exec(ctx, `ALTER TABLE events ADD COLUMN IF NOT EXISTS referrer_host Nullable(String)`); err != nil {
		return err
	}
	if err := s.ch.Exec(ctx, `ALTER TABLE events ADD COLUMN IF NOT EXISTS referrer_channel LowCardinality(String) DEFAULT ''`); err != nil {
		return err
	}
	if err := s.ch.Exec(ctx, `ALTER TABLE events ADD COLUMN IF NOT EXISTS user_agent Nullable(String)`); err != nil {
		return err
	}
	if err := s.ch.Exec(ctx, `ALTER TABLE events ADD COLUMN IF NOT EXISTS insert_id Nullable(String)`); err != nil {
		return err
	}
	// is_unplanned tags events whose name was absent from the project's established
	// event catalog at capture time (typo'd / untracked names). Advisory, never
	// rejected — the tracking.unplanned_event digest and Growth agent watch it.
	if err := s.ch.Exec(ctx, `ALTER TABLE events ADD COLUMN IF NOT EXISTS is_unplanned UInt8 DEFAULT 0`); err != nil {
		return err
	}
	// Identity alias map + dictionary. canonical-id stitching used to ship two
	// N-sized arrays into a transform() on every analytics query and every
	// run_sql (N = aliases in the project) — unbounded query payload plus a
	// per-row scan. The dictionary turns that into an in-memory keyed lookup:
	// dictGet by (project_id, distinct_id). Source is this local
	// ReplacingMergeTree, which the app keeps in sync from Postgres (the source
	// of truth) via dual-write + boot backfill; LIFETIME bounds staleness for a
	// brand-new identity to ~1 min.
	if err := s.ch.Exec(ctx, `
CREATE TABLE IF NOT EXISTS aliases (
	project_id UUID,
	anonymous_id String,
	canonical_id String,
	version DateTime64(3, 'UTC') DEFAULT now64()
)
ENGINE = ReplacingMergeTree(version)
ORDER BY (project_id, anonymous_id)`); err != nil {
		return err
	}
	if err := s.ch.Exec(ctx, fmt.Sprintf(`
CREATE DICTIONARY IF NOT EXISTS aliases_dict (
	project_id UUID,
	anonymous_id String,
	canonical_id String
)
PRIMARY KEY project_id, anonymous_id
SOURCE(CLICKHOUSE(TABLE 'aliases' DB '%s' USER '%s' PASSWORD '%s'))
LAYOUT(COMPLEX_KEY_HASHED())
LIFETIME(MIN 30 MAX 60)`,
		chIdentLiteral(cfg.ClickHouseDatabase),
		chIdentLiteral(cfg.ClickHouseUser),
		chIdentLiteral(cfg.ClickHousePassword))); err != nil {
		return err
	}
	if err := s.ch.Exec(ctx, `
CREATE MATERIALIZED VIEW IF NOT EXISTS sessions_mv
ENGINE = AggregatingMergeTree()
ORDER BY (project_id, session_id, distinct_id)
AS SELECT
	project_id,
	session_id,
	distinct_id,
	minState(timestamp) AS session_start,
	maxState(timestamp) AS session_end,
	countState() AS event_count,
	sumState(toUInt64(ifNull(tokens_input, toUInt32(0)))) AS total_tokens_in,
	sumState(toUInt64(ifNull(tokens_output, toUInt32(0)))) AS total_tokens_out,
	sumState(toFloat64(ifNull(cost_usd, toFloat32(0)))) AS total_cost_usd,
	maxState(timestamp) AS last_event_at
FROM events
WHERE session_id != ''
GROUP BY project_id, session_id, distinct_id`); err != nil {
		return err
	}
	if err := s.migrateRollups(ctx); err != nil {
		return err
	}
	if err := s.migratePersons(ctx); err != nil {
		return err
	}
	if err := s.migrateAgent(ctx); err != nil {
		return err
	}
	if err := s.migrateAgentTrace(ctx); err != nil {
		return err
	}
	if err := s.migrateAgentSessionLog(ctx); err != nil {
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
	// external_rows is the landing table for data-connector syncs: one wide
	// JSON row per source row, deduplicated on merge by the replacing key so
	// snapshot re-syncs and retried batches are idempotent. `cursor` versions
	// the replacement so the newest pull of a row wins; run_sql reaches this
	// table through the scoped_external_rows rewrite (scopedReadonlySQL) and
	// the readonly role's database-wide SELECT grant already covers it.
	if err := s.ch.Exec(ctx, `
CREATE TABLE IF NOT EXISTS external_rows (
	project_id UUID,
	connector_id UUID,
	table_name LowCardinality(String),
	row_key String,
	cursor String,
	data String,
	synced_at DateTime64(3, 'UTC') DEFAULT now64()
)
ENGINE = ReplacingMergeTree(synced_at)
ORDER BY (project_id, connector_id, table_name, row_key)`); err != nil {
		return err
	}
	if err := s.provisionReadonlyRole(ctx, cfg); err != nil {
		return err
	}
	return nil
}

// The rollup aggregation bodies are shared verbatim between the materialized view
// (which maintains the table going forward) and the one-time history backfill, so
// the two can never silently diverge (a divergence is a wrong-number bug: the
// pre-MV history would aggregate differently from post-MV rows). The MV prepends
// `CREATE MATERIALIZED VIEW ... TO <table> AS`, the backfill prepends
// `INSERT INTO <table>` — everything after is identical.
const eventsDailySelect = `
SELECT
	project_id,
	toDate(timestamp) AS day,
	event_type,
	event_name,
	visitor_class,
	referrer_channel,
	countState() AS events,
	uniqState(distinct_id) AS uniq_users,
	sumState(toUInt64(ifNull(tokens_input, toUInt32(0)))) AS tokens_in,
	sumState(toUInt64(ifNull(tokens_output, toUInt32(0)))) AS tokens_out,
	sumState(toFloat64(ifNull(cost_usd, toFloat32(0)))) AS cost_usd
FROM events
GROUP BY project_id, day, event_type, event_name, visitor_class, referrer_channel`

const agentUsageDailySelect = `
SELECT
	project_id,
	toDate(timestamp) AS day,
	ifNull(agent_id, 'unknown') AS agent_id,
	ifNull(model_name, 'unknown') AS model_name,
	countState() AS events,
	sumState(toUInt64(ifNull(tokens_input, toUInt32(0)))) AS tokens_in,
	sumState(toUInt64(ifNull(tokens_output, toUInt32(0)))) AS tokens_out,
	sumState(toFloat64(ifNull(cost_usd, toFloat32(0)))) AS cost_usd,
	sumState(toFloat64(ifNull(latency_ms, toUInt32(0)))) AS latency_sum,
	sumState(toUInt64(is_error)) AS errors
FROM events
WHERE event_type = 'agent'
GROUP BY project_id, day, agent_id, model_name`

// migrateRollups provisions the daily rollup tables + materialized views that let
// dashboard/agent reads answer common time-series and agent-usage questions from a
// small pre-aggregated table instead of re-scanning raw `events` every request
// (the raw table grows linearly with volume). It also applies compression codecs
// to the two fat/monotonic columns and backfills history exactly once.
//
// Pattern: an explicit target table (AggregatingMergeTree) plus a `TO`
// materialized view, so history can be backfilled with a plain INSERT … SELECT.
// The MV only sees rows inserted after its creation; the one-time backfill covers
// everything already stored. This runs during boot migration, before the ingest
// worker starts, so there are no concurrent inserts to double-count — and a marker
// row makes the backfill idempotent across restarts (the W5 "silent undercount"
// trap the design calls out).
func (s *Store) migrateRollups(ctx context.Context) error {
	// Compression codecs on the two columns that dominate on-disk size / scan cost:
	// the JSON properties blob (ZSTD) and the monotonic timestamp (Delta+ZSTD).
	// MODIFY COLUMN only re-encodes new parts (old parts recompress on merge), so
	// this is safe and idempotent on every boot.
	for _, ddl := range []string{
		`ALTER TABLE events MODIFY COLUMN properties String CODEC(ZSTD(3))`,
		`ALTER TABLE events MODIFY COLUMN timestamp DateTime64(3, 'UTC') CODEC(Delta, ZSTD)`,
	} {
		if err := s.ch.Exec(ctx, ddl); err != nil {
			return fmt.Errorf("apply events codec: %w", err)
		}
	}

	// Small marker table so one-shot data migrations (backfills) run exactly once.
	// Doubles as the seed of a future migration ledger (P5).
	if err := s.ch.Exec(ctx, `
CREATE TABLE IF NOT EXISTS schema_markers (
	name String,
	applied_at DateTime64(3, 'UTC') DEFAULT now64()
)
ENGINE = MergeTree()
ORDER BY name`); err != nil {
		return err
	}

	// events_daily: per-day volume broken down by the dimensions dashboards group
	// on (event, type, human/bot class, acquisition channel). uniq_users is over
	// the raw distinct_id (pre-identity-stitch) — a fast daily-active approximation;
	// exact canonical DAU still comes from the identity-resolved raw path.
	if err := s.ch.Exec(ctx, `
CREATE TABLE IF NOT EXISTS events_daily (
	project_id UUID,
	day Date,
	event_type LowCardinality(String),
	event_name LowCardinality(String),
	visitor_class LowCardinality(String),
	referrer_channel LowCardinality(String),
	events AggregateFunction(count),
	uniq_users AggregateFunction(uniq, String),
	tokens_in AggregateFunction(sum, UInt64),
	tokens_out AggregateFunction(sum, UInt64),
	cost_usd AggregateFunction(sum, Float64)
)
ENGINE = AggregatingMergeTree()
PARTITION BY toYYYYMM(day)
ORDER BY (project_id, day, event_type, event_name, visitor_class, referrer_channel)`); err != nil {
		return err
	}
	if err := s.ch.Exec(ctx, `CREATE MATERIALIZED VIEW IF NOT EXISTS events_daily_mv TO events_daily AS`+eventsDailySelect); err != nil {
		return err
	}

	// agent_usage_daily: per-day agent cost/latency keyed exactly as agentInsightRows
	// groups (agent_id, model_name), restricted to agent events. latency_sum + events
	// reproduce the raw avg(coalesce(latency,0)) exactly, so the rollup answer equals
	// the raw answer for a day-aligned window.
	if err := s.ch.Exec(ctx, `
CREATE TABLE IF NOT EXISTS agent_usage_daily (
	project_id UUID,
	day Date,
	agent_id String,
	model_name String,
	events AggregateFunction(count),
	tokens_in AggregateFunction(sum, UInt64),
	tokens_out AggregateFunction(sum, UInt64),
	cost_usd AggregateFunction(sum, Float64),
	latency_sum AggregateFunction(sum, Float64),
	errors AggregateFunction(sum, UInt64)
)
ENGINE = AggregatingMergeTree()
PARTITION BY toYYYYMM(day)
ORDER BY (project_id, day, agent_id, model_name)`); err != nil {
		return err
	}
	if err := s.ch.Exec(ctx, `CREATE MATERIALIZED VIEW IF NOT EXISTS agent_usage_daily_mv TO agent_usage_daily AS`+agentUsageDailySelect); err != nil {
		return err
	}

	return s.backfillRollups(ctx)
}

// backfillRollups seeds the rollup tables from all pre-existing events, once. The
// marker guard makes it a no-op on subsequent boots (re-running would double-count,
// since post-creation rows already flow in via the materialized views).
func (s *Store) backfillRollups(ctx context.Context) error {
	const marker = "backfill_rollups_v1"
	var applied uint64
	if err := s.ch.QueryRow(ctx, `SELECT count() FROM schema_markers WHERE name = ?`, marker).Scan(&applied); err != nil {
		return fmt.Errorf("check rollup backfill marker: %w", err)
	}
	if applied > 0 {
		return nil
	}
	if err := s.ch.Exec(ctx, `INSERT INTO events_daily`+eventsDailySelect); err != nil {
		return fmt.Errorf("backfill events_daily: %w", err)
	}
	if err := s.ch.Exec(ctx, `INSERT INTO agent_usage_daily`+agentUsageDailySelect); err != nil {
		return fmt.Errorf("backfill agent_usage_daily: %w", err)
	}
	if err := s.ch.Exec(ctx, `INSERT INTO schema_markers (name) VALUES (?)`, marker); err != nil {
		return fmt.Errorf("record rollup backfill marker: %w", err)
	}
	return nil
}

// provisionReadonlyRole creates the least-privilege ClickHouse account that every
// untrusted SELECT runs as. The security property is not the SQL denylist (which
// a table function in a subquery can slip past) but ClickHouse's own grant model:
//   - readonly=2 profile: SELECT + SET only, no DDL/DML, no SYSTEM.
//   - GRANT SELECT on <database>.* only: no other database, no table functions
//     (url/remote/mysql/postgresql/file/s3 require CREATE TEMPORARY TABLE +
//     per-function grants that we never issue), so `SELECT * FROM url(...)` fails
//     with a grant error instead of performing SSRF / cross-tenant reads.
//
// No-op when no RO user is configured (dev). Idempotent: IF NOT EXISTS + repeated
// GRANT are safe to run on every boot.
func (s *Store) provisionReadonlyRole(ctx context.Context, cfg config.Config) error {
	if cfg.ClickHouseROUser == "" {
		return nil
	}
	// CREATE USER with an explicit readonly=2 settings profile constraint. The
	// password is set via IDENTIFIED WITH plaintext_password; empty password uses
	// no_password (trusted local CH only).
	identified := "IDENTIFIED WITH no_password"
	if cfg.ClickHouseROPassword != "" {
		identified = fmt.Sprintf("IDENTIFIED WITH plaintext_password BY '%s'", chIdentLiteral(cfg.ClickHouseROPassword))
	}
	// Resource caps so a single untrusted SELECT can't monopolise CPU/RAM/IO and
	// starve ingestion inserts. These ride on the same readonly=2 settings profile.
	// MAX constraints (not CONST) let a query lower a limit but never raise it above
	// the ceiling. Values are sized for the shared 2 GB-capped ClickHouse box.
	//   - max_execution_time 30s: kill runaway scans.
	//   - max_rows_to_read 1e9 / read_overflow_mode throw: bound work before it runs.
	//   - max_result_rows 1e6 + result_overflow_mode throw: cap payload back to the agent.
	//   - max_memory_usage 2 GiB: hard per-query RAM ceiling under the server cap.
	//   - max_bytes_before_external_group_by 1 GiB: spill big GROUP BYs to disk
	//     instead of OOM-killing the query (and the server with it).
	settings := strings.Join([]string{
		"readonly = 2 CONST",
		"max_execution_time = 30 MAX 30",
		"max_rows_to_read = 1000000000 MAX 1000000000",
		"read_overflow_mode = 'throw'",
		"max_result_rows = 1000000 MAX 1000000",
		"result_overflow_mode = 'throw'",
		"max_memory_usage = 2147483648 MAX 2147483648",
		"max_bytes_before_external_group_by = 1073741824",
	}, ", ")
	roUser := chIdentBacktick(cfg.ClickHouseROUser)
	stmts := []string{
		fmt.Sprintf(`CREATE USER IF NOT EXISTS %s %s SETTINGS %s`, roUser, identified, settings),
		// Ensure the profile settings stick even if the user pre-existed.
		fmt.Sprintf(`ALTER USER %s SETTINGS %s`, roUser, settings),
		// SELECT on the project database only. No GRANT on system table functions,
		// no other database, no CREATE TEMPORARY TABLE — table functions stay denied.
		fmt.Sprintf(`GRANT SELECT ON %s.* TO %s`, chIdentBacktick(cfg.ClickHouseDatabase), roUser),
	}
	for _, stmt := range stmts {
		if err := s.ch.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("provision readonly CH role: %w", err)
		}
	}
	return nil
}

type pgQuerier interface {
	Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// seedStarterDashboard gives every new project a ready-use board with
// predefined graphs from base activity, so dashboards are never empty.
func seedStarterDashboard(ctx context.Context, q pgQuerier, projectID string) error {
	starterCharts := []Chart{
		{Name: "Event trend", Kind: "line", Metric: "events"},
		{Name: "Top events", Kind: "bar", Metric: "event_breakdown"},
		{Name: "Sessions", Kind: "stat", Metric: "sessions"},
		{Name: "Agent cost", Kind: "stat", Metric: "cost", EventType: "agent"},
		{
			Name:   "Traffic: human / bot / AI",
			Kind:   "pie",
			SQL:    `SELECT ifNull(visitor_class, 'human') AS class, count() AS count FROM events WHERE event_name = 'user.pageview' GROUP BY class ORDER BY count DESC`,
			XField: "class",
			YField: "count",
		},
		{
			Name:   "Visitors: guest vs identified",
			Kind:   "bar",
			SQL:    `SELECT if(JSONExtractString(properties, 'email') != '' OR JSONExtractString(properties, '$set', 'email') != '', 'Identified', 'Guest') AS user_type, uniqExact(canonical_id) AS visitors FROM events WHERE event_name = 'user.pageview' AND ifNull(visitor_class, 'human') = 'human' GROUP BY user_type ORDER BY visitors DESC`,
			XField: "user_type",
			YField: "visitors",
		},
		{
			Name:   "Visitor retention",
			Kind:   "bar",
			SQL:    `SELECT if(visits > 1, 'Returning', 'First-time') AS visitor_type, count() AS visitors FROM (SELECT canonical_id, count() AS visits FROM events WHERE event_name = 'user.pageview' AND ifNull(visitor_class, 'human') = 'human' GROUP BY canonical_id) GROUP BY visitor_type ORDER BY visitors DESC`,
			XField: "visitor_type",
			YField: "visitors",
		},
	}
	var dashboardID string
	if err := q.QueryRow(ctx, `
INSERT INTO dashboards (project_id, name, description)
VALUES ($1, $2, $3)
RETURNING id::text`, projectID, "Product overview", "Starter board auto-created with predefined graphs from base activity.").Scan(&dashboardID); err != nil {
		return err
	}
	for _, chart := range starterCharts {
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
	err := s.pg.QueryRow(ctx, `SELECT id::text, coalesce(workspace_id::text, ''), name, api_key, created_at FROM projects WHERE api_key = $1`, apiKey).
		Scan(&p.ID, &p.WorkspaceID, &p.Name, &p.APIKey, &p.CreatedAt)
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
RETURNING id::text, coalesce(workspace_id::text, ''), name, api_key, created_at`, name, apiKey).
		Scan(&p.ID, &p.WorkspaceID, &p.Name, &p.APIKey, &p.CreatedAt)
	return p, err
}

func (s *Store) RotateProjectAPIKey(ctx context.Context, projectID string) (Project, error) {
	apiKey := "agentray_" + uuid.NewString()
	var p Project
	err := s.pg.QueryRow(ctx, `
UPDATE projects
SET api_key = $2
WHERE id = $1
RETURNING id::text, coalesce(workspace_id::text, ''), name, api_key, created_at`, projectID, apiKey).
		Scan(&p.ID, &p.WorkspaceID, &p.Name, &p.APIKey, &p.CreatedAt)
	return p, err
}

func (s *Store) ListDashboards(ctx context.Context, projectID string) ([]Dashboard, error) {
	rows, err := s.pg.Query(ctx, `
SELECT id::text, project_id::text, name, description, created_at, updated_at
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
		if err := rows.Scan(&d.ID, &d.ProjectID, &d.Name, &d.Description, &d.CreatedAt, &d.UpdatedAt); err != nil {
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
RETURNING id::text, project_id::text, name, description, created_at, updated_at`, projectID, name, description).
		Scan(&d.ID, &d.ProjectID, &d.Name, &d.Description, &d.CreatedAt, &d.UpdatedAt)
	return d, err
}

func (s *Store) UpdateDashboard(ctx context.Context, projectID string, dashboardID string, name string, description string) (Dashboard, error) {
	if name == "" {
		name = "Untitled dashboard"
	}
	var d Dashboard
	err := s.pg.QueryRow(ctx, `
UPDATE dashboards
SET name = $3, description = $4, updated_at = now()
WHERE project_id = $1 AND id = $2
RETURNING id::text, project_id::text, name, description, created_at, updated_at`, projectID, dashboardID, name, description).
		Scan(&d.ID, &d.ProjectID, &d.Name, &d.Description, &d.CreatedAt, &d.UpdatedAt)
	return d, err
}

func (s *Store) DeleteDashboard(ctx context.Context, projectID string, dashboardID string) error {
	_, err := s.pg.Exec(ctx, `DELETE FROM dashboards WHERE project_id = $1 AND id = $2`, projectID, dashboardID)
	return err
}

func (s *Store) ListCharts(ctx context.Context, projectID string, dashboardID string) ([]Chart, error) {
	rows, err := s.pg.Query(ctx, `
SELECT id::text, dashboard_id::text, project_id::text, name, kind, metric, event_name, event_type, sql, x_field, y_field, sort_order, col_span, created_at, updated_at
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
		if err := rows.Scan(&c.ID, &c.DashboardID, &c.ProjectID, &c.Name, &c.Kind, &c.Metric, &c.EventName, &c.EventType, &c.SQL, &c.XField, &c.YField, &c.SortOrder, &c.ColSpan, &c.CreatedAt, &c.UpdatedAt); err != nil {
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
RETURNING id::text, dashboard_id::text, project_id::text, name, kind, metric, event_name, event_type, sql, x_field, y_field, sort_order, col_span, created_at, updated_at`,
		chart.DashboardID, chart.ProjectID, chart.Name, chart.Kind, chart.Metric, chart.EventName, chart.EventType, chart.SQL, chart.XField, chart.YField, chart.ColSpan).
		Scan(&c.ID, &c.DashboardID, &c.ProjectID, &c.Name, &c.Kind, &c.Metric, &c.EventName, &c.EventType, &c.SQL, &c.XField, &c.YField, &c.SortOrder, &c.ColSpan, &c.CreatedAt, &c.UpdatedAt)
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
	err := s.pg.QueryRow(ctx, `
UPDATE charts
SET name = $3, kind = $4, metric = $5, event_name = $6, event_type = $7, sql = $8, x_field = $9, y_field = $10, col_span = $11, updated_at = now()
WHERE project_id = $1 AND id = $2
RETURNING id::text, dashboard_id::text, project_id::text, name, kind, metric, event_name, event_type, sql, x_field, y_field, sort_order, col_span, created_at, updated_at`,
		chart.ProjectID, chart.ID, chart.Name, chart.Kind, chart.Metric, chart.EventName, chart.EventType, chart.SQL, chart.XField, chart.YField, chart.ColSpan).
		Scan(&c.ID, &c.DashboardID, &c.ProjectID, &c.Name, &c.Kind, &c.Metric, &c.EventName, &c.EventType, &c.SQL, &c.XField, &c.YField, &c.SortOrder, &c.ColSpan, &c.CreatedAt, &c.UpdatedAt)
	return c, err
}

// ReorderCharts persists a new board order: each chart id in `chartIDs` gets its
// sort_order set to its index. Scoped to the dashboard + project so a caller can
// only reorder charts it owns. Runs in one transaction so the board never reads
// a half-applied order.
func (s *Store) ReorderCharts(ctx context.Context, projectID string, dashboardID string, chartIDs []string) error {
	tx, err := s.pg.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
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
				{Name: "DAU", Kind: "line", SQL: `SELECT toDate(timestamp) AS day, uniqExact(distinct_id) AS dau FROM events GROUP BY day ORDER BY day ASC LIMIT 30`, XField: "day", YField: "dau", SortOrder: 0},
				{Name: "MAU", Kind: "line", SQL: `SELECT toStartOfMonth(timestamp) AS month, uniqExact(distinct_id) AS mau FROM events GROUP BY month ORDER BY month ASC LIMIT 12`, XField: "month", YField: "mau", SortOrder: 1},
				{Name: "Event trend", Kind: "line", Metric: "events", SortOrder: 2},
				{Name: "Sessions", Kind: "stat", Metric: "sessions", SortOrder: 3},
				{Name: "Top events", Kind: "bar", Metric: "event_breakdown", SortOrder: 4},
				{Name: "AI cost", Kind: "stat", Metric: "cost", EventType: "agent", SortOrder: 5},
				{Name: "Traffic by class", Kind: "pie", SQL: `SELECT ifNull(visitor_class, 'human') AS class, count() AS count FROM events WHERE event_name = 'user.pageview' GROUP BY class ORDER BY count DESC`, XField: "class", YField: "count", SortOrder: 6},
				{Name: "Visitors: guest vs identified", Kind: "bar", SQL: `SELECT if(JSONExtractString(properties, 'email') != '' OR JSONExtractString(properties, '$set', 'email') != '', 'Identified', 'Guest') AS user_type, uniqExact(distinct_id) AS visitors FROM events WHERE event_name = 'user.pageview' GROUP BY user_type ORDER BY visitors DESC`, XField: "user_type", YField: "visitors", SortOrder: 7},
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
				{Name: "New vs returning (daily)", Kind: "line", SQL: `SELECT toDate(timestamp) AS day, uniqExactIf(distinct_id, is_first) AS new_readers, uniqExactIf(distinct_id, NOT is_first) AS returning_readers FROM (SELECT distinct_id, timestamp, min(timestamp) OVER (PARTITION BY distinct_id) = timestamp AS is_first FROM events) GROUP BY day ORDER BY day ASC LIMIT 30`, XField: "day", YField: "returning_readers", SortOrder: 0},
				{Name: "WAU", Kind: "line", SQL: `SELECT toStartOfWeek(timestamp) AS week, uniqExact(distinct_id) AS wau FROM events GROUP BY week ORDER BY week ASC LIMIT 12`, XField: "week", YField: "wau", SortOrder: 1},
				{Name: "Active readers (7d)", Kind: "stat", Metric: "sessions", SortOrder: 2},
				{Name: "Reading depth (events/reader)", Kind: "bar", SQL: `SELECT toDate(timestamp) AS day, round(count() / uniqExact(distinct_id), 1) AS events_per_reader FROM events GROUP BY day ORDER BY day ASC LIMIT 30`, XField: "day", YField: "events_per_reader", SortOrder: 3},
				{Name: "Top events", Kind: "bar", Metric: "event_breakdown", SortOrder: 4},
			},
		},
		{
			id:          MarketingFunnelTemplateID,
			name:        "Marketing & Acquisition",
			description: "Traffic sources, the visit→read→subscribe funnel, and guest-to-identified conversion.",
			charts: []TemplateChart{
				{Name: "Traffic by class", Kind: "pie", SQL: `SELECT ifNull(visitor_class, 'human') AS class, count() AS count FROM events WHERE event_name = 'user.pageview' GROUP BY class ORDER BY count DESC`, XField: "class", YField: "count", SortOrder: 0},
				{Name: "Top referrers", Kind: "bar", SQL: `SELECT ifNull(nullIf(JSONExtractString(properties, 'referrer'), ''), 'direct') AS referrer, count() AS visits FROM events WHERE event_name = 'user.pageview' GROUP BY referrer ORDER BY visits DESC LIMIT 10`, XField: "referrer", YField: "visits", SortOrder: 1},
				{Name: "Guest vs identified", Kind: "bar", SQL: `SELECT if(JSONExtractString(properties, 'email') != '' OR JSONExtractString(properties, '$set', 'email') != '', 'Identified', 'Guest') AS user_type, uniqExact(distinct_id) AS visitors FROM events WHERE event_name = 'user.pageview' GROUP BY user_type ORDER BY visitors DESC`, XField: "user_type", YField: "visitors", SortOrder: 2},
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
		return seedStarterDashboard(ctx, s.pg, projectID)
	}
	return nil
}

func (s *Store) InsertEvents(ctx context.Context, events []Event) error {
	batch, err := s.ch.PrepareBatch(ctx, `
INSERT INTO events (
	project_id, event_id, distinct_id, session_id, event_name, event_type,
	properties, agent_id, tool_name, tool_input, tool_output, tokens_input,
	tokens_output, cost_usd, latency_ms, model_name, is_error, error_message,
	timestamp, visitor_class, bot_name, referrer_host, referrer_channel, user_agent,
	insert_id, is_unplanned
)`)
	if err != nil {
		return err
	}
	for _, event := range events {
		projectID, err := uuid.Parse(event.ProjectID)
		if err != nil {
			return err
		}
		eventID, err := uuid.Parse(event.EventID)
		if err != nil {
			return err
		}
		if err := batch.Append(
			projectID,
			eventID,
			event.DistinctID,
			event.SessionID,
			event.EventName,
			event.EventType,
			event.Properties,
			nullableString(event.AgentID),
			nullableString(event.ToolName),
			nullableString(event.ToolInput),
			nullableString(event.ToolOutput),
			event.TokensInput,
			event.TokensOutput,
			event.CostUSD,
			event.LatencyMS,
			nullableString(event.ModelName),
			boolToUInt8(event.IsError),
			nullableString(event.ErrorMessage),
			event.Timestamp,
			event.VisitorClass,
			nullableString(event.BotName),
			nullableString(event.ReferrerHost),
			event.ReferrerChannel,
			nullableString(event.UserAgent),
			nullableString(event.InsertID),
			boolToUInt8(event.IsUnplanned),
		); err != nil {
			return err
		}
	}
	return batch.Send()
}

// SinkEvents is the ingest worker's write path: it durably stores the batch (its
// error drives the JetStream ack/nak/dead-letter decision) and then, only on
// success, hands the batch to the background person applier. Profile maintenance
// runs off this ack path — a failure or dropped hand-off is best-effort and never
// fails the batch, since profiles self-heal on the next event for that person and
// blocking the ack on a derived table would risk redelivering already-stored
// events.
func (s *Store) SinkEvents(ctx context.Context, events []Event) error {
	if err := s.InsertEvents(ctx, events); err != nil {
		return err
	}
	// Hand profile maintenance to the background applier so it never adds latency to
	// this batch's ack. Events are already durable; the profile is a derived,
	// self-healing view, so a dropped hand-off (saturated applier) or a later apply
	// failure never costs an event.
	s.enqueuePersonUpdates(events)
	return nil
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
	// Postgres is the source of truth; the cached resolver and the ClickHouse
	// dictionary are derived. Invalidate the cache so a person-scoped filter sees
	// the new alias immediately, and dual-write into the dictionary's source
	// table (best effort — the dictionary's LIFETIME refresh and the boot
	// backfill both re-read Postgres, so a dropped write self-heals).
	s.resolvers.invalidate(projectID)
	if err := s.insertAliasRows(ctx, [][3]string{{projectID, anonymousID, canonicalID}}); err != nil {
		log.Printf("storage: dual-write alias to ClickHouse: %v", err)
	}
	return nil
}

// insertAliasRows writes (project_id, anonymous_id, canonical_id) triples into
// the ClickHouse aliases table that backs aliases_dict. ReplacingMergeTree makes
// re-inserts idempotent, so this is safe to call for both dual-writes and the
// boot backfill.
func (s *Store) insertAliasRows(ctx context.Context, rows [][3]string) error {
	if s.ch == nil || len(rows) == 0 {
		return nil
	}
	batch, err := s.ch.PrepareBatch(ctx, "INSERT INTO aliases (project_id, anonymous_id, canonical_id)")
	if err != nil {
		return err
	}
	for _, r := range rows {
		if err := batch.Append(r[0], r[1], r[2]); err != nil {
			return err
		}
	}
	return batch.Send()
}

// backfillAliasDictionary seeds the ClickHouse aliases table from every existing
// Postgres alias and reloads the dictionary. Done app-side (not via a ClickHouse
// postgresql() source) so it never assumes ClickHouse can reach Postgres on the
// same host the app uses — the two differ between local dev and the container
// deploy.
func (s *Store) backfillAliasDictionary(ctx context.Context) error {
	if s.ch == nil {
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
	if err := s.insertAliasRows(ctx, triples); err != nil {
		return err
	}
	return s.ch.Exec(ctx, "SYSTEM RELOAD DICTIONARY "+s.chDatabase+".aliases_dict")
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

	resolver := identityResolver{database: s.chDatabase}
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

// canonicalExpr returns a ClickHouse scalar that maps a distinct-id column to its
// stitched canonical id. It resolves through the aliases_dict dictionary —
// dictGet by (project_id, column) — so the alias map lives in ClickHouse memory
// instead of being shipped as per-query transform() arrays. Unknown ids fall back
// to themselves. project_id must be in scope (it always is: every events /
// sessions_mv / scoped_events query is project-keyed). Returns no bind args.
func (r identityResolver) canonicalExpr(column string) (string, []any) {
	dict := "aliases_dict"
	if r.database != "" {
		dict = r.database + ".aliases_dict"
	}
	return "dictGetOrDefault('" + dict + "', 'canonical_id', (project_id, " + column + "), " + column + ")", nil
}

// canonicalID resolves a raw distinct id to its stitched canonical id, mirroring
// the read-path dictGetOrDefault('aliases_dict','canonical_id',(project,id), id):
// an id that appears as an anonymous alias maps to its canonical, everything else
// (including canonical ids themselves) maps to itself. Single-hop, matching the
// dictionary; used by the person-profile write path to key by canonical id.
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
	rows, err := s.ch.Query(ctx, `
SELECT event_name, any(event_type) AS event_type, count() AS cnt, max(timestamp) AS last_seen
FROM events
WHERE project_id = ? AND event_name != ''
GROUP BY event_name
ORDER BY cnt DESC
LIMIT ?`, projectID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	entries := []EventCatalogEntry{}
	for rows.Next() {
		var e EventCatalogEntry
		if err := rows.Scan(&e.EventName, &e.EventType, &e.Count, &e.LastSeen); err != nil {
			return nil, err
		}
		entries = append(entries, e)
	}
	return entries, rows.Err()
}

// EventNameSet returns every distinct event name for a project with no volume cap,
// for callers that need the *complete* catalog rather than a ranked top-N. The
// tracking-plan guard uses it: a top-N (EventNames) would misflag a genuinely
// established but low-volume name that fell outside the slice as "unplanned"
// forever. Event names are low-cardinality, so the full set stays small.
func (s *Store) EventNameSet(ctx context.Context, projectID string) (map[string]struct{}, error) {
	rows, err := s.ch.Query(ctx, `
SELECT DISTINCT event_name
FROM events
WHERE project_id = ? AND event_name != ''`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]struct{}{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out[name] = struct{}{}
	}
	return out, rows.Err()
}

func (s *Store) RecentEvents(ctx context.Context, projectID string, limit int) ([]Event, error) {
	rows, err := s.ch.Query(ctx, `
SELECT
	project_id::String, event_id::String, distinct_id, session_id, event_name,
	event_type, properties, is_error, timestamp, inserted_at
FROM events
WHERE project_id = ?
ORDER BY inserted_at DESC
LIMIT ?`, projectID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	events := []Event{}
	for rows.Next() {
		var event Event
		var isError uint8
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
		); err != nil {
			return nil, err
		}
		event.IsError = isError == 1
		event.InsertedAt = &inserted
		events = append(events, event)
	}
	return events, rows.Err()
}

func (s *Store) RecentSessions(ctx context.Context, projectID string, limit int) ([]Session, error) {
	resolver, err := s.identityResolver(ctx, projectID)
	if err != nil {
		return nil, err
	}
	canonicalID, canonicalArgs := resolver.canonicalExpr("distinct_id")
	args := append(append([]any{}, canonicalArgs...), projectID, limit)
	rows, err := s.ch.Query(ctx, `
SELECT
	project_id::String,
	session_id,
	`+canonicalID+` AS canonical_distinct_id,
	minMerge(session_start) AS session_start,
	maxMerge(session_end) AS session_end,
	countMerge(event_count) AS event_count,
	sumMerge(total_tokens_in) AS total_tokens_in,
	sumMerge(total_tokens_out) AS total_tokens_out,
	sumMerge(total_cost_usd) AS total_cost_usd,
	maxMerge(last_event_at) AS last_event_at
FROM sessions_mv
WHERE project_id = ?
GROUP BY project_id, session_id, canonical_distinct_id
ORDER BY last_event_at DESC
LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	sessions := []Session{}
	for rows.Next() {
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
			return nil, err
		}
		session.LastEventAt = &lastEventAt
		sessions = append(sessions, session)
	}
	return sessions, rows.Err()
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
	canonicalID, canonicalArgs := resolver.canonicalExpr("distinct_id")
	summaryArgs := append(append([]any{}, canonicalArgs...), args...)

	err = s.ch.QueryRow(ctx, `
	SELECT
		count(),
		countIf(event_type = 'user'),
	countIf(event_type = 'agent'),
	countIf(event_type = 'system'),
	uniqExactIf(session_id, session_id != ''),
	uniqExact(`+canonicalID+`),
	sum(toUInt64(ifNull(tokens_input, toUInt32(0)))),
	sum(toUInt64(ifNull(tokens_output, toUInt32(0)))),
		sum(toFloat64(ifNull(cost_usd, toFloat32(0))))
	FROM events
	WHERE `+where, summaryArgs...).
		Scan(
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
	rows, err := s.ch.Query(ctx, `
	SELECT event_name, count() AS count
	FROM events
	WHERE `+where+`
	GROUP BY event_name
	ORDER BY count DESC
	LIMIT 20`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	counts := []EventCount{}
	for rows.Next() {
		var item EventCount
		if err := rows.Scan(&item.EventName, &item.Count); err != nil {
			return nil, err
		}
		counts = append(counts, item)
	}
	return counts, rows.Err()
}

func (s *Store) timeline(ctx context.Context, where string, args []any) ([]TimelinePoint, error) {
	rows, err := s.ch.Query(ctx, `
	SELECT toStartOfHour(timestamp) AS hour, count() AS count
	FROM events
	WHERE `+where+`
	GROUP BY hour
	ORDER BY hour ASC`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	points := []TimelinePoint{}
	for rows.Next() {
		var point TimelinePoint
		if err := rows.Scan(&point.Hour, &point.Count); err != nil {
			return nil, err
		}
		points = append(points, point)
	}
	return points, rows.Err()
}

func (s *Store) topAgents(ctx context.Context, where string, args []any) ([]AgentMetric, error) {
	rows, err := s.ch.Query(ctx, `
	SELECT
		ifNull(agent_id, 'unknown') AS agent_id,
	count() AS event_count,
	sum(toFloat64(ifNull(cost_usd, toFloat32(0)))) AS total_cost_usd,
		avg(toFloat64(ifNull(latency_ms, toUInt32(0)))) AS avg_latency_ms
	FROM events
	WHERE `+where+` AND event_type = 'agent'
	GROUP BY agent_id
	ORDER BY total_cost_usd DESC, event_count DESC
	LIMIT 10`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	agents := []AgentMetric{}
	for rows.Next() {
		var agent AgentMetric
		if err := rows.Scan(&agent.AgentID, &agent.EventCount, &agent.TotalCostUSD, &agent.AvgLatencyMS); err != nil {
			return nil, err
		}
		agents = append(agents, agent)
	}
	return agents, rows.Err()
}

func (s *Store) recentSessions(ctx context.Context, resolver identityResolver, where string, args []any, limit int) ([]Session, error) {
	queryArgs := append([]any{}, args...)
	queryArgs = append(queryArgs, limit)
	canonicalID, canonicalArgs := resolver.canonicalExpr("distinct_id")
	queryArgs = append(append([]any{}, canonicalArgs...), queryArgs...)
	rows, err := s.ch.Query(ctx, `
	SELECT
		project_id::String,
		session_id,
		any(`+canonicalID+`) AS session_distinct_id,
		min(timestamp) AS session_start,
		max(timestamp) AS session_end,
		count() AS event_count,
		sum(toUInt64(ifNull(tokens_input, toUInt32(0)))) AS total_tokens_in,
		sum(toUInt64(ifNull(tokens_output, toUInt32(0)))) AS total_tokens_out,
		sum(toFloat64(ifNull(cost_usd, toFloat32(0)))) AS total_cost_usd,
		max(timestamp) AS last_event_at
	FROM events
	WHERE `+where+` AND session_id != ''
	GROUP BY project_id, session_id
	ORDER BY last_event_at DESC
	LIMIT ?`, queryArgs...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	sessions := []Session{}
	for rows.Next() {
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
			return nil, err
		}
		session.LastEventAt = &lastEventAt
		sessions = append(sessions, session)
	}
	return sessions, rows.Err()
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
		retention, err := s.retention(ctx, projectID, firstNonEmpty(filter.EventName, metric, "user.pageview"), filter)
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
	canonicalID, canonicalArgs := resolver.canonicalExpr("distinct_id")
	webArgs := append(append([]any{}, canonicalArgs...), args...)
	err = s.ch.QueryRow(ctx, `
SELECT
	uniqExact(`+canonicalID+`),
	countIf(event_name = 'user.pageview'),
	uniqExactIf(session_id, session_id != ''),
	countIf(event_name IN ('user.conversion', 'user.signup'))
FROM events
WHERE `+where, webArgs...).Scan(&web.Visitors, &web.Pageviews, &web.Sessions, &web.Conversions)
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
	rows, err := s.ch.Query(ctx, `
SELECT ifNull(visitor_class, 'human') AS class, count() AS count
FROM events
WHERE `+where+` AND event_name = 'user.pageview'
GROUP BY class
ORDER BY count DESC`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []TrafficClass{}
	for rows.Next() {
		var item TrafficClass
		if err := rows.Scan(&item.Class, &item.Count); err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func (s *Store) trafficByProvider(ctx context.Context, resolver identityResolver, where string, args []any) ([]TrafficProvider, error) {
	canonicalID, canonicalArgs := resolver.canonicalExpr("distinct_id")
	queryArgs := append(append([]any{}, canonicalArgs...), args...)
	rows, err := s.ch.Query(ctx, `
SELECT
	class,
	provider,
	uniqExact(`+canonicalID+`) AS visitors,
	count() AS pageviews
FROM (
	SELECT
		project_id,
		distinct_id,
		ifNull(visitor_class, 'human') AS class,
		multiIf(
			ifNull(bot_name, '') != '', ifNull(bot_name, ''),
			referrer_channel = 'ai-referral' AND ifNull(referrer_host, '') != '', ifNull(referrer_host, ''),
			ifNull(visitor_class, 'human')
		) AS provider
	FROM events
	WHERE `+where+` AND event_name = 'user.pageview'
)
GROUP BY class, provider
ORDER BY pageviews DESC, visitors DESC
LIMIT 20`, queryArgs...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []TrafficProvider{}
	for rows.Next() {
		var item TrafficProvider
		if err := rows.Scan(&item.Class, &item.Provider, &item.Visitors, &item.Pageviews); err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func (s *Store) aiTopPaths(ctx context.Context, where string, args []any) ([]PathCount, error) {
	rows, err := s.ch.Query(ctx, `
SELECT value, count() AS count
FROM (
	SELECT ifNull(JSONExtractString(properties, 'path'), '') AS value
	FROM events
	WHERE `+where+` AND event_name = 'user.pageview'
		AND (visitor_class = 'ai-platform' OR referrer_channel = 'ai-referral')
)
WHERE value != ''
GROUP BY value
ORDER BY count DESC
LIMIT 10`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []PathCount{}
	for rows.Next() {
		var item PathCount
		if err := rows.Scan(&item.Value, &item.Count); err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func (s *Store) externalReferrers(ctx context.Context, where string, args []any) ([]PathCount, error) {
	rows, err := s.ch.Query(ctx, `
SELECT ifNull(referrer_host, '') AS value, count() AS count
FROM events
WHERE `+where+` AND event_name = 'user.pageview'
	AND referrer_channel NOT IN ('', 'direct', 'internal')
	AND referrer_host != '' AND isNotNull(referrer_host)
GROUP BY value
ORDER BY count DESC
LIMIT 20`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []PathCount{}
	for rows.Next() {
		var item PathCount
		if err := rows.Scan(&item.Value, &item.Count); err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func (s *Store) referrersByChannel(ctx context.Context, where string, args []any) ([]PathCount, error) {
	rows, err := s.ch.Query(ctx, `
SELECT ifNull(referrer_channel, '') AS channel, count() AS count
FROM events
WHERE `+where+` AND event_name = 'user.pageview'
	AND referrer_channel != '' AND referrer_channel != 'direct' AND referrer_channel != 'internal'
GROUP BY channel
ORDER BY count DESC`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []PathCount{}
	for rows.Next() {
		var item PathCount
		if err := rows.Scan(&item.Value, &item.Count); err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func (s *Store) guestVsUser(ctx context.Context, resolver identityResolver, where string, args []any) (GuestUser, error) {
	var gv GuestUser
	canonicalID, canonicalArgs := resolver.canonicalExpr("distinct_id")
	queryArgs := append(append([]any{}, canonicalArgs...), args...)
	err := s.ch.QueryRow(ctx, `
SELECT
	countIf(has_traits) AS users,
	countIf(NOT has_traits) AS guests
FROM (
	SELECT
		canonical_distinct_id,
		max(email_trait != '' OR name_trait != '') AS has_traits
	FROM (
		SELECT
			`+canonicalID+` AS canonical_distinct_id,
			if(JSONExtractString(properties, 'email') != '', JSONExtractString(properties, 'email'), JSONExtractString(properties, '$set', 'email')) AS email_trait,
			if(JSONExtractString(properties, 'name') != '', JSONExtractString(properties, 'name'), JSONExtractString(properties, '$set', 'name')) AS name_trait
		FROM events
		WHERE `+where+`
	)
	GROUP BY canonical_distinct_id
)`, queryArgs...).Scan(&gv.Users, &gv.Guests)
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
	canonicalID, canonicalArgs := resolver.canonicalExpr("distinct_id")

	traitSource := `
SELECT
	` + canonicalID + ` AS canonical_distinct_id, session_id, event_name, timestamp,
	if(JSONExtractString(properties, 'email') != '', JSONExtractString(properties, 'email'), JSONExtractString(properties, '$set', 'email')) AS email_trait,
	if(JSONExtractString(properties, 'name') != '', JSONExtractString(properties, 'name'), JSONExtractString(properties, '$set', 'name')) AS name_trait
FROM events
WHERE ` + where
	traitArgs := append(append([]any{}, canonicalArgs...), args...)

	if err := s.ch.QueryRow(ctx, `
SELECT
	count(),
	countIf(has_traits)
FROM (
	SELECT
		canonical_distinct_id,
		max(email_trait != '' OR name_trait != '') AS has_traits
	FROM (`+traitSource+`)
	GROUP BY canonical_distinct_id
)`, traitArgs...).Scan(&summary.Total, &summary.Identified); err != nil {
		return summary, err
	}
	summary.Anonymous = summary.Total - summary.Identified

	activeArgs := append(append([]any{}, canonicalArgs...), args...)
	activeRows, err := s.ch.Query(ctx, `
SELECT toStartOfHour(timestamp) AS hour, uniqExact(`+canonicalID+`) AS users
FROM events
WHERE `+where+`
GROUP BY hour
ORDER BY hour ASC`, activeArgs...)
	if err != nil {
		return summary, err
	}
	defer activeRows.Close()
	for activeRows.Next() {
		var point TimelinePoint
		if err := activeRows.Scan(&point.Hour, &point.Count); err != nil {
			return summary, err
		}
		summary.ActiveTimeline = append(summary.ActiveTimeline, point)
	}
	if err := activeRows.Err(); err != nil {
		return summary, err
	}

	personArgs := append(append([]any{}, traitArgs...), filter.Limit)
	personRows, err := s.ch.Query(ctx, `
SELECT
	canonical_distinct_id,
	argMaxIf(email_trait, timestamp, email_trait != '') AS email,
	argMaxIf(name_trait, timestamp, name_trait != '') AS name,
	min(timestamp) AS first_seen,
	max(timestamp) AS last_seen,
	count() AS event_count,
	uniqExactIf(session_id, session_id != '') AS sessions,
	argMax(event_name, timestamp) AS last_event_name
FROM (`+traitSource+`)
GROUP BY canonical_distinct_id
ORDER BY (email != '' OR name != '') DESC, last_seen DESC
LIMIT ?`, personArgs...)
	if err != nil {
		return summary, err
	}
	defer personRows.Close()
	for personRows.Next() {
		var person Person
		if err := personRows.Scan(
			&person.DistinctID, &person.Email, &person.Name,
			&person.FirstSeen, &person.LastSeen,
			&person.EventCount, &person.Sessions, &person.LastEventName,
		); err != nil {
			return summary, err
		}
		summary.Persons = append(summary.Persons, person)
	}
	if err := personRows.Err(); err != nil {
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
		if profiles, err := s.personProfilesByKeys(ctx, projectID, ids); err == nil {
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
	where, args := filteredWhereWithDistinctIDs(projectID, filter, defaultTimeWindow, resolver.relatedDistinctIDs(filter.DistinctID))
	args = append(args, filter.Limit)
	rows, err := s.ch.Query(ctx, `
SELECT
	project_id::String, event_id::String, distinct_id, session_id, event_name,
	event_type, properties, ifNull(agent_id, ''), ifNull(tool_name, ''),
	ifNull(tool_input, ''), ifNull(tool_output, ''),
	ifNull(tokens_input, toUInt32(0)), ifNull(tokens_output, toUInt32(0)),
	toFloat64(ifNull(cost_usd, toFloat32(0))), ifNull(latency_ms, toUInt32(0)),
	ifNull(model_name, ''), is_error, ifNull(error_message, ''), timestamp, inserted_at, is_unplanned
FROM events
WHERE `+where+`
ORDER BY timestamp DESC
LIMIT ?`, args...)
	if err != nil {
		return EventExplorer{}, err
	}
	defer rows.Close()

	explorer := EventExplorer{Events: []Event{}, Timeline: []Event{}, Generated: time.Now().UTC()}
	for rows.Next() {
		event, err := scanEvent(rows)
		if err != nil {
			return explorer, err
		}
		explorer.Events = append(explorer.Events, event)
	}
	if err := rows.Err(); err != nil {
		return explorer, err
	}

	if filter.SessionID != "" || filter.DistinctID != "" {
		timelineFilter := filter
		timelineFilter.Limit = 200
		timelineFilter.Search = ""
		timelineWhere, timelineArgs := filteredWhereWithDistinctIDs(projectID, timelineFilter, defaultTimeWindow, resolver.relatedDistinctIDs(timelineFilter.DistinctID))
		timelineArgs = append(timelineArgs, timelineFilter.Limit)
		timelineRows, err := s.ch.Query(ctx, `
SELECT
	project_id::String, event_id::String, distinct_id, session_id, event_name,
	event_type, properties, ifNull(agent_id, ''), ifNull(tool_name, ''),
	ifNull(tool_input, ''), ifNull(tool_output, ''),
	ifNull(tokens_input, toUInt32(0)), ifNull(tokens_output, toUInt32(0)),
	toFloat64(ifNull(cost_usd, toFloat32(0))), ifNull(latency_ms, toUInt32(0)),
	ifNull(model_name, ''), is_error, ifNull(error_message, ''), timestamp, inserted_at, is_unplanned
FROM events
WHERE `+timelineWhere+`
ORDER BY timestamp ASC
LIMIT ?`, timelineArgs...)
		if err != nil {
			return explorer, err
		}
		defer timelineRows.Close()
		for timelineRows.Next() {
			event, err := scanEvent(timelineRows)
			if err != nil {
				return explorer, err
			}
			explorer.Timeline = append(explorer.Timeline, event)
		}
		if err := timelineRows.Err(); err != nil {
			return explorer, err
		}
	}
	return explorer, nil
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

func (s *Store) RunSavedQuery(ctx context.Context, projectID string, queryID string) (SavedQueryResult, error) {
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
	cache, _ := json.Marshal(rows)
	_, _ = s.pg.Exec(ctx, `UPDATE saved_queries SET result_cache = $3 WHERE project_id = $1 AND id = $2`, projectID, queryID, cache)
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
	resolver, err := s.identityResolver(ctx, projectID)
	if err != nil {
		return nil, err
	}
	query, args, err := scopedReadonlySQL(sqlText, projectID, resolver)
	if err != nil {
		return nil, err
	}
	if !strings.Contains(strings.ToLower(query), "limit") {
		query += " LIMIT 100"
	}
	// Untrusted SQL runs on the least-privilege connection so a table-function
	// bypass fails with a grant error rather than the denylist being the only line.
	rows, err := s.readConn().Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	columns := rows.Columns()
	columnTypes := rows.ColumnTypes()
	results := []map[string]any{}
	for rows.Next() {
		valuePtrs := make([]any, len(columns))
		for i := range columns {
			scanType := columnTypes[i].ScanType()
			if scanType == nil {
				scanType = reflect.TypeOf("")
			}
			valuePtrs[i] = reflect.New(scanType).Interface()
		}
		if err := rows.Scan(valuePtrs...); err != nil {
			return nil, err
		}
		item := map[string]any{}
		for i, column := range columns {
			item[column] = normalizeSQLValue(valuePtrs[i])
		}
		results = append(results, item)
	}
	return results, rows.Err()
}

func (s *Store) filteredTimeline(ctx context.Context, projectID string, filter EventFilter) ([]TimelinePoint, error) {
	resolver, err := s.identityResolver(ctx, projectID)
	if err != nil {
		return nil, err
	}
	where, args := filteredWhereWithDistinctIDs(projectID, filter, true, resolver.relatedDistinctIDs(filter.DistinctID))
	rows, err := s.ch.Query(ctx, `
SELECT toStartOfHour(timestamp) AS hour, count() AS count
FROM events
WHERE `+where+`
GROUP BY hour
ORDER BY hour ASC`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	points := []TimelinePoint{}
	for rows.Next() {
		var point TimelinePoint
		if err := rows.Scan(&point.Hour, &point.Count); err != nil {
			return nil, err
		}
		points = append(points, point)
	}
	return points, rows.Err()
}

func (s *Store) funnel(ctx context.Context, projectID string, steps []string, filter EventFilter) ([]FunnelStep, error) {
	resolver, err := s.identityResolver(ctx, projectID)
	if err != nil {
		return nil, err
	}
	// A funnel measures people converting; crawler hits are not conversions.
	filter.HumansOnly = true
	canonicalID, canonicalArgs := resolver.canonicalExpr("distinct_id")
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
	out := []FunnelStep{}
	var firstCount uint64
	for i, eventName := range cleanSteps {
		stepFilter := filter
		stepFilter.EventName = eventName
		where, args := filteredWhereWithDistinctIDs(projectID, stepFilter, true, resolver.relatedDistinctIDs(stepFilter.DistinctID))
		queryArgs := append(append([]any{}, canonicalArgs...), args...)
		var count uint64
		if err := s.ch.QueryRow(ctx, `SELECT uniqExact(`+canonicalID+`) FROM events WHERE `+where, queryArgs...).Scan(&count); err != nil {
			return nil, err
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
	return out, nil
}

func (s *Store) retention(ctx context.Context, projectID string, firstEvent string, filter EventFilter) ([]RetentionPoint, error) {
	resolver, err := s.identityResolver(ctx, projectID)
	if err != nil {
		return nil, err
	}
	// Retention is a people metric: a returning crawler is not a retained user.
	filter.HumansOnly = true
	where, args := filteredWhereWithDistinctIDs(projectID, filter, true, resolver.relatedDistinctIDs(filter.DistinctID))
	canonicalID, canonicalArgs := resolver.canonicalExpr("distinct_id")
	canonicalEventID := strings.ReplaceAll(canonicalID, "distinct_id", "e.distinct_id")
	firstWhere := where + " AND event_name = ?"
	firstArgs := append(append([]any{}, args...), firstEvent)
	var base uint64
	baseArgs := append(append([]any{}, canonicalArgs...), firstArgs...)
	if err := s.ch.QueryRow(ctx, `SELECT uniqExact(`+canonicalID+`) FROM events WHERE `+firstWhere, baseArgs...).Scan(&base); err != nil {
		return nil, err
	}
	// Bracketed weekly cohort retention: week W counts cohort users active during
	// the half-open window [first_ts + W*7d, first_ts + (W+1)*7d). Week 0 is the
	// acquisition week (the base, 100%). A *periodic* curve — not the old rolling
	// "returned ever after X" — is what lets the PMF verdict read a plateau: a
	// curve that flattens to a stable floor is fit; one decaying toward zero is not.
	points := []RetentionPoint{{Period: "Week 0", Users: base, Rate: 1}}
	for week := 1; week <= retentionWeeks; week++ {
		lowerHours := week * retentionPeriodHours
		upperHours := (week + 1) * retentionPeriodHours
		query := `
WITH firsts AS (
	SELECT ` + canonicalID + ` AS canonical_distinct_id, min(timestamp) AS first_ts
	FROM events
	WHERE ` + firstWhere + `
	GROUP BY canonical_distinct_id
)
SELECT uniqExact(` + canonicalEventID + `)
FROM events e
INNER JOIN firsts f ON ` + canonicalEventID + ` = f.canonical_distinct_id
WHERE e.project_id = ?
	AND e.timestamp >= f.first_ts + INTERVAL ` + fmt.Sprint(lowerHours) + ` HOUR
	AND e.timestamp <  f.first_ts + INTERVAL ` + fmt.Sprint(upperHours) + ` HOUR`
		queryArgs := append([]any{}, canonicalArgs...)
		queryArgs = append(queryArgs, firstArgs...)
		queryArgs = append(queryArgs, canonicalArgs...)
		queryArgs = append(queryArgs, canonicalArgs...)
		queryArgs = append(queryArgs, projectID)
		var retained uint64
		if err := s.ch.QueryRow(ctx, query, queryArgs...).Scan(&retained); err != nil {
			return nil, err
		}
		rate := 0.0
		if base > 0 {
			rate = float64(retained) / float64(base)
		}
		points = append(points, RetentionPoint{Period: fmt.Sprintf("Week %d", week), Users: retained, Rate: rate})
	}
	return points, nil
}

// retentionWeeks / retentionPeriodHours define the cohort retention curve. Eight
// weekly periods is enough to see whether the curve plateaus (the PMF tell)
// rather than decaying to zero.
const (
	retentionWeeks       = 8
	retentionPeriodHours = 24 * 7
)

// cohortWindowWeeks is how far back Cohorts reaches to assemble cohort rows. It
// spans the full retention curve (retentionWeeks) plus a few extra weeks so the
// most recent cohorts are visible alongside older, fully-matured ones.
const cohortWindowWeeks = retentionWeeks + 4

// maxCohortRows caps the triangle height so a wide custom range can't return an
// unbounded number of weekly rows; rows are emitted newest-first, so the cap
// keeps the most recent cohorts.
const maxCohortRows = 16

// audienceSegment is one named slice of the audience. Predicate is a ClickHouse
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

// chStringLit renders a Go string as a ClickHouse single-quoted literal with
// backslash escaping. Mapping tokens are validated by validSubscriptionToken
// before they get here; this is defense in depth so interpolated config can
// never break out of its literal.
func chStringLit(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `'`, `\'`)
	return "'" + s + "'"
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

// compilePredicate turns a structured audience rule into a ClickHouse boolean
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
	canonicalID, _ := resolver.canonicalExpr("distinct_id")

	segmentClause := ""
	if predicate != "" {
		segmentClause = " AND " + predicate
	}

	// base is scanned twice (firsts derives the cohort anchor + per-person
	// attributes from it, the outer query measures activity against it); ClickHouse
	// inlines the CTE so this is two event scans, the same trade-off Persons
	// accepts. period is cast to Int32 so the driver scans it cleanly. The paid /
	// plan attributes come off the `revenue` event (amount/plan in properties) —
	// the documented revenue convention — and aggregate to one flag per person.
	periodExpr := "toInt32(intDiv(dateDiff('hour', f.first_ts, b.timestamp), " + fmt.Sprint(retentionPeriodHours) + "))"

	// Subscription projection literals, compiled from the per-project mapping. The
	// tokens are validated (validSubscriptionToken) and escaped (chStringLit) so
	// none of this is a raw-SQL surface. An event a product never emits simply
	// matches nothing, leaving sub_status = 'none'. See DESIGN-SUBSCRIPTION-AUDIENCES.md.
	startLit := chStringLit(mapping.StartEvent)
	renewLit := chStringLit(mapping.RenewEvent)
	cancelLit := chStringLit(mapping.CancelEvent)
	planLit := chStringLit(mapping.PlanProp)
	periodLit := chStringLit(mapping.PeriodEndProp)
	openExpr := "(event_name = " + startLit + " OR event_name = " + renewLit + ")"
	trialExpr := "toUInt8(0)"
	if strings.TrimSpace(mapping.TrialProp) != "" {
		trialExpr = "toUInt8(JSONExtractBool(properties, " + chStringLit(mapping.TrialProp) + "))"
	}
	grace := fmt.Sprint(mapping.GraceDays)

	query := `
WITH base AS (
	SELECT
		` + canonicalID + ` AS cid,
		timestamp,
		(if(JSONExtractString(properties, 'email') != '', JSONExtractString(properties, 'email'), JSONExtractString(properties, '$set', 'email')) != ''
		 OR if(JSONExtractString(properties, 'name') != '', JSONExtractString(properties, 'name'), JSONExtractString(properties, '$set', 'name')) != '') AS identified,
		(event_name = 'revenue' AND JSONExtractFloat(properties, 'amount') > 0) AS paid_event,
		if(event_name = 'revenue', lowerUTF8(JSONExtractString(properties, 'plan')), '') AS plan_value,
		toUInt8(` + openExpr + `) AS is_sub_open,
		toUInt8(event_name = ` + cancelLit + `) AS is_sub_cancel,
		if(` + openExpr + `, lowerUTF8(JSONExtractString(properties, ` + planLit + `)), '') AS sub_plan_value,
		if(` + openExpr + `, parseDateTimeBestEffortOrNull(JSONExtractString(properties, ` + periodLit + `)), NULL) AS sub_period_end,
		if(` + openExpr + `, ` + trialExpr + `, toUInt8(0)) AS sub_is_trial
	FROM events
	WHERE ` + where + `
),
firsts AS (
	SELECT
		cid,
		min(timestamp) AS first_ts,
		max(identified) AS has_traits,
		max(paid_event) AS is_paid,
		argMaxIf(plan_value, timestamp, plan_value != '') AS plan,
		countIf(is_sub_open = 1) AS sub_open_count,
		maxIf(timestamp, is_sub_open = 1) AS last_open_ts,
		maxIf(timestamp, is_sub_cancel = 1) AS last_cancel_ts,
		argMaxIf(sub_period_end, timestamp, is_sub_open = 1) AS sub_period_end,
		argMaxIf(sub_plan_value, timestamp, is_sub_open = 1 AND sub_plan_value != '') AS sub_plan,
		argMaxIf(sub_is_trial, timestamp, is_sub_open = 1) AS sub_is_trial
	FROM base
	GROUP BY cid
),
people AS (
	SELECT
		*,
		multiIf(
			sub_open_count = 0, 'none',
			last_cancel_ts > last_open_ts, 'churned',
			sub_period_end IS NULL, 'none',
			sub_period_end + INTERVAL ` + grace + ` DAY < now(), 'churned',
			sub_is_trial = 1, 'trialing',
			'active') AS sub_status
	FROM firsts
)
SELECT
	toStartOfWeek(f.first_ts, 1) AS cohort_week,
	` + periodExpr + ` AS period,
	uniqExact(b.cid) AS users
FROM base b
INNER JOIN people f ON b.cid = f.cid
WHERE b.timestamp >= f.first_ts` + segmentClause + `
	AND ` + periodExpr + ` <= ?
GROUP BY cohort_week, period
ORDER BY cohort_week DESC, period ASC`

	queryArgs := append(append([]any{}, args...), retentionWeeks)
	rows, err := s.ch.Query(ctx, query, queryArgs...)
	if err != nil {
		return result, err
	}
	defer rows.Close()

	// Bucket cells by cohort week, preserving the newest-first scan order so the
	// row cap keeps the most recent cohorts.
	byCohort := map[string]*CohortRow{}
	order := []string{}
	for rows.Next() {
		var cohortWeek time.Time
		var period int32
		var users uint64
		if err := rows.Scan(&cohortWeek, &period, &users); err != nil {
			return result, err
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
	}
	if err := rows.Err(); err != nil {
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

// agentRollupWindow reports whether filter is a plain per-(agent,model) aggregate
// over a day-aligned window with no per-row predicates, and if so returns the
// half-open [from, to) day bounds the pre-aggregated agent_usage_daily can serve
// exactly. Any row-level filter (distinct id, session, search, error-only, a
// specific event/agent/model) or a non-midnight boundary falls back to raw events,
// because the rollup only stores day-grained (agent, model) totals.
func agentRollupWindow(filter EventFilter) (from, to time.Time, ok bool) {
	if filter.DistinctID != "" || filter.SessionID != "" || filter.Search != "" ||
		filter.EventName != "" || filter.AgentID != "" || filter.ModelName != "" || filter.ErrorOnly {
		return time.Time{}, time.Time{}, false
	}
	from, to = filter.From.UTC(), filter.To.UTC()
	if from.IsZero() || to.IsZero() || !to.After(from) {
		return time.Time{}, time.Time{}, false
	}
	if !from.Equal(from.Truncate(24*time.Hour)) || !to.Equal(to.Truncate(24*time.Hour)) {
		return time.Time{}, time.Time{}, false
	}
	return from, to, true
}

func (s *Store) agentInsightRows(ctx context.Context, projectID string, filter EventFilter) ([]map[string]any, error) {
	filter.EventType = "agent"
	if from, to, ok := agentRollupWindow(filter); ok {
		if out, err := s.agentInsightRowsRollup(ctx, projectID, from, to); err == nil {
			return out, nil
		}
		// Rollup read failed (e.g. table not yet migrated on an old deploy) — fall
		// through to the always-correct raw path rather than surface an error.
	}
	resolver, err := s.identityResolver(ctx, projectID)
	if err != nil {
		return nil, err
	}
	where, args := filteredWhereWithDistinctIDs(projectID, filter, true, resolver.relatedDistinctIDs(filter.DistinctID))
	rows, err := s.ch.Query(ctx, `
SELECT
	ifNull(agent_id, 'unknown') AS agent_id,
	ifNull(model_name, 'unknown') AS model_name,
	count() AS events,
	sum(toUInt64(ifNull(tokens_input, toUInt32(0)))) AS tokens_in,
	sum(toUInt64(ifNull(tokens_output, toUInt32(0)))) AS tokens_out,
	sum(toFloat64(ifNull(cost_usd, toFloat32(0)))) AS cost_usd,
	avg(toFloat64(ifNull(latency_ms, toUInt32(0)))) AS avg_latency_ms,
	countIf(is_error = 1) AS errors
FROM events
WHERE `+where+`
GROUP BY agent_id, model_name
ORDER BY cost_usd DESC, events DESC
LIMIT 50`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var agentID, modelName string
		var events, tokensIn, tokensOut, errors uint64
		var cost, latency float64
		if err := rows.Scan(&agentID, &modelName, &events, &tokensIn, &tokensOut, &cost, &latency, &errors); err != nil {
			return nil, err
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
	}
	return out, rows.Err()
}

// agentInsightRowsRollup answers the same shape as agentInsightRows from the
// pre-aggregated agent_usage_daily table. avg_latency_ms = sum(latency)/count(),
// reproducing the raw avg(coalesce(latency,0)) exactly for a day-aligned window.
func (s *Store) agentInsightRowsRollup(ctx context.Context, projectID string, from, to time.Time) ([]map[string]any, error) {
	rows, err := s.ch.Query(ctx, `
SELECT
	agent_id,
	model_name,
	countMerge(events) AS events,
	sumMerge(tokens_in) AS tokens_in,
	sumMerge(tokens_out) AS tokens_out,
	sumMerge(cost_usd) AS cost_usd,
	sumMerge(latency_sum) / greatest(countMerge(events), 1) AS avg_latency_ms,
	toUInt64(sumMerge(errors)) AS errors
FROM agent_usage_daily
WHERE project_id = ? AND day >= toDate(?) AND day < toDate(?)
GROUP BY agent_id, model_name
ORDER BY cost_usd DESC, events DESC
LIMIT 50`, projectID, from.Format("2006-01-02"), to.Format("2006-01-02"))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var agentID, modelName string
		var events, tokensIn, tokensOut, errCount uint64
		var cost, latency float64
		if err := rows.Scan(&agentID, &modelName, &events, &tokensIn, &tokensOut, &cost, &latency, &errCount); err != nil {
			return nil, err
		}
		out = append(out, map[string]any{
			"agent_id":       agentID,
			"model_name":     modelName,
			"events":         events,
			"tokens_in":      tokensIn,
			"tokens_out":     tokensOut,
			"cost_usd":       cost,
			"avg_latency_ms": latency,
			"errors":         errCount,
		})
	}
	return out, rows.Err()
}

func (s *Store) sessionQuality(ctx context.Context, projectID string, filter EventFilter) (float64, float64, error) {
	resolver, err := s.identityResolver(ctx, projectID)
	if err != nil {
		return 0, 0, err
	}
	where, args := filteredWhereWithDistinctIDs(projectID, filter, true, resolver.relatedDistinctIDs(filter.DistinctID))
	var duration float64
	var bounceRate float64
	err = s.ch.QueryRow(ctx, `
WITH sessions AS (
	SELECT
		session_id,
		dateDiff('second', min(timestamp), max(timestamp)) AS duration_seconds,
		count() AS events
	FROM events
	WHERE `+where+` AND session_id != ''
	GROUP BY session_id
)
SELECT ifNull(avg(duration_seconds), 0), ifNull(avg(if(events <= 1, 1, 0)), 0)
FROM sessions`, args...).Scan(&duration, &bounceRate)
	return duration, bounceRate, err
}

func (s *Store) propertyCounts(ctx context.Context, projectID string, filter EventFilter, property string, eventName string) ([]PathCount, error) {
	filter.EventName = eventName
	resolver, err := s.identityResolver(ctx, projectID)
	if err != nil {
		return nil, err
	}
	where, args := filteredWhereWithDistinctIDs(projectID, filter, true, resolver.relatedDistinctIDs(filter.DistinctID))
	rows, err := s.ch.Query(ctx, `
SELECT ifNull(JSONExtractString(properties, ?), '') AS value, count() AS count
FROM events
WHERE `+where+` AND value != ''
GROUP BY value
ORDER BY count DESC
LIMIT 20`, append([]any{property}, args...)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []PathCount{}
	for rows.Next() {
		var item PathCount
		if err := rows.Scan(&item.Value, &item.Count); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func filteredWhere(projectID string, filter EventFilter) (string, []any) {
	return filteredWhereWithDefault(projectID, filter, true)
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
		clauses = append(clauses, "timestamp >= ?")
		args = append(args, from)
	}
	if !to.IsZero() {
		clauses = append(clauses, "timestamp <= ?")
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
			clauses = append(clauses, "distinct_id IN ("+placeholders(len(relatedDistinctIDs))+")")
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
		clauses = append(clauses, "ifNull(agent_id, '') = ?")
		args = append(args, filter.AgentID)
	}
	if filter.ModelName != "" {
		clauses = append(clauses, "ifNull(model_name, '') = ?")
		args = append(args, filter.ModelName)
	}
	if filter.ErrorOnly {
		clauses = append(clauses, "is_error = 1")
	}
	if filter.HumansOnly {
		clauses = append(clauses, "ifNull(visitor_class, 'human') = 'human'")
	}
	if filter.Search != "" {
		clauses = append(clauses, "(positionCaseInsensitive(event_name, ?) > 0 OR positionCaseInsensitive(distinct_id, ?) > 0 OR positionCaseInsensitive(session_id, ?) > 0 OR positionCaseInsensitive(properties, ?) > 0)")
		args = append(args, filter.Search, filter.Search, filter.Search, filter.Search)
	}
	return strings.Join(clauses, " AND "), args
}

func workspaceFilteredWhere(projectIDs []string, filter EventFilter, defaultTimeWindow bool) (string, []any) {
	clauses := []string{"project_id IN (" + placeholders(len(projectIDs)) + ")"}
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
		clauses = append(clauses, "timestamp >= ?")
		args = append(args, from)
	}
	if !to.IsZero() {
		clauses = append(clauses, "timestamp <= ?")
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
		clauses = append(clauses, "is_error = 1")
	}
	if filter.HumansOnly {
		clauses = append(clauses, "ifNull(visitor_class, 'human') = 'human'")
	}
	if filter.Search != "" {
		clauses = append(clauses, "(event_name ILIKE ? OR properties ILIKE ? OR distinct_id ILIKE ?)")
		search := "%" + filter.Search + "%"
		args = append(args, search, search, search)
	}
	return strings.Join(clauses, " AND "), args
}

func placeholders(count int) string {
	out := make([]string, count)
	for i := range out {
		out[i] = "?"
	}
	return strings.Join(out, ", ")
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
	var isError uint8
	var isUnplanned uint8
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
	); err != nil {
		return event, err
	}
	event.IsError = isError == 1
	event.IsUnplanned = isUnplanned == 1
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
)

func scopedReadonlySQL(sqlText string, projectID string, resolver identityResolver) (string, []any, error) {
	// Normalize: strip trailing semicolons before validation and query building.
	sqlText = strings.TrimRight(strings.TrimSpace(sqlText), ";")
	if err := validateReadonlySQL(sqlText); err != nil {
		return "", nil, err
	}
	if strings.Contains(sqlText, "?") {
		return "", nil, fmt.Errorf("SQL parameters are not supported; use {project_id}")
	}
	// JOIN events is always rejected — only the FROM position is rewritten to
	// the scoped CTE, so a joined bare `events` would read cross-tenant.
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
	// identifier, second occurrence) would hit the raw multi-tenant table.
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
		// two.
		canonicalExpr, canonicalArgs := resolver.canonicalExpr("distinct_id")
		args = append(args, canonicalArgs...)
		args = append(args, projectID)
		ctes = append(ctes, "scoped_events AS (SELECT *, "+canonicalExpr+" AS canonical_id FROM events WHERE project_id = ?)")
	}
	if hasExternal {
		// FINAL collapses the ReplacingMergeTree versions at query time, so a
		// re-synced row reads as one row even before background merges run.
		args = append(args, projectID)
		ctes = append(ctes, "scoped_external_rows AS (SELECT * FROM external_rows FINAL WHERE project_id = ?)")
	}
	for i := 0; i < projectPlaceholders; i++ {
		args = append(args, projectID)
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
	for _, keyword := range []string{"DROP", "DELETE", "INSERT", "UPDATE", "ALTER", "CREATE", "TRUNCATE", "SYSTEM", "GRANT", "REVOKE"} {
		if strings.Contains(upper, keyword) {
			return fmt.Errorf("forbidden SQL keyword: %s", keyword)
		}
	}
	// Defense-in-depth against the table-function bypass. The primary security
	// property is the least-privilege ClickHouse role (no table-function grants),
	// but the RO account is optional in dev, so also reject the known SSRF /
	// cross-tenant read table functions here regardless of where they appear
	// (including inside a subquery, which the old events-source guard did not
	// cover). Comments are stripped first, because ClickHouse treats a comment as
	// whitespace between the name and its paren (e.g. `url/**/('…')`), which would
	// otherwise slip past the `name(` match. Matched as `name(` so a column
	// literally named `url` is unaffected.
	if fn := forbiddenTableFunction(stripSQLComments(sqlText)); fn != "" {
		return fmt.Errorf("forbidden table function: %s", fn)
	}
	return nil
}

// sqlCommentPattern matches ClickHouse SQL comments: `/* … */` blocks (including
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

// tableFunctionPattern matches a ClickHouse table function call — an identifier
// immediately followed by `(`, tolerant of whitespace between name and paren.
var tableFunctionPattern = regexp.MustCompile(`(?i)\b([a-z_][a-z0-9_]*)\s*\(`)

// forbiddenTableFunctions are ClickHouse table functions that read outside the
// project database: network egress (SSRF), cross-tenant/other-server reads, and
// local filesystem access. run_sql must never reach them.
var forbiddenTableFunctions = map[string]bool{
	"url": true, "urlcluster": true, "remote": true, "remotesecure": true,
	"mysql": true, "postgresql": true, "mongodb": true, "redis": true,
	"file": true, "s3": true, "s3cluster": true, "hdfs": true, "hdfscluster": true,
	"jdbc": true, "odbc": true, "sqlite": true, "azureblobstorage": true, "deltalake": true,
	"iceberg": true, "gcs": true, "dictionary": true, "cluster": true, "clusterallreplicas": true,
}

func forbiddenTableFunction(sqlText string) string {
	for _, m := range tableFunctionPattern.FindAllStringSubmatch(sqlText, -1) {
		if forbiddenTableFunctions[strings.ToLower(m[1])] {
			return strings.ToLower(m[1])
		}
	}
	return ""
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
	default:
		return v
	}
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

func boolToUInt8(value bool) uint8 {
	if value {
		return 1
	}
	return 0
}
