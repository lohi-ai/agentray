package agentruntime

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/lohi-ai/agentray/agentcore"
	"github.com/lohi-ai/agentray/internal/httptool"
	"github.com/lohi-ai/agentray/sandbox"
)

// The selectable-tool registry (AgentGarden §6). It is the single catalog of
// tools a project can turn on per-agent and the only place that knows how to
// build one from its stored config JSON. Adding a new selectable tool is a
// registry entry plus its builder — no change to Build, the runner, or the
// control-plane handlers, which is the AgentGarden win condition ("a new tool
// is data, not a backend change").

// ToolBuildContext is the runtime host surface a selectable-tool builder may
// need. Most tools are config-only (http_request); risky tools such as run_shell
// are built only when the host injected the required isolation substrate.
type ToolBuildContext struct {
	Sandbox   agentcore.Sandbox
	Workspace *sandbox.Workspace
	// BrowserImage is the Chrome-capable sandbox image browser_use runs its
	// persistent session in (config: AGENTRAY_SANDBOX_BROWSER_IMAGE). Empty leaves
	// browser_use on the backend default image, which generally lacks a browser —
	// a deployment that grants browser_use should configure it.
	BrowserImage string
	// NetworkAllow, when non-empty, confines the computer_use session's egress to
	// the listed hosts and their subdomains (config:
	// AGENTRAY_SANDBOX_NETWORK_ALLOW, comma-separated). Empty keeps the current
	// open-network behavior. run_shell is unaffected (it never gets network).
	NetworkAllow []string
}

// ToolSpec describes one selectable tool: its stable name, human-facing catalog
// copy, whether it carries per-agent config, and how to build a live tool from
// that config.
type ToolSpec struct {
	Name         string
	Title        string
	Description  string
	Configurable bool
	available    func(ToolBuildContext) bool
	build        func(ToolBuildContext, string) (agentcore.Tool, error)
}

// ToolCatalogEntry is the JSON-serializable view of a ToolSpec for the
// control-plane catalog endpoint (the builder func is not exposed).
type ToolCatalogEntry struct {
	Name         string `json:"name"`
	Title        string `json:"title"`
	Description  string `json:"description"`
	Configurable bool   `json:"configurable"`
}

