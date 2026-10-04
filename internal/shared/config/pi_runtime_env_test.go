package config

import "testing"

func TestNativeRuntimeSelection(t *testing.T) {
	// Old process paths cannot select the TypeScript bridge in the application.
	t.Setenv("AGENTRAY_AGENT_PI_WORKER", "/missing/worker.mjs")
	t.Setenv("AGENTRAY_AGENT_PI_RUNTIME", "/missing/bun")
	for _, tc := range []struct {
		value string
		want  bool
	}{
		{"", true}, {"false", false}, {"true", true}, {"1", true}, {"invalid", true},
	} {
		t.Run(tc.value, func(t *testing.T) {
			t.Setenv("AGENTRAY_AGENT_NATIVE_GO", tc.value)
			if got := FromEnv().AgentNativeGo; got != tc.want {
				t.Fatalf("native Go selection = %v, want %v", got, tc.want)
			}
		})
	}
}
