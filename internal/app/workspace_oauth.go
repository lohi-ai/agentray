package app

import (
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/labstack/echo/v4"
	"github.com/lohi-ai/agentray/ai"
	"github.com/lohi-ai/agentray/internal/dataplane/store"
	"github.com/lohi-ai/agentray/internal/oauth"
)

// registerWorkspaceOAuthRoutes wires the multi-account subscription login
// surface: one workspace provider row per OAuth vendor owns a pool of signed-in
// accounts (workspace_provider_accounts), and the run path draws a live access
// token from that pool per request. Login is manual-paste — the vendors' OAuth
// clients only allow localhost redirect URIs, so the user completes sign-in in
// their own browser and pastes the final redirect URL back. Codex additionally
// offers its device-code flow, which needs no paste at all.
func registerWorkspaceOAuthRoutes(e *echo.Echo, store *storage.Store, mgr *oauth.Manager) {
	// Accounts of one provider pool (redacted — tokens never leave the store).
	e.GET("/api/workspace/providers/:id/accounts", func(c echo.Context) error {
		ctx, project, err := authProject(c, store)
		if err != nil {
			return err
		}
		accounts, err := store.ListProviderAccounts(c.Request().Context(), ctx.User.ID, project.WorkspaceID, c.Param("id"))
		if err != nil {
			return oauthStoreError(err)
		}
		if accounts == nil {
			accounts = []storage.WorkspaceProviderAccount{}
		}
		return c.JSON(http.StatusOK, map[string]any{"accounts": accounts})
	})

	// Begin a manual-paste OAuth login: returns the vendor authorize URL. The
	// pending login is keyed by `state` and expires after a few minutes.
	e.POST("/api/workspace/providers/:id/oauth/start", func(c echo.Context) error {
		ctx, project, err := authProject(c, store)
		if err != nil {
			return err
		}
		started, err := mgr.StartLogin(c.Request().Context(), ctx.User.ID, project.WorkspaceID, c.Param("id"))
		if err != nil {
			return oauthFlowError(err)
		}
		return c.JSON(http.StatusOK, started)
	})

	// Finish a manual-paste login: `input` is the redirect URL the browser
	// landed on (or the bare authorization code). `state` correlates the
	// pending login started above.
	e.POST("/api/workspace/providers/:id/oauth/complete", func(c echo.Context) error {
		ctx, project, err := authProject(c, store)
		if err != nil {
			return err
		}
		var payload struct {
			State string `json:"state"`
			Input string `json:"input"`
		}
		if err := c.Bind(&payload); err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, "invalid json")
		}
		account, err := mgr.CompleteLogin(c.Request().Context(), ctx.User.ID, project.WorkspaceID, c.Param("id"), payload.State, payload.Input)
		if err != nil {
			return oauthFlowError(err)
		}
		return c.JSON(http.StatusOK, map[string]any{"account": account})
	})

	// Codex device flow: start returns the user code + verification URL; the UI
	// polls `poll` until the user authorizes. One poll attempt per request —
	// the server holds no login goroutines.
	e.POST("/api/workspace/providers/:id/oauth/device/start", func(c echo.Context) error {
		ctx, project, err := authProject(c, store)
		if err != nil {
			return err
		}
		started, err := mgr.StartDeviceLogin(c.Request().Context(), ctx.User.ID, project.WorkspaceID, c.Param("id"))
		if err != nil {
			return oauthFlowError(err)
		}
		return c.JSON(http.StatusOK, started)
	})

	e.POST("/api/workspace/providers/:id/oauth/device/poll", func(c echo.Context) error {
		ctx, project, err := authProject(c, store)
		if err != nil {
			return err
		}
		var payload struct {
			PendingID string `json:"pending_id"`
		}
		if err := c.Bind(&payload); err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, "invalid json")
		}
		result, err := mgr.PollDeviceLogin(c.Request().Context(), ctx.User.ID, project.WorkspaceID, c.Param("id"), payload.PendingID)
		if err != nil {
			return oauthFlowError(err)
		}
		return c.JSON(http.StatusOK, result)
	})

	e.DELETE("/api/workspace/providers/:id/accounts/:accountID", func(c echo.Context) error {
		ctx, project, err := authProject(c, store)
		if err != nil {
			return err
		}
		if err := store.DeleteProviderAccount(c.Request().Context(), ctx.User.ID, project.WorkspaceID, c.Param("id"), c.Param("accountID")); err != nil {
			return oauthStoreError(err)
		}
		return c.JSON(http.StatusOK, map[string]any{"ok": true})
	})

	// Re-enable a disabled account (or disable one manually).
	e.POST("/api/workspace/providers/:id/accounts/:accountID/status", func(c echo.Context) error {
		ctx, project, err := authProject(c, store)
		if err != nil {
			return err
		}
		var payload struct {
			Status string `json:"status"`
		}
		if err := c.Bind(&payload); err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, "invalid json")
		}
		if payload.Status != "active" && payload.Status != "disabled" {
			return echo.NewHTTPError(http.StatusBadRequest, "status must be active or disabled")
		}
		if err := store.SetProviderAccountStatus(c.Request().Context(), ctx.User.ID, project.WorkspaceID, c.Param("id"), c.Param("accountID"), payload.Status); err != nil {
			return oauthStoreError(err)
		}
		return c.JSON(http.StatusOK, map[string]any{"ok": true})
	})

	// Probe the vendor's usage/quota endpoint for one account and persist the
	// snapshot on the row.
	e.POST("/api/workspace/providers/:id/accounts/:accountID/usage", func(c echo.Context) error {
		ctx, project, err := authProject(c, store)
		if err != nil {
			return err
		}
		usage, err := mgr.ProbeAccountUsage(c.Request().Context(), ctx.User.ID, project.WorkspaceID, c.Param("id"), c.Param("accountID"))
		if err != nil {
			return oauthFlowError(err)
		}
		return c.JSON(http.StatusOK, map[string]any{"usage": usage})
	})
}

