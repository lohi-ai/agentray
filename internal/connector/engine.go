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

// syncRunTimeout bounds one whole sync run (dial + every batch). Without it a
// source that hangs after connecting would pin a run goroutine forever; the
// cursor advanced per landed batch, so a timed-out run resumes cleanly.
const syncRunTimeout = 10 * time.Minute

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
	// CursorKey is the key of the last synced row — the tie-breaking half of
	// the keyset cursor, so rows sharing one cursor value are never skipped.
	CursorKey string
}

// LandedRow is one row ready for the ClickHouse landing table.
type LandedRow struct {
	Key      string
	Cursor   string
	DataJSON string
}

// SyncResult is what a finished run persists.
type SyncResult struct {
	// Cursor/CursorKey are the keyset position to persist when AdvanceCursor
	// is set. Cursor may legitimately be "" (NULL-cursor region) while
	// CursorKey carries the progress, so a separate flag decides persistence.
	Cursor        string
	CursorKey     string
	AdvanceCursor bool
	Rows          int
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
	// wg tracks tick-spawned runs so shutdown (and tests) can wait for them.
	wg sync.WaitGroup
}

func NewEngine(store Store) *Engine {
	return &Engine{store: store, running: map[string]bool{}}
}

// Tick starts every due sync for this minute. Called from the scheduler's
// OnTick, which runs alert evaluation and run publishing on the same single
// goroutine — so runs are dispatched to their own goroutines and never block
// the tick. Failures are recorded on the sync row; the per-sync claim keeps a
// still-running sync from being started again by a later tick.
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
		id := s.ID
		e.wg.Add(1)
		go func() {
			defer e.wg.Done()
			if err := e.RunSync(ctx, id); err != nil {
				log.Printf("connector: sync %s: %v", id, err)
			}
		}()
	}
}

// Wait blocks until every tick-spawned run has finished.
func (e *Engine) Wait() { e.wg.Wait() }

// RunSync executes one sync run end to end: open the source, pull incremental
// batches, land them in ClickHouse, persist cursor + status. The whole run is
// bounded by syncRunTimeout so a hung source cannot pin the goroutine. The
// returned error is also persisted on the sync row (sanitized upstream), so
// callers may ignore it for fire-and-forget scheduling.
func (e *Engine) RunSync(ctx context.Context, syncID string) error {
	if !e.claim(syncID) {
		return fmt.Errorf("sync %s is already running", syncID)
	}
	defer e.release(syncID)

	ctx, cancel := context.WithTimeout(ctx, syncRunTimeout)
	defer cancel()

	job, err := e.store.ConnectorSyncJob(ctx, syncID)
	if err != nil {
		return err
	}
	result := e.pullAndLand(ctx, job)
	// Persist the outcome even when the run itself timed out: the status write
	// must not ride the (possibly expired) run context.
	finishCtx, finishCancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer finishCancel()
	if err := e.store.FinishConnectorSync(finishCtx, syncID, result); err != nil {
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
	// Snapshot mode: order by the key column and never persist a cursor, so
	// each run re-lands the full table (idempotent via the landing key).
	cursorColumn := job.CursorColumn
	persistCursor := cursorColumn != ""
	if cursorColumn == "" {
		cursorColumn = job.KeyColumn
	}

	cursor := job.Cursor
	cursorKey := job.CursorKey
	total := 0
	result := func(errText string) SyncResult {
		advance := persistCursor && (cursor != job.Cursor || cursorKey != job.CursorKey)
		r := SyncResult{AdvanceCursor: advance, Rows: total, Err: errText}
		if advance {
			r.Cursor, r.CursorKey = cursor, cursorKey
		}
		return r
	}

	source, err := Open(ctx, job.Kind, job.DSN)
	if err != nil {
		return result(err.Error())
	}
	defer source.Close()

	hasMore := false
	for batch := 0; batch < maxBatchesPerRun; batch++ {
		pull, err := source.PullRows(ctx, PullRequest{
			Table:        job.Table,
			KeyColumn:    job.KeyColumn,
			CursorColumn: cursorColumn,
			Cursor:       cursor,
			CursorKey:    cursorKey,
			Limit:        pullBatchSize,
		})
		if err != nil {
			return result(err.Error())
		}
		if len(pull.Rows) == 0 {
			hasMore = false
			break
		}
		landed := make([]LandedRow, 0, len(pull.Rows))
		for _, r := range pull.Rows {
			data, err := json.Marshal(r.Data)
			if err != nil {
				return result(fmt.Sprintf("encode row %s: %v", r.Key, err))
			}
			landed = append(landed, LandedRow{Key: r.Key, Cursor: r.Cursor, DataJSON: string(data)})
		}
		if err := e.store.InsertExternalRows(ctx, job.ProjectID, job.ConnectorID, job.Table, landed); err != nil {
			return result(fmt.Sprintf("land rows: %v", err))
		}
		total += len(pull.Rows)
		hasMore = pull.HasMore
		if pull.NextCursor == cursor && pull.NextCursorKey == cursorKey {
			// No forward progress on the (cursor, key) pair — a source not
			// reporting keyset positions correctly; stop rather than loop.
			break
		}
		cursor, cursorKey = pull.NextCursor, pull.NextCursorKey
		if !pull.HasMore {
			break
		}
	}
	if hasMore && !persistCursor {
		// A snapshot sync restarts from scratch every run, so hitting the batch
		// cap means the tail of the table will never land — surface it instead
		// of reporting a silently truncated table as ok.
		return result(fmt.Sprintf("table exceeds the %d-row snapshot limit; configure a cursor column for incremental sync", maxBatchesPerRun*pullBatchSize))
	}
	return result("")
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
