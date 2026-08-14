package agentruntime

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lohi-ai/agentray/agentcore"
	"github.com/lohi-ai/agentray/internal/dataplane/store"
	"github.com/lohi-ai/agentray/internal/shared/credential"
	"github.com/lohi-ai/agentray/internal/shared/mcpclient"
)

// mcpSelection is one enabled `mcp` selection carrying the given config.
func mcpSelection(config string) []storage.AgentToolSelection {
	return []storage.AgentToolSelection{{Name: ToolMCP, Enabled: true, ConfigJSON: config}}
}

// unreachableMCPURL returns the URL of a live loopback server. It is reachable
// by nothing in the run path: the SSRF backstop refuses loopback, which is
// exactly the protection that makes accepting an operator-supplied URL safe —
// and conveniently gives these tests a server that is up but undialable.
func unreachableMCPURL(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestMCPIsInTheCatalog(t *testing.T) {
	var found *ToolCatalogEntry
	for _, e := range ToolCatalog(ToolBuildContext{}) {
		if e.Name == ToolMCP {
			entry := e
			found = &entry
		}
	}
	if found == nil {
		t.Fatal("mcp must be selectable from the catalog; it is the tenant extension path")
	}
	if !found.Configurable {
		t.Fatal("mcp carries per-agent config (the server list)")
	}
	// No host dependency: an operator can grant it on any deployment.
	if !ToolAvailable(ToolBuildContext{}, ToolMCP) {
		t.Fatal("mcp needs no sandbox or workspace to be available")
	}
}

// A remote server can do anything behind its API, so its tools must be treated
// as capable of publishing — that is what keeps them off unattended runs unless
// the project opted into full autonomy.
func TestRemoteToolsAreExternalWrite(t *testing.T) {
	if !ToolExternalWrite(ToolMCP) {
		t.Fatal("the mcp selection itself must be external-write")
	}
	if !ToolExternalWrite(mcpclient.ToolName("billing", "lookup")) {
		t.Fatal("a tool contributed by a remote server must be external-write")
	}
	// The prefix rule must not swallow ordinary tools.
	if ToolExternalWrite("run_shell") {
		t.Fatal("run_shell is not external-write")
	}
}

// The predicate above is only useful if the rail actually consumes it: a remote
// tool must be gone from an unattended run at anything below 'auto', while the
// project's own internal tools survive.
func TestAutonomyRailStripsRemoteMCPTools(t *testing.T) {
	remote := mcpclient.ToolName("billing", "charge_card")
	fixture := []agentcore.Tool{fakeTool{name: remote}, fakeTool{name: "run_sql"}}

	for _, trigger := range []string{"scheduled", "webhook", "delegate"} {
		for _, autonomy := range []string{storage.AutonomySuggest, storage.AutonomyScheduled} {
			names := toolNames(applyAutonomyRail(fixture, trigger, autonomy))
			if names[remote] {
				t.Errorf("trigger=%s autonomy=%s: remote MCP tool must be stripped", trigger, autonomy)
			}
			if !names["run_sql"] {
				t.Errorf("trigger=%s autonomy=%s: internal tools must survive, got %v", trigger, autonomy, names)
			}
		}
		if names := toolNames(applyAutonomyRail(fixture, trigger, storage.AutonomyAuto)); !names[remote] {
			t.Errorf("trigger=%s: autonomy 'auto' is the opt-in that keeps remote tools", trigger)
		}
	}
	// Interactive chat is never gated by the rail.
	if names := toolNames(applyAutonomyRail(fixture, "chat", storage.AutonomySuggest)); !names[remote] {
		t.Error("an attended chat run keeps its remote tools")
	}
}

func TestValidateMCPConfigIsOfflineAndStrict(t *testing.T) {
	cases := []struct {
		name   string
		config string
		want   string
	}{
		{"empty", "{}", "at least one server"},
		{"no name", `{"servers":[{"url":"https://mcp.example.test/mcp"}]}`, "has no name"},
		{"plain http", `{"servers":[{"name":"a","url":"http://mcp.example.test/mcp"}]}`, "refusing plain http"},
		{"bad scheme", `{"servers":[{"name":"a","url":"ftp://mcp.example.test/mcp"}]}`, "must be http(s)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateToolConfig(ToolBuildContext{}, ToolMCP, tc.config)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want error containing %q, got %v", tc.want, err)
			}
		})
	}

	// A well-formed config validates without I/O and without a vault, even though
	// it names a secret the control plane is not holding — the placeholder is
	// resolved at run time.
	ok := `{"servers":[{"name":"billing","url":"https://mcp.example.test/mcp","headers":{"Authorization":"Bearer {{cred:TOKEN}}"}}]}`
	if err := ValidateToolConfig(ToolBuildContext{}, ToolMCP, ok); err != nil {
		t.Fatalf("valid config rejected at write time: %v", err)
	}
}

