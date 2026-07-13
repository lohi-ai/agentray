package storage

import (
	"strings"
	"testing"
)

// TestGrowthAutopilotPreset pins Growth Autopilot v1 (#2): the config-only PMF
// loop is the growth-lead preset run on a schedule (the design deliberately has
// no separate autopilot agent — see growthLeadPreset). This asserts the loop is
// *complete* purely through config: it grants growth_suggest (which projects
// submit_recommendation, remember, AND send_notification), and it carries the
// measure→diagnose→test→learn→report skills, including the readout/notify close.
func TestGrowthAutopilotPreset(t *testing.T) {
	p, ok := AgentPresetBySlug("growth-lead")
	if !ok {
		t.Fatal("growth-lead preset does not resolve by slug")
	}
	// growth_suggest is the scope that projects send_notification (policy.go),
	// so the scheduled loop can deliver its readout without any bespoke code.
	if !p.Scopes["growth_suggest"] {
		t.Error("growth-lead must grant growth_suggest (submit_recommendation, remember, send_notification)")
	}
	// The autonomous loop must be self-contained in the persona: measure, diagnose,
	// decide, act, learn, and report — the last is the notify close added for #2.
	for _, marker := range []string{"Measure", "Diagnose", "Decide", "Learn", "Report", "send_notification"} {
		if !strings.Contains(p.AgentsMD, marker) {
			t.Errorf("growth-lead scheduled loop persona is missing %q — the PMF cycle is incomplete", marker)
		}
	}
	// The measure→act skills must exist as config (no bespoke Go per the design).
	want := map[string]bool{
		"pmf-scorecard":        false,
		"weakest-link-triage":  false,
		"experiment-design":    false,
		"experiment-readout":   false,
		"capability-request":   false,
		"cycle-readout":        false, // #2: the readout/notify close of the loop
	}
	for _, sk := range p.Skills {
		if _, tracked := want[sk.Name]; tracked {
			want[sk.Name] = true
		}
	}
	for name, found := range want {
		if !found {
			t.Errorf("growth-lead (autopilot) is missing the %q skill", name)
		}
	}
	// The readout skill must actually reach for send_notification (the deliver step).
	var readout AgentPresetSkill
	for _, sk := range p.Skills {
		if sk.Name == "cycle-readout" {
			readout = sk
		}
	}
	if !strings.Contains(readout.Body, "send_notification") {
		t.Error("cycle-readout skill must deliver via send_notification")
	}
}

// The marketplace catalog is product content compiled into the binary, so a
// malformed preset (bad slug, missing persona, empty skill) would ship silently
// and fail only at install time. This guards the catalog's structural invariants
// at build time instead.
func TestAgentPresetsCatalog(t *testing.T) {
	presets := AgentPresets()
	if len(presets) == 0 {
		t.Fatal("expected at least one agent preset")
	}
	seen := map[string]bool{}
	for _, p := range presets {
		if p.Slug == "" || !validAgentSlug(p.Slug) {
			t.Errorf("preset %q has an invalid slug %q (must satisfy the agent slug rule)", p.Name, p.Slug)
		}
		if seen[p.Slug] {
			t.Errorf("duplicate preset slug %q", p.Slug)
		}
		seen[p.Slug] = true
		if p.Name == "" || p.Tagline == "" || p.Description == "" || p.Category == "" {
			t.Errorf("preset %q is missing display copy", p.Slug)
		}
		if p.SoulMD == "" || p.AgentsMD == "" {
			t.Errorf("preset %q is missing its persona (soul/agents md)", p.Slug)
		}
		if len(p.Scopes) == 0 {
			t.Errorf("preset %q grants no scopes", p.Slug)
		}
		// A foundation agent must carry the identity capability (author charts).
		if !p.Scopes["analyze_build"] {
			t.Errorf("preset %q does not grant analyze_build (chart/dashboard authoring)", p.Slug)
		}
		for _, sk := range p.Skills {
			if sk.Name == "" || sk.Description == "" || sk.Body == "" {
				t.Errorf("preset %q has an incomplete skill %q", p.Slug, sk.Name)
			}
		}
	}
}

func TestAgentPresetBySlug(t *testing.T) {
	if _, ok := AgentPresetBySlug("growth-lead"); !ok {
		t.Error("expected growth-lead preset to resolve")
	}
	if _, ok := AgentPresetBySlug("does-not-exist"); ok {
		t.Error("expected unknown slug to miss")
	}
}