// oauthStoreError maps store failures onto HTTP statuses the same way the
// provider routes do: membership/ownership failures are 403, missing rows 404.
func oauthStoreError(err error) error {
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return echo.NewHTTPError(http.StatusNotFound, "not found")
	case errors.Is(err, storage.ErrNoUsableAccount):
		return echo.NewHTTPError(http.StatusConflict, err.Error())
	default:
		return echo.NewHTTPError(http.StatusForbidden, err.Error())
	}
}

// oauthFlowError maps login-flow failures. The typed oauth.Error carries the
// classification in Kind — "validation" is a 400, everything else a 502 with
// the vendor's message — so a reworded message can never flip the status.
// Non-oauth errors fall through to the generic mapping.
func oauthFlowError(err error) error {
	var httpErr *echo.HTTPError
	if errors.As(err, &httpErr) {
		return httpErr
	}
	var oerr *oauth.Error
	if errors.As(err, &oerr) {
		if oerr.Kind == "validation" {
			return echo.NewHTTPError(http.StatusBadRequest, oerr.Message)
		}
		return echo.NewHTTPError(http.StatusBadGateway, oerr.Message)
	}
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return echo.NewHTTPError(http.StatusNotFound, "not found")
	case errors.Is(err, storage.ErrAgentForbidden):
		return echo.NewHTTPError(http.StatusForbidden, "workspace admin required")
	default:
		return echo.NewHTTPError(http.StatusBadGateway, err.Error())
	}
}

// oauthProviderSpec fills an ai.Spec for one book provider, attaching the
// account pool as the TokenSource for OAuth vendors so list-models and
// connection tests authenticate exactly like a run.
func oauthProviderSpec(mgr *oauth.Manager, p storage.WorkspaceProviderRecord) ai.Spec {
	spec := ai.Spec{
		ID: p.ID, Vendor: p.Vendor, Name: p.Name,
		APIKey: p.APIKey, BaseURL: p.BaseURL,
	}
	if ai.IsOAuthVendor(p.Vendor) {
		spec.TokenSource = mgr.Pool(p.ID)
	}
	return spec
}
