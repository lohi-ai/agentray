package connector

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// The fake plugin is registered once (Register panics on duplicates); each
// test swaps the source it hands out.
var fakePlugin struct {
	mu      sync.Mutex
	source  Source
	openErr error
}

func init() {
	Register("faketest", func(ctx context.Context, dsn string) (Source, error) {
		fakePlugin.mu.Lock()
		defer fakePlugin.mu.Unlock()
		return fakePlugin.source, fakePlugin.openErr
	})
}

func useFakeSource(s Source, openErr error) {
	fakePlugin.mu.Lock()
	defer fakePlugin.mu.Unlock()
	fakePlugin.source = s
	fakePlugin.openErr = openErr
}

// fakeSource pops scripted batches; it records the cursors the engine asked
// for. Mutex-guarded because runs execute on their own goroutines. blockCh
// blocks PullRows until closed OR the run context is cancelled — the cancel
// path is what the cancellation tests exercise.
type fakeSource struct {
	mu      sync.Mutex
	batches []PullResult
	pullErr error
	cursors []string
	blockCh chan struct{}
}

func (f *fakeSource) Kind() string                             { return "faketest" }
func (f *fakeSource) TestConnection(ctx context.Context) error { return nil }
func (f *fakeSource) DiscoverSchema(ctx context.Context) ([]Table, error) {
	return nil, nil
}
func (f *fakeSource) Close() {}
func (f *fakeSource) PullRows(ctx context.Context, req PullRequest) (PullResult, error) {
	if f.blockCh != nil {
		select {
		case <-f.blockCh:
		case <-ctx.Done():
			return PullResult{}, ctx.Err()
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cursors = append(f.cursors, req.Cursor)
	if f.pullErr != nil {
		return PullResult{}, f.pullErr
	}
	if len(f.batches) == 0 {
		return PullResult{}, nil
	}
	next := f.batches[0]
	f.batches = f.batches[1:]
	return next, nil
}

func (f *fakeSource) pulledCursors() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.cursors...)
}

// fakeStore is the engine's Store with an in-memory run model mirroring the
// real contract: one active run per sync, claim moves queued→running under an
// owner, heartbeat reads back the persisted cancel flag.
type fakeStore struct {
	mu        sync.Mutex
	syncs     []ScheduledSync
	job       SyncJob
	inserted  [][]LandedRow
	insertErr error
	finished  []SyncResult
	cancelled []bool
	runs      map[string]*Run
	runSeq    int
}

func newFakeStore(job SyncJob) *fakeStore {
	return &fakeStore{job: job, runs: map[string]*Run{}}
}

func (f *fakeStore) ListEnabledConnectorSyncs(ctx context.Context) ([]ScheduledSync, error) {
	return f.syncs, nil
}
func (f *fakeStore) ConnectorSyncJob(ctx context.Context, syncID string) (SyncJob, error) {
	job := f.job
	job.SyncID = syncID
	return job, nil
}
func (f *fakeStore) InsertExternalRows(ctx context.Context, projectID, connectorID, table string, rows []LandedRow) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.insertErr != nil {
		return f.insertErr
	}
	f.inserted = append(f.inserted, rows)
	return nil
}

func (f *fakeStore) EnqueueConnectorRun(ctx context.Context, projectID, syncID, idemKey string) (Run, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.runs {
		if r.SyncID == syncID && (r.Status == "queued" || r.Status == "running") {
			return *r, false, nil
		}
	}
	f.runSeq++
	r := &Run{ID: fmt.Sprintf("run-%d", f.runSeq), ProjectID: projectID, SyncID: syncID, Status: "queued", QueuedAt: time.Now()}
	f.runs[r.ID] = r
	return *r, true, nil
}

func (f *fakeStore) ClaimConnectorRun(ctx context.Context, runID, owner string) (Run, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r := f.runs[runID]
	if r == nil || r.Status != "queued" || r.CancelRequested {
		return Run{}, false, nil
	}
	r.Status = "running"
	return *r, true, nil
}

func (f *fakeStore) HeartbeatConnectorRun(ctx context.Context, runID string) (bool, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r := f.runs[runID]
	if r == nil || r.Status != "running" {
		return false, false, nil
	}
	return r.CancelRequested, true, nil
}

