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
)

// DeliveryReceiptMark is the durable identity of one delivery. StreamSeq is
// deliberately separate from the consumer sequence kept by AppliedMark.
type DeliveryReceiptMark struct {
	StreamID      string
	Subject       string
	StreamSeq     uint64
	PayloadSHA256 string
	PublishedAt   *time.Time
	Replayed      bool
	Unverifiable  bool
}

// SourceReceiptMark consumes the frozen C1 envelope identity without owning
// C1's generation or promotion state machine. Promoted may only be set by the
// atomic local-promotion path; a completion marker alone is not landed data.
type SourceReceiptMark struct {
	ProjectID         string
	ConnectorID       string
	Table             string
	SyncID            string
	RunID             string
	Generation        string
	GenerationSeq     uint64
	BindingDigest     string
	BatchID           string
	BatchIndex        *uint64
	PayloadSHA256     string
	ExpectedBatches   *uint64
	ExpectedRows      *uint64
	CaptureStartedAt  *time.Time
	CaptureFinishedAt *time.Time
	PublishedAt       *time.Time
	Complete          bool
	Promoted          bool
}

// SnapshotPromotion is the C1→C2 seam. C1 calls RecordSnapshotPromotionTx in
// the same transaction that swaps staging into external_rows.
type SnapshotPromotion struct {
	SourceReceiptMark
	PromotedAt time.Time
}

type PublicationObservation struct {
	ProjectID, ConnectorID, Table string
	StableBatchID, PayloadSHA256  string
	PublishedAt                   time.Time
}

func (s *Store) migrateDataReadiness(ctx context.Context) error {
	_, err := s.pg.Exec(ctx, `CREATE TABLE IF NOT EXISTS source_publication_receipts (
	project_id UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
	connector_id UUID NOT NULL REFERENCES data_connectors(id) ON DELETE CASCADE,
	table_name VARCHAR(256) NOT NULL,
	stable_batch_id VARCHAR(128) NOT NULL,
	payload_sha256 CHAR(64) NOT NULL,
	first_published_at TIMESTAMPTZ NOT NULL,
	last_observed_at TIMESTAMPTZ NOT NULL,
	PRIMARY KEY (project_id, connector_id, table_name, stable_batch_id)
)`)
	return err
}

