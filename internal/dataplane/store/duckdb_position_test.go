package storage

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/lohi-ai/agentray/internal/dataplane/connector"
)

// The position record is what lets /readyz answer for the DuckDB file rather
// than for the durable's own arithmetic, so its load-bearing properties are
// pinned here: it commits with the rows it describes (a mark that landed ahead
// of its rows would vouch for rows the file does not hold), both ingest sinks
// record it (one durable consumer carries events AND connector rows), and a
// refusal written into the store cannot be laundered by later traffic.

func positionOf(t *testing.T, d *DuckDB, durable string) AppliedPosition {
	t.Helper()
	pos, err := d.AppliedPosition(context.Background(), durable)
	if err != nil {
		t.Fatalf("AppliedPosition(%q): %v", durable, err)
	}
	return pos
}

// A durable write records the position it landed, monotonically: a coalesced
// batch and a redelivery of an older message must not walk the record back.
func TestAppliedPositionRecordsTheDurableWrite(t *testing.T) {
	ctx := context.Background()
	d := openTestDuckDB(t)
	projectID := uuid.NewString()

	if got := positionOf(t, d, "colour-a"); got.Known {
		t.Fatalf("a store that has written nothing knows a position: %+v", got)
	}
	if err := d.SinkEvents(ctx, []Event{duckEvent(projectID, uuid.NewString(), "reader", time.Now())},
		AppliedMark{Durable: "colour-a", Seq: 7}); err != nil {
		t.Fatalf("SinkEvents: %v", err)
	}
	got := positionOf(t, d, "colour-a")
	if !got.Known || got.Seq != 7 || !got.HasIngest {
		t.Fatalf("after a marked write = %+v, want known seq 7 over ingested rows", got)
	}

	// A redelivered older message, and a store that is told about a different
	// durable, neither move this one's record.
	if err := d.SinkEvents(ctx, []Event{duckEvent(projectID, uuid.NewString(), "reader", time.Now())},
		AppliedMark{Durable: "colour-a", Seq: 3}); err != nil {
		t.Fatalf("SinkEvents (older mark): %v", err)
	}
	if err := d.SinkEvents(ctx, []Event{duckEvent(projectID, uuid.NewString(), "reader", time.Now())},
		AppliedMark{Durable: "colour-b", Seq: 9}); err != nil {
		t.Fatalf("SinkEvents (other durable): %v", err)
	}
	if got := positionOf(t, d, "colour-a"); got.Seq != 7 {
		t.Fatalf("record after an older redelivery = %d, want 7", got.Seq)
	}
	if got := positionOf(t, d, "colour-b"); got.Seq != 9 {
		t.Fatalf("record for the other durable = %d, want 9", got.Seq)
	}
}

// The mark commits with the rows or not at all. A transaction that fails has
// applied no rows, so it must not leave a position claiming otherwise — that
// would be the defect with the sign flipped: a store vouching for rows it
// never wrote.
func TestAppliedPositionRollsBackWithItsRows(t *testing.T) {
	ctx := context.Background()
	d := openTestDuckDB(t)

	err := d.SinkEvents(ctx, []Event{{ProjectID: "not-a-uuid", EventID: uuid.NewString()}},
		AppliedMark{Durable: "colour-a", Seq: 11})
	if err == nil {
		t.Fatal("SinkEvents accepted an unparseable project id")
	}
	if got := positionOf(t, d, "colour-a"); got.Known || got.Seq != 0 || got.HasIngest {
		t.Fatalf("a rolled-back write left a position behind: %+v", got)
	}
}

// Connector rows ride the same durable consumer as events, so they have to
// record the same position: a colour whose only traffic is a connector sync
// would otherwise look like a store that has applied nothing.
func TestConnectorRowsRecordThePosition(t *testing.T) {
	ctx := context.Background()
	d := openTestDuckDB(t)

	if err := d.InsertExternalRows(ctx, uuid.NewString(), uuid.NewString(), "users",
		[]connector.LandedRow{{Key: "k1", Cursor: "c1", DataJSON: `{"id":"k1"}`}},
		AppliedMark{Durable: "colour-a", Seq: 5}); err != nil {
		t.Fatalf("InsertExternalRows: %v", err)
	}
	got := positionOf(t, d, "colour-a")
	if !got.Known || got.Seq != 5 {
		t.Fatalf("connector-only store = %+v, want known seq 5", got)
	}
	if !got.HasIngest {
		t.Fatal("connector rows are ingest: a file holding only them must not look empty (that is the adoption edge)")
	}
}