func (f *fakeStore) FinishConnectorRun(ctx context.Context, runID, syncID, owner string, result SyncResult, cancelled bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.finished = append(f.finished, result)
	f.cancelled = append(f.cancelled, cancelled)
	if r := f.runs[runID]; r != nil {
		switch {
		case cancelled:
			r.Status = "cancelled"
		case result.Err != "":
			r.Status = "failed"
		default:
			r.Status = "succeeded"
		}
	}
	return nil
}

func (f *fakeStore) ReconcileConnectorRuns(ctx context.Context, staleBefore time.Time) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, r := range f.runs {
		if (r.Status == "running" || r.Status == "queued") && r.QueuedAt.Before(staleBefore) {
			r.Status = "failed"
			n++
		}
	}
	return n, nil
}

// requestCancel sets the persisted flag — what CancelConnectorRun does in the
// real store — without going through any engine, so tests can cancel a run
// owned by a different engine instance.
func (f *fakeStore) requestCancel(runID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r := f.runs[runID]; r != nil {
		r.CancelRequested = true
	}
}

func incrementalJob() SyncJob {
	return SyncJob{ProjectID: "p1", ConnectorID: "c1", Kind: "faketest",
		Table: "users", KeyColumn: "id", CursorColumn: "updated_at"}
}

func rowsBatch(cursor string, keys ...string) PullResult {
	out := PullResult{NextCursor: cursor}
	for _, k := range keys {
		out.Rows = append(out.Rows, Row{Key: k, Cursor: cursor, Data: map[string]any{"id": k}})
	}
	if len(keys) > 0 {
		out.NextCursorKey = keys[len(keys)-1]
	}
	return out
}

// runSync enqueues one run and waits for the worker to finish it — the
// synchronous shape the old RunSync tests asserted against.
func runSync(t *testing.T, engine *Engine, store *fakeStore, syncID string) {
	t.Helper()
	_, enqueued, err := engine.EnqueueRun(context.Background(), "p1", syncID, "")
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if !enqueued {
		t.Fatal("enqueue reported an existing active run on a fresh sync")
	}
	engine.Wait()
}

func TestRunSyncAdvancesCursorAcrossBatches(t *testing.T) {
	b1 := rowsBatch("5", "k1", "k2")
	b1.HasMore = true
	b2 := rowsBatch("9", "k3")
	useFakeSource(&fakeSource{batches: []PullResult{b1, b2}}, nil)
	store := newFakeStore(incrementalJob())

	runSync(t, NewEngine(store), store, "s1")

	if len(store.inserted) != 2 || len(store.inserted[0]) != 2 || len(store.inserted[1]) != 1 {
		t.Fatalf("inserted batches = %+v", store.inserted)
	}
	if len(store.finished) != 1 {
		t.Fatalf("finished = %+v", store.finished)
	}
	got := store.finished[0]
	if !got.AdvanceCursor || got.Cursor != "9" || got.CursorKey != "k3" || got.Rows != 3 || got.Err != "" {
		t.Fatalf("result = %+v, want advanced cursor (9, k3), rows 3, no error", got)
	}
}

// A failed insert must not advance the persisted cursor past the last batch
// that actually landed — the failed batch is re-pulled on the next run.
func TestRunSyncInsertFailureKeepsLandedCursor(t *testing.T) {
	b1 := rowsBatch("5", "k1")
	b1.HasMore = true
	b2 := rowsBatch("9", "k2")
	src := &fakeSource{batches: []PullResult{b1, b2}}
	useFakeSource(src, nil)
	store := newFakeStore(incrementalJob())
	failing := &failSecondInsertStore{fakeStore: store}

	runSync(t, NewEngine(failing), store, "s1")

	got := store.finished[0]
	if got.Cursor != "5" {
		t.Fatalf("cursor = %q, want the last successfully landed cursor 5", got.Cursor)
	}
	if got.Err == "" || !strings.Contains(got.Err, "land rows") {
		t.Fatalf("err = %q, want a land-rows failure", got.Err)
	}
}

type failSecondInsertStore struct {
	*fakeStore
	calls int
}

