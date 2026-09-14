package storage

import (
	"strings"
	"testing"
	"time"
)

func TestScopedReadonlySQLScopesEventsTable(t *testing.T) {
	query, args, err := scopedReadonlySQL(
		"SELECT event_type, count(*) AS total_events FROM events WHERE project_id != {project_id} GROUP BY event_type",
		"project-1",
		nil,
	)
	if err != nil {
		t.Fatalf("scopedReadonlySQL returned error: %v", err)
	}
	if !strings.Contains(query, "WITH scoped_events AS (SELECT *, canonical_distinct_id AS canonical_id FROM resolved_events WHERE project_id = ?)") {
		t.Fatalf("query did not scope events table: %s", query)
	}
	if !strings.Contains(query, "FROM scoped_events") {
		t.Fatalf("query did not read from scoped alias: %s", query)
	}
	if len(args) != 2 || args[0] != "project-1" || args[1] != "project-1" {
		t.Fatalf("args=%v want two project args", args)
	}
}

// scoped_events must expose a canonical_id that folds the anonymous id onto the
// identified user. Stitching resolves through the resolved_events view — a
// LEFT JOIN on the aliases mirror keyed by (project_id, distinct_id) — so the
// CTE carries no bind args for the alias map; only the project scoping arg.
func TestScopedReadonlySQLStitchesCanonicalID(t *testing.T) {
	query, args, err := scopedReadonlySQL(
		"SELECT count(DISTINCT canonical_id) AS users FROM events",
		"project-1",
		nil,
	)
	if err != nil {
		t.Fatalf("scopedReadonlySQL returned error: %v", err)
	}
	if !strings.Contains(query, "canonical_distinct_id AS canonical_id FROM resolved_events WHERE project_id = ?") {
		t.Fatalf("query did not stitch canonical_id via resolved_events: %s", query)
	}
	if len(args) != 1 || args[0] != "project-1" {
		t.Fatalf("args=%v want only the project scoping arg", args)
	}
}

// The table-function bypass (SSRF + cross-tenant read) must be rejected wherever
// the function appears, including inside a subquery that the events-source guard
// used to miss. The least-privilege CH role is the real backstop, but this
// keyword guard is the source of truth for the dev fallback path.
func TestValidateReadonlySQLRejectsTableFunctions(t *testing.T) {
	bypasses := []string{
		"SELECT * FROM url('http://169.254.169.254/', CSV)",
		"SELECT count() FROM events WHERE distinct_id IN (SELECT c FROM url('http://evil/', CSV, 'c String'))",
		"SELECT * FROM remote('other-host:9000', 'db', 'events')",
		"SELECT * FROM mysql('host:3306', 'db', 'secrets', 'u', 'p')",
		"SELECT * FROM postgresql('host:5432', 'db', 'secrets', 'u', 'p')",
		"SELECT * FROM file('/etc/passwd', 'CSV')",
		"SELECT * FROM s3('https://bucket/key', 'CSV')",
		"WITH x AS (SELECT * FROM REMOTE('h:9000','d','t')) SELECT * FROM x",
		// Comments are whitespace to the engine, so they must not bridge a
		// function name to its paren past the `name(` match.
		"SELECT * FROM url/**/('http://169.254.169.254/', CSV)",
		"SELECT * FROM url -- hop\n('http://evil/', CSV)",
		"SELECT * FROM s3/* wrap */('https://bucket/key', 'CSV')",
	}
	for _, q := range bypasses {
		if err := validateReadonlySQL(q); err == nil {
			t.Errorf("expected table-function bypass to be rejected: %s", q)
		}
	}
}

// A column or alias that merely shares a name with a table function must not be
// falsely rejected — the guard matches `name(`, not bare identifiers.
func TestValidateReadonlySQLAllowsInnocuousNames(t *testing.T) {
	oks := []string{
		"SELECT properties['url'] AS url FROM events",
		"SELECT count() AS file FROM events",
		"SELECT JSONExtractString(properties, 'url') FROM events WHERE event_name = 'user.pageview'",
	}
	for _, q := range oks {
		if err := validateReadonlySQL(q); err != nil {
			t.Errorf("innocuous query wrongly rejected (%v): %s", err, q)
		}
	}
}

