package mcpclient

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeServer is a minimal Streamable-HTTP MCP server: it speaks the subset of
// JSON-RPC this client uses and records what it was asked, so the tests assert
// on the wire rather than on the client's internals.
type fakeServer struct {
	mu sync.Mutex

	tools     []RemoteTool
	pages     [][]RemoteTool // when set, tools/list is served as cursor pages
	callText  string
	callError bool
	// rpcError, when set, is returned as a JSON-RPC error for tools/call.
	rpcError string
	// sse serves every response as text/event-stream instead of JSON.
	sse bool
	// sessionID, when set, is handed out on initialize and required afterwards.
	sessionID string

	initialized  bool
	notified     bool
	methods      []string
	lastArgs     json.RawMessage
	seenHeaders  http.Header
	seenSessions []string
}

func (s *fakeServer) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      *int            `json:"id"`
			Method  string          `json:"method"`
			Params  json.RawMessage `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}

		s.mu.Lock()
		s.methods = append(s.methods, req.Method)
		s.seenHeaders = r.Header.Clone()
		if sid := r.Header.Get("Mcp-Session-Id"); sid != "" {
			s.seenSessions = append(s.seenSessions, sid)
		}
		s.mu.Unlock()

		if req.Method == "notifications/initialized" {
			s.mu.Lock()
			s.notified = true
			s.mu.Unlock()
			w.WriteHeader(http.StatusAccepted)
			return
		}

		var result any
		var rpcErr map[string]any
		switch req.Method {
		case "initialize":
			s.mu.Lock()
			s.initialized = true
			s.mu.Unlock()
			if s.sessionID != "" {
				w.Header().Set("Mcp-Session-Id", s.sessionID)
			}
			result = map[string]any{
				"protocolVersion": protocolVersion,
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]any{"name": "fake", "version": "1"},
			}
		case "tools/list":
			if len(s.pages) > 0 {
				var params struct {
					Cursor string `json:"cursor"`
				}
				_ = json.Unmarshal(req.Params, &params)
				idx := 0
				if params.Cursor != "" {
					fmt.Sscanf(params.Cursor, "page-%d", &idx)
				}
				page := map[string]any{"tools": s.pages[idx]}
				if idx+1 < len(s.pages) {
					page["nextCursor"] = fmt.Sprintf("page-%d", idx+1)
				}
				result = page
			} else {
				result = map[string]any{"tools": s.tools}
			}
		case "tools/call":
			var params struct {
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
			}
			_ = json.Unmarshal(req.Params, &params)
			s.mu.Lock()
			s.lastArgs = params.Arguments
			s.mu.Unlock()
			if s.rpcError != "" {
				rpcErr = map[string]any{"code": -32602, "message": s.rpcError}
				break
			}
			result = map[string]any{
				"content": []map[string]any{{"type": "text", "text": s.callText}},
				"isError": s.callError,
			}
		default:
			rpcErr = map[string]any{"code": -32601, "message": "method not found"}
		}

		envelope := map[string]any{"jsonrpc": "2.0", "id": req.ID}
		if rpcErr != nil {
			envelope["error"] = rpcErr
		} else {
			envelope["result"] = result
		}
		body, _ := json.Marshal(envelope)

		if s.sse {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			// A progress notification first: the client must skip frames that are
			// not the answer to its own request.
			fmt.Fprintf(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\",\"params\":{}}\n\n")
			fmt.Fprintf(w, "event: message\ndata: %s\n\n", body)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write(body)
	}
}

// newTestClient starts the fake server and returns a client pointed at it. The
// guarded transport is replaced because the SSRF backstop refuses loopback — the
// very protection the production path depends on.
func newTestClient(t *testing.T, s *fakeServer, cfg ServerConfig) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(s.handler())
	t.Cleanup(srv.Close)
	if cfg.Name == "" {
		cfg.Name = "fake"
	}
	cfg.URL = srv.URL
	cfg.AllowHTTP = true
	c, err := New(cfg, WithHTTPClient(srv.Client()))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c, srv
}

func TestListToolsHandshakesOnce(t *testing.T) {
	s := &fakeServer{tools: []RemoteTool{
		{Name: "lookup", Description: "look something up", InputSchema: map[string]any{"type": "object"}},
		{Name: "write", Description: "write something"},
	}}
	c, _ := newTestClient(t, s, ServerConfig{})

	for i := 0; i < 2; i++ {
		got, err := c.ListTools(context.Background())
		if err != nil {
			t.Fatalf("ListTools: %v", err)
		}
		if len(got) != 2 || got[0].Name != "lookup" {
			t.Fatalf("unexpected tools: %+v", got)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.initialized || !s.notified {
		t.Fatalf("handshake incomplete: initialized=%v notified=%v", s.initialized, s.notified)
	}
	inits := 0
	for _, m := range s.methods {
		if m == "initialize" {
			inits++
		}
	}
	if inits != 1 {
		t.Fatalf("handshake should happen once, saw %d initialize calls (%v)", inits, s.methods)
	}
}

func TestListToolsAppliesAllowList(t *testing.T) {
	s := &fakeServer{tools: []RemoteTool{{Name: "lookup"}, {Name: "delete_everything"}}}
	c, _ := newTestClient(t, s, ServerConfig{AllowTools: []string{"lookup"}})

	got, err := c.ListTools(context.Background())
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(got) != 1 || got[0].Name != "lookup" {
		t.Fatalf("allow list not applied: %+v", got)
	}
}

func TestListToolsFollowsPagination(t *testing.T) {
	s := &fakeServer{pages: [][]RemoteTool{
		{{Name: "one"}},
		{{Name: "two"}},
		{{Name: "three"}},
	}}
	c, _ := newTestClient(t, s, ServerConfig{})

	got, err := c.ListTools(context.Background())
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("want 3 tools across pages, got %+v", got)
	}
}

func TestCallToolSendsArgumentsAndReturnsText(t *testing.T) {
	s := &fakeServer{tools: []RemoteTool{{Name: "lookup"}}, callText: "invoice 42 is paid"}
	c, _ := newTestClient(t, s, ServerConfig{})

	res, err := c.CallTool(context.Background(), "lookup", json.RawMessage(`{"id":42}`))
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Fatal("result should not be flagged an error")
	}
	if res.Text != "invoice 42 is paid" {
		t.Fatalf("unexpected text %q", res.Text)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if string(s.lastArgs) != `{"id":42}` {
		t.Fatalf("arguments were not forwarded verbatim: %s", s.lastArgs)
	}
}

// A tool-level failure is data for the model, not a transport failure: it comes
// back as a flagged result so the caller can decide how to surface it.
func TestCallToolSurfacesToolError(t *testing.T) {
	s := &fakeServer{callText: "no such invoice", callError: true}
	c, _ := newTestClient(t, s, ServerConfig{})

	res, err := c.CallTool(context.Background(), "lookup", nil)
	if err != nil {
		t.Fatalf("a tool-level error must not be a transport error: %v", err)
	}
	if !res.IsError || res.Text != "no such invoice" {
		t.Fatalf("unexpected result %+v", res)
	}
}

// A JSON-RPC-level error means no result was produced at all — that IS a Go
// error.
func TestCallToolPropagatesProtocolError(t *testing.T) {
	s := &fakeServer{rpcError: "unknown tool"}
	c, _ := newTestClient(t, s, ServerConfig{})

	if _, err := c.CallTool(context.Background(), "nope", nil); err == nil {
		t.Fatal("want an error for a JSON-RPC error response")
	} else if !strings.Contains(err.Error(), "unknown tool") {
		t.Fatalf("error should carry the server's message, got %v", err)
	}
}

func TestEventStreamTransport(t *testing.T) {
	s := &fakeServer{sse: true, tools: []RemoteTool{{Name: "lookup"}}, callText: "streamed answer"}
	c, _ := newTestClient(t, s, ServerConfig{})

	got, err := c.ListTools(context.Background())
	if err != nil {
		t.Fatalf("ListTools over SSE: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 tool, got %+v", got)
	}
	res, err := c.CallTool(context.Background(), "lookup", nil)
	if err != nil {
		t.Fatalf("CallTool over SSE: %v", err)
	}
	if res.Text != "streamed answer" {
		t.Fatalf("unexpected text %q", res.Text)
	}
}

func TestSessionIDIsEchoedBack(t *testing.T) {
	s := &fakeServer{sessionID: "sess-abc", tools: []RemoteTool{{Name: "lookup"}}}
	c, _ := newTestClient(t, s, ServerConfig{})

	if _, err := c.ListTools(context.Background()); err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.seenSessions) == 0 {
		t.Fatal("client never echoed the session id back")
	}
	for _, sid := range s.seenSessions {
		if sid != "sess-abc" {
			t.Fatalf("unexpected session id %q", sid)
		}
	}
}

func TestOperatorHeadersCannotClobberProtocolHeaders(t *testing.T) {
	s := &fakeServer{tools: []RemoteTool{{Name: "lookup"}}}
	c, _ := newTestClient(t, s, ServerConfig{Headers: map[string]string{
		"Authorization": "Bearer tok",
		"Content-Type":  "text/plain",
	}})

	if _, err := c.ListTools(context.Background()); err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if got := s.seenHeaders.Get("Authorization"); got != "Bearer tok" {
		t.Fatalf("operator header not sent, got %q", got)
	}
	if got := s.seenHeaders.Get("Content-Type"); got != "application/json" {
		t.Fatalf("operator header overrode the protocol content type: %q", got)
	}
}

func TestHTTPErrorIsReported(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "upstream is down", http.StatusBadGateway)
	}))
	defer srv.Close()
	c, err := New(ServerConfig{Name: "fake", URL: srv.URL, AllowHTTP: true}, WithHTTPClient(srv.Client()))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = c.ListTools(context.Background())
	if err == nil || !strings.Contains(err.Error(), "502") {
		t.Fatalf("want an http 502 error, got %v", err)
	}
}

func TestNewValidatesURL(t *testing.T) {
	cases := []struct {
		name string
		cfg  ServerConfig
		want string
	}{
		{"no name", ServerConfig{URL: "https://x.test/mcp"}, "requires a name"},
		{"plain http", ServerConfig{Name: "s", URL: "http://x.test/mcp"}, "refusing plain http"},
		{"bad scheme", ServerConfig{Name: "s", URL: "ftp://x.test/mcp"}, "must be http(s)"},
		{"no host", ServerConfig{Name: "s", URL: "https:///mcp"}, "no host"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := New(tc.cfg)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want error containing %q, got %v", tc.want, err)
			}
		})
	}
	if _, err := New(ServerConfig{Name: "s", URL: "https://x.test/mcp"}); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
}

// The SSRF backstop is the reason an operator-supplied URL is safe to accept at
// all: without an injected client, a loopback server must be refused at dial.
func TestGuardedTransportRefusesLoopback(t *testing.T) {
	s := &fakeServer{tools: []RemoteTool{{Name: "lookup"}}}
	srv := httptest.NewServer(s.handler())
	defer srv.Close()

	c, err := New(ServerConfig{Name: "fake", URL: srv.URL, AllowHTTP: true}) // no WithHTTPClient
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := c.ListTools(context.Background()); err == nil {
		t.Fatal("guarded transport must refuse a loopback MCP server")
	}
}
