package storage

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/google/uuid"
)

type ResultCompleteness string

const (
	ResultComplete ResultCompleteness = "complete"
	ResultBounded  ResultCompleteness = "bounded"
	ResultUnknown  ResultCompleteness = "unknown"
)

// QueryMeta describes this successful execution, not historical source
// completeness. AvailabilityReason is nullable so unknown stays distinct from
// an affirmative empty string.
type QueryMeta struct {
	QueryRef             string             `json:"query_ref"`
	QueryDigest          string             `json:"query_digest"`
	ExecutedAt           time.Time          `json:"executed_at"`
	ServingDataWatermark *ServingWatermark  `json:"serving_data_watermark"`
	ResultCompleteness   ResultCompleteness `json:"result_completeness"`
	Truncated            *bool              `json:"truncated"`
	AvailabilityReason   *string            `json:"availability_reason"`
}

type ServingWatermark struct {
	EventLandedAt *time.Time               `json:"event_landed_at"`
	Sources       []ServingSourceWatermark `json:"sources"`
}

type ServingSourceWatermark struct {
	ConnectorID       string     `json:"connector_id"`
	Table             string     `json:"table"`
	Generation        *string    `json:"generation"`
	CaptureStartedAt  *time.Time `json:"capture_started_at"`
	CaptureFinishedAt *time.Time `json:"capture_finished_at"`
	LandedAt          *time.Time `json:"landed_at"`
}

type sandboxEvidence struct {
	Watermark     *ServingWatermark
	Confirmations map[string]sourceConfirmation
	ProjectSeq    string
}

type sourceConfirmation struct {
	MutationSeq uint64
	ConfirmedAt time.Time
}

func (d *DuckDB) projectEvidence(ctx context.Context, projectID string) (sandboxEvidence, error) {
	var evidence sandboxEvidence
	err := d.ReadSnapshot(ctx, func(snapshot duckDBSnapshot) error {
		var err error
		evidence, err = projectEvidenceFrom(ctx, snapshot, projectID)
		return err
	})
	return evidence, err
}

func projectEvidenceFrom(ctx context.Context, snapshot duckDBSnapshot, projectID string) (sandboxEvidence, error) {
	e := sandboxEvidence{Watermark: &ServingWatermark{Sources: []ServingSourceWatermark{}}, Confirmations: map[string]sourceConfirmation{}}
	err := func() error {
		var eventCount, externalCount, eventMillis, externalMillis, sourceMutations int64
		if err := snapshot.QueryRowContext(ctx, `SELECT
(SELECT count(*) FROM events WHERE project_id = ?),
coalesce((SELECT epoch_ms(max(inserted_at)) FROM events WHERE project_id = ?), 0),
(SELECT count(*) FROM external_rows WHERE project_id = ?),
coalesce((SELECT epoch_ms(max(synced_at)) FROM external_rows WHERE project_id = ?), 0),
coalesce((SELECT sum(mutation_seq) FROM data_receipt_sources WHERE project_id = ?), 0)`,
			projectID, projectID, projectID, projectID, projectID).Scan(&eventCount, &eventMillis, &externalCount, &externalMillis, &sourceMutations); err != nil {
			return err
		}
		e.ProjectSeq = fmt.Sprintf("%d:%d:%d:%d:%d", eventCount, eventMillis, externalCount, externalMillis, sourceMutations)
		var eventAt sql.Null[time.Time]
		if err := snapshot.QueryRowContext(ctx, `SELECT max(inserted_at) FROM events WHERE project_id = ?`, projectID).Scan(&eventAt); err != nil {
			return err
		}
		if eventAt.Valid {
			v := eventAt.V.UTC()
			e.Watermark.EventLandedAt = &v
		}
		rows, err := snapshot.QueryContext(ctx, `SELECT connector_id::VARCHAR, table_name, generation::VARCHAR,
capture_started_at, capture_finished_at, landed_at, mutation_seq
FROM data_receipt_sources WHERE project_id = ? AND landed_at IS NOT NULL
ORDER BY updated_at DESC, connector_id, table_name LIMIT 64`, projectID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var w ServingSourceWatermark
			var generation sql.NullString
			var started, finished, landed sql.Null[time.Time]
			var mutation uint64
			if err := rows.Scan(&w.ConnectorID, &w.Table, &generation, &started, &finished, &landed, &mutation); err != nil {
				return err
			}
			if generation.Valid {
				v := generation.String
				w.Generation = &v
			}
			if started.Valid {
				v := started.V.UTC()
				w.CaptureStartedAt = &v
			}
			if finished.Valid {
				v := finished.V.UTC()
				w.CaptureFinishedAt = &v
			}
			if landed.Valid {
				v := landed.V.UTC()
				w.LandedAt = &v
			}
			e.Watermark.Sources = append(e.Watermark.Sources, w)
			e.Confirmations[w.ConnectorID+"\x00"+w.Table] = sourceConfirmation{MutationSeq: mutation}
		}
		return rows.Err()
	}()
	return e, err
}

func queryDigest(projectID, sqlText string) string {
	sum := sha256.Sum256([]byte(projectID + "\x00" + sqlText))
	return hex.EncodeToString(sum[:])
}

func unknownQueryMeta(projectID, sqlText string, now time.Time, reason string) QueryMeta {
	return QueryMeta{QueryRef: uuid.NewString(), QueryDigest: queryDigest(projectID, sqlText), ExecutedAt: now.UTC(),
		ResultCompleteness: ResultUnknown, AvailabilityReason: stringPtr(reason)}
}
