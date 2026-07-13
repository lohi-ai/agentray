package storage

import "testing"

// TestValidAutonomy pins the ladder: exactly suggest/scheduled/auto are
// accepted, so UpsertAgentConfig rejects anything else (after folding empty to
// the 'suggest' default) and existing agents stay on the strictest rung.
func TestValidAutonomy(t *testing.T) {
	for _, ok := range []string{AutonomySuggest, AutonomyScheduled, AutonomyAuto} {
		if !ValidAutonomy(ok) {
			t.Errorf("ValidAutonomy(%q) = false, want true", ok)
		}
	}
	for _, bad := range []string{"", "AUTO", "Suggest", "manual", "full", "auto "} {
		if ValidAutonomy(bad) {
			t.Errorf("ValidAutonomy(%q) = true, want false", bad)
		}
	}
}
