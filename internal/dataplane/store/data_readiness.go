package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/lohi-ai/agentray/internal/shared/cronx"
)

// Readiness is additive public evidence for one configured source. Pointer
// fields intentionally omit omitempty: once a readiness object is emitted,
// unknown is represented as JSON null rather than a zero value.
type Readiness struct {
	State             string     `json:"state"`
	PublishedAt       *time.Time `json:"published_at"`
	LandedAt          *time.Time `json:"landed_at"`
	QueryableAt       *time.Time `json:"queryable_at"`
	Generation        *string    `json:"generation"`
	CaptureStartedAt  *time.Time `json:"capture_started_at"`
	CaptureFinishedAt *time.Time `json:"capture_finished_at"`
	Reason            *string    `json:"reason"`
	LastCompleteAt    *time.Time `json:"last_complete_at"`
}

const (
	ReadinessNotConfigured = "not_configured"
	ReadinessSyncing       = "syncing"
	ReadinessReady         = "ready"
	ReadinessStale         = "stale"
	ReadinessIncomplete    = "incomplete"
	ReadinessError         = "error"

	// Receipt journals retain an exact recent suffix for replay diagnosis while
	// ingest_position and durable loss markers carry the long-lived store-binding
	// proof. Unresolved holes are never subject to this bound.
	deliveryReceiptSuffixLimit = 4096
	// Legacy publications have no run/completion identity that can trigger the
	// normal resolved-run compactor, so retain a bounded operator-visible suffix.
	legacyPublicationReceiptLimit = 256
)

// DeliveryReceiptMark is the durable identity of one delivery. StreamSeq is
// deliberately separate from the consumer sequence kept by AppliedMark.
type DeliveryReceiptMark struct {
	StreamID      string
	Subject       string
	StreamSeq     uint64
	PayloadSHA256 string
	PublishedAt   *time.Time
	ProjectID     string
	ConnectorID   string
	Table         string
	Replayed      bool
	Unverifiable  bool
}

// SourceReceiptMark consumes the frozen C1 envelope identity without owning
// C1's generation or promotion state machine. Promoted may only be set by the
// atomic local-promotion path; a completion marker alone is not landed data.
type SourceReceiptMark struct {
	ProjectID         string     `json:"project_id"`
	ConnectorID       string     `json:"connector_id"`
	Table             string     `json:"table"`
	SyncID            string     `json:"sync_id"`
	RunID             string     `json:"run_id"`
	Generation        string     `json:"generation,omitempty"`
	GenerationSeq     uint64     `json:"generation_seq,omitempty"`
	BindingDigest     string     `json:"binding_digest,omitempty"`
	BatchID           string     `json:"batch_id,omitempty"`
	BatchIndex        *uint64    `json:"batch_index,omitempty"`
	PayloadSHA256     string     `json:"payload_sha256,omitempty"`
	ExpectedBatches   *uint64    `json:"expected_batches,omitempty"`
	ExpectedRows      *uint64    `json:"expected_rows,omitempty"`
	CaptureStartedAt  *time.Time `json:"capture_started_at,omitempty"`
	CaptureFinishedAt *time.Time `json:"capture_finished_at,omitempty"`
	PublishedAt       *time.Time `json:"published_at,omitempty"`
	Complete          bool       `json:"complete,omitempty"`
	Promoted          bool       `json:"-"`
}

// SnapshotPromotion is the C1→C2 seam. C1 calls RecordSnapshotPromotionTx in
// the same transaction that swaps staging into external_rows.
type SnapshotPromotion struct {
	SourceReceiptMark
	PromotedAt time.Time
}

type PublicationObservation struct {
	SourceReceiptMark
	ProjectID, ConnectorID, Table string
	StableBatchID, PayloadSHA256  string
	PublishedAt                   time.Time
}

