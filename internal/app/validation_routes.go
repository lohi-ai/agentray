package app

import (
	"net/http"
	"strconv"

	"github.com/labstack/echo/v4"
	"github.com/lohi-ai/agentray/internal/dataplane/store"
	"github.com/lohi-ai/agentray/internal/shared/opcore"
)

// validation_routes.go — the owner's side of the pre-product pair. The public
// half (the landing page posting a signup) lives in ingest/waitlist.go; these
// routes are all authenticated and project-scoped through authProject, like
// every other /api surface.
//
// The two writes resolve through authProjectForWrite instead: authProject binds
// the project and the store then proves membership, which admits a viewer to
// commit the threshold and decide the verdict. The class is the one the
// operation surface already requires for the same act — plans:write, the class
// propose_test, update_test, record_outcome and abandon_test carry.
func registerValidationRoutes(e *echo.Echo, store *storage.Store, ops *opAdapter) {
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

	// Commit is the load-bearing click on /start?job=validate: the owner agreeing
	// to the number BEFORE the data arrives. Everything the readout says
	// afterwards is only meaningful because this happened first.
	e.POST("/api/validation/tests/:id/commit", func(c echo.Context) error {
		ctx, project, err := authProjectForWrite(c, store, ops, legacyWrite(opcore.AccessPlansWrite))
		if err != nil {
			return err
		}
		if err := store.CommitValidationTest(c.Request().Context(), ctx.User.ID, project.ID, c.Param("id")); err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, err.Error())
		}
		return c.JSON(http.StatusOK, map[string]any{"ok": true, "status": storage.TestCommitted})
	})

	e.POST("/api/validation/tests/:id/decide", func(c echo.Context) error {
		ctx, project, err := authProjectForWrite(c, store, ops, legacyWrite(opcore.AccessPlansWrite))
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

}