var toolRegistry = map[string]ToolSpec{
	httptool.ToolHTTPRequest: {
		Name:         httptool.ToolHTTPRequest,
		Title:        "Fetch / HTTP",
		Description:  "Make guarded outbound HTTP(S) requests to an allowlisted set of hosts. Use a {{cred:NAME}} placeholder for any secret; it is resolved at the trust boundary and never seen by the model.",
		Configurable: true,
		build:        buildHTTPRequestTool,
	},
	httptool.ToolWebFetch: {
		Name:         httptool.ToolWebFetch,
		Title:        "Web fetch",
		Description:  "Fetch a public web page and return its readable text (HTML stripped to text). No host allowlist, but loopback / private / link-local / cloud-metadata addresses are refused. Use http_request for authenticated calls to a specific API.",
		Configurable: false,
		build:        buildWebFetchTool,
	},
	sandbox.ToolRunShell: {
		Name:         sandbox.ToolRunShell,
		Title:        "Bash / shell",
		Description:  "Run shell commands inside the server-side sandbox. The tool never sees the host filesystem, host environment, or network unless the sandbox grants it.",
		Configurable: false,
		available:    func(ctx ToolBuildContext) bool { return ctx.Sandbox != nil },
		build:        buildRunShellTool,
	},
	sandbox.ToolComputerUse: {
		Name:         sandbox.ToolComputerUse,
		Title:        "Computer use",
		Description:  "A persistent, network-enabled Linux sandbox where the agent can install tools (pip/apt/npm), write and run code, and produce files — parse or generate PDF, DOCX, XLSX, PPTX, HTML. State persists across calls in the conversation. Higher-privilege than run_shell; grant it deliberately.",
		Configurable: false,
		available:    func(ctx ToolBuildContext) bool { return ctx.Sandbox != nil && ctx.Workspace != nil },
		build:        buildComputerUseTool,
	},
	sandbox.ToolReadFile: {
		Name:         sandbox.ToolReadFile,
		Title:        "Read file",
		Description:  "Read text files from the configured agent workspace. Paths must be relative and cannot escape the workspace root.",
		Configurable: false,
		available:    func(ctx ToolBuildContext) bool { return ctx.Workspace != nil },
		build:        buildReadFileTool,
	},
	sandbox.ToolWriteFile: {
		Name:         sandbox.ToolWriteFile,
		Title:        "Write file",
		Description:  "Write text files inside the configured agent workspace. Paths must be relative and parent directories are created as needed.",
		Configurable: false,
		available:    func(ctx ToolBuildContext) bool { return ctx.Workspace != nil },
		build:        buildWriteFileTool,
	},
	sandbox.ToolEditFile: {
		Name:         sandbox.ToolEditFile,
		Title:        "Edit file",
		Description:  "Make a surgical exact-string replacement in a workspace file instead of rewriting it. Refuses an ambiguous match unless replace_all is set.",
		Configurable: false,
		available:    func(ctx ToolBuildContext) bool { return ctx.Workspace != nil },
		build:        buildEditFileTool,
	},
	sandbox.ToolGrep: {
		Name:         sandbox.ToolGrep,
		Title:        "Grep",
		Description:  "Search workspace file contents by regular expression (RE2). Returns path:line:text matches, scoped by an optional subdirectory and filename glob.",
		Configurable: false,
		available:    func(ctx ToolBuildContext) bool { return ctx.Workspace != nil },
		build:        buildGrepTool,
	},
	sandbox.ToolGlob: {
		Name:         sandbox.ToolGlob,
		Title:        "Glob",
		Description:  "List workspace files whose relative path matches a glob pattern (supports *, ?, and ** for any depth).",
		Configurable: false,
		available:    func(ctx ToolBuildContext) bool { return ctx.Workspace != nil },
		build:        buildGlobTool,
	},
	sandbox.ToolBrowserUse: {
		Name:         sandbox.ToolBrowserUse,
		Title:        "Browser use",
		Description:  "Run browser automation commands inside the server-side sandbox with the agent workspace mounted for artifacts.",
		Configurable: false,
		available:    func(ctx ToolBuildContext) bool { return ctx.Sandbox != nil && ctx.Workspace != nil },
		build:        buildBrowserUseTool,
	},
}

