package ingestion

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/lohi-ai/agentray/internal/dataplane/store"
)

// The store binding: /readyz is a claim about a colour's DuckDB file, so the
// file behind the durable has to be able to honour it. These tests reproduce the
// parent gate's transcript — a warm durable pointed at a volume that has never
// existed — against a real broker and two real files, and pin the direction that
// must keep working: a file that applied the floor stays ready across restarts.

// publishParity writes one connector batch and two event batches to a colour's
// stream, so a durable that applies them holds a three-message history in its
// file.
func publishParity(t *testing.T, c *colour) {
	t.Helper()
	ctx := context.Background()
	queue := NewJetStreamQueue(c.ss.JS, c.ss.Subject, c.ss.ConnectorSubject)
	if err := queue.PublishExternalRows(ctx, parityProject, parityConnector, parityTable, parityRows("k1", "k2")); err != nil {
		t.Fatalf("publish connector batch: %v", err)
	}
	if err := queue.InsertEvents(ctx, []storage.Event{parityEvent(1), parityEvent(2)}); err != nil {
		t.Fatalf("publish events: %v", err)
	}
}

// TestWarmDurableOverEmptyStoreRefusesReady is the parent gate's transcript as a
// test: a colour started on a DuckDB path that has never existed, reusing a
// durable whose ack floor another colour's file produced. The store behind that
// verdict holds none of the history the floor claims — and applying the messages
// published after the boot must not launder that.
func TestWarmDurableOverEmptyStoreRefusesReady(t *testing.T) {
	url := startBroker(t)
	ctx := context.Background()

	// The colour that acked the range, into its own file.
	warm := newColour(t, url, testConfig("colour-warm"))
	warm.serve(t)
	publishParity(t, warm)
	warm.waitReady(t, 20*time.Second)
	if got := len(warm.eventIDs(t)); got != 2 {
		t.Fatalf("warm colour applied %d events, want 2", got)
	}
	warm.park()

	// The recreated volume: same durable, never-before-existing DuckDB file.
	cold := newColour(t, url, testConfig("colour-warm"))
	cold.serve(t)

	v, err := cold.ss.ReplayStatus(ctx)
	if err != nil {
		t.Fatalf("replay status: %v", err)
	}
	if v.Ready {
		t.Fatalf("a fresh DuckDB file vouched for by a warm durable reports ready: %+v", v)
	}
	if v.Reason != ReplayStoreBehind {
		t.Fatalf("reason = %q, want %q (verdict %+v)", v.Reason, ReplayStoreBehind, v)
	}

	// Traffic received after the boot is applied — which is exactly what makes
	// the hole invisible: the file is not empty, it is missing the acked range.
	queue := NewJetStreamQueue(cold.ss.JS, cold.ss.Subject, cold.ss.ConnectorSubject)
	if err := queue.InsertEvents(ctx, []storage.Event{parityEvent(3)}); err != nil {
		t.Fatalf("publish after the cold boot: %v", err)
	}
	deadline := time.Now().Add(20 * time.Second)
	for len(cold.eventIDs(t)) == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if got := cold.eventIDs(t); len(got) != 1 || got[0] != uuidAt(3) {
		t.Fatalf("cold colour applied %v, want only the post-boot event %s", got, uuidAt(3))
	}
	if got := len(warm.externalRowKeys(t, parityTable)); got != 2 {
		t.Fatalf("warm colour holds %d connector rows before parking, want 2", got)
	}
	if got := len(cold.externalRowKeys(t, parityTable)); got != 0 {
		t.Fatalf("cold colour holds %d connector rows it never applied, want 0", got)
	}

	// The verdict survives the traffic that moved the durable past the hole.
	v, err = cold.ss.ReplayStatus(ctx)
	if err != nil {
		t.Fatalf("replay status after traffic: %v", err)
	}
	if v.Ready || v.Reason != ReplayStoreBehind {
		t.Fatalf("verdict after applying later messages = %+v, want %q", v, ReplayStoreBehind)
	}

	// And it survives a restart of the same file, which is the move an operator
	// makes first: the durable's floor now sits at the traffic this file DID
	// apply, so only the refusal written into the store can still refuse it.
	cold.park()
	cold.serve(t)
	v, err = cold.ss.ReplayStatus(ctx)
	if err != nil {
		t.Fatalf("replay status after restart: %v", err)
	}
	if v.Ready || v.Reason != ReplayStoreBehind {
		t.Fatalf("verdict after restart = %+v, want %q (the store records the gap, so the restart cannot launder it)", v, ReplayStoreBehind)
	}
}

// TestExistingStoreWithoutRecordIsAdopted is the migration edge: a DuckDB file
// written by a build that kept no position record holds history but no mark, and
// refusing it would wedge the deploy that ships this check on every colour
// already serving. It is adopted at the durable's current position — and the
// file created EMPTY beside it is still refused, which is the defect.
func TestExistingStoreWithoutRecordIsAdopted(t *testing.T) {
	url := startBroker(t)
	ctx := context.Background()

	// The colour that acked a range into its own file.
	warm := newColour(t, url, testConfig("colour-upgrade"))
	warm.serve(t)
	publishParity(t, warm)
	warm.waitReady(t, 20*time.Second)
	warm.park()

	// The incoming colour: a different file, written before the record existed
	// (rows, no position), reusing the warm durable as an upgrade does.
	legacy := newColour(t, url, testConfig("colour-upgrade"))
	if err := legacy.duck.SinkEvents(ctx, []storage.Event{parityEvent(9)}, storage.AppliedMark{}); err != nil {
		t.Fatalf("seed the pre-record file: %v", err)
	}
	legacy.serve(t)

	v, err := legacy.ss.ReplayStatus(ctx)
	if err != nil {
		t.Fatalf("replay status: %v", err)
	}
	if !v.Ready || v.Reason != ReplayCaughtUp {
		t.Fatalf("verdict for a file that predates the record = %+v, want caught-up", v)
	}
	pos, err := legacy.duck.AppliedPosition(ctx, legacy.ss.durableName())
	if err != nil {
		t.Fatalf("AppliedPosition: %v", err)
	}
	if !pos.Known {
		t.Fatal("the adopted file did not keep the position it was adopted at")
	}
}

