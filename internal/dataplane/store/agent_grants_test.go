package storage

import "testing"

func TestAnyScopeTrue(t *testing.T) {
	if anyScopeTrue(map[string]bool{"monitor": false, "analyze_build": false}) {
		t.Fatal("all-false map should report no scopes")
	}
	if !anyScopeTrue(map[string]bool{"monitor": false, "analyze_build": true}) {
		t.Fatal("a single true scope should report a scope")
	}
	if anyScopeTrue(map[string]bool{}) {
		t.Fatal("empty map should report no scopes")
	}
}

func TestIntersectScopes(t *testing.T) {
	own := map[string]bool{"monitor": true, "data_quality": true, "analyze_build": true, "growth_suggest": true}
	cap := map[string]bool{"monitor": true, "data_quality": false, "analyze_build": true, "growth_suggest": false}

	got := intersectScopes(own, cap)
	want := map[string]bool{"monitor": true, "data_quality": false, "analyze_build": true, "growth_suggest": false}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("scope %q: got %v want %v", k, got[k], v)
		}
	}

	// Capping can only remove, never add: a scope the agent lacks stays denied
	// even if the grant would allow it.
	got = intersectScopes(map[string]bool{"analyze_build": false}, map[string]bool{"analyze_build": true})
	if got["analyze_build"] {
		t.Fatal("intersection must not grant a capability the agent lacks")
	}
}
