package app

import (
	"encoding/csv"
	"net/http"
	"strconv"

	"github.com/labstack/echo/v4"
	ingestion "github.com/lohi-ai/agentray/internal/dataplane/ingest"
	"github.com/lohi-ai/agentray/internal/dataplane/store"
)

// validation_routes.go — the owner's side of the pre-product pair. The public
// half (the landing page posting a signup) lives in ingest/waitlist.go; these
// routes are all authenticated and project-scoped through authProject, like
// every other /api surface.
func registerValidationRoutes(e *echo.Echo, store *storage.Store) {
	// The whole validate readout in one request: the active test, how it is
	// doing, and the waitlist count. One call because /start renders them
	// together and three round-trips would each flash their own empty state.
	e.GET("/api/validation/status", func(c echo.Context) error {
		_, project, err := authProject(c, store)
		if err != nil {
			return err
		}
		count, err := store.CountWaitlistSignups(c.Request().Context(), project.ID)
		if err != nil {
			return err
		}
		test, err := store.ActiveValidationTest(c.Request().Context(), project.ID)
		if err != nil {
			return err
		}
		out := map[string]any{"waitlist_count": count, "test": nil}
		if test != nil {
			out["test"] = test
			// A test the owner has not committed to counts nothing, so it gets no
			// progress and no verdict. Reporting "0 of 40" against a number nobody
			// agreed to reads as a failing test rather than an unanswered question.
			if test.Status == storage.TestCommitted {
				progress, err := store.ValidationTestProgress(c.Request().Context(), *test)
				if err != nil {
					return err
				}
				out["progress"] = progress
				// The verdict is computed server-side so the web app and any agent
				// reading test_status can never disagree about whether it passed.
				out["verdict"] = progress.Verdict()
			}
		}
		return c.JSON(http.StatusOK, out)
	})

	// The plural read — every prototype, not the one ActiveValidationTest picks.
	// Each committed row arrives already measured and already carrying its
	// verdict, computed server-side for the same reason /status computes it:
	// the page and any agent reading list_tests must not be able to disagree
	// about whether a test passed.
	//
	// `total` travels with the page because the list is capped (each measured row
	// costs one event-store aggregation). A project past the cap is told it is
	// looking at a page; it is never handed a silent fraction of its own record.
	e.GET("/api/validation/tests", func(c echo.Context) error {
		ctx, project, err := authProject(c, store)
		if err != nil {
			return err
		}
		tests, total, err := store.ListValidationTests(c.Request().Context(), ctx.User.ID, project.ID, intParam(c, "limit", 0, 0, 100))
		if err != nil {
			return err
		}
		measured, err := store.MeasureValidationTests(c.Request().Context(), tests)
		if err != nil {
			return err
		}
		count, err := store.CountWaitlistSignups(c.Request().Context(), project.ID)
		if err != nil {
			return err
		}
		return c.JSON(http.StatusOK, map[string]any{
			"tests":          measured,
			"total":          total,
			"truncated":      total > len(measured),
			"waitlist_count": count,
		})
	})

	e.GET("/api/validation/tests/:id", func(c echo.Context) error {
		ctx, project, err := authProject(c, store)
		if err != nil {
			return err
		}
		test, err := store.ValidationTestByID(c.Request().Context(), ctx.User.ID, project.ID, c.Param("id"))
		if err != nil {
			if storage.ErrNoSuchValidationTest(err) {
				return echo.NewHTTPError(http.StatusNotFound, "no prototype with that id")
			}
			return err
		}
		measured, err := store.MeasureValidationTest(c.Request().Context(), test)
		if err != nil {
			return err
		}
		count, err := store.CountWaitlistSignups(c.Request().Context(), project.ID)
		if err != nil {
			return err
		}
		return c.JSON(http.StatusOK, map[string]any{"test": measured, "waitlist_count": count})
	})

	// The owner writing their own test, for the case where no agent is involved.
	// Same table, same `proposed` starting state — there is no path that creates
	// an already-committed test, because commitment is always a second, separate
	// act.
	e.POST("/api/validation/tests", func(c echo.Context) error {
		_, project, err := authProject(c, store)
		if err != nil {
			return err
		}
		var payload struct {
			Hypothesis    string `json:"hypothesis"`
			MetricEvent   string `json:"metric_event"`
			BaselineEvent string `json:"baseline_event"`
			TargetCount   int    `json:"target_count"`
			WindowDays    int    `json:"window_days"`
		}
		if err := c.Bind(&payload); err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, "invalid json")
		}
		id, err := store.CreateValidationTest(c.Request().Context(), storage.ValidationTest{
			ProjectID:     project.ID,
			Hypothesis:    payload.Hypothesis,
			MetricEvent:   payload.MetricEvent,
			BaselineEvent: payload.BaselineEvent,
			TargetCount:   payload.TargetCount,
			WindowDays:    payload.WindowDays,
		})
		if err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, err.Error())
		}
		return c.JSON(http.StatusCreated, map[string]any{"id": id, "status": storage.TestProposed})
	})

	// Commit is the load-bearing click on /start?job=validate: the owner agreeing
	// to the number BEFORE the data arrives. Everything the readout says
	// afterwards is only meaningful because this happened first.
	e.POST("/api/validation/tests/:id/commit", func(c echo.Context) error {
		ctx, project, err := authProject(c, store)
		if err != nil {
			return err
		}
		if err := store.CommitValidationTest(c.Request().Context(), ctx.User.ID, project.ID, c.Param("id")); err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, err.Error())
		}
		return c.JSON(http.StatusOK, map[string]any{"ok": true, "status": storage.TestCommitted})
	})

	e.POST("/api/validation/tests/:id/decide", func(c echo.Context) error {
		ctx, project, err := authProject(c, store)
		if err != nil {
			return err
		}
		var payload struct {
			Status string `json:"status"` // passed | failed | abandoned
			Note   string `json:"note"`
		}
		if err := c.Bind(&payload); err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, "invalid json")
		}
		if err := store.DecideValidationTest(c.Request().Context(), ctx.User.ID, project.ID, c.Param("id"), payload.Status, payload.Note); err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, err.Error())
		}
		return c.JSON(http.StatusOK, map[string]any{"ok": true, "status": payload.Status})
	})

	// --- waitlist (the owner's view of it) ---

	e.GET("/api/validation/waitlist", func(c echo.Context) error {
		ctx, project, err := authProject(c, store)
		if err != nil {
			return err
		}
		limit, _ := strconv.Atoi(c.QueryParam("limit"))
		rows, err := store.ListWaitlistSignups(c.Request().Context(), ctx.User.ID, project.ID, limit)
		if err != nil {
			return err
		}
		count, err := store.CountWaitlistSignups(c.Request().Context(), project.ID)
		if err != nil {
			return err
		}
		return c.JSON(http.StatusOK, map[string]any{"signups": rows, "count": count})
	})

	// Export exists because a contact list the owner cannot take with them is a
	// hostage, not a feature.
	e.GET("/api/validation/waitlist.csv", func(c echo.Context) error {
		ctx, project, err := authProject(c, store)
		if err != nil {
			return err
		}
		c.Response().Header().Set(echo.HeaderContentType, "text/csv; charset=utf-8")
		c.Response().Header().Set(echo.HeaderContentDisposition, `attachment; filename="waitlist.csv"`)
		c.Response().WriteHeader(http.StatusOK)
		w := csv.NewWriter(c.Response())
		// unsubscribe_url ships in the export because the owner is the one who
		// sends the mail this file is for, and mail without a way out is spam.
		_ = w.Write([]string{"email", "status", "source", "referrer", "joined_at", "unsubscribe_url"})
		// Streamed in keyset pages rather than one capped query: the whole list or
		// an error, never a quiet fraction of it.
		err = store.ExportWaitlistSignups(c.Request().Context(), ctx.User.ID, project.ID, func(r storage.WaitlistSignup) error {
			return w.Write([]string{r.Email, r.Status, r.Source, r.Referrer,
				r.CreatedAt.Format("2006-01-02 15:04:05"), ingestion.UnsubscribeURL(c, r.UnsubscribeToken)})
		})
		if err != nil {
			return err
		}
		w.Flush()
		return w.Error()
	})

	// A real delete, not a status flag: "remove my data" has to mean the row is
	// gone from the table.
	e.DELETE("/api/validation/waitlist/:id", func(c echo.Context) error {
		ctx, project, err := authProject(c, store)
		if err != nil {
			return err
		}
		if err := store.DeleteWaitlistSignup(c.Request().Context(), ctx.User.ID, project.ID, c.Param("id")); err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, err.Error())
		}
		return c.JSON(http.StatusOK, map[string]any{"ok": true})
	})
}
