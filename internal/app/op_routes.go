package app

import (
	"github.com/labstack/echo/v4"
	"github.com/lohi-ai/agentray/internal/dataplane/store"
	"github.com/lohi-ai/agentray/internal/dataplane/usecase"
	"github.com/lohi-ai/agentray/internal/runtime"
	"github.com/lohi-ai/agentray/internal/shared/opcore"
)

// registerOpRoutes mounts the shared operation registry under /api/op/<name>.
// These endpoints run the exact same usecase handlers the agent's in-process
// tools and the client CLI run — one definition, three adapters. They are
// additive: the existing analytics routes remain the web client's contract and
// migrate onto /api/op incrementally. Auth resolves a Principal (management
// credential, legacy/capture project key, or session) and every call is
// authorized against the operation's access class before the handler runs.
func registerOpRoutes(e *echo.Echo, store *storage.Store, notifier usecase.Notifier, runner usecase.SourceRunner) {
	reg := usecase.Registry()
	deps := &usecase.Deps{
		Repo:     store,
		Memory:   agentruntime.NewPgMemory(store, false),
		Notifier: notifier,
		Runner:   runner,
	}
	group := e.Group("/api/op")
	opcore.MountHTTP(group, reg, deps, func(c echo.Context) (opcore.Principal, error) {
		return principalFromRequest(c, store)
	})
}
