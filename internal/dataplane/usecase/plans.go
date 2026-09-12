package usecase

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/lohi-ai/agentray/internal/dataplane/store"
	"github.com/lohi-ai/agentray/internal/shared/opcore"
)

// Plans operations (redesign slice 4): the resumable-experiment write surface
// and dataset preview. All writes are revision-checked and idempotency-keyed;
// all reads are project-scoped and paginated so an agent with no chat history
// can resume any experiment or finding.
//
// Access: the new plans:write class covers create/update/outcome/abandon —
// the narrow opt-in scope for management credentials. remember and
// send_notification stay on growth:write; legacy project keys keep their
// frozen allowlist by name regardless of the class change.

// --- update_test ---

type updateTestInput struct {
	TestID          string   `json:"test_id" desc:"id of the proposed test to edit" required:"true"`
	Revision        int64    `json:"revision" desc:"current revision from test_status/list_tests" required:"true"`
	Hypothesis      *string  `json:"hypothesis"`
	MetricEvent     *string  `json:"metric_event"`
	BaselineEvent   *string  `json:"baseline_event"`
	TargetCount     *int     `json:"target_count"`
	WindowDays      *int     `json:"window_days"`
	ObservationID   *string  `json:"observation_id" desc:"finding id this experiment answers"`
	Evidence        *string  `json:"evidence" desc:"typed envelope JSON: {query_ref, metric_version, dataset_version, range, filters, timezone, watermark}"`
	BaselineValue   *float64 `json:"baseline_value"`
	BaselineUnit    *string  `json:"baseline_unit"`
	BaselineWindow  *string  `json:"baseline_window"`
	Audience        *string  `json:"audience"`
	Owner           *string  `json:"owner"`
	SuccessMetric   *string  `json:"success_metric"`
	GuardrailMetric *string  `json:"guardrail_metric"`
	ReviewDate      *string  `json:"review_date" desc:"RFC3339 decision due date"`
	IdempotencyKey  string   `json:"idempotency_key" desc:"retry-safe write key"`
}

type updateTestOutput struct {
	TestID   string `json:"test_id"`
	Status   string `json:"status"`
	Revision int64  `json:"revision"`
	Note     string `json:"note"`
}

// update_test edits a proposed test's fields under a revision check. Only
// `proposed` rows are editable — a committed test is the owner's agreement
// and cannot be rewritten underneath them.
func updateTest() opcore.Operation[updateTestInput, updateTestOutput] {
	return opcore.Operation[updateTestInput, updateTestOutput]{
		Name:           "update_test",
		Summary:        "Edit a proposed experiment's fields (hypothesis, metrics, evidence, baseline, audience, owner, review date). Revision-checked; only proposed tests are editable.",
		Scope:          "growth_suggest",
		Access:         opcore.AccessPlansWrite,
		MinSessionRole: "member",
		Handler: func(ctx context.Context, cc opcore.CallContext, in updateTestInput) (updateTestOutput, error) {
			d, err := depsFrom(cc)
			if err != nil {
				return updateTestOutput{}, err
			}
			var reviewDate *time.Time
			if in.ReviewDate != nil && *in.ReviewDate != "" {
				t, perr := time.Parse(time.RFC3339, *in.ReviewDate)
				if perr != nil {
					return updateTestOutput{}, errBadInput("review_date must be RFC3339")
				}
				reviewDate = &t
			}
			upd := storage.ValidationTestUpdate{
				Hypothesis: in.Hypothesis, MetricEvent: in.MetricEvent,
				BaselineEvent: in.BaselineEvent, TargetCount: in.TargetCount,
				WindowDays: in.WindowDays, ObservationID: in.ObservationID,
				EvidenceJSON: in.Evidence, BaselineValue: in.BaselineValue,
				BaselineUnit: in.BaselineUnit, BaselineWindow: in.BaselineWindow,
				Audience: in.Audience, Owner: in.Owner,
				SuccessMetric: in.SuccessMetric, GuardrailMetric: in.GuardrailMetric,
				ReviewDate: reviewDate,
			}
			hash, err := requestHash(in)
			if err != nil {
				return updateTestOutput{}, err
			}
			t, err := d.Repo.UpdateValidationTestIdempotent(ctx, cc.ProjectID, in.TestID, upd, in.Revision, strings.TrimSpace(in.IdempotencyKey), hash)
			if err != nil {
				return updateTestOutput{}, err
			}
			return updateTestOutput{TestID: t.ID, Status: t.Status, Revision: t.Revision,
				Note: "Updated. The owner still commits on /start?job=validate — a proposal they have not agreed to is not a threshold."}, nil
		},
	}
}

