package app

import (
	"net/http"

	"github.com/labstack/echo/v4"
	"github.com/lohi-ai/agentray/internal/storage"
)

// registerAlertRoutes mounts the Alerting (#1) CRUD surface: rules are
// project-scoped, channels workspace-scoped, and firing history read-only. All
// use authProject (session cookie), so access inherits the project/workspace
// checks the store enforces (member read, owner/admin write).
func registerAlertRoutes(e *echo.Echo, store *storage.Store) {
	// --- rules ---
	e.GET("/api/alerts/rules", func(c echo.Context) error {
		ctx, project, err := authProject(c, store)
		if err != nil {
			return err
		}
		rules, err := store.ListAlertRules(c.Request().Context(), ctx.User.ID, project.ID)
		if err != nil {
			return echo.NewHTTPError(http.StatusForbidden, err.Error())
		}
		return c.JSON(http.StatusOK, map[string]any{"rules": rules})
	})

	e.POST("/api/alerts/rules", func(c echo.Context) error {
		ctx, project, err := authProject(c, store)
		if err != nil {
			return err
		}
		var payload storage.AlertRule
		if err := c.Bind(&payload); err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, "invalid json")
		}
		rule, err := store.CreateAlertRule(c.Request().Context(), ctx.User.ID, project.ID, payload)
		if err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, err.Error())
		}
		return c.JSON(http.StatusCreated, map[string]any{"rule": rule})
	})

	e.PUT("/api/alerts/rules/:rule_id", func(c echo.Context) error {
		ctx, project, err := authProject(c, store)
		if err != nil {
			return err
		}
		var payload storage.AlertRule
		if err := c.Bind(&payload); err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, "invalid json")
		}
		if err := store.UpdateAlertRule(c.Request().Context(), ctx.User.ID, project.ID, c.Param("rule_id"), payload); err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, err.Error())
		}
		return c.NoContent(http.StatusNoContent)
	})

	e.DELETE("/api/alerts/rules/:rule_id", func(c echo.Context) error {
		ctx, project, err := authProject(c, store)
		if err != nil {
			return err
		}
		if err := store.DeleteAlertRule(c.Request().Context(), ctx.User.ID, project.ID, c.Param("rule_id")); err != nil {
			return echo.NewHTTPError(http.StatusForbidden, err.Error())
		}
		return c.NoContent(http.StatusNoContent)
	})

	e.GET("/api/alerts/rules/:rule_id/events", func(c echo.Context) error {
		ctx, project, err := authProject(c, store)
		if err != nil {
			return err
		}
		events, err := store.ListAlertEvents(c.Request().Context(), ctx.User.ID, project.ID, c.Param("rule_id"), intParam(c, "limit", 50, 1, 200))
		if err != nil {
			return echo.NewHTTPError(http.StatusForbidden, err.Error())
		}
		return c.JSON(http.StatusOK, map[string]any{"events": events})
	})

	// --- channels (workspace-scoped) ---
	e.GET("/api/alerts/channels", func(c echo.Context) error {
		ctx, project, err := authProject(c, store)
		if err != nil {
			return err
		}
		channels, err := store.ListAlertChannels(c.Request().Context(), ctx.User.ID, project.WorkspaceID)
		if err != nil {
			return echo.NewHTTPError(http.StatusForbidden, err.Error())
		}
		return c.JSON(http.StatusOK, map[string]any{"channels": channels})
	})

	e.POST("/api/alerts/channels", func(c echo.Context) error {
		ctx, project, err := authProject(c, store)
		if err != nil {
			return err
		}
		var payload storage.AlertChannel
		if err := c.Bind(&payload); err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, "invalid json")
		}
		channel, err := store.CreateAlertChannel(c.Request().Context(), ctx.User.ID, project.WorkspaceID, payload)
		if err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, err.Error())
		}
		return c.JSON(http.StatusCreated, map[string]any{"channel": channel})
	})

	e.DELETE("/api/alerts/channels/:channel_id", func(c echo.Context) error {
		ctx, project, err := authProject(c, store)
		if err != nil {
			return err
		}
		if err := store.DeleteAlertChannel(c.Request().Context(), ctx.User.ID, project.WorkspaceID, c.Param("channel_id")); err != nil {
			return echo.NewHTTPError(http.StatusForbidden, err.Error())
		}
		return c.NoContent(http.StatusNoContent)
	})
}
