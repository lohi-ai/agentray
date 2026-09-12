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
// over the shared `overview` operation. The handler resolves the caller through
// principalFromRequest and authorizes against the operation's Access class via
// reg.Authorize — the exact check MountHTTP applies on /api/op — so a
// capture-only key is denied here exactly as it is there, and the web front
// door, POST /api/op/overview, and MCP tools/call all run the identical
// usecase code. There is no second contract to drift.
func registerOverviewRoutes(e *echo.Echo, store *storage.Store, notifier usecase.Notifier) {
	reg := usecase.Registry()
	spec, ok := reg.Get("overview")
	if !ok {
		panic("app: overview operation not registered")
	}
	deps := &usecase.Deps{
		Repo:     store,
		Memory:   agentruntime.NewPgMemory(store, false),
		Notifier: notifier,
	}
	e.GET("/api/overview", func(c echo.Context) error {
		principal, err := principalFromRequest(c, store)
		if err != nil {
			return err
		}
		if !reg.Authorize(principal, spec.OpName()) {
			return echo.NewHTTPError(http.StatusForbidden, "credential may not invoke "+spec.OpName())
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
		cc := opcore.CallContext{ProjectID: principal.ProjectID, Deps: deps, Principal: principal}
		out, err := spec.OpInvoke(c.Request().Context(), cc, body)
		if err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, err.Error())
		}
		return c.Blob(http.StatusOK, echo.MIMEApplicationJSON, []byte(out))
	})
}