// --- record_outcome ---

type recordOutcomeInput struct {
	TestID         string  `json:"test_id" required:"true"`
	Revision       int64   `json:"revision" required:"true"`
	Value          float64 `json:"value" desc:"measured outcome value" required:"true"`
	Unit           string  `json:"unit" desc:"e.g. 'weekly return %', 'signups'"`
	Window         string  `json:"window" desc:"measurement window, e.g. 'Sep 1-8'"`
	EvidenceRef    string  `json:"evidence_ref" desc:"resolvable ref: saved query id, metric name+version, or bounded SQL"`
	IdempotencyKey string  `json:"idempotency_key"`
}

type recordOutcomeOutput struct {
	TestID   string `json:"test_id"`
	Status   string `json:"status"`
	Revision int64  `json:"revision"`
	Entries  int    `json:"entries"`
	Note     string `json:"note"`
}

// record_outcome appends one observation to a test's outcome list — the
// append-only contract. Entries are immutable; a correction is a new entry.
// The human decide act (status + note) is separate and never appends here.
func recordOutcome() opcore.Operation[recordOutcomeInput, recordOutcomeOutput] {
	return opcore.Operation[recordOutcomeInput, recordOutcomeOutput]{
		Name:           "record_outcome",
		Summary:        "Append a measured outcome to a committed or decided experiment. Append-only, revision-checked, idempotent; the owner's pass/fail/abandon decision stays a separate human act.",
		Scope:          "growth_suggest",
		Access:         opcore.AccessPlansWrite,
		MinSessionRole: "member",
		Handler: func(ctx context.Context, cc opcore.CallContext, in recordOutcomeInput) (recordOutcomeOutput, error) {
			d, err := depsFrom(cc)
			if err != nil {
				return recordOutcomeOutput{}, err
			}
			authorKind, authorID := "agent", cc.RunID
			if authorID == "" {
				authorKind, authorID = "user", cc.Principal.UserID
			}
			entry := storage.TestOutcomeEntry{
				Value: in.Value, Unit: in.Unit, Window: in.Window,
				EvidenceRef: in.EvidenceRef,
				AuthorKind:  authorKind,
				AuthorID:    authorID,
				RecordedAt:  time.Now().UTC().Format(time.RFC3339),
			}
			hash, err := requestHash(in)
			if err != nil {
				return recordOutcomeOutput{}, err
			}
			t, err := d.Repo.AppendTestOutcomeIdempotent(ctx, cc.ProjectID, in.TestID, entry, in.Revision, strings.TrimSpace(in.IdempotencyKey), hash)
			if err != nil {
				return recordOutcomeOutput{}, err
			}
			entries := 0
			if t.OutcomeJSON != "" {
				var list []json.RawMessage
				if json.Unmarshal([]byte(t.OutcomeJSON), &list) == nil {
					entries = len(list)
				}
			}
			return recordOutcomeOutput{TestID: t.ID, Status: t.Status, Revision: t.Revision, Entries: entries,
				Note: "Outcome recorded. The pass/fail/abandon decision is still the owner's — this entry is evidence, not a verdict."}, nil
		},
	}
}

// --- abandon_test ---

type abandonTestInput struct {
	TestID         string `json:"test_id" required:"true"`
	Revision       int64  `json:"revision" required:"true"`
	Reason         string `json:"reason" desc:"why the proposal is being closed"`
	IdempotencyKey string `json:"idempotency_key"`
}

