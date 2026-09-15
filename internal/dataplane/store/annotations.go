package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// annotations.go — project-scoped chart annotations: the deploy, campaign or
// price change a member marks on a trend so "did X cause this movement" is
// answerable in-product.
//
// An annotation is an instant (starts_at only) or a range (starts_at..ends_at).
// Reads are overlap-windowed: a chart asks for the annotations its x-window
// can show, and an annotation outside the window is simply not returned —
// the renderer never has to decide what "off-screen" means. Writes are
// idempotency-keyed like every other retryable mutation: a retried add
// replays its receipt instead of double-marking the chart, and a retried
// delete re-returns the row it removed.
//
// Annotations are evidence-carrying: findings and outcome entries cite one by
// id (annotation:<uuid> / evidence.annotation_ids), and the usecase layer
// resolves those references through AnnotationForProject so a citation can
// never point at another project's row.

// Annotation kinds — the closed vocabulary the kind selector and the marker
// legend share. "other" is the explicit fallback, not an empty string, so a
// marker never has to guess what an unlabeled change was.
const (
	AnnotationKindDeploy   = "deploy"
	AnnotationKindCampaign = "campaign"
	AnnotationKindPrice    = "price"
	AnnotationKindOther    = "other"
)

// Bounds on one annotation. A label is rendered inside a chart tooltip and a
// dialog list, so it is bounded like a board title; a link is a navigation
// target, so it must be an absolute http(s) URL — a javascript: or relative
// link stored here would render as a trusted link on every reader's chart.
const (
	annotationLabelMaxLen = 140
	annotationLinkMaxLen  = 2048
)

// ErrAnnotationInvalid wraps every rejected annotation write, so an adapter
// can answer 400 with the specific reason instead of a generic failure.
var ErrAnnotationInvalid = errors.New("annotation is invalid")

// Annotation is one marked change on a project's timeline.
type Annotation struct {
	ID        string     `json:"id"`
	ProjectID string     `json:"project_id"`
	Label     string     `json:"label"`
	Kind      string     `json:"kind"`
	Link      string     `json:"link,omitempty"`
	StartsAt  time.Time  `json:"starts_at"`
	EndsAt    *time.Time `json:"ends_at,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
}

// AnnotationWrite is the mutable payload add_annotation carries. EndsAt nil
// means an instant; set means a range.
type AnnotationWrite struct {
	Label    string
	Kind     string
	Link     string
	StartsAt time.Time
	EndsAt   *time.Time
}

// migrateAnnotations creates the annotations table and its window index. The
// overlap read filters starts_at <= window end, so (project_id, starts_at) is
// the index that bounds the scan to the project's recent annotations.
func (s *Store) migrateAnnotations(ctx context.Context) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS annotations (
	id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
	project_id UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
	label VARCHAR(140) NOT NULL,
	kind VARCHAR(16) NOT NULL,
	link TEXT NOT NULL DEFAULT '',
	starts_at TIMESTAMPTZ NOT NULL,
	ends_at TIMESTAMPTZ,
	created_at TIMESTAMPTZ NOT NULL DEFAULT now()
)`,
		`CREATE INDEX IF NOT EXISTS annotations_project_starts_idx
	ON annotations (project_id, starts_at)`,
	}
	for _, stmt := range stmts {
		if _, err := s.pg.Exec(ctx, stmt); err != nil {
			return err
		}
	}
	return nil
}

// normalizeAnnotation trims, defaults and validates one write. Every rejection
// names the offending field — an annotation is composed by a model as often as
// by a person, and "invalid" without a reason costs a round trip.
func normalizeAnnotation(in AnnotationWrite) (AnnotationWrite, error) {
	in.Label = strings.TrimSpace(in.Label)
	if in.Label == "" {
		return in, fmt.Errorf("%w: label is required", ErrAnnotationInvalid)
	}
	if len(in.Label) > annotationLabelMaxLen {
		return in, fmt.Errorf("%w: label must be at most %d characters", ErrAnnotationInvalid, annotationLabelMaxLen)
	}
	in.Kind = strings.ToLower(strings.TrimSpace(in.Kind))
	if in.Kind == "" {
		in.Kind = AnnotationKindOther
	}
	switch in.Kind {
	case AnnotationKindDeploy, AnnotationKindCampaign, AnnotationKindPrice, AnnotationKindOther:
	default:
		return in, fmt.Errorf("%w: kind must be deploy, campaign, price or other", ErrAnnotationInvalid)
	}
	in.Link = strings.TrimSpace(in.Link)
	if in.Link != "" {
		if len(in.Link) > annotationLinkMaxLen {
			return in, fmt.Errorf("%w: link must be at most %d characters", ErrAnnotationInvalid, annotationLinkMaxLen)
		}
		u, err := url.Parse(in.Link)
		if err != nil || !u.IsAbs() || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return in, fmt.Errorf("%w: link must be an absolute http(s) URL", ErrAnnotationInvalid)
		}
	}
	if in.StartsAt.IsZero() {
		return in, fmt.Errorf("%w: starts_at is required", ErrAnnotationInvalid)
	}
	if in.EndsAt != nil && !in.EndsAt.After(in.StartsAt) {
		return in, fmt.Errorf("%w: ends_at must be after starts_at — an instant annotation omits it", ErrAnnotationInvalid)
	}
	return in, nil
}

