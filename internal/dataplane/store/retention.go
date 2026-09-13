package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"
)

// Event retention.
//
// The DuckDB event log is append-only, and the store it replaced was not: the
// ClickHouse schema carried `TTL toDateTime(timestamp) + INTERVAL 1 YEAR`
// before the port, so removing the engine silently changed all-history
// semantics AND removed the only bound on the file. On one VM, behind a
// per-colour volume whose other guard is container-log rotation, an unbounded
// event log is a disk-full outage, not a feature. So the bound is restored
// deliberately at the value the old schema enforced, and it is
// operator-visible: EVENT_RETENTION_DAYS (0 keeps everything) and the
// "Retention" section of docs/ARCHITECT-API.md.
//
// The sweep is admitted from the scheduler's minute tick but runs on its own
// goroutine under its own budget: that same callback drives alert evaluation
// and connector syncs, and a multi-minute delete must not hold that clock.
const (
	// retentionSweepInterval is how often a sweep may start. The window is
	// what the policy promises; this only decides how soon after the window
	// elapses the work happens.
	retentionSweepInterval = 24 * time.Hour
	// retentionBatch bounds one DELETE so a large backlog interleaves with
	// ingest instead of holding the single writer gate for the whole sweep.
	retentionBatch = 50_000
	// retentionBudget bounds one sweep for the same reason one level up: an
	// operator who shortens the window on a grown database gets progress per
	// sweep instead of one unbounded delete.
	retentionBudget = 5 * time.Minute
)

// Retention sweeps expired events out of the embedded event log. The zero
// value and a nil *Retention are both inert, so a caller that never enables
// retention needs no branch.
type Retention struct {
	store  *Store
	days   int
	batch  int
	budget time.Duration

	mu       sync.Mutex
	running  bool
	lastDone time.Time
	// cancel and done belong to the sweep in flight: Stop cancels it and waits
	// on done, which is what keeps a multi-minute delete from outliving the
	// store it deletes from.
	cancel context.CancelFunc
	done   chan struct{}
}

// NewRetention builds the sweep policy for a retention window in days. days
// <= 0 disables it — the operator's "keep everything" setting, and the reason
// this is a knob rather than a constant.
func NewRetention(s *Store, days int) *Retention {
	r := &Retention{store: s, days: days, batch: retentionBatch, budget: retentionBudget}
	if s == nil {
		return r
	}
	if r.Enabled() {
		log.Printf("agentray: event retention %d days — a daily sweep deletes events older than that (EVENT_RETENTION_DAYS=0 keeps every event)", days)
	} else {
		log.Printf("agentray: event retention disabled (EVENT_RETENTION_DAYS=%d) — the event log keeps every event", days)
	}
	return r
}

// Enabled reports whether this policy deletes anything.
func (r *Retention) Enabled() bool { return r != nil && r.store != nil && r.days > 0 }

// cutoff is the oldest timestamp the policy keeps: everything strictly older
// is expired. Buckets are UTC days, matching the store's TimeZone pin.
func (r *Retention) cutoff(now time.Time) time.Time {
	return now.UTC().AddDate(0, 0, -r.days)
}

// Tick admits at most one sweep per interval and returns immediately — the
// sweep runs on its own goroutine under its own budget, so a caller may use
// this directly as a periodic hook without holding its clock.
//
// A sweep that finishes marks the interval done, so steady state is one sweep
// a day. A sweep that stops at its budget (or fails) does NOT, so the next
// tick continues where it left off — which is what makes shortening the window
// on an already-grown database converge instead of taking one window per day.
func (r *Retention) Tick(ctx context.Context, now time.Time) {
	if !r.Enabled() {
		return
	}
	r.mu.Lock()
	if r.running || (!r.lastDone.IsZero() && now.Sub(r.lastDone) < retentionSweepInterval) {
		r.mu.Unlock()
		return
	}
	// The sweep must outlive the tick that admitted it: a hook whose context is
	// cancelled on return would abort the delete it just asked for. The budget,
	// not the caller, bounds the work — and Stop, not the tick, ends it early.
	budget := r.budget
	sweepCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), budget)
	done := make(chan struct{})
	r.running = true
	r.cancel, r.done = cancel, done
	r.mu.Unlock()

	go func() {
		defer close(done)
		defer cancel()

		removed, err := r.PruneOnce(sweepCtx, now)
		halved := 0

		r.mu.Lock()
		r.running = false
		r.cancel, r.done = nil, nil
		switch {
		case err == nil:
			r.lastDone = now
		case removed == 0 && errors.Is(sweepCtx.Err(), context.DeadlineExceeded):
			// The budget expired while the first batch was still scanning, so
			// its transaction rolled back and this sweep deleted nothing. The
			// next tick would re-issue the identical statement and hold the
			// writer gate for the same five minutes all over again; halving the
			// batch means a store that cannot commit one batch inside the
			// budget still converges instead of livelocking.
			if r.batch > 1 {
				r.batch /= 2
				halved = r.batch
			}
		}
		r.mu.Unlock()

		cutoff := r.cutoff(now).Format(time.RFC3339)
		switch {
		case err == nil && removed == 0:
			// Logged even when there was nothing to do: a missing line is how an
			// operator tells "retention ran and found nothing" from "retention
			// is not running at all".
			log.Printf("retention: nothing older than %s to delete", cutoff)
		case err == nil:
			log.Printf("retention: deleted %d events older than %s", removed, cutoff)
		case errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled):
			log.Printf("retention: sweep reached its %s budget after %d events older than %s — continuing on the next tick", budget, removed, cutoff)
			if halved > 0 {
				log.Printf("retention: a batch did not commit inside the budget; the next sweep deletes %d events at a time", halved)
			}
		default:
			log.Printf("retention: sweep failed after %d events older than %s: %v", removed, cutoff, err)
		}
	}()
}

