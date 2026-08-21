package config

import (
	"testing"

	"github.com/lohi-ai/agentray/agentcore"
)

// TestAgentCeilingEnvOverrides: an operator who exports the env var must get the
// number they exported, and one who exports nothing must get 0 — the sentinel
// that means "keep agentcore's default", not a ceiling of zero turns.
func TestAgentCeilingEnvOverrides(t *testing.T) {
	for _, tc := range []struct {
		name             string
		env              map[string]string
		wantTurns        int
		wantToolCalls    int
		wantContextToken int
	}{
		{name: "unset means keep the agentcore default"},
		{
			name:      "turns alone",
			env:       map[string]string{"AGENTRAY_AGENT_MAX_TURNS": "60"},
			wantTurns: 60,
		},
		{
			name:          "tool calls alone",
			env:           map[string]string{"AGENTRAY_AGENT_MAX_TOOL_CALLS": "100"},
			wantToolCalls: 100,
		},
		{
			name: "all three together",
			env: map[string]string{
				"AGENTRAY_AGENT_MAX_TURNS":          "60",
				"AGENTRAY_AGENT_MAX_TOOL_CALLS":     "100",
				"AGENTRAY_AGENT_MAX_CONTEXT_TOKENS": "120000",
			},
			wantTurns: 60, wantToolCalls: 100, wantContextToken: 120000,
		},
		{
			name:      "garbage falls back to the default rather than to zero",
			env:       map[string]string{"AGENTRAY_AGENT_MAX_TURNS": "lots"},
			wantTurns: 0,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			cfg := FromEnv()
			if cfg.AgentMaxTurns != tc.wantTurns {
				t.Errorf("AgentMaxTurns = %d, want %d", cfg.AgentMaxTurns, tc.wantTurns)
			}
			if cfg.AgentMaxToolCalls != tc.wantToolCalls {
				t.Errorf("AgentMaxToolCalls = %d, want %d", cfg.AgentMaxToolCalls, tc.wantToolCalls)
			}
			if cfg.AgentMaxContextTokens != tc.wantContextToken {
				t.Errorf("AgentMaxContextTokens = %d, want %d", cfg.AgentMaxContextTokens, tc.wantContextToken)
			}
		})
	}
}

// TestUnsetCeilingsDoNotSilenceAgentcore states what the 0 sentinel buys: with
// nothing exported, the numbers a run ends up with are agentcore's measured
// defaults. A config default of, say, 12 here would freeze the library's
// ceilings at whatever this file happened to say the day it was written.
func TestUnsetCeilingsDoNotSilenceAgentcore(t *testing.T) {
	cfg := FromEnv()
	if cfg.AgentMaxTurns != 0 || cfg.AgentMaxToolCalls != 0 {
		t.Fatalf("unset ceilings must be 0 (= defer to agentcore), got turns=%d tools=%d", cfg.AgentMaxTurns, cfg.AgentMaxToolCalls)
	}
	if d := agentcore.DefaultLimits(); d.MaxTurns == 0 || d.MaxToolCalls == 0 {
		t.Fatalf("deferring to agentcore only works while its defaults are non-zero: %+v", d)
	}
}

// TestDemoWorkspaceConfig: the demo is off unless an operator names a project,
// and the per-viewer run cap has a real default — those runs bill the instance
// owner's model key, so "unset" must never mean "unlimited".
func TestDemoWorkspaceConfig(t *testing.T) {
	for _, tc := range []struct {
		name        string
		env         map[string]string
		wantProject string
		wantRunsCap int
	}{
		{name: "no demo on this instance", wantRunsCap: 5},
		{
			name:        "operator names the shared demo project",
			env:         map[string]string{"AGENTRAY_DEMO_PROJECT_ID": "proj-demo"},
			wantProject: "proj-demo", wantRunsCap: 5,
		},
		{
			name:        "operator tightens the per-viewer run cap",
			env:         map[string]string{"AGENTRAY_DEMO_AGENT_RUNS_PER_USER_PER_DAY": "2"},
			wantRunsCap: 2,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			cfg := FromEnv()
			if cfg.DemoProjectID != tc.wantProject {
				t.Errorf("DemoProjectID = %q, want %q", cfg.DemoProjectID, tc.wantProject)
			}
			if cfg.DemoAgentRunsPerUserPerDay != tc.wantRunsCap {
				t.Errorf("DemoAgentRunsPerUserPerDay = %d, want %d", cfg.DemoAgentRunsPerUserPerDay, tc.wantRunsCap)
			}
		})
	}
}
