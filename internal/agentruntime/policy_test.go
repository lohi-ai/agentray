package agentruntime

import (
	"context"
	"testing"

	"github.com/lohi-ai/agentray/agentcore"
	"github.com/lohi-ai/agentray/internal/storage"
)

// TestDataAnalystPresetGrantsSQLTools proves the config-only Data Analyst preset
// installs as a *working* SQL/dashboard agent: its capability scopes, run through
// the same policy a human-configured agent uses, resolve to the run_sql + chart
// authoring tools. This is the end-to-end guarantee that no bespoke backend is
// needed — the generic runtime + the preset's scopes are the whole agent.
func TestDataAnalystPresetGrantsSQLTools(t *testing.T) {
	p, ok := storage.AgentPresetBySlug("data-analyst")
	if !ok {
		t.Fatal("data-analyst preset must resolve")
	}
	granted := map[string]bool{}
	for _, n := range ScopeToolNames(ScopesFromMap(p.Scopes)) {
		granted[n] = true
	}
	for _, want := range []string{ToolRunSQL, ToolExploreEvents, ToolRunInsight, ToolCreateDashboard, ToolCreateChart} {
		if !granted[want] {
			t.Errorf("data-analyst preset scopes do not grant %q — agent could not do its job", want)
		}
	}
}

// TestScopesFromMapRoundTrip verifies the stored agent_configs scope map (the
// business/usecase tool groups configured in the UI) maps onto the four Scopes
// and on into the granted tool allow-list. This is the seam between the web
// "Capability tools" checklist and the runtime policy.
func TestScopesFromMapRoundTrip(t *testing.T) {
	s := ScopesFromMap(map[string]bool{
		"monitor":        true,
		"analyze_build":  true,
		"data_quality":   false,
		"growth_suggest": false,
		"unknown_scope":  true, // an unrecognized key contributes nothing
	})
	if !s.Monitor || !s.AnalyzeBuild {
		t.Fatalf("enabled scopes not mapped: %+v", s)
	}
	if s.DataQuality || s.GrowthSuggest {
		t.Fatalf("disabled scopes leaked on: %+v", s)
	}

	granted := map[string]bool{}
	for _, n := range ScopeToolNames(s) {
		granted[n] = true
	}
	// monitor -> activity_summary, recent_events; analyze_build -> run_sql + authoring.
	for _, want := range []string{ToolActivitySummary, ToolRecentEvents, ToolRunSQL, ToolRunInsight, ToolCreateChart} {
		if !granted[want] {
			t.Errorf("expected %q granted by monitor+analyze_build", want)
		}
	}
	// A tool exclusive to a disabled scope (persons via data_quality) stays denied.
	if granted[ToolExploreEvents] {
		t.Error("explore_events must stay denied when data_quality is off")
	}
}

// TestScopesFromMapEmpty verifies a nil/empty map yields an all-off, no-tool agent.
func TestScopesFromMapEmpty(t *testing.T) {
	if names := ScopeToolNames(ScopesFromMap(nil)); len(names) != 0 {
		t.Fatalf("empty scope map should grant no tools, got %v", names)
	}
}

// TestPolicyForScopesDefaultDeny verifies an all-off Scopes permits no tools.
func TestPolicyForScopesDefaultDeny(t *testing.T) {
	p := PolicyForScopes(Scopes{})
	if got := p.PermittedTools(context.Background(), []string{ToolActivitySummary, ToolRunSQL}); len(got) != 0 {
		t.Fatalf("default-deny should permit nothing, got %v", got)
	}
}

// TestPolicyForScopesUnion verifies enabled scopes grant exactly their tools.
func TestPolicyForScopesUnion(t *testing.T) {
	p := PolicyForScopes(Scopes{Monitor: true, DataQuality: true})
	all := []string{ToolActivitySummary, ToolRecentEvents, ToolExploreEvents, ToolPersons, ToolRunSQL}
	got := map[string]bool{}
	for _, n := range p.PermittedTools(context.Background(), all) {
		got[n] = true
	}
	// monitor -> activity_summary, recent_events; data_quality -> explore, persons, run_sql
	for _, want := range []string{ToolActivitySummary, ToolRecentEvents, ToolExploreEvents, ToolPersons, ToolRunSQL} {
		if !got[want] {
			t.Errorf("expected %q permitted under monitor+data_quality", want)
		}
	}
	// analyze_build is off, but run_sql is granted via data_quality — confirm a
	// tool exclusive to a disabled scope stays denied.
	pNoSQL := PolicyForScopes(Scopes{Monitor: true})
	if d := pNoSQL.Allow(context.Background(), agentcore.ToolCall{Name: ToolRunSQL}); d.Allow {
		t.Error("run_sql must be denied when only monitor is enabled")
	}
}
