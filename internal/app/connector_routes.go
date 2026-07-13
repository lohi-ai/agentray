package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/lohi-ai/agentray/agentcore"
	"github.com/lohi-ai/agentray/internal/agentruntime"
	"github.com/lohi-ai/agentray/internal/connector"
	"github.com/lohi-ai/agentray/internal/storage"
)

// registerConnectorRoutes mounts the data-connector surface: connection CRUD
// (DSN write-only), test/schema probes, per-table sync configs with run
// status, manual run-now, and an AI-assisted sync draft. Reads are
// member-level; anything that writes or touches the decrypted DSN is
// owner/admin, enforced in the store.
func registerConnectorRoutes(e *echo.Echo, store *storage.Store, engine *connector.Engine) {
	// --- connectors ---
	e.GET("/api/connectors", func(c echo.Context) error {
		ctx, project, err := authProject(c, store)
		if err != nil {
			return err
		}
		connectors, err := store.ListDataConnectors(c.Request().Context(), ctx.User.ID, project.ID)
		if err != nil {
			return echo.NewHTTPError(http.StatusForbidden, err.Error())
		}
		return c.JSON(http.StatusOK, map[string]any{"connectors": connectors, "kinds": connector.Kinds()})
	})

	e.POST("/api/connectors", func(c echo.Context) error {
		ctx, project, err := authProject(c, store)
		if err != nil {
			return err
		}
		var payload struct {
			Name string `json:"name"`
			Kind string `json:"kind"`
			DSN  string `json:"dsn"`
		}
		if err := c.Bind(&payload); err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, "invalid json")
		}
		created, err := store.CreateDataConnector(c.Request().Context(), ctx.User.ID, project.ID, payload.Name, payload.Kind, payload.DSN)
		if err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, err.Error())
		}
		return c.JSON(http.StatusCreated, map[string]any{"connector": created})
	})

	e.DELETE("/api/connectors/:connector_id", func(c echo.Context) error {
		ctx, project, err := authProject(c, store)
		if err != nil {
			return err
		}
		if err := store.DeleteDataConnector(c.Request().Context(), ctx.User.ID, project.ID, c.Param("connector_id")); err != nil {
			return echo.NewHTTPError(http.StatusForbidden, err.Error())
		}
		return c.NoContent(http.StatusNoContent)
	})

	// --- probes (open the source in-process; the DSN never leaves) ---
	e.POST("/api/connectors/:connector_id/test", func(c echo.Context) error {
		ctx, project, err := authProject(c, store)
		if err != nil {
			return err
		}
		probeCtx, cancel := context.WithTimeout(c.Request().Context(), probeTimeout)
		defer cancel()
		source, err := openConnectorSource(probeCtx, store, ctx.User.ID, project.ID, c.Param("connector_id"))
		if err != nil {
			return c.JSON(http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		}
		defer source.Close()
		if err := source.TestConnection(probeCtx); err != nil {
			return c.JSON(http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		}
		return c.JSON(http.StatusOK, map[string]any{"ok": true})
	})

	e.GET("/api/connectors/:connector_id/schema", func(c echo.Context) error {
		ctx, project, err := authProject(c, store)
		if err != nil {
			return err
		}
		probeCtx, cancel := context.WithTimeout(c.Request().Context(), probeTimeout)
		defer cancel()
		source, err := openConnectorSource(probeCtx, store, ctx.User.ID, project.ID, c.Param("connector_id"))
		if err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, err.Error())
		}
		defer source.Close()
		tables, err := source.DiscoverSchema(probeCtx)
		if err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, err.Error())
		}
		return c.JSON(http.StatusOK, map[string]any{"tables": tables})
	})

	// --- syncs ---
	e.GET("/api/connectors/:connector_id/syncs", func(c echo.Context) error {
		ctx, project, err := authProject(c, store)
		if err != nil {
			return err
		}
		syncs, err := store.ListConnectorSyncs(c.Request().Context(), ctx.User.ID, project.ID, c.Param("connector_id"))
		if err != nil {
			return echo.NewHTTPError(http.StatusForbidden, err.Error())
		}
		return c.JSON(http.StatusOK, map[string]any{"syncs": syncs})
	})

	e.POST("/api/connectors/:connector_id/syncs", func(c echo.Context) error {
		ctx, project, err := authProject(c, store)
		if err != nil {
			return err
		}
		var payload storage.ConnectorSyncInput
		if err := c.Bind(&payload); err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, "invalid json")
		}
		created, err := store.CreateConnectorSync(c.Request().Context(), ctx.User.ID, project.ID, c.Param("connector_id"), payload)
		if err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, err.Error())
		}
		return c.JSON(http.StatusCreated, map[string]any{"sync": created})
	})

	e.PUT("/api/connector-syncs/:sync_id", func(c echo.Context) error {
		ctx, project, err := authProject(c, store)
		if err != nil {
			return err
		}
		var payload storage.ConnectorSyncInput
		if err := c.Bind(&payload); err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, "invalid json")
		}
		updated, err := store.UpdateConnectorSync(c.Request().Context(), ctx.User.ID, project.ID, c.Param("sync_id"), payload)
		if err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, err.Error())
		}
		return c.JSON(http.StatusOK, map[string]any{"sync": updated})
	})

	e.DELETE("/api/connector-syncs/:sync_id", func(c echo.Context) error {
		ctx, project, err := authProject(c, store)
		if err != nil {
			return err
		}
		if err := store.DeleteConnectorSync(c.Request().Context(), ctx.User.ID, project.ID, c.Param("sync_id")); err != nil {
			return echo.NewHTTPError(http.StatusForbidden, err.Error())
		}
		return c.NoContent(http.StatusNoContent)
	})

	// Run one sync now (owner/admin). Synchronous: the response carries the
	// outcome the run also persisted on the sync row.
	e.POST("/api/connector-syncs/:sync_id/run", func(c echo.Context) error {
		ctx, project, err := authProject(c, store)
		if err != nil {
			return err
		}
		canManage, err := store.UserCanManageWorkspace(c.Request().Context(), ctx.User.ID, project.WorkspaceID)
		if err != nil {
			return err
		}
		if !canManage {
			return echo.NewHTTPError(http.StatusForbidden, "connector permission denied")
		}
		syncID := c.Param("sync_id")
		ok, err := store.SyncBelongsToProject(c.Request().Context(), project.ID, syncID)
		if err != nil {
			return echo.NewHTTPError(http.StatusBadGateway, err.Error())
		}
		if !ok {
			return echo.NewHTTPError(http.StatusNotFound, "sync not found")
		}
		if err := engine.RunSync(c.Request().Context(), syncID); err != nil {
			return c.JSON(http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		}
		return c.JSON(http.StatusOK, map[string]any{"ok": true})
	})

	// AI-assisted sync draft: discover the schema, let the authoring model
	// propose table syncs, return them for human review — nothing is saved
	// until the operator approves each one via POST /syncs.
	e.POST("/api/connectors/:connector_id/syncs/draft", func(c echo.Context) error {
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
		probeCtx, cancel := context.WithTimeout(c.Request().Context(), probeTimeout)
		defer cancel()
		source, err := openConnectorSource(probeCtx, store, ctx.User.ID, project.ID, c.Param("connector_id"))
		if err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, err.Error())
		}
		defer source.Close()
		tables, err := source.DiscoverSchema(probeCtx)
		if err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, err.Error())
		}
		if len(tables) == 0 {
			return echo.NewHTTPError(http.StatusBadRequest, "no tables discovered on the source")
		}
		provider, model, err := authoringProvider(c.Request().Context(), store, project.WorkspaceID)
		if err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, err.Error())
		}
		draft, err := draftSyncConfigs(c.Request().Context(), provider, model, tables, payload.Prompt)
		if err != nil {
			return echo.NewHTTPError(http.StatusBadGateway, err.Error())
		}
		return c.JSON(http.StatusOK, draft)
	})
}

