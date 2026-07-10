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
	"strings"
	"time"

	"golang.org/x/term"
)

const sessionCookieName = "agentray_session"

// cliConfig is the persisted state in ~/.agentray/config.json.
type cliConfig struct {
	URL          string `json:"url,omitempty"`
	SessionToken string `json:"session_token,omitempty"`
	Email        string `json:"email,omitempty"`
	ProjectID    string `json:"project_id,omitempty"`
	ProjectName  string `json:"project_name,omitempty"`
	APIKey       string `json:"api_key,omitempty"`
}

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
		// Best-effort server-side revoke; local state is cleared regardless.
		if _, err := newAuthClient(base, cfg.SessionToken).do(http.MethodPost, "/api/auth/logout", nil, nil); err != nil {
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
	cfg.ProjectID = chosen.ID
	cfg.ProjectName = chosen.Name
	cfg.APIKey = chosen.APIKey
	if err := saveConfig(cfg); err != nil {
		return err
	}
	// Bare key on stdout: `export AGENTRAY_API_KEY=$(agentray key)`.
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

// --- helpers -----------------------------------------------------------------

func persistSession(base string, cfg cliConfig, email, token string, payload accountPayload) error {
	if token == "" {
		return errors.New("server did not return a session cookie")
	}
	cfg.URL = base
	cfg.SessionToken = token
	cfg.Email = email
	if payload.Project.ID != "" {
		cfg.ProjectID = payload.Project.ID
		cfg.ProjectName = payload.Project.Name
		cfg.APIKey = payload.Project.APIKey
	}
	return saveConfig(cfg)
}

func printSessionSummary(payload accountPayload) {
	if payload.Project.ID != "" {
		fmt.Fprintf(os.Stderr, "Default project: %s\n", payload.Project.Name)
		fmt.Fprintf(os.Stderr, "API key saved to %s — print it with `agentray key`.\n", configPath())
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
