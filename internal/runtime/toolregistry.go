package agentruntime

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/lohi-ai/agentray/agentcore"
	"github.com/lohi-ai/agentray/internal/shared/mcpclient"
	"github.com/lohi-ai/agentray/sandbox"
)

// ToolMCP is the catalog entry that connects an agent to remote Model Context
// Protocol servers. It is the one selection that expands into many tools: the
// config lists servers, and each server contributes whatever it advertises.
const ToolMCP = "mcp"

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
	Sandbox agentcore.Sandbox
	// SandboxRequired withholds the shell/computer/browser tools when no Sandbox
	// is wired, instead of building them against the host workspace.
	//
	// The default (false) is what makes the sandbox RECOMMENDED rather than
	// REQUIRED. A hard requirement reads as safe and behaves as a wall: a new
	// user with no Docker gets an agent that can do nothing, and the honest
	// summary of that trade is that the isolation protected no one because
	// nobody got that far. Host mode is not unguarded either — sandbox.HostSandbox
	// still withholds the server's environment from the child process, confines
	// it to the run's workspace, and kills its process group on timeout.
	//
	// Set it in a hosted, multi-tenant deployment, where "someone forgot to wire
	// Docker" and "this operator chose host execution" are indistinguishable at
	// startup and the blast radius is other people's data.
	SandboxRequired bool
	// Workspace is THIS RUN's workspace, set by the runner. It is nil on the
	// catalog and config-validation paths, which have no run to belong to.
	Workspace *sandbox.Workspace
	// WorkspaceBase is what those run-less paths set instead: the root under
	// which runs get workspaces, which answers the only question the catalog is
	// actually asking — does this deployment give agents a workspace at all?
	//
	// The two exist separately because they answer different questions. A catalog
	// that required a concrete Workspace would hide read_file from every
	// deployment that has one, since the catalog is rendered outside any run.
	WorkspaceBase string
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
	// Credentials is the run-scoped secret vault, supplied for tools whose
	// *config* (not the model's arguments) carries a {{cred:NAME}} placeholder.
	// Only `mcp` needs it today: an MCP server's Authorization header is operator
	// configuration resolved here, on the host side at build time, so it never
	// passes through the tool loop where a model-visible argument would. nil is
	// normal — a config with no placeholder needs no vault, and one with a
	// placeholder fails closed.
	Credentials agentcore.CredentialResolver
}

// isolationSatisfied reports whether the tools that would normally run inside a
// container may be built for this deployment: either isolation is wired, or the
// operator has not demanded it and the host substrate stands in.
func (c ToolBuildContext) isolationSatisfied() bool {
	return c.Sandbox != nil || !c.SandboxRequired
}

// workspaceAvailable reports whether the agent gets a working directory —
// either this run's, or (on the catalog path) because the deployment configures
// one for every run.
func (c ToolBuildContext) workspaceAvailable() bool {
	return c.Workspace != nil || strings.TrimSpace(c.WorkspaceBase) != ""
}

// workspaceToolAvailable gates the tools that cannot work without a directory to
// work in. It is what makes a stale selection SKIP rather than abort: a
// deployment with no workspace at all drops read_file from the run instead of
// failing a run that was only ever going to fail at the first call.
func workspaceToolAvailable(c ToolBuildContext) bool { return c.workspaceAvailable() }

// sandboxToolAvailable gates the tools that execute code. They need somewhere to
// run (the workspace) and permission to run there (isolation, or an operator who
// did not demand it).
func sandboxToolAvailable(c ToolBuildContext) bool {
	return c.isolationSatisfied() && c.workspaceAvailable()
}

