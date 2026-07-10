package app

import (
	"net/http"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/lohi-ai/agentray/internal/storage"
)

const sessionCookieName = "agentray_session"
const sessionTTL = 14 * 24 * time.Hour

type authContext struct {
	User    storage.User
	Session storage.UserSession
}

func authFromRequest(c echo.Context, store *storage.Store) (authContext, error) {
	cookie, err := c.Cookie(sessionCookieName)
	if err != nil || cookie.Value == "" {
		return authContext{}, echo.NewHTTPError(http.StatusUnauthorized, "login required")
	}
	user, session, err := store.UserBySessionToken(c.Request().Context(), cookie.Value)
	if err != nil {
		return authContext{}, echo.NewHTTPError(http.StatusUnauthorized, "login required")
	}
	return authContext{User: user, Session: session}, nil
}

func setSessionCookie(c echo.Context, token string, expiresAt time.Time) {
	c.SetCookie(&http.Cookie{
		Name:     sessionCookieName,
		Value:    token,
		Path:     "/",
		Expires:  expiresAt,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   c.IsTLS(),
	})
}

func clearSessionCookie(c echo.Context) {
	c.SetCookie(&http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		Expires:  time.Unix(0, 0),
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   c.IsTLS(),
	})
}

func authPayload(ctx authContext, workspaces []storage.Workspace, projects []storage.Project, project storage.Project) map[string]any {
	return map[string]any{
		"user":               ctx.User,
		"session_expires_at": ctx.Session.ExpiresAt,
		"workspaces":         workspaces,
		"projects":           projects,
		"project":            project,
	}
}