// probeTimeout bounds a whole probe request (authorize + dial + the
// test/discover query itself) so a source that connects but then hangs cannot
// pin the handler; each handler derives one probe context from it.
const probeTimeout = 15 * time.Second

// openConnectorSource authorizes (owner/admin, enforced by the store), decrypts
// the DSN, and opens the source. The caller's ctx must already carry the probe
// deadline — the same ctx then bounds the probe query, so the timeout covers
// the whole operation, not just the dial. The DSN is a local variable only —
// it is never serialized or logged.
func openConnectorSource(ctx context.Context, store *storage.Store, userID, projectID, connectorID string) (connector.Source, error) {
	kind, dsn, err := store.ConnectorDSNForUser(ctx, userID, projectID, connectorID)
	if err != nil {
		return nil, fmt.Errorf("connector not found or permission denied")
	}
	return connector.Open(ctx, kind, dsn)
}

// authoringProvider resolves the workspace's authoring model tier (same
// resolution as the agent-definition draft endpoint) into a callable provider.
func authoringProvider(ctx context.Context, store *storage.Store, workspaceID string) (agentcore.LLMProvider, string, error) {
	cfg, keys, err := store.WorkspaceTiersForRun(ctx, workspaceID)
	if err != nil {
		return nil, "", err
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
		return nil, "", fmt.Errorf("authoring model tier is not configured")
	}
	provider, err := agentruntime.NewTierProvider(pro.Provider, pro.BaseURL, pro.APIKey)
	if err != nil {
		return nil, "", err
	}
	return provider, pro.Model, nil
}

