package storage

import (
	"encoding/json"
	"testing"

	"github.com/google/uuid"
)

// A8, second half — the value types a real query returns must cross the child
// boundary intact. The first cut of this boundary killed the child on the first
// DECIMAL it met (`SELECT 31::DECIMAL(30,17)`), which is a money column and so
// not a corner: the tenant lost its warm copy and the caller got "sandbox
// stopped responding" for a query that was perfectly valid.
//
// Each case asserts three things: the query answers, the value is the shape a
// JSON client can read, and the child survived it.
func TestSandboxExoticResultTypes(t *testing.T) {
	d := openTestDuckDB(t)
	pool, ctx := newTestSandboxPool(t, d, nil)
	t.Cleanup(pool.closeAll)
	projectID := uuid.NewString()
	seedProject(t, d, projectID, 1)

	for _, tc := range []struct {
		sql  string
		want any
	}{
		{`SELECT 31::DECIMAL(30,17) AS n`, "31"},
		{`SELECT (-1.5)::DECIMAL(10,2) AS n`, "-1.5"},
		{`SELECT 170141183460469231731687303715884105727::HUGEINT AS n`, "170141183460469231731687303715884105727"},
		{`SELECT '1 day'::INTERVAL AS n`, "0 months 1 days 0 µs"},
		{`SELECT [1, 2, 3] AS n`, []any{int32(1), int32(2), int32(3)}},
		{`SELECT map([1], [2]) AS n`, map[string]any{"1": int32(2)}},
		{`SELECT union_value(a := 1) AS n`, map[string]any{"tag": "a", "value": int32(1)}},
	} {
		rows, err := pool.query(ctx, projectID, tc.sql, nil)
		if err != nil {
			t.Fatalf("%s: %v", tc.sql, err)
		}
		if len(rows) != 1 {
			t.Fatalf("%s returned %d rows, want 1", tc.sql, len(rows))
		}
		got := rows[0]["n"]
		if !deepEqualValue(got, tc.want) {
			t.Fatalf("%s = %#v (%T), want %#v", tc.sql, got, got, tc.want)
		}
		if _, err := json.Marshal(rows[0]); err != nil {
			t.Fatalf("%s: the row does not marshal to JSON: %v", tc.sql, err)
		}
		if _, err := pool.query(ctx, projectID, `SELECT 1 AS n`, nil); err != nil {
			t.Fatalf("%s cost the tenant its child: %v", tc.sql, err)
		}
	}

	// Nested values are driver values too: a LIST of DECIMAL is as unencodable
	// as a bare DECIMAL if the normalizer stops at the top level.
	for _, tc := range []struct {
		sql  string
		want any
	}{
		{`SELECT [1.5::DECIMAL(10,2)] AS n`, []any{"1.5"}},
		{`SELECT array_agg(v ORDER BY v) AS n FROM (SELECT 1.5::DECIMAL(10,2) AS v UNION ALL SELECT 2.25::DECIMAL(10,2)) t`,
			[]any{"1.5", "2.25"}},
		{`SELECT {'amount': 1.5::DECIMAL(10,2), 'when': INTERVAL 1 DAY} AS n`,
			map[string]any{"amount": "1.5", "when": "0 months 1 days 0 µs"}},
		{`SELECT [{'amount': 1.5::DECIMAL(10,2)}] AS n`, []any{map[string]any{"amount": "1.5"}}},
		// BIT: the driver rewrites a top-level BIT to its string form, but not
		// one nested in a LIST, and gob cannot carry the named type.
		{`SELECT ['101'::BIT] AS n`, []any{"101"}},
	} {
		rows, err := pool.query(ctx, projectID, tc.sql, nil)
		if err != nil {
			t.Fatalf("%s: %v", tc.sql, err)
		}
		if got := rows[0]["n"]; !deepEqualValue(got, tc.want) {
			t.Fatalf("%s = %#v (%T), want %#v", tc.sql, got, got, tc.want)
		}
		if _, err := json.Marshal(rows[0]); err != nil {
			t.Fatalf("%s: the row does not marshal to JSON: %v", tc.sql, err)
		}
	}

	// A STRUCT is a map already; a LIST of STRUCTs nests them.
	rows, err := pool.query(ctx, projectID, `SELECT {'a': 1, 'b': 'two'} AS n`, nil)
	if err != nil {
		t.Fatalf("struct: %v", err)
	}
	nested, ok := rows[0]["n"].(map[string]any)
	if !ok || nested["b"] != "two" {
		t.Fatalf("struct = %#v", rows[0]["n"])
	}

	if got := pool.spawns.Load(); got != 1 {
		t.Fatalf("spawns = %d, want 1: no case may have killed the child", got)
	}
}

// deepEqualValue compares the primitives these queries return without pulling
// in a reflection helper for one test.
func deepEqualValue(got, want any) bool {
	switch w := want.(type) {
	case []any:
		g, ok := got.([]any)
		if !ok || len(g) != len(w) {
			return false
		}
		for i := range w {
			if !deepEqualValue(g[i], w[i]) {
				return false
			}
		}
		return true
	case map[string]any:
		g, ok := got.(map[string]any)
		if !ok || len(g) != len(w) {
			return false
		}
		for k, v := range w {
			if !deepEqualValue(g[k], v) {
				return false
			}
		}
		return true
	default:
		return got == want
	}
}
