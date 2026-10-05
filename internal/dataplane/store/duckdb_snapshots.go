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

// ApplySnapshotEnvelope stages a batch or completion and attempts promotion.
// The live table changes only when the exact contiguous manifest is present;
// promotion, AppliedMark and readiness evidence share one writer transaction.
// A final batch is checkpointed first so its idempotent staging does not keep
// transaction-local row groups resident during the bounded promotion commit.
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
	if env.Kind == connector.SnapshotKindBatch {
		return d.applySnapshotBatchEnvelope(ctx, env, mark)
	}
	var promotion *connector.SnapshotPromotion
	err := d.Write(ctx, func(tx *sql.Tx) error {
		var cleaned bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM connector_snapshot_cleanup_receipts WHERE generation=?)`, env.Generation).Scan(&cleaned); err != nil {
			return err
		}
		if cleaned {
			return settleCleanedSnapshotEnvelopeTx(ctx, tx, mark)
		}
		if err := d.admitDataWrite(); err != nil {
			return err
		}
		envelopeApplied, err := stageSnapshotCompletion(ctx, tx, env)
		if err != nil {
			return err
		}
		promotion, err = applySnapshotTransitionTx(ctx, tx, env, mark, envelopeApplied)
		return err
	})
	return promotion, err
}

// applySnapshotBatchEnvelope commits idempotent staging before promotion. A
// final batch otherwise makes DuckDB scan a mixed committed/transaction-local
// million-row relation and leaves no block for commit under the fixed 128 MB
// limit. Promotion, position, deliveries and readiness still commit atomically;
// a crash between the two transactions is safe because redelivery observes an
// exact staged replay and retries the promotion.
func (d *DuckDB) applySnapshotBatchEnvelope(ctx context.Context, env connector.SnapshotEnvelope, mark AppliedMark) (*connector.SnapshotPromotion, error) {
	var envelopeApplied, cleaned, needsPromotionCheckpoint bool
	var promotion *connector.SnapshotPromotion
	if err := d.Write(ctx, func(tx *sql.Tx) error {
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM connector_snapshot_cleanup_receipts WHERE generation=?)`, env.Generation).Scan(&cleaned); err != nil {
			return err
		}
		if cleaned {
			return settleCleanedSnapshotEnvelopeTx(ctx, tx, mark)
		}
		if err := d.admitDataWrite(); err != nil {
			return err
		}
		var err error
		envelopeApplied, err = stageSnapshotBatch(ctx, tx, env)
		if err != nil {
			return err
		}
		needsPromotionCheckpoint, err = snapshotBatchMayCompleteGenerationTx(ctx, tx, env)
		if err != nil || needsPromotionCheckpoint {
			return err
		}
		promotion, err = applySnapshotTransitionTx(ctx, tx, env, mark, envelopeApplied)
		return err
	}); err != nil || cleaned {
		return nil, err
	}
	if !needsPromotionCheckpoint {
		return promotion, nil
	}
	if err := d.Checkpoint(ctx); err != nil {
		if envelopeApplied {
			if cleanupErr := d.removeStagedSnapshotBatch(context.WithoutCancel(ctx), env); cleanupErr != nil {
				return nil, errors.Join(fmt.Errorf("checkpoint snapshot batch: %w", err), fmt.Errorf("rollback staged snapshot batch: %w", cleanupErr))
			}
		}
		return nil, fmt.Errorf("checkpoint snapshot batch: %w", err)
	}
	err := d.Write(ctx, func(tx *sql.Tx) error {
		var err error
		promotion, err = applySnapshotTransitionTx(ctx, tx, env, mark, envelopeApplied)
		return err
	})
	if err != nil && envelopeApplied {
		cleanupErr := d.removeStagedSnapshotBatch(context.WithoutCancel(ctx), env)
		if cleanupErr != nil {
			return nil, errors.Join(err, fmt.Errorf("rollback staged snapshot batch: %w", cleanupErr))
		}
	}
	return promotion, err
}

