package storage

import (
	"strconv"
	"strings"
	"testing"
)

func TestNormalizeWorkspaceRole(t *testing.T) {
	for in, want := range map[string]string{
		"owner": "owner", "admin": "admin", "member": "member",
		"": "member", "nonsense": "member",
		"viewer": "member", "VIEWER": "member", " Viewer ": "member",
	} {
		if got := normalizeWorkspaceRole(in); got != want {
			t.Errorf("normalizeWorkspaceRole(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRoleLadderIsOwnerAdminMember(t *testing.T) {
	if !(workspaceRoleRank("owner") < workspaceRoleRank("admin") &&
		workspaceRoleRank("admin") < workspaceRoleRank("member")) {
		t.Fatalf("role ladder is not owner < admin < member: %v", workspaceRoles)
	}
	if workspaceRoleRank("nonsense") <= workspaceRoleRank("member") {
		t.Errorf("unknown role sorts at %d, at or above member (%d)",
			workspaceRoleRank("nonsense"), workspaceRoleRank("member"))
	}
}

// The ORDER BY that actually runs is built from workspaceRoles, so the SQL
// cannot drift from workspaceRoleRank the way a hand-written CASE did.
func TestMemberOrderSQLMatchesTheRoleLadder(t *testing.T) {
	sql := workspaceRoleOrderSQL("wm.role")
	if !strings.HasPrefix(sql, "CASE wm.role ") || !strings.HasSuffix(sql, " END") {
		t.Fatalf("not a CASE expression: %q", sql)
	}
	for _, role := range workspaceRoles {
		want := "WHEN '" + role + "' THEN " + strconv.Itoa(workspaceRoleRank(role))
		if !strings.Contains(sql, want) {
			t.Errorf("ordering SQL is missing %q: %q", want, sql)
		}
	}
	if strings.Count(sql, "WHEN ") != len(workspaceRoles) {
		t.Errorf("ordering SQL has %d WHEN arms for %d roles: %q",
			strings.Count(sql, "WHEN "), len(workspaceRoles), sql)
	}
}
