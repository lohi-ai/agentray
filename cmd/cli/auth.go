// Account commands: signup, login, logout, whoami, key, projects.
//
// These wrap the server's session-cookie auth (/api/auth/*) so an agent — or a
// human — can go from nothing to a working project API key without opening the
// web app:
//
//	agentray signup --email a@example.com --name Alice     # prompts for password
//	agentray login  --email a@example.com
//	agentray key                                           # prints the project API key
//	export AGENTRAY_API_KEY=$(agentray key)
//
// The session token and default project key persist in ~/.agentray/config.json
// (0600). Non-interactive callers pass --password or AGENTRAY_PASSWORD; `key`
// prints the bare key on stdout so it composes with $(...) while all prose goes
// to stderr.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/lohi-ai/agentray/internal/dataplane/usecase"
)

const sessionCookieName = "agentray_session"

// cliConfig is the persisted state in ~/.agentray/config.json.
type cliConfig struct {
	URL          string `json:"url,omitempty"`
	SessionToken string `json:"session_token,omitempty"`
	Email        string `json:"email,omitempty"`
	ProjectID    string `json:"project_id,omitempty"`
	ProjectName  string `json:"project_name,omitempty"`
	// APIKey is the CAPTURE key — it feeds events and nothing else on a
	// split project. ManagementKey is the scoped agm_ credential the CLI
	// uses for /api/op and /mcp; it is what `key` prints for ops use.
	APIKey        string `json:"api_key,omitempty"`
	ManagementKey string `json:"management_key,omitempty"`
	// ManagementKeyProject binds the credential to its project — a stale key
	// from a previous selection must never authenticate ops against the wrong
	// project.
	ManagementKeyProject string `json:"management_key_project,omitempty"`
	// ManagementKeyID is that credential's server-side id, the handle
	// revocation takes. Configs written before it existed resolve their id
	// from the credential list by key hint (see resolveCredentialID).
	ManagementKeyID string `json:"management_key_id,omitempty"`
}

// cliCredentialName is the name every CLI-minted management credential carries.
// It keeps the CLI's credential findable in the project's credential list —
// which is how a config without a stored id is resolved, and why the resolver
// refuses to touch a differently-named row.
const cliCredentialName = "cli"

func configPath() string {
	if dir := os.Getenv("AGENTRAY_CONFIG_DIR"); dir != "" {
		return filepath.Join(dir, "config.json")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".agentray-config.json"
	}
	return filepath.Join(home, ".agentray", "config.json")
}

func loadConfig() cliConfig {
	var cfg cliConfig
	b, err := os.ReadFile(configPath())
	if err != nil {
		return cfg
	}
	_ = json.Unmarshal(b, &cfg)
	return cfg
}

func saveConfig(cfg cliConfig) error {
	path := configPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o600)
}

// --- server payload shapes (subset of authPayload) --------------------------

type accountProject struct {
	ID          string `json:"id"`
	WorkspaceID string `json:"workspace_id"`
	Name        string `json:"name"`
	APIKey      string `json:"api_key"`
	// Role is the caller's workspace role on this project ("owner", "admin",
	// "member", "viewer", "" on key-authenticated paths). Credential minting is
	// owner/admin-only, so the role is what lets login explain a refusal
	// instead of reporting it as a fault.
	Role string `json:"role"`
}

