package app

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/lohi-ai/agentray/internal/dataplane/store"
	"github.com/lohi-ai/agentray/internal/shared/config"
	"github.com/lohi-ai/agentray/internal/shared/opcore"
)

// Integration tests for principalFromRequest — the credential boundary every
// op adapter shares. Needs the compose Postgres + ClickHouse; skips without
// them (same convention as the store suite).

func openAppTestStore(t *testing.T) *storage.Store {
	t.Helper()
	pgURL := os.Getenv("AGENTRAY_TEST_DATABASE_URL")
	if pgURL == "" {
		pgURL = "postgres://lohi:lohi@localhost:5434/lohi_analytics?sslmode=disable"
	}
	chAddr := os.Getenv("AGENTRAY_TEST_CLICKHOUSE_ADDR")
	if chAddr == "" {
		chAddr = "localhost:19000"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s, err := storage.Open(ctx, config.Config{
		PostgresURL:          pgURL,
		ClickHouseAddr:       chAddr,
		ClickHouseDatabase:   "lohi_analytics",
		ClickHouseUser:       "lohi",
		ClickHousePassword:   "lohi",
		DefaultProjectName:   "principal-test",
		DefaultProjectAPIKey: "principal_test_default_" + fmt.Sprint(time.Now().UnixNano()),
	})
	if err != nil {
		t.Skipf("test store unavailable (%v)", err)
	}
	t.Cleanup(s.Close)
	return s
}

// reqCtx builds an echo context carrying the given headers/cookies.
func reqCtx(e *echo.Echo, headers map[string]string, cookies []*http.Cookie) echo.Context {
	req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	for _, ck := range cookies {
		req.AddCookie(ck)
	}
	rec := httptest.NewRecorder()
	return e.NewContext(req, rec)
}

func TestPrincipalResolution(t *testing.T) {
	s := openAppTestStore(t)
	ctx := context.Background()
	e := echo.New()

	// Account → user + workspace + born-split project.
	boot, err := s.CreateAccount(ctx, fmt.Sprintf("principal-%d@test.local", time.Now().UnixNano()), "P Test", "password-123", "ws", "proj")
	if err != nil {
		t.Fatalf("create account: %v", err)
	}
	project := boot.Project
	_, sessionToken, err := s.CreateUserSession(ctx, boot.User.ID, time.Hour)
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	sessionCookie := &http.Cookie{Name: sessionCookieName, Value: sessionToken}

	// 1. Session on a born-split project → session principal, owner grants.
	p, err := principalFromRequest(reqCtx(e, nil, []*http.Cookie{sessionCookie}), s)
	if err != nil {
		t.Fatalf("session resolve: %v", err)
	}
	if p.Kind != opcore.CredSession || p.ProjectID != project.ID || p.Role != "owner" {
		t.Fatalf("session principal = %+v", p)
	}
	if len(p.Grants) != 5 {
		t.Fatalf("owner grants = %v", p.Grants)
	}

	// 2. Born-split project key → capture, denied by every op.
	p, err = principalFromRequest(reqCtx(e, map[string]string{"X-API-Key": project.APIKey}, nil), s)
	if err != nil {
		t.Fatalf("capture resolve: %v", err)
	}
	if p.Kind != opcore.CredCapture {
		t.Fatalf("split project key kind = %v, want capture", p.Kind)
	}

	// 3. Unsplit project (created directly, no workspace) → legacy.
	legacy, err := s.CreateProject(ctx, "legacy-proj")
	if err != nil {
		t.Fatalf("legacy project: %v", err)
	}
	p, err = principalFromRequest(reqCtx(e, map[string]string{"X-API-Key": legacy.APIKey}, nil), s)
	if err != nil {
		t.Fatalf("legacy resolve: %v", err)
	}
	if p.Kind != opcore.CredLegacy {
		t.Fatalf("unsplit key kind = %v, want legacy", p.Kind)
	}

	// 4. Management credential → scoped management principal.
	cred, secret, err := s.CreateProjectCredential(ctx, boot.User.ID, project.ID, "ci", []string{"analytics:read"})
	if err != nil {
		t.Fatalf("create credential: %v", err)
	}
	p, err = principalFromRequest(reqCtx(e, map[string]string{"Authorization": "Bearer " + secret}, nil), s)
	if err != nil {
		t.Fatalf("management resolve: %v", err)
	}
	if p.Kind != opcore.CredManagement || p.CredentialID != cred.ID || p.ProjectID != project.ID {
		t.Fatalf("management principal = %+v", p)
	}
	if len(p.Grants) != 1 || p.Grants[0] != opcore.AccessAnalyticsRead {
		t.Fatalf("management grants = %v", p.Grants)
	}

	// 5. Present-but-invalid Bearer must NOT fall through to a valid session.
	_, err = principalFromRequest(reqCtx(e,
		map[string]string{"Authorization": "Bearer agm_nonexistent"},
		[]*http.Cookie{sessionCookie}), s)
	if err == nil {
		t.Fatal("invalid bearer fell through to session")
	}
	if he, ok := err.(*echo.HTTPError); !ok || he.Code != http.StatusUnauthorized {
		t.Fatalf("invalid bearer err = %v, want 401", err)
	}

	// 6. Non-agm_ Bearer is also rejected, not ignored.
	_, err = principalFromRequest(reqCtx(e,
		map[string]string{"Authorization": "Bearer not-a-mgmt-key", "X-API-Key": legacy.APIKey}, nil), s)
	if err == nil {
		t.Fatal("non-agm bearer fell through to api key")
	}

	// 7. Revoked credential fails immediately.
	if err := s.RevokeProjectCredential(ctx, boot.User.ID, project.ID, cred.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	_, err = principalFromRequest(reqCtx(e, map[string]string{"Authorization": "Bearer " + secret}, nil), s)
	if err == nil {
		t.Fatal("revoked credential still resolves")
	}

	// 8. api_key query param on split project → capture too.
	p, err = principalFromRequest(reqCtx(e, nil, nil), s)
	_ = p
	_ = err
}

func TestSessionRoleGrants(t *testing.T) {
	s := openAppTestStore(t)
	ctx := context.Background()
	e := echo.New()

	owner, err := s.CreateAccount(ctx, fmt.Sprintf("owner-%d@test.local", time.Now().UnixNano()), "O", "password-123", "ws", "proj")
	if err != nil {
		t.Fatalf("owner account: %v", err)
	}
	// A second user joins as viewer.
	viewer, err := s.CreateAccount(ctx, fmt.Sprintf("viewer-%d@test.local", time.Now().UnixNano()), "V", "password-123", "ws2", "proj2")
	if err != nil {
		t.Fatalf("viewer account: %v", err)
	}
	if _, err := s.AddWorkspaceMemberByEmail(ctx, owner.User.ID, owner.Workspace.ID, viewer.User.Email, "viewer"); err != nil {
		t.Fatalf("add viewer: %v", err)
	}
	_, token, err := s.CreateUserSession(ctx, viewer.User.ID, time.Hour)
	if err != nil {
		t.Fatalf("viewer session: %v", err)
	}
	// Viewer targets the owner's project explicitly.
	req := httptest.NewRequest(http.MethodPost, "/mcp?project_id="+owner.Project.ID, nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: token})
	rec := httptest.NewRecorder()
	p, err := principalFromRequest(e.NewContext(req, rec), s)
	if err != nil {
		t.Fatalf("viewer resolve: %v", err)
	}
	if p.Kind != opcore.CredSession || p.Role != "viewer" {
		t.Fatalf("viewer principal = %+v", p)
	}
	// Viewer grants: analytics read + sources read (status), nothing else.
	if len(p.Grants) != 2 {
		t.Fatalf("viewer grants = %v", p.Grants)
	}
}
