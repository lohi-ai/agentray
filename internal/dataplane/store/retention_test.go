package storage

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
)

// retentionEvent inserts one event at `at` and returns its id.
func retentionEvent(t *testing.T, d *DuckDB, projectID string, at time.Time) string {
	t.Helper()
	id := uuid.NewString()
	if err := d.InsertEvents(context.Background(), []Event{duckEvent(projectID, id, "reader-1", at)}); err != nil {
		t.Fatalf("InsertEvents: %v", err)
	}
	return id
}

func eventCount(t *testing.T, d *DuckDB) int {
	t.Helper()
	return duckCount(t, d, `SELECT count(*) FROM events`)
}

// waitForEvents blocks until the event log holds exactly want rows. The sweep
// admitted by Tick runs on its own goroutine, so its effect is observed rather
// than assumed.
func waitForEvents(t *testing.T, d *DuckDB, want int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if got := eventCount(t, d); got == want {
			return
		} else if time.Now().After(deadline) {
			t.Fatalf("event log has %d rows, want %d", got, want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// waitForSweep blocks until the admitted sweep has finished and stamped its
// interval. waitForEvents watches the row count, which reaches its final value
// before the sweep returns; a test that reseeds on the count alone can race the
// still-running sweep, which then deletes the freshly inserted row.
func waitForSweep(t *testing.T, r *Retention) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		r.mu.Lock()
		finished := !r.running && !r.lastDone.IsZero()
		r.mu.Unlock()
		if finished {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the admitted sweep did not finish and stamp its interval")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestPruneRemovesOnlyExpiredEvents pins the boundary the retention window
// promises: strictly older than the cutoff goes, everything else stays.
func TestPruneRemovesOnlyExpiredEvents(t *testing.T) {
	d := openTestDuckDB(t)
	s := &Store{duck: d}
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	project := uuid.NewString()

	retentionEvent(t, d, project, now.AddDate(0, 0, -400))
	retentionEvent(t, d, project, now.AddDate(0, 0, -366))
	kept := retentionEvent(t, d, project, now.AddDate(0, 0, -364))
	if got := eventCount(t, d); got != 3 {
		t.Fatalf("seeded %d events, want 3", got)
	}

	removed, err := s.DeleteEventsBefore(ctx, now.AddDate(0, 0, -365), 1000)
	if err != nil {
		t.Fatalf("DeleteEventsBefore: %v", err)
	}
	if removed != 2 {
		t.Errorf("removed = %d, want 2", removed)
	}
	if got := eventCount(t, d); got != 1 {
		t.Fatalf("event log has %d rows after the prune, want 1", got)
	}
	if got := duckCount(t, d, `SELECT count(*) FROM events WHERE event_id::VARCHAR = ?`, kept); got != 1 {
		t.Errorf("the event inside the window was deleted (count = %d, want 1)", got)
	}
}

// TestRetentionPruneOnceUsesTheConfiguredWindow: the policy, not the caller,
// decides the cutoff, and a second sweep with nothing left to do is a no-op.
func TestRetentionPruneOnceUsesTheConfiguredWindow(t *testing.T) {
	d := openTestDuckDB(t)
	s := &Store{duck: d}
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	project := uuid.NewString()

	retentionEvent(t, d, project, now.AddDate(0, 0, -10))
	retentionEvent(t, d, project, now.AddDate(0, 0, -2))

	r := NewRetention(s, 7)
	removed, err := r.PruneOnce(ctx, now)
	if err != nil {
		t.Fatalf("PruneOnce: %v", err)
	}
	if removed != 1 {
		t.Errorf("removed = %d, want 1 (only the 10-day-old event is outside a 7-day window)", removed)
	}
	if got := eventCount(t, d); got != 1 {
		t.Fatalf("event log has %d rows, want 1", got)
	}

	removed, err = r.PruneOnce(ctx, now)
	if err != nil {
		t.Fatalf("second PruneOnce: %v", err)
	}
	if removed != 0 {
		t.Errorf("second sweep removed %d events, want 0", removed)
	}
}

// TestRetentionZeroKeepsEverything: 0 is the operator's "keep everything"
// setting, and it short-circuits before any statement reaches the store — a
// store with no DuckDB handle at all must not error, which is what proves no
// DELETE was attempted.
func TestRetentionZeroKeepsEverything(t *testing.T) {
	ctx := context.Background()
	r := NewRetention(&Store{}, 0)
	if r.Enabled() {
		t.Fatal("a 0-day window must not be enabled")
	}
	if removed, err := r.PruneOnce(ctx, time.Now()); err != nil || removed != 0 {
		t.Fatalf("PruneOnce with retention off = (%d, %v), want (0, nil)", removed, err)
	}
	r.Tick(ctx, time.Now())

	d := openTestDuckDB(t)
	s := &Store{duck: d}
	now := time.Now().UTC().Truncate(time.Second)
	retentionEvent(t, d, uuid.NewString(), now.AddDate(0, 0, -4000))
	off := NewRetention(s, 0)
	off.Tick(ctx, now)
	if got := eventCount(t, d); got != 1 {
		t.Errorf("retention off left %d rows, want the expired event kept (1)", got)
	}
}

// TestRetentionSweepsABacklogInBatches: a backlog larger than one batch is
// fully swept, and the batch bound is what stops it being one statement.
func TestRetentionSweepsABacklogInBatches(t *testing.T) {
	d := openTestDuckDB(t)
	s := &Store{duck: d}
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	project := uuid.NewString()

	for i := range 5 {
		retentionEvent(t, d, project, now.AddDate(0, 0, -400-i))
	}
	retentionEvent(t, d, project, now)

	r := NewRetention(s, 365)
	r.batch = 2
	removed, err := r.PruneOnce(ctx, now)
	if err != nil {
		t.Fatalf("PruneOnce: %v", err)
	}
	if removed != 5 {
		t.Errorf("removed = %d across batches, want 5", removed)
	}
	if got := eventCount(t, d); got != 1 {
		t.Fatalf("event log has %d rows, want 1", got)
	}
}

// TestPruneKeepsTheBatchesThatCommittedWhenTheBudgetExpires: the budget is
// enforced per sweep, not per statement, so a sweep cut off mid-backlog must
// hand back the batches that already committed — a rolled-back batch would make
// a slow store delete nothing at all, forever.
func TestPruneKeepsTheBatchesThatCommittedWhenTheBudgetExpires(t *testing.T) {
	d := openTestDuckDB(t)
	s := &Store{duck: d}
	now := time.Now().UTC().Truncate(time.Second)
	project := uuid.NewString()

	const seeded = 500
	for i := range seeded {
		retentionEvent(t, d, project, now.AddDate(0, 0, -400-i))
	}
	retentionEvent(t, d, project, now) // never expired

	r := NewRetention(s, 365)
	r.batch = 1 // one transaction per event, so the sweep cannot outrun the cancel
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	type outcome struct {
		removed int64
		err     error
	}
	done := make(chan outcome, 1)
	go func() {
		removed, err := r.PruneOnce(ctx, now)
		done <- outcome{removed, err}
	}()

	// Cancel as soon as the first batch has landed, which leaves a known-committed
	// prefix and a known-remaining backlog. The poll is bounded and watches the
	// sweep's own result, so a sweep that errors before its first delete, or
	// finishes before the test can interrupt it, fails instead of hanging.
	deadline := time.Now().Add(30 * time.Second)
	for eventCount(t, d) == seeded+1 {
		select {
		case early := <-done:
			t.Fatalf("the sweep finished (removed=%d err=%v) before the test could interrupt it", early.removed, early.err)
		default:
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("the sweep never committed a batch inside 30s")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()

	got := <-done
	if got.err == nil {
		t.Fatal("a sweep cut off mid-backlog must report the interruption")
	}
	if got.removed <= 0 || got.removed >= seeded {
		t.Fatalf("removed = %d, want the committed prefix (0 < n < %d)", got.removed, seeded)
	}
	if left := eventCount(t, d); left != seeded+1-int(got.removed) {
		t.Errorf("event log has %d rows, want %d left by an interrupted sweep", left, seeded+1-int(got.removed))
	}
}

// TestPruneStopsWhenItsContextIsDone: the budget is enforced by the context,
// and an interrupted sweep reports what it removed instead of losing it.
func TestPruneStopsWhenItsContextIsDone(t *testing.T) {
	d := openTestDuckDB(t)
	s := &Store{duck: d}
	now := time.Now().UTC().Truncate(time.Second)
	retentionEvent(t, d, uuid.NewString(), now.AddDate(0, 0, -400))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.DeleteEventsBefore(ctx, now.AddDate(0, 0, -365), 100); err == nil {
		t.Fatal("DeleteEventsBefore with a cancelled context must fail, not delete")
	}
	if got := eventCount(t, d); got != 1 {
		t.Errorf("event log has %d rows after a cancelled delete, want 1", got)
	}
	if _, err := NewRetention(s, 365).PruneOnce(ctx, now); err == nil {
		t.Error("PruneOnce with a cancelled context must report the interruption")
	}
}

// TestDeleteEventsBeforeRejectsAnEmptyBatch keeps the sweep loop honest: a
// non-positive batch would spin forever instead of terminating.
func TestDeleteEventsBeforeRejectsAnEmptyBatch(t *testing.T) {
	d := openTestDuckDB(t)
	s := &Store{duck: d}
	if _, err := s.DeleteEventsBefore(context.Background(), time.Now(), 0); err == nil {
		t.Fatal("a 0-row batch must be rejected, not treated as a completed sweep")
	}
}

// TestRetentionTickSweepsOncePerInterval: Tick does the work off the caller's
// goroutine, and an interval that has not elapsed admits no second sweep.
func TestRetentionTickSweepsOncePerInterval(t *testing.T) {
	d := openTestDuckDB(t)
	s := &Store{duck: d}
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	project := uuid.NewString()

	retentionEvent(t, d, project, now.AddDate(0, 0, -400))
	retentionEvent(t, d, project, now.AddDate(0, 0, -380))

	r := NewRetention(s, 365)
	r.Tick(ctx, now)
	waitForEvents(t, d, 0)
	waitForSweep(t, r)

	// Same interval: the tick must not sweep again.
	retentionEvent(t, d, project, now.AddDate(0, 0, -400))
	r.Tick(ctx, now.Add(time.Hour))
	time.Sleep(100 * time.Millisecond)
	if got := eventCount(t, d); got != 1 {
		t.Fatalf("second tick inside the interval swept the log to %d rows, want the expired event kept (1)", got)
	}

	// Next interval: it does.
	r.Tick(ctx, now.Add(retentionSweepInterval+time.Minute))
	waitForEvents(t, d, 0)
}

// TestRetentionTickDoesNotStackOnAnInFlightSweep: the running guard is what
// keeps a minute tick from piling sweeps onto the writer gate.
func TestRetentionTickDoesNotStackOnAnInFlightSweep(t *testing.T) {
	d := openTestDuckDB(t)
	s := &Store{duck: d}
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	retentionEvent(t, d, uuid.NewString(), now.AddDate(0, 0, -400))

	r := NewRetention(s, 365)
	r.mu.Lock()
	r.running = true // a sweep is in flight
	r.mu.Unlock()

	r.Tick(ctx, now)
	time.Sleep(100 * time.Millisecond)
	if got := eventCount(t, d); got != 1 {
		t.Fatalf("a tick while a sweep was in flight deleted to %d rows, want 1", got)
	}
}

// TestRetentionStopEndsTheSweepInFlight: the sweep detaches from the tick that
// admitted it, so nothing else can end it before its own budget — and a server
// closing DuckDB cannot wait five minutes. Stop cancels it and waits.
func TestRetentionStopEndsTheSweepInFlight(t *testing.T) {
	d := openTestDuckDB(t)
	s := &Store{duck: d}
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	project := uuid.NewString()

	const seeded = 500
	for i := range seeded {
		retentionEvent(t, d, project, now.AddDate(0, 0, -400-i))
	}

	r := NewRetention(s, 365)
	r.batch = 1
	r.budget = time.Hour // Stop, not the budget, is what has to end this sweep
	r.Tick(ctx, now)
	for eventCount(t, d) == seeded {
		time.Sleep(time.Millisecond)
	}

	stopped := make(chan struct{})
	go func() { r.Stop(); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(30 * time.Second):
		t.Fatal("Stop did not return while a sweep was in flight")
	}

	r.mu.Lock()
	running := r.running
	r.mu.Unlock()
	if running {
		t.Fatal("Stop returned with the sweep still marked in flight")
	}
	before := eventCount(t, d)
	time.Sleep(100 * time.Millisecond)
	if after := eventCount(t, d); after != before {
		t.Errorf("the event log moved from %d to %d rows after Stop returned", before, after)
	}
}

// TestRetentionHalvesABatchThatCannotCommitInsideTheBudget: a batch whose
// transaction rolls back deletes nothing, and lastDone stays unset so the next
// tick retries. Retrying the identical statement would hold the writer gate for
// another full budget every minute, so the batch has to shrink until it commits.
func TestRetentionHalvesABatchThatCannotCommitInsideTheBudget(t *testing.T) {
	d := openTestDuckDB(t)
	s := &Store{duck: d}
	now := time.Now().UTC().Truncate(time.Second)
	retentionEvent(t, d, uuid.NewString(), now.AddDate(0, 0, -400))

	r := NewRetention(s, 365)
	r.batch = 64
	r.budget = time.Nanosecond // no batch can commit inside this budget
	r.Tick(context.Background(), now)

	deadline := time.Now().Add(10 * time.Second)
	for {
		r.mu.Lock()
		running, batch, lastDone := r.running, r.batch, r.lastDone
		r.mu.Unlock()
		if !running {
			if batch != 32 {
				t.Errorf("batch = %d, want 32 — a batch that committed nothing must halve", batch)
			}
			if !lastDone.IsZero() {
				t.Error("a sweep that deleted nothing must not stamp the interval")
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the sweep did not return")
		}
		time.Sleep(5 * time.Millisecond)
	}

	if got := eventCount(t, d); got != 1 {
		t.Fatalf("event log has %d rows, want the expired event kept (1)", got)
	}
	// The next sweep runs on the smaller batch and does the work.
	r.budget = 5 * time.Second
	removed, err := r.PruneOnce(context.Background(), now)
	if err != nil {
		t.Fatalf("PruneOnce after the batch shrank: %v", err)
	}
	if removed != 1 || eventCount(t, d) != 0 {
		t.Errorf("removed = %d with %d rows left, want the expired event gone", removed, eventCount(t, d))
	}
}