func snapshotBatchMayCompleteGenerationTx(ctx context.Context, tx *sql.Tx, env connector.SnapshotEnvelope) (bool, error) {
	var expectedBatches, expectedRows int64
	err := tx.QueryRowContext(ctx, `SELECT expected_batches,expected_rows FROM connector_snapshot_completions
WHERE project_id=? AND connector_id=? AND table_name=? AND generation=?`,
		env.ProjectID, env.ConnectorID, env.Table, env.Generation).Scan(&expectedBatches, &expectedRows)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var activeSeq int64
	err = tx.QueryRowContext(ctx, `SELECT generation_seq FROM connector_snapshot_promotions
WHERE project_id=? AND connector_id=? AND table_name=?`, env.ProjectID, env.ConnectorID, env.Table).Scan(&activeSeq)
	if err == nil && activeSeq >= env.GenerationSeq {
		return false, nil
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	var batches, rows int64
	if err := tx.QueryRowContext(ctx, `SELECT count(*),coalesce(sum(row_count),0) FROM connector_snapshot_batches
WHERE project_id=? AND connector_id=? AND table_name=? AND generation=?`,
		env.ProjectID, env.ConnectorID, env.Table, env.Generation).Scan(&batches, &rows); err != nil {
		return false, err
	}
	return batches == expectedBatches && rows == expectedRows, nil
}

func settleCleanedSnapshotEnvelopeTx(ctx context.Context, tx *sql.Tx, mark AppliedMark) error {
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

func applySnapshotTransitionTx(ctx context.Context, tx *sql.Tx, env connector.SnapshotEnvelope, mark AppliedMark, envelopeApplied bool) (*connector.SnapshotPromotion, error) {
	source := sourceReceiptFromSnapshot(env)
	for _, delivery := range mark.Deliveries {
		if delivery.PublishedAt != nil {
			source.PublishedAt = delivery.PublishedAt
			break
		}
	}
	mark.Source = source
	transition, err := trySnapshotPromotion(ctx, tx, env)
	if err != nil {
		return nil, err
	}
	if err := advancePositionTx(ctx, tx, mark); err != nil {
		return nil, err
	}
	if transition.state != snapshotPromoted {
		// Exact batch/completion replay, an already-active replay and a stale
		// generation are source-state no-ops. Keep delivery/position settlement
		// without invalidating query proof or regressing the current receipt.
		if !envelopeApplied || transition.state == snapshotAlreadyActive || transition.state == snapshotSuperseded {
			mark.suppressSourceMutation = true
		}
		return transition.promotion, recordAppliedReceiptsTx(ctx, tx, mark, time.Now().UTC())
	}
	// Promotion and readiness evidence share this exact transaction: no reader
	// can observe active snapshot rows without their receipt.
	mark.Source = nil
	if err := recordAppliedReceiptsTx(ctx, tx, mark, transition.promotion.PromotedAt); err != nil {
		return nil, err
	}
	source.Complete, source.Promoted = true, true
	source.CaptureFinishedAt = &transition.promotion.CaptureFinishedAt
	expectedBatches, expectedRows := uint64(transition.promotion.ExpectedBatches), uint64(transition.promotion.ExpectedRows)
	source.ExpectedBatches, source.ExpectedRows = &expectedBatches, &expectedRows
	if err := RecordSnapshotPromotionTx(ctx, tx, SnapshotPromotion{SourceReceiptMark: *source, PromotedAt: transition.promotion.PromotedAt}); err != nil {
		return nil, err
	}
	return transition.promotion, nil
}

func (d *DuckDB) removeStagedSnapshotBatch(ctx context.Context, env connector.SnapshotEnvelope) error {
	cleanupCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return d.Write(cleanupCtx, func(tx *sql.Tx) error {
		var active bool
		if err := tx.QueryRowContext(cleanupCtx, `SELECT EXISTS(SELECT 1 FROM connector_snapshot_promotions WHERE generation=?)`, env.Generation).Scan(&active); err != nil || active {
			return err
		}
		if _, err := tx.ExecContext(cleanupCtx, `DELETE FROM connector_snapshot_rows
WHERE project_id=? AND connector_id=? AND table_name=? AND generation=? AND batch_id=?`,
			env.ProjectID, env.ConnectorID, env.Table, env.Generation, env.BatchID); err != nil {
			return err
		}
		_, err := tx.ExecContext(cleanupCtx, `DELETE FROM connector_snapshot_batches
WHERE project_id=? AND connector_id=? AND table_name=? AND generation=? AND batch_id=?`,
			env.ProjectID, env.ConnectorID, env.Table, env.Generation, env.BatchID)
		return err
	})
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
	// Snapshot sequence numbers are scoped to snapshot mode. A newer
	// incremental receipt may have reset that sequence to zero, so fence the
	// replacement by the shared capture boundary before touching serving rows.
	snapshotSource := sourceReceiptFromSnapshot(env)
	snapshotSource.GenerationSeq = uint64(complete.GenerationSeq)
	snapshotSource.CaptureStartedAt = &complete.CaptureStartedAt
	superseded, err := sourceReceiptSupersededTx(ctx, tx, snapshotSource)
	if err != nil {
		return snapshotPromotionResult{}, err
	}
	if superseded {
		return snapshotPromotionResult{state: snapshotSuperseded}, nil
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

func (d *DuckDB) snapshotGenerationSuperseded(ctx context.Context, generation StagingGenerationDescriptor) (bool, error) {
	var superseded bool
	err := d.Read(ctx, func(conn *sql.Conn) error {
		return conn.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM connector_snapshot_promotions
WHERE project_id=? AND connector_id=? AND table_name=? AND generation_seq>?)`, generation.ProjectID, generation.ConnectorID,
			generation.Table, generation.GenerationSeq).Scan(&superseded)
	})
	return superseded, err
}

func (d *DuckDB) classifySnapshotGenerations(ctx context.Context, generations []StagingGenerationDescriptor) error {
	if len(generations) == 0 {
		return nil
	}
	type sourceKey struct{ projectID, connectorID, table string }
	type promotionState struct {
		generation string
		sequence   int64
	}
	keys := make([]sourceKey, 0, len(generations))
	seen := make(map[sourceKey]struct{}, len(generations))
	for _, generation := range generations {
		key := sourceKey{generation.ProjectID, generation.ConnectorID, generation.Table}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		keys = append(keys, key)
	}
	var query strings.Builder
	query.WriteString(`SELECT project_id::VARCHAR,connector_id::VARCHAR,table_name,generation::VARCHAR,generation_seq
FROM connector_snapshot_promotions WHERE `)
	args := make([]any, 0, len(keys)*3)
	for i, key := range keys {
		if i > 0 {
			query.WriteString(" OR ")
		}
		query.WriteString("(project_id=? AND connector_id=? AND table_name=?)")
		args = append(args, key.projectID, key.connectorID, key.table)
	}
	promotions := make(map[sourceKey]promotionState, len(keys))
	if err := d.Read(ctx, func(conn *sql.Conn) error {
		rows, err := conn.QueryContext(ctx, query.String(), args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var key sourceKey
			var state promotionState
			if err := rows.Scan(&key.projectID, &key.connectorID, &key.table, &state.generation, &state.sequence); err != nil {
				return err
			}
			promotions[key] = state
		}
		return rows.Err()
	}); err != nil {
		return err
	}
	for i := range generations {
		key := sourceKey{generations[i].ProjectID, generations[i].ConnectorID, generations[i].Table}
		promotion, ok := promotions[key]
		if !ok {
			continue
		}
		generations[i].IsActiveOnThisStore = promotion.generation == generations[i].Generation
		generations[i].IsSupersededOnThisStore = generations[i].State == "sealed" && promotion.sequence > generations[i].GenerationSeq
	}
	return nil
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

func (d *DuckDB) deleteSnapshotStagingChunk(ctx context.Context, generation StagingGenerationDescriptor, limit int) (deleted int, more, eligible bool, err error) {
	err = d.Write(ctx, func(tx *sql.Tx) error {
		var active bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM connector_snapshot_promotions WHERE generation=?)`, generation.Generation).Scan(&active); err != nil {
			return err
		}
		if active {
			return nil
		}
		if generation.State == "sealed" {
			var superseded bool
			if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM connector_snapshot_promotions
WHERE project_id=? AND connector_id=? AND table_name=? AND generation_seq>?)`, generation.ProjectID, generation.ConnectorID,
				generation.Table, generation.GenerationSeq).Scan(&superseded); err != nil {
				return err
			}
			if !superseded {
				return nil
			}
		}
		eligible = true
		result, err := tx.ExecContext(ctx, `DELETE FROM connector_snapshot_rows WHERE rowid IN
(SELECT rowid FROM connector_snapshot_rows WHERE generation=? LIMIT ?)`, generation.Generation, limit)
		if err != nil {
			return err
		}
		if n, rowsErr := result.RowsAffected(); rowsErr == nil {
			deleted = int(n)
		}
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM connector_snapshot_rows WHERE generation=?)`, generation.Generation).Scan(&more); err != nil {
			return err
		}
		if !more {
			for _, stmt := range []string{
				`DELETE FROM connector_snapshot_batches WHERE generation=?`,
				`DELETE FROM connector_snapshot_completions WHERE generation=?`,
			} {
				if _, err := tx.ExecContext(ctx, stmt, generation.Generation); err != nil {
					return err
				}
			}
			if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO connector_snapshot_cleanup_receipts(generation,cleaned_at) VALUES(?,now())`, generation.Generation); err != nil {
				return err
			}
		}
		return nil
	})
	return deleted, more, eligible, err
}
