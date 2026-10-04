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
		var err error
		switch env.Kind {
		case connector.SnapshotKindBatch:
			err = stageSnapshotBatch(ctx, tx, env)
		case connector.SnapshotKindComplete:
			err = stageSnapshotCompletion(ctx, tx, env)
		}
		if err != nil {
			return err
		}
		promotion, err = trySnapshotPromotion(ctx, tx, env)
		if err != nil {
			return err
		}
		return advancePositionTx(ctx, tx, mark)
	})
	return promotion, err
}

func stageSnapshotBatch(ctx context.Context, tx *sql.Tx, env connector.SnapshotEnvelope) error {
	if err := validateSnapshotGenerationIdentity(ctx, tx, env); err != nil {
		return err
	}
	var existingIndex, existingCount int64
	var existingHash string
	err := tx.QueryRowContext(ctx, `SELECT batch_index, payload_sha256, row_count FROM connector_snapshot_batches
WHERE project_id=? AND connector_id=? AND table_name=? AND generation=? AND batch_id=?`,
		env.ProjectID, env.ConnectorID, env.Table, env.Generation, env.BatchID).Scan(&existingIndex, &existingHash, &existingCount)
	if err == nil {
		if existingIndex != env.BatchIndex || existingHash != env.PayloadSHA256 || existingCount != int64(len(env.Rows)) {
			return fmt.Errorf("snapshot batch replay conflicts with staged payload")
		}
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	var otherID, otherHash string
	err = tx.QueryRowContext(ctx, `SELECT batch_id, payload_sha256 FROM connector_snapshot_batches
WHERE project_id=? AND connector_id=? AND table_name=? AND generation=? AND batch_index=?`,
		env.ProjectID, env.ConnectorID, env.Table, env.Generation, env.BatchIndex).Scan(&otherID, &otherHash)
	if err == nil {
		return fmt.Errorf("snapshot batch index conflicts with %s", otherID)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}

	if _, err := tx.ExecContext(ctx, `INSERT INTO connector_snapshot_batches
(project_id,connector_id,table_name,sync_id,generation,generation_seq,binding_digest,batch_id,batch_index,payload_sha256,row_count,capture_started_at,run_id)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`, env.ProjectID, env.ConnectorID, env.Table, env.SyncID, env.Generation, env.GenerationSeq,
		env.BindingDigest, env.BatchID, env.BatchIndex, env.PayloadSHA256, len(env.Rows), env.CaptureStartedAt, env.RunID); err != nil {
		return err
	}
	if len(env.Rows) == 0 {
		return nil
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
		return errors.New("snapshot batch rows were rejected by staging")
	}
	return nil
}

func stageSnapshotCompletion(ctx context.Context, tx *sql.Tx, env connector.SnapshotEnvelope) error {
	if err := validateSnapshotGenerationIdentity(ctx, tx, env); err != nil {
		return err
	}
	var seq, batches, rows int64
	var digest, binding string
	err := tx.QueryRowContext(ctx, `SELECT generation_seq, expected_batches, expected_rows, batch_manifest_sha256, binding_digest
FROM connector_snapshot_completions WHERE project_id=? AND connector_id=? AND table_name=? AND generation=?`,
		env.ProjectID, env.ConnectorID, env.Table, env.Generation).Scan(&seq, &batches, &rows, &digest, &binding)
	if err == nil {
		if seq != env.GenerationSeq || batches != env.ExpectedBatches || rows != env.ExpectedRows || digest != env.BatchManifestSHA256 || binding != env.BindingDigest {
			return fmt.Errorf("snapshot completion replay conflicts with staged marker")
		}
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO connector_snapshot_completions
(project_id,connector_id,table_name,sync_id,generation,generation_seq,binding_digest,expected_batches,expected_rows,batch_manifest_sha256,capture_started_at,capture_finished_at,run_id)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`, env.ProjectID, env.ConnectorID, env.Table, env.SyncID, env.Generation, env.GenerationSeq,
		env.BindingDigest, env.ExpectedBatches, env.ExpectedRows, env.BatchManifestSHA256, env.CaptureStartedAt, *env.CaptureFinishedAt, env.RunID)
	return err
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

func trySnapshotPromotion(ctx context.Context, tx *sql.Tx, env connector.SnapshotEnvelope) (*connector.SnapshotPromotion, error) {
	var complete connector.SnapshotPromotion
	err := tx.QueryRowContext(ctx, `SELECT project_id::VARCHAR,connector_id::VARCHAR,table_name,sync_id::VARCHAR,generation::VARCHAR,generation_seq,
binding_digest,capture_started_at,capture_finished_at,expected_batches,expected_rows,batch_manifest_sha256
FROM connector_snapshot_completions WHERE project_id=? AND connector_id=? AND table_name=? AND generation=?`,
		env.ProjectID, env.ConnectorID, env.Table, env.Generation).Scan(&complete.ProjectID, &complete.ConnectorID, &complete.Table, &complete.SyncID,
		&complete.Generation, &complete.GenerationSeq, &complete.BindingDigest, &complete.CaptureStartedAt, &complete.CaptureFinishedAt,
		&complete.ExpectedBatches, &complete.ExpectedRows, &complete.BatchManifestSHA256)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	var activeSeq int64
	var activeGeneration string
	err = tx.QueryRowContext(ctx, `SELECT generation_seq,generation::VARCHAR FROM connector_snapshot_promotions
WHERE project_id=? AND connector_id=? AND table_name=?`, env.ProjectID, env.ConnectorID, env.Table).Scan(&activeSeq, &activeGeneration)
	if err == nil && activeSeq >= complete.GenerationSeq {
		if activeSeq == complete.GenerationSeq && activeGeneration == complete.Generation {
			_ = tx.QueryRowContext(ctx, `SELECT promoted_at FROM connector_snapshot_promotions WHERE project_id=? AND connector_id=? AND table_name=?`, env.ProjectID, env.ConnectorID, env.Table).Scan(&complete.PromotedAt)
			return &complete, nil
		}
		return nil, nil
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}

	rows, err := tx.QueryContext(ctx, `SELECT batch_index,batch_id,payload_sha256,row_count FROM connector_snapshot_batches
WHERE project_id=? AND connector_id=? AND table_name=? AND generation=? ORDER BY batch_index`, env.ProjectID, env.ConnectorID, env.Table, env.Generation)
	if err != nil {
		return nil, err
	}
	entries := make([]connector.SnapshotManifestEntry, 0)
	var rowCount int64
	for rows.Next() {
		var entry connector.SnapshotManifestEntry
		if err := rows.Scan(&entry.Index, &entry.BatchID, &entry.PayloadSHA256, &entry.RowCount); err != nil {
			_ = rows.Close()
			return nil, err
		}
		entries = append(entries, entry)
		rowCount += int64(entry.RowCount)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if int64(len(entries)) != complete.ExpectedBatches || rowCount != complete.ExpectedRows {
		return nil, nil
	}
	digest, err := connector.SnapshotManifestDigest(entries)
	if err != nil || digest != complete.BatchManifestSHA256 {
		return nil, nil
	}

	if _, err := tx.ExecContext(ctx, `DELETE FROM external_rows WHERE project_id=? AND connector_id=? AND table_name=?`, env.ProjectID, env.ConnectorID, env.Table); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO external_rows (project_id,connector_id,table_name,row_key,cursor,data,synced_at)
SELECT project_id,connector_id,table_name,row_key,'',data,now() FROM connector_snapshot_rows
WHERE project_id=? AND connector_id=? AND table_name=? AND generation=?`, env.ProjectID, env.ConnectorID, env.Table, env.Generation); err != nil {
		return nil, err
	}
	complete.PromotedAt = time.Now().UTC()
	if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO connector_snapshot_promotions
(project_id,connector_id,table_name,sync_id,generation,generation_seq,binding_digest,expected_batches,expected_rows,batch_manifest_sha256,capture_started_at,capture_finished_at,promoted_at)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`, complete.ProjectID, complete.ConnectorID, complete.Table, complete.SyncID, complete.Generation, complete.GenerationSeq,
		complete.BindingDigest, complete.ExpectedBatches, complete.ExpectedRows, complete.BatchManifestSHA256, complete.CaptureStartedAt, complete.CaptureFinishedAt, complete.PromotedAt); err != nil {
		return nil, err
	}
	return &complete, nil
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
