package app

import (
	"testing"

	"github.com/lohi-ai/agentray/internal/config"
)

// When the feature is off (the default), buildCredentials must return nil so
// tool arguments pass through unchanged — no env scan, no behavior change.
func TestBuildCredentialsDisabledReturnsNil(t *testing.T) {
	if cv := buildCredentials(config.Config{CredentialsEnabled: false}); cv != nil {
		t.Fatal("buildCredentials must return nil when AGENTRAY_CREDENTIALS_ENABLED is unset")
	}
}
