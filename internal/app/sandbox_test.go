package app

import (
	"context"
	"testing"

	"github.com/lohi-ai/agentray/internal/config"
)

// When the feature is off (the default), buildSandbox must return nil so agents
// stay analytics-only — no Docker probe, no behavior change.
func TestBuildSandboxDisabledReturnsNil(t *testing.T) {
	if sb := buildSandbox(context.Background(), config.Config{SandboxEnabled: false}); sb != nil {
		t.Fatal("buildSandbox must return nil when AGENTRAY_SANDBOX_ENABLED is unset")
	}
}
