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
		if fakePlugin.openErr != nil {
			return nil, fakePlugin.openErr
		}
		return fakePlugin.source, nil
	})
}

func useFakeSource(s Source, openErr error) {
	fakePlugin.mu.Lock()
	fakePlugin.source = s
	fakePlugin.openErr = openErr
	fakePlugin.mu.Unlock()
}

// fakeSource pops scripted batches; it records the cursors the engine asked
// for. Mutex-guarded because Tick dispatches runs to their own goroutines.
type fakeSource struct {
	mu      sync.Mutex
	batches []PullResult
	pullErr error
	cursors []string
	blockCh chan struct{} // when set, PullRows waits until closed
}

func (f *fakeSource) Kind() string                           { return "faketest" }
func (f *fakeSource) TestConnection(ctx context.Context) error { return nil }
func (f *fakeSource) DiscoverSchema(ctx context.Context) ([]Table, error) {
	return nil, nil
}
func (f *fakeSource) Close() {}
func (f *fakeSource) PullRows(ctx context.Context, req PullRequest) (PullResult, error) {
	if f.blockCh != nil {
		<-f.blockCh
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

type fakeStore struct {
	mu        sync.Mutex
	syncs     []ScheduledSync
	job       SyncJob
	inserted  [][]LandedRow
	insertErr error
	finished  []SyncResult
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
func (f *fakeStore) FinishConnectorSync(ctx context.Context, syncID string, result SyncResult) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.finished = append(f.finished, result)
	return nil
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

func TestRunSyncAdvancesCursorAcrossBatches(t *testing.T) {
	b1 := rowsBatch("5", "k1", "k2")
	b1.HasMore = true
	b2 := rowsBatch("9", "k3")
	useFakeSource(&fakeSource{batches: []PullResult{b1, b2}}, nil)
	store := &fakeStore{job: incrementalJob()}

	if err := NewEngine(store).RunSync(context.Background(), "s1"); err != nil {
		t.Fatalf("RunSync: %v", err)
	}
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
	store := &fakeStore{job: incrementalJob()}
	// Fail the second insert only.
	failing := &failSecondInsertStore{fakeStore: store}

	err := NewEngine(failing).RunSync(context.Background(), "s1")
	if err == nil {
		t.Fatal("want error from failed insert")
	}
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
	store := &fakeStore{job: job}

	if err := NewEngine(store).RunSync(context.Background(), "s1"); err != nil {
		t.Fatalf("RunSync: %v", err)
	}
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
	store := &fakeStore{job: incrementalJob()}

	if err := NewEngine(store).RunSync(context.Background(), "s1"); err != nil {
		t.Fatalf("RunSync: %v", err)
	}
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
	store := &fakeStore{job: incrementalJob()}

	err := NewEngine(store).RunSync(context.Background(), "s1")
	if err == nil {
		t.Fatal("want error")
	}
	if got := store.finished[0]; got.Err != "connect failed: host unreachable" || got.Cursor != "" || got.Rows != 0 {
		t.Fatalf("result = %+v", got)
	}
}

// Two concurrent runs of one sync must not double-pull: the second call is
// refused while the first holds the claim.
func TestRunSyncRefusesOverlap(t *testing.T) {
	block := make(chan struct{})
	src := &fakeSource{blockCh: block}
	useFakeSource(src, nil)
	store := &fakeStore{job: incrementalJob()}
	engine := NewEngine(store)

	done := make(chan error, 1)
	go func() { done <- engine.RunSync(context.Background(), "s1") }()
	// Wait until the first run holds the claim (it blocks inside PullRows).
	for i := 0; i < 100; i++ {
		if !engine.claim("s1") {
			break
		}
		engine.release("s1")
		time.Sleep(5 * time.Millisecond)
	}
	if err := engine.RunSync(context.Background(), "s1"); err == nil || !strings.Contains(err.Error(), "already running") {
		t.Fatalf("overlapping run: err = %v, want already-running refusal", err)
	}
	close(block)
	if err := <-done; err != nil {
		t.Fatalf("first run: %v", err)
	}
}

// Tick runs only the syncs whose cron matches the tick minute.
func TestTickRunsOnlyDueSyncs(t *testing.T) {
	useFakeSource(&fakeSource{}, nil)
	store := &fakeStore{
		job: incrementalJob(),
		syncs: []ScheduledSync{
			{ID: "due", Cron: "* * * * *"},
			{ID: "not-due", Cron: "30 3 * * *"},
			{ID: "unscheduled", Cron: ""},
		},
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
	store := &fakeStore{job: incrementalJob()}

	if err := NewEngine(store).RunSync(context.Background(), "s1"); err != nil {
		t.Fatalf("RunSync: %v", err)
	}
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
	store := &fakeStore{job: job}

	err := NewEngine(store).RunSync(context.Background(), "s1")
	if err == nil || !strings.Contains(err.Error(), "snapshot limit") {
		t.Fatalf("err = %v, want snapshot-limit truncation error", err)
	}
	got := store.finished[0]
	if got.AdvanceCursor || got.Err == "" || got.Rows != maxBatchesPerRun {
		t.Fatalf("result = %+v, want no cursor, an error, and %d landed rows", got, maxBatchesPerRun)
	}
}