// ToolCatalog returns the registered tools (name-sorted) for the UI to render a
// pick-list of what an agent can be granted. Tools whose host dependency is not
// wired for this deployment are hidden from the catalog, but still fail closed if
// a stale selection tries to build them.
func ToolCatalog(ctxs ...ToolBuildContext) []ToolCatalogEntry {
	ctx := ToolBuildContext{}
	if len(ctxs) > 0 {
		ctx = ctxs[0]
	}
	out := make([]ToolCatalogEntry, 0, len(toolRegistry))
	for _, spec := range toolRegistry {
		if spec.available != nil && !spec.available(ctx) {
			continue
		}
		out = append(out, ToolCatalogEntry{
			Name:         spec.Name,
			Title:        spec.Title,
			Description:  spec.Description,
			Configurable: spec.Configurable,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// IsRegisteredTool reports whether name is a known selectable tool. The control
// plane calls it to reject a selection for a tool that does not exist before it
// is ever stored.
func IsRegisteredTool(name string) bool {
	_, ok := toolRegistry[name]
	return ok
}

// ToolAvailable reports whether a registered tool's host dependencies are
// satisfied in this build context (e.g. run_shell needs a Sandbox). An
// unregistered name returns false. The run path uses this to skip a stale
// selection — a tool the operator once enabled but whose substrate this
// deployment no longer wires — instead of failing the whole run. The catalog
// already hides such tools from the picker, so a user often cannot toggle the
// selection off; dropping it at run time is the only graceful path, and it is
// not a security downgrade because the tool could not execute anyway.
func ToolAvailable(ctx ToolBuildContext, name string) bool {
	spec, ok := toolRegistry[name]
	if !ok {
		return false
	}
	return spec.available == nil || spec.available(ctx)
}

// BuildTool constructs a live config-only tool from a registered name and its
// config JSON. Tools that need host runtime dependencies should use
// BuildToolWithContext instead.
func BuildTool(name, configJSON string) (agentcore.Tool, error) {
	return BuildToolWithContext(ToolBuildContext{}, name, configJSON)
}

// BuildToolWithContext constructs a live tool from a registered name, the host
// runtime context, and its config JSON. An unknown name, missing dependency, or
// invalid config returns an error so a run fails closed rather than silently
// dropping a capability the operator selected.
func BuildToolWithContext(ctx ToolBuildContext, name, configJSON string) (agentcore.Tool, error) {
	spec, ok := toolRegistry[name]
	if !ok {
		return nil, fmt.Errorf("unknown tool %q", name)
	}
	return spec.build(ctx, configJSON)
}

// httpToolConfig is the per-agent config for http_request.
type httpToolConfig struct {
	AllowHosts []string `json:"allow_hosts"`
	AllowHTTP  bool     `json:"allow_http"`
}

// buildHTTPRequestTool constructs a per-agent http_request from its config. It
// mirrors app.buildHTTPTool's refusal of an empty allowlist: an outbound HTTP
// tool that can reach no host is useless and a standing SSRF risk, so it is
// rejected rather than built open.
func buildHTTPRequestTool(_ ToolBuildContext, configJSON string) (agentcore.Tool, error) {
	cfg := httpToolConfig{}
	if s := strings.TrimSpace(configJSON); s != "" {
		if err := json.Unmarshal([]byte(s), &cfg); err != nil {
			return nil, fmt.Errorf("invalid http_request config: %w", err)
		}
	}
	hosts := make([]string, 0, len(cfg.AllowHosts))
	for _, h := range cfg.AllowHosts {
		if t := strings.TrimSpace(h); t != "" {
			hosts = append(hosts, t)
		}
	}
	if len(hosts) == 0 {
		return nil, fmt.Errorf("http_request requires at least one allow_hosts entry")
	}
	return httptool.New(
		httptool.WithAllowHosts(hosts),
		httptool.WithAllowPlainHTTP(cfg.AllowHTTP),
	), nil
}

// buildRunShellTool constructs the sandbox-backed run_shell tool. The shell is
// selectable per-agent, but the backing Sandbox is host-injected; without it the
// build fails closed.
func buildRunShellTool(ctx ToolBuildContext, configJSON string) (agentcore.Tool, error) {
	if ctx.Sandbox == nil {
		return nil, fmt.Errorf("run_shell requires the sandbox to be enabled")
	}
	if err := rejectConfig(sandbox.ToolRunShell, configJSON); err != nil {
		return nil, err
	}
	// Pass the workspace (when enabled) so run_shell shares a filesystem with the
	// read_file/write_file tools; nil keeps the legacy ephemeral scratch dir.
	return sandbox.NewShellTool(ctx.Sandbox, agentcore.SandboxLimits{}, ctx.Workspace), nil
}

// buildComputerUseTool constructs the persistent computer_use shell. It needs
// both the host-injected sandbox (to run in) and the workspace (so produced
// files land on the host); without either it fails closed.
func buildComputerUseTool(ctx ToolBuildContext, configJSON string) (agentcore.Tool, error) {
	if ctx.Sandbox == nil {
		return nil, fmt.Errorf("computer_use requires the sandbox to be enabled")
	}
	if ctx.Workspace == nil {
		return nil, fmt.Errorf("computer_use requires the agent workspace to be enabled")
	}
	if err := rejectConfig(sandbox.ToolComputerUse, configJSON); err != nil {
		return nil, err
	}
	return sandbox.NewComputerUseTool(ctx.Sandbox, ctx.Workspace, ctx.NetworkAllow...), nil
}

func buildReadFileTool(ctx ToolBuildContext, configJSON string) (agentcore.Tool, error) {
	if ctx.Workspace == nil {
		return nil, fmt.Errorf("read_file requires the agent workspace to be enabled")
	}
	if err := rejectConfig(sandbox.ToolReadFile, configJSON); err != nil {
		return nil, err
	}
	return sandbox.NewReadFileTool(ctx.Workspace), nil
}

func buildEditFileTool(ctx ToolBuildContext, configJSON string) (agentcore.Tool, error) {
	if ctx.Workspace == nil {
		return nil, fmt.Errorf("edit_file requires the agent workspace to be enabled")
	}
	if err := rejectConfig(sandbox.ToolEditFile, configJSON); err != nil {
		return nil, err
	}
	return sandbox.NewEditFileTool(ctx.Workspace), nil
}

func buildGrepTool(ctx ToolBuildContext, configJSON string) (agentcore.Tool, error) {
	if ctx.Workspace == nil {
		return nil, fmt.Errorf("grep requires the agent workspace to be enabled")
	}
	if err := rejectConfig(sandbox.ToolGrep, configJSON); err != nil {
		return nil, err
	}
	return sandbox.NewGrepTool(ctx.Workspace), nil
}

func buildGlobTool(ctx ToolBuildContext, configJSON string) (agentcore.Tool, error) {
	if ctx.Workspace == nil {
		return nil, fmt.Errorf("glob requires the agent workspace to be enabled")
	}
	if err := rejectConfig(sandbox.ToolGlob, configJSON); err != nil {
		return nil, err
	}
	return sandbox.NewGlobTool(ctx.Workspace), nil
}

// buildWebFetchTool constructs the open web_fetch tool. It needs no host
// dependency and no per-agent config: SSRF is closed off at the IP layer inside
// the tool, so it is always buildable wherever policy grants it.
func buildWebFetchTool(_ ToolBuildContext, configJSON string) (agentcore.Tool, error) {
	if err := rejectConfig(httptool.ToolWebFetch, configJSON); err != nil {
		return nil, err
	}
	return httptool.NewWebFetch(), nil
}

func buildWriteFileTool(ctx ToolBuildContext, configJSON string) (agentcore.Tool, error) {
	if ctx.Workspace == nil {
		return nil, fmt.Errorf("write_file requires the agent workspace to be enabled")
	}
	if err := rejectConfig(sandbox.ToolWriteFile, configJSON); err != nil {
		return nil, err
	}
	return sandbox.NewWriteFileTool(ctx.Workspace), nil
}

func buildBrowserUseTool(ctx ToolBuildContext, configJSON string) (agentcore.Tool, error) {
	if ctx.Sandbox == nil {
		return nil, fmt.Errorf("browser_use requires the sandbox to be enabled")
	}
	if ctx.Workspace == nil {
		return nil, fmt.Errorf("browser_use requires the agent workspace to be enabled")
	}
	if err := rejectConfig(sandbox.ToolBrowserUse, configJSON); err != nil {
		return nil, err
	}
	return sandbox.NewBrowserTool(ctx.Sandbox, ctx.Workspace, sandbox.BrowserUseLimits(), ctx.BrowserImage), nil
}

func rejectConfig(toolName, configJSON string) error {
	if s := strings.TrimSpace(configJSON); s != "" && s != "{}" {
		var cfg map[string]any
		if err := json.Unmarshal([]byte(s), &cfg); err != nil {
			return fmt.Errorf("invalid %s config: %w", toolName, err)
		}
		if len(cfg) > 0 {
			return fmt.Errorf("%s does not accept config", toolName)
		}
	}
	return nil
}
