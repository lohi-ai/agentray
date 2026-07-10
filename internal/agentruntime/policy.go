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
	"analyze_build":  {ToolRunSQL, ToolRunInsight, ToolListDashboards, ToolCreateDashboard, ToolCreateChart},
	"growth_suggest": {ToolActivitySummary, ToolPersons, ToolSubmitRec, ToolRemember, ToolSendNotification},
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

// PolicyForScopes resolves enabled scopes into a default-deny agentcore.Policy.
// The union of enabled scopes' tools is the allow-list.
func PolicyForScopes(s Scopes) agentcore.Policy {
	return agentcore.NewAllowList(ScopeToolNames(s)...)
}
