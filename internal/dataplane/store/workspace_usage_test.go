package storage

import (
	"strings"
	"testing"
)

// The workspace usage meter is the screen someone reads right before deciding to
// upgrade, and its "People" figure was uniqExact(distinct_id): no identity
// stitching, so one human who browsed anonymously then logged in counted twice,
// and no bot filter, so every crawler counted as a person. Measured on real
// data: 1,083 reported against 835 actual — 6 crawler identities and 242
// double-counted logins. Stitching now comes from the resolved_events view's
// canonical_distinct_id column, keyed on (project_id, distinct_id) inside the
// view so identity namespaces stay per-project.
func TestWorkspaceCanonicalExprStitchesThroughTheView(t *testing.T) {
	s := &Store{}
	expr := s.workspaceCanonicalExpr("distinct_id")

	if expr != "canonical_distinct_id" {
		t.Errorf("expression must read the stitched view column, got %q", expr)
	}
}

// WorkspaceUsage passes workspaceFilteredWhere's args through unchanged and
// interpolates this expression into the SELECT. If the expression ever needed a
// bind arg it would land at the wrong position and silently query the wrong
// thing, so it must contribute none.
func TestWorkspaceCanonicalExprBindsNoArguments(t *testing.T) {
	s := &Store{}
	expr := s.workspaceCanonicalExpr("distinct_id")
	if strings.Contains(expr, "?") {
		t.Errorf("expression must contain no placeholders, got %q", expr)
	}
}

// Both agree on the same shape, so the workspace meter and every project-scoped
// surface resolve identity identically. A drift here is how two screens end up
// reporting different people counts for the same window.
func TestWorkspaceAndProjectCanonicalExprsAgree(t *testing.T) {
	s := &Store{}
	perProject, args := identityResolver{}.canonicalExpr("distinct_id")
	if len(args) != 0 {
		t.Fatalf("canonicalExpr should bind no args, got %d", len(args))
	}
	if got := s.workspaceCanonicalExpr("distinct_id"); got != perProject {
		t.Errorf("workspace expr %q != project expr %q", got, perProject)
	}
}

// HumansOnly is already plumbed here; the meter deliberately does not set it,
// because EventCount must stay every event that was ingested — that is what the
// plan ceiling is measured against. The People filter lives inside the aggregate
// instead. This pins that the clause exists and is spelled the same way the rest
// of the store spells it.
func TestWorkspaceFilteredWhereSpellsTheHumanFilterConsistently(t *testing.T) {
	where, _ := workspaceFilteredWhere([]string{"p1"}, EventFilter{HumansOnly: true}, false)
	const want = "coalesce(visitor_class, 'human') = 'human'"
	if !strings.Contains(where, want) {
		t.Errorf("got %q, want it to contain %q", where, want)
	}
}
