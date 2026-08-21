package storage

import (
	"strconv"
	"strings"
	"testing"
)

// The demo grants a READ-ONLY membership. If DemoViewerRole ever drifted to a
// role userCanManageWorkspace accepts, every signed-up visitor would silently
// become an admin of the demo site's workspace — able to rename its projects and
// rotate its API keys. Pin the constant against the two roles that grant writes.
func TestDemoMembershipIsNeverOwnerOrAdmin(t *testing.T) {
	if DemoViewerRole == "owner" || DemoViewerRole == "admin" {
		t.Fatalf("demo membership role is %q — that grants writes on someone else's workspace", DemoViewerRole)
	}
	if DemoViewerRole != "viewer" {
		t.Errorf("demo membership role = %q, want %q", DemoViewerRole, "viewer")
	}
}

// normalizeWorkspaceRole's default arm answers "member". Before 'viewer' was
// enumerated, a viewer round-tripped through the members API came back as a
// member — a quiet promotion out of read-only, with no error anywhere.
func TestViewerRoleSurvivesNormalization(t *testing.T) {
	for _, in := range []string{"viewer", "VIEWER", " Viewer "} {
		if got := normalizeWorkspaceRole(in); got != DemoViewerRole {
			t.Errorf("normalizeWorkspaceRole(%q) = %q, want %q", in, got, DemoViewerRole)
		}
	}
	// The rest of the scale must be unchanged by the new arm.
	for in, want := range map[string]string{
		"owner": "owner", "admin": "admin", "member": "member", "": "member", "nonsense": "member",
	} {
		if got := normalizeWorkspaceRole(in); got != want {
			t.Errorf("normalizeWorkspaceRole(%q) = %q, want %q", in, got, want)
		}
	}
}

// The members list is read as a privilege ladder. A viewer sorted among the
// members — or, worse, above an admin — reads as "these people have the same
// access", which is exactly what the demo membership must never look like.
func TestViewerSortsLastInTheRoleLadder(t *testing.T) {
	if !(workspaceRoleRank("owner") < workspaceRoleRank("admin") &&
		workspaceRoleRank("admin") < workspaceRoleRank("member") &&
		workspaceRoleRank("member") < workspaceRoleRank(DemoViewerRole)) {
		t.Fatalf("role ladder is not owner < admin < member < viewer: %v", workspaceRoles)
	}
	// An unknown role must not land inside the ladder.
	if workspaceRoleRank("nonsense") <= workspaceRoleRank(DemoViewerRole) {
		t.Errorf("unknown role sorts at %d, at or above viewer (%d)",
			workspaceRoleRank("nonsense"), workspaceRoleRank(DemoViewerRole))
	}
}

// The ORDER BY that actually runs is built from workspaceRoles, so the SQL
// cannot drift from workspaceRoleRank the way a hand-written CASE did — it
// listed owner and admin and swept 'member' and 'viewer' into one ELSE arm.
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
	// No role may be left to the ELSE arm, or two roles would tie.
	if strings.Count(sql, "WHEN ") != len(workspaceRoles) {
		t.Errorf("ordering SQL has %d WHEN arms for %d roles: %q",
			strings.Count(sql, "WHEN "), len(workspaceRoles), sql)
	}
}
