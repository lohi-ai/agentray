package agentcore

import "testing"

// buildPrompt mirrors buildSystemPrompt's section layout so the fold parser is
// exercised against the real prompt shape.
func labPrompt(soul, mem, skillLine string) string {
	s := "# Identity\n" + soul + "\n\n"
	if mem != "" {
		s += "# Recalled memory\nThe following are durable facts from prior runs.\n- (fact) " + mem + "\n\n"
	}
	if skillLine != "" {
		s += "# Available skills\nYou have on-demand skills.\n" + skillLine + "\n"
	}
	return s
}

func TestFoldStepsBasicTurn(t *testing.T) {
	records := []TurnRecord{
		{
			Messages: []Message{
				{Role: RoleSystem, Content: labPrompt("You are a tester.", "user likes brevity", "- id: search — Search: find things")},
				{Role: RoleUser, Content: "do the thing"},
			},
			Response:   "calling search",
			ToolCalls:  []ToolCall{{ID: "c1", Name: "search", Arguments: `{"q":"x"}`}},
			Tools:      []string{"search", "read_skill"},
			StopReason: "tool_calls",
			TokensIn:   100, TokensOut: 20, CostUSD: 0.001,
		},
		{
			Messages: []Message{
				{Role: RoleSystem, Content: labPrompt("You are a tester.", "user likes brevity", "- id: search — Search: find things")},
				{Role: RoleUser, Content: "do the thing"},
				{Role: RoleAssistant, Content: "calling search", ToolCalls: []ToolCall{{ID: "c1", Name: "search", Arguments: `{"q":"x"}`}}},
				{Role: RoleTool, ToolCallID: "c1", Name: "search", Content: "found it"},
			},
			Response:   "done",
			StopReason: "stop",
			TokensIn:   140, TokensOut: 10, CostUSD: 0.0008,
		},
	}

	steps := FoldSteps(records)
	if len(steps) != 2 {
		t.Fatalf("want 2 steps, got %d", len(steps))
	}

	s0 := steps[0]
	if s0.Kind != LabStepTurn || s0.Turn != 1 {
		t.Fatalf("step0 kind/turn = %s/%d", s0.Kind, s0.Turn)
	}
	if s0.Persona != "You are a tester." {
		t.Fatalf("persona = %q", s0.Persona)
	}
	if len(s0.Memory) != 1 || s0.Memory[0] != "(fact) user likes brevity" {
		t.Fatalf("memory = %v", s0.Memory)
	}
	if len(s0.SkillsAdvertised) != 1 || s0.SkillsAdvertised[0].ID != "search" || s0.SkillsAdvertised[0].Name != "Search" {
		t.Fatalf("advertised = %+v", s0.SkillsAdvertised)
	}
	if len(s0.ToolCalls) != 1 || s0.ToolCalls[0].Result != "found it" {
		t.Fatalf("tool call result not paired across turns: %+v", s0.ToolCalls)
	}
	if s0.CumTokensIn != 100 || s0.CumCostUSD != 0.001 {
		t.Fatalf("cum after step0 = %d/%v", s0.CumTokensIn, s0.CumCostUSD)
	}

	s1 := steps[1]
	if s1.CumTokensIn != 240 || s1.CumTokensOut != 30 {
		t.Fatalf("cumulative tokens wrong: in=%d out=%d", s1.CumTokensIn, s1.CumTokensOut)
	}
}

func TestFoldStepsLoadedSkills(t *testing.T) {
	records := []TurnRecord{
		{
			Messages:  []Message{{Role: RoleSystem, Content: labPrompt("p", "", "- id: deep — Deep: deep skill")}},
			ToolCalls: []ToolCall{{ID: "r1", Name: readSkillToolName, Arguments: `{"id":"deep"}`}},
			Tools:     []string{readSkillToolName},
		},
		{
			Messages: []Message{{Role: RoleSystem, Content: labPrompt("p", "", "- id: deep — Deep: deep skill")}},
			Response: "ok",
		},
	}
	steps := FoldSteps(records)
	if len(steps[0].SkillsLoaded) != 1 || steps[0].SkillsLoaded[0] != "deep" {
		t.Fatalf("step0 loaded = %v", steps[0].SkillsLoaded)
	}
	// Loaded set carries forward.
	if len(steps[1].SkillsLoaded) != 1 || steps[1].SkillsLoaded[0] != "deep" {
		t.Fatalf("step1 loaded should carry forward: %v", steps[1].SkillsLoaded)
	}
	// Advertised but not loaded is visible as a gap the UI can render.
	if len(steps[0].SkillsAdvertised) != 1 {
		t.Fatalf("advertised = %+v", steps[0].SkillsAdvertised)
	}
}

