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
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/labstack/echo/v4"
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
	// Chart lifecycle: per-chart revision fences update/archive; the
	// dashboard revision is the single fence for an atomic board reorder.
	ListChartsFiltered(ctx context.Context, projectID, dashboardID string, includeArchived bool) ([]storage.Chart, error)
	UpdateChartIdempotent(ctx context.Context, chart storage.Chart, expectedRevision int64, idemKey, requestHash string) (storage.Chart, error)
	ArchiveChartIdempotent(ctx context.Context, projectID, chartID string, expectedRevision int64, idemKey, requestHash string) (storage.Chart, error)
	UnarchiveChartIdempotent(ctx context.Context, projectID, chartID string, expectedRevision int64, idemKey, requestHash string) (storage.Chart, error)
	ReorderChartsIdempotent(ctx context.Context, projectID, dashboardID string, chartIDs []string, expectedRevision int64, idemKey, requestHash string) (storage.Dashboard, error)
	// The metric catalog and the declarative board content model. The catalog
	// is the vocabulary a board tile is validated against; the board reads and
	// the fenced declaration are the content surface behind list_metrics,
	// read_metric, get_board and save_board.
	ListMetricDefinitions(ctx context.Context) ([]storage.MetricDefinition, error)
	MetricDefinitionByKey(ctx context.Context, key string) (storage.MetricDefinition, error)
	BoardContentForProject(ctx context.Context, projectID, boardID string) (storage.BoardContent, error)
	BoardContentByKey(ctx context.Context, projectID, boardKey string) (storage.BoardContent, error)
	SaveBoardDefinition(ctx context.Context, projectID string, in storage.BoardDefinitionWrite, idemKey, requestHash string) (storage.BoardContent, error)
	DistinctIDLinked(ctx context.Context, projectID, distinctID string) (bool, error)
	RecentEventsForVerification(ctx context.Context, projectID string, limit int, since time.Time) ([]storage.Event, error)

	// Source lifecycle (slice 2): project-scoped connector probes and the
	// persistent run contract.
	ConnectorDSNForProject(ctx context.Context, projectID, connectorID string) (kind, dsn string, err error)
	// DataConnectorForProject is the project-scoped connector existence read.
	// source_status needs it to tell "a connector that has never synced" (a
	// real empty answer) from "no such connector" (not-found) — listing a
	// missing connector's syncs returns an empty slice for both.
	DataConnectorForProject(ctx context.Context, projectID, connectorID string) (storage.DataConnector, error)
	ListConnectorSyncsForProject(ctx context.Context, projectID, connectorID string) ([]storage.ConnectorSync, error)
	ConnectorSyncForProject(ctx context.Context, projectID, syncID string) (storage.ConnectorSync, error)
	SetConnectorSyncEnabled(ctx context.Context, projectID, syncID string, enabled bool, expectedRevision int64) (storage.ConnectorSync, error)
	SetConnectorSyncEnabledIdempotent(ctx context.Context, projectID, syncID string, enabled bool, expectedRevision int64, idemKey, requestHash string) (storage.ConnectorSync, error)
	ConnectorRunForProject(ctx context.Context, projectID, runID string) (storage.ConnectorRun, error)
	LatestConnectorRunsForProject(ctx context.Context, projectID string, syncIDs []string) (map[string]storage.ConnectorRun, error)
	CancelConnectorRun(ctx context.Context, projectID, runID string) (storage.ConnectorRun, error)
	CreateDataConnectorIdempotent(ctx context.Context, projectID, name, kind, credentialID, idemKey, requestHash string) (storage.DataConnector, error)
	UpdateDataConnectorIdempotent(ctx context.Context, projectID, connectorID string, name *string, credentialID *string, expectedRevision int64, idemKey, requestHash string) (storage.DataConnector, error)
	// Source archive is reversible: it keeps the connector row and its
	// credential reference and disables its syncs transactionally; unarchive
	// resumes exactly the syncs the archive paused.
	ListDataConnectorsFiltered(ctx context.Context, projectID string, includeArchived bool) ([]storage.DataConnector, error)
	ArchiveDataConnectorIdempotent(ctx context.Context, projectID, connectorID string, expectedRevision int64, idemKey, requestHash string) (storage.DataConnector, error)
	UnarchiveDataConnectorIdempotent(ctx context.Context, projectID, connectorID string, expectedRevision int64, idemKey, requestHash string) (storage.DataConnector, error)

	// Chart annotations: the project-scoped marks a member drops on a trend.
	// The window read is the overlap contract every temporal chart asks for;
	// the by-id read resolves evidence references without ever crossing a
	// project boundary.
	CreateAnnotationIdempotent(ctx context.Context, projectID string, in storage.AnnotationWrite, idemKey, requestHash string) (storage.Annotation, error)
	AnnotationsForWindow(ctx context.Context, projectID string, from, to time.Time, limit int) ([]storage.Annotation, error)
	AnnotationForProject(ctx context.Context, projectID, id string) (storage.Annotation, error)
	DeleteAnnotationIdempotent(ctx context.Context, projectID, id, idemKey, requestHash string) (storage.Annotation, error)
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

// MapOpError translates a handler's typed error into the HTTP status the
// operation contract promises: revision and idempotency conflicts are 409,
// missing rows 404, an archived source 409, a saturated engine 503
// (retryable). Everything else stays a 400. Installed on the registry so
// /api/op answers identically to the legacy adapter's opError — a web client
// classifying by status sees the typed outcome on every adapter.
func MapOpError(err error) error {
	var he *echo.HTTPError
	if errors.As(err, &he) {
		return he
	}
	// classifyOpError owns the sentinel→kind mapping; explicit OpErrors always
	// win (the same rule Registry.classifyError applies), so only untyped
	// errors are classified. Here we only render the kind as an HTTP status.
	if opcore.KindOf(err) == "" {
		err = classifyOpError(err)
	}
	var oe *opcore.OpError
	if errors.As(err, &oe) {
		switch oe.Kind {
		case opcore.ErrNotFound:
			return echo.NewHTTPError(http.StatusNotFound, oe.Message)
		case opcore.ErrConflict:
			return echo.NewHTTPError(http.StatusConflict, oe.Message)
		case opcore.ErrRetryable:
			return echo.NewHTTPError(http.StatusServiceUnavailable, oe.Message)
		}
	}
	return echo.NewHTTPError(http.StatusBadRequest, err.Error())
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