func (s *Store) RecordPublication(ctx context.Context, observation PublicationObservation) error {
	tag, err := s.pg.Exec(ctx, `INSERT INTO source_publication_receipts
(project_id, connector_id, table_name, stable_batch_id, payload_sha256, first_published_at, last_observed_at)
VALUES ($1, $2, $3, $4, $5, $6, $6)
ON CONFLICT (project_id, connector_id, table_name, stable_batch_id) DO UPDATE SET
last_observed_at = excluded.last_observed_at
WHERE source_publication_receipts.payload_sha256 = excluded.payload_sha256`,
		observation.ProjectID, observation.ConnectorID, observation.Table, observation.StableBatchID,
		observation.PayloadSHA256, observation.PublishedAt.UTC())
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("publication identity %q conflicts with different content", observation.StableBatchID)
	}
	return nil
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
		if d.Unverifiable {
			if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO data_receipt_holes
(stream_id, subject, stream_seq, payload_sha256, unverifiable) VALUES (?, ?, ?, ?, true)`, d.StreamID, d.Subject, d.StreamSeq, d.PayloadSHA256); err != nil {
				return err
			}
			continue
		}
		if _, err := tx.ExecContext(ctx, `UPDATE data_receipt_holes SET cleared_at = ?
WHERE stream_id = ? AND subject = ? AND stream_seq = ? AND payload_sha256 = ? AND cleared_at IS NULL AND NOT unverifiable`,
			landedAt, d.StreamID, d.Subject, d.StreamSeq, d.PayloadSHA256); err != nil {
			return err
		}
	}
	s := mark.Source
	if s == nil {
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
	var priorGenerationKey string
	var priorGenerationSeq uint64
	err = tx.QueryRowContext(ctx, `SELECT generation_key, generation_seq FROM data_receipt_sources
WHERE project_id = ? AND connector_id = ? AND table_name = ?`, pid, cid, s.Table).Scan(&priorGenerationKey, &priorGenerationSeq)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err == nil {
		if s.GenerationSeq < priorGenerationSeq {
			return nil
		}
		if s.GenerationSeq > 0 && s.GenerationSeq == priorGenerationSeq && generationKey != priorGenerationKey {
			return fmt.Errorf("generation sequence %d conflicts with current generation", s.GenerationSeq)
		}
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
 expected_batches, expected_rows, completion_seen, mutation_seq, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 1, ?)
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
 mutation_seq = data_receipt_sources.mutation_seq + 1,
 updated_at = excluded.updated_at`,
		pid, cid, s.Table, syncID, runID, generation, generationKey, s.GenerationSeq, s.BindingDigest,
		s.CaptureStartedAt, s.CaptureFinishedAt, publishedAt, sourceLandedAt, landedGenerationKey,
		func() any {
			if s.Promoted {
				return landedAt
			}
			return nil
		}(), expectedBatches, expectedRows,
		s.Complete, landedAt); err != nil {
		return err
	}
	return nil
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
	MutationSeq                                                                uint64
	CompletionSeen                                                             bool
	AppliedBatches                                                             uint64
	ExpectedBatches                                                            *uint64
	HasHole                                                                    bool
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
			var generation sql.NullString
			var expected sql.Null[uint64]
			err := conn.QueryRowContext(ctx, `SELECT published_at,
CASE WHEN landed_generation_key = generation_key THEN landed_at ELSE NULL END, last_complete_at,
capture_started_at, capture_finished_at, generation::VARCHAR, mutation_seq, completion_seen, expected_batches,
(SELECT count(*) FROM data_receipt_batches b WHERE b.project_id = s.project_id AND b.connector_id = s.connector_id AND b.table_name = s.table_name
 AND b.generation_key = coalesce(s.generation::VARCHAR, s.run_id::VARCHAR, 'legacy-unknown')),
EXISTS (SELECT 1 FROM data_receipt_holes h WHERE h.cleared_at IS NULL AND
 (h.project_id IS NULL OR (h.project_id = s.project_id AND (h.connector_id IS NULL OR h.connector_id = s.connector_id) AND (h.table_name IS NULL OR h.table_name = s.table_name))))
FROM data_receipt_sources s WHERE project_id = ? AND connector_id = ? AND table_name = ?`,
				projectID, source.ConnectorID, source.Table).Scan(&published, &landed, &complete, &started, &finished,
				&generation, &r.MutationSeq, &r.CompletionSeen, &expected, &r.AppliedBatches, &r.HasHole)
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

// SourceReadiness resolves only evidence from this Store's DuckDB and its live
// sandbox pool. No query is launched merely to make status look healthy.
func (s *Store) SourceReadiness(ctx context.Context, projectID string, sources []ReadinessSource) (map[string]*Readiness, error) {
	local, err := s.duck.localReadiness(ctx, projectID, sources)
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
			out[source.SyncID] = &Readiness{State: ReadinessSyncing, Reason: stringPtr("unknown_receipt")}
			continue
		}
		ready := &Readiness{State: ReadinessSyncing, PublishedAt: r.PublishedAt, LandedAt: r.LandedAt,
			Generation: r.Generation, CaptureStartedAt: r.CaptureStartedAt, CaptureFinishedAt: r.CaptureFinishedAt,
			LastCompleteAt: r.LastCompleteAt}
		if r.HasHole || (r.CompletionSeen && r.ExpectedBatches != nil && r.AppliedBatches != *r.ExpectedBatches) {
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
		if !confirmed || confirmation.MutationSeq != r.MutationSeq {
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
		} else if now.Sub(*r.LandedAt) > maxAge {
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
