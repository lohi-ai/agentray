package storage

import (
	"context"
	"database/sql"
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
	AuthorKind  string  `json:"author_kind"` // 'agent' | 'user' | 'system'
	AuthorID    string  `json:"author_id"`
	RecordedAt  string  `json:"recorded_at"`
	// Note carries the prose the numbers alone cannot: the auto-close writes
	// the guardrail delta and the reason an inconclusive close happened here,
	// so the append-only list is self-explanatory without the decision note.
	Note string `json:"note,omitempty"`
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
  AND status IN ('committed','passed','failed','abandoned','inconclusive')
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

// closeValidationTestSystem is the scheduled auto-close's write: the measured
// outcome entry and the terminal status land in ONE statement, so a crash can
// never leave a decided test without its evidence or an open test with a
// closing entry. The `status = 'committed'` guard is the whole concurrency
// contract — a test the owner decided between the due-read and this write is
// unreachable, and a replayed tick re-selects nothing.
//
// The outcome append is best-effort inside the same statement: a test whose
// outcome list is already at the cap still closes (the measurement lives in
// decision_note), because a full evidence list must not pin a test open
// forever.
func closeValidationTestSystem(ctx context.Context, q pgQuerier, projectID, id, status, note string, entry TestOutcomeEntry) (ValidationTest, error) {
	if status != TestPassed && status != TestFailed && status != TestInconclusive {
		return ValidationTest{}, fmt.Errorf("system close must be passed, failed or inconclusive, got %q", status)
	}
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
	outcome_json = CASE
		WHEN coalesce(jsonb_array_length(coalesce(outcome_json, '[]'::jsonb)), 0) < $6
		THEN coalesce(outcome_json, '[]'::jsonb) || $5::jsonb
		ELSE outcome_json END,
	status = $3, decided_at = now(), decision_note = $4,
	revision = coalesce(revision, 1) + 1
WHERE project_id = $1 AND id = $2 AND status = 'committed'
RETURNING `+validationTestCols,
		projectID, id, status, note, string(entryJSON), outcomeEntryCap).
		Scan(scanValidationTestDest(&t)...)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Not committed anymore — the owner's decide won the race, or a
			// previous pass already closed it. Either way there is nothing to
			// do and nothing to retry.
			return ValidationTest{}, ErrTestNoLongerCommitted
		}
		return ValidationTest{}, err
	}
	return t, nil
}

// ErrTestNoLongerCommitted marks the benign race where a test left the
// committed state between the due-read and the close write — the owner's
// decide or an earlier pass got there first.
var ErrTestNoLongerCommitted = errors.New("test is no longer committed")

// CloseValidationTestSystemIdempotent is closeValidationTestSystem under an
// idempotency claim — the auto-close always passes its deterministic key, so a
// crashed pass replays the stored receipt instead of double-appending.
func (s *Store) CloseValidationTestSystemIdempotent(ctx context.Context, projectID, id, status, note string, entry TestOutcomeEntry, idemKey, requestHash string) (ValidationTest, error) {
	raw, err := s.runIdempotent(ctx, projectID, "autoclose_test", idemKey, requestHash,
		func(ctx context.Context, q pgQuerier) (json.RawMessage, error) {
			t, err := closeValidationTestSystem(ctx, q, projectID, id, status, note, entry)
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
//
// The order is validationGroupRankSQL's, the same rank the capped list uses:
// `proposed` and `committed` are one open partition, so a committed test from
// yesterday never sorts behind a months-old proposal.
func (s *Store) ListValidationTestsPage(ctx context.Context, projectID, cursor string, limit int) ([]ValidationTest, string, error) {
	if limit <= 0 || limit > 50 {
		limit = 50
	}
	// Keyset on (group_rank, created_at, id): group_rank orders open before
	// decided; the (created_at, id) pair walks newest first within a group.
	// Mixed directions mean one tuple comparison cannot express "after the
	// cursor", so the predicate expands: a later group, or the same group
	// with an older (created_at, id) pair.
	var cursorTime time.Time
	var cursorID string
	var cursorRank int
	if cursor != "" {
		parts := strings.SplitN(cursor, "|", 4)
		if len(parts) != 4 || parts[0] != validationCursorVersion {
			return nil, "", fmt.Errorf("invalid cursor")
		}
		rank, rerr := strconv.Atoi(parts[1])
		if rerr != nil || rank < 0 || rank > 1 {
			return nil, "", fmt.Errorf("invalid cursor")
		}
		cursorRank = rank
		var perr error
		cursorTime, perr = time.Parse(time.RFC3339Nano, parts[2])
		if perr != nil {
			return nil, "", fmt.Errorf("invalid cursor")
		}
		cursorID = parts[3]
		if !looksLikeUUID(cursorID) {
			return nil, "", fmt.Errorf("invalid cursor")
		}
	}
	// A NULL id keeps the tuple's $5::uuid cast from ever seeing '' — the OR
	// short-circuits on an empty cursor, but the cast must not depend on that.
	var cursorIDArg any
	if cursor != "" {
		cursorIDArg = cursorID
	}
	rows, err := s.pg.Query(ctx, `
SELECT `+validationTestCols+`
FROM validation_tests
WHERE project_id = $1
  AND ($2::text = '' OR
       `+validationGroupRankSQL+` > $3
       OR (`+validationGroupRankSQL+` = $3
           AND (created_at, id) < ($4::timestamptz, $5::uuid)))
ORDER BY `+validationGroupRankSQL+`,
         created_at DESC, id DESC
LIMIT $6`,
		projectID, cursor, cursorRank, cursorTime, cursorIDArg, limit+1)
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
		next = fmt.Sprintf("%s|%d|%s|%s", validationCursorVersion, validationGroupRank(last.Status),
			last.CreatedAt.UTC().Format(time.RFC3339Nano), last.ID)
	}
	return out, next, nil
}

// recommendationsPageOrder is the one ordering contract for the findings
// page: open findings before settled ones, then impact, then the last_seen_at
// recurrence rewrites, with the primary key as the total tiebreak. The keyset
// predicate, the ORDER BY, the capped readout and the declared index all use
// this one string, so the page cannot be sorted by one key and indexed by
// another — which is exactly how the created_at page ended up paging a
// recurring finding behind a stale later-created one.
const recommendationsPageOrder = `(status = 'open') DESC, impact_score DESC, last_seen_at DESC, id DESC`

// recommendationsPlansPageIndex is the findings page index. It is deliberately
// a new name: the pre-change database already materialized an index under the
// old name on created_at, and CREATE INDEX IF NOT EXISTS would silently keep
// it — leaving the page unindexed. The old index is left in place, inert; this
// ticket drops nothing.
const recommendationsPlansPageIndex = "agent_recommendations_plans_lastseen_idx"

// recommendationsPlansPageIndexDDL is the declaration migrateAgent runs. The
// key is written out here rather than assembled from recommendationsPageOrder
// on purpose: the schema test compares the two strings, so the index and the
// query can only agree by being edited together.
const recommendationsPlansPageIndexDDL = `CREATE INDEX IF NOT EXISTS agent_recommendations_plans_lastseen_idx
	ON agent_recommendations (project_id, (status = 'open') DESC, impact_score DESC, last_seen_at DESC, id DESC)`

// recommendationsPageSQL is the findings keyset page. The row comparison is
// the keyset half of recommendationsPageOrder: a total ordering because
// status, impact_score, last_seen_at and id are all NOT NULL.
const recommendationsPageSQL = `
SELECT id::text, project_id::text, coalesce(run_id::text,''), category, title, rationale,
       evidence_json::text, impact_score, status, ack_note, created_at, seen_count, last_seen_at,
       coalesce(revision, 1), source, coalesce(dedupe_key, '')
FROM agent_recommendations
WHERE project_id = $1
  AND ($2::text = '' OR
       ((status = 'open'), impact_score, last_seen_at, id) <
       ($3::bool, $4::float8, $5::timestamptz, $6::uuid))
ORDER BY ` + recommendationsPageOrder + `
LIMIT $7`

// recommendationsCursorVersion prefixes every findings cursor. The cursor's
// timestamp half changed from created_at to last_seen_at, and the two agree in
// type but not in meaning — a pre-change cursor would silently resume at the
// wrong row. The version turns that into an explicit "invalid cursor".
const recommendationsCursorVersion = "v2"

// ListRecommendationsPage returns one keyset page of a project's findings:
// current open findings by impact first, followed by the historical record.
// Within an impact band, last_seen_at decides, so a finding a recurring agent
// just folded forward leads rather than hiding behind a staler later-created
// one.
func (s *Store) ListRecommendationsPage(ctx context.Context, projectID, cursor string, limit int) ([]AgentRecommendation, string, error) {
	if limit <= 0 || limit > 50 {
		limit = 50
	}
	var cursorOpen bool
	var cursorImpact float64
	var cursorTime time.Time
	var cursorID string
	if cursor != "" {
		parts := strings.SplitN(cursor, "|", 5)
		if len(parts) != 5 || parts[0] != recommendationsCursorVersion {
			return nil, "", fmt.Errorf("invalid cursor")
		}
		var err error
		cursorOpen, err = strconv.ParseBool(parts[1])
		if err != nil {
			return nil, "", fmt.Errorf("invalid cursor")
		}
		cursorImpact, err = strconv.ParseFloat(parts[2], 64)
		if err != nil {
			return nil, "", fmt.Errorf("invalid cursor")
		}
		cursorTime, err = time.Parse(time.RFC3339Nano, parts[3])
		if err != nil {
			return nil, "", fmt.Errorf("invalid cursor")
		}
		cursorID = parts[4]
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
	rows, err := s.pg.Query(ctx, recommendationsPageSQL,
		projectID, cursor, cursorOpen, cursorImpact, cursorTime, cursorIDArg, limit+1)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	out := []AgentRecommendation{}
	for rows.Next() {
		var r AgentRecommendation
		if err := rows.Scan(&r.ID, &r.ProjectID, &r.RunID, &r.Category, &r.Title, &r.Rationale,
			&r.EvidenceJSON, &r.ImpactScore, &r.Status, &r.AckNote, &r.CreatedAt,
			&r.SeenCount, &r.LastSeenAt, &r.Revision, &r.Source, &r.DedupeKey); err != nil {
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
		next = fmt.Sprintf("%s|%t|%s|%s|%s", recommendationsCursorVersion, last.Status == "open",
			strconv.FormatFloat(last.ImpactScore, 'g', -1, 64),
			last.LastSeenAt.UTC().Format(time.RFC3339Nano), last.ID)
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
       coalesce(revision, 1), source, coalesce(dedupe_key, '')
FROM agent_recommendations WHERE project_id = $1 AND id = $2`, projectID, id).
		Scan(&r.ID, &r.ProjectID, &r.RunID, &r.Category, &r.Title, &r.Rationale,
			&r.EvidenceJSON, &r.ImpactScore, &r.Status, &r.AckNote, &r.CreatedAt,
			&r.SeenCount, &r.LastSeenAt, &r.Revision, &r.Source, &r.DedupeKey)
	if errors.Is(err, pgx.ErrNoRows) {
		return AgentRecommendation{}, errNoSuchTest
	}
	return r, err
}

