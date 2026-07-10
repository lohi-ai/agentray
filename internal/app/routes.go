package app

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/lohi-ai/agentray/agentcore"
	"github.com/lohi-ai/agentray/internal/agentruntime"
	"github.com/lohi-ai/agentray/internal/ingestion"
	"github.com/lohi-ai/agentray/sandbox"
	"github.com/lohi-ai/agentray/internal/storage"
)

func registerRoutes(e *echo.Echo, store *storage.Store, events ingestion.EventQueue, rateLimit echo.MiddlewareFunc, authRateLimit echo.MiddlewareFunc, scheduler *agentruntime.Scheduler, sb agentcore.Sandbox, ws *sandbox.Workspace, liveReg *agentruntime.LiveRegistry, runnerOpts ...agentruntime.RunnerOption) {
	h := ingestion.NewHandler(store, events, store).WithCatalogGuard(store)

	e.GET("/healthz", func(c echo.Context) error {
		return c.JSON(http.StatusOK, map[string]string{"ok": "true"})
	})

	registerAgentRoutes(e, store, scheduler, sb, ws, liveReg, runnerOpts...)
	registerAgentMonitorRoutes(e, store)
	registerAgentLabRoutes(e, store, sb != nil, runnerOpts...)
	registerAlertRoutes(e, store)

	e.POST("/capture", h.Capture, rateLimit)
	e.POST("/batch", h.Batch, rateLimit)
	e.POST("/identify", h.Identify, rateLimit)
	e.POST("/alias", h.Alias, rateLimit)

	// PostHog-compatible aliases used by posthog-js and simple SDK shims.
	e.POST("/e/", h.Capture, rateLimit)
	e.POST("/e", h.Capture, rateLimit)
	e.POST("/i/v0/e/", h.Batch, rateLimit)
	e.POST("/i/v0/e", h.Batch, rateLimit)

	e.POST("/api/auth/signup", func(c echo.Context) error {
		var payload struct {
			Email         string `json:"email"`
			Name          string `json:"name"`
			Password      string `json:"password"`
			WorkspaceName string `json:"workspace_name"`
			ProjectName   string `json:"project_name"`
		}
		if err := c.Bind(&payload); err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, "invalid json")
		}
		bootstrap, err := store.CreateAccount(
			c.Request().Context(),
			payload.Email,
			payload.Name,
			payload.Password,
			payload.WorkspaceName,
			payload.ProjectName,
		)
		if err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, err.Error())
		}
		session, token, err := store.CreateUserSession(c.Request().Context(), bootstrap.User.ID, sessionTTL)
		if err != nil {
			return err
		}
		setSessionCookie(c, token, session.ExpiresAt)
		ctx := authContext{User: bootstrap.User, Session: session}
		return c.JSON(http.StatusCreated, authPayload(ctx, []storage.Workspace{bootstrap.Workspace}, []storage.Project{bootstrap.Project}, bootstrap.Project))
	}, authRateLimit)

	e.POST("/api/auth/login", func(c echo.Context) error {
		var payload struct {
			Email    string `json:"email"`
			Password string `json:"password"`
		}
		if err := c.Bind(&payload); err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, "invalid json")
		}
		user, err := store.AuthenticateUser(c.Request().Context(), payload.Email, payload.Password)
		if err != nil {
			return echo.NewHTTPError(http.StatusUnauthorized, "invalid email or password")
		}
		session, token, err := store.CreateUserSession(c.Request().Context(), user.ID, sessionTTL)
		if err != nil {
			return err
		}
		setSessionCookie(c, token, session.ExpiresAt)
		ctx := authContext{User: user, Session: session}
		workspaces, projects, project, err := accountResources(c, store, ctx, "")
		if err != nil {
			return err
		}
		return c.JSON(http.StatusOK, authPayload(ctx, workspaces, projects, project))
	}, authRateLimit)

	e.POST("/api/auth/logout", func(c echo.Context) error {
		if cookie, err := c.Cookie(sessionCookieName); err == nil {
			if err := store.DeleteUserSession(c.Request().Context(), cookie.Value); err != nil {
				return err
			}
		}
		clearSessionCookie(c)
		return c.NoContent(http.StatusNoContent)
	})

	e.GET("/api/auth/me", func(c echo.Context) error {
		ctx, err := authFromRequest(c, store)
		if err != nil {
			return err
		}
		workspaces, projects, project, err := accountResources(c, store, ctx, c.QueryParam("project_id"))
		if err != nil {
			return err
		}
		return c.JSON(http.StatusOK, authPayload(ctx, workspaces, projects, project))
	})

	e.PUT("/api/users/me", func(c echo.Context) error {
		ctx, err := authFromRequest(c, store)
		if err != nil {
			return err
		}
		var payload struct {
			Name string `json:"name"`
		}
		if err := c.Bind(&payload); err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, "invalid json")
		}
		user, err := store.UpdateUser(c.Request().Context(), ctx.User.ID, payload.Name)
		if err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, err.Error())
		}
		ctx.User = user
		workspaces, projects, project, err := accountResources(c, store, ctx, c.QueryParam("project_id"))
		if err != nil {
			return err
		}
		return c.JSON(http.StatusOK, authPayload(ctx, workspaces, projects, project))
	})

	e.GET("/api/workspaces", func(c echo.Context) error {
		ctx, err := authFromRequest(c, store)
		if err != nil {
			return err
		}
		workspaces, err := store.ListUserWorkspaces(c.Request().Context(), ctx.User.ID)
		if err != nil {
			return err
		}
		return c.JSON(http.StatusOK, map[string]any{"workspaces": workspaces})
	})

	e.POST("/api/workspaces", func(c echo.Context) error {
		ctx, err := authFromRequest(c, store)
		if err != nil {
			return err
		}
		var payload struct {
			Name string `json:"name"`
		}
		if err := c.Bind(&payload); err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, "invalid json")
		}
		workspace, err := store.CreateWorkspace(c.Request().Context(), ctx.User.ID, payload.Name)
		if err != nil {
			return err
		}
		return c.JSON(http.StatusCreated, map[string]any{"workspace": workspace})
	})

	e.PUT("/api/workspaces/:workspace_id", func(c echo.Context) error {
		ctx, err := authFromRequest(c, store)
		if err != nil {
			return err
		}
		var payload struct {
			Name string `json:"name"`
		}
		if err := c.Bind(&payload); err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, "invalid json")
		}
		workspace, err := store.UpdateWorkspace(c.Request().Context(), ctx.User.ID, c.Param("workspace_id"), payload.Name)
		if err != nil {
			return echo.NewHTTPError(http.StatusForbidden, "workspace permission denied")
		}
		return c.JSON(http.StatusOK, map[string]any{"workspace": workspace})
	})

	e.GET("/api/workspaces/:workspace_id/projects", func(c echo.Context) error {
		ctx, err := authFromRequest(c, store)
		if err != nil {
			return err
		}
		projects, err := store.ListWorkspaceProjects(c.Request().Context(), ctx.User.ID, c.Param("workspace_id"))
		if err != nil {
			return err
		}
		return c.JSON(http.StatusOK, map[string]any{"projects": projects})
	})

	e.GET("/api/workspaces/:workspace_id/usage", func(c echo.Context) error {
		ctx, err := authFromRequest(c, store)
		if err != nil {
			return err
		}
		usage, err := store.WorkspaceUsage(c.Request().Context(), ctx.User.ID, c.Param("workspace_id"), filterFromRequest(c))
		if err != nil {
			return echo.NewHTTPError(http.StatusForbidden, "workspace not available")
		}
		return c.JSON(http.StatusOK, map[string]any{"usage": usage})
	})

	e.GET("/api/workspaces/:workspace_id/members", func(c echo.Context) error {
		ctx, err := authFromRequest(c, store)
		if err != nil {
			return err
		}
		members, err := store.ListWorkspaceMembers(c.Request().Context(), ctx.User.ID, c.Param("workspace_id"))
		if err != nil {
			return echo.NewHTTPError(http.StatusForbidden, "workspace not available")
		}
		return c.JSON(http.StatusOK, map[string]any{"members": members})
	})

	e.GET("/api/workspaces/:workspace_id/audit-logs", func(c echo.Context) error {
		ctx, err := authFromRequest(c, store)
		if err != nil {
			return err
		}
		limit, _ := strconv.Atoi(c.QueryParam("limit"))
		logs, err := store.ListWorkspaceAuditLogs(c.Request().Context(), ctx.User.ID, c.Param("workspace_id"), limit)
		if err != nil {
			return echo.NewHTTPError(http.StatusForbidden, "workspace not available")
		}
		return c.JSON(http.StatusOK, map[string]any{"logs": logs})
	})

	e.POST("/api/workspaces/:workspace_id/members", func(c echo.Context) error {
		ctx, err := authFromRequest(c, store)
		if err != nil {
			return err
		}
		var payload struct {
			Email string `json:"email"`
			Role  string `json:"role"`
		}
		if err := c.Bind(&payload); err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, "invalid json")
		}
		member, err := store.AddWorkspaceMemberByEmail(c.Request().Context(), ctx.User.ID, c.Param("workspace_id"), payload.Email, payload.Role)
		if err != nil {
			return echo.NewHTTPError(http.StatusForbidden, err.Error())
		}
		return c.JSON(http.StatusCreated, map[string]any{"member": member})
	})

	e.PUT("/api/workspaces/:workspace_id/members/:user_id", func(c echo.Context) error {
		ctx, err := authFromRequest(c, store)
		if err != nil {
			return err
		}
		var payload struct {
			Role string `json:"role"`
		}
		if err := c.Bind(&payload); err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, "invalid json")
		}
		member, err := store.UpdateWorkspaceMemberRole(c.Request().Context(), ctx.User.ID, c.Param("workspace_id"), c.Param("user_id"), payload.Role)
		if err != nil {
			return echo.NewHTTPError(http.StatusForbidden, err.Error())
		}
		return c.JSON(http.StatusOK, map[string]any{"member": member})
	})

	e.DELETE("/api/workspaces/:workspace_id/members/:user_id", func(c echo.Context) error {
		ctx, err := authFromRequest(c, store)
		if err != nil {
			return err
		}
		if err := store.RemoveWorkspaceMember(c.Request().Context(), ctx.User.ID, c.Param("workspace_id"), c.Param("user_id")); err != nil {
			return echo.NewHTTPError(http.StatusForbidden, err.Error())
		}
		return c.NoContent(http.StatusNoContent)
	})

	e.POST("/api/workspaces/:workspace_id/projects", func(c echo.Context) error {
		ctx, err := authFromRequest(c, store)
		if err != nil {
			return err
		}
		var payload struct {
			Name string `json:"name"`
		}
		if err := c.Bind(&payload); err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, "invalid json")
		}
		project, err := store.CreateWorkspaceProject(c.Request().Context(), ctx.User.ID, c.Param("workspace_id"), payload.Name)
		if err != nil {
			return echo.NewHTTPError(http.StatusForbidden, "workspace permission denied")
		}
		return c.JSON(http.StatusCreated, map[string]any{"project": project})
	})

	e.GET("/api/projects", func(c echo.Context) error {
		project, err := projectFromRequest(c, store)
		if err != nil {
			return err
		}
		return c.JSON(http.StatusOK, map[string]any{"project": project})
	})

	e.POST("/api/projects", func(c echo.Context) error {
		ctx, err := authFromRequest(c, store)
		if err != nil {
			return err
		}
		var payload struct {
			WorkspaceID string `json:"workspace_id"`
			Name        string `json:"name"`
		}
		if err := c.Bind(&payload); err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, "invalid json")
		}
		workspaceID := strings.TrimSpace(payload.WorkspaceID)
		if workspaceID == "" {
			workspaces, err := store.ListUserWorkspaces(c.Request().Context(), ctx.User.ID)
			if err != nil {
				return err
			}
			if len(workspaces) == 0 {
				return echo.NewHTTPError(http.StatusBadRequest, "create a workspace first")
			}
			workspaceID = workspaces[0].ID
		}
		project, err := store.CreateWorkspaceProject(c.Request().Context(), ctx.User.ID, workspaceID, strings.TrimSpace(payload.Name))
		if err != nil {
			return echo.NewHTTPError(http.StatusForbidden, "workspace permission denied")
		}
		return c.JSON(http.StatusCreated, map[string]any{"project": project})
	})

	e.PUT("/api/projects/:project_id", func(c echo.Context) error {
		ctx, err := authFromRequest(c, store)
		if err != nil {
			return err
		}
		var payload struct {
			Name string `json:"name"`
		}
		if err := c.Bind(&payload); err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, "invalid json")
		}
		project, err := store.UpdateProjectForUser(c.Request().Context(), ctx.User.ID, c.Param("project_id"), payload.Name)
		if err != nil {
			return echo.NewHTTPError(http.StatusForbidden, "project permission denied")
		}
		return c.JSON(http.StatusOK, map[string]any{"project": project})
	})

	e.POST("/api/projects/:project_id/rotate-key", func(c echo.Context) error {
		ctx, err := authFromRequest(c, store)
		if err != nil {
			return err
		}
		rotated, err := store.RotateProjectAPIKeyForUser(c.Request().Context(), ctx.User.ID, c.Param("project_id"))
		if err != nil {
			return echo.NewHTTPError(http.StatusForbidden, "project permission denied")
		}
		return c.JSON(http.StatusOK, map[string]any{"project": rotated})
	})

	e.GET("/api/activity", func(c echo.Context) error {
		project, err := projectFromRequest(c, store)
		if err != nil {
			return err
		}
		summary, err := store.ActivitySummary(c.Request().Context(), project.ID, filterFromRequest(c))
		if err != nil {
			return err
		}
		return c.JSON(http.StatusOK, map[string]any{
			"project": project,
			"summary": summary,
		})
	})

	e.GET("/api/insights/run", func(c echo.Context) error {
		project, err := projectFromRequest(c, store)
		if err != nil {
			return err
		}
		steps := splitCSV(c.QueryParam("steps"))
		result, err := store.RunInsight(
			c.Request().Context(),
			project.ID,
			firstNonEmpty(c.QueryParam("type"), "trend"),
			c.QueryParam("metric"),
			steps,
			filterFromRequest(c),
		)
		if err != nil {
			return err
		}
		return c.JSON(http.StatusOK, map[string]any{"project": project, "insight": result})
	})

	e.GET("/api/templates", func(c echo.Context) error {
		project, err := projectFromRequest(c, store)
		if err != nil {
			return err
		}
		templates, err := store.ListTemplates(c.Request().Context(), project.ID)
		if err != nil {
			return err
		}
		return c.JSON(http.StatusOK, map[string]any{"templates": templates})
	})

	e.POST("/api/templates/:template_id/apply", func(c echo.Context) error {
		project, err := projectFromRequest(c, store)
		if err != nil {
			return err
		}
		dashboard, charts, err := store.CloneTemplate(c.Request().Context(), c.Param("template_id"), project.ID)
		if err != nil {
			return echo.NewHTTPError(http.StatusNotFound, "template not found")
		}
		return c.JSON(http.StatusCreated, map[string]any{
			"dashboard": dashboard,
			"charts":    charts,
		})
	})

	e.POST("/api/templates/:template_id/charts/:chart_id/clone", func(c echo.Context) error {
		project, err := projectFromRequest(c, store)
		if err != nil {
			return err
		}
		var body struct {
			DashboardID string `json:"dashboard_id"`
		}
		if err := c.Bind(&body); err != nil || body.DashboardID == "" {
			return echo.NewHTTPError(http.StatusBadRequest, "dashboard_id required")
		}
		chart, err := store.CloneTemplateChart(c.Request().Context(), c.Param("chart_id"), body.DashboardID, project.ID)
		if err != nil {
			if err.Error() == "forbidden" {
				return echo.NewHTTPError(http.StatusForbidden, "dashboard not owned by project")
			}
			return echo.NewHTTPError(http.StatusNotFound, "template chart not found")
		}
		return c.JSON(http.StatusCreated, map[string]any{"chart": chart})
	})

	e.GET("/api/web-analytics", func(c echo.Context) error {
		project, err := projectFromRequest(c, store)
		if err != nil {
			return err
		}
		analytics, err := store.WebAnalytics(c.Request().Context(), project.ID, filterFromRequest(c))
		if err != nil {
			return err
		}
		return c.JSON(http.StatusOK, map[string]any{"project": project, "web_analytics": analytics})
	})

	e.GET("/api/persons", func(c echo.Context) error {
		project, err := projectFromRequest(c, store)
		if err != nil {
			return err
		}
		persons, err := store.Persons(c.Request().Context(), project.ID, filterFromRequest(c))
		if err != nil {
			return err
		}
		return c.JSON(http.StatusOK, map[string]any{"project": project, "persons": persons})
	})

	e.GET("/api/cohorts", func(c echo.Context) error {
		project, err := projectFromRequest(c, store)
		if err != nil {
			return err
		}
		// A concrete from/to window is an explicit user choice; the preset `hours`
		// range is not. Cohorts widens the look-back only for the latter.
		explicitRange := c.QueryParam("from") != "" || c.QueryParam("to") != ""
		cohorts, err := store.Cohorts(c.Request().Context(), project.ID, filterFromRequest(c), strings.TrimSpace(c.QueryParam("segment")), explicitRange)
		if err != nil {
			return err
		}
		return c.JSON(http.StatusOK, map[string]any{"project": project, "cohorts": cohorts})
	})

	// Per-project custom cohort audiences (paid/premium-style groups). The rule is
	// structured (kind + plans), compiled to SQL server-side — never raw SQL.
	e.GET("/api/cohorts/audiences", func(c echo.Context) error {
		project, err := projectFromRequest(c, store)
		if err != nil {
			return err
		}
		audiences, err := store.ListProjectAudiences(c.Request().Context(), project.ID)
		if err != nil {
			return err
		}
		return c.JSON(http.StatusOK, map[string]any{"project": project, "audiences": audiences})
	})

	e.POST("/api/cohorts/audiences", func(c echo.Context) error {
		project, err := projectFromRequest(c, store)
		if err != nil {
			return err
		}
		var payload struct {
			Label string   `json:"label"`
			Kind  string   `json:"kind"`
			Plans []string `json:"plans"`
		}
		if err := c.Bind(&payload); err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, "invalid json")
		}
		audience, err := store.CreateProjectAudience(c.Request().Context(), project.ID, payload.Label, strings.TrimSpace(payload.Kind), payload.Plans)
		if err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, err.Error())
		}
		return c.JSON(http.StatusCreated, map[string]any{"audience": audience})
	})

	e.PUT("/api/cohorts/audiences/:audience_id", func(c echo.Context) error {
		project, err := projectFromRequest(c, store)
		if err != nil {
			return err
		}
		var payload struct {
			Label string   `json:"label"`
			Kind  string   `json:"kind"`
			Plans []string `json:"plans"`
		}
		if err := c.Bind(&payload); err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, "invalid json")
		}
		audience, err := store.UpdateProjectAudience(c.Request().Context(), project.ID, c.Param("audience_id"), payload.Label, strings.TrimSpace(payload.Kind), payload.Plans)
		if err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, err.Error())
		}
		return c.JSON(http.StatusOK, map[string]any{"audience": audience})
	})

	e.DELETE("/api/cohorts/audiences/:audience_id", func(c echo.Context) error {
		project, err := projectFromRequest(c, store)
		if err != nil {
			return err
		}
		if err := store.DeleteProjectAudience(c.Request().Context(), project.ID, c.Param("audience_id")); err != nil {
			return err
		}
		return c.NoContent(http.StatusNoContent)
	})

	// Per-project subscription mapping — how the cohort engine reads subscription
	// lifecycle off events (config, never raw SQL).
	e.GET("/api/subscription/mapping", func(c echo.Context) error {
		project, err := projectFromRequest(c, store)
		if err != nil {
			return err
		}
		mapping, err := store.GetSubscriptionMapping(c.Request().Context(), project.ID)
		if err != nil {
			return err
		}
		return c.JSON(http.StatusOK, map[string]any{"project": project, "mapping": mapping})
	})

	e.PUT("/api/subscription/mapping", func(c echo.Context) error {
		project, err := projectFromRequest(c, store)
		if err != nil {
			return err
		}
		var payload storage.SubscriptionMapping
		if err := c.Bind(&payload); err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, "invalid json")
		}
		mapping, err := store.UpsertSubscriptionMapping(c.Request().Context(), project.ID, payload)
		if err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, err.Error())
		}
		return c.JSON(http.StatusOK, map[string]any{"mapping": mapping})
	})

	e.GET("/api/events/explore", func(c echo.Context) error {
		project, err := projectFromRequest(c, store)
		if err != nil {
			return err
		}
		explorer, err := store.ExploreEvents(c.Request().Context(), project.ID, filterFromRequest(c))
		if err != nil {
			return err
		}
		return c.JSON(http.StatusOK, map[string]any{"project": project, "explorer": explorer})
	})

	e.GET("/api/events/names", func(c echo.Context) error {
		project, err := projectFromRequest(c, store)
		if err != nil {
			return err
		}
		names, err := store.EventNames(c.Request().Context(), project.ID, intParam(c, "limit", 500, 1, 1000))
		if err != nil {
			return err
		}
		return c.JSON(http.StatusOK, map[string]any{"project": project, "names": names})
	})

	e.GET("/api/sessions/:session_id/replay", func(c echo.Context) error {
		project, err := projectFromRequest(c, store)
		if err != nil {
			return err
		}
		replay, err := store.AgentReplay(c.Request().Context(), project.ID, c.Param("session_id"))
		if err != nil {
			return err
		}
		return c.JSON(http.StatusOK, map[string]any{"project": project, "replay": replay})
	})

	e.GET("/api/saved-queries", func(c echo.Context) error {
		project, err := projectFromRequest(c, store)
		if err != nil {
			return err
		}
		queries, err := store.ListSavedQueries(c.Request().Context(), project.ID)
		if err != nil {
			return err
		}
		return c.JSON(http.StatusOK, map[string]any{"project": project, "saved_queries": queries})
	})

	e.POST("/api/saved-queries", func(c echo.Context) error {
		project, err := projectFromRequest(c, store)
		if err != nil {
			return err
		}
		var payload struct {
			NaturalLanguage string `json:"natural_language"`
			GeneratedSQL    string `json:"generated_sql"`
			Verified        bool   `json:"verified"`
		}
		if err := c.Bind(&payload); err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, "invalid json")
		}
		query, err := store.CreateSavedQuery(c.Request().Context(), project.ID, strings.TrimSpace(payload.NaturalLanguage), strings.TrimSpace(payload.GeneratedSQL), payload.Verified)
		if err != nil {
			return err
		}
		return c.JSON(http.StatusCreated, map[string]any{"saved_query": query})
	})

	e.POST("/api/saved-queries/:query_id/run", func(c echo.Context) error {
		project, err := projectFromRequest(c, store)
		if err != nil {
			return err
		}
		result, err := store.RunSavedQuery(c.Request().Context(), project.ID, c.Param("query_id"))
		if err != nil {
			return c.JSON(http.StatusInternalServerError, map[string]any{"error": err.Error()})
		}
		return c.JSON(http.StatusOK, map[string]any{"result": result})
	})

	e.PATCH("/api/saved-queries/:query_id", func(c echo.Context) error {
		project, err := projectFromRequest(c, store)
		if err != nil {
			return err
		}
		var payload struct {
			NaturalLanguage string `json:"natural_language"`
		}
		if err := c.Bind(&payload); err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, "invalid json")
		}
		query, err := store.RenameSavedQuery(c.Request().Context(), project.ID, c.Param("query_id"), strings.TrimSpace(payload.NaturalLanguage))
		if err != nil {
			return err
		}
		return c.JSON(http.StatusOK, map[string]any{"saved_query": query})
	})

	e.DELETE("/api/saved-queries/:query_id", func(c echo.Context) error {
		project, err := projectFromRequest(c, store)
		if err != nil {
			return err
		}
		if err := store.DeleteSavedQuery(c.Request().Context(), project.ID, c.Param("query_id")); err != nil {
			return err
		}
		return c.NoContent(http.StatusNoContent)
	})

	e.POST("/api/sql/run", func(c echo.Context) error {
		project, err := projectFromRequest(c, store)
		if err != nil {
			return err
		}
		var payload struct {
			SQL string `json:"sql"`
		}
		if err := c.Bind(&payload); err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, "invalid json")
		}
		rows, err := store.RunSQL(c.Request().Context(), project.ID, payload.SQL)
		if err != nil {
			// Surface the underlying SQL error (e.g. ClickHouse syntax/column
			// errors) to the author instead of Echo's generic 500 — the SQL
			// screen shows this message inline so users can fix their query.
			return echo.NewHTTPError(http.StatusBadRequest, err.Error())
		}
		return c.JSON(http.StatusOK, map[string]any{"rows": rows, "generated_at": time.Now().UTC()})
	})

	e.GET("/api/dashboards", func(c echo.Context) error {
		project, err := projectFromRequest(c, store)
		if err != nil {
			return err
		}
		dashboards, err := store.ListDashboards(c.Request().Context(), project.ID)
		if err != nil {
			return err
		}
		return c.JSON(http.StatusOK, map[string]any{
			"project":    project,
			"dashboards": dashboards,
		})
	})

	e.POST("/api/dashboards", func(c echo.Context) error {
		project, err := projectFromRequest(c, store)
		if err != nil {
			return err
		}
		var payload struct {
			Name        string `json:"name"`
			Description string `json:"description"`
		}
		if err := c.Bind(&payload); err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, "invalid json")
		}
		dashboard, err := store.CreateDashboard(c.Request().Context(), project.ID, strings.TrimSpace(payload.Name), strings.TrimSpace(payload.Description))
		if err != nil {
			return err
		}
		return c.JSON(http.StatusCreated, map[string]any{"dashboard": dashboard})
	})

	e.PUT("/api/dashboards/:dashboard_id", func(c echo.Context) error {
		project, err := projectFromRequest(c, store)
		if err != nil {
			return err
		}
		var payload struct {
			Name        string `json:"name"`
			Description string `json:"description"`
		}
		if err := c.Bind(&payload); err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, "invalid json")
		}
		dashboard, err := store.UpdateDashboard(c.Request().Context(), project.ID, c.Param("dashboard_id"), strings.TrimSpace(payload.Name), strings.TrimSpace(payload.Description))
		if err != nil {
			return err
		}
		return c.JSON(http.StatusOK, map[string]any{"dashboard": dashboard})
	})

	e.DELETE("/api/dashboards/:dashboard_id", func(c echo.Context) error {
		project, err := projectFromRequest(c, store)
		if err != nil {
			return err
		}
		if err := store.DeleteDashboard(c.Request().Context(), project.ID, c.Param("dashboard_id")); err != nil {
			return err
		}
		return c.NoContent(http.StatusNoContent)
	})

	e.GET("/api/dashboards/:dashboard_id/charts", func(c echo.Context) error {
		project, err := projectFromRequest(c, store)
		if err != nil {
			return err
		}
		charts, err := store.ListCharts(c.Request().Context(), project.ID, c.Param("dashboard_id"))
		if err != nil {
			return err
		}
		return c.JSON(http.StatusOK, map[string]any{
			"project": project,
			"charts":  charts,
		})
	})

	e.POST("/api/dashboards/:dashboard_id/charts", func(c echo.Context) error {
		project, err := projectFromRequest(c, store)
		if err != nil {
			return err
		}
		chart, err := chartFromRequest(c)
		if err != nil {
			return err
		}
		chart.ProjectID = project.ID
		chart.DashboardID = c.Param("dashboard_id")
		created, err := store.CreateChart(c.Request().Context(), chart)
		if err != nil {
			return err
		}
		return c.JSON(http.StatusCreated, map[string]any{"chart": created})
	})

	e.PUT("/api/charts/:chart_id", func(c echo.Context) error {
		project, err := projectFromRequest(c, store)
		if err != nil {
			return err
		}
		chart, err := chartFromRequest(c)
		if err != nil {
			return err
		}
		chart.ProjectID = project.ID
		chart.ID = c.Param("chart_id")
		updated, err := store.UpdateChart(c.Request().Context(), chart)
		if err != nil {
			return err
		}
		return c.JSON(http.StatusOK, map[string]any{"chart": updated})
	})

	e.DELETE("/api/charts/:chart_id", func(c echo.Context) error {
		project, err := projectFromRequest(c, store)
		if err != nil {
			return err
		}
		if err := store.DeleteChart(c.Request().Context(), project.ID, c.Param("chart_id")); err != nil {
			return err
		}
		return c.NoContent(http.StatusNoContent)
	})

	e.PUT("/api/dashboards/:dashboard_id/charts/order", func(c echo.Context) error {
		project, err := projectFromRequest(c, store)
		if err != nil {
			return err
		}
		var payload struct {
			ChartIDs []string `json:"chart_ids"`
		}
		if err := c.Bind(&payload); err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, "invalid json")
		}
		if err := store.ReorderCharts(c.Request().Context(), project.ID, c.Param("dashboard_id"), payload.ChartIDs); err != nil {
			return err
		}
		return c.NoContent(http.StatusNoContent)
	})

	e.GET("/api/events", func(c echo.Context) error {
		project, err := projectFromRequest(c, store)
		if err != nil {
			return err
		}
		limit := intParam(c, "limit", 50, 1, 500)
		events, err := store.RecentEvents(c.Request().Context(), project.ID, limit)
		if err != nil {
			return err
		}
		return c.JSON(http.StatusOK, map[string]any{
			"project_id": project.ID,
			"events":     events,
			"checked_at": time.Now().UTC(),
		})
	})

	e.GET("/api/sessions", func(c echo.Context) error {
		project, err := projectFromRequest(c, store)
		if err != nil {
			return err
		}
		limit := intParam(c, "limit", 50, 1, 500)
		sessions, err := store.RecentSessions(c.Request().Context(), project.ID, limit)
		if err != nil {
			return err
		}
		return c.JSON(http.StatusOK, map[string]any{
			"project_id": project.ID,
			"sessions":   sessions,
			"checked_at": time.Now().UTC(),
		})
	})
}

