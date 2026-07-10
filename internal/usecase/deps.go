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
	"github.com/lohi-ai/agentray/internal/opcore"
	"github.com/lohi-ai/agentray/internal/storage"
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
	ListDashboards(ctx context.Context, projectID string) ([]storage.Dashboard, error)
	CreateDashboard(ctx context.Context, projectID, name, description string) (storage.Dashboard, error)
	CreateChart(ctx context.Context, chart storage.Chart) (storage.Chart, error)
	CreateRecommendation(ctx context.Context, rec storage.AgentRecommendation) (string, error)
	WorkspaceIDForProject(ctx context.Context, projectID string) (string, error)
	WorkspaceChannelByName(ctx context.Context, workspaceID, name string) (storage.AlertChannel, error)
}

// Notifier delivers a message to a saved alert channel. It is the send_notification
// operation's escape to the platform's delivery fan-out (an agent can post to the
// same channels its alerts fire on). Satisfied by an alerting.Deliverer adapter at
// the edge; nil disables the tool (handler returns a clear error) so a build
// without delivery wiring degrades cleanly instead of panicking.
type Notifier interface {
	Notify(ctx context.Context, ch storage.AlertChannel, title, body string) error
}

// Deps is the dependency bundle every operation handler receives via
// opcore.CallContext.Deps. It holds only the Repo interface and an optional agent
// MemoryStore — no pool, no queue — so a handler (and the agent that drives it)
// has no path to infra.
type Deps struct {
	Repo     Repo
	Memory   agentcore.MemoryStore
	Notifier Notifier
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