const annotationColumns = `id::text, project_id::text, label, kind, link, starts_at, ends_at, created_at`

func annotationScanDest(a *Annotation) []any {
	return []any{&a.ID, &a.ProjectID, &a.Label, &a.Kind, &a.Link, &a.StartsAt, &a.EndsAt, &a.CreatedAt}
}

// createAnnotation inserts one validated annotation. Runs on any pgQuerier so
// the idempotent claim wrapper can execute it inside the claim transaction.
func createAnnotation(ctx context.Context, q pgQuerier, projectID string, in AnnotationWrite) (Annotation, error) {
	in, err := normalizeAnnotation(in)
	if err != nil {
		return Annotation{}, err
	}
	var a Annotation
	err = q.QueryRow(ctx, `
INSERT INTO annotations (project_id, label, kind, link, starts_at, ends_at)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING `+annotationColumns,
		projectID, in.Label, in.Kind, in.Link, in.StartsAt, in.EndsAt).Scan(annotationScanDest(&a)...)
	return a, err
}

// CreateAnnotationIdempotent is createAnnotation under an idempotency claim:
// one transaction claims the key, inserts, and records the receipt, so a
// retried add returns the first annotation instead of a duplicate mark.
func (s *Store) CreateAnnotationIdempotent(ctx context.Context, projectID string, in AnnotationWrite, idemKey, requestHash string) (Annotation, error) {
	raw, err := s.runIdempotent(ctx, projectID, "add_annotation", idemKey, requestHash,
		func(ctx context.Context, q pgQuerier) (json.RawMessage, error) {
			a, err := createAnnotation(ctx, q, projectID, in)
			if err != nil {
				return nil, err
			}
			return json.Marshal(a)
		})
	if err != nil {
		return Annotation{}, err
	}
	var a Annotation
	if err := json.Unmarshal(raw, &a); err != nil {
		return Annotation{}, fmt.Errorf("stored annotation receipt unreadable: %w", err)
	}
	return a, nil
}

// AnnotationsForWindow returns the project's annotations whose instant or
// range overlaps [from, to), oldest first — the half-open contract the chart
// windows use: an annotation starting exactly at `to` is outside, a range
// ending exactly at `from` is outside, and an instant is a zero-width mark at
// starts_at (inside when from <= starts_at < to).
func (s *Store) AnnotationsForWindow(ctx context.Context, projectID string, from, to time.Time, limit int) ([]Annotation, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	rows, err := s.pg.Query(ctx, `
SELECT `+annotationColumns+` FROM annotations
WHERE project_id = $1 AND starts_at < $3
  AND ((ends_at IS NULL AND starts_at >= $2) OR (ends_at IS NOT NULL AND ends_at > $2))
ORDER BY starts_at ASC, id ASC
LIMIT $4`, projectID, from, to, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Annotation{}
	for rows.Next() {
		var a Annotation
		if err := rows.Scan(annotationScanDest(&a)...); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// AnnotationForProject reads one annotation by id, project-scoped — the
// resolution an evidence reference needs so a citation can never name a row
// in another project.
func (s *Store) AnnotationForProject(ctx context.Context, projectID, id string) (Annotation, error) {
	var a Annotation
	err := s.pg.QueryRow(ctx,
		`SELECT `+annotationColumns+` FROM annotations WHERE project_id = $1 AND id = $2`,
		projectID, id).Scan(annotationScanDest(&a)...)
	return a, err
}

// deleteAnnotation removes one annotation and returns the row it removed, so
// the receipt (and the caller's confirmation) carries what was deleted rather
// than a bare ok. A missing row is pgx.ErrNoRows — not-found on every adapter.
func deleteAnnotation(ctx context.Context, q pgQuerier, projectID, id string) (Annotation, error) {
	var a Annotation
	err := q.QueryRow(ctx, `
DELETE FROM annotations WHERE project_id = $1 AND id = $2
RETURNING `+annotationColumns, projectID, id).Scan(annotationScanDest(&a)...)
	return a, err
}

// DeleteAnnotationIdempotent is deleteAnnotation under an idempotency claim —
// a retried delete replays the stored receipt instead of answering not-found
// for a row the first attempt already removed.
func (s *Store) DeleteAnnotationIdempotent(ctx context.Context, projectID, id, idemKey, requestHash string) (Annotation, error) {
	raw, err := s.runIdempotent(ctx, projectID, "delete_annotation", idemKey, requestHash,
		func(ctx context.Context, q pgQuerier) (json.RawMessage, error) {
			a, err := deleteAnnotation(ctx, q, projectID, id)
			if err != nil {
				return nil, err
			}
			return json.Marshal(a)
		})
	if err != nil {
		return Annotation{}, err
	}
	var a Annotation
	if err := json.Unmarshal(raw, &a); err != nil {
		return Annotation{}, fmt.Errorf("stored annotation receipt unreadable: %w", err)
	}
	return a, nil
}
