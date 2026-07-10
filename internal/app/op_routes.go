package app

import (
	"github.com/labstack/echo/v4"
	"github.com/lohi-ai/agentray/internal/agentruntime"
	"github.com/lohi-ai/agentray/internal/opcore"
	"github.com/lohi-ai/agentray/internal/storage"
	"github.com/lohi-ai/agentray/internal/usecase"
)

// registerOpRoutes mounts the shared operation registry under /api/op/<name>.
// These endpoints run the exact same usecase handlers the agent's in-process
// tools and the client CLI run — one definition, three adapters. They are
// additive: the existing analytics routes remain the web client's contract and
// migrate onto /api/op incrementally. Auth reuses projectFromRequest (session
// cookie or X-API-Key), so the operation surface inherits the same access checks
// as the rest of the API.
func registerOpRoutes(e *echo.Echo, store *storage.Store, notifier usecase.Notifier) {
	reg := usecase.Registry()
	deps := &usecase.Deps{
		Repo:     store,
		Memory:   agentruntime.NewPgMemory(store, false),
		Notifier: notifier,
	}
	group := e.Group("/api/op")
	opcore.MountHTTP(group, reg, deps, func(c echo.Context) (string, error) {
		project, err := projectFromRequest(c, store)
		if err != nil {
			return "", err
		}
		return project.ID, nil
	})
}
