package app

import (
	"net/http"

	"github.com/labstack/echo/v4"
	"github.com/lohi-ai/agentray/internal/storage"
)

// registerAgentMonitorRoutes mounts the read-only observability surface for the
// monitoring console. It is deliberately separate from registerAgentRoutes (the
// agent CRUD/config surface) so the monitoring read model can evolve without
// touching the write paths. Every endpoint is project-scoped via authProject and
// agent-agnostic — it keys on agents.id, so a newly added agent appears here with
// no per-agent code.
func registerAgentMonitorRoutes(e *echo.Echo, store *storage.Store) {
	// Project-wide rollup: one row per agent with run counts, token/cost totals,
	// and last activity. Powers /agents/monitor.
	handleOverview := func(c echo.Context) error {
		ctx, project, err := authProject(c, store)
		if err != nil {
			return err
		}
		rows, err := store.ListAgentMonitor(c.Request().Context(), ctx.User.ID, project.ID)
		if err != nil {
			return err
		}
		return c.JSON(http.StatusOK, map[string]any{"agents": rows})
	}
	e.GET("/api/agents/monitor", handleOverview)

	// One agent's detail: its rollup plus its recent runs. Powers
	// /agents/<agentId>/monitor. The per-run loop trace (tool_calls + llm_calls)
	// is fetched separately via /api/agent/runs/:run_id on drill-in.
	handleDetail := func(c echo.Context) error {
		ctx, project, err := authProject(c, store)
		if err != nil {
			return err
		}
		agentID := c.Param("id")
		agent, err := store.GetAgentMonitor(c.Request().Context(), ctx.User.ID, project.ID, agentID)
		if err != nil {
			return err
		}
		runs, err := store.ListAgentRunsForAgent(c.Request().Context(), ctx.User.ID, project.ID, agentID, intParam(c, "limit", 50, 1, 200))
		if err != nil {
			return err
		}
		return c.JSON(http.StatusOK, map[string]any{"agent": agent, "runs": runs})
	}
	e.GET("/api/agents/:id/monitor", handleDetail)
}
