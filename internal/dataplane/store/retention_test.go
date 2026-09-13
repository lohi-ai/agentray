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
