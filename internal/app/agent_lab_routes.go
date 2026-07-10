package app

import (
	"net/http"

	"github.com/labstack/echo/v4"
	"github.com/lohi-ai/agentray/internal/agentruntime"
	"github.com/lohi-ai/agentray/internal/storage"
)

// registerAgentLabRoutes mounts the AgentCore Lab surface: saved test cases,
// test-mode runs (auto-run → pass/fail vs expected), explain-mode runs (SSE,
// paused before each step) with advance/stop controls, and step replay for any
// completed run. The LabService is constructed ONCE here so its in-memory
// explain-session registry survives across the separate explain / advance / stop
// requests of one stepped run.
func registerAgentLabRoutes(e *echo.Echo, store *storage.Store, sandboxReady bool, runnerOpts ...agentruntime.RunnerOption) {
	lab := agentruntime.NewLabService(store, sandboxReady, runnerOpts...)

	// --- saved test cases (AC5) ---
	e.GET("/api/agent/lab/cases", func(c echo.Context) error {
		ctx, project, err := authProject(c, store)
		if err != nil {
			return err
		}
		cases, err := store.ListAgentLabCases(c.Request().Context(), ctx.User.ID, project.ID, c.QueryParam("agent"))
		if err != nil {
			return err
		}
		return c.JSON(http.StatusOK, map[string]any{"cases": cases})
	})

	e.POST("/api/agent/lab/cases", func(c echo.Context) error {
		ctx, project, err := authProject(c, store)
		if err != nil {
			return err
		}
		var payload struct {
			Name     string `json:"name"`
			Input    string `json:"input"`
			Expected string `json:"expected"`
		}
		if err := c.Bind(&payload); err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, "invalid json")
		}
		saved, err := store.SaveAgentLabCase(c.Request().Context(), ctx.User.ID, project.ID, c.QueryParam("agent"), payload.Name, payload.Input, payload.Expected)
		if err != nil {
			return echo.NewHTTPError(http.StatusForbidden, err.Error())
		}
		return c.JSON(http.StatusCreated, map[string]any{"case": saved})
	})

	e.DELETE("/api/agent/lab/cases/:id", func(c echo.Context) error {
		ctx, project, err := authProject(c, store)
		if err != nil {
			return err
		}
		if err := store.DeleteAgentLabCase(c.Request().Context(), ctx.User.ID, project.ID, c.QueryParam("agent"), c.Param("id")); err != nil {
			return echo.NewHTTPError(http.StatusForbidden, err.Error())
		}
		return c.NoContent(http.StatusNoContent)
	})

	// Manual verdict override (AC: exact-match default with a manual pass override).
	e.POST("/api/agent/lab/cases/:id/verdict", func(c echo.Context) error {
		ctx, project, err := authProject(c, store)
		if err != nil {
			return err
		}
		var payload struct {
			Status string `json:"status"`
			RunID  string `json:"run_id"`
		}
		if err := c.Bind(&payload); err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, "invalid json")
		}
		if err := store.UpdateAgentLabCaseVerdict(c.Request().Context(), ctx.User.ID, project.ID, c.QueryParam("agent"), c.Param("id"), payload.Status, payload.RunID); err != nil {
			return echo.NewHTTPError(http.StatusForbidden, err.Error())
		}
		return c.JSON(http.StatusOK, map[string]any{"ok": true})
	})

	// --- test mode (AC1, AC5): auto-run to completion, verdict vs expected ---
	e.POST("/api/agent/lab/test", func(c echo.Context) error {
		ctx, project, err := authProject(c, store)
		if err != nil {
			return err
		}
		var payload struct {
			Input    string `json:"input"`
			Expected string `json:"expected"`
			CaseID   string `json:"case_id"` // when set, the case's verdict is updated
		}
		if err := c.Bind(&payload); err != nil || payload.Input == "" {
			return echo.NewHTTPError(http.StatusBadRequest, "input required")
		}
		result, err := lab.RunTest(c.Request().Context(), ctx.User.ID, project.ID, c.QueryParam("agent"), payload.Input, payload.Expected)
		if err != nil {
			return echo.NewHTTPError(http.StatusBadGateway, err.Error())
		}
		// Persist the verdict against a saved case when one was named (best-effort:
		// a non-manager can still run a case, the verdict just isn't recorded).
		if payload.CaseID != "" && result.RunID != "" {
			_ = store.UpdateAgentLabCaseVerdict(c.Request().Context(), ctx.User.ID, project.ID, c.QueryParam("agent"), payload.CaseID, result.Status, result.RunID)
		}
		return c.JSON(http.StatusOK, map[string]any{"result": result})
	})

	// --- replay (AC4): fold any completed run into steps ---
	e.GET("/api/agent/lab/runs/:run_id/steps", func(c echo.Context) error {
		ctx, project, err := authProject(c, store)
		if err != nil {
			return err
		}
		steps, err := lab.ReplaySteps(c.Request().Context(), ctx.User.ID, project.ID, c.Param("run_id"))
		if err != nil {
			return err
		}
		return c.JSON(http.StatusOK, map[string]any{"steps": steps})
	})

	// --- explain mode (AC1, AC3): SSE run paused before each step ---
	e.POST("/api/agent/lab/explain", func(c echo.Context) error {
		ctx, project, err := authProject(c, store)
		if err != nil {
			return err
		}
		var payload struct {
			Input string `json:"input"`
		}
		if err := c.Bind(&payload); err != nil || payload.Input == "" {
			return echo.NewHTTPError(http.StatusBadRequest, "input required")
		}

		w := c.Response()
		h := w.Header()
		h.Set("Content-Type", "text/event-stream")
		h.Set("Cache-Control", "no-cache")
		h.Set("Connection", "keep-alive")
		h.Set("X-Accel-Buffering", "no")
		w.WriteHeader(http.StatusOK)
		w.Flush()

		emit := func(ev agentruntime.LabEvent) { writeSSE(w, ev.Type, ev) }
		_ = lab.StartExplain(c.Request().Context(), ctx.User.ID, project.ID, c.QueryParam("agent"), payload.Input, emit)
		return nil
	})

	e.POST("/api/agent/lab/explain/:run_id/advance", func(c echo.Context) error {
		_, project, err := authProject(c, store)
		if err != nil {
			return err
		}
		if !lab.Advance(project.ID, c.Param("run_id")) {
			return echo.NewHTTPError(http.StatusNotFound, "no paused run for this id")
		}
		return c.JSON(http.StatusOK, map[string]any{"ok": true})
	})

	e.POST("/api/agent/lab/explain/:run_id/stop", func(c echo.Context) error {
		_, project, err := authProject(c, store)
		if err != nil {
			return err
		}
		if !lab.Stop(project.ID, c.Param("run_id")) {
			return echo.NewHTTPError(http.StatusNotFound, "no active run for this id")
		}
		return c.JSON(http.StatusOK, map[string]any{"ok": true})
	})

	// --- steer (§ steering): inject a mid-run correction into a paused explain
	// run, honored at the top of its next turn (before the model reasons). Lets a
	// builder nudge a stepped run without restarting it. ---
	e.POST("/api/agent/lab/explain/:run_id/steer", func(c echo.Context) error {
		_, project, err := authProject(c, store)
		if err != nil {
			return err
		}
		var payload struct {
			Message string `json:"message"`
		}
		if err := c.Bind(&payload); err != nil || payload.Message == "" {
			return echo.NewHTTPError(http.StatusBadRequest, "message required")
		}
		if !lab.Steer(project.ID, c.Param("run_id"), payload.Message) {
			return echo.NewHTTPError(http.StatusNotFound, "no active run for this id")
		}
		return c.JSON(http.StatusOK, map[string]any{"ok": true})
	})
}
