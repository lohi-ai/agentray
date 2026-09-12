// Package usecase is the agent's only door to data. Every analytics capability
// is an opcore.Operation whose handler reaches infra through the Repo interface —
// never a *storage.Store, a pgx pool, or a NATS connection. The concrete store is
// injected at the edge (HTTP mount / agent build); the handler, and therefore the
// agent, sees only the narrow Repo surface. This is the [API|Tool|CLI] -> usecase
// -> repo -> infra layering: usecase is the choke point that keeps the agent off
// the infrastructure.
package usecase

import (
	"context"
	"fmt"
	"time"

	"github.com/lohi-ai/agentray/agentcore"
	"github.com/lohi-ai/agentray/internal/dataplane/store"
	"github.com/lohi-ai/agentray/internal/shared/opcore"
)

// Repo is the data-access surface the usecase layer depends on. It is satisfied
// structurally by *storage.Store today, but declaring it here inverts the
// dependency: usecase (and the agent above it) imports the interface, not the
// implementation, so infra can never leak through. Storage structs are reused as
// the domain types — a type dependency, not an infrastructure one.
type Repo interface {
	ActivitySummary(ctx context.Context, projectID string, filter storage.EventFilter) (storage.ActivitySummary, error)
	RecentEvents(ctx context.Context, projectID string, limit int) ([]storage.Event, error)
	Persons(ctx context.Context, projectID string, filter storage.EventFilter) (storage.PersonsSummary, error)
	ExploreEvents(ctx context.Context, projectID string, filter storage.EventFilter) (storage.EventExplorer, error)
	RunSQL(ctx context.Context, projectID string, sqlText string) ([]map[string]any, error)
	RunInsight(ctx context.Context, projectID, insightType, metric string, steps []string, filter storage.EventFilter) (storage.InsightResult, error)
	ListDashboardsFiltered(ctx context.Context, projectID string, includeArchived bool) ([]storage.Dashboard, error)
	CreateDashboard(ctx context.Context, projectID, name, description string) (storage.Dashboard, error)
	CreateChart(ctx context.Context, chart storage.Chart) (storage.Chart, error)
	CreateRecommendation(ctx context.Context, rec storage.AgentRecommendation) (string, error)
	CreateRecommendationIdempotent(ctx context.Context, rec storage.AgentRecommendation, idemKey, requestHash string) (string, error)
	CreateValidationTest(ctx context.Context, t storage.ValidationTest) (string, error)
	CreateValidationTestIdempotent(ctx context.Context, t storage.ValidationTest, idemKey, requestHash string) (string, error)
	ActiveValidationTest(ctx context.Context, projectID string) (*storage.ValidationTest, error)
	// The plural reads. Without them an agent can only ever discuss the one test
	// ActiveValidationTest picks, while the owner is looking at a page of five —
	// the same LIMIT 1 the prototypes surface exists to undo.
	ValidationTestsForProject(ctx context.Context, projectID string, limit int) ([]storage.ValidationTest, int, error)
	ValidationTestForProject(ctx context.Context, projectID, id string) (storage.ValidationTest, error)
	ValidationTestProgress(ctx context.Context, t storage.ValidationTest) (storage.TestProgress, error)
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
	CountWaitlistSignups(ctx context.Context, projectID string) (int, error)
	WorkspaceIDForProject(ctx context.Context, projectID string) (string, error)
	WorkspaceChannelByName(ctx context.Context, workspaceID, name string) (storage.AlertChannel, error)
	// Overview is the deterministic product-overview read behind the shared
	// `overview` operation (REST /api/op/overview, MCP, and GET /api/overview).
	// now is injectable so tests can pin the complete-day boundary.
	Overview(ctx context.Context, projectID, period, platform string, now time.Time) (storage.OverviewResult, error)
	// Lifecycle operations (slice 2): revision-checked dashboard writes, soft
	// archive, atomic idempotent writes (claim+mutation+receipt in one tx),
	// identity linkage, and the bounded event read verify_sdk uses.
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
}

// Notifier delivers a message to a saved alert channel. It is the send_notification
// operation's escape to the platform's delivery fan-out (an agent can post to the
// same channels its alerts fire on). Satisfied by an alerting.Deliverer adapter at
// the edge; nil disables the tool (handler returns a clear error) so a build
// without delivery wiring degrades cleanly instead of panicking.
type Notifier interface {
	Notify(ctx context.Context, ch storage.AlertChannel, title, body string) error
}

// OperationAuditSink records successful remote mutations. It stays separate
// from Repo because runtime tools have no network principal to attribute.
type OperationAuditSink interface {
	RecordOperationAudit(ctx context.Context, projectID, actorID, credentialID, credentialKind, operation string) error
}

// Deps is the dependency bundle every operation handler receives via
// opcore.CallContext.Deps. It holds only the Repo interface and an optional agent
// MemoryStore — no pool, no queue — so a handler (and the agent that drives it)
// has no path to infra.
type Deps struct {
	Repo     Repo
	Memory   agentcore.MemoryStore
	Notifier Notifier
	Audit    OperationAuditSink
	// Runner is the connector engine's enqueue/cancel surface for
	// run_source/cancel_source_run. Nil in processes without an engine —
	// those operations report unavailable rather than silently queueing.
	Runner SourceRunner
}

// RecordOperationAudit records a successful network mutation without exposing
// storage to opcore. Audit failure is intentionally non-fatal: the mutation has
// already committed and existing workspace audit writes follow this same policy.
func (d *Deps) RecordOperationAudit(ctx context.Context, principal opcore.Principal, operation string) {
	if d == nil || d.Audit == nil {
		return
	}
	_ = d.Audit.RecordOperationAudit(ctx, principal.ProjectID, principal.UserID, principal.CredentialID, string(principal.Kind), operation)
}

// depsFrom recovers the typed Deps from an opcore.CallContext, failing loudly if
// the edge forgot to inject them.
func depsFrom(cc opcore.CallContext) (*Deps, error) {
	d, ok := cc.Deps.(*Deps)
	if !ok || d == nil {
		return nil, fmt.Errorf("usecase: operation invoked without deps")
	}
	return d, nil
}

// recentFilter returns the default look-back filter used when an operation takes
// a window in hours but no explicit from/to (matches the previous tool default).
func recentFilter(hours int) storage.EventFilter {
	if hours <= 0 || hours > 24*90 {
		hours = 24
	}
	now := time.Now().UTC()
	return storage.EventFilter{From: now.Add(-time.Duration(hours) * time.Hour), To: now, Limit: 200}
}
