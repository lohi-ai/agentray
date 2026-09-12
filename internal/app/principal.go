package app

import (
	"net/http"
	"strings"

	"github.com/labstack/echo/v4"
	"github.com/lohi-ai/agentray/internal/dataplane/store"
	"github.com/lohi-ai/agentray/internal/shared/opcore"
)

// principalFromRequest resolves the caller of an operation adapter (MCP,
// /api/op, CLI) into an opcore.Principal. Credential precedence is
// deterministic: a management credential (Authorization: Bearer agm_…) wins,
// then a project API key (X-API-Key / ?api_key=), then the session cookie. A
// supplied credential never borrows the session's broader role — a Bearer
// header that is present but not a valid management credential is rejected
// outright rather than falling through to a stronger identity.
//
// Project API keys resolve to CredLegacy while the project is unsplit
// (credential_split_at NULL) and CredCapture after opt-in. Management
// credentials resolve to CredManagement with their stored scopes. Sessions map
// the workspace role onto grants via sessionGrants.
func principalFromRequest(c echo.Context, store *storage.Store) (opcore.Principal, error) {
	ctx := c.Request().Context()

	// 1. Management credential: Authorization: Bearer <agm_…>. Header only —
	// a secret in a query param would land in logs and browser history.
	if tok, present := bearerToken(c); present {
		cred, err := store.CredentialBySecret(ctx, tok)
		if err != nil {
			return opcore.Principal{}, echo.NewHTTPError(http.StatusUnauthorized, "invalid credential")
		}
		return opcore.Principal{
			ProjectID:    cred.ProjectID,
			Kind:         opcore.CredManagement,
			CredentialID: cred.CredentialID,
			Grants:       managementGrants(cred.Scopes),
		}, nil
	}

	// 2. Project API key: legacy bridge or capture-only, per the split flag.
	if apiKey := firstNonEmpty(c.QueryParam("api_key"), c.QueryParam("token"), c.Request().Header.Get("X-API-Key")); apiKey != "" {
		project, err := store.ProjectByAPIKey(ctx, apiKey)
		if err != nil {
			return opcore.Principal{}, echo.NewHTTPError(http.StatusUnauthorized, "invalid api key")
		}
		split, err := store.ProjectCredentialSplit(ctx, project.ID)
		if err != nil {
			return opcore.Principal{}, err
		}
		kind := opcore.CredLegacy
		if split {
			kind = opcore.CredCapture
		}
		return opcore.Principal{ProjectID: project.ID, Kind: kind}, nil
	}

	// 3. Session cookie: the workspace role becomes the grant set.
	auth, err := authFromRequest(c, store)
	if err != nil {
		return opcore.Principal{}, err
	}
	projectID := firstNonEmpty(c.QueryParam("project_id"), c.Param("project_id"))
	var project storage.Project
	if projectID != "" {
		project, err = store.ProjectByIDForUser(ctx, auth.User.ID, projectID)
		if err != nil {
			return opcore.Principal{}, echo.NewHTTPError(http.StatusForbidden, "project not available")
		}
	} else {
		project, err = store.DefaultProjectForUser(ctx, auth.User.ID)
		if err != nil {
			return opcore.Principal{}, echo.NewHTTPError(http.StatusNotFound, "project not found")
		}
	}
	return opcore.Principal{
		ProjectID: project.ID,
		Kind:      opcore.CredSession,
		Role:      project.Role,
		UserID:    auth.User.ID,
		Grants:    sessionGrants(project.Role),
	}, nil
}

// bearerToken extracts an Authorization: Bearer token. present is false only
// when no Bearer header was sent at all; a Bearer header carrying a non-agm_
// or empty token returns present=true with an empty token so the caller
// rejects it instead of silently downgrading to another credential.
func bearerToken(c echo.Context) (token string, present bool) {
	h := c.Request().Header.Get("Authorization")
	if !strings.HasPrefix(h, "Bearer ") {
		return "", false
	}
	tok := strings.TrimSpace(strings.TrimPrefix(h, "Bearer "))
	if !strings.HasPrefix(tok, "agm_") {
		return "", true
	}
	return tok, true
}

// managementGrants resolves stored scope strings into Access classes.
// sources:manage implies sources:read. Unknown stored scopes grant nothing —
// they cannot be created through CreateProjectCredential, so reaching one here
// means the row was written by hand and deserves no power.
func managementGrants(scopes []string) []opcore.Access {
	set := map[opcore.Access]bool{}
	for _, s := range scopes {
		switch opcore.Access(s) {
		case opcore.AccessSourcesManage:
			set[opcore.AccessSourcesManage] = true
			set[opcore.AccessSourcesRead] = true
		case opcore.AccessAnalyticsRead, opcore.AccessDashboardsWrite,
			opcore.AccessSourcesRead, opcore.AccessGrowthWrite,
			opcore.AccessPlansWrite:
			set[opcore.Access(s)] = true
		}
	}
	out := make([]opcore.Access, 0, len(set))
	for a := range set {
		out = append(out, a)
	}
	return out
}