type accountPayload struct {
	User struct {
		ID    string `json:"id"`
		Email string `json:"email"`
		Name  string `json:"name"`
	} `json:"user"`
	Workspaces []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"workspaces"`
	Projects []accountProject `json:"projects"`
	Project  accountProject   `json:"project"`
}

// authClient speaks the session-cookie API. It is deliberately dumb: base URL +
// token, JSON in/out, no retries — auth calls are interactive, not hot-path.
type authClient struct {
	base  string
	token string
	http  *http.Client
}

func newAuthClient(base, token string) *authClient {
	return &authClient{
		base:  strings.TrimRight(base, "/"),
		token: token,
		http:  &http.Client{Timeout: 30 * time.Second},
	}
}

// do sends a JSON request; when the response sets a fresh session cookie the
// new token is returned so callers can persist it.
func (a *authClient) do(method, path string, body any, out any) (newToken string, err error) {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return "", err
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, a.base+path, reader)
	if err != nil {
		return "", err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if a.token != "" {
		req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: a.token})
	}
	resp, err := a.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return "", errors.New("not logged in — run `agentray login --email <email>` first")
	}
	if resp.StatusCode >= 300 {
		msg := strings.TrimSpace(string(respBody))
		var httpErr struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(respBody, &httpErr) == nil && httpErr.Message != "" {
			msg = httpErr.Message
		}
		return "", fmt.Errorf("%s %s failed (%d): %s", method, path, resp.StatusCode, msg)
	}
	if out != nil && len(respBody) > 0 {
		if err := json.Unmarshal(respBody, out); err != nil {
			return "", fmt.Errorf("decode %s response: %w", path, err)
		}
	}
	for _, c := range resp.Cookies() {
		if c.Name == sessionCookieName && c.Value != "" {
			return c.Value, nil
		}
	}
	return "", nil
}

// runAccountCommand dispatches the auth subcommands. base is the resolved
// server URL (flag > env > config > default).
func runAccountCommand(base string, args []string) error {
	cfg := loadConfig()
	switch args[0] {
	case "signup":
		return cmdSignup(base, cfg, args[1:])
	case "login":
		return cmdLogin(base, cfg, args[1:])
	case "logout":
		return cmdLogout(base, cfg)
	case "whoami":
		return cmdWhoami(base, cfg)
	case "key":
		return cmdKey(base, cfg, args[1:])
	case "projects":
		return cmdProjects(base, cfg)
	}
	return fmt.Errorf("unknown command %q", args[0])
}

func cmdSignup(base string, cfg cliConfig, args []string) error {
	fs := flag.NewFlagSet("signup", flag.ContinueOnError)
	email := fs.String("email", "", "account email (required)")
	name := fs.String("name", "", "display name (defaults to the email local part)")
	password := fs.String("password", "", "password (or AGENTRAY_PASSWORD, or interactive prompt)")
	workspace := fs.String("workspace", "", "workspace name (server default when empty)")
	project := fs.String("project", "", "first project name (server default when empty)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *email == "" {
		return errors.New("signup requires --email")
	}
	if *name == "" {
		*name = strings.SplitN(*email, "@", 2)[0]
	}
	pw, err := resolvePassword(*password, true)
	if err != nil {
		return err
	}
	var payload accountPayload
	token, err := newAuthClient(base, "").do(http.MethodPost, "/api/auth/signup", map[string]string{
		"email": *email, "name": *name, "password": pw,
		"workspace_name": *workspace, "project_name": *project,
	}, &payload)
	if err != nil {
		return err
	}
	if err := persistSession(base, cfg, *email, token, payload); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "Account created for %s.\n", payload.User.Email)
	printSessionSummary(payload)
	return nil
}

func cmdLogin(base string, cfg cliConfig, args []string) error {
	fs := flag.NewFlagSet("login", flag.ContinueOnError)
	email := fs.String("email", cfg.Email, "account email")
	password := fs.String("password", "", "password (or AGENTRAY_PASSWORD, or interactive prompt)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *email == "" {
		return errors.New("login requires --email")
	}
	pw, err := resolvePassword(*password, false)
	if err != nil {
		return err
	}
	var payload accountPayload
	token, err := newAuthClient(base, "").do(http.MethodPost, "/api/auth/login", map[string]string{
		"email": *email, "password": pw,
	}, &payload)
	if err != nil {
		return err
	}
	if err := persistSession(base, cfg, *email, token, payload); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "Logged in as %s.\n", payload.User.Email)
	printSessionSummary(payload)
	return nil
}

func cmdLogout(base string, cfg cliConfig) error {
	if cfg.SessionToken != "" {
		client := newAuthClient(base, cfg.SessionToken)
		// The management credential goes first: the session is what authorizes
		// the revoke, so it must outlive it. A revoke that cannot be proved
		// stops the logout with local state intact, so the credential stays
		// tracked and the retry is one command away.
		if err := abandonManagementCredential(client, &cfg, ""); err != nil {
			return fmt.Errorf("logout stopped: %w — nothing was cleared; remove %s only after the credential is revoked", err, configPath())
		}
		// Best-effort server-side session revoke; local state is cleared regardless.
		if _, err := client.do(http.MethodPost, "/api/auth/logout", nil, nil); err != nil {
			fmt.Fprintf(os.Stderr, "warning: server logout failed: %v\n", err)
		}
	}
	if err := saveConfig(cliConfig{URL: cfg.URL}); err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, "Logged out; local credentials cleared.")
	return nil
}

func cmdWhoami(base string, cfg cliConfig) error {
	if cfg.SessionToken == "" {
		return errors.New("not logged in — run `agentray login --email <email>` first")
	}
	var payload accountPayload
	if _, err := newAuthClient(base, cfg.SessionToken).do(http.MethodGet, "/api/auth/me", nil, &payload); err != nil {
		return err
	}
	fmt.Printf("%s (%s)\n", payload.User.Email, payload.User.Name)
	fmt.Printf("server:  %s\n", base)
	if payload.Project.ID != "" {
		fmt.Printf("project: %s (%s)\n", payload.Project.Name, payload.Project.ID)
	}
	fmt.Printf("workspaces: %d, projects: %d\n", len(payload.Workspaces), len(payload.Projects))
	return nil
}

func cmdKey(base string, cfg cliConfig, args []string) error {
	fs := flag.NewFlagSet("key", flag.ContinueOnError)
	project := fs.String("project", "", "project name or id (defaults to the saved/first project)")
	rotate := fs.Bool("rotate", false, "rotate the key before printing (invalidates the old one)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if cfg.SessionToken == "" {
		return errors.New("not logged in — run `agentray login --email <email>` first")
	}
	client := newAuthClient(base, cfg.SessionToken)
	var payload accountPayload
	if _, err := client.do(http.MethodGet, "/api/auth/me", nil, &payload); err != nil {
		return err
	}
	selector := *project
	if selector == "" {
		selector = cfg.ProjectID
	}
	chosen, err := pickProject(payload, selector)
	if err != nil {
		return err
	}
	if *rotate {
		var rotated struct {
			Project accountProject `json:"project"`
		}
		if _, err := client.do(http.MethodPost, "/api/projects/"+chosen.ID+"/rotate-key", map[string]any{}, &rotated); err != nil {
			return err
		}
		chosen = rotated.Project
		fmt.Fprintln(os.Stderr, "Key rotated — update every SDK/MCP consumer of the old key.")
	}
	// Revoke the credential bound to the project this selection replaces before
	// the new one takes its place: dropping it locally would strand a live key
	// with nothing pointing at it. A switch that cannot prove the revoke stops
	// here, leaving the config on disk still tracking what it holds.
	if err := abandonManagementCredential(client, &cfg, chosen.ID); err != nil {
		return fmt.Errorf("cannot switch from project %s: %w", cfg.ProjectName, err)
	}
	cfg.ProjectID = chosen.ID
	cfg.ProjectName = chosen.Name
	cfg.APIKey = chosen.APIKey // capture key — for SDK use, not ops
	// Reuse the stored management credential when it is already bound to this
	// project — minting on every invocation would proliferate live keys.
	if cfg.ManagementKey == "" {
		cred, mintErr := mintManagementCredential(client, chosen.ID, chosen.Role)
		if mintErr != nil {
			// The capture key is still printable and still valid for SDKs; a
			// credential the role may not mint must not fail the command.
			fmt.Fprintf(os.Stderr, "warning: %s\n", credentialNotice(mintErr, chosen.Name))
		} else {
			cfg.ManagementKey = cred.secret
			cfg.ManagementKeyID = cred.id
			cfg.ManagementKeyProject = chosen.ID
		}
	}
	if err := saveConfig(cfg); err != nil {
		return err
	}
	// Bare CAPTURE key on stdout: `export AGENTRAY_API_KEY=$(agentray key)` is
	// the documented SDK flow — printing the management credential here would
	// embed a private scoped secret in SDK config. Ops use the stored
	// ManagementKey internally; they never read it from stdout.
	fmt.Println(chosen.APIKey)
	return nil
}

func cmdProjects(base string, cfg cliConfig) error {
	if cfg.SessionToken == "" {
		return errors.New("not logged in — run `agentray login --email <email>` first")
	}
	var payload accountPayload
	if _, err := newAuthClient(base, cfg.SessionToken).do(http.MethodGet, "/api/auth/me", nil, &payload); err != nil {
		return err
	}
	for _, p := range payload.Projects {
		marker := " "
		if p.ID == cfg.ProjectID {
			marker = "*"
		}
		fmt.Printf("%s %-24s %s\n", marker, p.Name, p.ID)
	}
	if len(payload.Projects) == 0 {
		fmt.Fprintln(os.Stderr, "no projects — create one in the web app or POST /api/projects")
	}
	return nil
}

// --- management credentials ---------------------------------------------------
//
// A project key is the CAPTURE credential: it feeds events and, on a project
// that has opted into the split, nothing else. Operations authenticate with a
// scoped, revocable management credential the CLI mints once per project and
// keeps beside the session. Its lifecycle is the three helpers below — mint,
// resolve, revoke — and the rule that binds them: a live credential is never
// dropped from local state without proof that it is gone server-side.

var (
	// errCredentialUnsupported marks a server predating scoped credentials (the
	// mint route answers 404); the project key still carries operations there.
	errCredentialUnsupported = errors.New("server has no scoped-credential surface")
	// errCredentialDenied marks the owner/admin rule refusing the mint — a
	// statement about the caller's role, not a fault.
	errCredentialDenied = errors.New("this workspace role cannot mint a management credential")
)

type mintedCredential struct {
	secret string
	id     string
}

// cliMintScopes is the scope set the CLI asks for: every access class the
// shared registry's command surface can require, so no operation the CLI
// dispatches is refused for a scope the CLI itself failed to request. Derived,
// never retyped — the hand-written literal this replaces went stale the moment
// plans:write joined the registry.
func cliMintScopes() ([]string, error) {
	set := map[string]bool{}
	for _, spec := range usecase.Registry().Specs() {
		access := string(spec.OpAccess())
		if access == "" {
			// Access "" denies every remote caller, so no credential can cover
			// that operation: a silently incomplete credential would leave a
			// command that always 403s, which is the defect this replaces.
			return nil, fmt.Errorf("operation %s declares no access class", spec.OpName())
		}
		set[access] = true
	}
	scopes := make([]string, 0, len(set))
	for scope := range set {
		scopes = append(scopes, scope)
	}
	sort.Strings(scopes)
	return scopes, nil
}

// mintManagementCredential creates the project-bound credential the CLI's
// operations use. The secret is returned once by the server and lives only in
// the local config; the id is kept so revocation never has to guess.
func mintManagementCredential(client *authClient, projectID, role string) (mintedCredential, error) {
	scopes, err := cliMintScopes()
	if err != nil {
		return mintedCredential{}, err
	}
	var resp struct {
		Credential struct {
			ID string `json:"id"`
		} `json:"credential"`
		Secret string `json:"secret"`
	}
	_, err = client.do(http.MethodPost, "/api/projects/"+projectID+"/credentials",
		map[string]any{"name": cliCredentialName, "scopes": scopes}, &resp)
	switch {
	case err == nil && resp.Secret != "":
		return mintedCredential{secret: resp.Secret, id: resp.Credential.ID}, nil
	case err == nil:
		return mintedCredential{}, errors.New("credential endpoint returned no secret")
	case strings.Contains(err.Error(), "(404)"):
		return mintedCredential{}, fmt.Errorf("%w: %v", errCredentialUnsupported, err)
	case mintRefused(err, role):
		return mintedCredential{}, fmt.Errorf("%w (role %q): %v", errCredentialDenied, role, err)
	default:
		return mintedCredential{}, err
	}
}

// mintRefused reports whether a failed mint is the owner/admin rule rather than
// a fault. The route maps every store error to 400, so the status alone says
// nothing; the caller's role — reported on the project payload — is the signal.
func mintRefused(err error, role string) bool {
	if role != "member" && role != "viewer" {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "(400)") || strings.Contains(msg, "(403)")
}

// credentialNotice explains what the caller loses when the CLI holds no
// management credential, so a role that cannot mint one learns the limit at
// login instead of from a 403 mid-task.
func credentialNotice(err error, project string) string {
	switch {
	case errors.Is(err, errCredentialDenied):
		return fmt.Sprintf("no management credential for project %s: this workspace role cannot mint one (owner/admin only), so operations will be refused — ask a workspace owner or admin to mint a credential", project)
	case errors.Is(err, errCredentialUnsupported):
		return fmt.Sprintf("no management credential for project %s: this server predates scoped credentials, so the project key carries operations instead", project)
	default:
		return fmt.Sprintf("no management credential for project %s: %v", project, err)
	}
}

// credentialRow is the non-secret view of one management credential.
type credentialRow struct {
	ID        string  `json:"id"`
	Name      string  `json:"name"`
	KeyHint   string  `json:"key_hint"`
	RevokedAt *string `json:"revoked_at"`
}

// listCredentials reads the project's credentials (member-readable). It is the
// only way to tell a refused revocation from an already-revoked credential,
// because the delete route answers 403 to both.
func listCredentials(client *authClient, projectID string) ([]credentialRow, error) {
	var resp struct {
		Credentials []credentialRow `json:"credentials"`
	}
	if _, err := client.do(http.MethodGet, "/api/projects/"+projectID+"/credentials", nil, &resp); err != nil {
		return nil, err
	}
	return resp.Credentials, nil
}

// resolveCredentialID finds the credential a stored secret belongs to for a
// config written before ids were stored. The only handle is the 4-character key
// hint, so the match is narrowed to live rows carrying the CLI's own name and
// accepted only when exactly one remains: revoking the wrong credential would
// orphan the one we hold. Empty id means nothing live matches (already gone).
func resolveCredentialID(client *authClient, projectID, secret string) (string, error) {
	if len(secret) < 4 {
		return "", fmt.Errorf("stored credential secret for project %s is too short to identify", projectID)
	}
	hint := secret[len(secret)-4:]
	rows, err := listCredentials(client, projectID)
	if err != nil {
		return "", err
	}
	var matches []string
	for _, row := range rows {
		if row.RevokedAt == nil && row.Name == cliCredentialName && row.KeyHint == hint {
			matches = append(matches, row.ID)
		}
	}
	switch len(matches) {
	case 0:
		return "", nil
	case 1:
		return matches[0], nil
	default:
		return "", fmt.Errorf("%d live credentials share the key hint %q on project %s — revoke the right one in the web app, then retry", len(matches), hint, projectID)
	}
}

// revokeManagedCredential revokes the stored credential and returns nil only
// when it is provably gone: the delete succeeded, or the credential list shows
// the row absent or already revoked. Any other outcome is an error, so callers
// keep the credential tracked instead of discarding a secret that is still live.
func revokeManagedCredential(client *authClient, projectID, credentialID, secret string) error {
	id := credentialID
	if id == "" {
		resolved, err := resolveCredentialID(client, projectID, secret)
		if err != nil {
			return err
		}
		if resolved == "" {
			return nil
		}
		id = resolved
	}
	_, err := client.do(http.MethodDelete, "/api/projects/"+projectID+"/credentials/"+id, nil, nil)
	if err == nil {
		return nil
	}
	rows, listErr := listCredentials(client, projectID)
	if listErr != nil {
		return fmt.Errorf("could not revoke credential %s on project %s (%v), and the credential list that would confirm it is unreadable (%v) — revoke it in the web app, then retry", id, projectID, err, listErr)
	}
	for _, row := range rows {
		if row.ID != id {
			continue
		}
		if row.RevokedAt != nil {
			return nil
		}
		return fmt.Errorf("credential %s (%s) is still live on project %s (%v) — revoke it in the web app, then retry", id, row.KeyHint, projectID, err)
	}
	return nil // absent from the project's credentials: it is gone
}

// abandonManagementCredential revokes the credential bound to the project the
// CLI is leaving and clears its local trace. It is the only path that drops a
// stored credential, and it clears nothing until the server has confirmed the
// secret is dead — so a switch that cannot prove that fails with the config
// still tracking what it holds.
func abandonManagementCredential(client *authClient, cfg *cliConfig, switchingTo string) error {
	if cfg.ManagementKey == "" {
		return nil
	}
	// The project the credential has been used against: its recorded binding,
	// or the current selection for a config the CLI never writes.
	project := cfg.ManagementKeyProject
	if project == "" {
		project = cfg.ProjectID
	}
	if project == "" {
		return fmt.Errorf("the stored management credential has no project binding — revoke it in the web app, then remove management_key from %s", configPath())
	}
	if project == switchingTo {
		return nil
	}
	if err := revokeManagedCredential(client, project, cfg.ManagementKeyID, cfg.ManagementKey); err != nil {
		return err
	}
	cfg.ManagementKey, cfg.ManagementKeyProject, cfg.ManagementKeyID = "", "", ""
	return nil
}

// --- helpers -----------------------------------------------------------------

func persistSession(base string, cfg cliConfig, email, token string, payload accountPayload) error {
	if token == "" {
		return errors.New("server did not return a session cookie")
	}
	cfg.URL = base
	cfg.SessionToken = token
	cfg.Email = email
	if payload.Project.ID == "" {
		return saveConfig(cfg)
	}
	client := newAuthClient(base, token)
	// The credential bound to the project this selection replaces is revoked
	// before anything changes, and the switch waits on that proof.
	if err := abandonManagementCredential(client, &cfg, payload.Project.ID); err != nil {
		// A credential that cannot be revoked is a reason to keep the previous
		// selection — never a reason to discard the session this login earned.
		if saveErr := saveConfig(cfg); saveErr != nil {
			return saveErr
		}
		fmt.Fprintf(os.Stderr, "Session saved; project %s kept with its management credential.\n", cfg.ProjectName)
		return fmt.Errorf("could not switch to project %s: %w", payload.Project.Name, err)
	}
	cfg.ProjectID = payload.Project.ID
	cfg.ProjectName = payload.Project.Name
	cfg.APIKey = payload.Project.APIKey // capture key — for SDK use, not ops
	// Reuse the stored management credential when it is already bound to this
	// project — a re-login must not mint another live key. Born-split projects
	// make the project key capture-only, so the credential is what carries
	// operations; it lives only in this local config.
	if cfg.ManagementKey == "" {
		cred, err := mintManagementCredential(client, payload.Project.ID, payload.Project.Role)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: %s\n", credentialNotice(err, payload.Project.Name))
		} else {
			cfg.ManagementKey = cred.secret
			cfg.ManagementKeyID = cred.id
			cfg.ManagementKeyProject = payload.Project.ID
		}
	}
	if err := saveConfig(cfg); err != nil {
		return err
	}
	if cfg.ManagementKey != "" {
		fmt.Fprintf(os.Stderr, "Ops credential ready for project %s.\n", cfg.ProjectName)
	}
	return nil
}

// printSessionSummary reports where the session landed. The management
// credential's own outcome is reported where it is decided — it depends on the
// workspace role — so this function must not claim one exists.
func printSessionSummary(payload accountPayload) {
	if payload.Project.ID != "" {
		fmt.Fprintf(os.Stderr, "Default project: %s\n", payload.Project.Name)
		fmt.Fprintf(os.Stderr, "Credentials saved to %s — `agentray key` prints the capture key for SDKs.\n", configPath())
	}
}

func pickProject(payload accountPayload, selector string) (accountProject, error) {
	if selector == "" {
		if payload.Project.ID != "" {
			return payload.Project, nil
		}
		if len(payload.Projects) > 0 {
			return payload.Projects[0], nil
		}
		return accountProject{}, errors.New("account has no projects")
	}
	for _, p := range payload.Projects {
		if p.ID == selector || strings.EqualFold(p.Name, selector) {
			return p, nil
		}
	}
	return accountProject{}, fmt.Errorf("no project matching %q (try `agentray projects`)", selector)
}

// resolvePassword returns the password from the flag, AGENTRAY_PASSWORD, or an
// interactive no-echo prompt (confirmed twice on signup). Non-TTY stdin reads a
// single line so scripted callers can pipe it.
func resolvePassword(flagValue string, confirm bool) (string, error) {
	if flagValue != "" {
		return flagValue, nil
	}
	if env := os.Getenv("AGENTRAY_PASSWORD"); env != "" {
		return env, nil
	}
	pw, err := promptPassword("Password: ")
	if err != nil {
		return "", err
	}
	if pw == "" {
		return "", errors.New("password is required (flag --password, env AGENTRAY_PASSWORD, or prompt)")
	}
	if confirm && term.IsTerminal(int(os.Stdin.Fd())) {
		again, err := promptPassword("Confirm password: ")
		if err != nil {
			return "", err
		}
		if again != pw {
			return "", errors.New("passwords do not match")
		}
	}
	return pw, nil
}

func promptPassword(label string) (string, error) {
	fd := int(os.Stdin.Fd())
	if term.IsTerminal(fd) {
		fmt.Fprint(os.Stderr, label)
		b, err := term.ReadPassword(fd)
		fmt.Fprintln(os.Stderr)
		if err != nil {
			return "", err
		}
		return string(b), nil
	}
	var line string
	if _, err := fmt.Fscanln(os.Stdin, &line); err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	return strings.TrimSpace(line), nil
}
