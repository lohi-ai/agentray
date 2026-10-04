package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/lohi-ai/agentray/internal/dataplane/connector"
)

// snapshotPromotionState is the per-source snapshot state machine. In
// particular, already-active is distinct from promoted: a replay may settle
// its delivery, but only promoted proves that this transaction replaced the
// serving rows and may advance readiness.
type snapshotPromotionState uint8

const (
	snapshotAwaitingPromotion snapshotPromotionState = iota
	snapshotPromoted
	snapshotAlreadyActive
	snapshotSuperseded
)

type snapshotPromotionResult struct {
	promotion *connector.SnapshotPromotion
	state     snapshotPromotionState
}

// ApplySnapshotEnvelope stages a batch or completion and attempts promotion in
// the same DuckDB writer transaction. The live table changes only when the
// exact contiguous manifest is present; AppliedMark advances atomically with
// whichever staging/promotion mutation occurred.
func (d *DuckDB) ApplySnapshotEnvelope(ctx context.Context, env connector.SnapshotEnvelope, mark AppliedMark) (*connector.SnapshotPromotion, error) {
	if err := env.Validate(); err != nil {
		return nil, err
	}
	// A large generation arrives as many small committed transactions. Fold
	// those staged row groups into the database before the all-or-nothing
	// INSERT ... SELECT so DuckDB does not carry their WAL/buffer footprint
	// into a promotion commit under the bounded 128 MB memory budget. Either a
	// completion or the last missing batch can make the generation promotable,
	// so checkpoint before every promotion attempt.
	if err := d.Checkpoint(ctx); err != nil {
		return nil, fmt.Errorf("checkpoint snapshot staging: %w", err)
	}
	var promotion *connector.SnapshotPromotion
	err := d.Write(ctx, func(tx *sql.Tx) error {
		source := sourceReceiptFromSnapshot(env)
		for _, delivery := range mark.Deliveries {
			if delivery.PublishedAt != nil {
				source.PublishedAt = delivery.PublishedAt
				break
			}
		}
		mark.Source = source
		var cleaned bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM connector_snapshot_cleanup_receipts WHERE generation=?)`, env.Generation).Scan(&cleaned); err != nil {
			return err
		}
		if cleaned {
			// Cleanup receipts are local terminal-generation tombstones. A retained
			// broker delivery may still arrive after cleanup, but it must only settle
			// its durable position and delivery identity, never recreate staging or
			// manufacture landed-source evidence for the discarded generation.
			mark.Source = nil
			if err := advancePositionTx(ctx, tx, mark); err != nil {
				return err
			}
			return recordAppliedReceiptsTx(ctx, tx, mark, time.Now().UTC())
		}
		if err := d.admitDataWrite(); err != nil {
			return err
		}
		var envelopeApplied bool
		var err error
		switch env.Kind {
		case connector.SnapshotKindBatch:
			envelopeApplied, err = stageSnapshotBatch(ctx, tx, env)
		case connector.SnapshotKindComplete:
			envelopeApplied, err = stageSnapshotCompletion(ctx, tx, env)
		}
		if err != nil {
			return err
		}
		transition, err := trySnapshotPromotion(ctx, tx, env)
		if err != nil {
			return err
		}
		promotion = transition.promotion
		if err := advancePositionTx(ctx, tx, mark); err != nil {
			return err
		}
		if transition.state != snapshotPromoted {
			// Exact batch/completion replay and already-active replay are source
			// state-machine no-ops. Keep their delivery/position settlement, but
			// do not invalidate query proof or re-run a readiness transition.
			if !envelopeApplied || transition.state == snapshotAlreadyActive {
				mark.suppressSourceMutation = true
			}
			return recordAppliedReceiptsTx(ctx, tx, mark, time.Now().UTC())
		}
		// Promotion and readiness evidence share this exact transaction: no
		// reader can observe active snapshot rows without their receipt.
		mark.Source = nil
		if err := recordAppliedReceiptsTx(ctx, tx, mark, promotion.PromotedAt); err != nil {
			return err
		}
		source.Complete, source.Promoted = true, true
		source.CaptureFinishedAt = &promotion.CaptureFinishedAt
		expectedBatches, expectedRows := uint64(promotion.ExpectedBatches), uint64(promotion.ExpectedRows)
		source.ExpectedBatches, source.ExpectedRows = &expectedBatches, &expectedRows
		return RecordSnapshotPromotionTx(ctx, tx, SnapshotPromotion{SourceReceiptMark: *source, PromotedAt: promotion.PromotedAt})
	})
	return promotion, err
}

func sourceReceiptFromSnapshot(env connector.SnapshotEnvelope) *SourceReceiptMark {
	seq := uint64(env.GenerationSeq)
	mark := &SourceReceiptMark{ProjectID: env.ProjectID, ConnectorID: env.ConnectorID, Table: env.Table,
		SyncID: env.SyncID, RunID: env.RunID, Generation: env.Generation, GenerationSeq: seq,
		BindingDigest: env.BindingDigest, CaptureStartedAt: &env.CaptureStartedAt}
	if env.Kind == connector.SnapshotKindBatch {
		index := uint64(env.BatchIndex)
		mark.BatchID, mark.BatchIndex, mark.PayloadSHA256 = env.BatchID, &index, env.PayloadSHA256
	} else {
		mark.Complete, mark.CaptureFinishedAt = true, env.CaptureFinishedAt
		expectedBatches, expectedRows := uint64(env.ExpectedBatches), uint64(env.ExpectedRows)
		mark.ExpectedBatches, mark.ExpectedRows = &expectedBatches, &expectedRows
	}
	return mark
}

func stageSnapshotBatch(ctx context.Context, tx *sql.Tx, env connector.SnapshotEnvelope) (bool, error) {
	if err := validateSnapshotGenerationIdentity(ctx, tx, env); err != nil {
		return false, err
	}
	var existingIndex, existingCount int64
	var existingHash string
	err := tx.QueryRowContext(ctx, `SELECT batch_index, payload_sha256, row_count FROM connector_snapshot_batches
WHERE project_id=? AND connector_id=? AND table_name=? AND generation=? AND batch_id=?`,
		env.ProjectID, env.ConnectorID, env.Table, env.Generation, env.BatchID).Scan(&existingIndex, &existingHash, &existingCount)
	if err == nil {
		if existingIndex != env.BatchIndex || existingHash != env.PayloadSHA256 || existingCount != int64(len(env.Rows)) {
			return false, fmt.Errorf("snapshot batch replay conflicts with staged payload")
		}
		return false, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	var otherID, otherHash string
	err = tx.QueryRowContext(ctx, `SELECT batch_id, payload_sha256 FROM connector_snapshot_batches
WHERE project_id=? AND connector_id=? AND table_name=? AND generation=? AND batch_index=?`,
		env.ProjectID, env.ConnectorID, env.Table, env.Generation, env.BatchIndex).Scan(&otherID, &otherHash)
	if err == nil {
		return false, fmt.Errorf("snapshot batch index conflicts with %s", otherID)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}

	for _, row := range env.Rows {
		var priorBatch string
		err := tx.QueryRowContext(ctx, `SELECT batch_id FROM connector_snapshot_rows
WHERE project_id=? AND connector_id=? AND table_name=? AND generation=? AND row_key=?`,
			env.ProjectID, env.ConnectorID, env.Table, env.Generation, row.Key).Scan(&priorBatch)
		if err == nil {
			// The source row key is intentionally absent: ingestion logs this
			// category on retries and dead-letter settlement.
			return false, errors.New("snapshot batch rows were rejected by staging")
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return false, err
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO connector_snapshot_batches
(project_id,connector_id,table_name,sync_id,generation,generation_seq,binding_digest,batch_id,batch_index,payload_sha256,row_count,capture_started_at,run_id)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`, env.ProjectID, env.ConnectorID, env.Table, env.SyncID, env.Generation, env.GenerationSeq,
		env.BindingDigest, env.BatchID, env.BatchIndex, env.PayloadSHA256, len(env.Rows), env.CaptureStartedAt, env.RunID); err != nil {
		return false, err
	}
	if len(env.Rows) == 0 {
		return true, nil
	}
	const cols = 7
	var query strings.Builder
	query.WriteString(`INSERT INTO connector_snapshot_rows (project_id,connector_id,table_name,generation,batch_id,row_key,data) VALUES `)
	args := make([]any, 0, len(env.Rows)*cols)
	for i, row := range env.Rows {
		if i > 0 {
			query.WriteByte(',')
		}
		query.WriteString("(?,?,?,?,?,?,?)")
		args = append(args, env.ProjectID, env.ConnectorID, env.Table, env.Generation, env.BatchID, row.Key, string(row.Data))
	}
	if _, err = tx.ExecContext(ctx, query.String(), args...); err != nil {
		// DuckDB constraint messages include the rejected row_key. This error is
		// logged by the ingestion settler, so keep the staging category while
		// dropping database details that may contain source PII.
		return false, errors.New("snapshot batch rows were rejected by staging")
	}
	return true, nil
}

