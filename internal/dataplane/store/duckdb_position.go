package storage

import (
	"context"
	"database/sql"
)

// This file is the store-side half of the readiness contract. /readyz answers
// one question for the blue-green gate — may a deploy point traffic at this
// colour? — and the honest form of that question is "does THIS DuckDB file hold
// what the durable behind it has already acknowledged?".
//
// The durable's ack floor alone cannot answer it, which is the defect this
// record closes. A durable outlives the file its acks were made against: an
// operator recreating a colour's volume, or pointing a colour at a new
// DUCKDB_PATH, leaves a consumer whose floor describes a store that no longer
// exists. Sequences published after that boot are applied normally, so the file
// is not empty — it is missing exactly the range the floor claims, and
// "applied >= head" is true of it.
//
// So each store records, INSIDE the file the claim is about, how far its own
// writes have carried it along the durable stream. A record that cannot be
// separated from the store it protects cannot go missing with it: a fresh file
// has no record, which is what makes the lie visible. (The retention-loss
// marker takes the opposite approach — written beside the file — for the
// opposite reason: a store too damaged to be written must still be able to
// record its own loss.)
//
// Recording happens inside the ingest transaction, so a write that lands the
// rows lands the position with them or neither; acking a message therefore
// always implies the position covering it is durable.

// AppliedMark is the durable-stream position one write lands. Seq is the
// CONSUMER-side delivery sequence (jetstream.MsgMetadata.Sequence.Consumer), not
// the stream sequence: the consumer sequence counts only the messages this
// colour was offered, so it stays comparable when the stream is shared with
// another environment and its sequences are interleaved with a neighbour's.
//
// Durable empty means "this write did not come off the durable stream" — the
// core-NATS fallback, the pipeline self-metrics path, and test fixtures — and
// nothing is recorded.
type AppliedMark struct {
	Durable string
	Seq     uint64
}

// AppliedPosition is what one store remembers about a durable's progress.
//
//   - Known is false until this file records a position: its own first durable
//     write, or the adoption of a file that predates this record (below). An
//     unknown position proves nothing about the store.
//   - Seq is the highest consumer sequence this file's own writes have applied.
//   - RefusedMissing is non-zero once a boot proved the durable's ack floor
//     lay above Seq: the number of acknowledged messages this file cannot show.
//     It is the refusal the readiness gate reads, recorded in the store so it
//     survives the process that found it.
//   - HasIngest is whether the file holds any ingested row at all — the
//     evidence that separates a file predating this record from one created
//     empty under a warm durable.
type AppliedPosition struct {
	Known          bool
	Seq            uint64
	RefusedMissing uint64
	HasIngest      bool
}

// AppliedPosition reads this file's record for one durable. A store that has
// never written a position answers Known=false; that is not an error.
func (d *DuckDB) AppliedPosition(ctx context.Context, durable string) (AppliedPosition, error) {
	var pos AppliedPosition
	err := d.Read(ctx, func(conn *sql.Conn) error {
		row := conn.QueryRowContext(ctx, `
SELECT
	(SELECT applied_seq FROM ingest_position WHERE durable = ?),
	(SELECT refused_missing FROM ingest_position WHERE durable = ?),
	EXISTS (SELECT 1 FROM events) OR EXISTS (SELECT 1 FROM external_rows)`,
			durable, durable)
		var seq, refused sql.Null[uint64]
		if err := row.Scan(&seq, &refused, &pos.HasIngest); err != nil {
			return err
		}
		pos.Known = seq.Valid
		pos.Seq = seq.V
		if refused.Valid {
			pos.RefusedMissing = refused.V
		}
		return nil
	})
	if err != nil {
		return AppliedPosition{}, err
	}
	return pos, nil
}

// AdoptPosition records where a file that predates this record stands on the
// durable stream: it is the one-time adoption of a store written by a build
// that kept no position, and the readiness gate writes it only for a file that
// already holds ingested rows. A file created empty is never adopted — that is
// the case the record exists to catch.
func (d *DuckDB) AdoptPosition(ctx context.Context, durable string, seq uint64) error {
	return d.Write(ctx, func(tx *sql.Tx) error {
		return advancePositionTx(ctx, tx, AppliedMark{Durable: durable, Seq: seq})
	})
}

// RefusePosition records a gap a boot proved: the durable's ack floor lay
// `missing` messages above what this file had applied when it was bound. The
// first refusal stands — a later boot that finds the same store with traffic
// applied must not launder the hole it was refused for.
func (d *DuckDB) RefusePosition(ctx context.Context, durable string, missing uint64) error {
	return d.Write(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			`INSERT OR IGNORE INTO ingest_position (durable, applied_seq, refused_missing) VALUES (?, 0, ?)`,
			durable, missing); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx,
			`UPDATE ingest_position SET refused_missing = ?, updated_at = now() WHERE durable = ? AND refused_missing = 0`,
			missing, durable)
		return err
	})
}

// RecordPosition moves one durable's recorded position forward for a delivery
// that settles WITHOUT a row write: an empty batch, or a poison payload that
// leaves via the dead-letter queue. Acking and terminating both advance the
// consumer's ack floor, so a store that recorded only its row writes would fall
// behind a floor built of deliveries it deliberately wrote nothing for — and the
// next boot would read that difference as a range it had lost. That is a false
// refusal of a healthy colour, so those settlements record here.
//
// Its own transaction, because there are no rows to be atomic with. The row
// paths keep the position INSIDE the ingest transaction (advancePositionTx):
// there, a record that could outlive its rows would be the very lie this file
// exists to catch.
func (d *DuckDB) RecordPosition(ctx context.Context, mark AppliedMark) error {
	if mark.Durable == "" {
		return nil
	}
	return d.Write(ctx, func(tx *sql.Tx) error {
		return advancePositionTx(ctx, tx, mark)
	})
}

// advancePositionTx moves one durable's recorded position forward. Monotone on
// purpose: coalesced batches, redeliveries and connector batches arrive out of
// order, and the only claim the record makes is how far this file's writes have
// reached.
func advancePositionTx(ctx context.Context, tx *sql.Tx, mark AppliedMark) error {
	if mark.Durable == "" {
		return nil
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT OR IGNORE INTO ingest_position (durable, applied_seq, refused_missing) VALUES (?, ?, 0)`,
		mark.Durable, mark.Seq); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx,
		`UPDATE ingest_position SET applied_seq = ?, updated_at = now() WHERE durable = ? AND applied_seq < ?`,
		mark.Seq, mark.Durable, mark.Seq)
	return err
}