func (f *failSecondInsertStore) InsertExternalRows(ctx context.Context, projectID, connectorID, table string, rows []LandedRow) error {
	f.calls++
	if f.calls == 2 {
		return fmt.Errorf("clickhouse down")
	}
	return f.fakeStore.InsertExternalRows(ctx, projectID, connectorID, table, rows)
}

// Snapshot mode (no cursor column) re-pulls from the beginning every run:
// the key column orders the pull and no cursor is ever persisted.
func TestRunSyncSnapshotModePersistsNoCursor(t *testing.T) {
	src := &fakeSource{batches: []PullResult{rowsBatch("k2", "k1", "k2")}}
	useFakeSource(src, nil)
	job := incrementalJob()
	job.CursorColumn = ""
	store := newFakeStore(job)

	runSync(t, NewEngine(store), store, "s1")

	if got := store.finished[0]; got.AdvanceCursor || got.Cursor != "" || got.CursorKey != "" || got.Rows != 2 {
		t.Fatalf("result = %+v, want no persisted cursor and 2 rows", got)
	}
	if cursors := src.pulledCursors(); len(cursors) == 0 || cursors[0] != "" {
		t.Fatalf("pull cursors = %v, want snapshot pull from the beginning", cursors)
	}
}

// When a full batch cannot advance the cursor (every row shares one value),
// the engine must stop rather than loop forever.
func TestRunSyncBreaksOnCursorStall(t *testing.T) {
	b1 := rowsBatch("7", "k1")
	b1.HasMore = true
	b2 := rowsBatch("7", "k1") // same cursor again: no forward progress
	b2.HasMore = true
	useFakeSource(&fakeSource{batches: []PullResult{b1, b2}}, nil)
	store := newFakeStore(incrementalJob())

	runSync(t, NewEngine(store), store, "s1")

	if len(store.inserted) != 2 {
		t.Fatalf("inserted %d batches, want 2 (stall detected after the second)", len(store.inserted))
	}
	if got := store.finished[0]; got.Cursor != "7" {
		t.Fatalf("cursor = %q, want 7", got.Cursor)
	}
}

// A source that cannot open still persists a failed run (visible status), and
// the sanitized error is what lands.
func TestRunSyncOpenFailurePersistsError(t *testing.T) {
	useFakeSource(nil, fmt.Errorf("connect failed: host unreachable"))
	store := newFakeStore(incrementalJob())

	runSync(t, NewEngine(store), store, "s1")

	if got := store.finished[0]; got.Err != "connect failed: host unreachable" || got.Cursor != "" || got.Rows != 0 {
		t.Fatalf("result = %+v", got)
	}
}

// Two concurrent enqueues of one sync must not double-pull: the second call
// observes the active run instead of starting another.
func TestRunSyncRefusesOverlap(t *testing.T) {
	block := make(chan struct{})
	src := &fakeSource{blockCh: block}
	useFakeSource(src, nil)
	store := newFakeStore(incrementalJob())
	engine := NewEngine(store)

	run, enqueued, err := engine.EnqueueRun(context.Background(), "p1", "s1", "")
	if err != nil || !enqueued {
		t.Fatalf("first enqueue: %v enqueued=%v", err, enqueued)
	}
	// The worker may not have claimed yet — queued OR running both block a
	// second enqueue.
	again, enqueued, err := engine.EnqueueRun(context.Background(), "p1", "s1", "")
	if err != nil {
		t.Fatalf("second enqueue: %v", err)
	}
	if enqueued || again.ID != run.ID {
		t.Fatalf("second enqueue = %+v enqueued=%v, want the same active run", again, enqueued)
	}
	close(block)
	engine.Wait()
	if len(store.finished) != 1 {
		t.Fatalf("finished %d runs, want exactly one", len(store.finished))
	}
}

