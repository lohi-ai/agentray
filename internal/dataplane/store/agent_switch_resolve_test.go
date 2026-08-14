package storage

import (
	"context"
	"testing"
)

// TestAgentSwitchResolvesDistinctConfig is the run-path half of the per-message
// agent override: it proves that switching the agent_id a turn runs under actually
// changes what the run resolves — the system prompt / persona (soul_md), the tool
// selections, and the permission scopes — not just which row is stamped on the
// entry. This is the contract the message endpoint relies on when it threads the
// chosen agent into the run.
func TestAgentSwitchResolvesDistinctConfig(t *testing.T) {
	s := openConvTestStore(t)
	userID, projectID := seedConvProject(t, s)
	ctx := context.Background()

	agentA, err := s.CreateAgent(ctx, userID, projectID, "Analyst A", "analyst-a")
	if err != nil {
		t.Fatalf("CreateAgent A: %v", err)
	}
	agentB, err := s.CreateAgent(ctx, userID, projectID, "Analyst B", "analyst-b")
	if err != nil {
		t.Fatalf("CreateAgent B: %v", err)
	}

	// Distinct persona / system prompt per agent.
	if _, err := s.UpsertAgentDefinition(ctx, userID, projectID, agentA.ID, "You are A, terse.", "AGENTS-A"); err != nil {
		t.Fatalf("def A: %v", err)
	}
	if _, err := s.UpsertAgentDefinition(ctx, userID, projectID, agentB.ID, "You are B, verbose.", "AGENTS-B"); err != nil {
		t.Fatalf("def B: %v", err)
	}

	// Distinct tool selections per agent.
	if err := s.UpsertAgentTool(ctx, userID, projectID, agentA.ID, "run_sql", true, "{}"); err != nil {
		t.Fatalf("tool A: %v", err)
	}
	if err := s.UpsertAgentTool(ctx, userID, projectID, agentB.ID, "create_chart", true, "{}"); err != nil {
		t.Fatalf("tool B: %v", err)
	}

	// Distinct permission scopes per agent.
	if _, err := s.UpsertAgentCapabilities(ctx, userID, projectID, agentA.ID, map[string]bool{"monitor": true, "growth_suggest": false}); err != nil {
		t.Fatalf("caps A: %v", err)
	}
	if _, err := s.UpsertAgentCapabilities(ctx, userID, projectID, agentB.ID, map[string]bool{"monitor": false, "growth_suggest": true}); err != nil {
		t.Fatalf("caps B: %v", err)
	}

	// Resolve each agent down the same path a run takes: id → scope → definition /
	// tools / scopes.
	scopeA, err := s.AgentScopeForRun(ctx, projectID, agentA.ID)
	if err != nil {
		t.Fatalf("scope A: %v", err)
	}
	scopeB, err := s.AgentScopeForRun(ctx, projectID, agentB.ID)
	if err != nil {
		t.Fatalf("scope B: %v", err)
	}
	if scopeA == scopeB {
		t.Fatalf("two agents resolved to the same scope %q", scopeA)
	}

	defA, _ := s.AgentDefinitionForRun(ctx, scopeA)
	defB, _ := s.AgentDefinitionForRun(ctx, scopeB)
	if defA.SoulMD == defB.SoulMD || defA.SoulMD != "You are A, terse." || defB.SoulMD != "You are B, verbose." {
		t.Fatalf("system prompt did not switch with agent: A=%q B=%q", defA.SoulMD, defB.SoulMD)
	}
	if defA.AgentsMD == defB.AgentsMD {
		t.Fatalf("persona instructions did not switch with agent: %q", defA.AgentsMD)
	}

	toolsA, _ := s.AgentToolsForRun(ctx, scopeA)
	toolsB, _ := s.AgentToolsForRun(ctx, scopeB)
	if hasTool(toolsA, "create_chart") || !hasTool(toolsA, "run_sql") {
		t.Fatalf("agent A tools did not resolve to its own selection: %+v", toolsA)
	}
	if hasTool(toolsB, "run_sql") || !hasTool(toolsB, "create_chart") {
		t.Fatalf("agent B tools did not resolve to its own selection: %+v", toolsB)
	}

	scopesA, _ := s.AgentCapabilitiesForRun(ctx, projectID, scopeA)
	scopesB, _ := s.AgentCapabilitiesForRun(ctx, projectID, scopeB)
	if !scopesA["monitor"] || scopesA["growth_suggest"] {
		t.Fatalf("agent A permissions did not switch: %+v", scopesA)
	}
	if !scopesB["growth_suggest"] || scopesB["monitor"] {
		t.Fatalf("agent B permissions did not switch: %+v", scopesB)
	}
}

func hasTool(sels []AgentToolSelection, name string) bool {
	for _, s := range sels {
		if s.Name == name && s.Enabled {
			return true
		}
	}
	return false
}
