// Package httptool is the worked consumer of the credential vault (governance
// F7): an outbound HTTP tool. It is the legitimate-egress counterpart to the
// sandbox — where the sandbox runs untrusted code with --network none, this
// makes *controlled* outbound calls to an operator-approved host allowlist, and
// it is the place a {{cred:NAME}} secret actually gets used.
//
// The tool itself is deliberately dumb about credentials: the agentcore loop
// resolves {{cred:NAME}} placeholders in the argument JSON at the trust
// boundary before Run is ever called, so an Authorization header the model
// wrote as "Bearer {{cred:API_KEY}}" arrives here already resolved to the real
// value — the model never saw the literal, and this tool never needs the vault.
//
// What this tool *is* careful about is SSRF. An agent that can make arbitrary
// outbound requests can reach cloud metadata (169.254.169.254), internal
// services, and localhost. Defenses, default-deny:
//   - scheme must be https (http is opt-in)
//   - the URL host must be in the configured allowlist
//   - a guarded dialer re-checks the resolved IP at connect time and refuses
//     loopback / private / link-local / unspecified addresses, which also closes
//     the DNS-rebinding TOCTOU gap (allowlisted name re-pointed at a blocked IP)
//   - redirects are not followed (a 3xx is surfaced to the model as-is)
package httptool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/lohi-ai/agentray/agentcore"
)

// ToolHTTPRequest is the stable tool name. A consumer's Policy must permit it
// before the model is shown the tool.
const ToolHTTPRequest = "http_request"

const (
	defaultTimeout      = 15 * time.Second
	defaultMaxBodyBytes = 256 * 1024
)

// HTTPTool makes guarded outbound HTTP requests. Safe for concurrent use: the
// allowlist is read-only after construction and http.Client is concurrency-safe.
type HTTPTool struct {
	client         *http.Client
	allowHosts     map[string]struct{}
	allowPlainHTTP bool
	maxBodyBytes   int64
	// ipBlocked decides whether a resolved IP is off-limits. Defaults to
	// blockedIP (refuses loopback/private/link-local); a test seam can relax it
	// to exercise the happy path against an httptest server on 127.0.0.1. Read at
	// dial time, so an option may override it after the client is built.
	ipBlocked func(net.IP) (bool, string)
}

// Option configures an HTTPTool.
type Option func(*HTTPTool)

// WithAllowHosts sets the exact-match host allowlist (case-insensitive, port
// stripped). A request to any other host is refused.
func WithAllowHosts(hosts []string) Option {
	return func(t *HTTPTool) {
		for _, h := range hosts {
			h = strings.ToLower(strings.TrimSpace(h))
			if h != "" {
				t.allowHosts[h] = struct{}{}
			}
		}
	}
}

// WithAllowPlainHTTP permits http:// URLs (default: https only).
func WithAllowPlainHTTP(allow bool) Option {
	return func(t *HTTPTool) { t.allowPlainHTTP = allow }
}

// WithTimeout overrides the per-request timeout.
func WithTimeout(d time.Duration) Option {
	return func(t *HTTPTool) {
		if d > 0 {
			t.client.Timeout = d
		}
	}
}

// New builds an HTTPTool. The guarded dialer is installed here so every request
// — including each redirect hop, were they followed — is IP-checked at connect.
func New(opts ...Option) *HTTPTool {
	t := &HTTPTool{
		allowHosts:   make(map[string]struct{}),
		maxBodyBytes: defaultMaxBodyBytes,
		ipBlocked:    blockedIP,
	}
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	t.client = &http.Client{
		Timeout: defaultTimeout,
		// Do not follow redirects: a 3xx to an allowlisted host could bounce to a
		// blocked one. Surface the redirect response to the model instead.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Transport: &http.Transport{
			DialContext:           t.guardedDial(dialer),
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: defaultTimeout,
			MaxIdleConns:          10,
		},
	}
	for _, opt := range opts {
		opt(t)
	}
	return t
}