// A cancel issued against the persisted flag — by another process, with no
// in-memory cancel func — must still stop the run at the next heartbeat.
func TestRunSyncObservesPersistedCancel(t *testing.T) {
	block := make(chan struct{})
	src := &fakeSource{blockCh: block}
	useFakeSource(src, nil)
	store := newFakeStore(incrementalJob())
	engine := NewEngine(store)
	engine.heartbeatEvery = 5 * time.Millisecond

	run, enqueued, err := engine.EnqueueRun(context.Background(), "p1", "s1", "")
	if err != nil || !enqueued {
		t.Fatalf("enqueue: %v enqueued=%v", err, enqueued)
	}
	// Wait for the run to be running (blocked inside PullRows), then set the
	// persisted flag as a peer process would.
	for i := 0; i < 200; i++ {
		store.mu.Lock()
		running := store.runs[run.ID].Status == "running"
		store.mu.Unlock()
		if running {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	store.requestCancel(run.ID)
	engine.Wait()

	if len(store.cancelled) != 1 || !store.cancelled[0] {
		t.Fatalf("cancelled = %v, want one cancelled run", store.cancelled)
	}
	if store.runs[run.ID].Status != "cancelled" {
		t.Fatalf("status = %q, want cancelled", store.runs[run.ID].Status)
	}
}

// Tick runs only the syncs whose cron matches the tick minute.
func TestTickRunsOnlyDueSyncs(t *testing.T) {
	useFakeSource(&fakeSource{}, nil)
	store := newFakeStore(incrementalJob())
	store.syncs = []ScheduledSync{
		{ID: "due", ProjectID: "p1", Cron: "* * * * *"},
		{ID: "not-due", ProjectID: "p1", Cron: "30 3 * * *"},
		{ID: "unscheduled", ProjectID: "p1", Cron: ""},
	}
	engine := NewEngine(store)
	engine.Tick(context.Background(), time.Date(2026, 7, 14, 10, 0, 0, 0, time.UTC))
	engine.Wait() // Tick dispatches runs to goroutines; wait before asserting
	if len(store.finished) != 1 {
		t.Fatalf("finished %d runs, want exactly the due one", len(store.finished))
	}
}

// Rows whose cursor column is NULL sort first and are paged by key alone: the
// pair position ("", key) must count as forward progress — not a stall — and
// must be persisted so the next run resumes inside the NULL region.
func TestRunSyncNullCursorRegionAdvancesByKey(t *testing.T) {
	b1 := PullResult{
		Rows:          []Row{{Key: "k1", Cursor: "", Data: map[string]any{"id": "k1"}}},
		NextCursor:    "",
		NextCursorKey: "k1",
		HasMore:       true,
	}
	b2 := PullResult{
		Rows:          []Row{{Key: "k2", Cursor: "", Data: map[string]any{"id": "k2"}}},
		NextCursor:    "",
		NextCursorKey: "k2",
	}
	useFakeSource(&fakeSource{batches: []PullResult{b1, b2}}, nil)
	store := newFakeStore(incrementalJob())

	runSync(t, NewEngine(store), store, "s1")

	if len(store.inserted) != 2 {
		t.Fatalf("inserted %d batches, want 2 (key-only progress must not stall)", len(store.inserted))
	}
	got := store.finished[0]
	if !got.AdvanceCursor || got.Cursor != "" || got.CursorKey != "k2" || got.Rows != 2 {
		t.Fatalf("result = %+v, want persisted position (\"\", k2) and 2 rows", got)
	}
}

// A snapshot sync (no cursor column) that still has rows left after the batch
// cap must fail loudly: it restarts from scratch every run, so the tail would
// otherwise silently never land.
func TestRunSyncSnapshotCapReportsTruncation(t *testing.T) {
	batches := make([]PullResult, maxBatchesPerRun)
	for i := range batches {
		b := rowsBatch(fmt.Sprintf("c%03d", i), fmt.Sprintf("k%03d", i))
		b.HasMore = true
		batches[i] = b
	}
	useFakeSource(&fakeSource{batches: batches}, nil)
	job := incrementalJob()
	job.CursorColumn = ""
	store := newFakeStore(job)

	runSync(t, NewEngine(store), store, "s1")

	got := store.finished[0]
	if got.AdvanceCursor || got.Err == "" || !strings.Contains(got.Err, "snapshot limit") || got.Rows != maxBatchesPerRun {
		t.Fatalf("result = %+v, want no cursor, a snapshot-limit error, and %d landed rows", got, maxBatchesPerRun)
	}
}