func projectFromRequest(c echo.Context, store *storage.Store) (storage.Project, error) {
	apiKey := firstNonEmpty(c.QueryParam("api_key"), c.QueryParam("token"), c.Request().Header.Get("X-API-Key"))
	if apiKey != "" {
		project, err := store.ProjectByAPIKey(c.Request().Context(), apiKey)
		if err != nil {
			return storage.Project{}, echo.NewHTTPError(http.StatusUnauthorized, "invalid api key")
		}
		return project, nil
	}
	ctx, err := authFromRequest(c, store)
	if err != nil {
		return storage.Project{}, err
	}
	projectID := firstNonEmpty(c.QueryParam("project_id"), c.Param("project_id"))
	if projectID != "" {
		project, err := store.ProjectByIDForUser(c.Request().Context(), ctx.User.ID, projectID)
		if err != nil {
			return storage.Project{}, echo.NewHTTPError(http.StatusForbidden, "project not available")
		}
		return project, nil
	}
	project, err := store.DefaultProjectForUser(c.Request().Context(), ctx.User.ID)
	if err != nil {
		return storage.Project{}, echo.NewHTTPError(http.StatusNotFound, "project not found")
	}
	return project, nil
}

func accountResources(c echo.Context, store *storage.Store, ctx authContext, preferredProjectID string) ([]storage.Workspace, []storage.Project, storage.Project, error) {
	workspaces, err := store.ListUserWorkspaces(c.Request().Context(), ctx.User.ID)
	if err != nil {
		return nil, nil, storage.Project{}, err
	}
	if len(workspaces) == 0 {
		return workspaces, []storage.Project{}, storage.Project{}, nil
	}
	workspaceID := workspaces[0].ID
	if preferredProjectID != "" {
		if project, err := store.ProjectByIDForUser(c.Request().Context(), ctx.User.ID, preferredProjectID); err == nil {
			workspaceID = project.WorkspaceID
		}
	}
	for _, workspace := range workspaces {
		if c.QueryParam("workspace_id") == workspace.ID {
			workspaceID = workspace.ID
			break
		}
	}
	projects, err := store.ListWorkspaceProjects(c.Request().Context(), ctx.User.ID, workspaceID)
	if err != nil {
		return nil, nil, storage.Project{}, err
	}
	var project storage.Project
	if preferredProjectID != "" {
		for _, item := range projects {
			if item.ID == preferredProjectID {
				project = item
				break
			}
		}
	}
	if project.ID == "" && len(projects) > 0 {
		project = projects[0]
	}
	return workspaces, projects, project, nil
}

