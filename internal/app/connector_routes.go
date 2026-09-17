package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/lohi-ai/agentray/agentcore"
	"github.com/lohi-ai/agentray/ai"
	"github.com/lohi-ai/agentray/internal/dataplane/connector"
	"github.com/lohi-ai/agentray/internal/dataplane/store"
	"github.com/lohi-ai/agentray/internal/oauth"
	"github.com/lohi-ai/agentray/internal/runtime"
	"github.com/lohi-ai/agentray/internal/shared/opcore"
)

// registerConnectorRoutes mounts the data-connector surface: connection CRUD
// (DSN write-only), test/schema probes, per-table sync configs with run
// status, manual run-now, and an AI-assisted sync draft. The lifecycle
// endpoints are thin adapters over the shared operation registry — same URLs
// and envelopes, but the mutation runs the opcore -> usecase -> store path
// every other adapter runs. Auth stays session-only (the legacy contract);
// the session principal is authorized against each operation's Access class,
// which preserves the member-read / owner-admin-write boundary the store
// enforced.
func registerConnectorRoutes(e *echo.Echo, store *storage.Store, ops *opAdapter, oauthMgr *oauth.Manager) {
	// sessionOp resolves the session caller into an opcore principal for the
	// project this request names — the legacy session-only admission plus the
	// registry's access-class check.
	sessionOp := func(c echo.Context, opName string) (opcore.Principal, storage.Project, error) {
		_, principal, project, err := sessionCaller(c, store)
		if err != nil {
			return opcore.Principal{}, storage.Project{}, err
		}
		if err := ops.authorize(principal, opName); err != nil {
			return opcore.Principal{}, storage.Project{}, err
		}
		return principal, project, nil
	}

	// --- connectors ---
	e.GET("/api/connectors", func(c echo.Context) error {
		principal, _, err := sessionOp(c, "list_sources")
		if err != nil {
			return err
		}
		out, err := ops.invoke(c, principal, "list_sources", map[string]any{})
		if err != nil {
			return err
		}
		var result struct {
			Sources []storage.DataConnector `json:"sources"`
		}
		if err := json.Unmarshal(out, &result); err != nil {
			return err
		}
		return c.JSON(http.StatusOK, map[string]any{"connectors": result.Sources, "kinds": connector.Kinds()})
	})

	e.POST("/api/connectors", func(c echo.Context) error {
		principal, _, err := sessionOp(c, "create_source")
		if err != nil {
			return err
		}
		var payload struct {
			Name           string `json:"name"`
			Kind           string `json:"kind"`
			CredentialID   string `json:"credential_id"`
			IdempotencyKey string `json:"idempotency_key"`
		}
		if err := c.Bind(&payload); err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, "invalid json")
		}
		// New connectors reference a live source credential — the secret is
		// stored once via POST /api/projects/:id/source-credentials and never
		// transits this route. Inline-DSN rows remain resolvable for existing
		// connectors only.
		if strings.TrimSpace(payload.CredentialID) == "" {
			return echo.NewHTTPError(http.StatusBadRequest, "credential_id is required — store the DSN once via POST /api/projects/:id/source-credentials")
		}
		out, err := ops.invoke(c, principal, "create_source", map[string]any{
			"name":            payload.Name,
			"kind":            payload.Kind,
			"credential_id":   payload.CredentialID,
			"idempotency_key": payload.IdempotencyKey,
		})
		if err != nil {
			return err
		}
		return c.Blob(http.StatusCreated, echo.MIMEApplicationJSON, wrapObject(out, "connector"))
	})

	// DELETE is the reversible archive: the connector row, its credential
	// reference, and every landed row stay; its syncs pause transactionally
	// and unarchive resumes exactly those.
	e.DELETE("/api/connectors/:connector_id", func(c echo.Context) error {
		principal, _, err := sessionOp(c, "archive_source")
		if err != nil {
			return err
		}
		mut, err := readOptionalMutationBody(c)
		if err != nil {
			return err
		}
		connectorID := c.Param("connector_id")
		revision, err := revisionFor(c, mut.Revision, func(ctx context.Context) (int64, error) {
			existing, err := store.DataConnectorForProject(ctx, principal.ProjectID, connectorID)
			return existing.Revision, err
		})
		if err != nil {
			return err
		}
		if _, err := ops.invoke(c, principal, "archive_source", map[string]any{
			"connector_id":    connectorID,
			"revision":        revision,
			"idempotency_key": mut.IdempotencyKey,
		}); err != nil {
			return err
		}
		return c.NoContent(http.StatusNoContent)
	})

	// --- probes (open the source in-process; the DSN never leaves) ---
	e.POST("/api/connectors/:connector_id/test", func(c echo.Context) error {
		principal, _, err := sessionOp(c, "test_source")
		if err != nil {
			return err
		}
		out, err := ops.invoke(c, principal, "test_source", map[string]any{
			"connector_id": c.Param("connector_id"),
		})
		if err != nil {
			return err
		}
		// test_source already answers in the legacy {ok, error?} envelope.
		return c.Blob(http.StatusOK, echo.MIMEApplicationJSON, out)
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
		principal, _, err := sessionOp(c, "source_status")
		if err != nil {
			return err
		}
		out, err := ops.invoke(c, principal, "source_status", map[string]any{
			"connector_id": c.Param("connector_id"),
		})
		if err != nil {
			return err
		}
		var result struct {
			Syncs []struct {
				Sync storage.ConnectorSync `json:"sync"`
			} `json:"syncs"`
		}
		if err := json.Unmarshal(out, &result); err != nil {
			return err
		}
		syncs := make([]storage.ConnectorSync, 0, len(result.Syncs))
		for _, entry := range result.Syncs {
			syncs = append(syncs, entry.Sync)
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

	// Run one sync now (owner/admin). The shared run_source operation owns the
	// enqueue contract — one active run per sync, idempotent replay — so REST
	// and MCP callers observe identical status/cancel semantics. The legacy
	// envelope reports engine failures in-band as {ok:false,error}.
	e.POST("/api/connector-syncs/:sync_id/run", func(c echo.Context) error {
		principal, project, err := sessionOp(c, "run_source")
		if err != nil {
			return err
		}
		mut, err := readOptionalMutationBody(c)
		if err != nil {
			return err
		}
		syncID := c.Param("sync_id")
		ok, err := store.SyncBelongsToProject(c.Request().Context(), project.ID, syncID)
		if err != nil {
			return echo.NewHTTPError(http.StatusBadGateway, err.Error())
		}
		if !ok {
			return echo.NewHTTPError(http.StatusNotFound, "sync not found")
		}
		out, err := ops.invoke(c, principal, "run_source", map[string]any{
			"sync_id":         syncID,
			"idempotency_key": mut.IdempotencyKey,
		})
		if err != nil {
			var he *echo.HTTPError
			if errors.As(err, &he) {
				return c.JSON(http.StatusOK, map[string]any{"ok": false, "error": he.Message})
			}
			return c.JSON(http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		}
		var result struct {
			Run      connector.Run `json:"run"`
			Enqueued bool          `json:"enqueued"`
		}
		if err := json.Unmarshal(out, &result); err != nil {
			return err
		}
		return c.JSON(http.StatusOK, map[string]any{"ok": true, "run": result.Run, "enqueued": result.Enqueued})
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
		provider, model, err := authoringProvider(c.Request().Context(), store, project.WorkspaceID, oauthMgr.Pool)
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

// workspaceTierReader is the one read the authoring-tier resolution needs.
// Narrow so a unit test can drive every fallback without a database.
type workspaceTierReader interface {
	WorkspaceTiersForRun(ctx context.Context, workspaceID string) (storage.WorkspaceModelTiers, map[string]string, error)
}

// authoringProviderInitError marks a failure to CONSTRUCT the provider, as
// opposed to a failure to read its configuration. Callers keep their own status
// contract: /api/agent/definition/generate answers 502 for a provider it could
// not build and 400 for configuration it could not read.
type authoringProviderInitError struct{ err error }

func (e *authoringProviderInitError) Error() string { return e.err.Error() }
func (e *authoringProviderInitError) Unwrap() error { return e.err }

// authoringProvider resolves the workspace's authoring model tier (same
// resolution as the agent-definition draft endpoint) into a callable provider.
// It is the single owner of the tier/fallback rules both authoring endpoints
// use: the pro tier's overrides, falling back to the flash default, then to the
// workspace defaults and the flash key.
func authoringProvider(ctx context.Context, tiers workspaceTierReader, workspaceID string, poolFor func(providerID string) ai.TokenSource) (agentcore.LLMProvider, string, error) {
	cfg, keys, err := tiers.WorkspaceTiersForRun(ctx, workspaceID)
	if err != nil {
		return nil, "", err
	}
	pro := agentruntime.TierSetFromWorkspace(cfg, keys, poolFor).Resolve(agentruntime.DefaultAuthoringTier)
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
	provider, err := agentruntime.NewTierProviderWithSource(pro.Provider, pro.BaseURL, pro.APIKey, pro.TokenSource)
	if err != nil {
		return nil, "", &authoringProviderInitError{err: err}
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
