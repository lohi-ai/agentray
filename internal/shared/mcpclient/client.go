// Package mcpclient is the outbound half of AgentRay's Model Context Protocol
// support: it lets an agent call tools hosted on a *remote* MCP server the
// project operates, the mirror image of internal/shared/opcore's MountMCP, which
// exposes our own operations to external MCP clients.
//
// This is the tenant-facing extension path. A hosted, multi-tenant backend
// cannot load customer code — a plugin executing in this process would run with
// the server's identity and reach every project's data — so the extension
// boundary is a network boundary instead: the customer runs their own MCP server
// under their own credentials, and we speak JSON-RPC to it. Adding a capability
// stays configuration, never a backend change (docs/ARCHITECT-EXTENSIONS.md).
//
// Transport is the MCP "Streamable HTTP" binding only: one POST endpoint
// speaking JSON-RPC 2.0, answering either application/json or a text/event-stream
// carrying the response. stdio is deliberately unsupported — it would mean
// spawning a tenant-specified process on the API host, which is the exact
// arbitrary-code-execution boundary this design exists to avoid.
//
// Every connection rides sandbox.NewGuardedClient, so the SSRF backstop that
// protects http_request also protects an operator-supplied MCP URL: a server
// resolving to loopback, a private range, or the cloud-metadata address is
// refused at dial time, and redirects are never followed.
package mcpclient

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/lohi-ai/agentray/sandbox"
)

// protocolVersion is the MCP revision this client negotiates. It matches the
// version our own server adapter speaks (opcore/mcp.go), so the two halves of
// AgentRay's MCP support cannot drift.
const protocolVersion = "2025-06-18"

// defaultTimeout bounds one JSON-RPC round trip. A remote tool call is a
// synchronous step inside a turn, so this is also the ceiling on how long one
// tool can stall a run.
const defaultTimeout = 30 * time.Second

// maxResponseBytes caps how much a remote server can make us buffer for one
// response. A tool result is truncated to a far smaller budget before it reaches
// the model; this only stops a hostile or broken server from exhausting memory.
const maxResponseBytes = 8 << 20 // 8 MiB

// ServerConfig describes one remote MCP server as an operator configured it.
// Headers arrive already credential-resolved: {{cred:NAME}} placeholders are
// substituted at the trust boundary before a Client is built, so the model never
// sees a token and none is stored here in placeholder form.
type ServerConfig struct {
	// Name is the operator's label for this server. It namespaces the tool names
	// this server contributes, so two servers exposing "search" stay distinct.
	Name string
	// URL is the Streamable HTTP endpoint (the one that answers JSON-RPC POSTs).
	URL string
	// Headers are sent on every request (typically Authorization).
	Headers map[string]string
	// Timeout bounds one round trip; zero uses defaultTimeout.
	Timeout time.Duration
	// AllowTools, when non-empty, restricts which of the server's advertised
	// tools this agent may see. It is a narrowing filter over what the server
	// offers, never a way to invent one.
	AllowTools []string
	// AllowHTTP permits a plain-http:// endpoint. Off by default: an MCP server
	// URL usually carries a bearer token in a header, and the SSRF guard already
	// keeps cleartext off the private network, so cleartext to a *public* host is
	// the only case this covers — and it leaks the token.
	AllowHTTP bool
}

// Option customizes a Client at construction.
type Option func(*Client)

// WithHTTPClient replaces the guarded transport. The host may supply its own
// (proxying, custom TLS); tests use it to reach a local server the SSRF guard
// would otherwise refuse. Supplying one opts out of the private-network
// backstop, so production callers should not.
func WithHTTPClient(c *http.Client) Option {
	return func(cl *Client) {
		if c != nil {
			cl.http = c
		}
	}
}

// RemoteTool is one tool as the server advertises it in tools/list.
type RemoteTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

// Client is a connection to one remote MCP server. It is safe for concurrent
// use: the handshake happens at most once, and JSON-RPC ids are allocated under
// the same lock.
type Client struct {
	cfg  ServerConfig
	http *http.Client

	mu        sync.Mutex
	nextID    int
	sessionID string // echoed back from initialize, when the server uses sessions
	ready     bool
}

// New validates a server config and builds a client for it. It performs no I/O:
// the handshake is deferred to the first call, so constructing a client for an
// unreachable server is not itself an error.
func New(cfg ServerConfig, opts ...Option) (*Client, error) {
	if strings.TrimSpace(cfg.Name) == "" {
		return nil, fmt.Errorf("mcp server requires a name")
	}
	u, err := url.Parse(strings.TrimSpace(cfg.URL))
	if err != nil {
		return nil, fmt.Errorf("mcp server %q: invalid url: %w", cfg.Name, err)
	}
	switch {
	case u.Scheme == "https":
	case u.Scheme == "http" && cfg.AllowHTTP:
	case u.Scheme == "http":
		return nil, fmt.Errorf("mcp server %q: refusing plain http (set allow_http to override)", cfg.Name)
	default:
		return nil, fmt.Errorf("mcp server %q: url must be http(s), got %q", cfg.Name, u.Scheme)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("mcp server %q: url has no host", cfg.Name)
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	c := &Client{cfg: cfg, http: sandbox.NewGuardedClient(timeout)}
	for _, opt := range opts {
		opt(c)
	}
	return c, nil
}

// Name returns the operator's label for this server.
func (c *Client) Name() string { return c.cfg.Name }

// jsonRPCRequest is one outbound message. A notification omits ID.
type jsonRPCRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      *int   `json:"id,omitempty"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type jsonRPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *jsonRPCError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("jsonrpc error %d", e.Code)
	}
	return fmt.Sprintf("%s (code %d)", e.Message, e.Code)
}

type jsonRPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *int            `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *jsonRPCError   `json:"error,omitempty"`
}

