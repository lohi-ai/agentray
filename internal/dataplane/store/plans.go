package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Plans store (redesign slice 4): resumable experiments and findings.
//
// The resumable-experiment contract: every field an agent needs to continue
// an experiment without chat history is persisted on the row — observation
// link, evidence envelope, baseline value/unit/window, audience, owner,
// success/guardrail metrics, review date, append-only outcome entries,
// revision, idempotency. Reads paginate the full history (keyset cursor) and
// support exact-ID reads, so work older than the first page is resumable.

// --- validation test writes ---

// UpdateValidationTest edits a proposed test's mutable fields under a
// revision check. Only `proposed` rows are editable — a committed test is the
// owner's agreement and cannot be rewritten underneath them; a decided test
// is history. Runs on any pgQuerier so the idempotent claim wrapper can
// execute it inside the claim transaction.
func updateValidationTest(ctx context.Context, q pgQuerier, projectID, id string, in ValidationTestUpdate, expectedRevision int64) (ValidationTest, error) {
	if in.Hypothesis != nil && strings.TrimSpace(*in.Hypothesis) == "" {
		return ValidationTest{}, errors.New("hypothesis is required")
	}
	if in.MetricEvent != nil && strings.TrimSpace(*in.MetricEvent) == "" {
		return ValidationTest{}, errors.New("metric_event is required")
	}
	if in.TargetCount != nil && *in.TargetCount <= 0 {
		return ValidationTest{}, errors.New("target_count must be greater than zero")
	}
	if in.WindowDays != nil && *in.WindowDays <= 0 {
		return ValidationTest{}, errors.New("window_days must be greater than zero")
	}
	var t ValidationTest
	err := q.QueryRow(ctx, `
UPDATE validation_tests SET
	hypothesis = COALESCE($3::text, hypothesis),
	metric_event = COALESCE($4::text, metric_event),
	baseline_event = COALESCE($5::text, baseline_event),
	target_count = COALESCE($6::int, target_count),
	window_days = COALESCE($7::int, window_days),
	observation_id = COALESCE($8::uuid, observation_id),
	evidence_json = COALESCE($9::jsonb, evidence_json),
	baseline_value = COALESCE($10::float8, baseline_value),
	baseline_unit = COALESCE($11::text, baseline_unit),
	baseline_window = COALESCE($12::text, baseline_window),
	audience = COALESCE($13::text, audience),
	owner = COALESCE($14::text, owner),
	success_metric = COALESCE($15::text, success_metric),
	guardrail_metric = COALESCE($16::text, guardrail_metric),
	review_date = COALESCE($17::timestamptz, review_date),
	revision = coalesce(revision, 1) + 1
WHERE project_id = $1 AND id = $2 AND status = 'proposed' AND coalesce(revision, 1) = $18
RETURNING `+validationTestCols,
		projectID, id, in.Hypothesis, in.MetricEvent, in.BaselineEvent,
		in.TargetCount, in.WindowDays, in.ObservationID, in.EvidenceJSON,
		in.BaselineValue, in.BaselineUnit, in.BaselineWindow, in.Audience,
		in.Owner, in.SuccessMetric, in.GuardrailMetric, in.ReviewDate,
		expectedRevision).
		Scan(scanValidationTestDest(&t)...)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			var exists bool
			var status string
			if qerr := q.QueryRow(ctx,
				`SELECT EXISTS(SELECT 1 FROM validation_tests WHERE project_id = $1 AND id = $2), coalesce((SELECT status FROM validation_tests WHERE project_id = $1 AND id = $2), '')`,
				projectID, id).Scan(&exists, &status); qerr == nil && exists {
				if status != TestProposed {
					return ValidationTest{}, fmt.Errorf("test is %s — only proposed tests are editable", status)
				}
				return ValidationTest{}, ErrRevisionConflict
			}
			return ValidationTest{}, pgx.ErrNoRows
		}
		return ValidationTest{}, err
	}
	return t, nil
}

