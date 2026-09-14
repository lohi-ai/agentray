package connector

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/lohi-ai/agentray/internal/shared/cronx"
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
	ID        string
	ProjectID string
	Cron      string
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
	// (deduped by the landing table's (project, connector, table, row_key)
	// primary key — INSERT OR REPLACE overwrites the previous row in place).
	CursorColumn string
	Cursor       string
	// CursorKey is the key of the last synced row — the tie-breaking half of
	// the keyset cursor, so rows sharing one cursor value are never skipped.
	CursorKey string
}

// LandedRow is one row ready for the DuckDB landing table.
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
	EnqueueConnectorRun(ctx context.Context, projectID, syncID, idemKey string) (run Run, enqueued bool, err error)
	ConnectorRunByIdempotencyKey(ctx context.Context, projectID, syncID, idemKey string) (run Run, found bool, err error)
	ClaimConnectorRun(ctx context.Context, runID, owner string) (Run, bool, error)
	HeartbeatConnectorRun(ctx context.Context, runID string) (cancelRequested bool, stillRunning bool, err error)
	FinishConnectorRun(ctx context.Context, runID, syncID, owner string, result SyncResult, cancelled bool) error
	ReconcileConnectorRuns(ctx context.Context, staleBefore time.Time) (int, error)
	ConnectorRunForProject(ctx context.Context, projectID, runID string) (Run, error)
	CancelConnectorRun(ctx context.Context, projectID, runID string) (Run, error)
}

// Run is one durable sync-run record — the client-visible contract for
// run_source/source_status/cancel_source_run. storage owns the row; the type
// lives here so the engine's Store interface does not import its own
// implementation package.
type Run struct {
	ID              string     `json:"id"`
	ProjectID       string     `json:"project_id"`
	SyncID          string     `json:"sync_id"`
	ConnectorID     string     `json:"connector_id"`
	Status          string     `json:"status"` // queued | running | succeeded | failed | cancelled
	IdempotencyKey  string     `json:"idempotency_key,omitempty"`
	CancelRequested bool       `json:"cancel_requested"`
	Rows            int        `json:"rows"`
	Cursor          string     `json:"cursor,omitempty"`
	CursorKey       string     `json:"cursor_key,omitempty"`
	Error           string     `json:"error,omitempty"`
	QueuedAt        time.Time  `json:"queued_at"`
	StartedAt       *time.Time `json:"started_at,omitempty"`
	FinishedAt      *time.Time `json:"finished_at,omitempty"`
}

// Engine schedules and executes connector syncs. It rides the agent
// scheduler's minute tick. The durable connector_runs row is the client
// contract: enqueue is idempotent and at most one run per sync is active
// (DB unique index), so overlapping ticks or a tick racing a manual run can
// never double-pull. The in-memory maps only carry per-process execution
// state — the cancel func and the worker semaphore.
type Engine struct {
	store Store
	mu    sync.Mutex
	// id identifies this process's runs — the lease owner Reconcile uses to
	// tell a dead process's rows from a live one's.
	id string
	// cancels maps an active run id to its context cancel — cancel_source_run
	// sets the DB flag AND calls this so a running pull stops promptly.
	cancels map[string]context.CancelFunc
	// sem bounds concurrent runs in this process.
	sem chan struct{}
	// heartbeatEvery is the lease/cancel poll interval; tests shrink it.
	heartbeatEvery time.Duration
	// wg tracks spawned runs so shutdown (and tests) can wait for them.
	wg sync.WaitGroup
	// pending counts admitted calls and goroutines parked on sem — bounded by
	// maxPendingRuns. closed fences admission before shutdown waits on wg.
	pending int
	closed  bool
	// heartbeatCallTimeout bounds ONE lease RPC; tests shrink it.
	heartbeatCallTimeout time.Duration
	// leaseStaleAfter is the last-confirmed-lease deadline; tests shrink it.
	leaseStaleAfter time.Duration
}

