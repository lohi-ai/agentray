package storage

import (
	"context"
	"testing"
)

// The weekly digest is auto-provisioned at project creation: exactly one
// 'digest' rule, enabled, on the Monday 09:00 UTC cron, with no channels.
func TestEnsureDefaultWeeklyDigestSeedsOnce(t *testing.T) {
	s := openConvTestStore(t)
	ctx := context.Background()
	_, projectID := seedConvProject(t, s)

	if err := s.EnsureDefaultWeeklyDigest(ctx, projectID); err != nil {
		t.Fatalf("first seed: %v", err)
	}
	var (
		name, schedule string
		enabled        bool
		channels       string
		count          int
	)
	if err := s.pg.QueryRow(ctx, `
SELECT name, schedule_cron, enabled, channels::text, count(*) OVER ()
FROM alert_rules WHERE project_id = $1 AND source_kind = 'digest'`, projectID).
		Scan(&name, &schedule, &enabled, &channels, &count); err != nil {
		t.Fatalf("read seeded rule: %v", err)
	}
	if count != 1 {
		t.Fatalf("digest rules = %d, want exactly 1", count)
	}
	if name != "Weekly Decision Digest" || schedule != "0 9 * * 1" || !enabled || channels != "[]" {
		t.Fatalf("seeded rule = (%q, %q, enabled=%v, channels=%s)", name, schedule, enabled, channels)
	}

	if err := s.EnsureDefaultWeeklyDigest(ctx, projectID); err != nil {
		t.Fatalf("second seed: %v", err)
	}
	if err := s.pg.QueryRow(ctx, `
SELECT count(*) FROM alert_rules WHERE project_id = $1 AND source_kind = 'digest'`, projectID).
		Scan(&count); err != nil {
		t.Fatalf("recount: %v", err)
	}
	if count != 1 {
		t.Fatalf("digest rules after re-seed = %d, want 1 (seed duplicated)", count)
	}
}

// A rule the owner paused or re-pointed at a channel must survive a re-seed —
// the ensure is a default, not an enforcement.
func TestEnsureDefaultWeeklyDigestLeavesAnEditedRuleAlone(t *testing.T) {
	s := openConvTestStore(t)
	ctx := context.Background()
	userID, projectID := seedConvProject(t, s)

	if err := s.EnsureDefaultWeeklyDigest(ctx, projectID); err != nil {
		t.Fatalf("seed: %v", err)
	}
	rules, err := s.ListAlertRules(ctx, userID, projectID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rules) != 1 {
		t.Fatalf("rules = %d, want 1", len(rules))
	}
	rule := rules[0]
	rule.Enabled = false
	if err := s.UpdateAlertRule(ctx, userID, projectID, rule.ID, rule); err != nil {
		t.Fatalf("owner paused the digest: %v", err)
	}

	if err := s.EnsureDefaultWeeklyDigest(ctx, projectID); err != nil {
		t.Fatalf("re-seed: %v", err)
	}
	rules, err = s.ListAlertRules(ctx, userID, projectID)
	if err != nil {
		t.Fatalf("re-list: %v", err)
	}
	if len(rules) != 1 || rules[0].Enabled {
		t.Fatalf("re-seed resurrected/duplicated the digest: %+v", rules)
	}
}
