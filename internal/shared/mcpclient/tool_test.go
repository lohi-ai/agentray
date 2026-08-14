package mcpclient

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestToolsAdaptRemoteTools(t *testing.T) {
	s := &fakeServer{
		tools: []RemoteTool{
			{Name: "lookup_invoice", Description: "Look up an invoice by id.", InputSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{"id": map[string]any{"type": "integer"}},
				"required":   []any{"id"},
			}},
		},
		callText: "invoice 42 is paid",
	}
	c, _ := newTestClient(t, s, ServerConfig{Name: "billing"})

	tools, err := Tools(context.Background(), c)
	if err != nil {
		t.Fatalf("Tools: %v", err)
	}
	if len(tools) != 1 {
		t.Fatalf("want 1 tool, got %d", len(tools))
	}
	tool := tools[0]
	if tool.Name() != "mcp__billing__lookup_invoice" {
		t.Fatalf("unexpected local name %q", tool.Name())
	}

	schema := tool.Schema()
	if schema.Name != tool.Name() {
		t.Fatalf("schema name %q must match tool name %q", schema.Name, tool.Name())
	}
	if !strings.Contains(schema.Description, "Look up an invoice") {
		t.Fatalf("remote description lost: %q", schema.Description)
	}
	if !strings.Contains(schema.Description, `"billing"`) {
		t.Fatalf("description should name the source server: %q", schema.Description)
	}
	props, ok := schema.Parameters["properties"].(map[string]any)
	if !ok || props["id"] == nil {
		t.Fatalf("remote input schema was not republished: %+v", schema.Parameters)
	}

	out, err := tool.Run(context.Background(), `{"id":42}`)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out != "invoice 42 is paid" {
		t.Fatalf("unexpected result %q", out)
	}
	// The remote name — not the namespaced local one — goes over the wire.
	s.mu.Lock()
	defer s.mu.Unlock()
	if string(s.lastArgs) != `{"id":42}` {
		t.Fatalf("arguments not forwarded: %s", s.lastArgs)
	}
}

// A remote tool that reports failure must surface as a Go error so the loop's
// per-run circuit breaker counts it and eventually disables the tool.
func TestRemoteToolErrorFeedsTheBreaker(t *testing.T) {
	s := &fakeServer{tools: []RemoteTool{{Name: "flaky"}}, callText: "upstream refused", callError: true}
	c, _ := newTestClient(t, s, ServerConfig{Name: "svc"})

	tools, err := Tools(context.Background(), c)
	if err != nil {
		t.Fatalf("Tools: %v", err)
	}
	if _, err := tools[0].Run(context.Background(), `{}`); err == nil {
		t.Fatal("a tool-reported error must be an error at the agentcore boundary")
	} else if !strings.Contains(err.Error(), "upstream refused") {
		t.Fatalf("error should carry the server's text, got %v", err)
	}
}

// A server that advertises no schema still has to be callable: the remote side
// validates its own arguments.
func TestSchemalessToolGetsPermissiveSchema(t *testing.T) {
	s := &fakeServer{tools: []RemoteTool{{Name: "bare"}}, callText: "ok"}
	c, _ := newTestClient(t, s, ServerConfig{Name: "svc"})

	tools, err := Tools(context.Background(), c)
	if err != nil {
		t.Fatalf("Tools: %v", err)
	}
	schema := tools[0].Schema()
	if schema.Parameters["type"] != "object" {
		t.Fatalf("want a permissive object schema, got %+v", schema.Parameters)
	}
	if strings.TrimSpace(schema.Description) == "" {
		t.Fatal("a description-less remote tool still needs one for the model")
	}
}

func TestToolName(t *testing.T) {
	cases := []struct{ server, tool, want string }{
		{"billing", "lookup", "mcp__billing__lookup"},
		{"My Server!", "do a thing", "mcp__My_Server__do_a_thing"},
		{"a", strings.Repeat("t", 80), "mcp__a__" + strings.Repeat("t", 80)[:56]},
	}
	for _, tc := range cases {
		got := ToolName(tc.server, tc.tool)
		if len(got) > maxToolNameLen {
			t.Fatalf("ToolName(%q,%q) = %q exceeds the %d-char provider cap", tc.server, tc.tool, got, maxToolNameLen)
		}
		if got != tc.want {
			t.Fatalf("ToolName(%q,%q) = %q, want %q", tc.server, tc.tool, got, tc.want)
		}
	}
	// A long server name is trimmed before the remote tool name is touched: the
	// model reasons about the tool, not the server label.
	long := ToolName(strings.Repeat("s", 80), "lookup")
	if len(long) > maxToolNameLen || !strings.HasSuffix(long, "__lookup") {
		t.Fatalf("server segment should absorb the overflow, got %q", long)
	}
}

func TestToolsDropDuplicateComposedNames(t *testing.T) {
	// Two remote names that sanitize to the same local name.
	s := &fakeServer{tools: []RemoteTool{{Name: "do thing"}, {Name: "do-thing"}, {Name: "do_thing"}}}
	c, _ := newTestClient(t, s, ServerConfig{Name: "svc"})

	tools, err := Tools(context.Background(), c)
	if err != nil {
		t.Fatalf("Tools: %v", err)
	}
	seen := map[string]bool{}
	for _, tool := range tools {
		if seen[tool.Name()] {
			t.Fatalf("duplicate advertised tool name %q", tool.Name())
		}
		seen[tool.Name()] = true
	}
}

func TestRunAcceptsEmptyArguments(t *testing.T) {
	s := &fakeServer{tools: []RemoteTool{{Name: "ping"}}, callText: "pong"}
	c, _ := newTestClient(t, s, ServerConfig{Name: "svc"})
	tools, err := Tools(context.Background(), c)
	if err != nil {
		t.Fatalf("Tools: %v", err)
	}
	if _, err := tools[0].Run(context.Background(), ""); err != nil {
		t.Fatalf("Run with empty args: %v", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var decoded map[string]any
	if err := json.Unmarshal(s.lastArgs, &decoded); err != nil {
		t.Fatalf("empty args should become an empty object, got %s", s.lastArgs)
	}
}
