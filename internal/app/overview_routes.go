package app

import (
	"net/http"

	"github.com/labstack/echo/v4"
	"github.com/lohi-ai/agentray/internal/dataplane/store"
)

// registerOverviewRoutes mounts GET /api/overview — a thin convenience adapter
// over the shared `overview` operation. It runs through the same opAdapter the
// web front door, POST /api/op and the MCP tools use: one registry, one deps
// bundle, one authorize check (the operation's own Access class), and one
// typed error mapping — so a not-found or conflict from the operation reaches
// this route as 404/409 instead of collapsing into 400. There is no second
// contract to drift.
func registerOverviewRoutes(e *echo.Echo, store *storage.Store, ops *opAdapter) {
	e.GET("/api/overview", func(c echo.Context) error {
		principal, err := principalFromRequest(c, store)
		if err != nil {
			return err
		}
		if err := ops.authorize(principal, "overview"); err != nil {
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
		out, err := ops.invoke(c, principal, "overview", input)
		if err != nil {
			return err
		}
		return c.Blob(http.StatusOK, echo.MIMEApplicationJSON, out)
	})
}
