package opcore

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"
)

type echoIn struct {
	Text string `json:"text" desc:"text to echo" required:"true"`
}

type echoOut struct {
	Echoed    string `json:"echoed"`
	ProjectID string `json:"project_id"`
}

func echoOp() Operation[echoIn, echoOut] {
	return Operation[echoIn, echoOut]{
		Name:    "echo",
		Summary: "Echo the input text back. Use to verify the MCP connection.",
		Handler: func(_ context.Context, cc CallContext, in echoIn) (echoOut, error) {
			return echoOut{Echoed: in.Text, ProjectID: cc.ProjectID}, nil
		},
	}
}

// mcpServer wires a one-operation registry behind MountMCP, resolving every
// request to a fixed project unless the header says "deny".
func mcpServer(t *testing.T) *echo.Echo {
	t.Helper()
	reg := NewRegistry()
	Register(reg, echoOp())
	e := echo.New()
	MountMCP(e.Group("/mcp"), reg, struct{}{}, func(c echo.Context) (string, error) {
		if c.Request().Header.Get("X-Deny") != "" {
			return "", echo.NewHTTPError(http.StatusUnauthorized, "invalid api key")
		}
		return "proj-1", nil
	})
	return e
}

func post(t *testing.T, e *echo.Echo, body string, headers map[string]string) map[string]any {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK && rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if rec.Code == http.StatusAccepted {
		return nil
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode response: %v (body %s)", err, rec.Body.String())
	}
	return out
}

func TestMCPInitialize(t *testing.T) {
	e := mcpServer(t)
	resp := post(t, e, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26"}}`, nil)
	result, ok := resp["result"].(map[string]any)
	if !ok {
		t.Fatalf("no result: %v", resp)
	}
	if result["protocolVersion"] != "2025-03-26" {
		t.Errorf("protocolVersion not echoed: %v", result["protocolVersion"])
	}
	caps, _ := result["capabilities"].(map[string]any)
	if _, hasTools := caps["tools"]; !hasTools {
		t.Errorf("expected tools capability, got %v", caps)
	}
}

func TestMCPToolsList(t *testing.T) {
	e := mcpServer(t)
	resp := post(t, e, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`, nil)
	result := resp["result"].(map[string]any)
	tools := result["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(tools))
	}
	tool := tools[0].(map[string]any)
	if tool["name"] != "echo" {
		t.Errorf("name = %v", tool["name"])
	}
	schema := tool["inputSchema"].(map[string]any)
	req, _ := schema["required"].([]any)
	if len(req) != 1 || req[0] != "text" {
		t.Errorf("required = %v", req)
	}
}

func TestMCPToolsCall(t *testing.T) {
	e := mcpServer(t)
	resp := post(t, e, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"echo","arguments":{"text":"hi"}}}`, nil)
	result := resp["result"].(map[string]any)
	if result["isError"] == true {
		t.Fatalf("unexpected error result: %v", result)
	}
	content := result["content"].([]any)
	text := content[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, "hi") || !strings.Contains(text, "proj-1") {
		t.Errorf("text missing echo/project: %s", text)
	}
	sc := result["structuredContent"].(map[string]any)
	if sc["echoed"] != "hi" || sc["project_id"] != "proj-1" {
		t.Errorf("structuredContent = %v", sc)
	}
	meta := result["_meta"].(map[string]any)
	if meta["project_id"] != "proj-1" {
		t.Errorf("_meta project = %v", meta)
	}
}

func TestMCPToolsCallMissingRequired(t *testing.T) {
	e := mcpServer(t)
	resp := post(t, e, `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"echo","arguments":{}}}`, nil)
	result := resp["result"].(map[string]any)
	if result["isError"] != true {
		t.Fatalf("expected isError for missing required field: %v", result)
	}
	text := result["content"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, "text") {
		t.Errorf("error should name missing field: %s", text)
	}
}

func TestMCPToolsCallAuthFailure(t *testing.T) {
	e := mcpServer(t)
	resp := post(t, e, `{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"echo","arguments":{"text":"hi"}}}`,
		map[string]string{"X-Deny": "1"})
	result := resp["result"].(map[string]any)
	if result["isError"] != true {
		t.Fatalf("expected auth error result: %v", result)
	}
	text := result["content"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, "invalid api key") {
		t.Errorf("auth reason not surfaced: %s", text)
	}
}

func TestMCPUnknownTool(t *testing.T) {
	e := mcpServer(t)
	resp := post(t, e, `{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"nope","arguments":{}}}`, nil)
	rpcErr, ok := resp["error"].(map[string]any)
	if !ok {
		t.Fatalf("expected protocol error for unknown tool: %v", resp)
	}
	if !strings.Contains(rpcErr["message"].(string), "nope") {
		t.Errorf("error = %v", rpcErr)
	}
}

func TestMCPNotificationNoResponse(t *testing.T) {
	e := mcpServer(t)
	// A notification (no id) must produce no response body — 202 Accepted.
	resp := post(t, e, `{"jsonrpc":"2.0","method":"notifications/initialized"}`, nil)
	if resp != nil {
		t.Errorf("notification should yield no body, got %v", resp)
	}
}

func TestMCPUnknownMethod(t *testing.T) {
	e := mcpServer(t)
	resp := post(t, e, `{"jsonrpc":"2.0","id":7,"method":"bogus/method"}`, nil)
	rpcErr, ok := resp["error"].(map[string]any)
	if !ok {
		t.Fatalf("expected method-not-found error: %v", resp)
	}
	if rpcErr["code"].(float64) != -32601 {
		t.Errorf("code = %v", rpcErr["code"])
	}
}