// ValidationTestUpdate is the mutable-field subset update_test carries.
// Pointer fields distinguish "leave unchanged" (nil) from "set" — an empty
// string clears a text field only where clearing is meaningful.
type ValidationTestUpdate struct {
	Hypothesis      *string
	MetricEvent     *string
	BaselineEvent   *string
	TargetCount     *int
	WindowDays      *int
	ObservationID   *string
	EvidenceJSON    *string
	BaselineValue   *float64
	BaselineUnit    *string
	BaselineWindow  *string
	Audience        *string
	Owner           *string
	SuccessMetric   *string
	GuardrailMetric *string
	ReviewDate      *time.Time
}

// UpdateValidationTest is the non-transactional edge for callers that do not
// need an idempotency receipt.
func (s *Store) UpdateValidationTest(ctx context.Context, projectID, id string, in ValidationTestUpdate, expectedRevision int64) (ValidationTest, error) {
	return updateValidationTest(ctx, s.pg, projectID, id, in, expectedRevision)
}

// UpdateValidationTestIdempotent is updateValidationTest under an idempotency
// claim: one transaction claims the key, mutates, and records the receipt.
func (s *Store) UpdateValidationTestIdempotent(ctx context.Context, projectID, id string, in ValidationTestUpdate, expectedRevision int64, idemKey, requestHash string) (ValidationTest, error) {
	raw, err := s.runIdempotent(ctx, projectID, "update_test", idemKey, requestHash,
		func(ctx context.Context, q pgQuerier) (json.RawMessage, error) {
			t, err := updateValidationTest(ctx, q, projectID, id, in, expectedRevision)
			if err != nil {
				return nil, err
			}
			return json.Marshal(t)
		})
	if err != nil {
		return ValidationTest{}, err
	}
	var t ValidationTest
	if err := json.Unmarshal(raw, &t); err != nil {
		return ValidationTest{}, err
	}
	return t, nil
}

// TestOutcomeEntry is one appended observation on a test's outcome list.
// Entries are immutable — a correction is a new entry, never an overwrite.
type TestOutcomeEntry struct {
	Value       float64 `json:"value"`
	Unit        string  `json:"unit"`
	Window      string  `json:"window"`
	EvidenceRef string  `json:"evidence_ref"`
	AuthorKind  string  `json:"author_kind"` // 'agent' | 'user'
	AuthorID    string  `json:"author_id"`
	RecordedAt  string  `json:"recorded_at"`
}

const (
	// outcomeEntryCap bounds the append-only list per test.
	outcomeEntryCap = 20
	// outcomeEntryBytes bounds one entry's serialized size.
	outcomeEntryBytes = 8 << 10
)

// AppendTestOutcome appends one outcome entry to a committed or terminal
// test under a revision check — atomically: the revision guard, the entry
// append, and the idempotency receipt commit in one transaction via the
// caller's runIdempotent wrapper. Proposed tests reject outcomes (nothing to
// measure against yet); the human decide act writes status+decision_note
// separately and never appends here.
func appendTestOutcome(ctx context.Context, q pgQuerier, projectID, id string, entry TestOutcomeEntry, expectedRevision int64) (ValidationTest, error) {
	entryJSON, err := json.Marshal(entry)
	if err != nil {
		return ValidationTest{}, err
	}
	if len(entryJSON) > outcomeEntryBytes {
		return ValidationTest{}, fmt.Errorf("outcome entry exceeds %d bytes", outcomeEntryBytes)
	}
	var t ValidationTest
	err = q.QueryRow(ctx, `
UPDATE validation_tests SET
	outcome_json = coalesce(outcome_json, '[]'::jsonb) || $3::jsonb,
	revision = coalesce(revision, 1) + 1
WHERE project_id = $1 AND id = $2
  AND status IN ('committed','passed','failed','abandoned')
  AND coalesce(revision, 1) = $4
  AND coalesce(jsonb_array_length(coalesce(outcome_json, '[]'::jsonb)), 0) < $5
RETURNING `+validationTestCols,
		projectID, id, string(entryJSON), expectedRevision, outcomeEntryCap).
		Scan(scanValidationTestDest(&t)...)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			var exists bool
			var status string
			var count int
			if qerr := q.QueryRow(ctx, `
SELECT EXISTS(SELECT 1 FROM validation_tests WHERE project_id = $1 AND id = $2),
       coalesce((SELECT status FROM validation_tests WHERE project_id = $1 AND id = $2), ''),
       coalesce((SELECT jsonb_array_length(coalesce(outcome_json,'[]'::jsonb)) FROM validation_tests WHERE project_id = $1 AND id = $2), 0)`,
				projectID, id).Scan(&exists, &status, &count); qerr == nil && exists {
				if status == TestProposed {
					return ValidationTest{}, fmt.Errorf("test is proposed — outcomes append on committed or decided tests")
				}
				if count >= outcomeEntryCap {
					return ValidationTest{}, fmt.Errorf("outcome list is full (%d entries)", outcomeEntryCap)
				}
				return ValidationTest{}, ErrRevisionConflict
			}
			return ValidationTest{}, pgx.ErrNoRows
		}
		return ValidationTest{}, err
	}
	return t, nil
}