// syncDraft is the reviewed-not-saved output of the AI sync draft.
type syncDraft struct {
	Syncs []struct {
		SourceTable  string `json:"source_table"`
		KeyColumn    string `json:"key_column"`
		CursorColumn string `json:"cursor_column"`
		ScheduleCron string `json:"schedule_cron"`
		Reason       string `json:"reason,omitempty"`
	} `json:"syncs"`
	Warnings []string `json:"warnings,omitempty"`
}

const syncDraftSystem = `You configure table syncs from an external database into an analytics store.
Given the discovered schema (and an optional operator hint), propose which tables to sync.
Return JSON only: {"syncs": [{"source_table", "key_column", "cursor_column", "schedule_cron", "reason"}], "warnings": [...]}.

Rules:
- source_table and key_column must be names that appear in the schema; prefer the primary key as key_column.
- cursor_column should be an updated_at/modified timestamp or monotonically increasing id when one exists; use "" to re-sync the full table each run (only sensible for small tables).
- schedule_cron is a standard 5-field cron; default "0 * * * *" (hourly) unless the hint says otherwise.
- Skip migration/journal/log tables unless asked. Keep the list focused.
- warnings is a short array only for real caveats (no primary key, no usable cursor, very wide table).`

// draftSyncConfigs asks the authoring model to propose sync configs for the
// discovered schema. Strict JSON in/out; malformed output fails closed.
func draftSyncConfigs(ctx context.Context, provider agentcore.LLMProvider, model string, tables []connector.Table, hint string) (syncDraft, error) {
	var b strings.Builder
	b.WriteString("Discovered schema:\n")
	for i, t := range tables {
		if i >= 80 {
			fmt.Fprintf(&b, "… and %d more tables\n", len(tables)-i)
			break
		}
		fmt.Fprintf(&b, "- %s:", t.Name)
		for j, col := range t.Columns {
			if j >= 40 {
				b.WriteString(" …")
				break
			}
			fmt.Fprintf(&b, " %s %s", col.Name, col.Type)
			if col.IsPrimaryKey {
				b.WriteString(" (pk)")
			}
			if j < len(t.Columns)-1 {
				b.WriteString(",")
			}
		}
		b.WriteString("\n")
	}
	if hint = strings.TrimSpace(hint); hint != "" {
		b.WriteString("\nOperator hint: " + hint + "\n")
	}
	resp, err := provider.Chat(ctx, agentcore.ChatRequest{
		Model: model,
		Messages: []agentcore.Message{
			{Role: agentcore.RoleSystem, Content: syncDraftSystem},
			{Role: agentcore.RoleUser, Content: b.String()},
		},
		Temperature: 0.2,
		MaxTokens:   1500,
	})
	if err != nil {
		return syncDraft{}, err
	}
	var out syncDraft
	if err := json.Unmarshal([]byte(strings.TrimSpace(resp.Message.Content)), &out); err != nil {
		return syncDraft{}, fmt.Errorf("invalid sync draft response")
	}
	if len(out.Syncs) == 0 {
		return syncDraft{}, fmt.Errorf("sync draft proposed no tables")
	}
	return out, nil
}