// TestStoreBindingHoldsAcrossRestart is the direction that must never regress: a
// colour whose own file applied the durable's floor answers ready, restart after
// restart, on the same file.
func TestStoreBindingHoldsAcrossRestart(t *testing.T) {
	url := startBroker(t)
	ctx := context.Background()

	c := newColour(t, url, testConfig("colour-restart"))
	c.serve(t)
	publishParity(t, c)
	if v := c.waitReady(t, 20*time.Second); v.Reason != ReplayCaughtUp {
		t.Fatalf("first boot verdict = %+v, want caught-up", v)
	}
	c.park()

	// Same volume, same durable: the file holds what the floor claims.
	c.serve(t)
	v, err := c.ss.ReplayStatus(ctx)
	if err != nil {
		t.Fatalf("replay status after restart: %v", err)
	}
	if !v.Ready || v.Reason != ReplayCaughtUp {
		t.Fatalf("restart verdict = %+v, want caught-up on the file that applied the floor", v)
	}
}

// TestSettlementWithoutRowsIsRecorded pins the other half of the record: the
// settlements that carry nothing to write still move the durable's ack floor, so
// the store has to move with them. An empty batch is the reachable one — it is
// acked without ever entering a flush — and a build that recorded only its row
// writes would wake up behind its own floor and refuse a colour that lost
// nothing.
func TestSettlementWithoutRowsIsRecorded(t *testing.T) {
	url := startBroker(t)
	ctx := context.Background()

	c := newColour(t, url, testConfig("colour-empty"))
	c.serve(t)

	// A batch with no events: nothing to insert, and the message cannot be left
	// unacked or it redelivers forever.
	if _, err := c.ss.JS.Publish(ctx, c.ss.Subject, []byte("[]")); err != nil {
		t.Fatalf("publish an empty batch: %v", err)
	}
	durable := c.ss.durableName()
	deadline := time.Now().Add(20 * time.Second)
	for {
		pos, err := c.duck.AppliedPosition(ctx, durable)
		if err != nil {
			t.Fatalf("AppliedPosition: %v", err)
		}
		if pos.Seq >= 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("an empty batch settled without the store recording it: %+v", pos)
		}
		time.Sleep(20 * time.Millisecond)
	}
	c.park()

	// The floor is at 1 now and the file applied no rows at all; only the record
	// keeps the two from looking like a lost range.
	c.serve(t)
	v, err := c.ss.ReplayStatus(ctx)
	if err != nil {
		t.Fatalf("replay status after restart: %v", err)
	}
	if !v.Ready || v.Reason != ReplayCaughtUp {
		t.Fatalf("verdict after restart = %+v, want caught-up: the empty batch was settled, not lost", v)
	}
}

// unreadablePositions is a store whose own position record cannot be read.
type unreadablePositions struct{ *storage.DuckDB }

func (unreadablePositions) AppliedPosition(context.Context, string) (storage.AppliedPosition, error) {
	return storage.AppliedPosition{}, errNoPosition
}

// refuseFails is a store that cannot write a gap it just found down.
type refuseFails struct{ *storage.DuckDB }

func (refuseFails) RefusePosition(context.Context, string, uint64) error { return errNoRefusal }

var (
	errNoPosition = errors.New("position record unreadable")
	errNoRefusal  = errors.New("cannot write the refusal")
)

// TestBootRefusesToStartWhenTheBindingIsUnavailable is why the binding is
// allowed to stop the boot. This process consumes the moment it is up, and every
// ack it makes moves the durable's floor: a worker that started over a record it
// could not read — or over a gap it could not write down — would acknowledge a
// range whose provenance was never established, and its own write would then
// make the hole look like coherence. The container coming back and asking again
// costs nothing (the stream is durable and the sibling colour keeps serving);
// the silent hole is the defect.
func TestBootRefusesToStartWhenTheBindingIsUnavailable(t *testing.T) {
	url := startBroker(t)
	ctx := context.Background()

	// A colour whose file holds the range, so the durable has a floor to bind to.
	warm := newColour(t, url, testConfig("colour-unbindable"))
	warm.serve(t)
	publishParity(t, warm)
	warm.waitReady(t, 20*time.Second)
	warm.park()

	t.Run("the file's record cannot be read", func(t *testing.T) {
		// Same durable, a file that never existed: the binding has a question to
		// ask and cannot get an answer to it.
		cold := newColour(t, url, testConfig("colour-unbindable"))
		if _, err := StartJetStreamWorker(ctx, cold.ss, unreadablePositions{cold.duck}, nil); !errors.Is(err, errNoPosition) {
			t.Fatalf("boot error = %v, want the unreadable position to stop the worker", err)
		}
	})

	t.Run("the gap cannot be written down", func(t *testing.T) {
		// The parent gate's transcript again: a fresh file under a warm durable.
		// The gap is found; what must not happen is consuming over it because the
		// record of it could not be written.
		cold := newColour(t, url, testConfig("colour-unbindable"))
		if _, err := StartJetStreamWorker(ctx, cold.ss, refuseFails{cold.duck}, nil); !errors.Is(err, errNoRefusal) {
			t.Fatalf("boot error = %v, want the unwritable refusal to stop the worker", err)
		}
	})
}