// maxConcurrentRuns bounds simultaneous syncs in one process.
const maxConcurrentRuns = 4

// runLeaseStaleAfter is how old a running row's heartbeat (or a queued row's
// age) must be before reconcile fences it — comfortably above the heartbeat
// interval so a live worker is never fenced by a peer's boot or tick.
const runLeaseStaleAfter = 2 * time.Minute

// maxHeartbeatFailures bounds consecutive heartbeat errors before the run is
// cancelled: a worker that can no longer prove its lease must not keep
// landing rows — a peer may have fenced it and freed the active guard.
const maxHeartbeatFailures = 3

// heartbeatCallTimeout bounds ONE lease RPC: a hung query must not ride the
// 10-minute run context past the 2-minute lease.
const heartbeatCallTimeout = 15 * time.Second

// queuedCancelTimeout bounds the terminal write a cancelled, not-yet-claimed
// worker makes for its own queued receipt — the write must survive the
// cancellation that caused it, and must not outlive the shutdown waiting on
// the run.
const queuedCancelTimeout = 5 * time.Second

// maxPendingRuns bounds goroutines parked on the run semaphore. Runs beyond
// it are refused BEFORE a durable row exists — a queued row with no worker
// would be silently abandoned until reconcile fails it, which is not
// backpressure. Refusal is typed (ErrEngineBusy) and retryable.
const maxPendingRuns = maxConcurrentRuns

// ErrEngineBusy rejects an enqueue when every worker slot and every pending
// slot is taken AND the caller's idempotency key resolves to no existing run.
// It is typed and retryable: no run row is created, so a retry with the same
// key is a fresh claim rather than a replay of an abandoned one.
var ErrEngineBusy = errors.New("connector engine at capacity — retry shortly")

func NewEngine(store Store) *Engine {
	return &Engine{store: store, id: uuid.NewString(), cancels: map[string]context.CancelFunc{}, sem: make(chan struct{}, maxConcurrentRuns), heartbeatEvery: 10 * time.Second, heartbeatCallTimeout: heartbeatCallTimeout, leaseStaleAfter: runLeaseStaleAfter}
}

// Tick starts every due sync for this minute. Called from the scheduler's
// OnTick, which runs alert evaluation and run publishing on the same single
// goroutine — so runs are dispatched to their own goroutines and never block
// the tick. Failures are recorded on the sync row; the per-sync claim keeps a
// still-running sync from being started again by a later tick.
func (e *Engine) Tick(ctx context.Context, now time.Time) {
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return
	}
	// Track the whole callback, not only the runs it dispatches: Shutdown may
	// race the scheduler after Stop unsubscribes an already-running tick.
	e.wg.Add(1)
	e.mu.Unlock()
	defer e.wg.Done()

	// Periodic recovery: a run orphaned by a process that died and restarted
	// inside the stale window keeps a fresh-looking heartbeat at startup —
	// only a later pass fences it once the lease actually expires.
	if n, err := e.store.ReconcileConnectorRuns(ctx, now.Add(-runLeaseStaleAfter)); err != nil {
		log.Printf("connector: reconcile runs: %v", err)
	} else if n > 0 {
		log.Printf("connector: fenced %d orphaned runs", n)
	}
	syncs, err := e.store.ListEnabledConnectorSyncs(ctx)
	if err != nil {
		log.Printf("connector: list syncs: %v", err)
		return
	}
	for _, s := range syncs {
		if s.Cron == "" || !cronx.Matches(s.Cron, now) {
			continue
		}
		if _, _, err := e.EnqueueRun(ctx, s.ProjectID, s.ID, ""); err != nil {
			log.Printf("connector: enqueue sync %s: %v", s.ID, err)
		}
	}
}

// Wait blocks until every admitted run has finished. Callers that can race new
// enqueues must use Shutdown so the wait-group cannot be incremented after Wait.
func (e *Engine) Wait() { e.wg.Wait() }

