package agentruntime

// progressPhrases maps each analytics tool to a plain-language progress note — a
// human phrase with no operation identifier (D4). Unknown tools fall back to a
// generic "Working on it…" so a new tool never leaks its raw name to the user.
var progressPhrases = map[string]string{
	"recent_events":         "Looking at recent activity…",
	"explore_events":        "Looking at recent activity…",
	"list_persons":          "Checking who's been active…",
	"run_sql":               "Crunching the numbers…",
	"run_insight":           "Crunching the numbers…",
	"web_analytics":         "Checking your traffic…",
	"list_dashboards":       "Checking your dashboards…",
	"create_dashboard":      "Setting up a dashboard…",
	"create_chart":          "Putting together a chart…",
	"submit_recommendation": "Writing up a recommendation…",
	"remember":              "Noting that for next time…",
}

// progressNote returns the friendly phrase for a tool, or a generic fallback.
func progressNote(tool string) string {
	if p, ok := progressPhrases[tool]; ok {
		return p
	}
	return "Working on it…"
}
