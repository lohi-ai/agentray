package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/lohi-ai/agentray/agentcore"
	"github.com/lohi-ai/agentray/internal/agentruntime"
	"github.com/lohi-ai/agentray/sandbox"
	"github.com/lohi-ai/agentray/internal/storage"
)

// detachedRunCeiling bounds a chat run that has outlived its SSE connection (the
// user navigated away). The run finishes on a context independent of the request,
// persisting its terminal status + answer; this is the hard ceiling past which it
// is abandoned. Matches the scheduler's per-run budget.
const detachedRunCeiling = 10 * time.Minute

// registerAgentRoutes wires the AI-agent surface (§8, §14.10): config +
// definition + skills + memory CRUD, a key test, interactive chat, run history,
// recommendations, and a manual run trigger.
func registerAgentRoutes(e *echo.Echo, store *storage.Store, scheduler *agentruntime.Scheduler, sb agentcore.Sandbox, ws *sandbox.Workspace, liveReg *agentruntime.LiveRegistry, runnerOpts ...agentruntime.RunnerOption) {
	// --- config ---
	e.GET("/api/agent/config", func(c echo.Context) error {
		ctx, project, err := authProject(c, store)
		if err != nil {
			return err
		}
		cfg, err := store.GetAgentConfig(c.Request().Context(), ctx.User.ID, project.ID)
		if err != nil {
			return err
		}
		return c.JSON(http.StatusOK, map[string]any{"config": cfg})
	})

	e.PUT("/api/agent/config", func(c echo.Context) error {
		ctx, project, err := authProject(c, store)
		if err != nil {
			return err
		}
		var payload struct {
			Enabled      bool            `json:"enabled"`
			RedactPII    bool            `json:"redact_pii"`
			Scopes       map[string]bool `json:"scopes"`
			Autonomy     string          `json:"autonomy"`
			ScheduleCron string          `json:"schedule_cron"`
		}
		if err := c.Bind(&payload); err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, "invalid json")
		}
		cfg, err := store.UpsertAgentConfig(c.Request().Context(), ctx.User.ID, project.ID, storage.AgentConfigInput{
			Enabled: payload.Enabled, RedactPII: payload.RedactPII, Scopes: payload.Scopes,
			Autonomy: payload.Autonomy, ScheduleCron: payload.ScheduleCron,
		})
		if errors.Is(err, storage.ErrInvalidAutonomy) {
			return echo.NewHTTPError(http.StatusBadRequest, err.Error())
		}
		if err != nil {
			return echo.NewHTTPError(http.StatusForbidden, err.Error())
		}
		return c.JSON(http.StatusOK, map[string]any{"config": cfg})
	})

	// --- workspace model pool (the 3 tiers, configured once per workspace) ---
	e.GET("/api/workspace/models", func(c echo.Context) error {
		ctx, project, err := authProject(c, store)
		if err != nil {
			return err
		}
		cfg, err := store.GetWorkspaceModelTiers(c.Request().Context(), ctx.User.ID, project.WorkspaceID)
		if err != nil {
			return echo.NewHTTPError(http.StatusForbidden, err.Error())
		}
		return c.JSON(http.StatusOK, map[string]any{"config": cfg})
	})

	e.PUT("/api/workspace/models", func(c echo.Context) error {
		ctx, project, err := authProject(c, store)
		if err != nil {
			return err
		}
		var payload struct {
			Provider string `json:"provider"`
			Model    string `json:"model"`
			BaseURL  string `json:"base_url"`
			APIKey   string `json:"api_key"`

			LiteProvider  string `json:"lite_provider"`
			LiteModel     string `json:"lite_model"`
			LiteBaseURL   string `json:"lite_base_url"`
			LiteAPIKey    string `json:"lite_api_key"`
			ProProvider   string `json:"pro_provider"`
			ProModel      string `json:"pro_model"`
			ProBaseURL    string `json:"pro_base_url"`
			ProAPIKey     string `json:"pro_api_key"`
			ModelFallback bool   `json:"model_fallback"`
		}
		if err := c.Bind(&payload); err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, "invalid json")
		}
		cfg, err := store.UpsertWorkspaceModelTiers(c.Request().Context(), ctx.User.ID, project.WorkspaceID, storage.WorkspaceModelTiersInput{
			Provider: payload.Provider, Model: payload.Model, BaseURL: payload.BaseURL, APIKey: payload.APIKey,
			LiteProvider: payload.LiteProvider, LiteModel: payload.LiteModel,
			LiteBaseURL: payload.LiteBaseURL, LiteAPIKey: payload.LiteAPIKey,
			ProProvider: payload.ProProvider, ProModel: payload.ProModel,
			ProBaseURL: payload.ProBaseURL, ProAPIKey: payload.ProAPIKey,
			ModelFallback: payload.ModelFallback,
		})
		if err != nil {
			return echo.NewHTTPError(http.StatusForbidden, err.Error())
		}
		return c.JSON(http.StatusOK, map[string]any{"config": cfg})
	})

	e.POST("/api/workspace/models/test", func(c echo.Context) error {
		ctx, project, err := authProject(c, store)
		if err != nil {
			return err
		}
		if _, err := store.GetWorkspaceModelTiers(c.Request().Context(), ctx.User.ID, project.WorkspaceID); err != nil {
			return echo.NewHTTPError(http.StatusForbidden, err.Error())
		}
		cfg, keys, err := store.WorkspaceTiersForRun(c.Request().Context(), project.WorkspaceID)
		if err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, err.Error())
		}
		// Test flash plus any lite/pro tier with its own override, applying the same
		// merge a run uses (blank fields inherit flash, including the key). Each entry
		// reports {ok, error} so the UI can flag the exact failing tier.
		results := make(map[string]any, 3)
		allOK := true
		test := func(name, provider, model, baseURL, key string) {
			provider = firstNonEmpty(provider, cfg.Provider)
			model = firstNonEmpty(model, cfg.Model)
			baseURL = firstNonEmpty(baseURL, cfg.BaseURL)
			key = firstNonEmpty(key, keys["flash"])
			res := testTierProvider(c, provider, baseURL, model, key)
			results[name] = res
			if ok, _ := res["ok"].(bool); !ok {
				allOK = false
			}
		}
		test("flash", cfg.Provider, cfg.Model, cfg.BaseURL, keys["flash"])
		if cfg.LiteProvider != "" || cfg.LiteModel != "" || cfg.LiteBaseURL != "" || keys["lite"] != "" {
			test("lite", cfg.LiteProvider, cfg.LiteModel, cfg.LiteBaseURL, keys["lite"])
		}
		if cfg.ProProvider != "" || cfg.ProModel != "" || cfg.ProBaseURL != "" || keys["pro"] != "" {
			test("pro", cfg.ProProvider, cfg.ProModel, cfg.ProBaseURL, keys["pro"])
		}
		return c.JSON(http.StatusOK, map[string]any{"ok": allOK, "tiers": results})
	})

	// --- per-agent capabilities (which usecase/analytics tool groups are allowed) ---
	e.GET("/api/agent/capabilities", func(c echo.Context) error {
		ctx, project, err := authProject(c, store)
		if err != nil {
			return err
		}
		caps, err := store.GetAgentCapabilities(c.Request().Context(), ctx.User.ID, project.ID, c.QueryParam("agent"))
		if err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, err.Error())
		}
		return c.JSON(http.StatusOK, map[string]any{"capabilities": caps})
	})

	e.PUT("/api/agent/capabilities", func(c echo.Context) error {
		ctx, project, err := authProject(c, store)
		if err != nil {
			return err
		}
		var payload struct {
			Scopes map[string]bool `json:"scopes"`
		}
		if err := c.Bind(&payload); err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, "invalid json")
		}
		caps, err := store.UpsertAgentCapabilities(c.Request().Context(), ctx.User.ID, project.ID, c.QueryParam("agent"), payload.Scopes)
		if err != nil {
			return echo.NewHTTPError(http.StatusForbidden, err.Error())
		}
		return c.JSON(http.StatusOK, map[string]any{"capabilities": caps})
	})

	// --- per-agent task→tier map (which workspace tier each task kind uses) ---
	e.GET("/api/agent/task-tiers", func(c echo.Context) error {
		ctx, project, err := authProject(c, store)
		if err != nil {
			return err
		}
		tiers, err := store.GetAgentTaskTiers(c.Request().Context(), ctx.User.ID, project.ID, c.QueryParam("agent"))
		if err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, err.Error())
		}
		return c.JSON(http.StatusOK, map[string]any{"tiers": tiers})
	})

	e.PUT("/api/agent/task-tiers", func(c echo.Context) error {
		ctx, project, err := authProject(c, store)
		if err != nil {
			return err
		}
		var payload struct {
			Tiers storage.AgentTaskTiers `json:"tiers"`
		}
		if err := c.Bind(&payload); err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, "invalid json")
		}
		tiers, err := store.UpsertAgentTaskTiers(c.Request().Context(), ctx.User.ID, project.ID, c.QueryParam("agent"), payload.Tiers)
		if err != nil {
			return echo.NewHTTPError(http.StatusForbidden, err.Error())
		}
		return c.JSON(http.StatusOK, map[string]any{"tiers": tiers})
	})

	// --- definition (SOUL + AGENTS) ---
	e.GET("/api/agent/definition", func(c echo.Context) error {
		ctx, project, err := authProject(c, store)
		if err != nil {
			return err
		}
		def, err := store.GetAgentDefinition(c.Request().Context(), ctx.User.ID, project.ID, c.QueryParam("agent"))
		if err != nil {
			return err
		}
		return c.JSON(http.StatusOK, map[string]any{"definition": def})
	})

	e.PUT("/api/agent/definition", func(c echo.Context) error {
		ctx, project, err := authProject(c, store)
		if err != nil {
			return err
		}
		var payload struct {
			SoulMD   string `json:"soul_md"`
			AgentsMD string `json:"agents_md"`
		}
		if err := c.Bind(&payload); err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, "invalid json")
		}
		def, err := store.UpsertAgentDefinition(c.Request().Context(), ctx.User.ID, project.ID, c.QueryParam("agent"), payload.SoulMD, payload.AgentsMD)
		if err != nil {
			return echo.NewHTTPError(http.StatusForbidden, err.Error())
		}
		return c.JSON(http.StatusOK, map[string]any{"definition": def})
	})

	e.POST("/api/agent/definition/generate", func(c echo.Context) error {
		ctx, project, err := authProject(c, store)
		if err != nil {
			return err
		}
		var payload struct {
			Prompt string `json:"prompt"`
		}
		if err := c.Bind(&payload); err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, "invalid json")
		}
		prompt := strings.TrimSpace(payload.Prompt)
		if prompt == "" {
			return echo.NewHTTPError(http.StatusBadRequest, "prompt required")
		}
		canManage, err := store.UserCanManageWorkspace(c.Request().Context(), ctx.User.ID, project.WorkspaceID)
		if err != nil {
			return err
		}
		if !canManage {
			return echo.NewHTTPError(http.StatusForbidden, "agent config permission denied")
		}
		cfg, keys, err := store.WorkspaceTiersForRun(c.Request().Context(), project.WorkspaceID)
		if err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, err.Error())
		}
		pro := agentruntime.TierSetFromWorkspace(cfg, keys).Resolve(agentruntime.DefaultAuthoringTier)
		if strings.TrimSpace(pro.Provider) == "" {
			pro.Provider = cfg.Provider
		}
		if strings.TrimSpace(pro.BaseURL) == "" {
			pro.BaseURL = cfg.BaseURL
		}
		if pro.APIKey == "" {
			pro.APIKey = keys["flash"]
		}
		if strings.TrimSpace(pro.Model) == "" {
			pro.Model = cfg.Model
		}
		if strings.TrimSpace(pro.Model) == "" || strings.TrimSpace(pro.APIKey) == "" {
			return echo.NewHTTPError(http.StatusBadRequest, "pro model tier is not configured")
		}
		provider, err := agentruntime.NewTierProvider(pro.Provider, pro.BaseURL, pro.APIKey)
		if err != nil {
			return echo.NewHTTPError(http.StatusBadGateway, err.Error())
		}
		draft, err := agentcore.DraftDefinition(c.Request().Context(), provider, pro.Model, prompt)
		if err != nil {
			return echo.NewHTTPError(http.StatusBadGateway, err.Error())
		}
		return c.JSON(http.StatusOK, map[string]any{"definition": draft})
	})

	// --- skills (§14.3, §14.10) ---
	e.GET("/api/agent/skills", func(c echo.Context) error {
		ctx, project, err := authProject(c, store)
		if err != nil {
			return err
		}
		skills, err := store.ListAgentSkills(c.Request().Context(), ctx.User.ID, project.ID, c.QueryParam("agent"))
		if err != nil {
			return err
		}
		return c.JSON(http.StatusOK, map[string]any{"skills": skills})
	})

	e.POST("/api/agent/skills", func(c echo.Context) error {
		ctx, project, err := authProject(c, store)
		if err != nil {
			return err
		}
		var sk storage.AgentSkill
		if err := c.Bind(&sk); err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, "invalid json")
		}
		sk.ID = ""
		out, err := store.UpsertAgentSkill(c.Request().Context(), ctx.User.ID, project.ID, c.QueryParam("agent"), sk)
		if err != nil {
			return echo.NewHTTPError(http.StatusForbidden, err.Error())
		}
		return c.JSON(http.StatusOK, map[string]any{"skill": out})
	})

	e.PUT("/api/agent/skills/:id", func(c echo.Context) error {
		ctx, project, err := authProject(c, store)
		if err != nil {
			return err
		}
		var sk storage.AgentSkill
		if err := c.Bind(&sk); err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, "invalid json")
		}
		sk.ID = c.Param("id")
		out, err := store.UpsertAgentSkill(c.Request().Context(), ctx.User.ID, project.ID, c.QueryParam("agent"), sk)
		if err != nil {
			return echo.NewHTTPError(http.StatusForbidden, err.Error())
		}
		return c.JSON(http.StatusOK, map[string]any{"skill": out})
	})

	e.DELETE("/api/agent/skills/:id", func(c echo.Context) error {
		ctx, project, err := authProject(c, store)
		if err != nil {
			return err
		}
		if err := store.DeleteAgentSkill(c.Request().Context(), ctx.User.ID, project.ID, c.QueryParam("agent"), c.Param("id")); err != nil {
			return echo.NewHTTPError(http.StatusForbidden, err.Error())
		}
		return c.NoContent(http.StatusNoContent)
	})

	e.POST("/api/agent/skills/:id/approve", func(c echo.Context) error {
		ctx, project, err := authProject(c, store)
		if err != nil {
			return err
		}
		if err := store.ApproveAgentSkill(c.Request().Context(), ctx.User.ID, project.ID, c.QueryParam("agent"), c.Param("id")); err != nil {
			return echo.NewHTTPError(http.StatusForbidden, err.Error())
		}
		return c.JSON(http.StatusOK, map[string]any{"ok": true})
	})

	// --- secrets (AgentGarden §5): names-only reads, write-only values ---
	e.GET("/api/agent/secrets", func(c echo.Context) error {
		ctx, project, err := authProject(c, store)
		if err != nil {
			return err
		}
		names, err := store.ListAgentSecretNames(c.Request().Context(), ctx.User.ID, project.ID, c.QueryParam("agent"))
		if err != nil {
			return err
		}
		return c.JSON(http.StatusOK, map[string]any{"names": names})
	})

	e.PUT("/api/agent/secrets/:name", func(c echo.Context) error {
		ctx, project, err := authProject(c, store)
		if err != nil {
			return err
		}
		var payload struct {
			Value string `json:"value"`
		}
		if err := c.Bind(&payload); err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, "invalid json")
		}
		if err := store.UpsertAgentSecret(c.Request().Context(), ctx.User.ID, project.ID, c.QueryParam("agent"), c.Param("name"), payload.Value); err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, err.Error())
		}
		return c.JSON(http.StatusOK, map[string]any{"ok": true})
	})

	e.DELETE("/api/agent/secrets/:name", func(c echo.Context) error {
		ctx, project, err := authProject(c, store)
		if err != nil {
			return err
		}
		if err := store.DeleteAgentSecret(c.Request().Context(), ctx.User.ID, project.ID, c.QueryParam("agent"), c.Param("name")); err != nil {
			return echo.NewHTTPError(http.StatusForbidden, err.Error())
		}
		return c.NoContent(http.StatusNoContent)
	})

	// --- agents (AgentGarden §3): first-class agent identity per project ---
	e.GET("/api/agent/agents", func(c echo.Context) error {
		ctx, project, err := authProject(c, store)
		if err != nil {
			return err
		}
		agents, err := store.ListAgents(c.Request().Context(), ctx.User.ID, project.ID)
		if err != nil {
			return err
		}
		return c.JSON(http.StatusOK, map[string]any{"agents": agents})
	})

	e.POST("/api/agent/agents", func(c echo.Context) error {
		ctx, project, err := authProject(c, store)
		if err != nil {
			return err
		}
		var payload struct {
			Name string `json:"name"`
			Slug string `json:"slug"`
		}
		if err := c.Bind(&payload); err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, "invalid json")
		}
		agent, err := store.CreateAgent(c.Request().Context(), ctx.User.ID, project.ID, payload.Name, payload.Slug)
		if err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, err.Error())
		}
		return c.JSON(http.StatusCreated, agent)
	})

	// --- marketplace (foundation agent presets): one-click "first hire" ---
	e.GET("/api/marketplace/agents", func(c echo.Context) error {
		if _, _, err := authProject(c, store); err != nil {
			return err
		}
		return c.JSON(http.StatusOK, map[string]any{"agents": storage.AgentPresets()})
	})

	e.POST("/api/marketplace/agents/:slug/install", func(c echo.Context) error {
		ctx, project, err := authProject(c, store)
		if err != nil {
			return err
		}
		agent, err := store.InstallAgentPreset(c.Request().Context(), ctx.User.ID, project.ID, c.Param("slug"))
		if err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, err.Error())
		}
		return c.JSON(http.StatusCreated, map[string]any{"agent": agent})
	})

	e.PUT("/api/agent/agents/:id", func(c echo.Context) error {
		ctx, project, err := authProject(c, store)
		if err != nil {
			return err
		}
		var payload struct {
			Name    string `json:"name"`
			Enabled bool   `json:"enabled"`
		}
		if err := c.Bind(&payload); err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, "invalid json")
		}
		agent, err := store.UpdateAgent(c.Request().Context(), ctx.User.ID, project.ID, c.Param("id"), payload.Name, payload.Enabled)
		if err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, err.Error())
		}
		return c.JSON(http.StatusOK, agent)
	})

	e.DELETE("/api/agent/agents/:id", func(c echo.Context) error {
		ctx, project, err := authProject(c, store)
		if err != nil {
			return err
		}
		if err := store.DeleteAgent(c.Request().Context(), ctx.User.ID, project.ID, c.Param("id")); err != nil {
			return echo.NewHTTPError(http.StatusForbidden, err.Error())
		}
		return c.NoContent(http.StatusNoContent)
	})

	// --- agent grants (workspace-owned agents assigned into projects) ---
	// The workspace owns agents; a grant assigns one into a project (a product)
	// with a per-project scope cap. These power "assign this agent to a product".
	e.GET("/api/agent/workspace-agents", func(c echo.Context) error {
		ctx, project, err := authProject(c, store)
		if err != nil {
			return err
		}
		agents, err := store.ListWorkspaceAgents(c.Request().Context(), ctx.User.ID, project.WorkspaceID)
		if err != nil {
			return err
		}
		return c.JSON(http.StatusOK, map[string]any{"agents": agents})
	})

	e.GET("/api/agent/agents/:id/grants", func(c echo.Context) error {
		ctx, _, err := authProject(c, store)
		if err != nil {
			return err
		}
		grants, err := store.ListAgentGrants(c.Request().Context(), ctx.User.ID, c.Param("id"))
		if err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, err.Error())
		}
		return c.JSON(http.StatusOK, map[string]any{"grants": grants})
	})

	e.POST("/api/agent/agents/:id/grant", func(c echo.Context) error {
		ctx, project, err := authProject(c, store)
		if err != nil {
			return err
		}
		var payload struct {
			Scopes map[string]bool `json:"scopes"`
		}
		if err := c.Bind(&payload); err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, "invalid json")
		}
		grant, err := store.GrantAgentToProject(c.Request().Context(), ctx.User.ID, c.Param("id"), project.ID, payload.Scopes)
		if err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, err.Error())
		}
		return c.JSON(http.StatusCreated, map[string]any{"grant": grant})
	})

	e.DELETE("/api/agent/agents/:id/grant", func(c echo.Context) error {
		ctx, project, err := authProject(c, store)
		if err != nil {
			return err
		}
		if err := store.RevokeAgentFromProject(c.Request().Context(), ctx.User.ID, c.Param("id"), project.ID); err != nil {
			return echo.NewHTTPError(http.StatusForbidden, err.Error())
		}
		return c.NoContent(http.StatusNoContent)
	})

	// --- selectable tools (AgentGarden §6): catalog + per-agent selections ---
	e.GET("/api/agent/tools", func(c echo.Context) error {
		ctx, project, err := authProject(c, store)
		if err != nil {
			return err
		}
		selections, err := store.ListAgentTools(c.Request().Context(), ctx.User.ID, project.ID, c.QueryParam("agent"))
		if err != nil {
			return err
		}
		toolCtx := agentruntime.ToolBuildContext{Sandbox: sb, Workspace: ws}
		return c.JSON(http.StatusOK, map[string]any{
			"catalog":    agentruntime.ToolCatalog(toolCtx),
			"selections": selections,
		})
	})

	e.PUT("/api/agent/tools/:name", func(c echo.Context) error {
		ctx, project, err := authProject(c, store)
		if err != nil {
			return err
		}
		name := c.Param("name")
		if !agentruntime.IsRegisteredTool(name) {
			return echo.NewHTTPError(http.StatusBadRequest, "unknown tool")
		}
		var payload struct {
			Enabled bool            `json:"enabled"`
			Config  json.RawMessage `json:"config"`
		}
		if err := c.Bind(&payload); err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, "invalid json")
		}
		config := strings.TrimSpace(string(payload.Config))
		if config == "" {
			config = "{}"
		}
		// Validate the config eagerly when enabling, so an unusable selection (e.g.
		// an empty http_request allowlist or unavailable sandbox) is rejected at write
		// time instead of failing the next run closed.
		if payload.Enabled {
			if _, err := agentruntime.BuildToolWithContext(agentruntime.ToolBuildContext{Sandbox: sb, Workspace: ws}, name, config); err != nil {
				return echo.NewHTTPError(http.StatusBadRequest, err.Error())
			}
		}
		if err := store.UpsertAgentTool(c.Request().Context(), ctx.User.ID, project.ID, c.QueryParam("agent"), name, payload.Enabled, config); err != nil {
			return echo.NewHTTPError(http.StatusForbidden, err.Error())
		}
		return c.JSON(http.StatusOK, map[string]any{"ok": true})
	})

	e.DELETE("/api/agent/tools/:name", func(c echo.Context) error {
		ctx, project, err := authProject(c, store)
		if err != nil {
			return err
		}
		if err := store.DeleteAgentTool(c.Request().Context(), ctx.User.ID, project.ID, c.QueryParam("agent"), c.Param("name")); err != nil {
			return echo.NewHTTPError(http.StatusForbidden, err.Error())
		}
		return c.NoContent(http.StatusNoContent)
	})

	// --- delegation grants (agent teams): which other agents this agent may
	// hand tasks to via spawn_subagent's agent parameter. Self-delegation is
	// built into the harness; the grant list only covers teammates. GET bundles
	// the project's agent roster so the settings UI renders candidates plus the
	// current selections from one call.
	e.GET("/api/agent/delegates", func(c echo.Context) error {
		ctx, project, err := authProject(c, store)
		if err != nil {
			return err
		}
		selections, err := store.ListAgentDelegates(c.Request().Context(), ctx.User.ID, project.ID, c.QueryParam("agent"))
		if err != nil {
			return echo.NewHTTPError(http.StatusForbidden, err.Error())
		}
		agents, err := store.ListAgents(c.Request().Context(), ctx.User.ID, project.ID)
		if err != nil {
			return err
		}
		return c.JSON(http.StatusOK, map[string]any{
			"agents":     agents,
			"selections": selections,
		})
	})

	e.PUT("/api/agent/delegates/:id", func(c echo.Context) error {
		ctx, project, err := authProject(c, store)
		if err != nil {
			return err
		}
		var payload struct {
			Enabled bool `json:"enabled"`
		}
		if err := c.Bind(&payload); err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, "invalid json")
		}
		if err := store.UpsertAgentDelegate(c.Request().Context(), ctx.User.ID, project.ID, c.QueryParam("agent"), c.Param("id"), payload.Enabled); err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, err.Error())
		}
		return c.JSON(http.StatusOK, map[string]any{"ok": true})
	})

	e.DELETE("/api/agent/delegates/:id", func(c echo.Context) error {
		ctx, project, err := authProject(c, store)
		if err != nil {
			return err
		}
		if err := store.DeleteAgentDelegate(c.Request().Context(), ctx.User.ID, project.ID, c.QueryParam("agent"), c.Param("id")); err != nil {
			return echo.NewHTTPError(http.StatusForbidden, err.Error())
		}
		return c.NoContent(http.StatusNoContent)
	})

	// --- per-agent budgets & quotas (#4) ---
	// GET returns the agent's own budget rows plus its current spend + effective
	// (resolved incl. workspace default) status, so the setup page can draw a
	// budget bar without a second call.
	e.GET("/api/agent/budgets", func(c echo.Context) error {
		ctx, project, err := authProject(c, store)
		if err != nil {
			return err
		}
		agentID := c.QueryParam("agent")
		budgets, err := store.GetAgentBudget(c.Request().Context(), ctx.User.ID, project.ID, agentID)
		if err != nil {
			return echo.NewHTTPError(http.StatusForbidden, err.Error())
		}
		scopeID, serr := store.AgentScopeForRun(c.Request().Context(), project.ID, agentID)
		var status storage.BudgetStatus
		if serr == nil {
			wsID, _ := store.WorkspaceIDForProject(c.Request().Context(), project.ID)
			status, _ = store.BudgetStatusForRun(c.Request().Context(), scopeID, wsID, "day")
		}
		return c.JSON(http.StatusOK, map[string]any{"budgets": budgets, "status": status})
	})

	e.PUT("/api/agent/budgets", func(c echo.Context) error {
		ctx, project, err := authProject(c, store)
		if err != nil {
			return err
		}
		var payload storage.AgentBudget
		if err := c.Bind(&payload); err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, "invalid json")
		}
		out, err := store.UpsertAgentBudget(c.Request().Context(), ctx.User.ID, project.ID, c.QueryParam("agent"), payload)
		if err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, err.Error())
		}
		return c.JSON(http.StatusOK, out)
	})

	e.DELETE("/api/agent/budgets/:period", func(c echo.Context) error {
		ctx, project, err := authProject(c, store)
		if err != nil {
			return err
		}
		if err := store.DeleteAgentBudget(c.Request().Context(), ctx.User.ID, project.ID, c.QueryParam("agent"), c.Param("period")); err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, err.Error())
		}
		return c.NoContent(http.StatusNoContent)
	})

	// --- long-term memory (§14.7, §14.10) ---
	e.GET("/api/agent/memory", func(c echo.Context) error {
		ctx, project, err := authProject(c, store)
		if err != nil {
			return err
		}
		mem, err := store.ListAgentMemory(c.Request().Context(), ctx.User.ID, project.ID, intParam(c, "limit", 50, 1, 200))
		if err != nil {
			return err
		}
		return c.JSON(http.StatusOK, map[string]any{"memory": mem})
	})

	e.DELETE("/api/agent/memory/:id", func(c echo.Context) error {
		ctx, project, err := authProject(c, store)
		if err != nil {
			return err
		}
		if err := store.DeleteAgentMemory(c.Request().Context(), ctx.User.ID, project.ID, c.Param("id")); err != nil {
			return echo.NewHTTPError(http.StatusForbidden, err.Error())
		}
		return c.NoContent(http.StatusNoContent)
	})

	// --- runs (§8) ---
	e.GET("/api/agent/runs", func(c echo.Context) error {
		ctx, project, err := authProject(c, store)
		if err != nil {
			return err
		}
		runs, err := store.ListAgentRuns(c.Request().Context(), ctx.User.ID, project.ID, intParam(c, "limit", 50, 1, 200))
		if err != nil {
			return err
		}
		return c.JSON(http.StatusOK, map[string]any{"runs": runs})
	})

	// Reattach: the latest run for a conversation id, so a client returning to a
	// chat it streamed before navigating away can hydrate the (now background-
	// finished) answer. 404 when the session has no run yet. The summary field
	// carries the final answer for a done run.
	e.GET("/api/agent/sessions/:session_id/run", func(c echo.Context) error {
		ctx, project, err := authProject(c, store)
		if err != nil {
			return err
		}
		run, err := store.LatestRunForSession(c.Request().Context(), ctx.User.ID, project.ID, c.Param("session_id"))
		if err != nil {
			return echo.NewHTTPError(http.StatusNotFound, "no run for session")
		}
		// The persisted tool-call trace lets a client that reloaded mid-run rebuild
		// the step timeline it lost (progress narration isn't durable, but the tool
		// steps are). Best-effort: a trace read failure still returns the run.
		_, calls, callErr := store.GetAgentRun(c.Request().Context(), ctx.User.ID, project.ID, run.ID)
		if callErr != nil {
			calls = nil
		}
		return c.JSON(http.StatusOK, map[string]any{"run": run, "tool_calls": calls})
	})

	e.GET("/api/agent/runs/:run_id", func(c echo.Context) error {
		ctx, project, err := authProject(c, store)
		if err != nil {
			return err
		}
		run, calls, err := store.GetAgentRun(c.Request().Context(), ctx.User.ID, project.ID, c.Param("run_id"))
		if err != nil {
			return err
		}
		// The per-LLM-call trace (messages, est. cost, latency) — the deepest tier
		// of the loop timeline. Best-effort: a trace read failure must not blank the
		// run detail, so an empty slice is returned rather than erroring the request.
		llmCalls, llmErr := store.ListAgentLLMCalls(c.Request().Context(), ctx.User.ID, project.ID, c.Param("run_id"))
		if llmErr != nil {
			llmCalls = []storage.AgentLLMCall{}
		}
		return c.JSON(http.StatusOK, map[string]any{"run": run, "tool_calls": calls, "llm_calls": llmCalls})
	})

	// --- recommendations (§8, §13.2) ---
	e.GET("/api/agent/recommendations", func(c echo.Context) error {
		ctx, project, err := authProject(c, store)
		if err != nil {
			return err
		}
		recs, err := store.ListRecommendations(c.Request().Context(), ctx.User.ID, project.ID)
		if err != nil {
			return err
		}
		return c.JSON(http.StatusOK, map[string]any{"recommendations": recs})
	})

	e.POST("/api/agent/recommendations/:id/ack", func(c echo.Context) error {
		ctx, project, err := authProject(c, store)
		if err != nil {
			return err
		}
		var payload struct {
			Status string `json:"status"` // accepted | dismissed
			Note   string `json:"note"`
		}
		if err := c.Bind(&payload); err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, "invalid json")
		}
		if err := store.AckRecommendation(c.Request().Context(), ctx.User.ID, project.ID, c.Param("id"), payload.Status, payload.Note); err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, err.Error())
		}
		return c.JSON(http.StatusOK, map[string]any{"ok": true})
	})

	// --- chat (§8): one conversational turn owned by the orchestrator. Small talk
	// gets an instant cheap reply (no analytics run); a data question is delegated
	// to the Growth Analyst. Streams tokens + plain-language progress + a result
	// card over SSE when the client sends Accept: text/event-stream or ?stream=1;
	// otherwise returns the completed turn as one JSON body (back-compatible).
	// `history` carries prior turns (client-held, no conversation store). ---
	e.POST("/api/agent/chat", func(c echo.Context) error {
		_, project, err := authProject(c, store)
		if err != nil {
			return err
		}
		var payload struct {
			Message string `json:"message"`
			// SessionID is the client-held conversation id. When set, the in-flight
			// run registers under it so a sibling /chat/steer request can inject a
			// mid-run correction. Optional and back-compatible (empty = no live control).
			SessionID string `json:"session_id"`
			// Mode selects what an auto-routed in-flight message does: "steer"
			// (default) injects it into the current turn; "followup" continues the run
			// after its next answer. Only consulted when a run is already live for the
			// session; ignored for a fresh turn.
			Mode    string `json:"mode"`
			History []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"history"`
		}
		if err := c.Bind(&payload); err != nil || payload.Message == "" {
			return echo.NewHTTPError(http.StatusBadRequest, "message required")
		}

		// Auto-route convenience: when a run is already live for this conversation,
		// a second /chat is an amendment to that turn, not a new run — inject it and
		// tell the caller (via a `steered` discriminator) that the answer flows on the
		// original, still-open stream. A non-live session (the run just finished, or
		// none started) falls through to a normal turn; the registry's presence +
		// project-ownership check makes this race-safe. The explicit /chat/steer
		// endpoint stays for clients that prefer to branch themselves.
		if liveReg != nil && payload.SessionID != "" {
			mode := "steer"
			var delivered bool
			if payload.Mode == "followup" {
				mode = "followup"
				delivered = liveReg.FollowUp(project.ID, payload.SessionID, payload.Message)
			} else {
				delivered = liveReg.Steer(project.ID, payload.SessionID, payload.Message)
			}
			if delivered {
				if wantsEventStream(c) {
					return steerAck(c, mode)
				}
				return c.JSON(http.StatusOK, map[string]any{"steered": true, "delivered": true, "mode": mode})
			}
		}

		svc := agentruntime.NewChatService(store, runnerOpts...)
		opts := agentruntime.ChatOptions{
			ProjectID: project.ID, AgentID: c.QueryParam("agent"),
			Message: payload.Message, History: chatHistory(payload.History),
			SessionID: payload.SessionID,
		}

		if wantsEventStream(c) {
			return streamChat(c, svc, opts)
		}
		res, runErr := svc.Chat(c.Request().Context(), opts, nil)
		if runErr != nil {
			return c.JSON(http.StatusBadGateway, map[string]any{"error": runErr.Error(), "run_id": res.RunID})
		}
		return c.JSON(http.StatusOK, map[string]any{
			"run_id": res.RunID, "final": res.Final, "tool_calls": res.Tools,
			"usage": res.Usage, "turns": res.Turns, "card": res.Card, "route": res.Route,
		})
	})

	// --- live control (§ steering): inject a message into an in-flight run keyed
	// on the client conversation id. `steer` is honored before the model reasons on
	// its next turn (a mid-run correction); `followup` continues the same bounded
	// run after it produces its next final answer. Returns delivered:false when no
	// run is live for the session (the client then starts a normal turn). ---
	e.POST("/api/agent/chat/steer", func(c echo.Context) error {
		_, project, err := authProject(c, store)
		if err != nil {
			return err
		}
		if liveReg == nil {
			return echo.NewHTTPError(http.StatusServiceUnavailable, "live control unavailable")
		}
		var payload struct {
			SessionID string `json:"session_id"`
			Message   string `json:"message"`
			Mode      string `json:"mode"` // "steer" (default) | "followup"
		}
		if err := c.Bind(&payload); err != nil || payload.SessionID == "" || payload.Message == "" {
			return echo.NewHTTPError(http.StatusBadRequest, "session_id and message required")
		}
		delivered := false
		if payload.Mode == "followup" {
			delivered = liveReg.FollowUp(project.ID, payload.SessionID, payload.Message)
		} else {
			delivered = liveReg.Steer(project.ID, payload.SessionID, payload.Message)
		}
		return c.JSON(http.StatusOK, map[string]any{"delivered": delivered})
	})

	// runConversationTurn appends a user message off the conversation's current leaf
	// and runs the agent on the server-derived history — the shared tail of the
	// send-message and fork/regenerate handlers. Callers that want to fork first
	// repoint the leaf (store.SetConversationLeaf) so this append parents off the
	// branch point. History is derived BEFORE the append so Message carries the
	// current turn without double-counting.
	runConversationTurn := func(c echo.Context, ctx authContext, project storage.Project, conv storage.AgentConversation, message, agentID string) error {
		// agentID is the acting agent for this turn (the per-message override, or the
		// conversation's current agent). It is stamped on the user and assistant
		// entries and threaded into the run, so switching agents changes the system
		// prompt, persona, permissions, and tools from this turn onward — past entries
		// keep the agent that handled them.
		if agentID == "" {
			agentID = conv.AgentID
		}
		history, err := agentruntime.BuildHistory(c.Request().Context(), store, conv.ID)
		if err != nil {
			return err
		}
		if _, err := agentruntime.AppendMessageEntry(c.Request().Context(), store, conv.ID,
			string(agentcore.RoleUser), message, agentID, ctx.User.ID, "", 0); err != nil {
			return err
		}
		svc := agentruntime.NewChatService(store, runnerOpts...)
		opts := agentruntime.ChatOptions{
			ProjectID: project.ID, AgentID: agentID,
			Message: message, History: history,
			SessionID: conv.ID, ConversationID: conv.ID,
		}
		if wantsEventStream(c) {
			return streamChat(c, svc, opts)
		}
		res, runErr := svc.Chat(c.Request().Context(), opts, nil)
		if runErr != nil {
			return c.JSON(http.StatusBadGateway, map[string]any{"error": runErr.Error(), "run_id": res.RunID})
		}
		return c.JSON(http.StatusOK, map[string]any{
			"run_id": res.RunID, "final": res.Final, "tool_calls": res.Tools,
			"usage": res.Usage, "turns": res.Turns, "card": res.Card, "route": res.Route,
		})
	}

	// --- conversations (DESIGN-CONVERSATION-STORE.md): the server-side durable
	// thread three machines share. A conversation is a stable server row; its
	// append-only entry log is the source of truth for both the human view (message
	// entries listed by GET) and the model context (folded server-side by
	// BuildHistory). This replaces the client's localStorage-held history: machine 1
	// opens a conversation, machine 2 GETs its entries and continues, and the LLM
	// context is rebuilt on the server, not the client. ---

	// Open a new conversation for an agent. Returns the row; the client navigates by
	// its id instead of a locally-minted session id.
	e.POST("/api/agent/conversations", func(c echo.Context) error {
		ctx, project, err := authProject(c, store)
		if err != nil {
			return err
		}
		var payload struct {
			AgentID string `json:"agent_id"`
			Title   string `json:"title"`
		}
		_ = c.Bind(&payload)
		conv, err := store.CreateConversation(c.Request().Context(), ctx.User.ID, project.ID, payload.AgentID, payload.Title)
		if err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, err.Error())
		}
		return c.JSON(http.StatusOK, map[string]any{"conversation": conv})
	})

	// List recent conversations for the project (newest activity first).
	e.GET("/api/agent/conversations", func(c echo.Context) error {
		ctx, project, err := authProject(c, store)
		if err != nil {
			return err
		}
		convs, err := store.ListConversations(c.Request().Context(), ctx.User.ID, project.ID, 0)
		if err != nil {
			return err
		}
		return c.JSON(http.StatusOK, map[string]any{"conversations": convs})
	})

	// Read a conversation's entries since a sync cursor (?since=<seq>, default 0 =
	// whole log). Returns every branch's entries in seq order plus the current leaf
	// seq, so a returning/joining client renders the full human view and knows where
	// to resume polling from. This is the multi-machine load path.
	e.GET("/api/agent/conversations/:id", func(c echo.Context) error {
		ctx, project, err := authProject(c, store)
		if err != nil {
			return err
		}
		var since int64
		if v := c.QueryParam("since"); v != "" {
			since, _ = strconv.ParseInt(v, 10, 64)
		}
		conv, err := store.GetConversation(c.Request().Context(), ctx.User.ID, project.ID, c.Param("id"))
		if err != nil {
			return echo.NewHTTPError(http.StatusNotFound, "conversation not found")
		}
		entries, err := store.ConversationEntries(c.Request().Context(), ctx.User.ID, project.ID, conv.ID, since)
		if err != nil {
			return err
		}
		var leafSeq int64
		for _, en := range entries {
			if en.Seq > leafSeq {
				leafSeq = en.Seq
			}
		}
		return c.JSON(http.StatusOK, map[string]any{"conversation": conv, "entries": entries, "leaf_seq": leafSeq})
	})

	// Send a message into a conversation: append the user turn as a durable entry,
	// derive the model History server-side from the conversation (NOT from a
	// client-shipped history), then run the agent and stream it. The assistant turn
	// is appended as an entry by the ChatService when the run finishes — so the whole
	// exchange is durable and a second machine sees it. SessionID is set to the
	// conversation id so live steer/follow-up keeps working. Mirrors POST /chat,
	// minus the client history.
	e.POST("/api/agent/conversations/:id/messages", func(c echo.Context) error {
		ctx, project, err := authProject(c, store)
		if err != nil {
			return err
		}
		convID := c.Param("id")
		conv, err := store.GetConversation(c.Request().Context(), ctx.User.ID, project.ID, convID)
		if err != nil {
			return echo.NewHTTPError(http.StatusNotFound, "conversation not found")
		}
		var payload struct {
			Message string `json:"message"`
			Mode    string `json:"mode"`
			// AgentID switches the acting agent from this message onward (the
			// per-message agent override). Empty keeps the conversation's current
			// agent. Only new entries are affected; past entries keep their stamp.
			AgentID string `json:"agent_id"`
		}
		if err := c.Bind(&payload); err != nil || payload.Message == "" {
			return echo.NewHTTPError(http.StatusBadRequest, "message required")
		}

		// Resolve the acting agent for this turn and, when it differs from the
		// conversation's current agent, persist the switch so subsequent
		// (override-less) messages continue with the newly selected agent. Past
		// entries keep the agent that handled them.
		actingAgent := payload.AgentID
		if actingAgent == "" {
			actingAgent = conv.AgentID
		}
		if payload.AgentID != "" && payload.AgentID != conv.AgentID {
			if err := store.SetConversationAgent(c.Request().Context(), ctx.User.ID, project.ID, convID, payload.AgentID); err != nil {
				return echo.NewHTTPError(http.StatusBadRequest, err.Error())
			}
		}

		// A second message while a run is live on this conversation is an amendment,
		// not a new run: steer/follow-up it (same auto-route as /chat), keyed on the
		// conversation id. The amendment is still appended below so it survives.
		if liveReg != nil {
			mode := "steer"
			delivered := false
			if payload.Mode == "followup" {
				mode = "followup"
				delivered = liveReg.FollowUp(project.ID, convID, payload.Message)
			} else {
				delivered = liveReg.Steer(project.ID, convID, payload.Message)
			}
			if delivered {
				_, _ = agentruntime.AppendMessageEntry(c.Request().Context(), store, convID,
					string(agentcore.RoleUser), payload.Message, actingAgent, ctx.User.ID, "", 0)
				if wantsEventStream(c) {
					return steerAck(c, mode)
				}
				return c.JSON(http.StatusOK, map[string]any{"steered": true, "delivered": true, "mode": mode})
			}
		}

		// Append off the current leaf and run on the server-derived history (the
		// shared tail; History is derived before the append so Message isn't double
		// counted).
		return runConversationTurn(c, ctx, project, conv, payload.Message, actingAgent)
	})

	// Edit a prior USER message and re-run from there: fork the tree at the edited
	// message's parent, so the new (edited) turn and its answer form a fresh branch
	// while the original message and everything after it stay reachable (every
	// branch is still returned by GET /conversations/:id). This is the
	// session-as-tree navigation (pi's moveTo/navigateTree) expressed as a thin op
	// on the existing parent_id + movable-leaf store — no new storage shape.
	e.POST("/api/agent/conversations/:id/messages/:entry_id/edit", func(c echo.Context) error {
		ctx, project, err := authProject(c, store)
		if err != nil {
			return err
		}
		convID := c.Param("id")
		conv, err := store.GetConversation(c.Request().Context(), ctx.User.ID, project.ID, convID)
		if err != nil {
			return echo.NewHTTPError(http.StatusNotFound, "conversation not found")
		}
		var payload struct {
			Message string `json:"message"`
		}
		if err := c.Bind(&payload); err != nil || payload.Message == "" {
			return echo.NewHTTPError(http.StatusBadRequest, "message required")
		}
		target, err := store.GetConversationEntry(c.Request().Context(), convID, c.Param("entry_id"))
		if err != nil {
			return echo.NewHTTPError(http.StatusNotFound, "entry not found")
		}
		if target.Kind != agentruntime.ConvKindMessage || target.Role != string(agentcore.RoleUser) {
			return echo.NewHTTPError(http.StatusBadRequest, "can only edit a user message")
		}
		// Fork at the edited message's parent, then run the edited turn there.
		if err := store.SetConversationLeaf(c.Request().Context(), convID, target.ParentID); err != nil {
			return err
		}
		return runConversationTurn(c, ctx, project, conv, payload.Message, "")
	})

	// Regenerate an ASSISTANT message: fork at the user turn it answered and re-run
	// that same user message, producing a fresh answer on a new branch while the
	// original answer stays reachable. Like edit, this is pure tree navigation on
	// the existing store.
	e.POST("/api/agent/conversations/:id/messages/:entry_id/regenerate", func(c echo.Context) error {
		ctx, project, err := authProject(c, store)
		if err != nil {
			return err
		}
		convID := c.Param("id")
		conv, err := store.GetConversation(c.Request().Context(), ctx.User.ID, project.ID, convID)
		if err != nil {
			return echo.NewHTTPError(http.StatusNotFound, "conversation not found")
		}
		target, err := store.GetConversationEntry(c.Request().Context(), convID, c.Param("entry_id"))
		if err != nil {
			return echo.NewHTTPError(http.StatusNotFound, "entry not found")
		}
		if target.Kind != agentruntime.ConvKindMessage || target.Role != string(agentcore.RoleAssistant) {
			return echo.NewHTTPError(http.StatusBadRequest, "can only regenerate an assistant message")
		}
		// The user turn this answer replied to is the branch point; resend it.
		parent, err := store.GetConversationEntry(c.Request().Context(), convID, target.ParentID)
		if err != nil || parent.Role != string(agentcore.RoleUser) {
			return echo.NewHTTPError(http.StatusBadRequest, "cannot resolve the message to regenerate")
		}
		message := agentruntime.MessageEntryText(parent)
		if message == "" {
			return echo.NewHTTPError(http.StatusBadRequest, "parent message is empty")
		}
		// Fork at the user turn's parent and resend the user turn verbatim.
		if err := store.SetConversationLeaf(c.Request().Context(), convID, parent.ParentID); err != nil {
			return err
		}
		return runConversationTurn(c, ctx, project, conv, message, "")
	})

	// --- resume (§ durable runs): replay a crashed/interrupted run from its durable
	// session log. Reduces the append-only log into a conservative recovery plan,
	// rebuilds a valid transcript, and drives a fresh run seeded with it. Owner/admin
	// only — it spends model budget. ---
	e.POST("/api/agent/run/:run_id/resume", func(c echo.Context) error {
		ctx, project, err := authProject(c, store)
		if err != nil {
			return err
		}
		runner := agentruntime.NewRunner(store, runnerOpts...)
		run, res, runErr := runner.ResumeRun(c.Request().Context(), ctx.User.ID, project.ID, c.Param("run_id"))
		if runErr != nil {
			return c.JSON(http.StatusBadGateway, map[string]any{"error": runErr.Error(), "run_id": run.ID})
		}
		return c.JSON(http.StatusOK, map[string]any{
			"run_id": run.ID, "final": res.Final, "tool_calls": res.Tools,
			"usage": res.Usage, "turns": res.Turns,
		})
	})

	// --- manual "run now": enqueue a scheduled-style autonomous run ---
	e.POST("/api/agent/run", func(c echo.Context) error {
		ctx, project, err := authProject(c, store)
		if err != nil {
			return err
		}
		// Only owner/admin can trigger autonomous runs.
		cfg, err := store.GetAgentConfig(c.Request().Context(), ctx.User.ID, project.ID)
		if err != nil {
			return err
		}
		if !cfg.Enabled {
			return echo.NewHTTPError(http.StatusForbidden, "agent is disabled for this project")
		}
		if scheduler == nil {
			return echo.NewHTTPError(http.StatusServiceUnavailable, "scheduler unavailable")
		}
		if err := scheduler.Publish(project.ID, agentruntime.MonitorPrompt); err != nil {
			return echo.NewHTTPError(http.StatusBadGateway, err.Error())
		}
		return c.JSON(http.StatusAccepted, map[string]any{"queued": true})
	})

	// --- triggers (AgentGarden §7): per-agent schedule + webhook config ---
	e.GET("/api/agent/triggers", func(c echo.Context) error {
		ctx, project, err := authProject(c, store)
		if err != nil {
			return err
		}
		triggers, err := store.ListAgentTriggers(c.Request().Context(), ctx.User.ID, project.ID, c.QueryParam("agent"))
		if err != nil {
			return err
		}
		return c.JSON(http.StatusOK, map[string]any{"triggers": triggers})
	})

	e.POST("/api/agent/triggers", func(c echo.Context) error {
		ctx, project, err := authProject(c, store)
		if err != nil {
			return err
		}
		var payload struct {
			Kind           string `json:"kind"`
			Enabled        bool   `json:"enabled"`
			Cron           string `json:"cron"`
			PromptTemplate string `json:"prompt_template"`
			HMACSecretName string `json:"hmac_secret_name"`
		}
		if err := c.Bind(&payload); err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, "invalid json")
		}
		trigger, err := store.CreateAgentTrigger(c.Request().Context(), ctx.User.ID, project.ID, c.QueryParam("agent"), storage.AgentTrigger{
			Kind: payload.Kind, Enabled: payload.Enabled, Cron: payload.Cron,
			PromptTemplate: payload.PromptTemplate, HMACSecretName: payload.HMACSecretName,
		})
		if err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, err.Error())
		}
		return c.JSON(http.StatusCreated, trigger)
	})

	e.PUT("/api/agent/triggers/:id", func(c echo.Context) error {
		ctx, project, err := authProject(c, store)
		if err != nil {
			return err
		}
		var payload struct {
			Enabled        bool   `json:"enabled"`
			Cron           string `json:"cron"`
			PromptTemplate string `json:"prompt_template"`
			HMACSecretName string `json:"hmac_secret_name"`
		}
		if err := c.Bind(&payload); err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, "invalid json")
		}
		trigger, err := store.UpdateAgentTrigger(c.Request().Context(), ctx.User.ID, project.ID, c.QueryParam("agent"), c.Param("id"), storage.AgentTrigger{
			Enabled: payload.Enabled, Cron: payload.Cron,
			PromptTemplate: payload.PromptTemplate, HMACSecretName: payload.HMACSecretName,
		})
		if err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, err.Error())
		}
		return c.JSON(http.StatusOK, trigger)
	})

	e.DELETE("/api/agent/triggers/:id", func(c echo.Context) error {
		ctx, project, err := authProject(c, store)
		if err != nil {
			return err
		}
		if err := store.DeleteAgentTrigger(c.Request().Context(), ctx.User.ID, project.ID, c.QueryParam("agent"), c.Param("id")); err != nil {
			return echo.NewHTTPError(http.StatusForbidden, err.Error())
		}
		return c.NoContent(http.StatusNoContent)
	})

	// --- webhook ingress (AgentGarden §7): the bot-app path. NOT session-authed —
	// the unguessable per-trigger token in the URL is the credential, and the body
	// is optionally HMAC-authenticated (X-Agent-Signature). The body is size-capped,
	// resolved+verified inside storage (the signing secret never reaches this
	// layer), then dispatched async via the same NATS run path as the scheduler. ---
	e.POST("/api/agent/hook/:token", func(c echo.Context) error {
		if scheduler == nil {
			return echo.NewHTTPError(http.StatusServiceUnavailable, "scheduler unavailable")
		}
		// Cap the body so a webhook can't be used to exhaust memory; 1 MiB is well
		// above any reasonable JSON event payload.
		body, err := io.ReadAll(io.LimitReader(c.Request().Body, 1<<20))
		if err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, "unreadable body")
		}
		run, ok, err := store.ResolveWebhook(c.Request().Context(), c.Param("token"), string(body), c.Request().Header.Get("X-Agent-Signature"))
		if err != nil {
			return echo.NewHTTPError(http.StatusBadGateway, err.Error())
		}
		if !ok {
			// One opaque status for unknown token, disabled trigger, and bad
			// signature alike, so the endpoint leaks nothing about why it refused.
			return echo.NewHTTPError(http.StatusUnauthorized, "unauthorized")
		}
		if err := scheduler.PublishWebhook(run.ProjectID, run.AgentID, run.Prompt); err != nil {
			return echo.NewHTTPError(http.StatusBadGateway, err.Error())
		}
		return c.JSON(http.StatusAccepted, map[string]any{"queued": true})
	})
}

// authProject resolves the auth context + project for a request in one step.
func authProject(c echo.Context, store *storage.Store) (authContext, storage.Project, error) {
	ctx, err := authFromRequest(c, store)
	if err != nil {
		return authContext{}, storage.Project{}, err
	}
	project, err := projectFromRequest(c, store)
	if err != nil {
		return authContext{}, storage.Project{}, err
	}
	return ctx, project, nil
}

// testTierProvider runs a 1-token ping against one tier's provider/model and
// returns {ok, error}. It routes through agentruntime so an OpenAI-compatible
// vendor (custom base_url) is tested exactly as a real run would call it.
func testTierProvider(c echo.Context, provider, baseURL, model, key string) map[string]any {
	p, err := agentruntime.NewTierProvider(provider, baseURL, key)
	if err != nil {
		return map[string]any{"ok": false, "error": err.Error()}
	}
	_, callErr := p.Chat(c.Request().Context(), agentcore.ChatRequest{
		Model:     model,
		Messages:  []agentcore.Message{{Role: agentcore.RoleUser, Content: "ping"}},
		MaxTokens: 1,
	})
	if callErr != nil {
		return map[string]any{"ok": false, "error": callErr.Error()}
	}
	return map[string]any{"ok": true}
}

// wantsEventStream reports whether the client asked for an SSE token stream.
func wantsEventStream(c echo.Context) bool {
	if c.QueryParam("stream") == "1" {
		return true
	}
	return strings.Contains(c.Request().Header.Get("Accept"), "text/event-stream")
}

// chatHistory converts the client-supplied prior turns into agentcore messages,
// keeping only user/assistant roles (the model rebuilds tool turns itself).
func chatHistory(in []struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}) []agentcore.Message {
	out := make([]agentcore.Message, 0, len(in))
	for _, m := range in {
		role := agentcore.Role(m.Role)
		if role != agentcore.RoleUser && role != agentcore.RoleAssistant {
			continue
		}
		if strings.TrimSpace(m.Content) == "" {
			continue
		}
		out = append(out, agentcore.Message{Role: role, Content: m.Content})
	}
	return out
}

// streamChat runs one orchestrated chat turn and streams it to the client as SSE.
// Events, all additive: `token` carries assistant text fragments; `progress`
// carries a plain-language note (no tool identifier); `tool` carries the raw
// tool-call trace (debug only on the client); `card` carries a structured result
// card; a terminal `done` carries the persisted run_id, final answer, full tool
// trace, usage, and card (same shape as the JSON path). A mid-run error becomes
// an in-band `error` event followed by `done`.
func streamChat(c echo.Context, svc *agentruntime.ChatService, opts agentruntime.ChatOptions) error {
	w := c.Response()
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no") // disable proxy buffering so tokens flush live
	w.WriteHeader(http.StatusOK)
	w.Flush()

	// The SSE connection is only the *view* of the run, not its lifetime. Once the
	// client disconnects (navigated away), `live` flips false under the mutex and we
	// stop touching the now-orphaned ResponseWriter; the run keeps going on a
	// detached context (below) and persists its terminal status + answer, which the
	// client reattaches to on return. The mutex serializes the disconnect flip
	// against in-flight frame writes so we never write after the handler returns.
	var mu sync.Mutex
	live := true
	safeSSE := func(event string, data any) {
		mu.Lock()
		defer mu.Unlock()
		if !live {
			return
		}
		writeSSE(w, event, data)
	}

	sink := func(ev agentcore.StreamEvent) {
		switch ev.Type {
		case agentcore.StreamToken:
			safeSSE("token", map[string]any{"token": ev.Token})
		case agentcore.StreamProgress:
			safeSSE("progress", map[string]any{"note": ev.Note})
		case agentcore.StreamCard:
			if ev.Card != nil {
				safeSSE("card", ev.Card)
			}
		case agentcore.StreamTool:
			// The canonical completed tool-call trace (debug-only on the client).
			// StreamToolExecEnd carries the same trace for the fine-grained lifecycle
			// and must NOT also emit a `tool` frame — doing so double-rendered every
			// tool call in the chat debug trace.
			if ev.Tool != nil {
				safeSSE("tool", map[string]any{
					"tool": ev.Tool.Tool, "allowed": ev.Tool.Allowed,
					"reason": ev.Tool.Reason, "error": ev.Tool.Error,
					"result_meta": ev.Tool.ResultMeta,
				})
			}
		case agentcore.StreamToolExecUpdate:
			// A streaming tool's partial output (P8): forward as a progress-shaped
			// note so a client can show live tool output without a new contract.
			safeSSE("tool_update", map[string]any{"note": ev.Note, "turn": ev.Turn})
		case agentcore.StreamToolExecStart:
			// A tool call is about to run: emit its name so the step timeline can show
			// it as in-flight (reconciled to done/blocked when the `tool` trace lands).
			if ev.Tool != nil {
				safeSSE("tool_start", map[string]any{"tool": ev.Tool.Tool, "turn": ev.Turn})
			}
		case agentcore.StreamAgentStart, agentcore.StreamAgentEnd,
			agentcore.StreamTurnStart, agentcore.StreamTurnEnd,
			agentcore.StreamMessageStart, agentcore.StreamMessageEnd,
			agentcore.StreamToolExecEnd, agentcore.StreamSavePoint:
			// Granular run lifecycle (additive): a client that drives a fine-grained
			// progress UI off these can; older clients ignore the unknown event name.
			safeSSE("lifecycle", map[string]any{"phase": string(ev.Type), "turn": ev.Turn})
		}
	}

	// Emit the run id the moment the run row opens, before any token, so the client
	// can persist it and reattach after navigating away.
	opts.OnRunID = func(runID string) {
		safeSSE("run", map[string]any{"run_id": runID})
	}

	// Detach the run from the request: WithoutCancel keeps auth/values but drops the
	// connection's cancellation, and a fresh ceiling bounds an abandoned run. The run
	// executes in a goroutine so we can return as soon as the client leaves while it
	// finishes in the background.
	runCtx, cancel := context.WithTimeout(context.WithoutCancel(c.Request().Context()), detachedRunCeiling)
	done := make(chan struct{})
	var res agentruntime.ChatResult
	var runErr error
	go func() {
		defer cancel() // owned by the goroutine, so returning early never cancels the run
		defer close(done)
		res, runErr = svc.Chat(runCtx, opts, sink)
	}()

	select {
	case <-done:
		// Finished while the client was watching: emit the terminal frames inline.
		if runErr != nil {
			safeSSE("error", map[string]any{"error": runErr.Error(), "run_id": res.RunID})
		}
		safeSSE("done", map[string]any{
			"run_id": res.RunID, "final": res.Final, "tool_calls": res.Tools,
			"usage": res.Usage, "turns": res.Turns, "card": res.Card, "route": res.Route,
		})
		return nil
	case <-c.Request().Context().Done():
		// Client navigated away: stop writing to the dead connection and return. The
		// goroutine keeps running on runCtx and FinishAgentRun persists the result.
		mu.Lock()
		live = false
		mu.Unlock()
		return nil
	}
}

// steerAck answers an event-stream /chat request that was auto-routed into a live
// run: it emits a single `steered` frame (so the client learns its message was
// injected and the answer will arrive on the original, still-open stream) and
// returns, leaving no run of its own. The JSON path returns the same discriminator
// as a plain body.
func steerAck(c echo.Context, mode string) error {
	w := c.Response()
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	writeSSE(w, "steered", map[string]any{"delivered": true, "mode": mode})
	return nil
}

// writeSSE marshals data and writes one server-sent event frame, flushing so the
// client receives it immediately.
func writeSSE(w *echo.Response, event string, data any) {
	b, err := json.Marshal(data)
	if err != nil {
		return
	}
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
	w.Flush()
}
