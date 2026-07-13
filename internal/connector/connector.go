// Package connector is the generic external-data-source plugin framework: a
// Source knows how to test a connection, discover its schema, and pull rows
// incrementally; the Engine schedules configured syncs and lands the pulled
// rows in ClickHouse through the storage layer. Platform code, not agent code
// — agents only ever see the landed rows through run_sql, never a Source.
package connector

import (
	"context"
	"fmt"
	"sort"
	"sync"
)

// Column is one discovered source column.
type Column struct {
	Name string `json:"name"`
	Type string `json:"type"`
	// IsPrimaryKey marks primary-key membership so the UI (and the AI draft)
	// can propose a sensible row key without the operator knowing the schema.
	IsPrimaryKey bool `json:"is_primary_key"`
}

// Table is one discovered source table.
type Table struct {
	Name    string   `json:"name"`
	Columns []Column `json:"columns"`
}

// PullRequest asks a Source for the next incremental batch of one table.
type PullRequest struct {
	Table string
	// KeyColumn uniquely identifies a row (used as the idempotency key in the
	// landing table).
	KeyColumn string
	// CursorColumn orders the incremental pull (updated_at, id, …). Pagination
	// is keyset on (CursorColumn, KeyColumn) so rows tied on one cursor value
	// are never skipped across a batch boundary; NULL cursors sort first and
	// are paged by key alone.
	CursorColumn string
	// Cursor is the last synced cursor value ("" = NULL region or from the
	// beginning, disambiguated by CursorKey).
	Cursor string
	// CursorKey is the key of the last synced row, the tie-breaking half of
	// the keyset cursor ("" = from the beginning).
	CursorKey string
	// Limit caps the batch size; the Engine loops until HasMore is false.
	Limit int
}

// Row is one pulled source row.
type Row struct {
	// Key is the row's identity (KeyColumn value, stringified).
	Key string
	// Cursor is the row's cursor value (CursorColumn value, stringified).
	Cursor string
	// Data is the full selected row, JSON-encodable.
	Data map[string]any
}

// PullResult is one incremental batch.
type PullResult struct {
	Rows []Row
	// NextCursor is the cursor of the batch's last row ("" while still inside
	// the NULL-cursor region).
	NextCursor string
	// NextCursorKey is the key of the batch's last row — together with
	// NextCursor it is the keyset position the next pull resumes from.
	NextCursorKey string
	// HasMore hints that another batch is immediately available.
	HasMore bool
}

// Source is one external data source connection. Implementations must be safe
// to discard after use; the Engine opens a fresh Source per sync run.
type Source interface {
	// Kind returns the plugin name, e.g. "postgres".
	Kind() string
	// TestConnection verifies the DSN reaches a live server. The returned
	// error must be operator-readable and must never echo credentials.
	TestConnection(ctx context.Context) error
	// DiscoverSchema lists tables and columns the connection can read.
	DiscoverSchema(ctx context.Context) ([]Table, error)
	// PullRows returns the next incremental batch for one table.
	PullRows(ctx context.Context, req PullRequest) (PullResult, error)
	// Close releases the underlying connection.
	Close()
}

// OpenFunc constructs a Source from an operator-supplied DSN.
type OpenFunc func(ctx context.Context, dsn string) (Source, error)

var (
	registryMu sync.RWMutex
	registry   = map[string]OpenFunc{}
)

// Register adds a source plugin under its kind name. Called from plugin init;
// duplicate registration panics because it is always a programming error.
func Register(kind string, open OpenFunc) {
	registryMu.Lock()
	defer registryMu.Unlock()
	if _, dup := registry[kind]; dup {
		panic("connector: duplicate plugin " + kind)
	}
	registry[kind] = open
}

// Kinds lists the registered plugin names, sorted.
func Kinds() []string {
	registryMu.RLock()
	defer registryMu.RUnlock()
	out := make([]string, 0, len(registry))
	for k := range registry {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Open constructs a Source of the given kind from a DSN.
func Open(ctx context.Context, kind, dsn string) (Source, error) {
	registryMu.RLock()
	open, ok := registry[kind]
	registryMu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("connector: unknown source kind %q (available: %v)", kind, Kinds())
	}
	return open(ctx, dsn)
}