// The single `mcp` selection must not reach BuildToolWithContext, which can only
// hand back one tool.
func TestBuildToolWithContextRejectsExpandingSelection(t *testing.T) {
	_, err := BuildToolWithContext(ToolBuildContext{}, ToolMCP, `{"servers":[{"name":"a","url":"https://x.test/mcp"}]}`)
	if err == nil || !strings.Contains(err.Error(), "several tools") {
		t.Fatalf("want a use-the-plural-builder error, got %v", err)
	}
}

// Configuration errors are the operator's, not the weather's: they fail the run
// closed rather than quietly dropping a granted capability.
func TestResolveRunToolsFailsClosedOnMalformedMCPConfig(t *testing.T) {
	_, _, err := resolveRunTools(context.Background(), ToolBuildContext{}, nil, mcpSelection(`{"servers":[]}`))
	if err == nil || !strings.Contains(err.Error(), "at least one server") {
		t.Fatalf("want a fail-closed error, got %v", err)
	}
}

// An unresolvable secret is configuration too — never ship a literal
// "{{cred:…}}" as an Authorization header.
func TestResolveRunToolsFailsClosedOnUnresolvableSecret(t *testing.T) {
	vault, err := credential.FromMap(map[string]string{"OTHER": "x"})
	if err != nil {
		t.Fatalf("FromMap: %v", err)
	}
	config := `{"servers":[{"name":"billing","url":"https://mcp.example.test/mcp","headers":{"Authorization":"Bearer {{cred:MISSING}}"}}]}`
	_, _, err = resolveRunTools(context.Background(), ToolBuildContext{Credentials: vault}, nil, mcpSelection(config))
	if err == nil {
		t.Fatal("an unresolvable placeholder must fail the run closed")
	}
}

// A third-party outage is weather: the run continues without that server's
// tools, and says so.
func TestResolveRunToolsSkipsUnreachableOptionalServer(t *testing.T) {
	config := `{"servers":[{"name":"flaky","url":"` + unreachableMCPURL(t) + `","allow_http":true}]}`
	tools, notes, err := resolveRunTools(context.Background(), ToolBuildContext{}, nil, mcpSelection(config))
	if err != nil {
		t.Fatalf("an optional server being down must not fail the run: %v", err)
	}
	if len(tools) != 0 {
		t.Fatalf("a skipped server contributes no tools, got %v", toolNames(tools))
	}
	if len(notes) != 1 || !strings.Contains(notes[0], "flaky") {
		t.Fatalf("the skip must be reported, got %v", notes)
	}
}

// "required" is the opt-in for the opposite call: an agent whose answers would
// be wrong without this server should not run at all.
func TestResolveRunToolsFailsOnUnreachableRequiredServer(t *testing.T) {
	config := `{"servers":[{"name":"ledger","url":"` + unreachableMCPURL(t) + `","allow_http":true,"required":true}]}`
	_, _, err := resolveRunTools(context.Background(), ToolBuildContext{}, nil, mcpSelection(config))
	if err == nil || !strings.Contains(err.Error(), "required mcp server") {
		t.Fatalf("want a required-server failure, got %v", err)
	}
}

// A disabled selection contributes nothing and is never dialed.
func TestResolveRunToolsIgnoresDisabledMCPSelection(t *testing.T) {
	sel := []storage.AgentToolSelection{{Name: ToolMCP, Enabled: false, ConfigJSON: `{"servers":[]}`}}
	tools, notes, err := resolveRunTools(context.Background(), ToolBuildContext{}, nil, sel)
	if err != nil {
		t.Fatalf("a disabled selection must not be built: %v", err)
	}
	if len(tools) != 0 || len(notes) != 0 {
		t.Fatalf("disabled selection produced %v / %v", toolNames(tools), notes)
	}
}