func TestFoldStepsCompactionStep(t *testing.T) {
	sys := labPrompt("p", "", "")
	records := []TurnRecord{
		{
			Messages: []Message{{Role: RoleSystem, Content: sys}, {Role: RoleUser, Content: "hi"}},
			Response: "a", TokensIn: 50,
		},
		{
			// Compaction folded a summary message into this turn's context.
			Messages: []Message{
				{Role: RoleSystem, Content: sys},
				{Role: RoleSystem, Content: summaryMarker + "\nearlier: user asked many things"},
				{Role: RoleUser, Content: "more"},
			},
			Response: "b", TokensIn: 30,
		},
	}
	steps := FoldSteps(records)
	if len(steps) != 3 {
		t.Fatalf("want 3 steps (turn, compaction, turn), got %d", len(steps))
	}
	if steps[1].Kind != LabStepCompaction {
		t.Fatalf("step1 should be compaction, got %s", steps[1].Kind)
	}
	if steps[1].Summary != "earlier: user asked many things" {
		t.Fatalf("summary = %q", steps[1].Summary)
	}
	// Cumulative carries through the compaction step unchanged.
	if steps[1].CumTokensIn != 50 {
		t.Fatalf("cum at compaction = %d", steps[1].CumTokensIn)
	}
	if steps[2].CumTokensIn != 80 {
		t.Fatalf("cum after final turn = %d", steps[2].CumTokensIn)
	}
}

func TestFoldNoSecretsLeak(t *testing.T) {
	records := []TurnRecord{{
		Messages:  []Message{{Role: RoleSystem, Content: labPrompt("p", "", "")}},
		ToolCalls: []ToolCall{{ID: "c1", Name: "http", Arguments: `{"token":"{{cred:API_KEY}}"}`}},
	}}
	steps := FoldSteps(records)
	if steps[0].ToolCalls[0].Args != `{"token":"{{cred:API_KEY}}"}` {
		t.Fatalf("fold must preserve placeholder form, got %q", steps[0].ToolCalls[0].Args)
	}
}

func TestDiffStep(t *testing.T) {
	steps := FoldSteps([]TurnRecord{
		{
			Messages:  []Message{{Role: RoleSystem, Content: labPrompt("p", "m1", "- id: s — S: s")}, {Role: RoleUser, Content: "x"}},
			ToolCalls: []ToolCall{{ID: "r", Name: readSkillToolName, Arguments: `{"id":"s"}`}},
			TokensIn:  10, TokensOut: 5, CostUSD: 0.1,
		},
		{
			Messages: []Message{{Role: RoleSystem, Content: labPrompt("p", "m1", "- id: s — S: s")}, {Role: RoleUser, Content: "x"}, {Role: RoleAssistant, Content: "y"}},
			ToolCalls: []ToolCall{{ID: "t", Name: "search", Arguments: `{}`}},
			TokensIn: 20, TokensOut: 8, CostUSD: 0.2,
		},
	})
	d := DiffStep(steps[0], steps[1])
	if d.TokensInDelta != 20 || d.CostDelta != 0.2 {
		t.Fatalf("delta wrong: %+v", d)
	}
	if len(d.ToolsCalled) != 1 || d.ToolsCalled[0] != "search" {
		t.Fatalf("tools called = %v", d.ToolsCalled)
	}
	// Skill "s" was already loaded in step0; not new in step1.
	if len(d.SkillsLoaded) != 0 {
		t.Fatalf("no new skills expected, got %v", d.SkillsLoaded)
	}

	// First step diffs against empty: shows its full setup.
	d0 := DiffStep(LabStep{}, steps[0])
	if len(d0.SkillsLoaded) != 1 || len(d0.MemoryAdded) != 1 {
		t.Fatalf("first-step diff should show full setup: %+v", d0)
	}
}
