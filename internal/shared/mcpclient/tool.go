package mcpclient

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/lohi-ai/agentray/agentcore"
)

// ToolPrefix marks every tool sourced from a remote MCP server. It is load
// bearing beyond cosmetics: the runtime uses it to recognise a remote capability
// it has no catalog entry for — chiefly to treat one as external-write under the
// unattended-publish rail, since a third-party server can do anything behind its
// own API.
const ToolPrefix = "mcp__"

// maxToolNameLen is the tightest advertised-name limit across the providers we
// target (OpenAI's function-name cap). Namespacing can push a long remote name
// past it, so composed names are trimmed to fit.
const maxToolNameLen = 64

// unsafeNameChars matches everything outside the character class every provider
// accepts in a tool name.
var unsafeNameChars = regexp.MustCompile(`[^a-zA-Z0-9_-]+`)

// Tools connects to the server, lists what it offers, and adapts each entry to
// an agentcore.Tool. The names are namespaced mcp__<server>__<tool> so two
// servers exposing the same tool name stay distinct and an operator reading a
// trace can see which server ran what.
//
// Discovery is eager (one tools/list per server per run) rather than lazy: the
// model has to be told what exists before it can choose, and a mid-turn
// discovery would make the advertised tool set change under it.
func Tools(ctx context.Context, c *Client) ([]agentcore.Tool, error) {
	remote, err := c.ListTools(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]agentcore.Tool, 0, len(remote))
	seen := make(map[string]bool, len(remote))
	for _, rt := range remote {
		name := ToolName(c.Name(), rt.Name)
		if seen[name] {
			// Two remote names that collide only after sanitizing/trimming. Keep
			// the first; silently shadowing the second would make the trace lie.
			continue
		}
		seen[name] = true
		out = append(out, &remoteTool{client: c, local: name, remote: rt.Name, desc: rt.Description, schema: rt.InputSchema})
	}
	return out, nil
}

// ToolName composes the local, provider-safe name for a remote tool.
func ToolName(server, tool string) string {
	name := ToolPrefix + sanitize(server) + "__" + sanitize(tool)
	if len(name) <= maxToolNameLen {
		return name
	}
	// Trim from the server segment first: the remote tool name is what the model
	// reasons about, so it keeps its characters.
	overflow := len(name) - maxToolNameLen
	srv := sanitize(server)
	if len(srv) > overflow {
		return ToolPrefix + srv[:len(srv)-overflow] + "__" + sanitize(tool)
	}
	return name[:maxToolNameLen]
}

// sanitize maps an operator- or server-supplied identifier into the character
// class providers accept, collapsing runs of anything else to a single "_".
func sanitize(s string) string {
	cleaned := unsafeNameChars.ReplaceAllString(strings.TrimSpace(s), "_")
	return strings.Trim(cleaned, "_")
}

// remoteTool adapts one tool on a remote MCP server to the agentcore.Tool
// interface. It holds no state of its own: the client owns the connection and
// the handshake.
type remoteTool struct {
	client *Client
	local  string
	remote string
	desc   string
	schema map[string]any
}

func (t *remoteTool) Name() string { return t.local }

// Schema republishes the server's advertised input schema under the local name.
// A server that advertises no schema (or a non-object one) gets a permissive
// object schema rather than being dropped: the remote side validates its own
// arguments, and refusing to expose the tool would be a worse failure than
// passing an argument through.
func (t *remoteTool) Schema() agentcore.ToolSchema {
	params := t.schema
	if params == nil || params["type"] == nil {
		params = map[string]any{"type": "object", "properties": map[string]any{}}
	}
	desc := strings.TrimSpace(t.desc)
	if desc == "" {
		desc = fmt.Sprintf("Tool %q on the %q MCP server.", t.remote, t.client.Name())
	}
	// Naming the server in the description keeps the model's own account of what
	// it did honest ("I asked the billing server"), which matters when several
	// servers offer overlapping capabilities.
	desc = fmt.Sprintf("%s\n\n(Provided by the %q MCP server.)", desc, t.client.Name())
	return agentcore.ToolSchema{Name: t.local, Description: desc, Parameters: params}
}

// Run forwards the call to the remote server. A server-reported tool error comes
// back as an error so the loop's circuit breaker counts it: a remote tool
// failing repeatedly is exactly the case that breaker exists for.
func (t *remoteTool) Run(ctx context.Context, args string) (string, error) {
	res, err := t.client.CallTool(ctx, t.remote, json.RawMessage(args))
	if err != nil {
		return "", err
	}
	if res.IsError {
		return "", fmt.Errorf("%s: %s", t.local, res.Text)
	}
	return res.Text, nil
}
