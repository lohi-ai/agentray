package app

import (
	"net/http"

	"github.com/labstack/echo/v4"
	"github.com/lohi-ai/agentray/internal/storage"
)

// registerTeamRoutes mounts the agent-team surface: team CRUD, roster
// membership, lead selection, and the kanban board. Reads and card writes are
// member-level; structural writes (team create/update/delete, membership,
// lead pick) are owner/admin, enforced in the store — the lead choice decides
// which agent receives the orchestrator skill and team_board tool at run time.
func registerTeamRoutes(e *echo.Echo, store *storage.Store) {
	// --- teams ---
	e.GET("/api/teams", func(c echo.Context) error {
		ctx, project, err := authProject(c, store)
		if err != nil {
			return err
		}
		teams, err := store.ListTeams(c.Request().Context(), ctx.User.ID, project.ID)
		if err != nil {
			return echo.NewHTTPError(http.StatusForbidden, err.Error())
		}
		return c.JSON(http.StatusOK, map[string]any{"teams": teams, "statuses": storage.TeamCardStatuses()})
	})

	e.POST("/api/teams", func(c echo.Context) error {
		ctx, project, err := authProject(c, store)
		if err != nil {
			return err
		}
		var payload struct {
			Name string `json:"name"`
		}
		if err := c.Bind(&payload); err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, "invalid json")
		}
		created, err := store.CreateTeam(c.Request().Context(), ctx.User.ID, project.ID, payload.Name)
		if err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, err.Error())
		}
		return c.JSON(http.StatusCreated, map[string]any{"team": created})
	})

	e.GET("/api/teams/:team_id", func(c echo.Context) error {
		ctx, project, err := authProject(c, store)
		if err != nil {
			return err
		}
		team, members, err := store.GetTeam(c.Request().Context(), ctx.User.ID, project.ID, c.Param("team_id"))
		if err != nil {
			return echo.NewHTTPError(http.StatusNotFound, err.Error())
		}
		return c.JSON(http.StatusOK, map[string]any{"team": team, "members": members})
	})

	e.PUT("/api/teams/:team_id", func(c echo.Context) error {
		ctx, project, err := authProject(c, store)
		if err != nil {
			return err
		}
		var payload struct {
			Name        string `json:"name"`
			LeadAgentID string `json:"lead_agent_id"`
		}
		if err := c.Bind(&payload); err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, "invalid json")
		}
		updated, err := store.UpdateTeam(c.Request().Context(), ctx.User.ID, project.ID, c.Param("team_id"), payload.Name, payload.LeadAgentID)
		if err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, err.Error())
		}
		return c.JSON(http.StatusOK, map[string]any{"team": updated})
	})

	e.DELETE("/api/teams/:team_id", func(c echo.Context) error {
		ctx, project, err := authProject(c, store)
		if err != nil {
			return err
		}
		if err := store.DeleteTeam(c.Request().Context(), ctx.User.ID, project.ID, c.Param("team_id")); err != nil {
			return echo.NewHTTPError(http.StatusForbidden, err.Error())
		}
		return c.NoContent(http.StatusNoContent)
	})

	// --- roster ---
	e.PUT("/api/teams/:team_id/members/:agent_id", func(c echo.Context) error {
		ctx, project, err := authProject(c, store)
		if err != nil {
			return err
		}
		var payload struct {
			Role     string `json:"role"`
			Position int    `json:"position"`
		}
		if err := c.Bind(&payload); err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, "invalid json")
		}
		if err := store.UpsertTeamMember(c.Request().Context(), ctx.User.ID, project.ID, c.Param("team_id"), c.Param("agent_id"), payload.Role, payload.Position); err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, err.Error())
		}
		return c.NoContent(http.StatusNoContent)
	})

	e.DELETE("/api/teams/:team_id/members/:agent_id", func(c echo.Context) error {
		ctx, project, err := authProject(c, store)
		if err != nil {
			return err
		}
		if err := store.RemoveTeamMember(c.Request().Context(), ctx.User.ID, project.ID, c.Param("team_id"), c.Param("agent_id")); err != nil {
			return echo.NewHTTPError(http.StatusForbidden, err.Error())
		}
		return c.NoContent(http.StatusNoContent)
	})

	// --- kanban cards ---
	e.GET("/api/teams/:team_id/cards", func(c echo.Context) error {
		ctx, project, err := authProject(c, store)
		if err != nil {
			return err
		}
		cards, err := store.ListTeamCards(c.Request().Context(), ctx.User.ID, project.ID, c.Param("team_id"))
		if err != nil {
			return echo.NewHTTPError(http.StatusNotFound, err.Error())
		}
		return c.JSON(http.StatusOK, map[string]any{"cards": cards, "statuses": storage.TeamCardStatuses()})
	})

	e.POST("/api/teams/:team_id/cards", func(c echo.Context) error {
		ctx, project, err := authProject(c, store)
		if err != nil {
			return err
		}
		var payload struct {
			Title string `json:"title"`
			Body  string `json:"body"`
		}
		if err := c.Bind(&payload); err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, "invalid json")
		}
		created, err := store.CreateTeamCard(c.Request().Context(), ctx.User.ID, project.ID, c.Param("team_id"), payload.Title, payload.Body)
		if err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, err.Error())
		}
		return c.JSON(http.StatusCreated, map[string]any{"card": created})
	})

	e.PUT("/api/teams/:team_id/cards/:card_id", func(c echo.Context) error {
		ctx, project, err := authProject(c, store)
		if err != nil {
			return err
		}
		var payload storage.TeamCardUpdate
		if err := c.Bind(&payload); err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, "invalid json")
		}
		updated, err := store.UpdateTeamCard(c.Request().Context(), ctx.User.ID, project.ID, c.Param("team_id"), c.Param("card_id"), payload)
		if err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, err.Error())
		}
		return c.JSON(http.StatusOK, map[string]any{"card": updated})
	})

	e.DELETE("/api/teams/:team_id/cards/:card_id", func(c echo.Context) error {
		ctx, project, err := authProject(c, store)
		if err != nil {
			return err
		}
		if err := store.DeleteTeamCard(c.Request().Context(), ctx.User.ID, project.ID, c.Param("team_id"), c.Param("card_id")); err != nil {
			return echo.NewHTTPError(http.StatusForbidden, err.Error())
		}
		return c.NoContent(http.StatusNoContent)
	})
}