// AppendTestOutcomeIdempotent is appendTestOutcome under an idempotency
// claim — the revision check, entry append and receipt are one transaction.
func (s *Store) AppendTestOutcomeIdempotent(ctx context.Context, projectID, id string, entry TestOutcomeEntry, expectedRevision int64, idemKey, requestHash string) (ValidationTest, error) {
	raw, err := s.runIdempotent(ctx, projectID, "record_outcome", idemKey, requestHash,
		func(ctx context.Context, q pgQuerier) (json.RawMessage, error) {
			t, err := appendTestOutcome(ctx, q, projectID, id, entry, expectedRevision)
			if err != nil {
				return nil, err
			}
			return json.Marshal(t)
		})
	if err != nil {
		return ValidationTest{}, err
	}
	var t ValidationTest
	if err := json.Unmarshal(raw, &t); err != nil {
		return ValidationTest{}, err
	}
	return t, nil
}

// AbandonValidationTest closes a proposed test — the one additive transition
// this slice adds: proposed → abandoned only. Committed tests still close
// through the session-only decide path (owner agreement is a human act).
func abandonValidationTest(ctx context.Context, q pgQuerier, projectID, id, reason string, expectedRevision int64) (ValidationTest, error) {
	var t ValidationTest
	err := q.QueryRow(ctx, `
UPDATE validation_tests SET
	status = 'abandoned', decided_at = now(), decision_note = $3,
	revision = coalesce(revision, 1) + 1
WHERE project_id = $1 AND id = $2 AND status = 'proposed' AND coalesce(revision, 1) = $4
RETURNING `+validationTestCols,
		projectID, id, reason, expectedRevision).
		Scan(scanValidationTestDest(&t)...)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			var exists bool
			var status string
			if qerr := q.QueryRow(ctx,
				`SELECT EXISTS(SELECT 1 FROM validation_tests WHERE project_id = $1 AND id = $2), coalesce((SELECT status FROM validation_tests WHERE project_id = $1 AND id = $2), '')`,
				projectID, id).Scan(&exists, &status); qerr == nil && exists {
				if status != TestProposed {
					return ValidationTest{}, fmt.Errorf("test is %s — only proposed tests can be abandoned this way; committed tests close through decide", status)
				}
				return ValidationTest{}, ErrRevisionConflict
			}
			return ValidationTest{}, pgx.ErrNoRows
		}
		return ValidationTest{}, err
	}
	return t, nil
}

// AbandonValidationTestIdempotent is abandonValidationTest under an
// idempotency claim.
func (s *Store) AbandonValidationTestIdempotent(ctx context.Context, projectID, id, reason string, expectedRevision int64, idemKey, requestHash string) (ValidationTest, error) {
	raw, err := s.runIdempotent(ctx, projectID, "abandon_test", idemKey, requestHash,
		func(ctx context.Context, q pgQuerier) (json.RawMessage, error) {
			t, err := abandonValidationTest(ctx, q, projectID, id, reason, expectedRevision)
			if err != nil {
				return nil, err
			}
			return json.Marshal(t)
		})
	if err != nil {
		return ValidationTest{}, err
	}
	var t ValidationTest
	if err := json.Unmarshal(raw, &t); err != nil {
		return ValidationTest{}, err
	}
	return t, nil
}

