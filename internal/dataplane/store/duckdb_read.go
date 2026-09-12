package storage

import (
	"context"
	"database/sql"
	"errors"
	"strings"
)

// This file is the DuckDB read edge: every analytics read that used to run
// through s.ch (ClickHouse) now runs through s.duck.Read, which admits the
// query on a pooled connection under the reader gate (maxDuckDBReaders
// concurrent MVCC snapshots). Rows cannot escape the gate — *sql.Rows is bound
// to the *sql.Conn — so the helpers consume the result inside the closure.

var errDuckDBNotOpen = errors.New("storage: duckdb not open")

// duckQuery runs query on a snapshot reader and feeds each row to scan, which
// fills caller-owned destinations. Scan errors abort the read.
func (s *Store) duckQuery(ctx context.Context, query string, args []any, scan func(rows *sql.Rows) error) error {
	if s.duck == nil {
		return errDuckDBNotOpen
	}
	return s.duck.Read(ctx, func(conn *sql.Conn) error {
		rows, err := conn.QueryContext(ctx, query, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			if err := scan(rows); err != nil {
				return err
			}
		}
		return rows.Err()
	})
}

// duckQueryRow runs a single-row query and scans it inside the read gate.
// Aggregate queries without GROUP BY always return one row; bare SELECTs may
// return sql.ErrNoRows, which callers handle like any other engine error.
func (s *Store) duckQueryRow(ctx context.Context, query string, args []any, dest ...any) error {
	if s.duck == nil {
		return errDuckDBNotOpen
	}
	return s.duck.Read(ctx, func(conn *sql.Conn) error {
		return conn.QueryRowContext(ctx, query, args...).Scan(dest...)
	})
}

// duckList converts a DuckDB LIST column scanned as []any into []string.
// groupUniqArrayIf/arraySort returned a native []string under clickhouse-go;
// go-duckdb hands back []any.
func duckList(v any) []string {
	items, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// --- DuckDB SQL fragments ---------------------------------------------------
//
// The ClickHouse dialect these replace, kept as named helpers so the port is
// mechanical and the semantics stay in one place:
//
//	countIf(cond)                    -> count(*) FILTER (WHERE cond)
//	uniqExact(x)                     -> count(DISTINCT x)
//	uniqExactIf(x, cond)             -> count(DISTINCT x) FILTER (WHERE cond)
//	argMaxIf(v, ts, cond)            -> arg_max(v, ts) FILTER (WHERE cond)
//	                                    (wrap in coalesce(..., '') when the CH
//	                                    default-value-on-empty mattered)
//	ifNull(x, d)                     -> coalesce(x, d)
//	JSONExtractString(p, 'k')        -> json_extract_string(p, '$.k')
//	JSONExtractString(p, '$set','k') -> json_extract_string(p, '$."$set".k')
//	JSONExtractFloat(p, 'k')         -> coalesce(try_cast(json_extract_string(p,'$.k') AS DOUBLE), 0)
//	JSONExtractBool(p, 'k')          -> coalesce(try_cast(json_extract(p,'$.k') AS BOOLEAN), false)
//	toStartOfHour(ts)                -> date_trunc('hour', ts)
//	toDate(ts, tz)                   -> CAST(timezone(tz, ts) AS DATE)
//	toStartOfWeek(ts, 1)             -> date_trunc('week', timezone('UTC', ts))
//	dateDiff('second', a, b)         -> date_diff('second', a, b)
//	positionCaseInsensitive(h, n)    -> strpos(lower(h), lower(n))
//	multiIf(...)                     -> CASE WHEN ... END
//	FINAL / *Merge aggregates        -> plain aggregates over the keyed tables
//	                                   and views (events PK, sessions view)

// duckJSONPath renders a property key as a DuckDB JSON path. Keys are bound
// values in the queries that use this; the path is concatenated in SQL text.
func duckJSONPath(key string) string {
	return "$." + key
}

// sqlQuote renders a Go string as a single-quoted SQL literal using standard
// '' escaping (DuckDB does not need ClickHouse's backslash form). Inputs are
// validated tokens (validSubscriptionToken) or internal allow-list values;
// this is defense in depth, not the injection boundary.
func sqlQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}
