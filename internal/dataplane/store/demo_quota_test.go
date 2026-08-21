package storage

import (
	"context"
	"testing"
	"time"
)

// The role scale has two independent readings: how it SORTS (demo_role_test.go)
// and whether it may WRITE. This file pins the second one, because it is what
// the HTTP write guard asks and getting it wrong is either a hole in someone
// else's workspace or every user locked out of their own.

func TestRoleMayWrite(t *testing.T) {
	for role, want := range map[string]bool{
		"owner":          true,
		"admin":          true,
		"member":         true,
		"OWNER":          true,
		"  admin  ":      true,
		"viewer":         false,
		"VIEWER":         false,
		"":               false,
		"guest":          false,
		"administrator":  false,
		"owner-ish":      false,
		"read-write-all": false,
	} {
		if got := RoleMayWrite(role); got != want {
			t.Errorf("RoleMayWrite(%q) = %v, want %v", role, got, want)
		}
	}
}

// Every role on the scale must have a deliberate answer. A role added to
// workspaceRoles and not classified here is read-only by default — which is the
// safe direction — but it must be a decision someone made, not one they missed,
// so this test is the place that forces it.
func TestEveryWorkspaceRoleIsClassified(t *testing.T) {
	writers := 0
	for _, role := range workspaceRoles {
		if RoleMayWrite(role) {
			writers++
		}
	}
	if writers != len(writeRoles) {
		t.Fatalf("%d of %v may write, but writeRoles has %d entries — a role was added to the scale without deciding whether it writes",
			writers, workspaceRoles, len(writeRoles))
	}
	// The one that must never drift.
	if RoleMayWrite(DemoViewerRole) {
		t.Fatalf("the demo membership role %q may write; every signed-up visitor could change someone else's site", DemoViewerRole)
	}
}

// The project API key is a write credential — it is what the customer's site
// posts events with — so a read-only member must not be handed it. Without this
// a demo viewer could read the key off /api/auth/me and start writing events
// into the demo owner's live project, which is exactly what the viewer role is
// supposed to prevent.
func TestAPIKeyIsRedactedForReadOnlyRoles(t *testing.T) {
	for _, role := range []string{"owner", "admin", "member"} {
		project := Project{APIKey: "agentray_secret", Role: role}
		project.redactAPIKeyForRole()
		if project.APIKey != "agentray_secret" {
			t.Errorf("role %q lost the api key it is entitled to", role)
		}
	}
	for _, role := range []string{DemoViewerRole, "", "some-future-role"} {
		project := Project{APIKey: "agentray_secret", Role: role}
		project.redactAPIKeyForRole()
		if project.APIKey != "" {
			t.Errorf("role %q kept the api key: %q", role, project.APIKey)
		}
	}
}

// --- the demo agent budget -------------------------------------------------

// A ceiling of zero is an operator switching the demo agent off, not switching
// the limit off. Reading it the other way round would turn a lockdown setting
// into an unbounded bill on the instance owner's model key. Neither arm here
// touches the database, which is the point: the refusal is decided before the
// ledger is consulted.
func TestZeroOrNegativeCapAllowsNothing(t *testing.T) {
	s := &Store{}
	for _, limit := range []int{0, -1, -100} {
		quota, err := s.ConsumeDemoAgentRun(context.Background(), "55555555-5555-5555-5555-555555555555", limit)
		if err != nil {
			t.Fatalf("limit %d: %v", limit, err)
		}
		if quota.Allowed {
			t.Errorf("limit %d allowed a run", limit)
		}
		if quota.Limit != limit {
			t.Errorf("limit %d reported back as %d", limit, quota.Limit)
		}
	}
}

// A caller with no usable id has no budget to spend, and must not reach the
// ledger as a cast error that the HTTP layer would report as a 500.
func TestAMalformedUserSpendsNothing(t *testing.T) {
	s := &Store{}
	quota, err := s.ConsumeDemoAgentRun(context.Background(), "not-a-uuid", 5)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if quota.Allowed {
		t.Error("a malformed user id was granted a demo run")
	}
	used, err := s.DemoAgentRunsUsed(context.Background(), "not-a-uuid")
	if err != nil || used != 0 {
		t.Errorf("DemoAgentRunsUsed(malformed) = %d, %v; want 0, nil", used, err)
	}
}

// The refusal has to promise a moment the caller can wait for, so the reset is
// the next UTC midnight rather than "24 hours from whenever you asked".
func TestResetIsTheNextUTCMidnight(t *testing.T) {
	cases := []struct {
		now  string
		want string
	}{
		{"2026-08-21T00:00:00Z", "2026-08-22T00:00:00Z"},
		{"2026-08-21T23:59:59Z", "2026-08-22T00:00:00Z"},
		{"2026-12-31T18:00:00Z", "2027-01-01T00:00:00Z"},
	}
	for _, tc := range cases {
		now, err := time.Parse(time.RFC3339, tc.now)
		if err != nil {
			t.Fatal(err)
		}
		want, err := time.Parse(time.RFC3339, tc.want)
		if err != nil {
			t.Fatal(err)
		}
		if got := nextUTCDay(now); !got.Equal(want) {
			t.Errorf("nextUTCDay(%s) = %s, want %s", tc.now, got, want)
		}
	}
}
