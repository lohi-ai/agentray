package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeServer mimics the /api/auth/* + rotate-key surface the CLI talks to.
func fakeServer(t *testing.T) *httptest.Server {
	t.Helper()
	const token = "session-token-1"
	project := map[string]any{"id": "proj-1", "workspace_id": "ws-1", "name": "Demo", "api_key": "key-original"}
	payload := func() map[string]any {
		return map[string]any{
			"user":       map[string]any{"id": "u1", "email": "a@example.com", "name": "Alice"},
			"workspaces": []any{map[string]any{"id": "ws-1", "name": "Main"}},
			"projects":   []any{project},
			"project":    project,
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/auth/signup", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["email"] == "" || body["password"] == "" {
			http.Error(w, `{"message":"email and password required"}`, http.StatusBadRequest)
			return
		}
		http.SetCookie(w, &http.Cookie{Name: sessionCookieName, Value: token})
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(payload())
	})
	mux.HandleFunc("POST /api/auth/login", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["password"] != "secret" {
			http.Error(w, `{"message":"invalid email or password"}`, http.StatusUnauthorized)
			return
		}
		http.SetCookie(w, &http.Cookie{Name: sessionCookieName, Value: token})
		_ = json.NewEncoder(w).Encode(payload())
	})
	mux.HandleFunc("GET /api/auth/me", func(w http.ResponseWriter, r *http.Request) {
		if c, err := r.Cookie(sessionCookieName); err != nil || c.Value != token {
			http.Error(w, `{"message":"login required"}`, http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(payload())
	})
	mux.HandleFunc("POST /api/projects/proj-1/rotate-key", func(w http.ResponseWriter, r *http.Request) {
		if c, err := r.Cookie(sessionCookieName); err != nil || c.Value != token {
			http.Error(w, `{"message":"login required"}`, http.StatusUnauthorized)
			return
		}
		project["api_key"] = "key-rotated"
		_ = json.NewEncoder(w).Encode(map[string]any{"project": project})
	})
	mux.HandleFunc("POST /api/auth/logout", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func withTempConfig(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("AGENTRAY_CONFIG_DIR", dir)
	return dir
}

func TestOperationCredentialPrefersBoundManagementKey(t *testing.T) {
	tests := []struct {
		name string
		cfg  cliConfig
		env  string
		want string
	}{
		{
			name: "stored management key outranks capture environment",
			cfg:  cliConfig{ProjectID: "project-a", APIKey: "capture-a", ManagementKey: "agm_stored", ManagementKeyProject: "project-a"},
			env:  "capture-from-key-command",
			want: "agm_stored",
		},
		{
			name: "stale management key is discarded",
			cfg:  cliConfig{ProjectID: "project-b", APIKey: "capture-b", ManagementKey: "agm_stale", ManagementKeyProject: "project-a"},
			env:  "capture-from-env",
			want: "capture-from-env",
		},
		{
			name: "capture config remains legacy fallback",
			cfg:  cliConfig{ProjectID: "project-a", APIKey: "capture-a"},
			want: "capture-a",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := operationCredential(tt.cfg, tt.env); got != tt.want {
				t.Fatalf("operationCredential() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestLoginKeyRotateLogoutFlow(t *testing.T) {
	srv := fakeServer(t)
	dir := withTempConfig(t)

	if err := runAccountCommand(srv.URL, []string{"login", "--email", "a@example.com", "--password", "secret"}); err != nil {
		t.Fatalf("login: %v", err)
	}

	cfg := loadConfig()
	if cfg.SessionToken != "session-token-1" {
		t.Fatalf("session token not persisted: %+v", cfg)
	}
	if cfg.APIKey != "key-original" || cfg.ProjectID != "proj-1" {
		t.Fatalf("default project not persisted: %+v", cfg)
	}
	if cfg.URL != srv.URL {
		t.Fatalf("server url not persisted: %+v", cfg)
	}
	info, err := os.Stat(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatalf("config file: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("config perms = %v, want 0600", info.Mode().Perm())
	}

	// key --rotate must persist and select the fresh key.
	if err := runAccountCommand(srv.URL, []string{"key", "--rotate"}); err != nil {
		t.Fatalf("key --rotate: %v", err)
	}
	if cfg = loadConfig(); cfg.APIKey != "key-rotated" {
		t.Fatalf("rotated key not persisted: %+v", cfg)
	}

	// whoami and projects should succeed with the stored session.
	if err := runAccountCommand(srv.URL, []string{"whoami"}); err != nil {
		t.Fatalf("whoami: %v", err)
	}
	if err := runAccountCommand(srv.URL, []string{"projects"}); err != nil {
		t.Fatalf("projects: %v", err)
	}

	if err := runAccountCommand(srv.URL, []string{"logout"}); err != nil {
		t.Fatalf("logout: %v", err)
	}
	if cfg = loadConfig(); cfg.SessionToken != "" || cfg.APIKey != "" {
		t.Fatalf("logout must clear credentials: %+v", cfg)
	}
	if cfg.URL != srv.URL {
		t.Fatalf("logout should keep the server url: %+v", cfg)
	}
}

func TestSignupPersistsSessionAndKey(t *testing.T) {
	srv := fakeServer(t)
	withTempConfig(t)

	err := runAccountCommand(srv.URL, []string{
		"signup", "--email", "a@example.com", "--password", "secret", "--workspace", "Main", "--project", "Demo",
	})
	if err != nil {
		t.Fatalf("signup: %v", err)
	}
	cfg := loadConfig()
	if cfg.SessionToken == "" || cfg.APIKey != "key-original" || cfg.Email != "a@example.com" {
		t.Fatalf("signup state not persisted: %+v", cfg)
	}
	// Name defaulting: not sent empty (server rejects) — covered by fake requiring email+password only.
}

func TestLoginBadPassword(t *testing.T) {
	srv := fakeServer(t)
	withTempConfig(t)

	err := runAccountCommand(srv.URL, []string{"login", "--email", "a@example.com", "--password", "wrong"})
	if err == nil {
		t.Fatal("expected login failure")
	}
	if cfg := loadConfig(); cfg.SessionToken != "" {
		t.Fatalf("failed login must not persist a session: %+v", cfg)
	}
}

func TestCommandsRequireLogin(t *testing.T) {
	srv := fakeServer(t)
	withTempConfig(t)

	for _, cmd := range [][]string{{"whoami"}, {"key"}, {"projects"}} {
		err := runAccountCommand(srv.URL, cmd)
		if err == nil || !strings.Contains(err.Error(), "not logged in") {
			t.Fatalf("%v: want not-logged-in error, got %v", cmd, err)
		}
	}
}

func TestPickProjectBySelector(t *testing.T) {
	var payload accountPayload
	payload.Projects = []accountProject{
		{ID: "p1", Name: "Web"},
		{ID: "p2", Name: "Mobile"},
	}
	if p, err := pickProject(payload, "mobile"); err != nil || p.ID != "p2" {
		t.Fatalf("name match failed: %+v %v", p, err)
	}
	if p, err := pickProject(payload, "p1"); err != nil || p.Name != "Web" {
		t.Fatalf("id match failed: %+v %v", p, err)
	}
	if _, err := pickProject(payload, "nope"); err == nil {
		t.Fatal("expected error for unknown selector")
	}
	if p, err := pickProject(payload, ""); err != nil || p.ID != "p1" {
		t.Fatalf("empty selector should fall back to first project: %+v %v", p, err)
	}
}

// Ops key precedence: the stored management credential must outrank
// AGENTRAY_API_KEY — that env is the documented SDK flow and holds the
// capture key, which every op adapter denies. The env still beats the saved
// capture key, and a management credential minted under another project is
// never reused.
func TestOpKeyPrecedence(t *testing.T) {
	cfg := cliConfig{
		ProjectID:            "p1",
		APIKey:               "capture-saved",
		ManagementKey:        "agm_mgmt",
		ManagementKeyProject: "p1",
	}
	if got := operationCredential(cfg, "capture-env"); got != "agm_mgmt" {
		t.Fatalf("management key must outrank AGENTRAY_API_KEY, got %q", got)
	}
	if got := operationCredential(cfg, ""); got != "agm_mgmt" {
		t.Fatalf("management key should be the default, got %q", got)
	}
	// No management credential: env beats the saved capture key.
	cfg.ManagementKey = ""
	cfg.ManagementKeyProject = ""
	if got := operationCredential(cfg, "capture-env"); got != "capture-env" {
		t.Fatalf("env should beat saved capture key, got %q", got)
	}
	if got := operationCredential(cfg, ""); got != "capture-saved" {
		t.Fatalf("saved capture key is the last resort, got %q", got)
	}
	// A credential minted under a different project is dropped, not reused.
	cfg.ManagementKey = "agm_stale"
	cfg.ManagementKeyProject = "p2"
	if got := operationCredential(cfg, "capture-env"); got != "capture-env" {
		t.Fatalf("stale management key must not be reused, got %q", got)
	}
}
