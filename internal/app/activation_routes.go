package app

import (
	"net/http"

	"github.com/labstack/echo/v4"
	storage "github.com/lohi-ai/agentray/internal/dataplane/store"
)

// registerActivationRoutes mounts GET /api/projects/:project_id/activation-candidates
// — a thin convenience adapter over the shared `activation_candidates`
// operation, same shape as registerOverviewRoutes: one registry, one authorize
// check (the operation's Access class), one typed error mapping.
//
// The path's project_id must be the principal's project. A session resolves
// the path id through membership already; a management credential names its
// own project, and serving a different one would read across the grant — a
// mismatch is a not-found, not a silent cross-project read.
func registerActivationRoutes(e *echo.Echo, store *storage.Store, ops *opAdapter) {
	e.GET("/api/projects/:project_id/activation-candidates", func(c echo.Context) error {
		principal, err := principalFromRequest(c, store)
		if err != nil {
			return err
		}
		if c.Param("project_id") != principal.ProjectID {
			return echo.NewHTTPError(http.StatusNotFound, "project not found")
		}
		if err := ops.authorize(principal, "activation_candidates"); err != nil {
			return err
		}
		out, err := ops.invoke(c, principal, "activation_candidates", map[string]any{})
		if err != nil {
			return err
		}
		return c.Blob(http.StatusOK, echo.MIMEApplicationJSON, out)
	})
}