func stageSnapshotCompletion(ctx context.Context, tx *sql.Tx, env connector.SnapshotEnvelope) (bool, error) {
	if err := validateSnapshotGenerationIdentity(ctx, tx, env); err != nil {
		return false, err
	}
	var seq, batches, rows int64
	var digest, binding string
	err := tx.QueryRowContext(ctx, `SELECT generation_seq, expected_batches, expected_rows, batch_manifest_sha256, binding_digest
FROM connector_snapshot_completions WHERE project_id=? AND connector_id=? AND table_name=? AND generation=?`,
		env.ProjectID, env.ConnectorID, env.Table, env.Generation).Scan(&seq, &batches, &rows, &digest, &binding)
	if err == nil {
		if seq != env.GenerationSeq || batches != env.ExpectedBatches || rows != env.ExpectedRows || digest != env.BatchManifestSHA256 || binding != env.BindingDigest {
			return false, fmt.Errorf("snapshot completion replay conflicts with staged marker")
		}
		return false, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO connector_snapshot_completions
(project_id,connector_id,table_name,sync_id,generation,generation_seq,binding_digest,expected_batches,expected_rows,batch_manifest_sha256,capture_started_at,capture_finished_at,run_id)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`, env.ProjectID, env.ConnectorID, env.Table, env.SyncID, env.Generation, env.GenerationSeq,
		env.BindingDigest, env.ExpectedBatches, env.ExpectedRows, env.BatchManifestSHA256, env.CaptureStartedAt, *env.CaptureFinishedAt, env.RunID)
	return true, err
}

func validateSnapshotGenerationIdentity(ctx context.Context, tx *sql.Tx, env connector.SnapshotEnvelope) error {
	var syncID, binding string
	var seq int64
	var started time.Time
	err := tx.QueryRowContext(ctx, `SELECT sync_id::VARCHAR,generation_seq,binding_digest,capture_started_at FROM connector_snapshot_batches
WHERE project_id=? AND connector_id=? AND table_name=? AND generation=? LIMIT 1`, env.ProjectID, env.ConnectorID, env.Table, env.Generation).Scan(&syncID, &seq, &binding, &started)
	if errors.Is(err, sql.ErrNoRows) {
		err = tx.QueryRowContext(ctx, `SELECT sync_id::VARCHAR,generation_seq,binding_digest,capture_started_at FROM connector_snapshot_completions
WHERE project_id=? AND connector_id=? AND table_name=? AND generation=?`, env.ProjectID, env.ConnectorID, env.Table, env.Generation).Scan(&syncID, &seq, &binding, &started)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
	}
	if err != nil {
		return err
	}
	if syncID != env.SyncID || seq != env.GenerationSeq || binding != env.BindingDigest || !started.Equal(env.CaptureStartedAt) {
		return fmt.Errorf("snapshot generation identity conflicts with staged data")
	}
	return nil
}

func trySnapshotPromotion(ctx context.Context, tx *sql.Tx, env connector.SnapshotEnvelope) (snapshotPromotionResult, error) {
	var complete connector.SnapshotPromotion
	err := tx.QueryRowContext(ctx, `SELECT project_id::VARCHAR,connector_id::VARCHAR,table_name,sync_id::VARCHAR,generation::VARCHAR,generation_seq,
binding_digest,capture_started_at,capture_finished_at,expected_batches,expected_rows,batch_manifest_sha256
FROM connector_snapshot_completions WHERE project_id=? AND connector_id=? AND table_name=? AND generation=?`,
		env.ProjectID, env.ConnectorID, env.Table, env.Generation).Scan(&complete.ProjectID, &complete.ConnectorID, &complete.Table, &complete.SyncID,
		&complete.Generation, &complete.GenerationSeq, &complete.BindingDigest, &complete.CaptureStartedAt, &complete.CaptureFinishedAt,
		&complete.ExpectedBatches, &complete.ExpectedRows, &complete.BatchManifestSHA256)
	if errors.Is(err, sql.ErrNoRows) {
		return snapshotPromotionResult{state: snapshotAwaitingPromotion}, nil
	}
	if err != nil {
		return snapshotPromotionResult{}, err
	}

	var activeSeq int64
	var activeGeneration string
	err = tx.QueryRowContext(ctx, `SELECT generation_seq,generation::VARCHAR FROM connector_snapshot_promotions
WHERE project_id=? AND connector_id=? AND table_name=?`, env.ProjectID, env.ConnectorID, env.Table).Scan(&activeSeq, &activeGeneration)
	if err == nil && activeSeq >= complete.GenerationSeq {
		if activeSeq == complete.GenerationSeq && activeGeneration == complete.Generation {
			_ = tx.QueryRowContext(ctx, `SELECT promoted_at FROM connector_snapshot_promotions WHERE project_id=? AND connector_id=? AND table_name=?`, env.ProjectID, env.ConnectorID, env.Table).Scan(&complete.PromotedAt)
			return snapshotPromotionResult{promotion: &complete, state: snapshotAlreadyActive}, nil
		}
		return snapshotPromotionResult{state: snapshotSuperseded}, nil
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return snapshotPromotionResult{}, err
	}

	rows, err := tx.QueryContext(ctx, `SELECT batch_index,batch_id,payload_sha256,row_count FROM connector_snapshot_batches
WHERE project_id=? AND connector_id=? AND table_name=? AND generation=? ORDER BY batch_index`, env.ProjectID, env.ConnectorID, env.Table, env.Generation)
	if err != nil {
		return snapshotPromotionResult{}, err
	}
	entries := make([]connector.SnapshotManifestEntry, 0)
	var rowCount int64
	for rows.Next() {
		var entry connector.SnapshotManifestEntry
		if err := rows.Scan(&entry.Index, &entry.BatchID, &entry.PayloadSHA256, &entry.RowCount); err != nil {
			_ = rows.Close()
			return snapshotPromotionResult{}, err
		}
		entries = append(entries, entry)
		rowCount += int64(entry.RowCount)
	}
	if err := rows.Close(); err != nil {
		return snapshotPromotionResult{}, err
	}
	if int64(len(entries)) != complete.ExpectedBatches || rowCount != complete.ExpectedRows {
		return snapshotPromotionResult{state: snapshotAwaitingPromotion}, nil
	}
	digest, err := connector.SnapshotManifestDigest(entries)
	if err != nil || digest != complete.BatchManifestSHA256 {
		return snapshotPromotionResult{state: snapshotAwaitingPromotion}, nil
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM external_rows WHERE project_id=? AND connector_id=? AND table_name=?`, env.ProjectID, env.ConnectorID, env.Table); err != nil {
		return snapshotPromotionResult{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO external_rows (project_id,connector_id,table_name,row_key,cursor,data,synced_at)
SELECT project_id,connector_id,table_name,row_key,'',data,now() FROM connector_snapshot_rows
WHERE project_id=? AND connector_id=? AND table_name=? AND generation=?`, env.ProjectID, env.ConnectorID, env.Table, env.Generation); err != nil {
		return snapshotPromotionResult{}, err
	}
	complete.PromotedAt = time.Now().UTC()
	if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO connector_snapshot_promotions
(project_id,connector_id,table_name,sync_id,generation,generation_seq,binding_digest,expected_batches,expected_rows,batch_manifest_sha256,capture_started_at,capture_finished_at,promoted_at)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`, complete.ProjectID, complete.ConnectorID, complete.Table, complete.SyncID, complete.Generation, complete.GenerationSeq,
		complete.BindingDigest, complete.ExpectedBatches, complete.ExpectedRows, complete.BatchManifestSHA256, complete.CaptureStartedAt, complete.CaptureFinishedAt, complete.PromotedAt); err != nil {
		return snapshotPromotionResult{}, err
	}
	return snapshotPromotionResult{promotion: &complete, state: snapshotPromoted}, nil
}

func (d *DuckDB) SnapshotPromotion(ctx context.Context, projectID, connectorID, table string) (connector.SnapshotPromotion, error) {
	var out connector.SnapshotPromotion
	err := d.Read(ctx, func(conn *sql.Conn) error {
		return conn.QueryRowContext(ctx, `SELECT project_id::VARCHAR,connector_id::VARCHAR,table_name,sync_id::VARCHAR,generation::VARCHAR,generation_seq,binding_digest,
capture_started_at,capture_finished_at,promoted_at,expected_batches,expected_rows,batch_manifest_sha256 FROM connector_snapshot_promotions
WHERE project_id=? AND connector_id=? AND table_name=?`, projectID, connectorID, table).Scan(&out.ProjectID, &out.ConnectorID, &out.Table, &out.SyncID,
			&out.Generation, &out.GenerationSeq, &out.BindingDigest, &out.CaptureStartedAt, &out.CaptureFinishedAt, &out.PromotedAt,
			&out.ExpectedBatches, &out.ExpectedRows, &out.BatchManifestSHA256)
	})
	return out, err
}

func (d *DuckDB) snapshotGenerationActive(ctx context.Context, generation string) (bool, error) {
	var active bool
	err := d.Read(ctx, func(conn *sql.Conn) error {
		return conn.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM connector_snapshot_promotions WHERE generation=?)`, generation).Scan(&active)
	})
	return active, err
}

func (d *DuckDB) snapshotGenerationCleanupComplete(ctx context.Context, generation string) (bool, error) {
	var complete bool
	err := d.Read(ctx, func(conn *sql.Conn) error {
		return conn.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM connector_snapshot_cleanup_receipts WHERE generation=?)`, generation).Scan(&complete)
	})
	return complete, err
}