// ListTools returns the server's advertised tools, narrowed by AllowTools when
// set. It walks tools/list pagination to completion.
func (c *Client) ListTools(ctx context.Context) ([]RemoteTool, error) {
	if err := c.ensureReady(ctx); err != nil {
		return nil, err
	}
	allowed := map[string]bool{}
	for _, n := range c.cfg.AllowTools {
		if t := strings.TrimSpace(n); t != "" {
			allowed[t] = true
		}
	}

	var out []RemoteTool
	cursor := ""
	// A server that keeps handing back cursors must not loop us forever.
	for page := 0; page < 32; page++ {
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		raw, err := c.call(ctx, "tools/list", params)
		if err != nil {
			return nil, err
		}
		var payload struct {
			Tools      []RemoteTool `json:"tools"`
			NextCursor string       `json:"nextCursor"`
		}
		if err := json.Unmarshal(raw, &payload); err != nil {
			return nil, fmt.Errorf("mcp server %q: malformed tools/list: %w", c.cfg.Name, err)
		}
		for _, t := range payload.Tools {
			if t.Name == "" {
				continue
			}
			if len(allowed) > 0 && !allowed[t.Name] {
				continue
			}
			out = append(out, t)
		}
		if payload.NextCursor == "" || payload.NextCursor == cursor {
			break
		}
		cursor = payload.NextCursor
	}
	return out, nil
}

// CallResult is one tools/call outcome flattened for the model: the text
// rendering of every content block, and whether the server flagged it an error.
type CallResult struct {
	Text    string
	IsError bool
}