// Stop ends the sweep in flight and waits for its goroutine to exit. It is what
// lets a server close its DuckDB handle without a delete landing in a closed
// pool: the sweep's own budget can be minutes away, and shutdown cannot wait
// that long. Safe to call when nothing is running, and safe to call twice.
func (r *Retention) Stop() {
	if r == nil {
		return
	}
	r.mu.Lock()
	cancel, done := r.cancel, r.done
	r.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if done != nil {
		<-done
	}
}

// PruneOnce runs one sweep and reports how many events it deleted. It is
// synchronous — Tick is the asynchronous wrapper — so a caller (or a test)
// that wants the work done before it returns uses this directly.
//
// The loop stops when nothing expired is left, when ctx is done, or on error;
// a partial result is returned with that error rather than discarded, so an
// interrupted sweep still reports its progress. Only committed batches count:
// a batch that is cut off rolls back, which is why Tick shrinks the batch when
// a sweep expires without removing anything.
func (r *Retention) PruneOnce(ctx context.Context, now time.Time) (int64, error) {
	if !r.Enabled() {
		return 0, nil
	}
	r.mu.Lock()
	batch := r.batch
	r.mu.Unlock()
	cutoff := r.cutoff(now)
	var removed int64
	for {
		n, err := r.store.DeleteEventsBefore(ctx, cutoff, batch)
		removed += n
		if err != nil {
			return removed, err
		}
		if n == 0 {
			break
		}
	}
	if removed > 0 {
		if err := r.store.checkpoint(ctx); err != nil {
			// The rows are already gone; folding the WAL only lets the freed
			// blocks be reused, so a failure here is not a failed sweep.
			log.Printf("retention: checkpoint after deleting %d events: %v", removed, err)
		}
	}
	return removed, nil
}

// DeleteEventsBefore deletes up to limit events older than cutoff and reports
// how many rows it removed. One call is one transaction on the writer gate.
//
// events has no index on timestamp — the primary key is (project_id, event_id)
// — so this is a scan, narrowed by DuckDB's per-row-group zone maps over the
// oldest row groups, which is the range it deletes. That is why the sweep is
// batched and budgeted rather than one statement.
func (s *Store) DeleteEventsBefore(ctx context.Context, cutoff time.Time, limit int) (int64, error) {
	if s.duck == nil {
		return 0, errDuckDBNotOpen
	}
	if limit <= 0 {
		return 0, fmt.Errorf("retention: batch limit must be positive, got %d", limit)
	}
	var removed int64
	err := s.duck.Write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
DELETE FROM events WHERE (project_id, event_id) IN (
	SELECT project_id, event_id FROM events WHERE "timestamp" < ? LIMIT ?)`,
			cutoff, limit)
		if err != nil {
			return err
		}
		removed, err = res.RowsAffected()
		return err
	})
	if err != nil {
		return 0, err
	}
	return removed, nil
}

// checkpoint folds the write-ahead log into the database file so the blocks a
// sweep freed can be reused.
func (s *Store) checkpoint(ctx context.Context) error {
	if s.duck == nil {
		return errDuckDBNotOpen
	}
	return s.duck.Checkpoint(ctx)
}