// abandon_test closes a proposed test — the one additive transition this
// slice adds: proposed → abandoned only. Committed tests still close through
// the session-only decide path; owner agreement is a human act.
func abandonTest() opcore.Operation[abandonTestInput, updateTestOutput] {
	return opcore.Operation[abandonTestInput, updateTestOutput]{
		Name:           "abandon_test",
		Summary:        "Close a proposed experiment without owner commitment (proposed → abandoned only). Committed tests close through the owner's decide action.",
		Scope:          "growth_suggest",
		Access:         opcore.AccessPlansWrite,
		MinSessionRole: "member",
		Handler: func(ctx context.Context, cc opcore.CallContext, in abandonTestInput) (updateTestOutput, error) {
			d, err := depsFrom(cc)
			if err != nil {
				return updateTestOutput{}, err
			}
			hash, err := requestHash(in)
			if err != nil {
				return updateTestOutput{}, err
			}
			t, err := d.Repo.AbandonValidationTestIdempotent(ctx, cc.ProjectID, in.TestID, in.Reason, in.Revision, strings.TrimSpace(in.IdempotencyKey), hash)
			if err != nil {
				return updateTestOutput{}, err
			}
			return updateTestOutput{TestID: t.ID, Status: t.Status, Revision: t.Revision,
				Note: "Abandoned. The proposal is closed; the record stays for history."}, nil
		},
	}
}

// --- list_findings ---

type listFindingsInput struct {
	Cursor    string `json:"cursor" desc:"keyset cursor from a previous page"`
	Limit     int    `json:"limit" desc:"page size, max 50"`
	FindingID string `json:"finding_id" desc:"exact read — one finding by id, ignoring pagination"`
}

type listFindingsOutput struct {
	Findings   []storage.AgentRecommendation `json:"findings,omitempty"`
	Finding    *storage.AgentRecommendation  `json:"finding,omitempty"`
	NextCursor string                        `json:"next_cursor,omitempty"`
}

// list_findings is the resume entry point for findings: paginated full
// history plus exact-ID reads, so an agent with no chat history can pick up
// any finding — not just the newest page.
func listFindings() opcore.Operation[listFindingsInput, listFindingsOutput] {
	return opcore.Operation[listFindingsInput, listFindingsOutput]{
		Name:    "list_findings",
		Summary: "List the project's findings (recommendations) with keyset pagination, or read one by id. The resume entry point — every field needed to continue is on the row.",
		Scope:   "monitor",
		Access:  opcore.AccessAnalyticsRead,
		Handler: func(ctx context.Context, cc opcore.CallContext, in listFindingsInput) (listFindingsOutput, error) {
			d, err := depsFrom(cc)
			if err != nil {
				return listFindingsOutput{}, err
			}
			if in.FindingID != "" {
				r, err := d.Repo.RecommendationForProject(ctx, cc.ProjectID, in.FindingID)
				if err != nil {
					return listFindingsOutput{}, err
				}
				return listFindingsOutput{Finding: &r}, nil
			}
			page, next, err := d.Repo.ListRecommendationsPage(ctx, cc.ProjectID, in.Cursor, in.Limit)
			if err != nil {
				return listFindingsOutput{}, err
			}
			return listFindingsOutput{Findings: page, NextCursor: next}, nil
		},
	}
}

// --- dataset_preview ---

type datasetPreviewInput struct {
	SyncID string `json:"sync_id" desc:"the sync (dataset) id" required:"true"`
	Limit  int    `json:"limit" desc:"rows to preview, max 50"`
}

// dataset_preview is the dataset-semantics read: deduped FINAL rows with the
// soft-delete filter applied, plus the freshness block. run_sql stays raw —
// this is the only path that applies business semantics.
func datasetPreview() opcore.Operation[datasetPreviewInput, storage.DatasetPreview] {
	return opcore.Operation[datasetPreviewInput, storage.DatasetPreview]{
		Name:           "dataset_preview",
		Summary:        "Preview one synced dataset: deduped rows with the soft-delete filter applied, plus freshness (last success, last attempt, landed watermark, resume cursor).",
		Scope:          "data_quality",
		Access:         opcore.AccessSourcesRead,
		MinSessionRole: "admin",
		Handler: func(ctx context.Context, cc opcore.CallContext, in datasetPreviewInput) (storage.DatasetPreview, error) {
			d, err := depsFrom(cc)
			if err != nil {
				return storage.DatasetPreview{}, err
			}
			return d.Repo.DatasetPreviewForProject(ctx, cc.ProjectID, in.SyncID, in.Limit)
		},
	}
}

// errBadInput is a client-error shape for malformed op inputs.
func errBadInput(msg string) error { return &inputError{msg} }

type inputError struct{ msg string }

func (e *inputError) Error() string { return e.msg }
