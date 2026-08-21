package agentruntime

import "github.com/lohi-ai/agentray/agentcore"

// Scopes are the four independently-toggleable capability scopes (§3). Each maps
// to a set of analytics tools.
type Scopes struct {
	Monitor       bool `json:"monitor"`
	DataQuality   bool `json:"data_quality"`
	AnalyzeBuild  bool `json:"analyze_build"`
	GrowthSuggest bool `json:"growth_suggest"`
}

// scopeTools maps each scope to the tools it grants (§3). monitor/data_quality
// are read-only; analyze_build adds insight + chart/dashboard authoring;
// growth_suggest adds the recommendation + memory writes.
var scopeTools = map[string][]string{
	"monitor":        {ToolActivitySummary, ToolRecentEvents},
	"data_quality":   {ToolExploreEvents, ToolPersons, ToolRunSQL},
	"analyze_build":  {ToolRunSQL, ToolRunInsight, ToolRunFunnel, ToolRunRetention, ToolListDashboards, ToolCreateDashboard, ToolCreateChart},
	"growth_suggest": {ToolActivitySummary, ToolPersons, ToolSubmitRec, ToolProposeTest, ToolTestStatus, ToolListTests, ToolRemember, ToolSendNotification},
}

// readTools classifies which scope-granted tools READ project data, versus the
// authoring/side-effect tools (create_chart, create_dashboard,
// submit_recommendation, remember, send_notification). Kept beside scopeTools
// so a scope gaining a new tool is classified in the same file — the evidence
// guard (evidence_guard.go) derives its evidence set from this, and a read
// tool missing here would silently be treated as non-evidence.
var readTools = map[string]bool{
	ToolActivitySummary: true,
	ToolRecentEvents:    true,
	ToolExploreEvents:   true,
	ToolPersons:         true,
	ToolRunSQL:          true,
	ToolRunInsight:      true,
	ToolRunFunnel:       true,
	ToolRunRetention:    true,
	ToolListDashboards:  true,
	// test_status reads the live experiment out of the event store against a
	// committed threshold. It is the pre-product agent's activity_summary, and
	// leaving it unclassified would nudge the one agent that DID check.
	ToolTestStatus: true,
	ToolListTests:  true,
}

// ScopesFromMap maps a stored scope map (agent_configs columns) onto Scopes.
func ScopesFromMap(m map[string]bool) Scopes {
	return Scopes{
		Monitor:       m["monitor"],
		DataQuality:   m["data_quality"],
		AnalyzeBuild:  m["analyze_build"],
		GrowthSuggest: m["growth_suggest"],
	}
}

// ScopeToolNames returns the union of tool names granted by the enabled scopes.
// A scope that is off contributes no tools. Exported so the builder can extend
// the allow-list with non-scope tools (e.g. a sandboxed run_shell) before
// constructing the Policy.
func ScopeToolNames(s Scopes) []string {
	allowed := map[string]bool{}
	add := func(on bool, scope string) {
		if !on {
			return
		}
		for _, t := range scopeTools[scope] {
			allowed[t] = true
		}
	}
	add(s.Monitor, "monitor")
	add(s.DataQuality, "data_quality")
	add(s.AnalyzeBuild, "analyze_build")
	add(s.GrowthSuggest, "growth_suggest")

	names := make([]string, 0, len(allowed))
	for n := range allowed {
		names = append(names, n)
	}
	return names
}

// ReadOnlyToolNames narrows a granted tool list to the ones that only READ.
//
// It exists for the shared demo (internal/app/demo_guard.go): a visitor there
// may ask the agent anything — that is the point of the demo — and may not
// change the site they are asking about. Without this, "ask the agent" is a
// way around every check the write guard makes, because the agent holds
// create_dashboard and create_chart and does what it is asked.
//
// It filters through readTools, so a scope that gains a tool and does not
// classify it as a read is withheld from a read-only run. That is the same
// fail-closed direction the evidence guard depends on, and it means the
// decision for a new tool is made once, in one file, beside the scope.
func ReadOnlyToolNames(names []string) []string {
	out := make([]string, 0, len(names))
	for _, name := range names {
		if readTools[name] {
			out = append(out, name)
		}
	}
	return out
}

// PolicyForScopes resolves enabled scopes into a default-deny agentcore.Policy.
// The union of enabled scopes' tools is the allow-list.
func PolicyForScopes(s Scopes) agentcore.Policy {
	return agentcore.NewAllowList(ScopeToolNames(s)...)
}