// Shutdown rejects new runs, cancels the currently active runs, and waits for
// every admitted run to finish before its backing stores are closed.
func (e *Engine) Shutdown() {
	e.mu.Lock()
	e.closed = true
	for _, cancel := range e.cancels {
		cancel()
	}
	e.mu.Unlock()
	e.wg.Wait()
}

// EnqueueRun records a queued run and dispatches a worker. The store decides
// whether this call created the run (enqueued) or observed an existing one —
// a duplicate idempotency key replays the original run, an already-active
// sync returns its live run. Either way the caller gets the run row back.
func (e *Engine) EnqueueRun(ctx context.Context, projectID, syncID, idemKey string) (run Run, enqueued bool, err error) {
	// Admission BEFORE the durable row: a run this process cannot serve must
	// never be recorded — an orphaned queued row would be fenced stale and a
	// same-key retry would replay the failure instead of running.
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return Run{}, false, ErrEngineBusy
	}
	if e.pending >= maxPendingRuns {
		e.mu.Unlock()
		// Capacity bounds NEW work. A retry carrying a key this sync already
		// resolved is a receipt read — refusing it as busy would hide the run
		// the caller asked about — and it dispatches nothing, so it consumes
		// no slot. A key with no prior run is still refused.
		if strings.TrimSpace(idemKey) != "" {
			existing, found, lerr := e.store.ConnectorRunByIdempotencyKey(ctx, projectID, syncID, idemKey)
			if lerr != nil {
				return Run{}, false, lerr
			}
			if found {
				return existing, false, nil
			}
		}
		return Run{}, false, ErrEngineBusy
	}
	e.pending++
	// Add while holding the admission lock so Shutdown cannot begin waiting
	// between accepting this call and registering it with the wait group.
	e.wg.Add(1)
	e.mu.Unlock()
	run, enqueued, err = e.store.EnqueueConnectorRun(ctx, projectID, syncID, idemKey)
	if err != nil {
		e.mu.Lock()
		e.pending--
		e.mu.Unlock()
		e.wg.Done()
		return run, enqueued, err
	}
	if !enqueued {
		// Replay or observed-active: no worker needed, release the slot.
		e.mu.Lock()
		e.pending--
		e.mu.Unlock()
		e.wg.Done()
		return run, enqueued, err
	}
	go func() {
		defer e.wg.Done()
		defer func() {
			e.mu.Lock()
			e.pending--
			e.mu.Unlock()
		}()
		e.sem <- struct{}{}
		defer func() { <-e.sem }()
		e.executeRun(run.ID, run.SyncID, run.ProjectID)
	}()
	return run, true, nil
}

// CancelRun asks a run to stop: the DB flag is the cross-process contract;
// the in-memory cancel makes it prompt when the worker lives here.
func (e *Engine) CancelRun(runID string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if cancel, ok := e.cancels[runID]; ok {
		cancel()
	}
}

