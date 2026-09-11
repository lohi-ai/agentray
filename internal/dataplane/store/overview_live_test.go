package storage

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// overview_live_test.go proves the overview queries against real ClickHouse
// with a fixture that exercises every locked semantic: identity stitching,
// the verification-event exclusion, bot exclusion, per-cohort-day retention
// maturity, platform-scoped first-seen, and the honest empty states.
//
// Skipped unless AGENTRAY_LIVE_CH is set, matching alias_dict_live_test.go.
//
//	AGENTRAY_LIVE_CH=1 \
//	AGENTRAY_LIVE_PG=postgres://lohi:lohi@localhost:5434/lohi_analytics?sslmode=disable \
//	AGENTRAY_LIVE_CH_ADDR=localhost:19000 \
//	go test ./internal/dataplane/store/ -run TestOverviewLive -v

func TestOverviewLive(t *testing.T) {
	if os.Getenv("AGENTRAY_LIVE_CH") == "" {
		t.Skip("set AGENTRAY_LIVE_CH to run the live overview test")
	}
	ctx := context.Background()

	pgURL := envOr("AGENTRAY_LIVE_PG", "postgres://lohi:lohi@localhost:5434/lohi_analytics?sslmode=disable")
	chAddr := envOr("AGENTRAY_LIVE_CH_ADDR", "localhost:19000")

	pg, err := pgxpool.New(ctx, pgURL)
	if err != nil {
		t.Fatalf("pg: %v", err)
	}
	defer pg.Close()

	ch, err := clickhouse.Open(&clickhouse.Options{
		Addr: []string{chAddr},
		Auth: clickhouse.Auth{Database: "lohi_analytics", Username: "lohi", Password: "lohi"},
	})
	if err != nil {
		t.Fatalf("ch: %v", err)
	}
	defer ch.Close()

	s := &Store{pg: pg, ch: ch, chDatabase: "lohi_analytics", resolvers: newResolverCache(30 * time.Second)}

	// Minimal DDL: events + aliases + the dictionary canonicalExpr reads.
	for _, ddl := range []string{
		`CREATE TABLE IF NOT EXISTS events (
			project_id UUID, event_id UUID DEFAULT generateUUIDv4(),
			distinct_id String, session_id String,
			event_name LowCardinality(String), event_type LowCardinality(String),
			properties String, agent_id Nullable(String), tool_name Nullable(String),
			tool_input Nullable(String), tool_output Nullable(String),
			tokens_input Nullable(UInt32), tokens_output Nullable(UInt32),
			cost_usd Nullable(Float32), latency_ms Nullable(UInt32),
			model_name Nullable(String), is_error UInt8 DEFAULT 0,
			error_message Nullable(String), timestamp DateTime64(3, 'UTC'),
			inserted_at DateTime64(3, 'UTC') DEFAULT now64(),
			visitor_class LowCardinality(String) DEFAULT 'human',
			bot_name Nullable(String), referrer_host Nullable(String),
			referrer_channel LowCardinality(String) DEFAULT '',
			user_agent Nullable(String), insert_id Nullable(String),
			platform LowCardinality(String) DEFAULT '', is_unplanned UInt8 DEFAULT 0
		) ENGINE = MergeTree() PARTITION BY toYYYYMM(timestamp)
		ORDER BY (project_id, event_name, timestamp, distinct_id)`,
		`CREATE TABLE IF NOT EXISTS aliases (
			project_id UUID, anonymous_id String, canonical_id String,
			version DateTime64(3, 'UTC') DEFAULT now64()
		) ENGINE = ReplacingMergeTree(version) ORDER BY (project_id, anonymous_id)`,
		`CREATE DICTIONARY IF NOT EXISTS aliases_dict (
			project_id UUID, anonymous_id String, canonical_id String
		) PRIMARY KEY project_id, anonymous_id
		SOURCE(CLICKHOUSE(TABLE 'aliases' DB 'lohi_analytics' USER 'lohi' PASSWORD 'lohi'))
		LAYOUT(COMPLEX_KEY_HASHED()) LIFETIME(MIN 1 MAX 2)`,
	} {
		if err := ch.Exec(ctx, ddl); err != nil {
			t.Fatalf("ddl: %v", err)
		}
	}

	// Generated ids keep repeat and concurrent runs isolated; cleanup is
	// synchronous (mutations_sync) so a rerun never sees a prior fixture.
	projectID := uuid.NewString()
	emptyProjectID := uuid.NewString()
	// Pinned "now": 2026-09-12 14:00 UTC → 7d range = Sep 5..11 complete days.
	now := time.Date(2026, 9, 12, 14, 0, 0, 0, time.UTC)
	day := func(d int) time.Time { return time.Date(2026, 9, d, 12, 0, 0, 0, time.UTC) }

	type fx struct {
		distinct, session, name, typ, class, platform, channel, props string
		ts                                                            time.Time
	}
	ev := func(distinct, name string, ts time.Time) fx {
		return fx{distinct: distinct, session: "s-" + distinct + ts.Format("02"), name: name,
			typ: "user", class: "human", platform: "web", channel: "direct", props: `{"path":"/"}`, ts: ts}
	}
	fixtures := []fx{
		// alice: qualifying activity Sep 6 + Sep 7 (D1 return), pageviews.
		ev("alice", "user.pageview", day(6)), ev("alice", "user.pageview", day(7)),
		// bob: first seen Sep 8, no return.
		ev("bob", "user.pageview", day(8)),
		// carol: first seen long ago (Aug 10) — mature for D1/D7, not D30.
		ev("carol", "user.pageview", time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)),
		ev("carol", "user.pageview", time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)), // D1 return
		ev("carol", "user.pageview", time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)), // D7 return
		ev("carol", "user.pageview", day(9)),                                        // active in range
		// The verification event: receipt proof, must not count anywhere.
		ev("verify-bot", overviewVerificationEvent, day(6)),
		// A crawler: excluded from qualifying activity.
		{distinct: "crawler", session: "s-c", name: "user.pageview", typ: "user",
			class: "bot", platform: "web", channel: "organic", props: `{"path":"/"}`, ts: day(6)},
		// An agent event: excluded by event_type.
		{distinct: "agentrun", session: "", name: "agent.tool_call", typ: "agent",
			class: "human", platform: "server", channel: "", props: "{}", ts: day(6)},
		// dave: first event on ios — must count as new only under platform=ios
		// or all, never under platform=web.
		{distinct: "dave", session: "s-dave", name: "user.pageview", typ: "user",
			class: "human", platform: "ios", channel: "direct", props: `{"path":"/home"}`, ts: day(7)},
		// erin: known web person (first seen Aug 20 on web) who opens the iOS
		// app inside the range — project-wide first-seen means she is NOT a new
		// user on either platform.
		{distinct: "erin", session: "s-erin-aug", name: "user.pageview", typ: "user",
			class: "human", platform: "web", channel: "direct", props: `{"path":"/"}`, ts: time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)},
		{distinct: "erin", session: "s-erin-ios", name: "user.pageview", typ: "user",
			class: "human", platform: "ios", channel: "direct", props: `{"path":"/home"}`, ts: day(9)},
		// frank: events under BOTH the anonymous id and the canonical id the
		// alias maps it to — the dedup proof: 2 raw ids, 1 person.
		ev("frank", "user.pageview", day(6)),
		{distinct: "canonical-frank", session: "s-cf", name: "user.pageview", typ: "user",
			class: "human", platform: "web", channel: "direct", props: `{"path":"/"}`, ts: day(7)},
	}

	batch, err := ch.PrepareBatch(ctx, `INSERT INTO events (project_id, distinct_id, session_id, event_name, event_type, properties, timestamp, visitor_class, referrer_channel, platform)`)
	if err != nil {
		t.Fatalf("batch: %v", err)
	}
	for _, f := range fixtures {
		if err := batch.Append(projectID, f.distinct, f.session, f.name, f.typ, f.props, f.ts, f.class, f.channel, f.platform); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	if err := batch.Send(); err != nil {
		t.Fatalf("send: %v", err)
	}
	defer func() {
		// mutations_sync=2 makes the cleanup synchronous — an async DELETE can
		// still be in flight when a rerun inserts the same fixture.
		_ = ch.Exec(ctx, `ALTER TABLE events DELETE WHERE project_id = ? SETTINGS mutations_sync = 2`, projectID)
		_ = ch.Exec(ctx, `ALTER TABLE aliases DELETE WHERE project_id = ? SETTINGS mutations_sync = 2`, projectID)
	}()

	// frank's alias exists from the start so the dedup proof is real: two raw
	// distinct_ids (frank, canonical-frank) must fold to one person.
	if err := ch.Exec(ctx, `INSERT INTO aliases (project_id, anonymous_id, canonical_id) VALUES (?, 'frank', 'canonical-frank')`, projectID); err != nil {
		t.Fatalf("alias insert: %v", err)
	}
	s.resolvers.invalidate(projectID)
	// Deterministic reload — LIFETIME+sleep raced the dictionary load once and
	// silently un-folded the fixture.
	if err := ch.Exec(ctx, "SYSTEM RELOAD DICTIONARY lohi_analytics.aliases_dict"); err != nil {
		t.Fatalf("dict reload: %v", err)
	}

	res, err := s.Overview(ctx, projectID, "7d", "", now)
	if err != nil {
		t.Fatalf("overview: %v", err)
	}

	// Active users in Sep 5–11: alice, bob, carol, dave, erin, frank = 6 people
	// (frank's two raw ids fold to one). The verification event, the crawler,
	// and the agent event must not count.
	if res.Metrics.ActiveUsers.State != OverviewStateOK || res.Metrics.ActiveUsers.Value == nil || *res.Metrics.ActiveUsers.Value != 6 {
		t.Fatalf("active_users = %d (state %s), want ok 6", *res.Metrics.ActiveUsers.Value, res.Metrics.ActiveUsers.State)
	}
	// New users in range: alice (Sep 6), bob (Sep 8), dave (Sep 7), frank
	// (Sep 6) = 4. carol (Aug 10) and erin (Aug 20) first seen before the
	// range — not new on any platform.
	if res.Metrics.NewUsers.Value == nil || *res.Metrics.NewUsers.Value != 4 {
		t.Fatalf("new_users = %+v, want 4", res.Metrics.NewUsers)
	}
	// Sessions: alice s-alice06 + s-alice07, bob s-bob08, carol s-carol09,
	// dave s-dave, erin s-erin-ios, frank s-frank06 + s-cf = 8.
	if res.Metrics.Sessions.Value == nil || *res.Metrics.Sessions.Value != 8 {
		t.Fatalf("sessions = %+v, want 8", res.Metrics.Sessions)
	}
	// Activation/revenue: unconfigured, never a number.
	if res.Metrics.Activation.State != OverviewStateUnconfigured || res.Metrics.Revenue.State != OverviewStateUnconfigured {
		t.Fatalf("activation/revenue must be unconfigured: %+v %+v", res.Metrics.Activation, res.Metrics.Revenue)
	}
	// Trend: 7 points. Sep 6: alice+frank=2, Sep 7: alice+dave+frank(canon)=3,
	// Sep 8: bob=1, Sep 9: carol+erin=2, others 0.
	if len(res.Trend) != 7 {
		t.Fatalf("trend len = %d, want 7", len(res.Trend))
	}
	wantTrend := map[string]uint64{"2026-09-05": 0, "2026-09-06": 2, "2026-09-07": 3, "2026-09-08": 1, "2026-09-09": 2, "2026-09-10": 0, "2026-09-11": 0}
	for _, p := range res.Trend {
		if p.ActiveUsers != wantTrend[p.Day] {
			t.Fatalf("trend %s = %d, want %d", p.Day, p.ActiveUsers, wantTrend[p.Day])
		}
	}
	// Retention (lifetime cohorts): alice Sep 6 (D1 returned Sep 7), bob Sep 8
	// (no return), carol Aug 10 (D1+D7+D30 returned), dave Sep 7 (no return),
	// erin Aug 20 (no return), frank Sep 6 (canonical-frank event Sep 7 = D1
	// return — the stitched person, not a second member).
	// D1 eligible: all six matured (cohort+2d <= Sep 12) = 6;
	// returned: alice + carol + frank = 3 → rate 0.5.
	if res.Retention.D1.State != OverviewStateOK || res.Retention.D1.Eligible != 6 || res.Retention.D1.Returned != 3 {
		t.Fatalf("d1 = %+v, want ok eligible 6 returned 3", res.Retention.D1)
	}
	if res.Retention.D1.Rate != 0.5 {
		t.Fatalf("d1 rate = %v, want 0.5", res.Retention.D1.Rate)
	}
	// D7 eligible: carol (Aug 10 + 8d = Aug 18) and erin (Aug 20 + 8d = Aug 28)
	// — both <= Sep 12. alice/bob/dave/frank cohorts are Sep 6-8, +8d lands
	// after Sep 12 — NOT eligible. Per-cohort-day maturity is the whole point:
	// they must not enter the denominator.
	if res.Retention.D7.State != OverviewStateOK || res.Retention.D7.Eligible != 2 || res.Retention.D7.Returned != 1 {
		t.Fatalf("d7 = %+v, want ok eligible 2 returned 1", res.Retention.D7)
	}
	// D30: carol Aug 10 + 31d = Sep 10 <= Sep 12 → eligible 1; her Sep-9 event
	// lands exactly on cohort day 30 → returned 1, rate 1.
	if res.Retention.D30.State != OverviewStateOK || res.Retention.D30.Eligible != 1 || res.Retention.D30.Returned != 1 || res.Retention.D30.Rate != 1 {
		t.Fatalf("d30 = %+v, want ok eligible 1 returned 1 rate 1", res.Retention.D30)
	}
	if res.Retention.CohortWindow != "lifetime" {
		t.Fatalf("cohort_window = %q, want lifetime", res.Retention.CohortWindow)
	}
	// Data status: events arrived (incl. non-qualifying), state fresh.
	if !res.DataStatus.EverReceived || res.DataStatus.State != "fresh" {
		t.Fatalf("data_status = %+v", res.DataStatus)
	}
	if res.DataStatus.PipelineLag != "unavailable" {
		t.Fatalf("pipeline_lag = %q, want unavailable", res.DataStatus.PipelineLag)
	}
	// events_in_range counts everything (11 events Sep 5-11: alice×2, bob,
	// carol, verify, crawler, agent, dave, erin-ios, frank, canonical-frank);
	// qualifying_in_range excludes verify/crawler/agent → 8.
	if res.DataStatus.EventsInRange != 11 || res.DataStatus.QualifyingInRange != 8 {
		t.Fatalf("events_in_range=%d qualifying=%d, want 11/8", res.DataStatus.EventsInRange, res.DataStatus.QualifyingInRange)
	}
	// Top pages: raw pageview counts, unit declared.
	if res.Content.TopPages.Unit != "pageviews" || len(res.Content.TopPages.Rows) == 0 {
		t.Fatalf("top_pages = %+v", res.Content.TopPages)
	}
	// Top sources: direct + organic present, unknown explicit when empty.
	foundDirect := false
	for _, r := range res.Content.TopSources.Rows {
		if r.Value == "direct" {
			foundDirect = true
		}
	}
	if !foundDirect {
		t.Fatalf("top_sources missing direct: %+v", res.Content.TopSources.Rows)
	}

	// Platform scoping: platform=ios → dave + erin's ios event qualify;
	// new_users=1 (dave's first event IS ios; erin first seen on web in Aug —
	// project-wide first-seen means she is not a new iOS user).
	ios, err := s.Overview(ctx, projectID, "7d", "ios", now)
	if err != nil {
		t.Fatalf("overview ios: %v", err)
	}
	if ios.Metrics.NewUsers.Value == nil || *ios.Metrics.NewUsers.Value != 1 {
		t.Fatalf("ios new_users = %+v, want 1 (dave)", ios.Metrics.NewUsers)
	}
	if ios.Metrics.ActiveUsers.Value == nil || *ios.Metrics.ActiveUsers.Value != 2 {
		t.Fatalf("ios active_users = %+v, want 2 (dave + erin)", ios.Metrics.ActiveUsers)
	}
	web, err := s.Overview(ctx, projectID, "7d", "web", now)
	if err != nil {
		t.Fatalf("overview web: %v", err)
	}
	// web new users: alice + bob + frank = 3 (dave's first event was ios — not
	// a new web user; carol and erin predate the range).
	if web.Metrics.NewUsers.Value == nil || *web.Metrics.NewUsers.Value != 3 {
		t.Fatalf("web new_users = %+v, want 3", web.Metrics.NewUsers)
	}

	// Empty project: honest no_data everywhere, never a fabricated zero.
	empty, err := s.Overview(ctx, emptyProjectID, "7d", "", now)
	if err != nil {
		t.Fatalf("overview empty: %v", err)
	}
	if empty.DataStatus.EverReceived || empty.DataStatus.State != "no_events" {
		t.Fatalf("empty data_status = %+v", empty.DataStatus)
	}
	if empty.Metrics.ActiveUsers.State != OverviewStateNoData || empty.Metrics.ActiveUsers.Value != nil {
		t.Fatalf("empty active_users = %+v, want no_data with no value", empty.Metrics.ActiveUsers)
	}
	if empty.Retention.D1.State != OverviewStateNotReady {
		t.Fatalf("empty d1 = %+v, want not_ready", empty.Retention.D1)
	}

	// "today" is partial: no previous comparison.
	partial, err := s.Overview(ctx, projectID, "today", "", now)
	if err != nil {
		t.Fatalf("overview today: %v", err)
	}
	if partial.Context.Range.CompleteDays {
		t.Fatalf("today must be partial")
	}
	if partial.Metrics.ActiveUsers.Previous != nil {
		t.Fatalf("partial range must omit previous comparison, got %v", *partial.Metrics.ActiveUsers.Previous)
	}

	fmt.Println("overview live fixture: all semantics verified")
}
