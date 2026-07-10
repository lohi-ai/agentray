package opcore

import (
	"encoding/json"
	"io"
	"net/http"

	"github.com/labstack/echo/v4"
)

// MCP (Model Context Protocol) is the fourth adapter over the operation registry,
// alongside the in-process tool (tool.go), the REST endpoint (http.go), and the
// CLI transport (client.go). It lets an *external* agent — Claude Code, Codex,
// any MCP client — call the same usecase handlers the in-house agent calls, with
// no second definition. One operation, one schema, one handler; now four
// consumers that cannot drift.
//
// The transport is the MCP "Streamable HTTP" binding: a single POST endpoint that
// speaks JSON-RPC 2.0. We implement the tools-only subset an analytics client
// needs (initialize, tools/list, tools/call, ping) by hand rather than vendor an
// SDK — the surface is small, depends only on encoding/json + Echo, and stays
// inside opcore's no-storage wall like the other adapters.
//
// Auth is the same ProjectResolver the REST adapter uses: the resolved project id
// scopes every CallContext. An MCP client authenticates per request by carrying
// the project's API key (X-API-Key header or ?api_key=), so no OAuth dance is
// required for self-hosted use.

const mcpProtocolVersion = "2025-06-18"

// jsonRPCRequest is one inbound JSON-RPC 2.0 message. A request carries an id and
// expects a response; a notification omits id and expects none.
type jsonRPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type jsonRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type jsonRPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *jsonRPCError   `json:"error,omitempty"`
}

// mcpTool is one entry in a tools/list response, derived straight from a Spec.
// The description doubles as workflow choreography for the model: a good Summary
// says when to reach for the tool and what it costs, so a flat list orchestrates
// itself.
type mcpTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

// mcpToolResult is the tools/call payload. We hand back both a text rendering
// (always present, the human/model-readable view) and structuredContent (the
// parsed object, when the handler returned one) plus a _meta envelope carrying
// the acting project — mirroring the dual-channel result shape that lets a client
// show a number and still link back to where it came from.
type mcpToolResult struct {
	Content           []mcpContent   `json:"content"`
	StructuredContent any            `json:"structuredContent,omitempty"`
	Meta              map[string]any `json:"_meta,omitempty"`
	IsError           bool           `json:"isError,omitempty"`
}

type mcpContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// MountMCP registers the JSON-RPC endpoint at the group root (e.g. POST /mcp).
// deps is the same dependency bundle the REST and in-process adapters use, so an
// external MCP client runs the identical usecase code as the in-house agent.
func MountMCP(g *echo.Group, r *Registry, deps any, resolve ProjectResolver) {
	g.POST("", func(c echo.Context) error { return handleMCP(c, r, deps, resolve) })
	g.POST("/", func(c echo.Context) error { return handleMCP(c, r, deps, resolve) })
}

func handleMCP(c echo.Context, r *Registry, deps any, resolve ProjectResolver) error {
	body, err := io.ReadAll(c.Request().Body)
	if err != nil {
		return c.JSON(http.StatusOK, rpcError(nil, -32700, "parse error: unreadable body"))
	}

	// JSON-RPC permits a batch (array) or a single object. Probe the first
	// non-space byte so both shapes are handled with one decode path.
	trimmed := firstNonSpace(body)
	if trimmed == '[' {
		var batch []jsonRPCRequest
		if err := json.Unmarshal(body, &batch); err != nil {
			return c.JSON(http.StatusOK, rpcError(nil, -32700, "parse error"))
		}
		var responses []jsonRPCResponse
		for i := range batch {
			if resp, ok := dispatch(c, r, deps, resolve, batch[i]); ok {
				responses = append(responses, resp)
			}
		}
		if len(responses) == 0 {
			return c.NoContent(http.StatusAccepted)
		}
		return c.JSON(http.StatusOK, responses)
	}

	var req jsonRPCRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return c.JSON(http.StatusOK, rpcError(nil, -32700, "parse error"))
	}
	resp, ok := dispatch(c, r, deps, resolve, req)
	if !ok {
		// Notification — acknowledged with no body per the HTTP transport.
		return c.NoContent(http.StatusAccepted)
	}
	return c.JSON(http.StatusOK, resp)
}

