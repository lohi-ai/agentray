package app

import (
	"encoding/json"
	"net/http"

	"github.com/labstack/echo/v4"
	"github.com/lohi-ai/agentray/internal/dataplane/store"
	"github.com/lohi-ai/agentray/internal/dataplane/usecase"
	"github.com/lohi-ai/agentray/internal/runtime"
	"github.com/lohi-ai/agentray/internal/shared/opcore"
)

// registerOverviewRoutes mounts GET /api/overview — a thin convenience adapter
// over the shared `overview` operation. The handler invokes the registered op
// verbatim, so the web front door, POST /api/op/overview, and MCP tools/call
// all run the identical usecase code; there is no second contract to drift.
//
// Auth: projectFromRequest is the same resolver MountHTTP uses today. When the
// credential split lands (opcore.PrincipalResolver), this adapter must switch
// to that resolver so a capture-only key is denied here exactly as it is on
// /api/op — never a separate, weaker check.
func registerOverviewRoutes(e *echo.Echo, store *storage.Store, notifier usecase.Notifier) {
	overviewDeps := &usecase.Deps{
		Repo:     store,
		Memory:   agentruntime.NewPgMemory(store, false),
		Notifier: notifier,
	}
	reg := usecase.Registry()
	spec, ok := reg.Get("overview")
	if !ok {
		panic("app: overview operation not registered")
	}
	e.GET("/api/overview", func(c echo.Context) error {
		project, err := projectFromRequest(c, store)
		if err != nil {
			return err
		}
		// GET carries the input as query params; the op's own decoder validates
		// the result, so the adapter adds no second validation layer.
		input := map[string]string{}
		if p := c.QueryParam("period"); p != "" {
			input["period"] = p
		}
		if p := c.QueryParam("platform"); p != "" {
			input["platform"] = p
		}
		body := "{}"
		if len(input) > 0 {
			if b, err := json.Marshal(input); err == nil {
				body = string(b)
			}
		}
		out, err := spec.OpInvoke(c.Request().Context(), opcore.CallContext{ProjectID: project.ID, Deps: overviewDeps}, body)
		if err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, err.Error())
		}
		return c.Blob(http.StatusOK, echo.MIMEApplicationJSON, []byte(out))
	})
}