// TestDataAnalystPreset pins the config-only SQL/dashboard agent: it must show in
// the marketplace catalog (what GET /api/marketplace/agents returns) and resolve by
// slug (the only preset-specific step in InstallAgentPreset — the rest is generic).
// It also asserts the scopes that grant the SQL/chart tools, so the agent can do its
// job purely through config with no bespoke backend.
func TestDataAnalystPreset(t *testing.T) {
	// Shows in the marketplace list.
	var inCatalog bool
	for _, p := range AgentPresets() {
		if p.Slug == "data-analyst" {
			inCatalog = true
		}
	}
	if !inCatalog {
		t.Fatal("data-analyst preset is not in the marketplace catalog")
	}

	// Resolves for install.
	p, ok := AgentPresetBySlug("data-analyst")
	if !ok {
		t.Fatal("data-analyst preset does not resolve by slug (install would 400)")
	}

	// data_quality grants run_sql/explore_events; analyze_build grants
	// run_sql/run_insight/create_dashboard/create_chart (see policy.go scopeTools).
	if !p.Scopes["data_quality"] {
		t.Error("data-analyst must grant data_quality (run_sql, explore_events)")
	}
	if !p.Scopes["analyze_build"] {
		t.Error("data-analyst must grant analyze_build (create_chart, create_dashboard)")
	}

	// Schema/SQL knowledge must live in a skill (config), not bespoke Go.
	want := map[string]bool{"write-sql": false, "chart-from-sql": false}
	for _, sk := range p.Skills {
		if _, tracked := want[sk.Name]; tracked {
			want[sk.Name] = true
		}
	}
	for name, found := range want {
		if !found {
			t.Errorf("data-analyst is missing the %q skill", name)
		}
	}
}

// TestMarketingLeadPreset pins the config-only marketing team agent: the full
// ORIENT→PLAN→CREATE→REVIEW→PUBLISH→LEARN loop must be expressible as persona + scopes
// + skills over the generic runtime (no bespoke Go), the human review gate must
// be explicit in the persona, and everything channel-specific (hosts, creds,
// call shapes) must live in the publish-manifest skill so a new channel is a
// skill edit, not a backend change.
func TestMarketingLeadPreset(t *testing.T) {
	p, ok := AgentPresetBySlug("marketing-lead")
	if !ok {
		t.Fatal("marketing-lead preset does not resolve by slug")
	}
	if p.Category != "marketing" {
		t.Errorf("marketing-lead category = %q, want marketing", p.Category)
	}
	// growth_suggest projects submit_recommendation (dev tickets + publish audit),
	// remember (calendar state), and send_notification (draft delivery).
	if !p.Scopes["growth_suggest"] {
		t.Error("marketing-lead must grant growth_suggest (submit_recommendation, remember, send_notification)")
	}
	// data_quality + analyze_build ground the plan in real product data.
	if !p.Scopes["data_quality"] || !p.Scopes["analyze_build"] {
		t.Error("marketing-lead must grant data_quality and analyze_build (plan from data)")
	}

	// The scheduled loop must be self-contained, parallelize channel drafts, and
	// carry the conditional review gate: at suggest/scheduled an unattended run
	// hard-stops at drafts; at the opt-in `auto` rung it may publish but owes
	// the audit trail (submit_recommendation of what shipped). The gate is read
	// off the toolset — the runner strips http_request below `auto` — so the
	// persona must reference both branches.
	for _, marker := range []string{
		"Orient", "Plan", "Create", "Review gate", "Learn", "spawn_subagent", "send_notification",
		"publish from an unattended run",
		"explicitly opted in",
		"audit trail",
	} {
		if !strings.Contains(p.AgentsMD, marker) {
			t.Errorf("marketing-lead loop persona is missing %q", marker)
		}
	}
	// The audit-trail duty for auto-mode publishes must be explicit in the
	// never-list too: no unattended publish without a submit_recommendation.
	if !strings.Contains(p.AgentsMD, "Never publish unattended without filing the audit-trail") {
		t.Error("marketing-lead never-list is missing the auto-mode audit-trail duty")
	}

	// The loop's stages ship as skills (config), not bespoke Go.
	want := map[string]bool{
		"content-calendar": false,
		"channel-port":     false,
		"publish-manifest": false,
		"image-gen":        false,
		"video-script":     false,
		"trend-scout":      false,
		"dev-ticket":       false,
	}
	var publish, video AgentPresetSkill
	for _, sk := range p.Skills {
		if _, tracked := want[sk.Name]; tracked {
			want[sk.Name] = true
		}
		switch sk.Name {
		case "publish-manifest":
			publish = sk
		case "video-script":
			video = sk
		}
	}
	for name, found := range want {
		if !found {
			t.Errorf("marketing-lead is missing the %q skill", name)
		}
	}

	// The publish contract must route through the governed http_request tool with
	// write-only cred placeholders — never a bespoke publish tool.
	for _, marker := range []string{"http_request", "allow_hosts", "{{cred:", "graph.facebook.com", "api.x.com", "oauth.reddit.com"} {
		if !strings.Contains(publish.Body, marker) {
			t.Errorf("publish-manifest skill is missing %q", marker)
		}
	}
	// Video is human-produced by design (user decision): script only, no claim of
	// generation or publishing.
	if !strings.Contains(video.Body, "asking the human to record") {
		t.Error("video-script skill must hand production to a human")
	}
}
