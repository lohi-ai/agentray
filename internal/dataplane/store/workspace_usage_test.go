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
// double-counted logins.
func TestWorkspaceCanonicalExprStitchesThroughTheDictionary(t *testing.T) {
	s := &Store{chDatabase: "lohi_analytics"}
	expr := s.workspaceCanonicalExpr("distinct_id")

	if !strings.Contains(expr, "lohi_analytics.aliases_dict") {
		t.Errorf("expression must be database-qualified, got %q", expr)
	}
	// The dictionary key is (project_id, distinct_id). A workspace query spans
	// many projects, so dropping project_id from the key would resolve one
	// project's anonymous id against another project's alias map.
	if !strings.Contains(expr, "(project_id, distinct_id)") {
		t.Errorf("expression must key on (project_id, distinct_id), got %q", expr)
	}
	// Unknown ids fall back to themselves, or every un-aliased visitor collapses.
	if !strings.Contains(expr, "dictGetOrDefault") {
		t.Errorf("expression must fall back to the raw id, got %q", expr)
	}
}

// WorkspaceUsage passes workspaceFilteredWhere's args through unchanged and
// interpolates this expression into the SELECT. If the expression ever needed a
// bind arg it would land at the wrong position and silently query the wrong
// thing, so it must contribute none.
func TestWorkspaceCanonicalExprBindsNoArguments(t *testing.T) {
	s := &Store{chDatabase: "lohi_analytics"}
	expr := s.workspaceCanonicalExpr("distinct_id")
	if strings.Contains(expr, "?") {
		t.Errorf("expression must contain no placeholders, got %q", expr)
	}
}

func TestWorkspaceCanonicalExprWithoutADatabase(t *testing.T) {
	s := &Store{}
	if got, want := s.workspaceCanonicalExpr("distinct_id"), "aliases_dict"; !strings.Contains(got, want) {
		t.Errorf("got %q, want it to reference %q", got, want)
	}
}

// Both agree on the same shape, so the workspace meter and every project-scoped
// surface resolve identity identically. A drift here is how two screens end up
// reporting different people counts for the same window.
func TestWorkspaceAndProjectCanonicalExprsAgree(t *testing.T) {
	s := &Store{chDatabase: "lohi_analytics"}
	perProject, args := identityResolver{database: "lohi_analytics"}.canonicalExpr("distinct_id")
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
	const want = "ifNull(visitor_class, 'human') = 'human'"
	if !strings.Contains(where, want) {
		t.Errorf("got %q, want it to contain %q", where, want)
	}
}
