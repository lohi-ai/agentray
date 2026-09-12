// Package agentruntime is the first consumer of agentcore: the AgentRay Growth
// Analyst. It builds the agent's tools from the shared opcore/usecase operation
// registry (so the agent runs the same code paths the REST API and CLI do),
// injects a scope -> tool permission policy, and a per-project agent definition.
// It depends on agentcore, opcore, and usecase; agentcore depends on neither.
package agentruntime

import (
	"context"
	"time"

	"github.com/lohi-ai/agentray/internal/dataplane/store"
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
	CreateDashboard(ctx context.Context, projectID, name, description string) (storage.Dashboard, error)
	CreateChart(ctx context.Context, chart storage.Chart) (storage.Chart, error)

	// Recommendation write (P3, growth_suggest).
	CreateRecommendation(ctx context.Context, rec storage.AgentRecommendation) (string, error)
	CreateRecommendationIdempotent(ctx context.Context, rec storage.AgentRecommendation, idemKey, requestHash string) (string, error)

	// Validation test + waitlist (growth_suggest). The pre-product pair: the
	// agent proposes a threshold and reads the running test back against it.
	CreateValidationTest(ctx context.Context, t storage.ValidationTest) (string, error)
	ActiveValidationTest(ctx context.Context, projectID string) (*storage.ValidationTest, error)
	ValidationTestsForProject(ctx context.Context, projectID string, limit int) ([]storage.ValidationTest, int, error)
	ValidationTestForProject(ctx context.Context, projectID, id string) (storage.ValidationTest, error)
	ValidationTestProgress(ctx context.Context, t storage.ValidationTest) (storage.TestProgress, error)
	CountWaitlistSignups(ctx context.Context, projectID string) (int, error)

	// Notification channel resolution (send_notification, growth_suggest).
	WorkspaceIDForProject(ctx context.Context, projectID string) (string, error)
	WorkspaceChannelByName(ctx context.Context, workspaceID, name string) (storage.AlertChannel, error)

	// Product overview (monitor). Mirrors usecase.Repo — the shared `overview`
	// operation needs it on every adapter, including the in-process tool set.
	Overview(ctx context.Context, projectID, period, platform string, now time.Time) (storage.OverviewResult, error)
	// Lifecycle (slice 2): revision-checked dashboard writes, soft archive,
	// atomic idempotent writes (claim+mutation+receipt in one tx), identity
	// linkage, and the bounded event read verify_sdk uses.
	ListDashboardsFiltered(ctx context.Context, projectID string, includeArchived bool) ([]storage.Dashboard, error)
	UpdateDashboardRevision(ctx context.Context, projectID, dashboardID string, name, description *string, expectedRevision int64) (storage.Dashboard, error)
	ArchiveDashboard(ctx context.Context, projectID, dashboardID string, expectedRevision int64) (storage.Dashboard, error)
	UpdateDashboardIdempotent(ctx context.Context, projectID, dashboardID string, name, description *string, expectedRevision int64, idemKey, requestHash string) (storage.Dashboard, error)
	ArchiveDashboardIdempotent(ctx context.Context, projectID, dashboardID string, expectedRevision int64, idemKey, requestHash string) (storage.Dashboard, error)
	UnarchiveDashboardIdempotent(ctx context.Context, projectID, dashboardID string, expectedRevision int64, idemKey, requestHash string) (storage.Dashboard, error)
	DistinctIDLinked(ctx context.Context, projectID, distinctID string) (bool, error)
	RecentEventsForVerification(ctx context.Context, projectID string, limit int, since time.Time) ([]storage.Event, error)

	// Source lifecycle (slice 2): project-scoped connector probes and the
	// persistent run contract.
	ConnectorDSNForProject(ctx context.Context, projectID, connectorID string) (kind, dsn string, err error)
	ListConnectorSyncsForProject(ctx context.Context, projectID, connectorID string) ([]storage.ConnectorSync, error)
	ConnectorSyncForProject(ctx context.Context, projectID, syncID string) (storage.ConnectorSync, error)
	SetConnectorSyncEnabled(ctx context.Context, projectID, syncID string, enabled bool, expectedRevision int64) (storage.ConnectorSync, error)
	SetConnectorSyncEnabledIdempotent(ctx context.Context, projectID, syncID string, enabled bool, expectedRevision int64, idemKey, requestHash string) (storage.ConnectorSync, error)
	ConnectorRunForProject(ctx context.Context, projectID, runID string) (storage.ConnectorRun, error)
	LatestConnectorRun(ctx context.Context, projectID, syncID string) (storage.ConnectorRun, error)
	CancelConnectorRun(ctx context.Context, projectID, runID string) (storage.ConnectorRun, error)
	CreateDataConnectorIdempotent(ctx context.Context, projectID, name, kind, credentialID, idemKey, requestHash string) (storage.DataConnector, error)
	UpdateDataConnectorIdempotent(ctx context.Context, projectID, connectorID string, name *string, credentialID *string, expectedRevision int64, idemKey, requestHash string) (storage.DataConnector, error)

	// Plans (slice 4): revision-checked proposed-state edits, append-only
	// outcomes, proposed→abandoned, keyset-paginated lists, exact-ID finding
	// reads, and the dataset-semantics preview.
	UpdateValidationTestIdempotent(ctx context.Context, projectID, id string, in storage.ValidationTestUpdate, expectedRevision int64, idemKey, requestHash string) (storage.ValidationTest, error)
	AppendTestOutcomeIdempotent(ctx context.Context, projectID, id string, entry storage.TestOutcomeEntry, expectedRevision int64, idemKey, requestHash string) (storage.ValidationTest, error)
	AbandonValidationTestIdempotent(ctx context.Context, projectID, id, reason string, expectedRevision int64, idemKey, requestHash string) (storage.ValidationTest, error)
	ListValidationTestsPage(ctx context.Context, projectID, cursor string, limit int) ([]storage.ValidationTest, string, error)
	ListRecommendationsPage(ctx context.Context, projectID, cursor string, limit int) ([]storage.AgentRecommendation, string, error)
	RecommendationForProject(ctx context.Context, projectID, id string) (storage.AgentRecommendation, error)
	DatasetPreviewForProject(ctx context.Context, projectID, syncID string, limit int) (storage.DatasetPreview, error)

}

