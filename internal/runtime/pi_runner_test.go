package agentruntime

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/lohi-ai/agentray/agentcore"
)

func TestPiRunnerRejectsUnsupportedMigrationWithoutGoFallback(t *testing.T) {
	for _, tc := range []struct {
		name   string
		opts   RunOptions
		reason string
	}{
		{"legacy history", RunOptions{History: []agentcore.Message{{Role: agentcore.RoleUser, Content: "old"}}}, "native history"},
		{"resume", RunOptions{ResumeFromRunID: "prior"}, "resume"},
		{"turn hook", RunOptions{PrepareNextTurn: func(_ context.Context, s agentcore.TurnState) agentcore.TurnState { return s }}, "native turn"},
		{"malformed native history", RunOptions{NativeHistory: json.RawMessage(`{}`)}, "message array"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := NewRunner(nil, WithPiRuntime(PiRuntimeConfig{}))
			// Empty build parameters would fail Build. The migration error must win
			// before either driver, provider, or tool host can be invoked.
			_, err := r.runModelLoop(context.Background(), BuildParams{}, tc.opts, ModelTier{}, nil)
			if err == nil || !strings.Contains(err.Error(), tc.reason) {
				t.Fatalf("wrong dispatch: %v", err)
			}
		})
	}
	r := NewRunner(nil)
	if _, err := r.runModelLoop(context.Background(), BuildParams{}, RunOptions{NativeHistory: json.RawMessage(`[]`)}, ModelTier{}, nil); err == nil || !strings.Contains(err.Error(), "requires the Pi runtime") {
		t.Fatalf("legacy loop consumed native history: %v", err)
	}
}
