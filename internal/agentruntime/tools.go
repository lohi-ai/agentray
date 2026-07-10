// Package agentruntime is the first consumer of agentcore: the AgentRay Growth
// Analyst. It builds the agent's tools from the shared opcore/usecase operation
// registry (so the agent runs the same code paths the REST API and CLI do),
// injects a scope -> tool permission policy, and a per-project agent definition.
// It depends on agentcore, opcore, and usecase; agentcore depends on neither.
package agentruntime

import (
	"context"

	"github.com/lohi-ai/agentray/internal/storage"
)

// DataSource is the narrow slice of *storage.Store the analytics operations use.
// It is structurally identical to usecase.Repo: BuildParams.Data is typed as
// DataSource here so the runner can pass *storage.Store, and Build hands it to
// the usecase layer (which depends only on the interface, never the concrete
// store). This is the seam that keeps the agent off the infra layer.
type DataSource interface {
	// Read (P0).
	ActivitySummary(ctx context.Context, projectID string, filter storage.EventFilter) (storage.ActivitySummary, error)
	RecentEvents(ctx context.Context, projectID string, limit int) ([]storage.Event, error)
	Persons(ctx context.Context, projectID string, filter storage.EventFilter) (storage.PersonsSummary, error)
	ExploreEvents(ctx context.Context, projectID string, filter storage.EventFilter) (storage.EventExplorer, error)
	RunSQL(ctx context.Context, projectID string, sqlText string) ([]map[string]any, error)

	// Insight + authoring (P1, analyze_build).
	RunInsight(ctx context.Context, projectID, insightType, metric string, steps []string, filter storage.EventFilter) (storage.InsightResult, error)
	ListDashboards(ctx context.Context, projectID string) ([]storage.Dashboard, error)
	CreateDashboard(ctx context.Context, projectID, name, description string) (storage.Dashboard, error)
	CreateChart(ctx context.Context, chart storage.Chart) (storage.Chart, error)

	// Recommendation write (P3, growth_suggest).
	CreateRecommendation(ctx context.Context, rec storage.AgentRecommendation) (string, error)

	// Notification channel resolution (send_notification, growth_suggest).
	WorkspaceIDForProject(ctx context.Context, projectID string) (string, error)
	WorkspaceChannelByName(ctx context.Context, workspaceID, name string) (storage.AlertChannel, error)
}

// Tool names — the stable identifiers the model calls and the policy permits.
// They must equal the opcore operation names registered in usecase.Registry so
// the scope -> tool allow-list (policy.go) lines up with the registry.
const (
	ToolActivitySummary = "activity_summary"
	ToolRecentEvents    = "recent_events"
	ToolPersons         = "persons"
	ToolExploreEvents   = "explore_events"
	ToolRunSQL          = "run_sql"
	ToolRunInsight      = "run_insight"
	ToolListDashboards  = "list_dashboards"
	ToolCreateDashboard = "create_dashboard"
	ToolCreateChart     = "create_chart"
	ToolSubmitRec       = "submit_recommendation"
	ToolRemember        = "remember"
	ToolSendNotification = "send_notification"
)