// --- dataset preview ---

// softDeleteCondition renders the positive "this row is deleted" predicate for
// one sync's soft-delete contract, applied to the landed row: external_rows
// keeps one row per row_key — a re-sync replaces it in place — so there is no
// duplicate version left for a read to collapse. bool_true reads a JSON
// boolean; non_null treats any PRESENT non-null value (e.g. a deleted_at
// timestamp) as deleted. Both read json_extract_string, which returns SQL NULL
// for a missing key AND for a JSON null: json_extract does not (it yields a
// JSON 'null', which IS NOT NULL), which is how `deleted_at: null` came to
// hide a live row.
// Both read paths — dataset_preview and the scoped_external_rows CTE behind
// run_sql — build their filter from this one function so they can never
// disagree about which rows are deleted.
func softDeleteCondition(column, semantics string) string {
	// sqlQuote escapes backslashes before quotes — a column name like
	// `a\b` or a trailing backslash must not corrupt the literal.
	col := sqlQuote(column)
	switch semantics {
	case "bool_true":
		// json_extract_string returns NULL both for a missing key and for a
		// value that will not cast, and under three-valued logic `NOT (NULL)`
		// is NULL — which drops a LIVE row. The predicate is therefore total
		// rather than "false for rows without the column": only an explicit
		// true marks a row deleted, so missing, false and malformed all read
		// as live.
		return `coalesce(try_cast(json_extract_string(data, ` + col + `) AS BOOLEAN), false)`
	case "non_null":
		// A null-valued marker is a live row: `deleted_at: null` is what an
		// un-deleted row carries, and only a present value marks it deleted.
		return `json_extract_string(data, ` + col + `) IS NOT NULL`
	}
	return ""
}

