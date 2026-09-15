package storage

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// annotations_test.go pins the chart-annotation contract against live
// Postgres: validated writes, the overlap-window read, project isolation, and
// idempotent add/delete. Skipped without a test database, matching the other
// live store tests.

func TestAnnotationValidation(t *testing.T) {
	s := plansTestStore(t)
	ctx := context.Background()
	_, projectID := seedConvProject(t, s)
	now := time.Now().UTC().Truncate(time.Second)

	cases := []struct {
		name string
		in   AnnotationWrite
		want string
	}{
		{"empty label", AnnotationWrite{Kind: "deploy", StartsAt: now}, "label"},
		{"overlong label", AnnotationWrite{Label: strings.Repeat("x", 141), StartsAt: now}, "label"},
		{"bad kind", AnnotationWrite{Label: "x", Kind: "incident", StartsAt: now}, "kind"},
		{"relative link", AnnotationWrite{Label: "x", StartsAt: now, Link: "/changelog"}, "link"},
		{"javascript link", AnnotationWrite{Label: "x", StartsAt: now, Link: "javascript:alert(1)"}, "link"},
		{"zero starts_at", AnnotationWrite{Label: "x"}, "starts_at"},
		{"ends before start", AnnotationWrite{Label: "x", StartsAt: now, EndsAt: &[]time.Time{now.Add(-time.Hour)}[0]}, "ends_at"},
		{"ends equals start", AnnotationWrite{Label: "x", StartsAt: now, EndsAt: &now}, "ends_at"},
	}
	for _, tc := range cases {
		_, err := s.CreateAnnotationIdempotent(ctx, projectID, tc.in, "", "")
		if !errors.Is(err, ErrAnnotationInvalid) {
			t.Errorf("%s: err = %v, want ErrAnnotationInvalid", tc.name, err)
		} else if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want it to name %q", tc.name, err, tc.want)
		}
	}
}

func TestAnnotationOverlapWindow(t *testing.T) {
	s := plansTestStore(t)
	ctx := context.Background()
	_, projectID := seedConvProject(t, s)
	day := func(n int) time.Time { return time.Date(2026, 9, n, 0, 0, 0, 0, time.UTC) }
	end := day(13)

	inside, err := s.CreateAnnotationIdempotent(ctx, projectID, AnnotationWrite{
		Label: "v2.4 deploy", Kind: "deploy", StartsAt: day(10),
	}, "", "")
	if err != nil {
		t.Fatalf("create instant: %v", err)
	}
	if inside.Kind != "deploy" || inside.EndsAt != nil {
		t.Fatalf("instant annotation = %+v", inside)
	}
	ranged, err := s.CreateAnnotationIdempotent(ctx, projectID, AnnotationWrite{
		Label: "pricing campaign", Kind: "campaign", StartsAt: day(12), EndsAt: &end,
	}, "", "")
	if err != nil {
		t.Fatalf("create range: %v", err)
	}
	if _, err := s.CreateAnnotationIdempotent(ctx, projectID, AnnotationWrite{
		Label: "old change", StartsAt: day(1),
	}, "", ""); err != nil {
		t.Fatalf("create outside: %v", err)
	}
	// An instant exactly at the window's end is outside (half-open window).
	if _, err := s.CreateAnnotationIdempotent(ctx, projectID, AnnotationWrite{
		Label: "edge", StartsAt: day(14),
	}, "", ""); err != nil {
		t.Fatalf("create edge: %v", err)
	}

	// Window Sep 8–14: the instant, the range, and nothing else.
	got, err := s.AnnotationsForWindow(ctx, projectID, day(8), day(14), 0)
	if err != nil {
		t.Fatalf("window read: %v", err)
	}
	if len(got) != 2 || got[0].ID != inside.ID || got[1].ID != ranged.ID {
		t.Fatalf("window read = %+v", got)
	}

	// A window ending where the range starts still sees the range; a window
	// before everything sees nothing.
	got, err = s.AnnotationsForWindow(ctx, projectID, day(9), day(12), 0)
	if err != nil {
		t.Fatalf("narrow window: %v", err)
	}
	if len(got) != 1 || got[0].ID != inside.ID {
		t.Fatalf("narrow window = %+v", got)
	}
	got, err = s.AnnotationsForWindow(ctx, projectID, day(2), day(3), 0)
	if err != nil {
		t.Fatalf("empty window: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("empty window = %+v", got)
	}
}

func TestAnnotationProjectIsolation(t *testing.T) {
	s := plansTestStore(t)
	ctx := context.Background()
	_, projectA := seedConvProject(t, s)
	_, projectB := seedConvProject(t, s)
	now := time.Now().UTC().Truncate(time.Second)

	a, err := s.CreateAnnotationIdempotent(ctx, projectA, AnnotationWrite{Label: "deploy", StartsAt: now}, "", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// The window read is scoped: project B sees nothing.
	got, err := s.AnnotationsForWindow(ctx, projectB, now.Add(-time.Hour), now.Add(time.Hour), 0)
	if err != nil {
		t.Fatalf("window read: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("project B saw %d annotations", len(got))
	}
	// The by-id read and delete are scoped too — a citation or delete naming
	// another project's row is not-found, not a leak.
	if _, err := s.AnnotationForProject(ctx, projectB, a.ID); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("cross-project read: got %v, want ErrNoRows", err)
	}
	if _, err := s.DeleteAnnotationIdempotent(ctx, projectB, a.ID, "", ""); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("cross-project delete: got %v, want ErrNoRows", err)
	}
}

func TestAnnotationIdempotentAddAndDelete(t *testing.T) {
	s := plansTestStore(t)
	ctx := context.Background()
	_, projectID := seedConvProject(t, s)
	now := time.Now().UTC().Truncate(time.Second)
	in := AnnotationWrite{Label: "v2.4 deploy", Kind: "deploy", StartsAt: now}

	first, err := s.CreateAnnotationIdempotent(ctx, projectID, in, "add-1", "h1")
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	// A replayed add returns the receipt — one mark, not two.
	replay, err := s.CreateAnnotationIdempotent(ctx, projectID, in, "add-1", "h1")
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if replay.ID != first.ID {
		t.Fatalf("replay created a second annotation: %s vs %s", replay.ID, first.ID)
	}
	// The same key with a different payload conflicts.
	other := in
	other.Label = "different"
	if _, err := s.CreateAnnotationIdempotent(ctx, projectID, other, "add-1", "h2"); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("key reuse: got %v, want ErrIdempotencyConflict", err)
	}
	got, err := s.AnnotationsForWindow(ctx, projectID, now.Add(-time.Hour), now.Add(time.Hour), 0)
	if err != nil {
		t.Fatalf("window read: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("window read = %d annotations, want 1", len(got))
	}

	// A replayed delete re-returns the removed row instead of not-found.
	del, err := s.DeleteAnnotationIdempotent(ctx, projectID, first.ID, "del-1", "hd1")
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if del.ID != first.ID || del.Label != first.Label {
		t.Fatalf("delete receipt = %+v", del)
	}
	again, err := s.DeleteAnnotationIdempotent(ctx, projectID, first.ID, "del-1", "hd1")
	if err != nil {
		t.Fatalf("delete replay: %v", err)
	}
	if again.ID != first.ID {
		t.Fatalf("delete replay = %+v", again)
	}
	// A fresh delete of the removed row is not-found.
	if _, err := s.DeleteAnnotationIdempotent(ctx, projectID, first.ID, "", ""); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("delete of removed row: got %v, want ErrNoRows", err)
	}
}
