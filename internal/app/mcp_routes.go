package app

import (
	"github.com/labstack/echo/v4"
	"github.com/lohi-ai/agentray/internal/dataplane/store"
	"github.com/lohi-ai/agentray/internal/dataplane/usecase"
	"github.com/lohi-ai/agentray/internal/runtime"
	"github.com/lohi-ai/agentray/internal/shared/opcore"
)

// registerMcpRoutes mounts the operation registry as an MCP server at POST /mcp,
// so an external agent (Claude Code, Codex, any MCP client) can call the same
// usecase handlers the in-house agent and the web client call. It is the fourth
// projection of one operation definition — there is no second schema or handler
// to keep in sync.
//
// Auth resolves a Principal per request: a scoped management credential
// (Authorization: Bearer agm_…), the legacy project key on unsplit projects
// (X-API-Key or ?api_key=), or a session cookie. Capture-only keys are denied.
// tools/list advertises only what the principal may call; tools/call
// re-authorizes by name.
//
// Connect a client with, e.g.:
//
//	claude mcp add --transport http --header "Authorization: Bearer <agm_…>" \
//	  agentray https://agentray.lohi2.com/mcp
func registerMcpRoutes(e *echo.Echo, store *storage.Store, notifier usecase.Notifier) {
	reg := usecase.Registry()
	deps := &usecase.Deps{
		Repo:     store,
		Memory:   agentruntime.NewPgMemory(store, false),
		Notifier: notifier,
	}
	group := e.Group("/mcp")
	opcore.MountMCP(group, reg, deps, func(c echo.Context) (opcore.Principal, error) {
		return principalFromRequest(c, store)
	})
}
