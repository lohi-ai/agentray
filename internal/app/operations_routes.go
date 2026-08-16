package app

import (
	"net/http"
	"strings"

	"github.com/labstack/echo/v4"
	"github.com/lohi-ai/agentray/internal/dataplane/store"
	"github.com/lohi-ai/agentray/internal/runtime"
)

// operations_routes.go — the project-wide view of unattended work.
//
// It adds no run engine and no second way to configure a trigger. Reading is a
// projection over agent_triggers × agents/teams × agent_runs (store/operators.go);
// editing still goes through the per-agent trigger routes, which already own the
// permission checks and the audit trail; and "run now" publishes onto the same
// NATS subject the scheduler and the webhook ingress use. What is new here is
// only that the owner can see all of it in one place, which is the thing the
// per-agent setup tab structurally could not do.
func registerOperationsRoutes(e *echo.Echo, store *storage.Store, scheduler *agentruntime.Scheduler) {
	e.GET("/api/operations", func(c echo.Context) error {
		ctx, project, err := authProject(c, store)
		if err != nil {
			return err
		}
		operators, err := store.ListOperators(c.Request().Context(), ctx.User.ID, project.ID)
		if err != nil {
			return err
		}
		return c.JSON(http.StatusOK, map[string]any{"operators": operators})
	})

	e.GET("/api/operations/:id", func(c echo.Context) error {
		ctx, project, err := authProject(c, store)
		if err != nil {
			return err
		}
		op, err := store.OperatorByID(c.Request().Context(), ctx.User.ID, project.ID, c.Param("id"))
		if err != nil {
			if storage.ErrNoSuchOperator(err) {
				return echo.NewHTTPError(http.StatusNotFound, "no operator with that id")
			}
			return err
		}
		runs, err := store.OperatorRuns(c.Request().Context(), ctx.User.ID, project.ID, c.Param("id"), intParam(c, "limit", 25, 1, 200))
		if err != nil {
			return err
		}
		return c.JSON(http.StatusOK, map[string]any{"operator": op, "runs": runs})
	})

	// Run now. The manual escape hatch an owner needs the first time they wire a
	// schedule: waiting until 07:00 tomorrow to find out the prompt was wrong is
	// how a broken operator stays broken for a day.
	//
	// It runs the operator's own prompt, not a generic one — a "run now" that
	// executes something different from what the schedule executes is a test of
	// nothing.
	e.POST("/api/operations/:id/run", func(c echo.Context) error {
		ctx, project, err := authProject(c, store)
		if err != nil {
			return err
		}
		canManage, err := store.UserCanManageWorkspace(c.Request().Context(), ctx.User.ID, project.WorkspaceID)
		if err != nil {
			return err
		}
		if !canManage {
			return echo.NewHTTPError(http.StatusForbidden, "only a workspace owner or admin can start a run")
		}
		op, err := store.OperatorByID(c.Request().Context(), ctx.User.ID, project.ID, c.Param("id"))
		if err != nil {
			if storage.ErrNoSuchOperator(err) {
				return echo.NewHTTPError(http.StatusNotFound, "no operator with that id")
			}
			return err
		}
		// A paused AGENT is a harder stop than a paused trigger: the owner turned
		// the teammate off, and honouring "run now" would work around that.
		if !op.AgentEnabled {
			return echo.NewHTTPError(http.StatusConflict, "that teammate is paused — turn it back on before running it")
		}
		if scheduler == nil {
			return echo.NewHTTPError(http.StatusServiceUnavailable, "the run queue is unavailable")
		}
		prompt := strings.TrimSpace(op.PromptTemplate)
		if prompt == "" {
			prompt = agentruntime.MonitorPrompt
		}
		// A webhook operator's template is written around {{body}}; there is no
		// body on a hand-started run, so say so rather than handing the agent a
		// literal placeholder to reason about.
		prompt = strings.ReplaceAll(prompt, "{{body}}",
			"(no payload — this run was started by hand from Operations, not by the webhook)")
		// The run keeps the operator's own channel, not "manual". Two reasons, and
		// both matter: the autonomy rail gates on it, so a rehearsal labelled
		// manual would run with tools the real 07:00 run never gets; and the run
		// history on this page is scoped by channel, so a manual label would make
		// the run the owner just started invisible on the screen they started it
		// from.
		if op.Kind == storage.TriggerWebhook {
			err = scheduler.PublishWebhook(project.ID, op.AgentID, prompt)
		} else {
			err = scheduler.PublishScheduled(project.ID, op.AgentID, prompt)
		}
		if err != nil {
			return echo.NewHTTPError(http.StatusBadGateway, err.Error())
		}
		return c.JSON(http.StatusAccepted, map[string]any{"queued": true})
	})
}