func chartFromRequest(c echo.Context) (storage.Chart, error) {
	var payload struct {
		Name      string `json:"name"`
		Kind      string `json:"kind"`
		Metric    string `json:"metric"`
		EventName string `json:"event_name"`
		EventType string `json:"event_type"`
		SQL       string `json:"sql"`
		XField    string `json:"x_field"`
		YField    string `json:"y_field"`
		ColSpan   int    `json:"col_span"`
	}
	if err := c.Bind(&payload); err != nil {
		return storage.Chart{}, echo.NewHTTPError(http.StatusBadRequest, "invalid json")
	}
	return storage.Chart{
		Name:      strings.TrimSpace(payload.Name),
		Kind:      strings.TrimSpace(payload.Kind),
		Metric:    strings.TrimSpace(payload.Metric),
		EventName: strings.TrimSpace(payload.EventName),
		EventType: strings.TrimSpace(payload.EventType),
		SQL:       strings.TrimSpace(payload.SQL),
		XField:    strings.TrimSpace(payload.XField),
		YField:    strings.TrimSpace(payload.YField),
		ColSpan:   payload.ColSpan,
	}, nil
}

func intParam(c echo.Context, name string, fallback int, minValue int, maxValue int) int {
	value := fallback
	if raw := c.QueryParam(name); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil {
			value = parsed
		}
	}
	if value < minValue {
		return minValue
	}
	if value > maxValue {
		return maxValue
	}
	return value
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func filterFromRequest(c echo.Context) storage.EventFilter {
	filter := storage.EventFilter{
		EventType:  strings.TrimSpace(c.QueryParam("event_type")),
		EventName:  strings.TrimSpace(c.QueryParam("event_name")),
		DistinctID: strings.TrimSpace(c.QueryParam("distinct_id")),
		SessionID:  strings.TrimSpace(c.QueryParam("session_id")),
		AgentID:    strings.TrimSpace(c.QueryParam("agent_id")),
		ModelName:  strings.TrimSpace(c.QueryParam("model_name")),
		Search:     strings.TrimSpace(c.QueryParam("search")),
		ErrorOnly:  c.QueryParam("error_only") == "true" || c.QueryParam("error_only") == "1",
		Limit:      intParam(c, "limit", 100, 1, 500),
	}
	if raw := c.QueryParam("from"); raw != "" {
		if parsed, err := time.Parse(time.RFC3339, raw); err == nil {
			filter.From = parsed.UTC()
		}
	}
	if raw := c.QueryParam("to"); raw != "" {
		if parsed, err := time.Parse(time.RFC3339, raw); err == nil {
			filter.To = parsed.UTC()
		}
	}
	if filter.From.IsZero() && filter.To.IsZero() {
		hours := intParam(c, "hours", 24, 1, 24*90)
		filter.From = time.Now().UTC().Add(-time.Duration(hours) * time.Hour)
		filter.To = time.Now().UTC()
	}
	return filter
}

func splitCSV(value string) []string {
	parts := strings.Split(value, ",")
	out := []string{}
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}