// A caller's own CTE list must join ours rather than follow a second WITH: a
// de-dup grid or a cohort is written as a CTE, and `WITH a AS (…) WITH b AS (…)`
// is a DuckDB parser error — so every such query failed before this.
func TestScopedReadonlySQLMergesCallerCTEs(t *testing.T) {
	query, args, err := scopedReadonlySQL(
		"WITH money AS (SELECT amount FROM events WHERE event_name = 'revenue')\nSELECT sum(amount) FROM money",
		"project-1",
		nil,
	)
	if err != nil {
		t.Fatalf("scopedReadonlySQL returned error: %v", err)
	}
	if strings.Count(strings.ToUpper(query), "WITH ") != 1 {
		t.Fatalf("query must carry exactly one WITH keyword: %s", query)
	}
	if !strings.HasPrefix(query, "WITH scoped_events AS (SELECT *, canonical_distinct_id AS canonical_id FROM resolved_events WHERE project_id = ?), money AS (") {
		t.Fatalf("caller CTE was not merged into the scoped list: %s", query)
	}
	if !strings.Contains(query, "FROM scoped_events") {
		t.Fatalf("query did not read from scoped alias: %s", query)
	}
	if len(args) != 1 || args[0] != "project-1" {
		t.Fatalf("args=%v want one project arg", args)
	}

	// RECURSIVE covers the whole list, so it stays on the front and our CTEs
	// ride inside it.
	recursive, _, err := scopedReadonlySQL(
		"WITH RECURSIVE t AS (SELECT 1 AS n FROM events) SELECT n FROM t",
		"project-1",
		nil,
	)
	if err != nil {
		t.Fatalf("scopedReadonlySQL returned error: %v", err)
	}
	if !strings.HasPrefix(recursive, "WITH RECURSIVE scoped_events AS (") || strings.Count(strings.ToUpper(recursive), "WITH ") != 1 {
		t.Fatalf("recursive query not merged correctly: %s", recursive)
	}
}

// external_rows (the data-connector landing table) is the second readable
// source: it must be rewritten to a project-scoped FINAL CTE, alone or next to
// events, with CTE args in CTE order.
func TestScopedReadonlySQLScopesExternalRows(t *testing.T) {
	query, args, err := scopedReadonlySQL(
		"SELECT json_extract_string(data, '$.email') FROM external_rows WHERE table_name = 'users'",
		"project-1",
		nil,
	)
	if err != nil {
		t.Fatalf("scopedReadonlySQL returned error: %v", err)
	}
	if !strings.Contains(query, "WITH scoped_external_rows AS (SELECT * FROM external_rows WHERE project_id = ?)") {
		t.Fatalf("query did not scope external_rows: %s", query)
	}
	if !strings.Contains(query, "FROM scoped_external_rows") {
		t.Fatalf("query did not read from scoped alias: %s", query)
	}
	if strings.Contains(query, "scoped_events") {
		t.Fatalf("events CTE must not appear for an external_rows-only query: %s", query)
	}
	if len(args) != 1 || args[0] != "project-1" {
		t.Fatalf("args=%v want only the external_rows project arg", args)
	}
}

// A sync configured with soft_column deletions must hide its deleted rows on
// the SQL path exactly as dataset_preview does — the same predicate, applied
// per connector/table inside the scoped CTE. Identical source-table names may
// legitimately exist on two connectors in one project, so the connector ID is
// part of the rule identity.
func TestScopedReadonlySQLAppliesSoftDeleteRules(t *testing.T) {
	rules := []softDeleteRule{
		{ConnectorID: "connector-users", Table: "users", Column: "is_deleted", Semantics: "bool_true"},
		{ConnectorID: "connector-orders", Table: "orders", Column: "deleted_at", Semantics: "non_null"},
	}
	query, _, err := scopedReadonlySQL(
		"SELECT row_key FROM external_rows",
		"project-1",
		rules,
	)
	if err != nil {
		t.Fatalf("scopedReadonlySQL returned error: %v", err)
	}
	wantUsers := "AND NOT (connector_id = 'connector-users' AND table_name = 'users' AND coalesce(try_cast(json_extract_string(data, 'is_deleted') AS BOOLEAN), false))"
	wantOrders := "AND NOT (connector_id = 'connector-orders' AND table_name = 'orders' AND json_extract_string(data, 'deleted_at') IS NOT NULL)"
	if !strings.Contains(query, wantUsers) || !strings.Contains(query, wantOrders) {
		t.Fatalf("scoped CTE missing connector-specific soft-delete predicates: %s", query)
	}
}