// sessionGrants maps a workspace role onto the operation access classes the
// matrix assigns it. Viewer gets analytics read plus sources:read (the matrix
// allows a viewer source_status; the probe operations carry MinSessionRole
// "admin" so the class alone never lets a viewer touch a credential).
// Member adds dashboard and growth writes; owner/admin add source management.
// An unrecognized role gets NOTHING — a role the vocabulary does not know is
// not a viewer by accident.
func sessionGrants(role string) []opcore.Access {
	switch role {
	case "owner", "admin":
		return []opcore.Access{
			opcore.AccessAnalyticsRead, opcore.AccessDashboardsWrite,
			opcore.AccessSourcesRead, opcore.AccessSourcesManage,
			opcore.AccessGrowthWrite, opcore.AccessPlansWrite,
		}
	case "member":
		return []opcore.Access{
			opcore.AccessAnalyticsRead, opcore.AccessDashboardsWrite,
			opcore.AccessSourcesRead, opcore.AccessGrowthWrite,
			opcore.AccessPlansWrite,
		}
	case "viewer":
		return []opcore.Access{opcore.AccessAnalyticsRead, opcore.AccessSourcesRead}
	default:
		return nil
	}
}

// registerCredentialRoutes mounts the session-only management-credential
// surface: create (show-once secret), list (metadata only), revoke, and the
// Option A split opt-in. All are owner/admin writes — the demo write guard
// covers them by default (no exemption entry).
func registerCredentialRoutes(e *echo.Echo, store *storage.Store) {
	e.GET("/api/projects/:project_id/credentials", func(c echo.Context) error {
		ctx, err := authFromRequest(c, store)
		if err != nil {
			return err
		}
		creds, err := store.ListProjectCredentials(c.Request().Context(), ctx.User.ID, c.Param("project_id"))
		if err != nil {
			return echo.NewHTTPError(http.StatusForbidden, "project not available")
		}
		return c.JSON(http.StatusOK, map[string]any{"credentials": creds})
	})

	e.POST("/api/projects/:project_id/credentials", func(c echo.Context) error {
		ctx, err := authFromRequest(c, store)
		if err != nil {
			return err
		}
		var payload struct {
			Name   string   `json:"name"`
			Scopes []string `json:"scopes"`
		}
		if err := c.Bind(&payload); err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, "invalid json")
		}
		cred, secret, err := store.CreateProjectCredential(c.Request().Context(), ctx.User.ID, c.Param("project_id"), payload.Name, payload.Scopes)
		if err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, err.Error())
		}
		// The secret is returned exactly once, here. It is never stored
		// plaintext, never listed, and never logged.
		return c.JSON(http.StatusCreated, map[string]any{"credential": cred, "secret": secret})
	})

	e.DELETE("/api/projects/:project_id/credentials/:credential_id", func(c echo.Context) error {
		ctx, err := authFromRequest(c, store)
		if err != nil {
			return err
		}
		if err := store.RevokeProjectCredential(c.Request().Context(), ctx.User.ID, c.Param("project_id"), c.Param("credential_id")); err != nil {
			return echo.NewHTTPError(http.StatusForbidden, err.Error())
		}
		return c.NoContent(http.StatusNoContent)
	})

	e.POST("/api/projects/:project_id/credential-split", func(c echo.Context) error {
		ctx, err := authFromRequest(c, store)
		if err != nil {
			return err
		}
		if err := store.MarkProjectCredentialSplit(c.Request().Context(), ctx.User.ID, c.Param("project_id")); err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, err.Error())
		}
		return c.JSON(http.StatusOK, map[string]any{"split": true})
	})

	// --- source credentials: session-only, write-only secrets. ---
	// The DSN is stored encrypted and never returned — not on create, not on
	// list. Operations and agents reference the credential by ID only.
	e.GET("/api/projects/:project_id/source-credentials", func(c echo.Context) error {
		ctx, err := authFromRequest(c, store)
		if err != nil {
			return err
		}
		creds, err := store.ListSourceCredentials(c.Request().Context(), ctx.User.ID, c.Param("project_id"))
		if err != nil {
			return echo.NewHTTPError(http.StatusForbidden, "project not available")
		}
		return c.JSON(http.StatusOK, map[string]any{"credentials": creds})
	})

	e.POST("/api/projects/:project_id/source-credentials", func(c echo.Context) error {
		ctx, err := authFromRequest(c, store)
		if err != nil {
			return err
		}
		var payload struct {
			Name string `json:"name"`
			DSN  string `json:"dsn"`
		}
		if err := c.Bind(&payload); err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, "invalid json")
		}
		cred, err := store.CreateSourceCredential(c.Request().Context(), ctx.User.ID, c.Param("project_id"), payload.Name, payload.DSN)
		if err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, err.Error())
		}
		// Metadata only — the secret is stored, never echoed back.
		return c.JSON(http.StatusCreated, map[string]any{"credential": cred})
	})

	e.DELETE("/api/projects/:project_id/source-credentials/:credential_id", func(c echo.Context) error {
		ctx, err := authFromRequest(c, store)
		if err != nil {
			return err
		}
		if err := store.RevokeSourceCredential(c.Request().Context(), ctx.User.ID, c.Param("project_id"), c.Param("credential_id")); err != nil {
			return echo.NewHTTPError(http.StatusForbidden, err.Error())
		}
		return c.NoContent(http.StatusNoContent)
	})
}