// Tool names — the stable identifiers the model calls and the policy permits.
// They must equal the opcore operation names registered in usecase.Registry so
// the scope -> tool allow-list (policy.go) lines up with the registry.
const (
	ToolActivitySummary    = "activity_summary"
	ToolRecentEvents       = "recent_events"
	ToolPersons            = "persons"
	ToolExploreEvents      = "explore_events"
	ToolRunSQL             = "run_sql"
	ToolRunInsight         = "run_insight"
	ToolRunFunnel          = "run_funnel"
	ToolRunRetention       = "run_retention"
	ToolListDashboards     = "list_dashboards"
	ToolCreateDashboard    = "create_dashboard"
	ToolCreateChart        = "create_chart"
	ToolSubmitRec          = "submit_recommendation"
	ToolProposeTest        = "propose_test"
	ToolTestStatus         = "test_status"
	ToolListTests          = "list_tests"
	ToolRemember           = "remember"
	ToolSendNotification   = "send_notification"
	ToolOverview           = "overview"
	ToolVerifySDK          = "verify_sdk"
	ToolUpdateDashboard    = "update_dashboard"
	ToolArchiveDashboard   = "archive_dashboard"
	ToolUnarchiveDashboard = "unarchive_dashboard"
	ToolTestSource         = "test_source"
	ToolPreviewSource      = "preview_source"
	ToolPauseSource        = "pause_source"
	ToolRunSource          = "run_source"
	ToolSourceStatus       = "source_status"
	ToolCancelSourceRun    = "cancel_source_run"
	ToolCreateSource       = "create_source"
	ToolUpdateSource       = "update_source"
	ToolUpdateTest         = "update_test"
	ToolRecordOutcome      = "record_outcome"
	ToolAbandonTest        = "abandon_test"
	ToolListFindings       = "list_findings"
	ToolDatasetPreview     = "dataset_preview"
)
