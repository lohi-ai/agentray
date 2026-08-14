package storage

import "testing"

func TestNormalizeScopesKeepsKnownKeys(t *testing.T) {
	got := normalizeScopes(map[string]bool{
		"monitor":        true,
		"data_quality":   true,
		"unknown_scope":  true,
		"growth_suggest": false,
	})

	if !got["monitor"] {
		t.Error("monitor = false, want true")
	}
	if !got["data_quality"] {
		t.Error("data_quality = false, want true")
	}
	if got["growth_suggest"] {
		t.Error("growth_suggest = true, want false")
	}
	if got["analyze_build"] {
		t.Error("analyze_build = true, want default false")
	}
	if _, ok := got["unknown_scope"]; ok {
		t.Error("unknown scope must not survive normalization")
	}
	if len(got) != 4 {
		t.Errorf("normalized scopes has %d keys, want 4", len(got))
	}
}

func TestNormalizeScopesDefaultDeny(t *testing.T) {
	got := normalizeScopes(nil)
	for k, v := range got {
		if v {
			t.Errorf("%s = true, want default false", k)
		}
	}
	if len(got) != 4 {
		t.Errorf("normalized nil has %d keys, want 4", len(got))
	}
}
