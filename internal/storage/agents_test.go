package storage

import (
	"context"
	"strings"
	"testing"
)

func TestValidAgentSlug(t *testing.T) {
	for _, ok := range []string{"default", "novel-mod", "a", "a1", "x" + strings.Repeat("y", 63)} {
		if !validAgentSlug(ok) {
			t.Errorf("validAgentSlug(%q) = false, want true", ok)
		}
	}
	for _, bad := range []string{"", "-leading", "Has-Upper", "has space", "under_score", "x" + strings.Repeat("y", 64)} {
		if validAgentSlug(bad) {
			t.Errorf("validAgentSlug(%q) = true, want false", bad)
		}
	}
}

func TestSlugify(t *testing.T) {
	cases := map[string]string{
		"Novel Moderator":  "novel-moderator",
		"  Support  Bot  ": "support-bot",
		"Café & Crème":     "caf-cr-me",
		"already-slug":     "already-slug",
		"!!!":              "",
	}
	for in, want := range cases {
		if got := slugify(in); got != want {
			t.Errorf("slugify(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestIsDefaultAgent(t *testing.T) {
	// The default agent's id equals its project_id by construction.
	if !isDefaultAgent("p1", "p1") {
		t.Error("agentID == projectID should be the default agent")
	}
	if isDefaultAgent("p1", "a2") {
		t.Error("a distinct agentID must not be treated as the default")
	}
}

// TestAgentScopeForRunDefaultPath covers the two branches that resolve a run's
// scope without a DB read: an empty agentID and an agentID equal to the project
// both map to the project id (the default agent), preserving the original
// single-agent run path byte-for-byte. A non-default agentID would query the DB,
// so it is not exercised here.
func TestAgentScopeForRunDefaultPath(t *testing.T) {
	var s Store // nil pg: the default branches must return before touching it
	for _, agentID := range []string{"", "proj-1"} {
		scope, err := s.AgentScopeForRun(context.Background(), "proj-1", agentID)
		if err != nil {
			t.Fatalf("AgentScopeForRun(proj-1, %q) error: %v", agentID, err)
		}
		if scope != "proj-1" {
			t.Errorf("AgentScopeForRun(proj-1, %q) = %q, want proj-1", agentID, scope)
		}
	}
}
