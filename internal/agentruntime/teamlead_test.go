package agentruntime

import (
	"context"
	"strings"
	"testing"

	"github.com/lohi-ai/agentray/agentcore"
	"github.com/lohi-ai/agentray/internal/storage"
)

func teamFixture() []storage.TeamLeadership {
	return []storage.TeamLeadership{
		{
			TeamID: "t1", Name: "Growth", Slug: "growth",
			Members: []storage.AgentDelegateTarget{
				{AgentID: "a1", Name: "Writer", Slug: "writer", Description: "Writes copy"},
				{AgentID: "a2", Name: "Analyst", Slug: "analyst", Description: "Runs numbers"},
			},
		},
	}
}

func noopRunFor(string) func(context.Context, string, agentcore.StreamSink) (string, agentcore.Usage, error) {
	return func(context.Context, string, agentcore.StreamSink) (string, agentcore.Usage, error) {
		return "", agentcore.Usage{}, nil
	}
}

func TestMergeTeamDelegatesAddsMembers(t *testing.T) {
	out := mergeTeamDelegates(nil, teamFixture(), noopRunFor)
	if len(out) != 2 {
		t.Fatalf("want 2 delegates, got %d", len(out))
	}
	if out[0].Name != "Writer" || out[1].Name != "Analyst" {
		t.Fatalf("unexpected roster: %+v", out)
	}
	if out[0].Run == nil {
		t.Fatal("member delegate must carry a run closure")
	}
}

func TestMergeTeamDelegatesExplicitGrantWins(t *testing.T) {
	marker := func(context.Context, string, agentcore.StreamSink) (string, agentcore.Usage, error) {
		return "explicit", agentcore.Usage{}, nil
	}
	existing := []agentcore.Delegate{{Name: "writer", Description: "explicit grant", Run: marker}}
	out := mergeTeamDelegates(existing, teamFixture(), noopRunFor)
	if len(out) != 2 {
		t.Fatalf("want 2 delegates (writer deduped case-insensitively), got %d", len(out))
	}
	if out[0].Description != "explicit grant" {
		t.Fatalf("explicit grant must win over team-derived entry, got %+v", out[0])
	}
}

func TestMergeTeamDelegatesMemberOnTwoTeamsAppearsOnce(t *testing.T) {
	teams := append(teamFixture(), storage.TeamLeadership{
		TeamID: "t2", Name: "Ops", Slug: "ops",
		Members: []storage.AgentDelegateTarget{
			{AgentID: "a1", Name: "Writer", Slug: "writer"},
			{AgentID: "a3", Name: "Fixer", Slug: "fixer"},
		},
	})
	out := mergeTeamDelegates(nil, teams, noopRunFor)
	if len(out) != 3 {
		t.Fatalf("want 3 unique delegates, got %d: %+v", len(out), out)
	}
}

func TestOrchestratorSkillHeaderAndBody(t *testing.T) {
	skill := orchestratorSkill(teamFixture())
	if skill.ID != orchestratorSkillID || !skill.Enabled {
		t.Fatalf("bad skill header: %+v", skill)
	}
	for _, want := range []string{"Growth", "Writer", "Analyst", "team_board", "spawn_subagent"} {
		if !strings.Contains(skill.Body, want) {
			t.Fatalf("body missing %q", want)
		}
	}
}

func TestApplyDelegateNameCollisionsRenamesShadowedMember(t *testing.T) {
	// Two distinct agents share the display name "Writer"; slugs stay unique
	// per project, so the member's slug is the collision-free handle.
	teams := []storage.TeamLeadership{{
		TeamID: "t1", Name: "Growth", Slug: "growth",
		Members: []storage.AgentDelegateTarget{
			{AgentID: "a1", Name: "Writer", Slug: "writer-2", Description: "Writes copy"},
			{AgentID: "a2", Name: "Analyst", Slug: "analyst"},
		},
	}}
	explicit := []storage.AgentDelegateTarget{{AgentID: "x9", Name: "Writer", Slug: "writer"}}
	out := applyDelegateNameCollisions(teams, explicit)
	if got := out[0].Members[0].Name; got != "writer-2" {
		t.Fatalf("shadowed member must be advertised by slug, got %q", got)
	}
	if out[0].Members[1].Name != "Analyst" {
		t.Fatalf("non-colliding member must keep its name: %+v", out[0].Members[1])
	}
	if teams[0].Members[0].Name != "Writer" {
		t.Fatal("input roster must not be mutated")
	}
	// End to end: the merged delegate list carries the slug, so
	// spawn_subagent("writer-2") reaches a1 while "Writer" stays the explicit
	// grant to x9.
	existing := []agentcore.Delegate{{Name: "Writer", Description: "explicit grant"}}
	merged := mergeTeamDelegates(existing, out, noopRunFor)
	names := make([]string, 0, len(merged))
	for _, d := range merged {
		names = append(names, d.Name)
	}
	if len(merged) != 3 || merged[1].Name != "writer-2" {
		t.Fatalf("merge must expose the renamed member: %v", names)
	}
}

func TestApplyDelegateNameCollisionsSameAgentKeepsName(t *testing.T) {
	explicit := []storage.AgentDelegateTarget{{AgentID: "a1", Name: "Writer", Slug: "writer"}}
	out := applyDelegateNameCollisions(teamFixture(), explicit)
	if out[0].Members[0].Name != "Writer" {
		t.Fatalf("same-agent overlap must keep the name (merge dedupes it): %+v", out[0].Members[0])
	}
}

func TestTeamBoardResolveTeamDefaultsWhenSingle(t *testing.T) {
	tool := &teamBoardTool{teams: teamFixture()}
	team, err := tool.resolveTeam("")
	if err != nil || team.Name != "Growth" {
		t.Fatalf("got %+v, %v", team, err)
	}
	if _, err := tool.resolveTeam("nope"); err == nil {
		t.Fatal("unknown team must error")
	}
}

func TestTeamBoardResolveTeamAmbiguousWithoutName(t *testing.T) {
	teams := append(teamFixture(), storage.TeamLeadership{TeamID: "t2", Name: "Ops", Slug: "ops"})
	tool := &teamBoardTool{teams: teams}
	if _, err := tool.resolveTeam(""); err == nil {
		t.Fatal("two teams without a name must error")
	}
	team, err := tool.resolveTeam("ops")
	if err != nil || team.TeamID != "t2" {
		t.Fatalf("slug lookup failed: %+v, %v", team, err)
	}
}

func TestResolveMemberByNameSlugID(t *testing.T) {
	team := teamFixture()[0]
	for _, who := range []string{"Writer", "writer", "a1"} {
		id, err := resolveMember(team, who)
		if err != nil || id != "a1" {
			t.Fatalf("resolveMember(%q) = %q, %v", who, id, err)
		}
	}
	if _, err := resolveMember(team, "stranger"); err == nil {
		t.Fatal("non-member must be refused")
	}
	if id, err := resolveMember(team, ""); err != nil || id != "" {
		t.Fatalf("empty assignee should clear: %q, %v", id, err)
	}
}
