package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/lohi-ai/agentray/internal/dataplane/store"
	"github.com/lohi-ai/agentray/internal/dataplane/usecase"
	"github.com/lohi-ai/agentray/internal/shared/opcore"
)

// fakeProject is one project the fake server serves.
type fakeProject struct {
	ID     string
	Name   string
	APIKey string
}

// fakeCred is one management credential the fake server hands out or lists.
type fakeCred struct {
	ID      string
	Project string
	Name    string
	Secret  string
	Live    bool
}

// hint mirrors the server's key_hint: the last four characters of the secret,
// which is the only handle a config written before ids existed carries.
func (c fakeCred) hint() string {
	if len(c.Secret) < 4 {
		return c.Secret
	}
	return c.Secret[len(c.Secret)-4:]
}

// fakeAgentRay stands in for the account API and the scoped-credential surface.
// Tests drive it by mutating fields between calls: the workspace role the login
// payload reports, whether the mint is refused, and whether the delete route
// answers. Every credential request is recorded so a test can assert the order
// of the lifecycle (revoke before session, never revoke what the list disproves).
type fakeAgentRay struct {
	*httptest.Server

	mu           sync.Mutex
	role         string
	mintStatus   int
	mintBody     string
	mintScopes   []string
	mintCalls    int
	deleteStatus int
	events       []string
	creds        []fakeCred
	projects     []fakeProject
}

func (f *fakeAgentRay) record(event string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, event)
}

// eventIndex returns the position of an event in the request trace, or -1.
func (f *fakeAgentRay) eventIndex(event string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, e := range f.events {
		if e == event {
			return i
		}
	}
	return -1
}

func (f *fakeAgentRay) requestedScopes() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.mintScopes...)
}

func (f *fakeAgentRay) mintedCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.mintCalls
}

func (f *fakeAgentRay) deleted() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var ids []string
	for _, event := range f.events {
		if id, ok := strings.CutPrefix(event, "delete:"); ok {
			ids = append(ids, id)
		}
	}
	return ids
}

func (f *fakeAgentRay) seedCred(c fakeCred) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.creds = append(f.creds, c)
}

// authPayload is the body /api/auth/{login,signup,me} answers with.
func (f *fakeAgentRay) authPayload() map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	projects := make([]map[string]any, 0, len(f.projects))
	for _, p := range f.projects {
		projects = append(projects, map[string]any{
			"id": p.ID, "workspace_id": "ws-1", "name": p.Name, "api_key": p.APIKey, "role": f.role,
		})
	}
	return map[string]any{
		"user":       map[string]any{"id": "u1", "email": "a@example.com", "name": "Alice"},
		"workspaces": []any{map[string]any{"id": "ws-1", "name": "Main"}},
		"projects":   projects,
		"project":    projects[0],
	}
}

func writeJSONError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"message": message})
}