// softDeleteRule is one sync's deletion contract reduced to what the scoped
// read needs: which landing table it governs and how the deletion mark reads.
type softDeleteRule struct {
	ConnectorID string
	Table       string
	Column      string
	Semantics   string
}

// softDeleteRulesForProject loads every soft-delete contract in the project.
// run_sql applies these inside scoped_external_rows so a row the source marked
// deleted reads as gone on every path, not just in dataset_preview.
func (s *Store) softDeleteRulesForProject(ctx context.Context, projectID string) ([]softDeleteRule, error) {
	rows, err := s.pg.Query(ctx, `
SELECT connector_id::text, source_table, soft_delete_column, soft_delete_semantics
FROM connector_syncs
WHERE project_id = $1 AND deletion_mode = 'soft_column' AND soft_delete_column != ''`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var rules []softDeleteRule
	for rows.Next() {
		var r softDeleteRule
		if err := rows.Scan(&r.ConnectorID, &r.Table, &r.Column, &r.Semantics); err != nil {
			return nil, err
		}
		rules = append(rules, r)
	}
	return rules, rows.Err()
}

// datasetWarnings states the honesty limits a reader must know before
// trusting the rows: the grain (current state, not history), deletion
// coverage, and the replacement tie-break. They are standing facts about the
// dataset, not transient errors, and they ship in the preview payload so
// external agents and built-in clients read the same contract.
func datasetWarnings(sync ConnectorSync) []string {
	w := []string{
		"current state only — one row per row_key; a re-sync replaces the row, it does not append history",
	}
	if sync.DeletionMode == "soft_column" && sync.SoftDeleteColumn != "" {
		w = append(w, "rows marked deleted by "+sync.SoftDeleteColumn+" are excluded; hard deletes in the source are not tracked")
	} else {
		w = append(w, "deletions are not tracked — a row removed in the source stays in this dataset until the source marks it")
	}
	// synced_at is wall-clock per batch; a deterministic landing version is
	// designed but deferred (engine change). Until then the tie is honest.
	w = append(w, "two batches landing in the same millisecond tie on synced_at and an arbitrary version wins")
	return w
}

// DatasetPreviewRow is one landed external row as the dataset surface reads
// it — the raw JSON plus the cursor it landed under.
type DatasetPreviewRow struct {
	RowKey   string `json:"row_key"`
	Cursor   string `json:"cursor"`
	DataJSON string `json:"data"`
	SyncedAt string `json:"synced_at"`
}

// DatasetPreview is the dataset-semantics read: the landed rows — one per
// row_key, replaced in place by a re-sync — with the soft-delete filter applied,
// plus the freshness block the UI and agents need to trust or distrust what
// they see.
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
	// Warnings carries the standing honesty limits — grain, deletion
	// coverage, replacement tie-break — so no client has to re-derive them.
	Warnings []string `json:"warnings,omitempty"`
}

