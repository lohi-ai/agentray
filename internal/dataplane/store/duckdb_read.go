package storage

import (
	"context"
	"database/sql"
	"errors"
	"strings"
)

// This file is the DuckDB read edge: every analytics read runs through
// s.duck.Read, which admits the
// query on a pooled connection under the reader gate (maxDuckDBReaders
// concurrent MVCC snapshots). Rows cannot escape the gate — *sql.Rows is bound
// to the *sql.Conn — so the helpers consume the result inside the closure.

var errDuckDBNotOpen = errors.New("storage: duckdb not open")

// IsEngineResourceError reports whether err is the trusted DuckDB engine
// refusing a query on its own budget — memory_limit or temp-directory size —
// rather than on the SQL itself. Matched on message text because the driver
// surfaces these as plain errors with no typed sentinel; the phrases are the
// engine's stable prefixes ("Out of Memory Error", "failed to allocate").
// Callers use this to answer retryable instead of leaking engine internals.
func IsEngineResourceError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "Out of Memory") ||
		strings.Contains(msg, "failed to allocate") ||
		strings.Contains(msg, "max_temp_directory_size")
}

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

// duckJSONPath renders a property key as a DuckDB JSON path. Keys are bound
// values in the queries that use this; the path is concatenated in SQL text.
func duckJSONPath(key string) string {
	return "$." + key
}

// sqlQuote renders a Go string as a single-quoted SQL literal using standard
// '' escaping. Inputs are
// this is defense in depth, not the injection boundary.
func sqlQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}