// CallTool invokes a remote tool. args is the raw JSON object the model
// produced. A server-reported tool error is returned as a CallResult with
// IsError set — not a Go error — because the model should see and adapt to it;
// a Go error means the call never produced a result (transport, protocol, or a
// JSON-RPC-level failure).
func (c *Client) CallTool(ctx context.Context, name string, args json.RawMessage) (CallResult, error) {
	if err := c.ensureReady(ctx); err != nil {
		return CallResult{}, err
	}
	if len(bytes.TrimSpace(args)) == 0 {
		args = json.RawMessage(`{}`)
	}
	raw, err := c.call(ctx, "tools/call", map[string]any{
		"name":      name,
		"arguments": json.RawMessage(args),
	})
	if err != nil {
		return CallResult{}, err
	}
	var payload struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		StructuredContent json.RawMessage `json:"structuredContent,omitempty"`
		IsError           bool            `json:"isError"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return CallResult{}, fmt.Errorf("mcp server %q: malformed tools/call result: %w", c.cfg.Name, err)
	}
	var b strings.Builder
	for _, part := range payload.Content {
		if part.Text == "" {
			// A non-text block (image, audio, resource link) has no rendering the
			// model can read here; name it so the omission is visible rather than
			// silently dropped.
			if part.Type != "" && part.Type != "text" {
				fmt.Fprintf(&b, "[%s content omitted]\n", part.Type)
			}
			continue
		}
		b.WriteString(part.Text)
		if !strings.HasSuffix(part.Text, "\n") {
			b.WriteString("\n")
		}
	}
	text := strings.TrimRight(b.String(), "\n")
	// Some servers answer only with structuredContent. Fall back to it so the
	// model gets the data rather than an empty result.
	if text == "" && len(payload.StructuredContent) > 0 {
		text = string(payload.StructuredContent)
	}
	if text == "" {
		text = "(no content)"
	}
	return CallResult{Text: text, IsError: payload.IsError}, nil
}

// ensureReady performs the initialize handshake once. Concurrent callers block
// on the same attempt; a failed handshake is not cached, so the next call
// retries it.
func (c *Client) ensureReady(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ready {
		return nil
	}
	raw, err := c.callLocked(ctx, "initialize", map[string]any{
		"protocolVersion": protocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "agentray", "version": "1"},
	})
	if err != nil {
		return err
	}
	// The handshake's only load-bearing output is the session header, already
	// captured in post(). The result body is read but not enforced: servers vary
	// in how strictly they advertise capabilities, and refusing one whose
	// initialize payload we cannot parse would reject working servers. If it
	// genuinely has no tools, ListTools returns an empty set and says so.
	_ = raw

	// The spec requires the initialized notification before normal operation.
	// It is fire-and-forget: a server that rejects it still usually works, and
	// failing the whole connection here would be less useful than trying.
	_ = c.notifyLocked(ctx, "notifications/initialized", map[string]any{})
	c.ready = true
	return nil
}

// call issues one request/response round trip, taking the lock for id + session
// bookkeeping.
func (c *Client) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.callLocked(ctx, method, params)
}

// callLocked is call with c.mu already held.
func (c *Client) callLocked(ctx context.Context, method string, params any) (json.RawMessage, error) {
	c.nextID++
	id := c.nextID
	resp, err := c.post(ctx, jsonRPCRequest{JSONRPC: "2.0", ID: &id, Method: method, Params: params})
	if err != nil {
		return nil, err
	}
	if resp == nil {
		return nil, fmt.Errorf("mcp server %q: no response to %s", c.cfg.Name, method)
	}
	if resp.Error != nil {
		return nil, fmt.Errorf("mcp server %q: %s: %w", c.cfg.Name, method, resp.Error)
	}
	return resp.Result, nil
}

// notifyLocked sends a notification (no id, no response expected).
func (c *Client) notifyLocked(ctx context.Context, method string, params any) error {
	_, err := c.post(ctx, jsonRPCRequest{JSONRPC: "2.0", Method: method, Params: params})
	return err
}

// post sends one JSON-RPC message and decodes the response, accepting either a
// plain JSON body or a text/event-stream carrying it. A notification (nil id)
// returns (nil, nil) when the server answers with no body.
func (c *Client) post(ctx context.Context, msg jsonRPCRequest) (*jsonRPCResponse, error) {
	body, err := json.Marshal(msg)
	if err != nil {
		return nil, fmt.Errorf("mcp server %q: encode %s: %w", c.cfg.Name, msg.Method, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.URL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("mcp server %q: %w", c.cfg.Name, err)
	}
	// Operator headers first, so they cannot clobber the protocol headers below.
	for k, v := range c.cfg.Headers {
		req.Header.Set(k, v)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("MCP-Protocol-Version", protocolVersion)
	if c.sessionID != "" {
		req.Header.Set("Mcp-Session-Id", c.sessionID)
	}

	res, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("mcp server %q: %s: %w", c.cfg.Name, msg.Method, err)
	}
	defer res.Body.Close()
	if sid := res.Header.Get("Mcp-Session-Id"); sid != "" {
		c.sessionID = sid
	}

	raw, err := io.ReadAll(io.LimitReader(res.Body, maxResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("mcp server %q: %s: reading response: %w", c.cfg.Name, msg.Method, err)
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return nil, fmt.Errorf("mcp server %q: %s: http %d: %s",
			c.cfg.Name, msg.Method, res.StatusCode, snippet(raw))
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		if msg.ID == nil {
			return nil, nil // an accepted notification
		}
		return nil, fmt.Errorf("mcp server %q: %s: empty response", c.cfg.Name, msg.Method)
	}
	if strings.Contains(res.Header.Get("Content-Type"), "text/event-stream") {
		return c.fromEventStream(raw, msg)
	}
	var decoded jsonRPCResponse
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, fmt.Errorf("mcp server %q: %s: malformed response: %w", c.cfg.Name, msg.Method, err)
	}
	return &decoded, nil
}

// fromEventStream pulls the JSON-RPC response matching msg's id out of an SSE
// body. Servers interleave progress notifications on the same stream, so this
// skips anything that is not our answer.
func (c *Client) fromEventStream(raw []byte, msg jsonRPCRequest) (*jsonRPCResponse, error) {
	var data strings.Builder
	flush := func() *jsonRPCResponse {
		payload := strings.TrimSpace(data.String())
		data.Reset()
		if payload == "" {
			return nil
		}
		var decoded jsonRPCResponse
		if err := json.Unmarshal([]byte(payload), &decoded); err != nil {
			return nil // not a JSON-RPC frame (a comment/keepalive); keep reading
		}
		if decoded.ID == nil || msg.ID == nil || *decoded.ID != *msg.ID {
			return nil // a notification or another call's answer
		}
		return &decoded
	}

	scanner := bufio.NewScanner(bytes.NewReader(raw))
	scanner.Buffer(make([]byte, 0, 64*1024), maxResponseBytes)
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case line == "":
			if got := flush(); got != nil {
				return got, nil
			}
		case strings.HasPrefix(line, "data:"):
			if data.Len() > 0 {
				data.WriteString("\n")
			}
			data.WriteString(strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		default:
			// event:, id:, retry:, and comments carry no payload we need.
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("mcp server %q: %s: reading event stream: %w", c.cfg.Name, msg.Method, err)
	}
	if got := flush(); got != nil {
		return got, nil
	}
	if msg.ID == nil {
		return nil, nil
	}
	return nil, fmt.Errorf("mcp server %q: %s: event stream carried no response", c.cfg.Name, msg.Method)
}

// snippet trims a server error body to something safe to put in an error string.
func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}
