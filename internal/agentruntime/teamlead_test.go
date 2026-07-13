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
	header, body := orchestratorSkill(teamFixture())
	if header.ID != orchestratorSkillID || !header.Enabled {
		t.Fatalf("bad header: %+v", header)
	}
	for _, want := range []string{"Growth", "Writer", "Analyst", "team_board", "spawn_subagent"} {
		if !strings.Contains(body, want) {
			t.Fatalf("body missing %q", want)
		}
	}
}

func TestWithOrchestratorBodyServesSyntheticAndDelegatesRest(t *testing.T) {
	inner := func(_ context.Context, ids []string) (map[string]string, error) {
		out := map[string]string{}
		for _, id := range ids {
			if id == orchestratorSkillID {
				return nil, context.Canceled // the synthetic id must never reach the inner loader
			}
			out[id] = "stored:" + id
		}
		return out, nil
	}
	loader := withOrchestratorBody(inner, "THE BODY")
	got, err := loader(context.Background(), []string{"uuid-1", orchestratorSkillID})
	if err != nil {
		t.Fatal(err)
	}
	if got[orchestratorSkillID] != "THE BODY" {
		t.Fatalf("synthetic body not served: %v", got)
	}
	if got["uuid-1"] != "stored:uuid-1" {
		t.Fatalf("stored skill not loaded through inner loader: %v", got)
	}
}

func TestWithOrchestratorBodyNilInner(t *testing.T) {
	loader := withOrchestratorBody(nil, "B")
	got, err := loader(context.Background(), []string{orchestratorSkillID})
	if err != nil || got[orchestratorSkillID] != "B" {
		t.Fatalf("got %v, %v", got, err)
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
