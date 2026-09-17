package storage

import (
	"context"
	"testing"
)

// alerts_test.go — live tests for alert rule/channel provisioning. Needs the
// compose Postgres; skips without one (openConvTestStore).

// projectWorkspace resolves the workspace a seeded project belongs to.
func projectWorkspace(t *testing.T, s *Store, projectID string) string {
	t.Helper()
	var wsID string
	if err := s.pg.QueryRow(context.Background(),
		`SELECT workspace_id::text FROM projects WHERE id = $1`, projectID).Scan(&wsID); err != nil {
		t.Fatalf("project workspace: %v", err)
	}
	return wsID
}

func digestRules(t *testing.T, s *Store, userID, projectID string) []AlertRule {
	t.Helper()
	rules, err := s.ListAlertRules(context.Background(), userID, projectID)
	if err != nil {
		t.Fatalf("list rules: %v", err)
	}
	out := []AlertRule{}
	for _, r := range rules {
		if r.SourceKind == "digest" {
			out = append(out, r)
		}
	}
	return out
}

// A project created through the real path carries a weekly digest from the
// start — enabled, on the Monday-morning cron, with no channels yet so it
// computes silently until a delivery target exists.
func TestProjectSeedProvisionsDigestRule(t *testing.T) {
	s := openConvTestStore(t)
	ctx := context.Background()
	userID, projectID := seedConvProject(t, s)
	wsID := projectWorkspace(t, s, projectID)

	project, err := s.CreateWorkspaceProject(ctx, userID, wsID, "digest-proj")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	rules := digestRules(t, s, userID, project.ID)
	if len(rules) != 1 {
		t.Fatalf("digest rules = %d, want 1", len(rules))
	}
	rule := rules[0]
	if !rule.Enabled || rule.Schedule != "0 9 * * 1" || len(rule.Channels) != 0 {
		t.Fatalf("digest rule = %+v, want enabled weekly channel-less", rule)
	}

	// Re-seeding the same project must not double the rule.
	if err := s.ensureDigestRule(ctx, project.ID); err != nil {
		t.Fatalf("re-ensure: %v", err)
	}
	if got := len(digestRules(t, s, userID, project.ID)); got != 1 {
		t.Fatalf("digest rules after re-ensure = %d, want 1", got)
	}
}

// The first channel a workspace adds is what makes the seeded digest able to
// deliver — it is attached to every channel-less digest rule. A rule the owner
// already manages (non-empty channels) is left alone, and a second channel is
// not auto-attached.
func TestFirstChannelAttachesToDigestRules(t *testing.T) {
	s := openConvTestStore(t)
	ctx := context.Background()
	userID, projectID := seedConvProject(t, s)
	wsID := projectWorkspace(t, s, projectID)

	project, err := s.CreateWorkspaceProject(ctx, userID, wsID, "digest-proj")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	// A second project whose digest the owner deliberately pointed at a
	// channel it already had — simulated by a non-empty channel list.
	other, err := s.CreateWorkspaceProject(ctx, userID, wsID, "digest-proj-2")
	if err != nil {
		t.Fatalf("create other project: %v", err)
	}
	if _, err := s.pg.Exec(ctx, `
UPDATE alert_rules SET channels = '["00000000-0000-0000-0000-000000000000"]'::jsonb
WHERE project_id = $1 AND source_kind = 'digest'`, other.ID); err != nil {
		t.Fatalf("pin other digest channels: %v", err)
	}

	ch, err := s.CreateAlertChannel(ctx, userID, wsID, AlertChannel{Kind: "slack", Name: "ops", Config: []byte(`{"webhook":"{{cred:SLACK}}"}`)})
	if err != nil {
		t.Fatalf("create channel: %v", err)
	}

	rules := digestRules(t, s, userID, project.ID)
	if len(rules) != 1 || len(rules[0].Channels) != 1 || rules[0].Channels[0] != ch.ID {
		t.Fatalf("digest channels = %+v, want [%s]", rules, ch.ID)
	}
	otherRules := digestRules(t, s, userID, other.ID)
	if len(otherRules) != 1 || len(otherRules[0].Channels) != 1 || otherRules[0].Channels[0] == ch.ID {
		t.Fatalf("managed digest channels = %+v, want untouched", otherRules)
	}

	// A second channel does not re-attach: the rule's list is no longer empty.
	if _, err := s.CreateAlertChannel(ctx, userID, wsID, AlertChannel{Kind: "email", Name: "mail", Config: []byte(`{"to":"a@b.c"}`)}); err != nil {
		t.Fatalf("second channel: %v", err)
	}
	rules = digestRules(t, s, userID, project.ID)
	if len(rules[0].Channels) != 1 || rules[0].Channels[0] != ch.ID {
		t.Fatalf("digest channels after second channel = %+v, want [%s]", rules[0].Channels, ch.ID)
	}
}

// A project created in a workspace that ALREADY has a channel inherits that
// channel on its seeded digest immediately, so it doesn't deliver nowhere.
func TestProjectSeedInheritsExistingChannel(t *testing.T) {
	s := openConvTestStore(t)
	ctx := context.Background()
	userID, projectID := seedConvProject(t, s)
	wsID := projectWorkspace(t, s, projectID)

	ch, err := s.CreateAlertChannel(ctx, userID, wsID, AlertChannel{
		Kind: "slack",
		Name: "team-slack",
	})
	if err != nil {
		t.Fatalf("create channel: %v", err)
	}

	// Now create a NEW project in the same workspace. Its digest should inherit ch.
	newProj, err := s.CreateWorkspaceProject(ctx, userID, wsID, "digest-proj-after-ch")
	if err != nil {
		t.Fatalf("create second project: %v", err)
	}
	rules := digestRules(t, s, userID, newProj.ID)
	if len(rules) != 1 {
		t.Fatalf("digest rules = %d, want 1", len(rules))
	}
	if len(rules[0].Channels) != 1 || rules[0].Channels[0] != ch.ID {
		t.Fatalf("new project digest channels = %+v, want [%s]", rules[0].Channels, ch.ID)
	}
}