// AllowHosts returns the configured allowlist (for startup logging / tests).
func (t *HTTPTool) AllowHosts() []string {
	out := make([]string, 0, len(t.allowHosts))
	for h := range t.allowHosts {
		out = append(out, h)
	}
	sort.Strings(out)
	return out
}

func (t *HTTPTool) Name() string { return ToolHTTPRequest }

func (t *HTTPTool) Schema() agentcore.ToolSchema {
	return agentcore.ToolSchema{
		Name: ToolHTTPRequest,
		Description: "Make an outbound HTTP request to an approved host. Use a " +
			"{{cred:NAME}} placeholder for any secret (e.g. an Authorization " +
			"header value); it is resolved securely and never exposed. Only " +
			"allowlisted hosts over HTTPS are reachable. Returns the status, " +
			"response headers, and body.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"method": map[string]any{
					"type":        "string",
					"description": "HTTP method (GET, POST, PUT, PATCH, DELETE). Defaults to GET.",
				},
				"url": map[string]any{
					"type":        "string",
					"description": "Absolute https:// URL whose host is on the allowlist.",
				},
				"headers": map[string]any{
					"type":        "object",
					"description": "Optional request headers. Secret values may use {{cred:NAME}}.",
				},
				"body": map[string]any{
					"type":        "string",
					"description": "Optional request body (sent verbatim).",
				},
			},
			"required": []string{"url"},
		},
	}
}

type httpRequestArgs struct {
	Method  string            `json:"method"`
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers"`
	Body    string            `json:"body"`
}

// Run executes the guarded request. args has already had {{cred:NAME}} resolved
// by the loop, so any secret in Headers is the real value here.
func (t *HTTPTool) Run(ctx context.Context, args string) (string, error) {
	var in httpRequestArgs
	if err := json.Unmarshal([]byte(args), &in); err != nil {
		return "", fmt.Errorf("http_request: invalid arguments: %w", err)
	}

	method := strings.ToUpper(strings.TrimSpace(in.Method))
	if method == "" {
		method = http.MethodGet
	}
	if !allowedMethod(method) {
		return "", fmt.Errorf("http_request: method %q not allowed", method)
	}
	if err := t.validateURL(in.URL); err != nil {
		return "", err
	}

	var bodyReader io.Reader
	if in.Body != "" {
		bodyReader = strings.NewReader(in.Body)
	}
	req, err := http.NewRequestWithContext(ctx, method, in.URL, bodyReader)
	if err != nil {
		return "", fmt.Errorf("http_request: %w", err)
	}
	for k, v := range in.Headers {
		req.Header.Set(k, v)
	}

	resp, err := t.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("http_request: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, t.maxBodyBytes))
	return formatResponse(resp, body), nil
}

// validateURL enforces scheme + host allowlist. The IP-level guard is the
// dialer's job (it sees the resolved address at connect time).
func (t *HTTPTool) validateURL(raw string) error {
	u, err := parseAbsoluteURL(raw)
	if err != nil {
		return fmt.Errorf("http_request: %w", err)
	}
	switch u.Scheme {
	case "https":
	case "http":
		if !t.allowPlainHTTP {
			return errors.New("http_request: only https:// URLs are allowed")
		}
	default:
		return fmt.Errorf("http_request: unsupported scheme %q", u.Scheme)
	}
	host := strings.ToLower(u.Hostname())
	if _, ok := t.allowHosts[host]; !ok {
		return fmt.Errorf("http_request: host %q is not on the allowlist", host)
	}
	return nil
}

func allowedMethod(m string) bool {
	switch m {
	case http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodHead:
		return true
	default:
		return false
	}
}

func formatResponse(resp *http.Response, body []byte) string {
	var b strings.Builder
	fmt.Fprintf(&b, "status: %s\n", resp.Status)
	if loc := resp.Header.Get("Location"); loc != "" {
		fmt.Fprintf(&b, "location: %s\n", loc)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		fmt.Fprintf(&b, "content-type: %s\n", ct)
	}
	if len(body) > 0 {
		fmt.Fprintf(&b, "body:\n%s", string(body))
	}
	return strings.TrimRight(b.String(), "\n")
}