func TestScopedReadonlySQLJoinsEventsWithExternalRows(t *testing.T) {
	query, args, err := scopedReadonlySQL(
		"SELECT count(*) FROM events e JOIN external_rows x ON x.row_key = e.distinct_id WHERE x.project_id != {project_id}",
		"project-1",
		nil,
	)
	if err != nil {
		t.Fatalf("scopedReadonlySQL returned error: %v", err)
	}
	if !strings.Contains(query, "FROM scoped_events") || !strings.Contains(query, "JOIN scoped_external_rows") {
		t.Fatalf("query did not scope both sources: %s", query)
	}
	// CTE order is events then external_rows, then the in-query placeholder.
	if len(args) != 3 || args[0] != "project-1" || args[1] != "project-1" || args[2] != "project-1" {
		t.Fatalf("args=%v want three project args (events CTE, external CTE, placeholder)", args)
	}
}

func TestScopedReadonlySQLStillRejectsUnknownSources(t *testing.T) {
	if _, _, err := scopedReadonlySQL("SELECT * FROM secrets", "project-1", nil); err == nil {
		t.Fatal("expected query with no scoped source to be rejected")
	}
}

// Comma joins, quoted identifiers, and any other reference form the rewrite
// does not recognize must be rejected outright — a residual bare `events` or
// `external_rows` token reads the raw multi-tenant table on a role with
// database-wide SELECT, so the guard fails closed on anything left over.
func TestScopedReadonlySQLRejectsResidualTenantTableReferences(t *testing.T) {
	bypasses := []string{
		// Comma cross-joins: the second table is not preceded by FROM/JOIN.
		"SELECT count() FROM external_rows x, events e",
		"SELECT count() FROM events e, external_rows x",
		"SELECT a.data, b.data FROM external_rows a, external_rows b WHERE a.row_key = b.row_key",
		"SELECT count() FROM events e, events f",
		// Quoted identifier beside a recognized source.
		`SELECT count() FROM external_rows x, "events" e`,
		// Alias shadowing the table name would survive rewriting as a bare token.
		"SELECT events.name FROM external_rows AS events",
	}
	for _, q := range bypasses {
		if _, _, err := scopedReadonlySQL(q, "project-1", nil); err == nil {
			t.Errorf("expected residual tenant-table reference to be rejected: %s", q)
		}
	}
}

// String literals that merely contain a table name must not trip the residual
// check — only identifier positions are dangerous.
func TestScopedReadonlySQLAllowsTableNamesInsideStringLiterals(t *testing.T) {
	oks := []string{
		"SELECT count() FROM events WHERE event_name = 'events'",
		"SELECT count() FROM external_rows WHERE table_name = 'external_rows'",
		"SELECT count() FROM events WHERE event_name = 'it''s external_rows'",
	}
	for _, q := range oks {
		if _, _, err := scopedReadonlySQL(q, "project-1", nil); err != nil {
			t.Errorf("literal-only mention wrongly rejected (%v): %s", err, q)
		}
	}
}

// Only FROM events is rewritten, so `… JOIN events` would read the bare table
// cross-tenant — it must stay rejected even when external_rows is present.
func TestScopedReadonlySQLRejectsEventsJoinedOntoExternalRows(t *testing.T) {
	_, _, err := scopedReadonlySQL(
		"SELECT count(*) FROM external_rows x JOIN events e ON e.distinct_id = x.row_key",
		"project-1",
		nil,
	)
	if err == nil {
		t.Fatal("expected JOIN events beside external_rows to be rejected")
	}
}

func TestScopedReadonlySQLRejectsEventsJoin(t *testing.T) {
	_, _, err := scopedReadonlySQL(
		"SELECT count(*) FROM events JOIN events AS other ON other.distinct_id = events.distinct_id",
		"project-1",
		nil,
	)
	if err == nil {
		t.Fatal("expected events join to be rejected")
	}
}

func TestFilteredWhereHumansOnlyExcludesBots(t *testing.T) {
	off, _ := filteredWhereWithDefault("project-1", EventFilter{}, false)
	if strings.Contains(off, "visitor_class") {
		t.Fatalf("humans-only must be opt-in; default added a bot filter: %s", off)
	}
	on, _ := filteredWhereWithDefault("project-1", EventFilter{HumansOnly: true}, false)
	if !strings.Contains(on, "coalesce(visitor_class, 'human') = 'human'") {
		t.Fatalf("HumansOnly did not exclude crawler traffic: %s", on)
	}
}