// A refusal is the record of a gap that waiting cannot close, so traffic
// applied after it must not erase it — a restart reads the store, not the
// process that found the loss.
func TestRefusedPositionSurvivesLaterTraffic(t *testing.T) {
	ctx := context.Background()
	d := openTestDuckDB(t)

	if err := d.RefusePosition(ctx, "colour-a", 12); err != nil {
		t.Fatalf("RefusePosition: %v", err)
	}
	// The colour keeps ingesting: the durable's floor advances past the hole,
	// which is exactly the state that used to read as caught-up.
	if err := d.SinkEvents(ctx, []Event{duckEvent(uuid.NewString(), uuid.NewString(), "reader", time.Now())},
		AppliedMark{Durable: "colour-a", Seq: 20}); err != nil {
		t.Fatalf("SinkEvents: %v", err)
	}
	got := positionOf(t, d, "colour-a")
	if got.Seq != 20 {
		t.Fatalf("record = %d, want the traffic applied (20)", got.Seq)
	}
	if got.RefusedMissing != 12 {
		t.Fatalf("refusal = %d, want the 12 acknowledged messages this file never applied", got.RefusedMissing)
	}

	// A second refusal does not overwrite the first with a smaller number: the
	// loss is what it was.
	if err := d.RefusePosition(ctx, "colour-a", 4); err != nil {
		t.Fatalf("RefusePosition (again): %v", err)
	}
	if got := positionOf(t, d, "colour-a"); got.RefusedMissing != 12 {
		t.Fatalf("refusal after a second boot = %d, want 12", got.RefusedMissing)
	}
}

// Adoption is the one-time migration of a file written by a build that kept no
// position: it records where the durable stood so the file is not refused for
// the history it demonstrably holds. The record it writes is the durable's
// current mark, which is what the readiness binding has already verified.
func TestAdoptPositionStartsTheRecord(t *testing.T) {
	ctx := context.Background()
	d := openTestDuckDB(t)

	if err := d.AdoptPosition(ctx, "colour-a", 42); err != nil {
		t.Fatalf("AdoptPosition: %v", err)
	}
	got := positionOf(t, d, "colour-a")
	if !got.Known || got.Seq != 42 || got.HasIngest {
		t.Fatalf("adopted position = %+v, want known seq 42 over a file with no rows of its own", got)
	}
}

// The record survives a close and reopen: it is the file's memory, not the
// process's.
func TestAppliedPositionSurvivesReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "analytics.duckdb")

	d, err := OpenDuckDB(ctx, path)
	if err != nil {
		t.Fatalf("OpenDuckDB: %v", err)
	}
	if err := d.RefusePosition(ctx, "colour-a", 6); err != nil {
		t.Fatalf("RefusePosition: %v", err)
	}
	if err := d.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reopened, err := OpenDuckDB(ctx, path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	if got := positionOf(t, reopened, "colour-a"); got.RefusedMissing != 6 {
		t.Fatalf("refusal after reopen = %+v, want 6 missing", got)
	}
}

// RecordPosition is the row-less settlement path: an empty batch or a poison
// payload is acked (or terminated) without writing anything, and both move the
// durable's ack floor. It has to move the record the same way — monotonically,
// over no rows, and not at all for a write that did not come off the stream.
func TestRecordPositionCoversSettlementsWithoutRows(t *testing.T) {
	ctx := context.Background()
	d := openTestDuckDB(t)

	if err := d.RecordPosition(ctx, AppliedMark{Durable: "colour-empty", Seq: 4}); err != nil {
		t.Fatalf("RecordPosition: %v", err)
	}
	got := positionOf(t, d, "colour-empty")
	if !got.Known || got.Seq != 4 {
		t.Fatalf("after a row-less settlement = %+v, want known seq 4", got)
	}
	if got.HasIngest {
		t.Fatal("a row-less settlement claims the file holds ingested rows")
	}
	if n := duckCount(t, d, `SELECT count(*) FROM events`); n != 0 {
		t.Fatalf("events rows = %d, want none: a settlement with no rows must write none", n)
	}

	// Monotone, like the marked write: a redelivered older delivery cannot walk
	// the record back, and a mark that came off nothing (the core-NATS path)
	// records nothing at all.
	if err := d.RecordPosition(ctx, AppliedMark{Durable: "colour-empty", Seq: 2}); err != nil {
		t.Fatalf("RecordPosition (older): %v", err)
	}
	if err := d.RecordPosition(ctx, AppliedMark{Seq: 99}); err != nil {
		t.Fatalf("RecordPosition (no durable): %v", err)
	}
	if got := positionOf(t, d, "colour-empty"); got.Seq != 4 {
		t.Fatalf("record after an older and an unbound settlement = %d, want 4", got.Seq)
	}
}
