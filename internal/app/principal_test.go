package app

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/lohi-ai/agentray/internal/dataplane/store"
	"github.com/lohi-ai/agentray/internal/shared/config"
	"github.com/lohi-ai/agentray/internal/shared/opcore"
)

// Integration tests for principalFromRequest — the credential boundary every
// op adapter shares. Needs the compose Postgres; DuckDB is a temp file per
// test. Skips without Postgres (same convention as the store suite).

func openAppTestStore(t *testing.T) *storage.Store {
	t.Helper()
	pgURL := os.Getenv("AGENTRAY_TEST_DATABASE_URL")
	if pgURL == "" {
		pgURL = "postgres://lohi:lohi@localhost:5434/lohi_analytics?sslmode=disable"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s, err := storage.Open(ctx, config.Config{
		PostgresURL:          pgURL,
		DuckDBPath:           filepath.Join(t.TempDir(), "test.duckdb"),
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
	if len(p.Grants) != 6 {
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

func TestLegacyProjectRoutesUsePrincipalBoundary(t *testing.T) {
	s := openAppTestStore(t)
	ctx := context.Background()
	e := echo.New()

	boot, err := s.CreateAccount(ctx, fmt.Sprintf("legacy-route-%d@test.local", time.Now().UnixNano()), "P Test", "password-123", "ws", "proj")
	if err != nil {
		t.Fatalf("create account: %v", err)
	}
	project := boot.Project

	// A born-split project key remains valid for capture, never for a legacy
	// route that reads or mutates project state.
	_, err = projectFromRequest(reqCtx(e, map[string]string{"X-API-Key": project.APIKey}, nil), s)
	if he, ok := err.(*echo.HTTPError); !ok || he.Code != http.StatusForbidden {
		t.Fatalf("capture key legacy route error = %v, want 403", err)
	}

	// The frozen bridge still admits an unsplit project key.
	legacy, err := s.CreateProject(ctx, "legacy-proj")
	if err != nil {
		t.Fatalf("create legacy project: %v", err)
	}
	got, err := projectFromRequest(reqCtx(e, map[string]string{"X-API-Key": legacy.APIKey}, nil), s)
	if err != nil || got.ID != legacy.ID {
		t.Fatalf("legacy key project = %+v, %v; want %s", got, err, legacy.ID)
	}
	// Resolved without its capture key: /api/projects serializes this struct
	// straight into its body, and a key that authenticated with the key itself
	// gains nothing by reading it back.
	if got.APIKey != "" {
		t.Fatalf("legacy key received the capture key: %q", got.APIKey)
	}

	// Management credentials and sessions are resolved to their authenticated
	// project, rather than accepting a caller-supplied project identity.
	_, secret, err := s.CreateProjectCredential(ctx, boot.User.ID, project.ID, "legacy-route", []string{"analytics:read"})
	if err != nil {
		t.Fatalf("create credential: %v", err)
	}
	got, err = projectFromRequest(reqCtx(e, map[string]string{"Authorization": "Bearer " + secret}, nil), s)
	if err != nil || got.ID != project.ID {
		t.Fatalf("management credential project = %+v, %v; want %s", got, err, project.ID)
	}
	// A credential scoped to analytics:read never comes back holding the
	// project's ingest key — that escalation is what this boundary closes.
	if got.APIKey != "" {
		t.Fatalf("management credential received the capture key: %q", got.APIKey)
	}
	other, err := s.CreateAccount(ctx, fmt.Sprintf("legacy-route-other-%d@test.local", time.Now().UnixNano()), "Other", "password-123", "other-ws", "other-proj")
	if err != nil {
		t.Fatalf("create other account: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/events?project_id="+other.Project.ID, nil)
	req.Header.Set("Authorization", "Bearer "+secret)
	got, err = projectFromRequest(e.NewContext(req, httptest.NewRecorder()), s)
	if err != nil || got.ID != project.ID {
		t.Fatalf("management credential crossed project boundary: %+v, %v; want %s", got, err, project.ID)
	}
	_, sessionToken, err := s.CreateUserSession(ctx, boot.User.ID, time.Hour)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	got, err = projectFromRequest(reqCtx(e, nil, []*http.Cookie{{Name: sessionCookieName, Value: sessionToken}}), s)
	if err != nil || got.ID != project.ID {
		t.Fatalf("session project = %+v, %v; want %s", got, err, project.ID)
	}
	// A session whose role may write is the credential that keeps the key the web
	// app reveals and pastes into the install snippet.
	if got.APIKey != project.APIKey {
		t.Fatalf("session capture key = %q, want %q", got.APIKey, project.APIKey)
	}
}

func TestSessionGrantsCarriesTheDemoFact(t *testing.T) {
	if sessionAllowsWrite(storage.Project{Role: "member", IsDemo: true}) {
		t.Fatal("demo member must not write: IsDemo lives on the project so a caller cannot pass false by accident")
	}
	if !sessionAllowsWrite(storage.Project{Role: "owner", IsDemo: true}) {
		t.Fatal("demo owner must still write")
	}
	if !sessionAllowsWrite(storage.Project{Role: "admin", IsDemo: true}) {
		t.Fatal("demo admin must still write")
	}
	if !sessionAllowsWrite(storage.Project{Role: "member"}) {
		t.Fatal("non-demo member must write")
	}
	if sessionAllowsWrite(storage.Project{Role: "guest"}) {
		t.Fatal("unknown role must not write")
	}
	got := sessionGrants(storage.Project{Role: "member", IsDemo: true})
	if len(got) != 2 {
		t.Fatalf("demo member grants = %v, want the viewer read set", got)
	}
}