// executeRun claims and runs one queued run end to end: open the source, pull
// incremental batches, land them in DuckDB, persist cursor + status on
// both the run row and the sync's last_* columns. The whole run is bounded by
// syncRunTimeout; the finish write rides an independent bounded context so a
// timed-out run still records its outcome.
func (e *Engine) executeRun(runID, syncID, projectID string) {
	ctx := context.Background()
	runCtx, cancel := context.WithTimeout(ctx, syncRunTimeout)

	// Register this run's cancel BEFORE the claim. Shutdown sweeps e.cancels
	// under e.mu, so a worker that registers after that sweep must observe
	// e.closed here and cancel itself — otherwise a run admitted just before
	// shutdown would keep pulling (and keep e.wg incremented) for up to
	// syncRunTimeout while Shutdown waits on e.wg.
	e.mu.Lock()
	shuttingDown := e.closed
	e.cancels[runID] = cancel
	e.mu.Unlock()
	if shuttingDown {
		cancel()
	}

	heartbeatDone := make(chan struct{})
	heartbeatRunning := false
	defer func() {
		cancel()
		if heartbeatRunning {
			<-heartbeatDone
		}
		e.mu.Lock()
		delete(e.cancels, runID)
		e.mu.Unlock()
	}()

	_, claimed, err := e.store.ClaimConnectorRun(runCtx, runID, e.id)
	if err != nil {
		if errors.Is(runCtx.Err(), context.Canceled) {
			// Cancelled between admission and the claim: no worker will ever
			// finish this row, so record the terminal state here rather than
			// leaving a live-looking queued receipt for stale-run reconcile.
			e.cancelQueuedRun(projectID, runID)
		}
		log.Printf("connector: claim run %s: %v", runID, err)
		return
	}
	if !claimed {
		return // cancelled while queued, or claimed elsewhere
	}

	// The heartbeat does two jobs in one write: it keeps the lease fresh so a
	// peer process's boot reconcile cannot fence this live run, and it reads
	// back cancel_requested so a cancel issued against another process still
	// lands here.
	heartbeatRunning = true
	go func() {
		defer close(heartbeatDone)
		tick := time.NewTicker(e.heartbeatEvery)
		defer tick.Stop()
		failures := 0
		// Time fence, not just error count: a heartbeat that never confirms
		// (hung query, slow network) must not let the lease lapse while the
		// pull keeps writing. lastConfirmed tracks the last successful lease
		// renewal; once it ages past the stale window the run is cancelled.
		lastConfirmed := time.Now()
		for {
			select {
			case <-runCtx.Done():
				return
			case <-tick.C:
				// Each lease RPC is bounded — a hung query cannot ride the
				// 10-minute run context past the 2-minute lease.
				hbCtx, hbCancel := context.WithTimeout(runCtx, e.heartbeatCallTimeout)
				cancelRequested, stillRunning, herr := e.store.HeartbeatConnectorRun(hbCtx, runID)
				hbCancel()
				if herr != nil {
					failures++
					if failures >= maxHeartbeatFailures || time.Since(lastConfirmed) > e.leaseStaleAfter {
						// Lease unprovable — stop before a peer's reconcile
						// frees the guard and a second writer lands rows.
						cancel()
						return
					}
					continue
				}
				failures = 0
				lastConfirmed = time.Now()
				if cancelRequested || !stillRunning {
					cancel()
					return
				}
			}
		}
	}()

	job, err := e.store.ConnectorSyncJob(runCtx, syncID)
	var result SyncResult
	if err != nil {
		result = SyncResult{Err: err.Error()}
	} else {
		result = e.pullAndLand(runCtx, job)
	}
	cancelled := errors.Is(runCtx.Err(), context.Canceled)
	finishCtx, finishCancel := context.WithTimeout(context.WithoutCancel(runCtx), 30*time.Second)
	defer finishCancel()
	if err := e.store.FinishConnectorRun(finishCtx, runID, syncID, e.id, result, cancelled); err != nil {
		log.Printf("connector: finish run %s: %v", runID, err)
	}
}

// cancelQueuedRun records the terminal cancelled state for a run this process
// admitted but could not claim because shutdown cancelled it mid-claim. The
// write rides its own bounded context — it must survive the cancellation that
// caused it — and only touches a row that is still queued, so a shutdown can
// never cancel a run another process already owns.
func (e *Engine) cancelQueuedRun(projectID, runID string) {
	ctx, cancel := context.WithTimeout(context.Background(), queuedCancelTimeout)
	defer cancel()
	run, err := e.store.ConnectorRunForProject(ctx, projectID, runID)
	if err != nil {
		log.Printf("connector: read run %s for cancellation: %v", runID, err)
		return
	}
	if run.Status != "queued" {
		return
	}
	if _, err := e.store.CancelConnectorRun(ctx, projectID, runID); err != nil {
		log.Printf("connector: cancel queued run %s: %v", runID, err)
	}
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
