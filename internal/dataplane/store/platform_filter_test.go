package storage

import (
	"strings"
	"testing"
)

func TestPlatformClause(t *testing.T) {
	tests := []struct {
		name       string
		platform   string
		wantClause string
		wantArg    any
		wantOK     bool
	}{
		{"empty filter adds nothing", "", "", nil, false},
		{"whitespace adds nothing", "   ", "", nil, false},
		{"unknown selects the undetermined rows", "unknown", "coalesce(platform, '') = ''", nil, true},
		{"a named platform binds", "ios", "coalesce(platform, '') = ?", "ios", true},
		{"case folded", "IOS", "coalesce(platform, '') = ?", "ios", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			clause, arg, ok := platformClause(tc.platform)
			if ok != tc.wantOK || clause != tc.wantClause || arg != tc.wantArg {
				t.Fatalf("platformClause(%q) = (%q, %v, %v), want (%q, %v, %v)",
					tc.platform, clause, arg, ok, tc.wantClause, tc.wantArg, tc.wantOK)
			}
		})
	}
}

// The clause and its argument have to travel together: DuckDB binds ? by
// position, so a platform arg appended without its clause (or the other way
// round) silently filters a different column instead of erroring.
func TestFilteredWhereCarriesPlatform(t *testing.T) {
	where, args := filteredWhereWithDefault("11111111-1111-1111-1111-111111111111",
		EventFilter{Platform: "ios", EventName: "user.pageview"}, false)
	if !strings.Contains(where, "coalesce(platform, '') = ?") {
		t.Fatalf("where clause missing platform: %s", where)
	}
	if strings.Count(where, "?") != len(args) {
		t.Fatalf("placeholder/arg mismatch: %d placeholders, %d args (%s)", strings.Count(where, "?"), len(args), where)
	}
	if args[len(args)-1] != "ios" {
		t.Fatalf("platform arg not bound last: %v", args)
	}
}

// 'unknown' contributes a clause and no argument — the one case where a clause
// without an arg is correct.
func TestFilteredWhereUnknownPlatformBindsNoArg(t *testing.T) {
	where, args := filteredWhereWithDefault("11111111-1111-1111-1111-111111111111",
		EventFilter{Platform: PlatformUnknown}, false)
	if !strings.Contains(where, "coalesce(platform, '') = ''") {
		t.Fatalf("where clause missing unknown-platform test: %s", where)
	}
	if strings.Count(where, "?") != len(args) {
		t.Fatalf("placeholder/arg mismatch: %d placeholders, %d args (%s)", strings.Count(where, "?"), len(args), where)
	}
}

func TestWorkspaceFilteredWhereCarriesPlatform(t *testing.T) {
	where, args := workspaceFilteredWhere([]string{"11111111-1111-1111-1111-111111111111"},
		EventFilter{Platform: "android"}, false)
	if !strings.Contains(where, "coalesce(platform, '') = ?") {
		t.Fatalf("where clause missing platform: %s", where)
	}
	if strings.Count(where, "?") != len(args) {
		t.Fatalf("placeholder/arg mismatch: %d placeholders, %d args (%s)", strings.Count(where, "?"), len(args), where)
	}
}
