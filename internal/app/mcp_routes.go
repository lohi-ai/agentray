package app

import (
	"github.com/labstack/echo/v4"
	"github.com/lohi-ai/agentray/internal/agentruntime"
	"github.com/lohi-ai/agentray/internal/opcore"
	"github.com/lohi-ai/agentray/internal/storage"
	"github.com/lohi-ai/agentray/internal/usecase"
)

// registerMcpRoutes mounts the operation registry as an MCP server at POST /mcp,
// so an external agent (Claude Code, Codex, any MCP client) can call the same
// usecase handlers the in-house agent and the web client call. It is the fourth
// projection of one operation definition — there is no second schema or handler
// to keep in sync. Auth reuses projectFromRequest, so an MCP client authenticates
// with the project's API key (X-API-Key header or ?api_key=), inheriting the
// exact access checks as the REST surface.
//
// Connect a client with, e.g.:
//
//	claude mcp add --transport http --header "X-API-Key: <project-key>" \
//	  agentray https://agentray.lohi2.com/mcp
func registerMcpRoutes(e *echo.Echo, store *storage.Store, notifier usecase.Notifier) {
	reg := usecase.Registry()
	deps := &usecase.Deps{
		Repo:     store,
		Memory:   agentruntime.NewPgMemory(store, false),
		Notifier: notifier,
	}
	group := e.Group("/mcp")
	opcore.MountMCP(group, reg, deps, func(c echo.Context) (string, error) {
		project, err := projectFromRequest(c, store)
		if err != nil {
			return "", err
		}
		return project.ID, nil
	})
}
