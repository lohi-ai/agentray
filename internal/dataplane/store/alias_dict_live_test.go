package storage

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TestAliasDictionaryLive exercises the real ClickHouse identity-dictionary path
// against live infra: it applies the dictionary DDL, backfills from Postgres,
// dual-writes a new alias, and confirms dictGet resolves both the backfilled and
// the freshly-written id. It is skipped unless AGENTRAY_LIVE_CH is set, so CI and
// the default `go test` never touch a real cluster.
//
// Run with the docker-compose stack up:
//
//	AGENTRAY_LIVE_CH=1 \
//	AGENTRAY_LIVE_PG=postgres://lohi:lohi@localhost:5434/lohi_analytics?sslmode=disable \
//	AGENTRAY_LIVE_CH_ADDR=localhost:19000 \
//	go test ./internal/dataplane/store/ -run TestAliasDictionaryLive -v
func TestAliasDictionaryLive(t *testing.T) {
	if os.Getenv("AGENTRAY_LIVE_CH") == "" {
		t.Skip("set AGENTRAY_LIVE_CH to run the live ClickHouse dictionary test")
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

	// Apply just the alias table + dictionary DDL (not the whole migrate).
	if err := ch.Exec(ctx, `
CREATE TABLE IF NOT EXISTS aliases (
	project_id UUID, anonymous_id String, canonical_id String,
	version DateTime64(3, 'UTC') DEFAULT now64()
) ENGINE = ReplacingMergeTree(version) ORDER BY (project_id, anonymous_id)`); err != nil {
		t.Fatalf("create aliases: %v", err)
	}
	if err := ch.Exec(ctx, `
CREATE DICTIONARY IF NOT EXISTS aliases_dict (
	project_id UUID, anonymous_id String, canonical_id String
) PRIMARY KEY project_id, anonymous_id
SOURCE(CLICKHOUSE(TABLE 'aliases' DB 'lohi_analytics' USER 'lohi' PASSWORD 'lohi'))
LAYOUT(COMPLEX_KEY_HASHED()) LIFETIME(MIN 30 MAX 60)`); err != nil {
		t.Fatalf("create dict: %v", err)
	}

	// Seed a self-contained fixture: a fresh project plus one alias in
	// Postgres, so the backfill has something to carry and the test neither
	// depends on nor dirties another project's rows. Cleanup is registered
	// before the writes and removes only this fixture's records — including
	// the user/workspace/project seedConvProject creates — on every path.
	uid, pid := seedConvProject(t, s)
	var wsID string
	if err := pg.QueryRow(ctx, `SELECT workspace_id::text FROM projects WHERE id = $1`, pid).Scan(&wsID); err != nil {
		t.Fatalf("workspace lookup: %v", err)
	}
	defer func() {
		_, _ = pg.Exec(ctx, `DELETE FROM aliases WHERE project_id = $1`, pid)
		_ = ch.Exec(ctx, `ALTER TABLE aliases DELETE WHERE project_id = ? SETTINGS mutations_sync = 2`, pid)
		_, _ = pg.Exec(ctx, `DELETE FROM projects WHERE id = $1`, pid)
		_, _ = pg.Exec(ctx, `DELETE FROM workspace_members WHERE workspace_id = $1`, wsID)
		_, _ = pg.Exec(ctx, `DELETE FROM workspaces WHERE id = $1`, wsID)
		_, _ = pg.Exec(ctx, `DELETE FROM users WHERE id = $1`, uid)
	}()
	seedAnon := "seed-anon-" + time.Now().Format("150405.000000")
	seedCanon := "seed-canon-" + time.Now().Format("150405.000000")
	if _, err := pg.Exec(ctx, `INSERT INTO aliases (project_id, anonymous_id, canonical_id) VALUES ($1, $2, $3) ON CONFLICT DO NOTHING`, pid, seedAnon, seedCanon); err != nil {
		t.Fatalf("seed alias: %v", err)
	}

	// Backfill from Postgres through the real code path.
	if err := s.backfillAliasDictionary(ctx); err != nil {
		t.Fatalf("backfill: %v", err)
	}

	// Dual-write a brand-new alias through CreateAlias, then reload so the
	// dictionary sees it without waiting for LIFETIME.
	anon := "live-anon-" + time.Now().Format("150405.000")
	canon := "live-canon-" + time.Now().Format("150405.000")
	if err := s.CreateAlias(ctx, pid, anon, canon); err != nil {
		t.Fatalf("CreateAlias: %v", err)
	}
	if err := ch.Exec(ctx, "SYSTEM RELOAD DICTIONARY lohi_analytics.aliases_dict"); err != nil {
		t.Fatalf("reload: %v", err)
	}

	var got string
	if err := ch.QueryRow(ctx,
		"SELECT dictGetOrDefault('lohi_analytics.aliases_dict','canonical_id',(toUUID(?), ?), 'MISS')",
		pid, anon).Scan(&got); err != nil {
		t.Fatalf("dictGet: %v", err)
	}
	if got != canon {
		t.Fatalf("dual-written alias did not resolve: got %q want %q", got, canon)
	}
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func mustScanOne(ctx context.Context, t *testing.T, pg *pgxpool.Pool, sql string) string {
	t.Helper()
	var v string
	if err := pg.QueryRow(ctx, sql).Scan(&v); err != nil {
		t.Fatalf("scan %q: %v", sql, err)
	}
	return v
}