func (s *Store) migrateDataReadiness(ctx context.Context) error {
	for _, stmt := range []string{`CREATE TABLE IF NOT EXISTS source_publication_receipts (
	project_id UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
	connector_id UUID NOT NULL REFERENCES data_connectors(id) ON DELETE CASCADE,
	table_name VARCHAR(256) NOT NULL,
	stable_batch_id VARCHAR(128) NOT NULL,
	payload_sha256 CHAR(64) NOT NULL,
	first_published_at TIMESTAMPTZ NOT NULL,
	last_observed_at TIMESTAMPTZ NOT NULL,
	PRIMARY KEY (project_id, connector_id, table_name, stable_batch_id)
)`,
		`ALTER TABLE source_publication_receipts ADD COLUMN IF NOT EXISTS sync_id UUID`,
		`ALTER TABLE source_publication_receipts ADD COLUMN IF NOT EXISTS run_id UUID`,
		`ALTER TABLE source_publication_receipts ADD COLUMN IF NOT EXISTS generation UUID`,
		`ALTER TABLE source_publication_receipts ADD COLUMN IF NOT EXISTS generation_seq BIGINT NOT NULL DEFAULT 0`,
		`ALTER TABLE source_publication_receipts ADD COLUMN IF NOT EXISTS receipt_kind VARCHAR(16) NOT NULL DEFAULT 'batch'`,
		`ALTER TABLE source_publication_receipts ADD COLUMN IF NOT EXISTS capture_started_at TIMESTAMPTZ`,
		`ALTER TABLE source_publication_receipts ADD COLUMN IF NOT EXISTS capture_finished_at TIMESTAMPTZ`,
		`CREATE INDEX IF NOT EXISTS source_publication_receipts_latest_idx
		 ON source_publication_receipts(project_id, sync_id, generation_seq DESC, last_observed_at DESC)`,
	} {
		if _, err := s.pg.Exec(ctx, stmt); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) RecordPublication(ctx context.Context, observation PublicationObservation) error {
	if observation.ProjectID == "" {
		observation.ProjectID = observation.SourceReceiptMark.ProjectID
	}
	if observation.ConnectorID == "" {
		observation.ConnectorID = observation.SourceReceiptMark.ConnectorID
	}
	if observation.Table == "" {
		observation.Table = observation.SourceReceiptMark.Table
	}
	var syncID, runID, generation any
	if observation.SyncID != "" {
		syncID = observation.SyncID
	}
	if observation.RunID != "" {
		runID = observation.RunID
	}
	if observation.Generation != "" {
		generation = observation.Generation
	}
	kind := "batch"
	if observation.Complete {
		kind = "complete"
	}
	tx, err := s.pg.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	tag, err := tx.Exec(ctx, `INSERT INTO source_publication_receipts
(project_id, connector_id, table_name, stable_batch_id, payload_sha256, first_published_at, last_observed_at,
 sync_id, run_id, generation, generation_seq, receipt_kind, capture_started_at, capture_finished_at)
VALUES ($1, $2, $3, $4, $5, $6, $6, $7, $8, $9, $10, $11, $12, $13)
ON CONFLICT (project_id, connector_id, table_name, stable_batch_id) DO UPDATE SET
last_observed_at = excluded.last_observed_at
WHERE source_publication_receipts.payload_sha256 = excluded.payload_sha256
  AND source_publication_receipts.run_id IS NOT DISTINCT FROM excluded.run_id
  AND source_publication_receipts.generation IS NOT DISTINCT FROM excluded.generation`,
		observation.ProjectID, observation.ConnectorID, observation.Table, observation.StableBatchID,
		observation.PayloadSHA256, observation.PublishedAt.UTC(), syncID, runID, generation,
		observation.GenerationSeq, kind, observation.CaptureStartedAt, observation.CaptureFinishedAt)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("publication identity %q conflicts with different content", observation.StableBatchID)
	}
	// Once a complete generation/run is durably observed, older publication
	// rows for this source are resolved history. Keeping the current identity's
	// batch set preserves reconciliation while bounding repeated publications.
	if observation.Complete && observation.RunID != "" && (observation.GenerationSeq > 0 || observation.CaptureStartedAt != nil) {
		if _, err := tx.Exec(ctx, `DELETE FROM source_publication_receipts
WHERE project_id=$1 AND connector_id=$2 AND table_name=$3 AND run_id IS DISTINCT FROM $4
  AND (($5 > 0 AND generation_seq <= $5)
       OR ($5 = 0 AND (capture_started_at IS NULL OR capture_started_at <= $6)))`,
			observation.ProjectID, observation.ConnectorID, observation.Table, observation.RunID,
			observation.GenerationSeq, observation.CaptureStartedAt); err != nil {
			return err
		}
	}
	if observation.RunID == "" && observation.Generation == "" {
		if _, err := tx.Exec(ctx, `DELETE FROM source_publication_receipts
WHERE project_id=$1 AND connector_id=$2 AND table_name=$3 AND run_id IS NULL AND generation IS NULL
  AND stable_batch_id IN (
	SELECT stable_batch_id FROM source_publication_receipts
	WHERE project_id=$1 AND connector_id=$2 AND table_name=$3 AND run_id IS NULL AND generation IS NULL
	ORDER BY last_observed_at DESC, stable_batch_id DESC
	OFFSET $4
  )`, observation.ProjectID, observation.ConnectorID, observation.Table, legacyPublicationReceiptLimit); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// RecordSnapshotPromotionTx adds receipt evidence to an existing promotion
// transaction. It does not perform, infer, or validate promotion itself.
func RecordSnapshotPromotionTx(ctx context.Context, tx *sql.Tx, p SnapshotPromotion) error {
	mark := AppliedMark{Source: &p.SourceReceiptMark}
	mark.Source.Complete = true
	mark.Source.Promoted = true
	return recordAppliedReceiptsTx(ctx, tx, mark, p.PromotedAt.UTC())
}

func recordAppliedReceiptsTx(ctx context.Context, tx *sql.Tx, mark AppliedMark, landedAt time.Time) error {
	for _, d := range mark.Deliveries {
		if strings.TrimSpace(d.StreamID) == "" || strings.TrimSpace(d.Subject) == "" || d.StreamSeq == 0 || strings.TrimSpace(d.PayloadSHA256) == "" {
			continue
		}
		projectID, connectorID, table := d.ProjectID, d.ConnectorID, d.Table
		generationKey := ""
		if mark.Source != nil {
			projectID, connectorID, table = mark.Source.ProjectID, mark.Source.ConnectorID, mark.Source.Table
			generationKey = mark.Source.Generation
			if generationKey == "" {
				generationKey = mark.Source.RunID
			}
		}
		var pid, cid, tableValue, generationValue any
		if projectID != "" {
			parsed, err := uuid.Parse(projectID)
			if err != nil {
				return fmt.Errorf("delivery project_id: %w", err)
			}
			pid = parsed
		}
		if connectorID != "" {
			parsed, err := uuid.Parse(connectorID)
			if err != nil {
				return fmt.Errorf("delivery connector_id: %w", err)
			}
			cid = parsed
		}
		if table != "" {
			tableValue = table
		}
		if generationKey != "" {
			generationValue = generationKey
		}
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO data_receipt_deliveries
(stream_id, subject, stream_seq, payload_sha256, project_id, connector_id, table_name, generation_key, applied_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, d.StreamID, d.Subject, d.StreamSeq, d.PayloadSHA256,
			pid, cid, tableValue, generationValue, landedAt); err != nil {
			return err
		}
		if d.Unverifiable {
			if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO data_receipt_holes
(stream_id, subject, stream_seq, payload_sha256, project_id, connector_id, table_name, unverifiable)
VALUES (?, ?, ?, ?, ?, ?, ?, true)`, d.StreamID, d.Subject, d.StreamSeq, d.PayloadSHA256, pid, cid, tableValue); err != nil {
				return err
			}
			continue
		}
		var holeProject, holeConnector, holeTable sql.NullString
		holeErr := tx.QueryRowContext(ctx, `SELECT project_id::VARCHAR, connector_id::VARCHAR, table_name
FROM data_receipt_holes WHERE stream_id=? AND subject=? AND stream_seq=? AND payload_sha256=? AND cleared_at IS NULL AND NOT unverifiable`,
			d.StreamID, d.Subject, d.StreamSeq, d.PayloadSHA256).Scan(&holeProject, &holeConnector, &holeTable)
		if holeErr != nil && !errors.Is(holeErr, sql.ErrNoRows) {
			return holeErr
		}
		if _, err := tx.ExecContext(ctx, `UPDATE data_receipt_holes SET cleared_at = ?
WHERE stream_id = ? AND subject = ? AND stream_seq = ? AND payload_sha256 = ? AND cleared_at IS NULL AND NOT unverifiable`,
			landedAt, d.StreamID, d.Subject, d.StreamSeq, d.PayloadSHA256); err != nil {
			return err
		}
		if holeErr == nil {
			if err := invalidateSourceConfirmationsTx(ctx, tx, holeProject, holeConnector, holeTable, landedAt); err != nil {
				return err
			}
			// Once this exact replay repaired the hole, neither row carries live
			// readiness authority. Remove both in the same transaction so event-only
			// replay traffic cannot grow the journals forever.
			if _, err := tx.ExecContext(ctx, `DELETE FROM data_receipt_deliveries
WHERE stream_id=? AND subject=? AND stream_seq=? AND payload_sha256=?`,
				d.StreamID, d.Subject, d.StreamSeq, d.PayloadSHA256); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM data_receipt_holes
WHERE stream_id=? AND subject=? AND stream_seq=? AND payload_sha256=? AND cleared_at IS NOT NULL`,
				d.StreamID, d.Subject, d.StreamSeq, d.PayloadSHA256); err != nil {
				return err
			}
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM data_receipt_deliveries WHERE rowid IN (
	SELECT rowid FROM (
		SELECT rowid, row_number() OVER (ORDER BY applied_at DESC, stream_id DESC, stream_seq DESC) AS receipt_rank
		FROM data_receipt_deliveries
	) ranked WHERE receipt_rank > ?
)`, deliveryReceiptSuffixLimit); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM data_receipt_holes WHERE cleared_at IS NOT NULL`); err != nil {
		return err
	}
	s := mark.Source
	if s == nil || mark.suppressSourceMutation {
		return nil
	}
	pid, err := uuid.Parse(s.ProjectID)
	if err != nil {
		return fmt.Errorf("readiness project_id: %w", err)
	}
	cid, err := uuid.Parse(s.ConnectorID)
	if err != nil {
		return fmt.Errorf("readiness connector_id: %w", err)
	}
	if strings.TrimSpace(s.Table) == "" {
		return errors.New("readiness table is required")
	}
	generationKey := s.Generation
	if generationKey == "" {
		generationKey = s.RunID
	}
	if generationKey == "" {
		generationKey = "legacy-unknown"
	}
	superseded, err := sourceReceiptSupersededTx(ctx, tx, s)
	if err != nil {
		return err
	}
	if superseded {
		return nil
	}
	if s.BatchID != "" {
		var prior string
		err := tx.QueryRowContext(ctx, `SELECT payload_sha256 FROM data_receipt_batches
WHERE project_id = ? AND connector_id = ? AND table_name = ? AND generation_key = ? AND batch_id = ?`,
			pid, cid, s.Table, generationKey, s.BatchID).Scan(&prior)
		if err == nil && prior != s.PayloadSHA256 {
			return fmt.Errorf("readiness batch %q reused with conflicting content", s.BatchID)
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		var batchIndex any
		if s.BatchIndex != nil {
			batchIndex = *s.BatchIndex
		}
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO data_receipt_batches
(project_id, connector_id, table_name, generation_key, batch_id, batch_index, payload_sha256, landed_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, pid, cid, s.Table, generationKey, s.BatchID, batchIndex, s.PayloadSHA256, landedAt); err != nil {
			return err
		}
	}
	var syncID, runID, generation any
	if s.SyncID != "" {
		v, err := uuid.Parse(s.SyncID)
		if err != nil {
			return fmt.Errorf("readiness sync_id: %w", err)
		}
		syncID = v
	}
	if s.RunID != "" {
		v, err := uuid.Parse(s.RunID)
		if err != nil {
			return fmt.Errorf("readiness run_id: %w", err)
		}
		runID = v
	}
	if s.Generation != "" {
		v, err := uuid.Parse(s.Generation)
		if err != nil {
			return fmt.Errorf("readiness generation: %w", err)
		}
		generation = v
	}
	var expectedBatches, expectedRows any
	if s.ExpectedBatches != nil {
		expectedBatches = *s.ExpectedBatches
	}
	if s.ExpectedRows != nil {
		expectedRows = *s.ExpectedRows
	}
	var publishedAt any
	if s.PublishedAt != nil {
		publishedAt = s.PublishedAt.UTC()
	}
	var sourceLandedAt any
	var landedGenerationKey any
	if s.Promoted {
		sourceLandedAt = landedAt
		landedGenerationKey = generationKey
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO data_receipt_sources
(project_id, connector_id, table_name, sync_id, run_id, generation, generation_key, generation_seq, binding_digest,
 capture_started_at, capture_finished_at, published_at, landed_at, landed_generation_key, last_complete_at,
 expected_batches, expected_rows, completion_seen, ordering_ambiguous, mutation_seq, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, false, 1, ?)
ON CONFLICT (project_id, connector_id, table_name) DO UPDATE SET
 sync_id = coalesce(excluded.sync_id, data_receipt_sources.sync_id),
 run_id = CASE WHEN excluded.generation_key <> data_receipt_sources.generation_key THEN excluded.run_id ELSE coalesce(excluded.run_id, data_receipt_sources.run_id) END,
 generation = CASE WHEN excluded.generation_key <> data_receipt_sources.generation_key THEN excluded.generation ELSE coalesce(excluded.generation, data_receipt_sources.generation) END,
 generation_key = excluded.generation_key,
 generation_seq = excluded.generation_seq,
 binding_digest = CASE WHEN excluded.generation_key <> data_receipt_sources.generation_key THEN excluded.binding_digest WHEN excluded.binding_digest = '' THEN data_receipt_sources.binding_digest ELSE excluded.binding_digest END,
 capture_started_at = CASE WHEN excluded.generation_key <> data_receipt_sources.generation_key THEN excluded.capture_started_at ELSE coalesce(excluded.capture_started_at, data_receipt_sources.capture_started_at) END,
 capture_finished_at = CASE WHEN excluded.generation_key <> data_receipt_sources.generation_key THEN excluded.capture_finished_at ELSE coalesce(excluded.capture_finished_at, data_receipt_sources.capture_finished_at) END,
 published_at = CASE WHEN excluded.generation_key <> data_receipt_sources.generation_key THEN excluded.published_at ELSE coalesce(excluded.published_at, data_receipt_sources.published_at) END,
 landed_at = CASE WHEN excluded.generation_key <> data_receipt_sources.generation_key THEN excluded.landed_at ELSE coalesce(excluded.landed_at, data_receipt_sources.landed_at) END,
 landed_generation_key = coalesce(excluded.landed_generation_key, data_receipt_sources.landed_generation_key),
 last_complete_at = coalesce(excluded.last_complete_at, data_receipt_sources.last_complete_at),
 expected_batches = CASE WHEN excluded.generation_key <> data_receipt_sources.generation_key THEN excluded.expected_batches ELSE coalesce(excluded.expected_batches, data_receipt_sources.expected_batches) END,
 expected_rows = CASE WHEN excluded.generation_key <> data_receipt_sources.generation_key THEN excluded.expected_rows ELSE coalesce(excluded.expected_rows, data_receipt_sources.expected_rows) END,
 completion_seen = CASE WHEN excluded.generation_key <> data_receipt_sources.generation_key THEN excluded.completion_seen ELSE excluded.completion_seen OR data_receipt_sources.completion_seen END,
	ordering_ambiguous = CASE
	WHEN excluded.generation_seq > 0 AND excluded.completion_seen AND excluded.landed_generation_key = excluded.generation_key
		 AND (data_receipt_sources.landed_generation_key IS NULL
		      OR data_receipt_sources.landed_generation_key <> excluded.landed_generation_key)
		THEN false
	ELSE coalesce(data_receipt_sources.ordering_ambiguous, false)
 END,
 mutation_seq = data_receipt_sources.mutation_seq + 1,
 updated_at = excluded.updated_at`,
		pid, cid, s.Table, syncID, runID, generation, generationKey, s.GenerationSeq, s.BindingDigest,
		s.CaptureStartedAt, s.CaptureFinishedAt, publishedAt, sourceLandedAt, landedGenerationKey,
		func() any {
			if s.Promoted && s.Complete {
				return landedAt
			}
			return nil
		}(), expectedBatches, expectedRows,
		s.Complete, landedAt); err != nil {
		return err
	}
	if s.Complete {
		// A completed current identity subsumes older per-batch detail. Keep the
		// latest complete generation/run and every unresolved hole; discard only
		// history that can no longer affect readiness.
		if _, err := tx.ExecContext(ctx, `DELETE FROM data_receipt_batches
WHERE project_id=? AND connector_id=? AND table_name=? AND generation_key<>?`, pid, cid, s.Table, generationKey); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM data_receipt_deliveries
WHERE project_id=? AND connector_id=? AND table_name=? AND generation_key IS NOT NULL AND generation_key<>?`, pid, cid, s.Table, generationKey); err != nil {
			return err
		}
	}
	return nil
}

// sourceReceiptSupersededTx is the ordering fence shared by receipt-only and
// row mutations. Callers must evaluate it before the first row statement so a
// delayed incremental run can preserve newer values, merge only absent keys,
// and invalidate the newer readiness proof when that merge changes data.
func sourceReceiptSupersededTx(ctx context.Context, tx *sql.Tx, s *SourceReceiptMark) (bool, error) {
	if s == nil {
		return false, nil
	}
	generationKey := s.Generation
	if generationKey == "" {
		generationKey = s.RunID
	}
	if generationKey == "" {
		generationKey = "legacy-unknown"
	}
	var priorGenerationKey string
	var priorGenerationSeq uint64
	var priorCaptureStarted sql.NullTime
	err := tx.QueryRowContext(ctx, `SELECT generation_key, generation_seq, capture_started_at FROM data_receipt_sources
WHERE project_id = ? AND connector_id = ? AND table_name = ?`, s.ProjectID, s.ConnectorID, s.Table).
		Scan(&priorGenerationKey, &priorGenerationSeq, &priorCaptureStarted)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if s.GenerationSeq < priorGenerationSeq {
		return true, nil
	}
	if s.GenerationSeq > 0 && s.GenerationSeq == priorGenerationSeq && generationKey != priorGenerationKey {
		return false, fmt.Errorf("generation sequence %d conflicts with current generation", s.GenerationSeq)
	}
	if s.GenerationSeq == 0 && priorGenerationSeq == 0 && generationKey != priorGenerationKey {
		// Incremental runs do not have a generation sequence, so their capture
		// boundary is the ordering fence.
		if s.CaptureStartedAt == nil || (priorCaptureStarted.Valid && !s.CaptureStartedAt.UTC().After(priorCaptureStarted.Time.UTC())) {
			return true, nil
		}
	}
	return false, nil
}

func invalidateSourceConfirmationsTx(ctx context.Context, tx *sql.Tx, projectID, connectorID, table sql.NullString, at time.Time) error {
	query := `UPDATE data_receipt_sources SET mutation_seq=mutation_seq+1, updated_at=? WHERE 1=1`
	args := []any{at}
	if projectID.Valid {
		query += ` AND project_id=?`
		args = append(args, projectID.String)
	}
	if connectorID.Valid {
		query += ` AND connector_id=?`
		args = append(args, connectorID.String)
	}
	if table.Valid {
		query += ` AND table_name=?`
		args = append(args, table.String)
	}
	_, err := tx.ExecContext(ctx, query, args...)
	return err
}

// markSourceOrderingAmbiguousTx makes a conservative, durable claim about an
// incremental merge. Once a newer run is already authoritative, row presence
// alone cannot prove whether an overlapping key belongs to that run or to a
// delayed run in between. A fresh query can prove visibility, but not key
// ownership, so this bit deliberately survives query confirmation and restart.
// Only an atomically promoted sequenced snapshot clears it in the receipt
// upsert above because that is complete replacement evidence.
func markSourceOrderingAmbiguousTx(ctx context.Context, tx *sql.Tx, source *SourceReceiptMark, at time.Time) error {
	if source == nil {
		return nil
	}
	_, err := tx.ExecContext(ctx, `UPDATE data_receipt_sources
SET ordering_ambiguous=true, updated_at=?
WHERE project_id=? AND connector_id=? AND table_name=? AND NOT coalesce(ordering_ambiguous, false)`,
		at, source.ProjectID, source.ConnectorID, source.Table)
	return err
}

// RecordReadinessHole durably records a delivery that was settled into DLQ.
// It is separate from RecordPosition: settlement health must never erase the
// fact that the serving data has a hole.
func (d *DuckDB) RecordReadinessHole(ctx context.Context, delivery DeliveryReceiptMark, source *SourceReceiptMark) error {
	if delivery.StreamID == "" || delivery.Subject == "" || delivery.StreamSeq == 0 || delivery.PayloadSHA256 == "" {
		return errors.New("readiness hole requires stream, subject, sequence and digest")
	}
	return d.Write(ctx, func(tx *sql.Tx) error {
		var projectID, connectorID, table any
		if source != nil {
			if source.ProjectID != "" {
				v, err := uuid.Parse(source.ProjectID)
				if err != nil {
					return err
				}
				projectID = v
			}
			if source.ConnectorID != "" {
				v, err := uuid.Parse(source.ConnectorID)
				if err != nil {
					return err
				}
				connectorID = v
			}
			if source.Table != "" {
				table = source.Table
			}
		} else {
			if delivery.ProjectID != "" {
				v, err := uuid.Parse(delivery.ProjectID)
				if err != nil {
					return err
				}
				projectID = v
			}
			if delivery.ConnectorID != "" {
				v, err := uuid.Parse(delivery.ConnectorID)
				if err != nil {
					return err
				}
				connectorID = v
			}
			if delivery.Table != "" {
				table = delivery.Table
			}
		}
		_, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO data_receipt_holes
(stream_id, subject, stream_seq, payload_sha256, project_id, connector_id, table_name, unverifiable)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, delivery.StreamID, delivery.Subject, delivery.StreamSeq, delivery.PayloadSHA256, projectID, connectorID, table, delivery.Unverifiable)
		return err
	})
}

type ReadinessSource struct {
	SyncID, ConnectorID, Table, ScheduleCron string
	Configured                               bool
}

type localSourceReceipt struct {
	PublishedAt, LandedAt, LastCompleteAt, CaptureStartedAt, CaptureFinishedAt *time.Time
	Generation                                                                 *string
	RunID, GenerationKey                                                       string
	GenerationSeq                                                              uint64
	MutationSeq                                                                uint64
	CompletionSeen                                                             bool
	AppliedBatches                                                             uint64
	ExpectedBatches                                                            *uint64
	HasHole                                                                    bool
	OrderingAmbiguous                                                          bool
}

func (d *DuckDB) localReadiness(ctx context.Context, projectID string, sources []ReadinessSource) (map[string]localSourceReceipt, error) {
	out := make(map[string]localSourceReceipt, len(sources))
	err := d.Read(ctx, func(conn *sql.Conn) error {
		for _, source := range sources {
			if !source.Configured {
				continue
			}
			var r localSourceReceipt
			var published, landed, complete, started, finished sql.Null[time.Time]
			var generation, runID sql.NullString
			var expected sql.Null[uint64]
			err := conn.QueryRowContext(ctx, `SELECT published_at,
CASE WHEN landed_generation_key = generation_key THEN landed_at ELSE NULL END, last_complete_at,
capture_started_at, capture_finished_at, generation::VARCHAR, run_id::VARCHAR, generation_key, generation_seq,
mutation_seq, completion_seen, expected_batches, coalesce(ordering_ambiguous, false),
(SELECT count(*) FROM data_receipt_batches b WHERE b.project_id = s.project_id AND b.connector_id = s.connector_id AND b.table_name = s.table_name
 AND b.generation_key = coalesce(s.generation::VARCHAR, s.run_id::VARCHAR, 'legacy-unknown')),
EXISTS (SELECT 1 FROM ingest_position WHERE refused_missing > 0) OR EXISTS (SELECT 1 FROM data_receipt_holes h WHERE h.cleared_at IS NULL AND
 (h.project_id IS NULL OR (h.project_id = s.project_id AND (h.connector_id IS NULL OR h.connector_id = s.connector_id) AND (h.table_name IS NULL OR h.table_name = s.table_name))))
FROM data_receipt_sources s WHERE project_id = ? AND connector_id = ? AND table_name = ?`,
				projectID, source.ConnectorID, source.Table).Scan(&published, &landed, &complete, &started, &finished,
				&generation, &runID, &r.GenerationKey, &r.GenerationSeq, &r.MutationSeq, &r.CompletionSeen, &expected, &r.OrderingAmbiguous, &r.AppliedBatches, &r.HasHole)
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			if err != nil {
				return err
			}
			if published.Valid {
				v := published.V.UTC()
				r.PublishedAt = &v
			}
			if landed.Valid {
				v := landed.V.UTC()
				r.LandedAt = &v
			}
			if complete.Valid {
				v := complete.V.UTC()
				r.LastCompleteAt = &v
			}
			if started.Valid {
				v := started.V.UTC()
				r.CaptureStartedAt = &v
			}
			if finished.Valid {
				v := finished.V.UTC()
				r.CaptureFinishedAt = &v
			}
			if generation.Valid {
				v := generation.String
				r.Generation = &v
			}
			if runID.Valid {
				r.RunID = runID.String
			}
			if expected.Valid {
				v := expected.V
				r.ExpectedBatches = &v
			}
			out[source.SyncID] = r
		}
		return nil
	})
	return out, err
}

type publicationTarget struct {
	PublishedAt    time.Time
	CaptureStarted *time.Time
	RunID          string
	Generation     string
	GenerationSeq  uint64
}

func (s *Store) publicationTargets(ctx context.Context, projectID string, sources []ReadinessSource) (map[string]publicationTarget, error) {
	out := map[string]publicationTarget{}
	if s.pg == nil || len(sources) == 0 {
		return out, nil
	}
	ids := make([]string, 0, len(sources))
	for _, source := range sources {
		if source.SyncID != "" {
			ids = append(ids, source.SyncID)
		}
	}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := s.pg.Query(ctx, `SELECT DISTINCT ON (sync_id) sync_id::text, first_published_at,
capture_started_at, coalesce(run_id::text,''), coalesce(generation::text,''), generation_seq
FROM source_publication_receipts
WHERE project_id=$1 AND sync_id=ANY($2::uuid[])
ORDER BY sync_id, generation_seq DESC, capture_started_at DESC NULLS LAST, first_published_at DESC, last_observed_at DESC`, projectID, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var syncID string
		var target publicationTarget
		var captureStarted sql.NullTime
		if err := rows.Scan(&syncID, &target.PublishedAt, &captureStarted, &target.RunID, &target.Generation, &target.GenerationSeq); err != nil {
			return nil, err
		}
		target.PublishedAt = target.PublishedAt.UTC()
		if captureStarted.Valid {
			v := captureStarted.Time.UTC()
			target.CaptureStarted = &v
		}
		out[syncID] = target
	}
	return out, rows.Err()
}

func publicationRequiresNewerLocal(target publicationTarget, local localSourceReceipt) bool {
	if target.GenerationSeq > local.GenerationSeq {
		return true
	}
	if target.GenerationSeq < local.GenerationSeq {
		return false
	}
	if target.Generation != "" {
		return local.Generation == nil || *local.Generation != target.Generation
	}
	if target.RunID == "" || local.RunID == target.RunID {
		return false
	}
	if target.CaptureStarted != nil && local.CaptureStartedAt != nil && !target.CaptureStarted.After(*local.CaptureStartedAt) {
		return false
	}
	return true
}

// SourceReadiness resolves only evidence from this Store's DuckDB and its live
// sandbox pool. No query is launched merely to make status look healthy.
func (s *Store) SourceReadiness(ctx context.Context, projectID string, sources []ReadinessSource) (map[string]*Readiness, error) {
	local, err := s.duck.localReadiness(ctx, projectID, sources)
	if err != nil {
		return nil, err
	}
	targets, err := s.publicationTargets(ctx, projectID, sources)
	if err != nil {
		return nil, err
	}
	confirmations := s.sandboxes.confirmations(projectID)
	now := s.now().UTC()
	out := make(map[string]*Readiness, len(sources))
	for _, source := range sources {
		if !source.Configured {
			out[source.SyncID] = &Readiness{State: ReadinessNotConfigured, Reason: stringPtr("source_not_configured")}
			continue
		}
		r, ok := local[source.SyncID]
		if !ok {
			ready := &Readiness{State: ReadinessSyncing, Reason: stringPtr("unknown_receipt")}
			if target, found := targets[source.SyncID]; found {
				published := target.PublishedAt
				ready.PublishedAt = &published
				if target.Generation != "" {
					generation := target.Generation
					ready.Generation = &generation
				}
				ready.Reason = stringPtr("awaiting_landing")
			}
			out[source.SyncID] = ready
			continue
		}
		ready := &Readiness{State: ReadinessSyncing, PublishedAt: r.PublishedAt, LandedAt: r.LandedAt,
			Generation: r.Generation, CaptureStartedAt: r.CaptureStartedAt, CaptureFinishedAt: r.CaptureFinishedAt,
			LastCompleteAt: r.LastCompleteAt}
		if target, found := targets[source.SyncID]; found && ready.PublishedAt == nil {
			published := target.PublishedAt
			ready.PublishedAt = &published
		}
		if target, found := targets[source.SyncID]; found && publicationRequiresNewerLocal(target, r) {
			published := target.PublishedAt
			ready.PublishedAt = &published
			ready.LandedAt, ready.QueryableAt = nil, nil
			if target.Generation != "" {
				generation := target.Generation
				ready.Generation = &generation
			}
			ready.Reason = stringPtr("awaiting_landing")
			out[source.SyncID] = ready
			continue
		}
		if r.HasHole {
			ready.State, ready.Reason = ReadinessIncomplete, stringPtr("coverage_hole")
			out[source.SyncID] = ready
			continue
		}
		if r.OrderingAmbiguous {
			ready.State, ready.Reason = ReadinessIncomplete, stringPtr("incremental_ordering_ambiguous")
			out[source.SyncID] = ready
			continue
		}
		if !r.CompletionSeen {
			ready.Reason = stringPtr("awaiting_completion")
			out[source.SyncID] = ready
			continue
		}
		if r.ExpectedBatches == nil || r.AppliedBatches != *r.ExpectedBatches {
			ready.State, ready.Reason = ReadinessIncomplete, stringPtr("coverage_hole")
			out[source.SyncID] = ready
			continue
		}
		if r.LandedAt == nil {
			ready.Reason = stringPtr("awaiting_landing")
			out[source.SyncID] = ready
			continue
		}
		confirmation, confirmed := confirmations[source.ConnectorID+"\x00"+source.Table]
		if !confirmed || !confirmation.matches(r) {
			ready.Reason = stringPtr("awaiting_query_confirmation")
			out[source.SyncID] = ready
			continue
		}
		q := confirmation.ConfirmedAt.UTC()
		ready.QueryableAt = &q
		maxAge, known := scheduleFreshness(source.ScheduleCron)
		if !known {
			maxAge, known = s.manualFreshness, s.manualFreshness > 0
		}
		if !known {
			ready.State, ready.Reason = ReadinessStale, stringPtr("freshness_policy_unknown")
		} else if r.CaptureFinishedAt == nil && r.CaptureStartedAt == nil {
			ready.State, ready.Reason = ReadinessStale, stringPtr("capture_boundary_unknown")
		} else if freshnessAt := r.CaptureFinishedAt; freshnessAt != nil && now.Sub(*freshnessAt) > maxAge {
			ready.State, ready.Reason = ReadinessStale, stringPtr("freshness_deadline_missed")
		} else if r.CaptureFinishedAt == nil && now.Sub(*r.CaptureStartedAt) > maxAge {
			ready.State, ready.Reason = ReadinessStale, stringPtr("freshness_deadline_missed")
		} else {
			ready.State, ready.Reason = ReadinessReady, nil
		}
		out[source.SyncID] = ready
	}
	return out, nil
}

func stringPtr(v string) *string { return &v }

// scheduleFreshness derives two intervals plus the ingest allowance from the
// same minimal cron matcher used by the scheduler. Unsupported/irregular
// schedules remain unknown instead of becoming indefinitely fresh.
func scheduleFreshness(expr string) (time.Duration, bool) {
	if strings.TrimSpace(expr) == "" {
		return 0, false
	}
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var hits []time.Time
	for at := start; at.Before(start.Add(32*24*time.Hour)) && len(hits) < 3; at = at.Add(time.Minute) {
		if cronx.Matches(expr, at) {
			hits = append(hits, at)
		}
	}
	if len(hits) < 3 {
		return 0, false
	}
	a, b := hits[1].Sub(hits[0]), hits[2].Sub(hits[1])
	if a <= 0 || a != b {
		return 0, false
	}
	return 2*a + time.Minute, true
}