func TestFilteredWhereCanSkipDefaultTimeWindow(t *testing.T) {
	where, args := filteredWhereWithDefault("project-1", EventFilter{SessionID: "session-1"}, false)
	if strings.Contains(where, "timestamp") {
		t.Fatalf("where included default timestamp filter: %s", where)
	}
	if len(args) != 2 || args[0] != "project-1" || args[1] != "session-1" {
		t.Fatalf("args=%v want project and session args", args)
	}
}

func TestIdentityResolverCanonicalExpression(t *testing.T) {
	// The resolved_events view carries the stitched column; canonicalExpr maps
	// a distinct_id column reference onto it, preserving any table alias.
	bare := identityResolver{}
	expr, args := bare.canonicalExpr("distinct_id")
	if expr != "canonical_distinct_id" {
		t.Fatalf("expr=%q", expr)
	}
	if len(args) != 0 {
		t.Fatalf("args=%v want no bind args for a view column", args)
	}
	expr, _ = bare.canonicalExpr("e.distinct_id")
	if expr != "e.canonical_distinct_id" {
		t.Fatalf("expr did not preserve table alias: %q", expr)
	}
}

func TestFilteredWhereExpandsAliasedDistinctID(t *testing.T) {
	where, args := filteredWhereWithDistinctIDs(
		"project-1",
		EventFilter{DistinctID: "user-1"},
		false,
		[]string{"user-1", "anon-1"},
	)
	if !strings.Contains(where, "distinct_id IN (?, ?)") {
		t.Fatalf("where did not include expanded distinct_id filter: %s", where)
	}
	if len(args) != 3 || args[0] != "project-1" || args[1] != "user-1" || args[2] != "anon-1" {
		t.Fatalf("args=%v want project plus related distinct ids", args)
	}
}

// The resolver cache must serve a hit within the TTL, miss after it expires, and
// drop an entry on explicit invalidation (the alias-write path) so a freshly
// identified user is not stranded behind the TTL.
func TestResolverCacheHitExpiryAndInvalidate(t *testing.T) {
	c := newResolverCache(30 * time.Second)
	now := time.Unix(1000, 0)
	c.now = func() time.Time { return now }

	c.put("p1", identityResolver{anonymousIDs: []string{"a"}})
	if r, ok := c.get("p1"); !ok || len(r.anonymousIDs) != 1 {
		t.Fatalf("want cache hit within TTL, got ok=%v r=%+v", ok, r)
	}

	now = now.Add(31 * time.Second)
	if _, ok := c.get("p1"); ok {
		t.Fatal("want cache miss after TTL expiry")
	}

	now = time.Unix(1000, 0)
	c.put("p1", identityResolver{anonymousIDs: []string{"a"}})
	c.invalidate("p1")
	if _, ok := c.get("p1"); ok {
		t.Fatal("want cache miss after explicit invalidation")
	}
}

// A nil cache (a Store built without one) must degrade to always-miss, never
// panic — get/put/invalidate are all called unconditionally on the hot path.
func TestResolverCacheNilSafe(t *testing.T) {
	var c *resolverCache
	if _, ok := c.get("p1"); ok {
		t.Fatal("nil cache must always miss")
	}
	c.put("p1", identityResolver{})
	c.invalidate("p1")
}

func TestWorkspaceFilteredWhereScopesProjectIDs(t *testing.T) {
	where, args := workspaceFilteredWhere(
		[]string{"project-1", "project-2"},
		EventFilter{EventType: "user", DistinctID: "user-1"},
		false,
	)
	if !strings.Contains(where, "project_id IN (?, ?)") {
		t.Fatalf("where did not scope project ids: %s", where)
	}
	if !strings.Contains(where, "event_type = ?") || !strings.Contains(where, "distinct_id = ?") {
		t.Fatalf("where did not include filters: %s", where)
	}
	want := []any{"project-1", "project-2", "user", "user-1"}
	if len(args) != len(want) {
		t.Fatalf("args=%v want %v", args, want)
	}
	for i := range want {
		if args[i] != want[i] {
			t.Fatalf("args=%v want %v", args, want)
		}
	}
}