// scanValidationTestDest mirrors scanValidationTest's field order for the
// QueryRow paths above (which need a dest list, not a rowScanner).
func scanValidationTestDest(t *ValidationTest) []any {
	return []any{&t.ID, &t.ProjectID, &t.RunID, &t.Hypothesis, &t.MetricEvent, &t.BaselineEvent,
		&t.TargetCount, &t.WindowDays, &t.Status, &t.CommittedAt, &t.DecidedAt, &t.DecisionNote, &t.CreatedAt,
		&t.ObservationID, &t.EvidenceJSON, &t.BaselineValue, &t.BaselineUnit, &t.BaselineWindow,
		&t.Audience, &t.Owner, &t.SuccessMetric, &t.GuardrailMetric, &t.ReviewDate, &t.OutcomeJSON,
		&t.Revision}
}

// --- paginated reads ---

// ListValidationTestsPage returns one keyset page of a project's tests —
// open states first, then decided, each newest first — plus the cursor for
// the next page. Unlike the capped list, this walks the full history so an
// agent can resume work older than the first page.
func (s *Store) ListValidationTestsPage(ctx context.Context, projectID, cursor string, limit int) ([]ValidationTest, string, error) {
	if limit <= 0 || limit > 50 {
		limit = 50
	}
	// Keyset on (group_rank, created_at, id): group_rank orders open before
	// decided; the (created_at, id) pair paginates within and across groups.
	var cursorTime time.Time
	var cursorID string
	var cursorRank int
	if cursor != "" {
		parts := strings.SplitN(cursor, "|", 3)
		if len(parts) != 3 {
			return nil, "", fmt.Errorf("invalid cursor")
		}
		if _, err := fmt.Sscanf(parts[0], "%d", &cursorRank); err != nil {
			return nil, "", fmt.Errorf("invalid cursor")
		}
		var perr error
		cursorTime, perr = time.Parse(time.RFC3339Nano, parts[1])
		if perr != nil {
			return nil, "", fmt.Errorf("invalid cursor")
		}
		cursorID = parts[2]
	}
	rows, err := s.pg.Query(ctx, `
SELECT `+validationTestCols+`
FROM validation_tests
WHERE project_id = $1
  AND ($2::text = '' OR
       (CASE status WHEN 'proposed' THEN 0 WHEN 'committed' THEN 1 ELSE 2 END,
        created_at, id::text) > ($3, $4::timestamptz, $5))
ORDER BY CASE status WHEN 'proposed' THEN 0 WHEN 'committed' THEN 1 ELSE 2 END,
         created_at ASC, id ASC
LIMIT $6`,
		projectID, cursor, cursorRank, cursorTime, cursorID, limit+1)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	out := []ValidationTest{}
	for rows.Next() {
		var t ValidationTest
		if err := rows.Scan(scanValidationTestDest(&t)...); err != nil {
			return nil, "", err
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	next := ""
	if len(out) > limit {
		out = out[:limit]
		last := out[len(out)-1]
		rank := 2
		switch last.Status {
		case TestProposed:
			rank = 0
		case TestCommitted:
			rank = 1
		}
		next = fmt.Sprintf("%d|%s|%s", rank, last.CreatedAt.UTC().Format(time.RFC3339Nano), last.ID)
	}
	return out, next, nil
}

// ListRecommendationsPage returns one keyset page of a project's findings:
// current open findings by impact first, followed by the historical record.
func (s *Store) ListRecommendationsPage(ctx context.Context, projectID, cursor string, limit int) ([]AgentRecommendation, string, error) {
	if limit <= 0 || limit > 50 {
		limit = 50
	}
	var cursorOpen bool
	var cursorImpact float64
	var cursorTime time.Time
	var cursorID string
	if cursor != "" {
		parts := strings.SplitN(cursor, "|", 4)
		if len(parts) != 4 {
			return nil, "", fmt.Errorf("invalid cursor")
		}
		var err error
		cursorOpen, err = strconv.ParseBool(parts[0])
		if err != nil {
			return nil, "", fmt.Errorf("invalid cursor")
		}
		cursorImpact, err = strconv.ParseFloat(parts[1], 64)
		if err != nil {
			return nil, "", fmt.Errorf("invalid cursor")
		}
		cursorTime, err = time.Parse(time.RFC3339Nano, parts[2])
		if err != nil {
			return nil, "", fmt.Errorf("invalid cursor")
		}
		cursorID = parts[3]
		if !looksLikeUUID(cursorID) {
			return nil, "", fmt.Errorf("invalid cursor")
		}
	}
	// A NULL id keeps the tuple's $6::uuid cast from ever seeing '' — the OR
	// short-circuits on an empty cursor, but the cast must not depend on that.
	var cursorIDArg any
	if cursor != "" {
		cursorIDArg = cursorID
	}
	rows, err := s.pg.Query(ctx, `
SELECT id::text, project_id::text, coalesce(run_id::text,''), category, title, rationale,
       evidence_json::text, impact_score, status, ack_note, created_at, seen_count, last_seen_at,
       coalesce(revision, 1)
FROM agent_recommendations
WHERE project_id = $1
  AND ($2::text = '' OR
       ((status = 'open'), impact_score, created_at, id) <
       ($3::bool, $4::float8, $5::timestamptz, $6::uuid))
ORDER BY (status = 'open') DESC, impact_score DESC, created_at DESC, id DESC
LIMIT $7`, projectID, cursor, cursorOpen, cursorImpact, cursorTime, cursorIDArg, limit+1)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	out := []AgentRecommendation{}
	for rows.Next() {
		var r AgentRecommendation
		if err := rows.Scan(&r.ID, &r.ProjectID, &r.RunID, &r.Category, &r.Title, &r.Rationale,
			&r.EvidenceJSON, &r.ImpactScore, &r.Status, &r.AckNote, &r.CreatedAt,
			&r.SeenCount, &r.LastSeenAt, &r.Revision); err != nil {
			return nil, "", err
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	next := ""
	if len(out) > limit {
		out = out[:limit]
		last := out[len(out)-1]
		next = fmt.Sprintf("%t|%s|%s|%s", last.Status == "open",
			strconv.FormatFloat(last.ImpactScore, 'g', -1, 64),
			last.CreatedAt.UTC().Format(time.RFC3339Nano), last.ID)
	}
	return out, next, nil
}

// RecommendationForProject reads one finding by id, project-scoped — the
// exact-ID read that makes an old finding resumable without paging.
func (s *Store) RecommendationForProject(ctx context.Context, projectID, id string) (AgentRecommendation, error) {
	if strings.TrimSpace(id) == "" || !looksLikeUUID(id) {
		return AgentRecommendation{}, errNoSuchTest
	}
	var r AgentRecommendation
	err := s.pg.QueryRow(ctx, `
SELECT id::text, project_id::text, coalesce(run_id::text,''), category, title, rationale,
       evidence_json::text, impact_score, status, ack_note, created_at, seen_count, last_seen_at,
       coalesce(revision, 1)
FROM agent_recommendations WHERE project_id = $1 AND id = $2`, projectID, id).
		Scan(&r.ID, &r.ProjectID, &r.RunID, &r.Category, &r.Title, &r.Rationale,
			&r.EvidenceJSON, &r.ImpactScore, &r.Status, &r.AckNote, &r.CreatedAt,
			&r.SeenCount, &r.LastSeenAt, &r.Revision)
	if errors.Is(err, pgx.ErrNoRows) {
		return AgentRecommendation{}, errNoSuchTest
	}
	return r, err
}

// --- dataset preview ---

// DatasetPreviewRow is one landed external row as the dataset surface reads
// it — the raw JSON plus the cursor it landed under.
type DatasetPreviewRow struct {
	RowKey   string `json:"row_key"`
	Cursor   string `json:"cursor"`
	DataJSON string `json:"data"`
	SyncedAt string `json:"synced_at"`
}

// DatasetPreview is the dataset-semantics read: rows through the deduped
// FINAL view with the soft-delete filter applied, plus the freshness block
// the UI and agents need to trust or distrust what they see.
type DatasetPreview struct {
	Sync ConnectorSync       `json:"sync"`
	Rows []DatasetPreviewRow `json:"rows"`
	// LandedWatermark is the max cursor actually present in the landing table
	// — the honest frontier, which can lag the resume cursor when a run
	// failed mid-pull (the cursor only advances past landed rows, but a crash
	// between landing and checkpointing leaves durable rows ahead of it).
	LandedWatermark string `json:"landed_watermark"`
	// TotalRows is the deduped row count after the soft-delete filter.
	TotalRows int64 `json:"total_rows"`
}

// DatasetPreviewForProject reads one sync's landed rows through FINAL +
// soft-delete filter, project-scoped. run_sql stays raw — this is the
// dataset-semantics path and the only one that applies the filter.
func (s *Store) DatasetPreviewForProject(ctx context.Context, projectID, syncID string, limit int) (DatasetPreview, error) {
	if limit <= 0 || limit > 50 {
		limit = 50
	}
	var sync ConnectorSync
	err := s.pg.QueryRow(ctx, `
SELECT `+connectorSyncColumns+`
FROM connector_syncs WHERE project_id = $1 AND id = $2`, projectID, syncID).
		Scan(syncScanDest(&sync)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return DatasetPreview{}, errNoSuchTest
	}
	if err != nil {
		return DatasetPreview{}, err
	}

	// Soft-delete filter: applied AFTER FINAL so the newest version of a row
	// decides whether it is deleted. bool_true reads a JSON boolean;
	// non_null treats any present non-null value (e.g. a deleted_at
	// timestamp) as deleted.
	softFilter := ""
	if sync.DeletionMode == "soft_column" && sync.SoftDeleteColumn != "" {
		col := strings.ReplaceAll(sync.SoftDeleteColumn, `'`, `\'`)
		switch sync.SoftDeleteSemantics {
		case "bool_true":
			softFilter = ` AND NOT (JSONHas(data, '` + col + `') AND JSONExtractBool(data, '` + col + `'))`
		case "non_null":
			// JSONExtractRaw returns a non-Nullable String, so isNull() on it is
			// constant-false and deleted_at:null would hide a live row. Compare
			// the raw text: JSON null extracts to 'null', a real value does not.
			softFilter = ` AND NOT (JSONHas(data, '` + col + `') AND JSONExtractRaw(data, '` + col + `') != 'null')`
		}
	}

	out := DatasetPreview{Sync: sync, Rows: []DatasetPreviewRow{}}
	if err := s.ch.QueryRow(ctx, `
SELECT count(), coalesce(max(cursor), '')
FROM external_rows FINAL
WHERE project_id = ? AND connector_id = ? AND table_name = ?`+softFilter,
		sync.ProjectID, sync.ConnectorID, sync.SourceTable).
		Scan(&out.TotalRows, &out.LandedWatermark); err != nil {
		return out, err
	}
	rows, err := s.ch.Query(ctx, `
SELECT row_key, cursor, data, toString(synced_at)
FROM external_rows FINAL
WHERE project_id = ? AND connector_id = ? AND table_name = ?`+softFilter+`
ORDER BY cursor DESC, row_key ASC
LIMIT ?`, sync.ProjectID, sync.ConnectorID, sync.SourceTable, limit)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var r DatasetPreviewRow
		if err := rows.Scan(&r.RowKey, &r.Cursor, &r.DataJSON, &r.SyncedAt); err != nil {
			return out, err
		}
		out.Rows = append(out.Rows, r)
	}
	return out, rows.Err()
}

// CreateRecommendationIdempotent is CreateRecommendation under an idempotency
// claim — a retried submit_recommendation replays the stored receipt instead
// of folding into (or duplicating) a finding twice.
func (s *Store) CreateRecommendationIdempotent(ctx context.Context, rec AgentRecommendation, idemKey, requestHash string) (string, error) {
	raw, err := s.runIdempotent(ctx, rec.ProjectID, "submit_recommendation", idemKey, requestHash,
		func(ctx context.Context, q pgQuerier) (json.RawMessage, error) {
			id, err := createRecommendation(ctx, q, rec)
			if err != nil {
				return nil, err
			}
			return json.Marshal(map[string]string{"recommendation_id": id})
		})
	if err != nil {
		return "", err
	}
	var out struct {
		ID string `json:"recommendation_id"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", err
	}
	return out.ID, nil
}
