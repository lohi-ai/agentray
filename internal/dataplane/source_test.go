package dataplane

import (
	"slices"
	"testing"
)

// TestPostgresPluginRegistered is the "easy to add a source" contract: a
// plugin that calls Register from init shows up in SourceKinds without any
// other wiring. postgres is the built-in proof.
func TestPostgresPluginRegistered(t *testing.T) {
	kinds := SourceKinds()
	if !slices.Contains(kinds, "postgres") {
		t.Fatalf("postgres source plugin not registered; got %v", kinds)
	}
}