// fakeServer mimics the /api/auth/*, rotate-key and scoped-credential surface
// the CLI talks to. The default account is an owner of two projects.
func fakeServer(t *testing.T) *fakeAgentRay {
	t.Helper()
	const token = "session-token-1"
	f := &fakeAgentRay{
		role: "owner",
		projects: []fakeProject{
			{ID: "proj-1", Name: "Demo", APIKey: "key-original"},
			{ID: "proj-2", Name: "Mobile", APIKey: "key-mobile"},
		},
	}
	requireSession := func(w http.ResponseWriter, r *http.Request) bool {
		if c, err := r.Cookie(sessionCookieName); err != nil || c.Value != token {
			writeJSONError(w, http.StatusUnauthorized, "login required")
			return false
		}
		return true
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/auth/signup", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["email"] == "" || body["password"] == "" {
			writeJSONError(w, http.StatusBadRequest, "email and password required")
			return
		}
		http.SetCookie(w, &http.Cookie{Name: sessionCookieName, Value: token})
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(f.authPayload())
	})
	mux.HandleFunc("POST /api/auth/login", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["password"] != "secret" {
			writeJSONError(w, http.StatusUnauthorized, "invalid email or password")
			return
		}
		http.SetCookie(w, &http.Cookie{Name: sessionCookieName, Value: token})
		_ = json.NewEncoder(w).Encode(f.authPayload())
	})
	mux.HandleFunc("GET /api/auth/me", func(w http.ResponseWriter, r *http.Request) {
		if !requireSession(w, r) {
			return
		}
		_ = json.NewEncoder(w).Encode(f.authPayload())
	})
	mux.HandleFunc("POST /api/auth/logout", func(w http.ResponseWriter, r *http.Request) {
		f.record("logout")
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/projects/{project}/rotate-key", func(w http.ResponseWriter, r *http.Request) {
		if !requireSession(w, r) {
			return
		}
		id := r.PathValue("project")
		f.mu.Lock()
		for i := range f.projects {
			if f.projects[i].ID == id {
				f.projects[i].APIKey = "key-rotated"
			}
		}
		var rotated map[string]any
		for _, p := range f.projects {
			if p.ID == id {
				rotated = map[string]any{"id": p.ID, "workspace_id": "ws-1", "name": p.Name, "api_key": p.APIKey, "role": f.role}
			}
		}
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"project": rotated})
	})
	mux.HandleFunc("POST /api/projects/{project}/credentials", func(w http.ResponseWriter, r *http.Request) {
		if !requireSession(w, r) {
			return
		}
		var body struct {
			Name   string   `json:"name"`
			Scopes []string `json:"scopes"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.mintCalls++
		f.mintScopes = append([]string(nil), body.Scopes...)
		status, message := f.mintStatus, f.mintBody
		f.mu.Unlock()
		if status != 0 {
			writeJSONError(w, status, message)
			return
		}
		project := r.PathValue("project")
		cred := fakeCred{ID: "cred-" + project, Project: project, Name: body.Name, Secret: "agm_secret_" + project, Live: true}
		f.seedCred(cred)
		f.record("mint:" + cred.ID)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"credential": map[string]any{"id": cred.ID, "name": cred.Name, "scopes": body.Scopes, "key_hint": cred.hint()},
			"secret":     cred.Secret,
		})
	})
	mux.HandleFunc("GET /api/projects/{project}/credentials", func(w http.ResponseWriter, r *http.Request) {
		if !requireSession(w, r) {
			return
		}
		project := r.PathValue("project")
		f.mu.Lock()
		rows := make([]map[string]any, 0, len(f.creds))
		for _, c := range f.creds {
			if c.Project != project {
				continue
			}
			row := map[string]any{"id": c.ID, "name": c.Name, "key_hint": c.hint()}
			if !c.Live {
				row["revoked_at"] = "2026-09-13T00:00:00Z"
			}
			rows = append(rows, row)
		}
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"credentials": rows})
	})
	mux.HandleFunc("DELETE /api/projects/{project}/credentials/{cred}", func(w http.ResponseWriter, r *http.Request) {
		if !requireSession(w, r) {
			return
		}
		f.mu.Lock()
		status := f.deleteStatus
		f.mu.Unlock()
		if status != 0 {
			// The real route answers every revocation failure with one status,
			// so "already revoked" and "refused" are indistinguishable here.
			writeJSONError(w, status, "agent config permission denied")
			return
		}
		id := r.PathValue("cred")
		f.mu.Lock()
		found := false
		for i := range f.creds {
			if f.creds[i].ID == id {
				f.creds[i].Live = false
				found = true
			}
		}
		f.mu.Unlock()
		if !found {
			writeJSONError(w, http.StatusForbidden, "credential not found or already revoked")
			return
		}
		f.record("delete:" + id)
		w.WriteHeader(http.StatusNoContent)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	f.Server = srv
	return f
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

func (f *fakeAgentRay) trace() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.events...)
}

// captureStderr runs fn with os.Stderr redirected, returning what a user would
// have read on the terminal.
func captureStderr(t *testing.T, fn func() error) (string, error) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	old := os.Stderr
	os.Stderr = w
	runErr := fn()
	os.Stderr = old
	if err := w.Close(); err != nil {
		t.Fatalf("close stderr: %v", err)
	}
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read stderr: %v", err)
	}
	return string(out), runErr
}

// seedConfig writes the state a previous invocation would have left behind.
func seedConfig(t *testing.T, cfg cliConfig) {
	t.Helper()
	if err := saveConfig(cfg); err != nil {
		t.Fatalf("seed config: %v", err)
	}
}

// trackedConfig is a logged-in CLI holding a management credential minted for
// proj-1, the state every switch and logout test starts from.
func trackedConfig(srv *fakeAgentRay) cliConfig {
	return cliConfig{
		URL:                  srv.URL,
		SessionToken:         "session-token-1",
		Email:                "a@example.com",
		ProjectID:            "proj-1",
		ProjectName:          "Demo",
		APIKey:               "key-original",
		ManagementKey:        "agm_old_secret",
		ManagementKeyProject: "proj-1",
		ManagementKeyID:      "cred-old",
	}
}

// Acceptance 1: credential creation is owner/admin-only, so a member's mint is
// refused — the session the API issued must survive it, and the CLI must say
// plainly what this role cannot do instead of failing the login.
func TestMemberLoginPersistsSessionWithoutMintRights(t *testing.T) {
	srv := fakeServer(t)
	dir := withTempConfig(t)
	srv.role = "member"
	srv.mintStatus = http.StatusBadRequest
	srv.mintBody = "agent config permission denied"

	notice, err := captureStderr(t, func() error {
		return runAccountCommand(srv.URL, []string{"login", "--email", "a@example.com", "--password", "secret"})
	})
	if err != nil {
		t.Fatalf("login must complete without mint rights: %v", err)
	}
	cfg := loadConfig()
	if cfg.SessionToken != "session-token-1" || cfg.ProjectID != "proj-1" || cfg.APIKey != "key-original" {
		t.Fatalf("session and project not persisted for a member: %+v", cfg)
	}
	if cfg.ManagementKey != "" || cfg.ManagementKeyProject != "" {
		t.Fatalf("a refused mint must not leave a credential behind: %+v", cfg)
	}
	if !strings.Contains(notice, "no management credential for project Demo") || !strings.Contains(notice, "cannot mint one") {
		t.Fatalf("login must state the role's limit, got:\n%s", notice)
	}
	info, err := os.Stat(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatalf("config file: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("config perms = %v, want 0600", info.Mode().Perm())
	}
	if srv.mintedCount() != 1 {
		t.Fatalf("mint attempts = %d, want 1", srv.mintedCount())
	}
}

// Acceptance 2: one credential must cover every operation the CLI can dispatch
// — including the growth/plans writes the retired four-scope literal missed
// (submit_recommendation, propose_test, update_test, record_outcome,
// abandon_test, remember, send_notification) — and every scope asked for must
// be one the server will actually mint.
func TestCLIMintScopesCoverRegistry(t *testing.T) {
	scopes, err := cliMintScopes()
	if err != nil {
		t.Fatalf("derive mint scopes: %v", err)
	}
	grants := make([]opcore.Access, 0, len(scopes))
	for _, scope := range scopes {
		if !storage.ManagementScopes[scope] {
			t.Errorf("requested scope %q is not mintable by the server", scope)
		}
		grants = append(grants, opcore.Access(scope))
	}
	reg := usecase.Registry()
	principal := opcore.Principal{Kind: opcore.CredManagement, Grants: grants}
	for _, spec := range reg.Specs() {
		if !reg.Authorize(principal, spec.OpName()) {
			t.Errorf("minted credential cannot call %s (access %q)", spec.OpName(), spec.OpAccess())
		}
	}
}

// Acceptance 2, on the wire: login asks for exactly the registry's access
// classes and stores the credential the server returns.
func TestLoginMintsRegistryScopes(t *testing.T) {
	srv := fakeServer(t)
	withTempConfig(t)

	if err := runAccountCommand(srv.URL, []string{"login", "--email", "a@example.com", "--password", "secret"}); err != nil {
		t.Fatalf("login: %v", err)
	}
	want := make([]string, 0)
	for _, spec := range usecase.Registry().Specs() {
		want = append(want, string(spec.OpAccess()))
	}
	sort.Strings(want)
	want = slices.Compact(want)
	got := srv.requestedScopes()
	sort.Strings(got)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("mint requested %v, want the registry's access classes %v", got, want)
	}
	cfg := loadConfig()
	if cfg.ManagementKey != "agm_secret_proj-1" || cfg.ManagementKeyID != "cred-proj-1" || cfg.ManagementKeyProject != "proj-1" {
		t.Fatalf("minted credential not persisted: %+v", cfg)
	}
}

// A server with no credential surface leaves the project key carrying
// operations, exactly as it did before the split.
func TestLegacyServerLoginKeepsProjectKeyPath(t *testing.T) {
	srv := fakeServer(t)
	withTempConfig(t)
	srv.mintStatus = http.StatusNotFound
	srv.mintBody = "not found"

	notice, err := captureStderr(t, func() error {
		return runAccountCommand(srv.URL, []string{"login", "--email", "a@example.com", "--password", "secret"})
	})
	if err != nil {
		t.Fatalf("login against a legacy server: %v", err)
	}
	cfg := loadConfig()
	if cfg.ManagementKey != "" || cfg.SessionToken != "session-token-1" || cfg.APIKey != "key-original" {
		t.Fatalf("legacy login state wrong: %+v", cfg)
	}
	if !strings.Contains(notice, "predates scoped credentials") {
		t.Fatalf("legacy login must explain the fallback, got:\n%s", notice)
	}
}

// Acceptance 3: the credential bound to the project being left is revoked
// before the new selection replaces it.
func TestProjectSwitchRevokesAbandonedCredential(t *testing.T) {
	srv := fakeServer(t)
	withTempConfig(t)
	srv.seedCred(fakeCred{ID: "cred-old", Project: "proj-1", Name: cliCredentialName, Secret: "agm_old_secret", Live: true})
	seedConfig(t, trackedConfig(srv))

	if err := runAccountCommand(srv.URL, []string{"key", "--project", "Mobile"}); err != nil {
		t.Fatalf("key --project: %v", err)
	}
	if deleted := srv.deleted(); len(deleted) != 1 || deleted[0] != "cred-old" {
		t.Fatalf("abandoned credential not revoked: %v", deleted)
	}
	cfg := loadConfig()
	if cfg.ProjectID != "proj-2" || cfg.APIKey != "key-mobile" {
		t.Fatalf("switch not saved: %+v", cfg)
	}
	if cfg.ManagementKey != "agm_secret_proj-2" || cfg.ManagementKeyID != "cred-proj-2" || cfg.ManagementKeyProject != "proj-2" {
		t.Fatalf("credential not replaced by the new project's: %+v", cfg)
	}
}

// Acceptance 3, failure path: a revoke that cannot be proved must abort the
// switch with the live credential still tracked in the config.
func TestProjectSwitchPreservesTrackedCredentialOnRevokeFailure(t *testing.T) {
	srv := fakeServer(t)
	withTempConfig(t)
	srv.seedCred(fakeCred{ID: "cred-old", Project: "proj-1", Name: cliCredentialName, Secret: "agm_old_secret", Live: true})
	srv.deleteStatus = http.StatusForbidden
	seedConfig(t, trackedConfig(srv))

	err := runAccountCommand(srv.URL, []string{"key", "--project", "Mobile"})
	if err == nil || !strings.Contains(err.Error(), "still live") {
		t.Fatalf("switch must abort while the credential is live, got %v", err)
	}
	cfg := loadConfig()
	if cfg.ProjectID != "proj-1" || cfg.APIKey != "key-original" {
		t.Fatalf("failed switch must not move the selection: %+v", cfg)
	}
	if cfg.ManagementKey != "agm_old_secret" || cfg.ManagementKeyID != "cred-old" || cfg.ManagementKeyProject != "proj-1" {
		t.Fatalf("the live credential must stay tracked: %+v", cfg)
	}
	if srv.mintedCount() != 0 {
		t.Fatal("no credential may be minted while the old one is still live")
	}
}

// The delete route answers one status for a refusal and for an already-revoked
// row, so the member-readable list decides: a revoked row means it is gone.
func TestRevokeAcceptedWhenListShowsCredentialAlreadyRevoked(t *testing.T) {
	srv := fakeServer(t)
	withTempConfig(t)
	srv.seedCred(fakeCred{ID: "cred-old", Project: "proj-1", Name: cliCredentialName, Secret: "agm_old_secret"})
	srv.deleteStatus = http.StatusForbidden
	seedConfig(t, trackedConfig(srv))

	if err := runAccountCommand(srv.URL, []string{"key", "--project", "Mobile"}); err != nil {
		t.Fatalf("key --project: %v", err)
	}
	cfg := loadConfig()
	if cfg.ProjectID != "proj-2" || cfg.ManagementKeyProject != "proj-2" {
		t.Fatalf("switch must proceed once the credential is provably gone: %+v", cfg)
	}
}

// A config written before ids were stored carries only the key hint: the CLI
// resolves the live row the server shows and revokes that id.
func TestLegacyConfigResolvesAbandonedCredentialByKeyHint(t *testing.T) {
	srv := fakeServer(t)
	withTempConfig(t)
	srv.seedCred(fakeCred{ID: "cred-legacy", Project: "proj-1", Name: cliCredentialName, Secret: "agm_old_secret", Live: true})
	cfg := trackedConfig(srv)
	cfg.ManagementKeyID = ""
	seedConfig(t, cfg)

	if err := runAccountCommand(srv.URL, []string{"key", "--project", "Mobile"}); err != nil {
		t.Fatalf("key --project: %v", err)
	}
	if deleted := srv.deleted(); len(deleted) != 1 || deleted[0] != "cred-legacy" {
		t.Fatalf("resolved credential not revoked: %v", deleted)
	}
}

// The hint is four characters, so it can collide. Two live matches mean the CLI
// cannot prove which credential it holds — it revokes neither and keeps the key.
func TestLegacyConfigAmbiguityAbortsSwitch(t *testing.T) {
	srv := fakeServer(t)
	withTempConfig(t)
	srv.seedCred(fakeCred{ID: "cred-a", Project: "proj-1", Name: cliCredentialName, Secret: "agm_old_secret", Live: true})
	srv.seedCred(fakeCred{ID: "cred-b", Project: "proj-1", Name: cliCredentialName, Secret: "agm_other_secret", Live: true})
	cfg := trackedConfig(srv)
	cfg.ManagementKeyID = ""
	seedConfig(t, cfg)

	err := runAccountCommand(srv.URL, []string{"key", "--project", "Mobile"})
	if err == nil || !strings.Contains(err.Error(), "share the key hint") {
		t.Fatalf("an ambiguous match must abort, got %v", err)
	}
	if deleted := srv.deleted(); len(deleted) != 0 {
		t.Fatalf("nothing may be revoked on an ambiguous match: %v", deleted)
	}
	stored := loadConfig()
	if stored.ManagementKey != "agm_old_secret" || stored.ProjectID != "proj-1" {
		t.Fatalf("config must be untouched: %+v", stored)
	}
}

// Acceptance 4: the management credential is revoked before the session, since
// the session is what authorizes the revoke.
func TestLogoutRevokesManagementCredentialBeforeSession(t *testing.T) {
	srv := fakeServer(t)
	withTempConfig(t)
	srv.seedCred(fakeCred{ID: "cred-old", Project: "proj-1", Name: cliCredentialName, Secret: "agm_old_secret", Live: true})
	seedConfig(t, trackedConfig(srv))

	if err := runAccountCommand(srv.URL, []string{"logout"}); err != nil {
		t.Fatalf("logout: %v", err)
	}
	revokedAt, sessionAt := srv.eventIndex("delete:cred-old"), srv.eventIndex("logout")
	if revokedAt < 0 || sessionAt < 0 || revokedAt > sessionAt {
		t.Fatalf("credential must be revoked before the session, trace: %v", srv.trace())
	}
	cfg := loadConfig()
	if cfg.SessionToken != "" || cfg.APIKey != "" || cfg.ManagementKey != "" || cfg.ManagementKeyID != "" {
		t.Fatalf("logout must clear credentials: %+v", cfg)
	}
	if cfg.URL != srv.URL {
		t.Fatalf("logout should keep the server url: %+v", cfg)
	}
}

// Acceptance 4, failure path: a logout that cannot revoke keeps everything it
// would have to abandon, so the retry is one command away.
func TestLogoutPreservesCredentialsOnRevokeFailure(t *testing.T) {
	srv := fakeServer(t)
	withTempConfig(t)
	srv.seedCred(fakeCred{ID: "cred-old", Project: "proj-1", Name: cliCredentialName, Secret: "agm_old_secret", Live: true})
	srv.deleteStatus = http.StatusForbidden
	seedConfig(t, trackedConfig(srv))

	err := runAccountCommand(srv.URL, []string{"logout"})
	if err == nil || !strings.Contains(err.Error(), "still live") {
		t.Fatalf("logout must stop while the credential is live, got %v", err)
	}
	cfg := loadConfig()
	if cfg.SessionToken != "session-token-1" || cfg.ManagementKey != "agm_old_secret" || cfg.ManagementKeyID != "cred-old" {
		t.Fatalf("credentials must be preserved for retry: %+v", cfg)
	}
	if srv.eventIndex("logout") >= 0 {
		t.Fatal("the session must stay valid until the credential is revoked")
	}
}

// A credential minted during login that cannot be persisted must not be left
// live and untracked: the CLI revokes it before reporting the save failure.
func TestLoginRevokesMintedCredentialWhenConfigSaveFails(t *testing.T) {
	srv := fakeServer(t)
	dir := withTempConfig(t)
	// config.json as a directory makes every saveConfig fail.
	if err := os.Mkdir(filepath.Join(dir, "config.json"), 0o700); err != nil {
		t.Fatalf("seed unwritable config: %v", err)
	}

	err := runAccountCommand(srv.URL, []string{"login", "--email", "a@example.com", "--password", "secret"})
	if err == nil {
		t.Fatal("login must report the failed save")
	}
	if srv.mintedCount() != 1 {
		t.Fatalf("mint attempts = %d, want 1", srv.mintedCount())
	}
	if deleted := srv.deleted(); len(deleted) != 1 || deleted[0] != "cred-proj-1" {
		t.Fatalf("the unpersisted credential must be revoked, not orphaned: %v", deleted)
	}
}