// dispatch routes one JSON-RPC message. The bool is false for notifications
// (id absent), which must not produce a response.
func dispatch(c echo.Context, r *Registry, deps any, resolve ProjectResolver, req jsonRPCRequest) (jsonRPCResponse, bool) {
	isNotification := len(req.ID) == 0
	switch req.Method {
	case "initialize":
		return ok(req.ID, initializeResult(req.Params)), !isNotification
	case "notifications/initialized", "notifications/cancelled":
		return jsonRPCResponse{}, false
	case "ping":
		return ok(req.ID, map[string]any{}), !isNotification
	case "tools/list":
		return ok(req.ID, map[string]any{"tools": listTools(r)}), !isNotification
	case "tools/call":
		if isNotification {
			return jsonRPCResponse{}, false
		}
		return callTool(c, r, deps, resolve, req), true
	default:
		if isNotification {
			return jsonRPCResponse{}, false
		}
		return rpcError(req.ID, -32601, "method not found: "+req.Method), true
	}
}

func initializeResult(params json.RawMessage) map[string]any {
	// Echo the client's requested protocol version when it sends one; this is the
	// version-negotiation contract of MCP. Fall back to the version we target.
	version := mcpProtocolVersion
	if len(params) > 0 {
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		if json.Unmarshal(params, &p) == nil && p.ProtocolVersion != "" {
			version = p.ProtocolVersion
		}
	}
	return map[string]any{
		"protocolVersion": version,
		"capabilities":    map[string]any{"tools": map[string]any{}},
		"serverInfo":      map[string]any{"name": "agentray", "version": "1"},
	}
}

func listTools(r *Registry) []mcpTool {
	specs := r.Specs()
	tools := make([]mcpTool, 0, len(specs))
	for _, s := range specs {
		tools = append(tools, mcpTool{
			Name:        s.OpName(),
			Description: s.OpSummary(),
			InputSchema: s.OpSchema(),
		})
	}
	return tools
}

func callTool(c echo.Context, r *Registry, deps any, resolve ProjectResolver, req jsonRPCRequest) jsonRPCResponse {
	var params struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return rpcError(req.ID, -32602, "invalid params")
	}
	spec, found := r.Get(params.Name)
	if !found {
		return rpcError(req.ID, -32602, "unknown tool: "+params.Name)
	}

	projectID, err := resolve(c)
	if err != nil {
		// Auth failures are reported as a tool error (isError) rather than a
		// protocol error, so the model sees the reason and can surface it.
		return ok(req.ID, errorResult("authentication failed: "+httpErrMessage(err)))
	}

	args := string(params.Arguments)
	if args == "" {
		args = "{}"
	}
	cc := CallContext{ProjectID: projectID, Deps: deps}
	out, err := spec.OpInvoke(c.Request().Context(), cc, args)
	if err != nil {
		return ok(req.ID, errorResult(err.Error()))
	}

	result := mcpToolResult{
		Content: []mcpContent{{Type: "text", Text: out}},
		Meta:    map[string]any{"project_id": projectID},
	}
	// When the handler returned a JSON object, expose it as structuredContent so a
	// client can render it natively; leave it off for scalars/arrays the spec says
	// structuredContent must be an object.
	var obj map[string]any
	if json.Unmarshal([]byte(out), &obj) == nil {
		result.StructuredContent = obj
	}
	return ok(req.ID, result)
}

func errorResult(msg string) mcpToolResult {
	return mcpToolResult{
		Content: []mcpContent{{Type: "text", Text: msg}},
		IsError: true,
	}
}

func ok(id json.RawMessage, result any) jsonRPCResponse {
	return jsonRPCResponse{JSONRPC: "2.0", ID: id, Result: result}
}

func rpcError(id json.RawMessage, code int, message string) jsonRPCResponse {
	return jsonRPCResponse{JSONRPC: "2.0", ID: id, Error: &jsonRPCError{Code: code, Message: message}}
}

func firstNonSpace(b []byte) byte {
	for _, ch := range b {
		switch ch {
		case ' ', '\t', '\n', '\r':
			continue
		default:
			return ch
		}
	}
	return 0
}

// httpErrMessage extracts a readable string from an echo.HTTPError (what the
// ProjectResolver returns) so the auth reason reaches the model intact.
func httpErrMessage(err error) string {
	if he, ok := err.(*echo.HTTPError); ok {
		if s, ok := he.Message.(string); ok {
			return s
		}
	}
	return err.Error()
}
