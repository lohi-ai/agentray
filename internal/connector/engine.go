package connector

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/lohi-ai/agentray/internal/cronx"
)

// maxBatchesPerRun caps one sync run so a first pull of a huge table cannot
// monopolize the tick; the remainder lands on the next scheduled run because
// the cursor advanced.
const maxBatchesPerRun = 200

// pullBatchSize is the per-batch row limit requested from the source.
const pullBatchSize = 1000

// ScheduledSync is the engine's view of one enabled sync config: enough to
// decide "due now" without loading the connector or its credentials.
type ScheduledSync struct {
	ID   string
	Cron string
}

// SyncJob is everything one sync run needs, resolved by the store (including
// the decrypted DSN — the engine is platform code at the trust boundary; the
// DSN never leaves it).
type SyncJob struct {
	SyncID      string
	ProjectID   string
	ConnectorID string
	Kind        string
	DSN         string
	Table       string
	KeyColumn   string
	// CursorColumn empty = snapshot mode: the key column orders the pull and
	// the cursor is never persisted, so every run re-lands the whole table
	// (deduped by the landing table's ReplacingMergeTree key).
	CursorColumn string
	Cursor       string
}

// LandedRow is one row ready for the ClickHouse landing table.
type LandedRow struct {
	Key      string
	Cursor   string
	DataJSON string
}

// SyncResult is what a finished run persists.
type SyncResult struct {
	// Cursor is the new persisted cursor ("" = leave unchanged).
	Cursor string
	Rows   int
	// Err is the operator-readable failure ("" = success). Sources sanitize
	// their own errors; the store additionally truncates.
	Err string
}

// Store is the narrow persistence surface the engine needs; storage.Store
// implements it.
type Store interface {
	ListEnabledConnectorSyncs(ctx context.Context) ([]ScheduledSync, error)
	ConnectorSyncJob(ctx context.Context, syncID string) (SyncJob, error)
	InsertExternalRows(ctx context.Context, projectID, connectorID, table string, rows []LandedRow) error
	FinishConnectorSync(ctx context.Context, syncID string, result SyncResult) error
}

// Engine schedules and executes connector syncs. It rides the agent
// scheduler's minute tick; runs are serialized behind a mutex so overlapping
// ticks (or a tick racing a manual "run now") never double-pull one sync.
type Engine struct {
	store Store
	mu    sync.Mutex
	// running guards per-sync overlap when RunSync is called concurrently.
	running map[string]bool
}

func NewEngine(store Store) *Engine {
	return &Engine{store: store, running: map[string]bool{}}
}

// Tick runs every due sync for this minute. Called from the scheduler's
// OnTick; failures are recorded on the sync row, never propagated — a broken
// source must not disturb the tick.
func (e *Engine) Tick(ctx context.Context, now time.Time) {
	syncs, err := e.store.ListEnabledConnectorSyncs(ctx)
	if err != nil {
		log.Printf("connector: list syncs: %v", err)
		return
	}
	for _, s := range syncs {
		if s.Cron == "" || !cronx.Matches(s.Cron, now) {
			continue
		}
		if err := e.RunSync(ctx, s.ID); err != nil {
			log.Printf("connector: sync %s: %v", s.ID, err)
		}
	}
}

// RunSync executes one sync run end to end: open the source, pull incremental
// batches, land them in ClickHouse, persist cursor + status. The returned
// error is also persisted on the sync row (sanitized upstream), so callers may
// ignore it for fire-and-forget scheduling.
func (e *Engine) RunSync(ctx context.Context, syncID string) error {
	if !e.claim(syncID) {
		return fmt.Errorf("sync %s is already running", syncID)
	}
	defer e.release(syncID)

	job, err := e.store.ConnectorSyncJob(ctx, syncID)
	if err != nil {
		return err
	}
	result := e.pullAndLand(ctx, job)
	if err := e.store.FinishConnectorSync(ctx, syncID, result); err != nil {
		return err
	}
	if result.Err != "" {
		return fmt.Errorf("%s", result.Err)
	}
	return nil
}

// pullAndLand does the fallible middle of a run and always returns a
// persistable result.
func (e *Engine) pullAndLand(ctx context.Context, job SyncJob) SyncResult {
	source, err := Open(ctx, job.Kind, job.DSN)
	if err != nil {
		return SyncResult{Err: err.Error()}
	}
	defer source.Close()

	// Snapshot mode: order by the key column and never persist a cursor, so
	// each run re-lands the full table (idempotent via the landing key).
	cursorColumn := job.CursorColumn
	persistCursor := cursorColumn != ""
	if cursorColumn == "" {
		cursorColumn = job.KeyColumn
	}

	cursor := job.Cursor
	total := 0
	for batch := 0; batch < maxBatchesPerRun; batch++ {
		pull, err := source.PullRows(ctx, PullRequest{
			Table:        job.Table,
			KeyColumn:    job.KeyColumn,
			CursorColumn: cursorColumn,
			Cursor:       cursor,
			Limit:        pullBatchSize,
		})
		if err != nil {
			return SyncResult{Cursor: persistedCursor(persistCursor, cursor, job.Cursor), Rows: total, Err: err.Error()}
		}
		if len(pull.Rows) == 0 {
			break
		}
		landed := make([]LandedRow, 0, len(pull.Rows))
		for _, r := range pull.Rows {
			data, err := json.Marshal(r.Data)
			if err != nil {
				return SyncResult{Cursor: persistedCursor(persistCursor, cursor, job.Cursor), Rows: total, Err: fmt.Sprintf("encode row %s: %v", r.Key, err)}
			}
			landed = append(landed, LandedRow{Key: r.Key, Cursor: r.Cursor, DataJSON: string(data)})
		}
		if err := e.store.InsertExternalRows(ctx, job.ProjectID, job.ConnectorID, job.Table, landed); err != nil {
			return SyncResult{Cursor: persistedCursor(persistCursor, cursor, job.Cursor), Rows: total, Err: fmt.Sprintf("land rows: %v", err)}
		}
		total += len(pull.Rows)
		if pull.NextCursor != "" {
			if pull.NextCursor == cursor {
				// No forward progress (every row in a full batch shares one
				// cursor value); stop rather than loop. The stall is visible
				// as rows re-landing each run.
				break
			}
			cursor = pull.NextCursor
		}
		if !pull.HasMore {
			break
		}
	}
	return SyncResult{Cursor: persistedCursor(persistCursor, cursor, job.Cursor), Rows: total}
}

// persistedCursor returns the cursor to store: advanced in incremental mode,
// unchanged ("" = leave) in snapshot mode or when nothing moved.
func persistedCursor(persist bool, cursor, previous string) string {
	if !persist || cursor == previous {
		return ""
	}
	return cursor
}

func (e *Engine) claim(syncID string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.running[syncID] {
		return false
	}
	e.running[syncID] = true
	return true
}

func (e *Engine) release(syncID string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.running, syncID)
}
