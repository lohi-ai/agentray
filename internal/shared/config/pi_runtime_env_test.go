package config

import "testing"

func TestPiRuntimeSelectionIsExplicit(t *testing.T) {
	t.Setenv("AGENTRAY_AGENT_PI_WORKER", "")
	t.Setenv("AGENTRAY_AGENT_PI_RUNTIME", "")
	if cfg := FromEnv(); cfg.AgentPiWorker != "" || cfg.AgentPiRuntime != "" {
		t.Fatal("Pi selected without operator configuration")
	}
	t.Setenv("AGENTRAY_AGENT_PI_WORKER", "/opt/agentray/pi/worker.mjs")
	t.Setenv("AGENTRAY_AGENT_PI_RUNTIME", "bun")
	if cfg := FromEnv(); cfg.AgentPiWorker != "/opt/agentray/pi/worker.mjs" || cfg.AgentPiRuntime != "bun" {
		t.Fatal("Pi runtime paths were not loaded")
	}
}