// DatasetPreviewForProject reads one sync's landed rows with the soft-delete
// filter applied, project-scoped. The scoped_external_rows CTE behind
// run_sql applies the same predicate (softDeleteCondition), so the preview
// and SQL can never disagree about which rows are deleted.
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

	softFilter := ""
	if sync.DeletionMode == "soft_column" && sync.SoftDeleteColumn != "" {
		if cond := softDeleteCondition(sync.SoftDeleteColumn, sync.SoftDeleteSemantics); cond != "" {
			softFilter = ` AND NOT (` + cond + `)`
		}
	}

	out := DatasetPreview{Sync: sync, Rows: []DatasetPreviewRow{}, Warnings: datasetWarnings(sync)}
	var total uint64
	if err := s.duckQueryRow(ctx, `
SELECT count(*), coalesce(max(cursor), '')
FROM external_rows
WHERE project_id = ? AND connector_id = ? AND table_name = ?`+softFilter,
		[]any{sync.ProjectID, sync.ConnectorID, sync.SourceTable},
		&total, &out.LandedWatermark); err != nil {
		return out, err
	}
	out.TotalRows = int64(total)
	err = s.duckQuery(ctx, `
SELECT row_key, cursor, data, synced_at::VARCHAR
FROM external_rows
WHERE project_id = ? AND connector_id = ? AND table_name = ?`+softFilter+`
ORDER BY cursor DESC, row_key ASC
LIMIT ?`, []any{sync.ProjectID, sync.ConnectorID, sync.SourceTable, limit}, func(rows *sql.Rows) error {
		var r DatasetPreviewRow
		if err := rows.Scan(&r.RowKey, &r.Cursor, &r.DataJSON, &r.SyncedAt); err != nil {
			return err
		}
		out.Rows = append(out.Rows, r)
		return nil
	})
	return out, err
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
