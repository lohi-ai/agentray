package usecase

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/lohi-ai/agentray/internal/dataplane/store"
	"github.com/lohi-ai/agentray/internal/shared/opcore"
)

// annotations.go — the chart-annotation operations: add_annotation,
// list_annotations, delete_annotation, plus the evidence-reference resolution
// that lets a finding or an outcome cite an annotation by id.
//
// Access: list_annotations is an analytics read — a viewer sees the marks on
// the same charts it can already read. add/delete are plans:write at the
// member floor: marking the timeline is the same class of act as filing a
// finding, and a demo visitor or capture key cannot do either.

// annotationRefPrefix is the evidence grammar for citing an annotation: a
// record_outcome evidence_ref of "annotation:<uuid>", or an
// "annotation_ids": ["<uuid>", …] array inside a finding/test evidence
// envelope. Both resolve through AnnotationForProject, so a reference can
// only ever name a row in the caller's own project.
const annotationRefPrefix = "annotation:"

// --- add_annotation ---

type addAnnotationInput struct {
	Label    string  `json:"label" desc:"what changed, e.g. 'v2.4 deploy' (max 140 chars)" required:"true"`
	Kind     string  `json:"kind" desc:"deploy | campaign | price | other (default other)"`
	Link     string  `json:"link" desc:"optional absolute http(s) URL with more context"`
	StartsAt string  `json:"starts_at" desc:"RFC3339 instant the change began" required:"true"`
	EndsAt   *string `json:"ends_at" desc:"optional RFC3339 end — set for a range, omit for an instant"`
	// IdempotencyKey makes a retried add replay the stored receipt instead of
	// marking the chart twice.
	IdempotencyKey string `json:"idempotency_key"`
}

func addAnnotation() opcore.Operation[addAnnotationInput, storage.Annotation] {
	return opcore.Operation[addAnnotationInput, storage.Annotation]{
		Name:           "add_annotation",
		Summary:        "Mark a deploy, campaign, price change or other event on the project's charts. Instant (starts_at) or range (starts_at..ends_at); renders on every temporal chart that spans it.",
		Scope:          "growth_suggest",
		Access:         opcore.AccessPlansWrite,
		MinSessionRole: "member",
		Handler: func(ctx context.Context, cc opcore.CallContext, in addAnnotationInput) (storage.Annotation, error) {
			d, err := depsFrom(cc)
			if err != nil {
				return storage.Annotation{}, err
			}
			startsAt, err := time.Parse(time.RFC3339, strings.TrimSpace(in.StartsAt))
			if err != nil {
				return storage.Annotation{}, errBadInput("starts_at must be RFC3339")
			}
			var endsAt *time.Time
			if in.EndsAt != nil && strings.TrimSpace(*in.EndsAt) != "" {
				t, perr := time.Parse(time.RFC3339, strings.TrimSpace(*in.EndsAt))
				if perr != nil {
					return storage.Annotation{}, errBadInput("ends_at must be RFC3339")
				}
				endsAt = &t
			}
			hash, err := requestHash(in)
			if err != nil {
				return storage.Annotation{}, err
			}
			a, err := d.Repo.CreateAnnotationIdempotent(ctx, cc.ProjectID, storage.AnnotationWrite{
				Label: in.Label, Kind: in.Kind, Link: in.Link,
				StartsAt: startsAt, EndsAt: endsAt,
			}, strings.TrimSpace(in.IdempotencyKey), hash)
			if err != nil {
				return storage.Annotation{}, err
			}
			d.RecordOperationAudit(ctx, cc.Principal, "add_annotation")
			return a, nil
		},
	}
}

// --- list_annotations ---

type listAnnotationsInput struct {
	From  string `json:"from" desc:"RFC3339 window start (default: 90 days ago)"`
	To    string `json:"to" desc:"RFC3339 window end (default: now)"`
	Limit int    `json:"limit" desc:"max annotations 1-500 (default 200)"`
}

type listAnnotationsOutput struct {
	Annotations []storage.Annotation `json:"annotations"`
}