// ToolSpec describes one selectable tool: its stable name, human-facing catalog
// copy, whether it carries per-agent config, and how to build a live tool from
// that config.
type ToolSpec struct {
	Name         string
	Title        string
	Description  string
	Configurable bool
	// ExternalWrite marks a tool whose calls can publish outside the platform
	// (post to an API, send content to a third party). The runner strips such
	// tools from unattended (scheduled/webhook/delegate) runs unless the
	// agent's autonomy is 'auto' — the hard publish rail. Read-only fetchers
	// and workspace/sandbox tools are not marked.
	ExternalWrite bool
	available     func(ToolBuildContext) bool
	// build constructs the one tool this entry provides. Exactly one of build or
	// buildMany is set.
	build func(ToolBuildContext, string) (agentcore.Tool, error)
	// buildMany constructs the several tools one selection expands into: `mcp` is
	// a single catalog entry whose config names N remote servers, each
	// advertising its own tool list. It takes a context because expansion talks
	// to the network, and returns notes describing any capability it had to skip
	// so the caller can report a degraded — rather than silent — tool set.
	buildMany func(context.Context, ToolBuildContext, string) ([]agentcore.Tool, []string, error)
	// validate checks a stored config without building anything. The control
	// plane uses it at write time; a buildMany entry MUST set it, so accepting a
	// selection never depends on a remote server answering right now.
	validate func(ToolBuildContext, string) error
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
	sandbox.ToolHTTPRequest: {
		Name:          sandbox.ToolHTTPRequest,
		Title:         "Fetch / HTTP",
		Description:   "Make guarded outbound HTTP(S) requests to an allowlisted set of hosts. Use a {{cred:NAME}} placeholder for any secret; it is resolved at the trust boundary and never seen by the model. Large responses can be saved straight into the agent's workspace instead of being read inline.",
		Configurable:  true,
		ExternalWrite: true,
		build:         buildHTTPRequestTool,
	},
	ToolMCP: {
		Name:         ToolMCP,
		Title:        "MCP servers",
		Description:  "Connect the agent to remote Model Context Protocol servers you operate. Each server's tools are advertised to the model as mcp__<server>__<tool>. Configure servers with a name and an https URL; put any token in a {{cred:NAME}} placeholder, which is resolved on the server and never seen by the model.",
		Configurable: true,
		// A third-party server can do anything behind its own API, so every tool
		// it contributes is treated as capable of publishing outside the platform:
		// the unattended-publish rail strips them from background runs unless the
		// project's autonomy is 'auto'.
		ExternalWrite: true,
		validate:      validateMCPConfig,
		buildMany:     buildMCPTools,
	},
	sandbox.ToolWebFetch: {
		Name:         sandbox.ToolWebFetch,
		Title:        "Web fetch",
		Description:  "Fetch a public web page and return its readable text (HTML stripped to text), or save the raw document into the agent's workspace for the file and shell tools to process. No host allowlist, but loopback / private / link-local / cloud-metadata addresses are refused. Use http_request for authenticated calls to a specific API.",
		Configurable: false,
		build:        buildWebFetchTool,
	},
	sandbox.ToolRunShell: {
		Name:         sandbox.ToolRunShell,
		Title:        "Bash / shell",
		Description:  "Run shell commands in the agent's workspace. With a sandbox configured they run inside it, seeing no host filesystem, environment, or network unless granted. Without one they run on the server as this process, still confined to the workspace and still denied the server's environment — convenient for a single-operator install, not isolation.",
		Configurable: false,
		available:    sandboxToolAvailable,
		build:        buildRunShellTool,
	},
	sandbox.ToolComputerUse: {
		Name:         sandbox.ToolComputerUse,
		Title:        "Computer use",
		Description:  "A persistent, network-enabled Linux sandbox where the agent can install tools (pip/apt/npm), write and run code, and produce files — parse or generate PDF, DOCX, XLSX, PPTX, HTML. State persists across calls in the conversation. Higher-privilege than run_shell; grant it deliberately.",
		Configurable: false,
		available:    sandboxToolAvailable,
		build:        buildComputerUseTool,
	},
	sandbox.ToolReadFile: {
		Name:         sandbox.ToolReadFile,
		Title:        "Read file",
		Description:  "Read text files from the configured agent workspace. Paths must be relative and cannot escape the workspace root.",
		Configurable: false,
		available:    workspaceToolAvailable,
		build:        buildReadFileTool,
	},
	sandbox.ToolWriteFile: {
		Name:         sandbox.ToolWriteFile,
		Title:        "Write file",
		Description:  "Write text files inside the configured agent workspace. Paths must be relative and parent directories are created as needed.",
		Configurable: false,
		available:    workspaceToolAvailable,
		build:        buildWriteFileTool,
	},
	sandbox.ToolEditFile: {
		Name:         sandbox.ToolEditFile,
		Title:        "Edit file",
		Description:  "Make a surgical exact-string replacement in a workspace file instead of rewriting it. Refuses an ambiguous match unless replace_all is set.",
		Configurable: false,
		available:    workspaceToolAvailable,
		build:        buildEditFileTool,
	},
	sandbox.ToolGrep: {
		Name:         sandbox.ToolGrep,
		Title:        "Grep",
		Description:  "Search workspace file contents by regular expression (RE2). Returns path:line:text matches, scoped by an optional subdirectory and filename glob.",
		Configurable: false,
		available:    workspaceToolAvailable,
		build:        buildGrepTool,
	},
	sandbox.ToolGlob: {
		Name:         sandbox.ToolGlob,
		Title:        "Glob",
		Description:  "List workspace files whose relative path matches a glob pattern (supports *, ?, and ** for any depth).",
		Configurable: false,
		available:    workspaceToolAvailable,
		build:        buildGlobTool,
	},
	sandbox.ToolBrowserUse: {
		Name:         sandbox.ToolBrowserUse,
		Title:        "Browser use",
		Description:  "Run browser automation commands inside the server-side sandbox with the agent workspace mounted for artifacts.",
		Configurable: false,
		available:    sandboxToolAvailable,
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

// ToolExternalWrite reports whether a tool is external-write (it can publish
// outside the platform). Catalog tools answer from their spec. An unregistered
// name returns false — the rail only governs catalog tools; run-derived tools
// (analytics ops, team_board) are internal by construction — with one exception:
// a tool contributed by a remote MCP server carries the mcp__ prefix and no
// catalog entry of its own, and we cannot see what it does behind the server's
// API, so it is external-write by construction.
func ToolExternalWrite(name string) bool {
	if strings.HasPrefix(name, mcpclient.ToolPrefix) {
		return true
	}
	spec, ok := toolRegistry[name]
	return ok && spec.ExternalWrite
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
// dropping a capability the operator selected. A selection that expands to
// several tools (mcp) has no single tool to return — use BuildToolsWithContext.
func BuildToolWithContext(ctx ToolBuildContext, name, configJSON string) (agentcore.Tool, error) {
	spec, ok := toolRegistry[name]
	if !ok {
		return nil, fmt.Errorf("unknown tool %q", name)
	}
	if spec.build == nil {
		return nil, fmt.Errorf("tool %q expands to several tools; use BuildToolsWithContext", name)
	}
	return spec.build(ctx, configJSON)
}

// BuildToolsWithContext is the run path's builder: it returns every tool one
// selection provides — one for an ordinary entry, N for an expanding one — plus
// notes describing any capability that had to be skipped (an optional MCP server
// that would not answer). Notes are for reporting, not control flow: anything
// the operator must know about but that should not take the run down.
func BuildToolsWithContext(ctx context.Context, tctx ToolBuildContext, name, configJSON string) ([]agentcore.Tool, []string, error) {
	spec, ok := toolRegistry[name]
	if !ok {
		return nil, nil, fmt.Errorf("unknown tool %q", name)
	}
	if spec.buildMany != nil {
		return spec.buildMany(ctx, tctx, configJSON)
	}
	tool, err := spec.build(tctx, configJSON)
	if err != nil {
		return nil, nil, err
	}
	return []agentcore.Tool{tool}, nil, nil
}

// ValidateToolConfig checks a stored selection's config without building it or
// touching the network. The control plane calls it at write time so an unusable
// selection (an empty http_request allowlist, a malformed MCP server list) is
// rejected there rather than failing the next run closed — while a remote MCP
// server that happens to be down right now does not block saving a valid config.
func ValidateToolConfig(tctx ToolBuildContext, name, configJSON string) error {
	spec, ok := toolRegistry[name]
	if !ok {
		return fmt.Errorf("unknown tool %q", name)
	}
	if spec.validate != nil {
		return spec.validate(tctx, configJSON)
	}
	_, err := spec.build(tctx, configJSON)
	return err
}

// validateMCPConfig checks the server list is well formed and that each URL is
// one we would be willing to dial. It performs no I/O and resolves no secrets:
// a config is savable before the agent's vault is loaded and while a remote
// server is down. An unresolvable {{cred:NAME}} therefore surfaces at run time,
// where it fails the run closed.
func validateMCPConfig(_ ToolBuildContext, configJSON string) error {
	_, err := mcpclient.ParseConfig(configJSON)
	return err
}

// buildMCPTools connects to each configured server and adapts its advertised
// tools. A malformed config fails the run closed — the operator asked for a
// capability we cannot honor. An unreachable server is skipped with a note
// instead, because a third-party outage should not take down an agent's
// unrelated work; a server marked "required" opts back into failing closed.
func buildMCPTools(ctx context.Context, tctx ToolBuildContext, configJSON string) ([]agentcore.Tool, []string, error) {
	cfg, err := mcpclient.ParseConfig(configJSON)
	if err != nil {
		return nil, nil, err
	}
	var tools []agentcore.Tool
	var notes []string
	for _, spec := range cfg.Servers {
		client, err := spec.Connect(ctx, tctx.Credentials)
		if err != nil {
			// A bad URL or an unresolvable secret is configuration, not weather:
			// fail closed regardless of "required".
			return nil, nil, err
		}
		got, err := mcpclient.Tools(ctx, client)
		if err != nil {
			if spec.Required {
				return nil, nil, fmt.Errorf("required mcp server %q is unavailable: %w", spec.Name, err)
			}
			notes = append(notes, fmt.Sprintf("mcp server %q was unavailable and its tools were skipped: %v", spec.Name, err))
			continue
		}
		tools = append(tools, got...)
	}
	return tools, notes, nil
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
func buildHTTPRequestTool(ctx ToolBuildContext, configJSON string) (agentcore.Tool, error) {
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
	// Deliberately host-substrate (nil sandbox), even where one is wired.
	// sandbox.NewHTTPRequestTool can route the request through a container, but
	// that path needs curl in the image and the default sandbox image (alpine)
	// has none — enabling it here would turn a working capability into exit 127
	// on every deployment with AGENTRAY_SANDBOX_ENABLED set. The host path keeps
	// its own SSRF defenses (allowlist + guarded dialer), so this is a
	// deployment-shape choice, not a weaker one. Flip it to ctx.Sandbox once a
	// curl-capable image is the configured default.
	//
	// The workspace is passed even though the request itself runs on the host
	// substrate: it is what save_as writes into, so a fetched dataset lands in the
	// same directory read_file and run_shell work in.
	return sandbox.NewHTTPRequestTool(nil,
		sandbox.WithHTTPAllowHosts(hosts),
		sandbox.WithHTTPAllowPlain(cfg.AllowHTTP),
		sandbox.WithHTTPWorkspace(ctx.Workspace),
	), nil
}

// buildRunShellTool constructs the run_shell tool. The shell is selectable
// per-agent; the backing Sandbox is host-injected and OPTIONAL.
//
// sandbox.NewShellTool accepts a nil sandbox and runs on the host, and this
// control plane now takes that path by default. The earlier rule — no sandbox,
// no shell — was the safer-looking half of a trade whose other half was that a
// user without Docker got an agent that could not do anything, so the isolation
// protected nobody who never got that far. What replaces it is a choice the
// operator makes: host mode by default, SandboxRequired for the deployment where
// the blast radius is other people's data.
//
// Host mode is still guarded rather than raw — HostSandbox withholds the
// server's environment from the child, roots it in the run's workspace, and
// kills its process group on timeout — but it is NOT isolation, and a
// deployment that needs isolation must say so.
func buildRunShellTool(ctx ToolBuildContext, configJSON string) (agentcore.Tool, error) {
	if !ctx.isolationSatisfied() {
		return nil, fmt.Errorf("run_shell requires the sandbox to be enabled")
	}
	// The shell shares one directory with read_file/write_file/edit_file because
	// that sharing is the point: an agent writes a script with write_file and
	// runs it with run_shell. A shell in its own ephemeral scratch dir could not
	// see the file it had just been asked to execute.
	if ctx.Workspace == nil {
		return nil, fmt.Errorf("run_shell requires the agent workspace to be enabled")
	}
	if err := rejectConfig(sandbox.ToolRunShell, configJSON); err != nil {
		return nil, err
	}
	return sandbox.NewShellTool(ctx.Sandbox, agentcore.SandboxLimits{}, ctx.Workspace), nil
}

// buildComputerUseTool constructs the persistent computer_use shell. It needs
// both the host-injected sandbox (to run in) and the workspace (so produced
// files land on the host); without either it fails closed.
func buildComputerUseTool(ctx ToolBuildContext, configJSON string) (agentcore.Tool, error) {
	if !ctx.isolationSatisfied() {
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

// The file and search tools need the workspace — every path they take is
// workspace-relative — but the sandbox stays optional, exactly as their
// constructors describe it: with one wired the I/O happens inside the container
// against the bind-mounted workspace, without one it happens on the host under
// the Workspace guards. Either way the tool can only ever touch the workspace,
// which is why (unlike run_shell) there is nothing here to gate on the sandbox.
func buildReadFileTool(ctx ToolBuildContext, configJSON string) (agentcore.Tool, error) {
	if ctx.Workspace == nil {
		return nil, fmt.Errorf("read_file requires the agent workspace to be enabled")
	}
	if err := rejectConfig(sandbox.ToolReadFile, configJSON); err != nil {
		return nil, err
	}
	return sandbox.NewReadFileTool(ctx.Sandbox, ctx.Workspace), nil
}

func buildEditFileTool(ctx ToolBuildContext, configJSON string) (agentcore.Tool, error) {
	if ctx.Workspace == nil {
		return nil, fmt.Errorf("edit_file requires the agent workspace to be enabled")
	}
	if err := rejectConfig(sandbox.ToolEditFile, configJSON); err != nil {
		return nil, err
	}
	return sandbox.NewEditFileTool(ctx.Sandbox, ctx.Workspace), nil
}

func buildGrepTool(ctx ToolBuildContext, configJSON string) (agentcore.Tool, error) {
	if ctx.Workspace == nil {
		return nil, fmt.Errorf("grep requires the agent workspace to be enabled")
	}
	if err := rejectConfig(sandbox.ToolGrep, configJSON); err != nil {
		return nil, err
	}
	return sandbox.NewGrepTool(ctx.Sandbox, ctx.Workspace), nil
}

func buildGlobTool(ctx ToolBuildContext, configJSON string) (agentcore.Tool, error) {
	if ctx.Workspace == nil {
		return nil, fmt.Errorf("glob requires the agent workspace to be enabled")
	}
	if err := rejectConfig(sandbox.ToolGlob, configJSON); err != nil {
		return nil, err
	}
	return sandbox.NewGlobTool(ctx.Sandbox, ctx.Workspace), nil
}

// buildWebFetchTool constructs the open web_fetch tool. It needs no host
// dependency and no per-agent config: SSRF is closed off at the IP layer inside
// the tool, so it is always buildable wherever policy grants it. It is built on
// the host substrate for the same reason as http_request — the default sandbox
// image has no curl.
func buildWebFetchTool(ctx ToolBuildContext, configJSON string) (agentcore.Tool, error) {
	if err := rejectConfig(sandbox.ToolWebFetch, configJSON); err != nil {
		return nil, err
	}
	return sandbox.NewWebFetchTool(nil, ctx.Workspace), nil
}

func buildWriteFileTool(ctx ToolBuildContext, configJSON string) (agentcore.Tool, error) {
	if ctx.Workspace == nil {
		return nil, fmt.Errorf("write_file requires the agent workspace to be enabled")
	}
	if err := rejectConfig(sandbox.ToolWriteFile, configJSON); err != nil {
		return nil, err
	}
	return sandbox.NewWriteFileTool(ctx.Sandbox, ctx.Workspace), nil
}

func buildBrowserUseTool(ctx ToolBuildContext, configJSON string) (agentcore.Tool, error) {
	if !ctx.isolationSatisfied() {
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