func (d *DuckDB) storeIdentity(ctx context.Context) (string, error) {
	var id string
	err := d.Read(ctx, func(conn *sql.Conn) error {
		return conn.QueryRowContext(ctx, `SELECT store_id::VARCHAR FROM data_store_identity WHERE slot=1`).Scan(&id)
	})
	return id, err
}

func (d *DuckDB) deleteSnapshotStagingChunk(ctx context.Context, generation string, limit int) (deleted int, more bool, err error) {
	err = d.Write(ctx, func(tx *sql.Tx) error {
		var active bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM connector_snapshot_promotions WHERE generation=?)`, generation).Scan(&active); err != nil {
			return err
		}
		if active {
			return nil
		}
		result, err := tx.ExecContext(ctx, `DELETE FROM connector_snapshot_rows WHERE rowid IN
(SELECT rowid FROM connector_snapshot_rows WHERE generation=? LIMIT ?)`, generation, limit)
		if err != nil {
			return err
		}
		if n, rowsErr := result.RowsAffected(); rowsErr == nil {
			deleted = int(n)
		}
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM connector_snapshot_rows WHERE generation=?)`, generation).Scan(&more); err != nil {
			return err
		}
		if !more {
			for _, stmt := range []string{
				`DELETE FROM connector_snapshot_batches WHERE generation=?`,
				`DELETE FROM connector_snapshot_completions WHERE generation=?`,
			} {
				if _, err := tx.ExecContext(ctx, stmt, generation); err != nil {
					return err
				}
			}
			if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO connector_snapshot_cleanup_receipts(generation,cleaned_at) VALUES(?,now())`, generation); err != nil {
				return err
			}
		}
		return nil
	})
	return deleted, more, err
}