// list_annotations is the overlap-window read every temporal chart performs:
// the annotations whose instant or range intersects [from, to]. An annotation
// outside the window is not returned, so "renders only when it overlaps the
// visible range" is the read's contract, not a client-side filter.
func listAnnotations() opcore.Operation[listAnnotationsInput, listAnnotationsOutput] {
	return opcore.Operation[listAnnotationsInput, listAnnotationsOutput]{
		Name:    "list_annotations",
		Summary: "List the project's chart annotations overlapping a time window — deploys, campaigns, price changes and other marked events.",
		Scope:   "monitor",
		Access:  opcore.AccessAnalyticsRead,
		Handler: func(ctx context.Context, cc opcore.CallContext, in listAnnotationsInput) (listAnnotationsOutput, error) {
			d, err := depsFrom(cc)
			if err != nil {
				return listAnnotationsOutput{}, err
			}
			to := time.Now().UTC()
			if s := strings.TrimSpace(in.To); s != "" {
				t, perr := time.Parse(time.RFC3339, s)
				if perr != nil {
					return listAnnotationsOutput{}, errBadInput("to must be RFC3339")
				}
				to = t
			}
			from := to.Add(-90 * 24 * time.Hour)
			if s := strings.TrimSpace(in.From); s != "" {
				t, perr := time.Parse(time.RFC3339, s)
				if perr != nil {
					return listAnnotationsOutput{}, errBadInput("from must be RFC3339")
				}
				from = t
			}
			if !from.Before(to) {
				return listAnnotationsOutput{}, errBadInput("from must be before to")
			}
			rows, err := d.Repo.AnnotationsForWindow(ctx, cc.ProjectID, from, to, in.Limit)
			if err != nil {
				return listAnnotationsOutput{}, err
			}
			return listAnnotationsOutput{Annotations: rows}, nil
		},
	}
}

// --- delete_annotation ---

type deleteAnnotationInput struct {
	AnnotationID   string `json:"annotation_id" desc:"id of the annotation to remove" required:"true"`
	IdempotencyKey string `json:"idempotency_key"`
}

// delete_annotation removes one annotation, returning the row it removed. A
// retried delete replays the receipt, so a second attempt does not read as a
// fresh not-found.
func deleteAnnotation() opcore.Operation[deleteAnnotationInput, storage.Annotation] {
	return opcore.Operation[deleteAnnotationInput, storage.Annotation]{
		Name:           "delete_annotation",
		Summary:        "Remove a chart annotation by id.",
		Scope:          "growth_suggest",
		Access:         opcore.AccessPlansWrite,
		MinSessionRole: "member",
		Handler: func(ctx context.Context, cc opcore.CallContext, in deleteAnnotationInput) (storage.Annotation, error) {
			d, err := depsFrom(cc)
			if err != nil {
				return storage.Annotation{}, err
			}
			hash, err := requestHash(in)
			if err != nil {
				return storage.Annotation{}, err
			}
			a, err := d.Repo.DeleteAnnotationIdempotent(ctx, cc.ProjectID, strings.TrimSpace(in.AnnotationID), strings.TrimSpace(in.IdempotencyKey), hash)
			if err != nil {
				return storage.Annotation{}, err
			}
			d.RecordOperationAudit(ctx, cc.Principal, "delete_annotation")
			return a, nil
		},
	}
}

// --- evidence references ---

// resolveAnnotationRef checks one "annotation:<id>" citation: the id must
// resolve to an annotation in the caller's project. A bad reference is a
// client error naming the id — never a silent pass, and never a leak about
// another project's rows.
func resolveAnnotationRef(ctx context.Context, d *Deps, projectID, ref string) error {
	id := strings.TrimSpace(strings.TrimPrefix(ref, annotationRefPrefix))
	if id == "" {
		return errBadInput("evidence_ref annotation: reference needs an annotation id")
	}
	if _, err := d.Repo.AnnotationForProject(ctx, projectID, id); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return errBadInput(fmt.Sprintf("evidence_ref annotation:%s does not resolve in this project", id))
		}
		return err
	}
	return nil
}

// resolveAnnotationEnvelope checks the annotation_ids array an evidence
// envelope may carry: when present it must be an array of strings, and every
// id must resolve in the caller's project. Other envelope fields are
// untouched — the envelope contract stays additive.
func resolveAnnotationEnvelope(ctx context.Context, d *Deps, projectID string, env map[string]any) error {
	raw, ok := env["annotation_ids"]
	if !ok {
		return nil
	}
	list, ok := raw.([]any)
	if !ok {
		return errBadInput("evidence.annotation_ids must be an array of annotation ids")
	}
	for _, item := range list {
		id, ok := item.(string)
		if !ok || strings.TrimSpace(id) == "" {
			return errBadInput("evidence.annotation_ids must be an array of annotation ids")
		}
		if _, err := d.Repo.AnnotationForProject(ctx, projectID, strings.TrimSpace(id)); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return errBadInput(fmt.Sprintf("evidence.annotation_ids entry %q does not resolve in this project", id))
			}
			return err
		}
	}
	return nil
}

// annotationRefsInEnvelope parses a raw evidence JSON string and resolves any
// annotation_ids it carries. An unparseable envelope is left to
// validateEvidenceEnvelope, which the caller runs first.
func annotationRefsInEnvelope(ctx context.Context, d *Deps, projectID, raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	var env map[string]any
	if err := json.Unmarshal([]byte(raw), &env); err != nil || env == nil {
		return nil
	}
	return resolveAnnotationEnvelope(ctx, d, projectID, env)
}
