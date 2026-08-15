package agentruntime

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"

	"github.com/lohi-ai/agentray/agentcore"
	"github.com/lohi-ai/agentray/agentcore/plugins/observe"
	"github.com/lohi-ai/agentray/agentcore/plugins/spill"
	"github.com/lohi-ai/agentray/agentcore/plugins/subagent"
	"github.com/lohi-ai/agentray/agentcore/plugins/todo"
	"github.com/lohi-ai/agentray/internal/dataplane/store"
	"github.com/lohi-ai/agentray/internal/dataplane/usecase"
	"github.com/lohi-ai/agentray/internal/shared/credential"
	"github.com/lohi-ai/agentray/sandbox"
)

// defaultRunMaxTokens caps a run's per-turn model output when the caller doesn't
// set one. Generous enough that large artifacts (long documents, full HTML pages)
// complete instead of truncating at the gateway's low default with
// stop_reason:"length".
const defaultRunMaxTokens = 16000

// Runner builds, executes, and persists a Growth Analyst run end-to-end: it
// resolves the project's config/key/definition/skills, opens an agent_run,
// drives the agentcore loop, persists the tool-call trace + token usage, and
// optionally fires the reflect pass (§14.9). Both the chat handler and the NATS
// scheduler go through this one path.
type Runner struct {
	Store *storage.Store
	// Sandbox, when non-nil, is threaded into every BuildParams so agents get
	// selectable risky tools + injection guard running inside an isolated container.
	// nil (the default) keeps agents analytics-only.
	Sandbox agentcore.Sandbox
	// Workspace, when non-nil, is the host-approved root for file/browser tools.
	// nil hides those tools and fails stale selections closed.
	Workspace *sandbox.Workspace
	// BrowserImage is the Chrome-capable sandbox image the browser_use tool runs
	// its persistent session in. Empty leaves browser_use on the backend default
	// image (which generally lacks a browser).
	BrowserImage string
	// NetworkAllow confines the computer_use sandbox's egress to the listed hosts
	// (and their subdomains) via the sandbox filtering proxy (#5b). Empty keeps the
	// open-network default.
	NetworkAllow []string
	// Credentials, when non-nil, is the secret vault threaded into every
	// BuildParams so an agent can resolve {{cred:NAME}} placeholders without the
	// model seeing the literal (governance F7). nil (the default) leaves tool
	// arguments untouched.
	Credentials agentcore.CredentialResolver
	// HTTPTool, when non-nil, is the outbound http_request capability threaded
	// into every BuildParams (the worked consumer of the vault). nil (the
	// default) leaves agents with no outbound HTTP surface.
	HTTPTool agentcore.Tool
	// Tracer, when non-nil, is the per-LLM-call trace sink threaded into every
	// run (and the cheap classifier). It captures the request messages, response,
	// tokens, and computed cost per call. nil (the default) still prices each call
	// — only the trace emission is skipped.
	Tracer observe.Sink
	// SessionStore, when non-nil, makes every run durable: the loop appends an
	// append-only entry log (keyed by run id) so a crashed or compacted run can be
	// reduced and resumed (ResumeRun / the resume endpoint). nil (the default)
	// keeps runs in-memory only.
	SessionStore agentcore.SessionStore
	// SpillStore, when non-nil, keeps an oversized tool result out of the model's
	// context WITHOUT destroying it: the full text is persisted and the model gets
	// a preview plus a locator it can page through with read_spill, instead of a
	// head+tail truncation whose omitted middle is gone for good. It must be
	// durable — the locator is written to the session log, so an in-memory store
	// would make a resumed run read its own spill back as not-found. nil (the
	// default) leaves the loop's truncation in place.
	SpillStore spill.SpillStore
	// Live, when non-nil, is the process-wide registry of in-flight runs that backs
	// mid-run steering and follow-up. A run with a conversation SessionID registers
	// here so a sibling request can inject a message into it. nil (the default)
	// disables live control — runs neither register nor drain.
	Live *LiveRegistry
	// KeyRefresh, when true, re-resolves each rung's BYO API key from the store
	// before every turn (agentcore's RefreshKey), so a key rotated mid-run is
	// picked up without killing a long run. false (the default) keeps the key fixed
	// for the whole run.
	KeyRefresh bool
	// MaxContextTokens overrides the loop's soft compaction budget for every run
	// this Runner drives. 0 (the default) keeps agentcore's 200k default. Mainly a
	// deployment/test knob to tune or exercise compaction.
	MaxContextTokens int
	// KeepRecentTokens overrides how much recent context compaction keeps verbatim
	// for every run this Runner drives. 0 (the default) keeps agentcore's 20k. Must
	// be below MaxContextTokens for the LLM summary path to engage.
	KeepRecentTokens int
	// Notifier, when non-nil, backs the send_notification agent tool: it delivers a
	// message to a saved alert channel through the platform's SSRF-guarded,
	// secret-resolving fan-out. nil leaves the tool wired but returning a clear
	// "not configured" error, so an agent granted the scope degrades cleanly.
	Notifier usecase.Notifier
}

// RunnerOption configures a Runner at construction. Threaded unchanged through
// the scheduler and orchestrator so a single sandbox built in app.New reaches
// both the scheduled and HTTP-chat run paths.
type RunnerOption func(*Runner)

// WithSandbox wires an isolation substrate into every run this Runner drives.
// A nil sandbox is a no-op, preserving the default analytics-only behavior.
func WithSandbox(sb agentcore.Sandbox) RunnerOption {
	return func(r *Runner) {
		if sb != nil {
			r.Sandbox = sb
		}
	}
}

// WithWorkspace wires the host-approved workspace root for file/browser tools.
// A nil workspace is a no-op, preserving the default hidden/fail-closed behavior.
func WithWorkspace(ws *sandbox.Workspace) RunnerOption {
	return func(r *Runner) {
		if ws != nil {
			r.Workspace = ws
		}
	}
}

// WithBrowserImage sets the Chrome-capable sandbox image the browser_use tool
// runs in for every run this Runner drives. An empty image is a no-op, leaving
// browser_use on the backend default image.
func WithBrowserImage(image string) RunnerOption {
	return func(r *Runner) {
		if image != "" {
			r.BrowserImage = image
		}
	}
}

// WithNetworkAllow confines the computer_use sandbox's egress to the given hosts
// (and their subdomains) for every run this Runner drives (#5b). An empty list is
// a no-op, leaving the sandbox on the open-network default.
func WithNetworkAllow(hosts []string) RunnerOption {
	return func(r *Runner) {
		if len(hosts) > 0 {
			r.NetworkAllow = hosts
		}
	}
}

// WithCredentials wires a secret vault into every run this Runner drives, so
// agents can resolve {{cred:NAME}} placeholders (governance F7). A nil resolver
// is a no-op, preserving the default behavior where arguments pass through
// unchanged.
func WithCredentials(c agentcore.CredentialResolver) RunnerOption {
	return func(r *Runner) {
		if c != nil {
			r.Credentials = c
		}
	}
}

// WithHTTPTool wires the outbound http_request tool into every run this Runner
// drives (the worked consumer of the vault). A nil tool is a no-op, preserving
// the default where agents have no outbound HTTP surface.
func WithHTTPTool(tool agentcore.Tool) RunnerOption {
	return func(r *Runner) {
		if tool != nil {
			r.HTTPTool = tool
		}
	}
}

// WithTraceSink wires a per-LLM-call trace sink into every run this Runner
// drives. A nil sink is a no-op, preserving the default where calls are priced
// but not traced.
func WithTraceSink(sink observe.Sink) RunnerOption {
	return func(r *Runner) {
		if sink != nil {
			r.Tracer = sink
		}
	}
}

// WithSessionStore wires a durable append-only session store into every run this
// Runner drives, so runs can be resumed after a crash. A nil store is a no-op,
// preserving the default in-memory behavior.
func WithSessionStore(s agentcore.SessionStore) RunnerOption {
	return func(r *Runner) {
		if s != nil {
			r.SessionStore = s
		}
	}
}

// WithSpillStore wires durable oversized-output storage into every run this
// Runner drives. A nil store is a no-op, leaving the loop's head+tail truncation
// in place.
func WithSpillStore(s spill.SpillStore) RunnerOption {
	return func(r *Runner) {
		if s != nil {
			r.SpillStore = s
		}
	}
}

// WithLiveRegistry wires the shared in-flight-run registry that backs mid-run
// steering and follow-up. A nil registry is a no-op, leaving runs without live
// control. The same registry must be handed to the steer/follow-up HTTP handlers.
func WithLiveRegistry(reg *LiveRegistry) RunnerOption {
	return func(r *Runner) {
		if reg != nil {
			r.Live = reg
		}
	}
}

// WithMaxContextTokens overrides the loop's soft compaction budget for every run
// this Runner drives. A non-positive value is a no-op, keeping agentcore's 200k
// default.
func WithMaxContextTokens(n int) RunnerOption {
	return func(r *Runner) {
		if n > 0 {
			r.MaxContextTokens = n
		}
	}
}

// WithKeepRecentTokens overrides how much recent context compaction keeps
// verbatim for every run this Runner drives. A non-positive value is a no-op,
// keeping agentcore's 20k default.
func WithKeepRecentTokens(n int) RunnerOption {
	return func(r *Runner) {
		if n > 0 {
			r.KeepRecentTokens = n
		}
	}
}

// WithNotifier sets the delivery backend for the send_notification agent tool.
// nil leaves the tool returning a "not configured" error.
func WithNotifier(n usecase.Notifier) RunnerOption {
	return func(r *Runner) {
		r.Notifier = n
	}
}

// WithKeyRefresh turns on per-turn API-key re-resolution (rotation-safe long
// runs). Off by default so the common run reads the key once.
func WithKeyRefresh() RunnerOption {
	return func(r *Runner) { r.KeyRefresh = true }
}

// NewRunner builds a Runner over the storage layer.
func NewRunner(store *storage.Store, opts ...RunnerOption) *Runner {
	r := &Runner{Store: store}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// RunOptions parameterize a single run.
type RunOptions struct {
	ProjectID string
	// AgentID selects which of the project's agents runs (AgentGarden §3). Empty
	// targets the project's default agent (id == project_id), so the original
	// single-agent path is preserved. The runner resolves it to a scope id that
	// keys the agent's persona/skills/secrets/tools/memory; ProjectID still keys
	// the analytics tools, which read project-wide data through the usecase layer.
	AgentID string
	Trigger string // chat | scheduled | manual
	Prompt  string
	Reflect bool // run the self-improvement pass at the end (§14.9)
	// History is the prior conversation turns (oldest first), threaded into a chat
	// run so the analyst answers with multi-turn context. Empty = a fresh run.
	// Cross-session persistence is out of scope; the caller (client) holds these.
	History []agentcore.Message
	// StepGate is the optional pause-before-each-turn hook for the Lab's explain
	// mode. Threaded straight into BuildParams; nil keeps the run continuous.
	StepGate func(ctx context.Context, turn int) error
	// OnRunID, when set, is invoked with the run id the moment the run row is
	// created — before the loop starts. The Lab uses it to register the explain
	// session against the run id so a separate advance/stop request can find it.
	OnRunID func(runID string)
	// SessionID is the client-held conversation id. When set and the Runner has a
	// LiveRegistry, the run registers under it so a sibling request can steer or
	// follow-up the in-flight run. Empty disables live control for the run. It is
	// distinct from the durable session id (the run id), which keys the resume log.
	SessionID string
	// GetSteering / GetFollowUp let a caller inject the agentcore steering and
	// follow-up sources directly (the Lab's explain mode uses this for a run that
	// has no conversation SessionID). When set they take precedence over the
	// LiveRegistry-derived sources; nil falls back to the registry (or to no
	// live control when neither is present).
	GetSteering func(ctx context.Context) []agentcore.Message
	GetFollowUp func(ctx context.Context) []agentcore.Message
	// PrepareNextTurn is the per-turn save-point seam (agentcore P7): after each
	// turn the returned TurnState (model / tools / system) drives the next one. nil
	// (the default) keeps the run static.
	PrepareNextTurn func(ctx context.Context, state agentcore.TurnState) agentcore.TurnState
	// ResumeSessionID, when set, makes this run CONTINUE the durable session of a
	// previous run instead of opening a fresh log: the session is the durable
	// object, runs are attempts on it. A new run row is still created (billing,
	// trace), but the loop keys its log on this id and recovers from it —
	// history rebuilt from the log, the crashed run's disabled tools re-applied,
	// dangling retry-safe calls replayed with their original call IDs (so
	// idempotency keys and sub-agent child sessions reattach), the rest closed
	// with interrupted notes. Empty — the default — keys the log on the new run id.
	ResumeSessionID string
	// MaxTokens caps the model's output tokens per turn. 0 — the default — uses
	// the provider's own cap. Set a generous value for runs that emit large
	// artifacts so output isn't truncated with stop_reason:"length".
	MaxTokens int
	// Goal, when non-empty, is this run's completion condition (Claude Code
	// /goal analog). The model is told to end with STATUS: DONE once the goal
	// holds (or STATUS: BLOCKED with the reason), and a finish without either
	// sentinel re-opens the run. Still bounded by turn/tool/budget limits.
	// Empty — the default — leaves the run ungated.
	Goal string
}

// Run executes one agent run and returns the persisted run row plus the loop
// result. The run row is always finished (done|error) before returning so the
// trace is visible in the UI even on failure.
func (r *Runner) Run(ctx context.Context, opts RunOptions) (storage.AgentRun, agentcore.RunResult, error) {
	return r.execute(ctx, opts, nil)
}

// RunStream is Run with live streaming: assistant tokens and tool-call traces
// are forwarded to sink as they are produced (for the SSE chat endpoint). The
// persisted run row and returned result are identical to Run's.
func (r *Runner) RunStream(ctx context.Context, opts RunOptions, sink agentcore.StreamSink) (storage.AgentRun, agentcore.RunResult, error) {
	return r.execute(ctx, opts, sink)
}

// execute is the shared run path; a non-nil sink streams the interactive turn.
func (r *Runner) execute(ctx context.Context, opts RunOptions, sink agentcore.StreamSink) (storage.AgentRun, agentcore.RunResult, error) {
	// Live control, registered FIRST — before any I/O, and in particular before the
	// run row opens and OnRunID fires. Stop is offered by the UI the moment a turn
	// is sent, so registering late would leave a window where the user has pressed
	// Stop, the UI says "Stopped", and the run is still burning tokens because
	// nothing was there to cancel. runCtx bounds only the model loop: the terminal
	// FinishAgentRun below stays on the parent ctx, so a stopped run still records
	// its status and partial answer rather than being resurrected as "still
	// working" by the client's resume poll.
	runCtx, cancelRun := context.WithCancelCause(ctx)
	defer cancelRun(nil)
	live := r.Live.register(opts.SessionID, opts.ProjectID, cancelRun)
	defer r.Live.unregister(opts.SessionID)
	// A caller-supplied source (the Lab) takes precedence; both are nil for a plain
	// run, leaving the loop's defaults.
	getSteering := opts.GetSteering
	if getSteering == nil {
		getSteering = live.steeringSource()
	}
	getFollowUp := opts.GetFollowUp
	if getFollowUp == nil {
		getFollowUp = live.followUpSource()
	}

	// Resolve which agent runs (AgentGarden §3). The scope id keys this agent's
	// persona/skills/secrets/tools/memory; ProjectID still keys the shared scopes
	// and the analytics tools. A stale/foreign agent id is refused here, so it can
	// never reach the run path. An empty AgentID resolves to the project's default
	// agent (scope id == project_id), preserving the original single-agent path.
	scopeID, err := r.Store.AgentScopeForRun(ctx, opts.ProjectID, opts.AgentID)
	if err != nil {
		return storage.AgentRun{}, agentcore.RunResult{}, err
	}

	cfg, err := r.Store.AgentConfigForRunAgent(ctx, opts.ProjectID, scopeID)
	if err != nil {
		return storage.AgentRun{}, agentcore.RunResult{}, err
	}
	if !cfg.Enabled {
		return storage.AgentRun{}, agentcore.RunResult{}, fmt.Errorf("agent is disabled for this project")
	}

	// Resolve the model pool from the workspace (the 3 tiers are configured once
	// per workspace and shared by every project/agent), plus this agent's task→tier
	// map (which tier each kind of work draws from). The project's agent_configs
	// still owns the run-eligibility fields (enable gate, scopes, PII).
	wsID, err := r.Store.WorkspaceIDForProject(ctx, opts.ProjectID)
	if err != nil {
		return storage.AgentRun{}, agentcore.RunResult{}, err
	}
	wsTiers, tierKeys, err := r.Store.WorkspaceTiersForRun(ctx, wsID)
	if err != nil {
		return storage.AgentRun{}, agentcore.RunResult{}, err
	}
	tierSet := tierSetFromWorkspace(wsTiers, tierKeys)
	// flash is the always-present default every unconfigured tier resolves to, so
	// its key is mandatory; lite/pro keys are optional.
	if tierKeys["flash"] == "" {
		return storage.AgentRun{}, agentcore.RunResult{}, fmt.Errorf("no workspace model key configured")
	}
	taskMap, err := r.Store.TaskTiersForRun(ctx, scopeID)
	if err != nil {
		return storage.AgentRun{}, agentcore.RunResult{}, err
	}

	def, err := r.Store.AgentDefinitionForRun(ctx, scopeID)
	if err != nil {
		return storage.AgentRun{}, agentcore.RunResult{}, err
	}
	skills, err := r.loadSkills(ctx, scopeID)
	if err != nil {
		return storage.AgentRun{}, agentcore.RunResult{}, err
	}

	// Per-agent secrets (AgentGarden §5): decrypt this agent's stored secrets
	// into a run-scoped credential.Vault that backs {{cred:NAME}} resolution. An
	// agent with no secrets falls back to the host-global resolver (r.Credentials);
	// a malformed secret fails the run closed rather than running with a gap.
	secrets, err := r.Store.AgentSecretsForRun(ctx, scopeID)
	if err != nil {
		return storage.AgentRun{}, agentcore.RunResult{}, err
	}
	creds, err := runCredentials(r.Credentials, secrets)
	if err != nil {
		return storage.AgentRun{}, agentcore.RunResult{}, err
	}

	// Per-agent selectable tools (AgentGarden §6): resolve this agent's tool
	// selections into live tools, folding in the host-global http_request default
	// for any tool the agent hasn't decided on. A malformed selection fails the
	// run closed rather than silently dropping a granted capability.
	toolSelections, err := r.Store.AgentToolsForRun(ctx, scopeID)
	if err != nil {
		return storage.AgentRun{}, agentcore.RunResult{}, err
	}
	runTools, toolNotes, err := resolveRunTools(ctx, ToolBuildContext{
		Sandbox:      r.Sandbox,
		Workspace:    r.Workspace,
		BrowserImage: r.BrowserImage,
		NetworkAllow: r.NetworkAllow,
		// The vault backs {{cred:NAME}} in a tool's *config* (an MCP server's
		// Authorization header), resolved here rather than in the tool loop.
		Credentials: creds,
	}, r.HTTPTool, toolSelections)
	if err != nil {
		return storage.AgentRun{}, agentcore.RunResult{}, err
	}
	// A capability the operator granted but that could not be built (an optional
	// MCP server that would not answer) is a degraded run, not a silent one.
	for _, note := range toolNotes {
		log.Printf("agentruntime: scope=%s %s", scopeID, note)
	}

	// Cross-agent delegation roster: the other agents this one has been granted
	// (via the FE Teammates setting) to hand tasks to. Each closure re-enters
	// this same execute path under the target's own AgentID, so the delegate
	// runs with its OWN persona/tools/policy/secrets and gets its own run row —
	// nothing of the caller's capabilities leaks across. Only resolved for a
	// top-level run: past the delegation depth cap the tool isn't advertised,
	// so a delegate's own run skips the roster query.
	var delegates []subagent.Delegate
	if agentcore.DelegationDepth(ctx) == 0 {
		targets, err := r.Store.AgentDelegatesForRun(ctx, scopeID)
		if err != nil {
			return storage.AgentRun{}, agentcore.RunResult{}, err
		}
		for _, target := range targets {
			delegates = append(delegates, subagent.Delegate{
				Name:        target.Name,
				Description: target.Description,
				Run:         r.delegateRunner(opts.ProjectID, target.AgentID),
			})
		}

		// Team leadership (ARCHITECT-AGENT-TEAM P2/P3): when this agent is the
		// selected lead of one or more teams, merge the member rosters into the
		// delegate list (members run through the same guarded delegate path),
		// inject the orchestrator skill, and grant the board tool — all derived
		// per run, so demoting the lead revokes everything on its next run.
		// Non-lead members resolve zero teams here and are untouched.
		teams, err := r.Store.TeamsLedByAgentForRun(ctx, scopeID)
		if err != nil {
			return storage.AgentRun{}, agentcore.RunResult{}, err
		}
		if len(teams) > 0 {
			// A member whose name collides with an explicit grant for a different
			// agent is advertised by its slug everywhere (delegate list, skill
			// roster, board) so spawn_subagent always reaches the shown agent.
			teams = applyDelegateNameCollisions(teams, targets)
			delegates = mergeTeamDelegates(delegates, teams, func(agentID string) func(context.Context, string, agentcore.StreamSink) (string, agentcore.Usage, error) {
				return r.delegateRunner(opts.ProjectID, agentID)
			})
			skills = append(skills, orchestratorSkill(teams))
			runTools = append(runTools, &teamBoardTool{store: r.Store, teams: teams})
		}
	}

	trigger := opts.Trigger
	if trigger == "" {
		trigger = "manual"
	}

	// Hard unattended-publish rail: a background run keeps external-write tools
	// (http_request) only when autonomy is 'auto'. Autonomy is project-level —
	// agent_configs is keyed by project_id, so every agent in the project shares
	// one rung (only scopes/tools are per-agent). Applied after the team-lead
	// merge so it governs the final selectable tool set; evaluated here — inside
	// execute — so a delegated child, which re-enters execute for its own run,
	// passes through the rail again on its own trigger.
	runTools = applyAutonomyRail(runTools, trigger, cfg.Autonomy)

	// Budget resolution (#4). Resolve the agent's effective ceiling + already-spent
	// baseline for the current day once, so the per-turn gate is a cheap in-memory
	// comparison rather than a query each turn. Two enforcement points:
	//   1. Admission: a background run (scheduled/webhook/delegate) that is already
	//      at cap is rejected up front — it records a run row marked budget-stopped
	//      so it shows in history, then returns an error.
	//   2. Mid-run: an interactive run gets a graceful stop when its own turns push
	//      spend over the ceiling (budgetGate below, honored by the loop).
	// Failure to resolve a budget is non-fatal: a metering error must not block runs.
	var budgetGate func(context.Context, agentcore.Usage) bool
	if status, berr := r.Store.BudgetStatusForRun(ctx, scopeID, wsID, "day"); berr == nil && status.HasBudget {
		if status.Exceeded && isBackgroundTrigger(trigger) {
			runID, cerr := r.Store.CreateAgentRun(ctx, opts.ProjectID, scopeID, trigger, opts.SessionID)
			if cerr == nil {
				_ = r.Store.FinishAgentRun(ctx, runID, "error", "budget exhausted: "+status.Reason, 0, 0, 0)
			}
			return storage.AgentRun{}, agentcore.RunResult{}, fmt.Errorf("agent budget exhausted: %s", status.Reason)
		}
		baseCost := status.Spend.CostUSD
		baseTokens := status.Spend.Tokens
		b := status.Budget
		budgetGate = func(_ context.Context, u agentcore.Usage) bool {
			if b.MaxCostUSD > 0 && baseCost+u.CostUSD >= b.MaxCostUSD {
				return true
			}
			if b.MaxTokens > 0 && baseTokens+int64(u.InputTokens+u.OutputTokens) >= b.MaxTokens {
				return true
			}
			return false
		}
	}

	// A resume attempt records the durable session it continues on its run row
	// (the session_id column) so a later resume of THIS run can follow the chain
	// back to the log; a chat run records its conversation id there as before.
	rowSession := opts.SessionID
	if rowSession == "" && opts.ResumeSessionID != "" {
		rowSession = opts.ResumeSessionID
	}
	runID, err := r.Store.CreateAgentRun(ctx, opts.ProjectID, scopeID, trigger, rowSession)
	if err != nil {
		return storage.AgentRun{}, agentcore.RunResult{}, err
	}
	// Hand the run id to the caller before the loop runs, so the Lab's explain
	// session can be keyed on it ahead of the first StepGate pause.
	if opts.OnRunID != nil {
		opts.OnRunID(runID)
	}

	// Correlate every per-LLM-call trace this run emits with the run id, so the
	// TracingProvider's records (file + DB sinks) attribute back to this run —
	// and through agent_runs.agent_id, to this agent. agentcore treats the id as
	// opaque; the run→agent mapping stays here in the consumer.
	ctx = observe.WithTraceID(ctx, runID)

	// The durable log this run appends to: its own id, or — on a resume — the
	// original run's session, so the log continues as one durable object across
	// attempts (child-session ids and idempotency keys stay stable).
	durableSession := runID
	if opts.ResumeSessionID != "" {
		durableSession = opts.ResumeSessionID
	}

	// Key the persistent computer_use session to the conversation (so installed
	// tooling and produced files survive across turns) and fall back to the run id
	// for a one-off run. A session-capable sandbox reuses one container under this
	// id; an ephemeral backend ignores it. The container is reaped at run end below.
	sandboxSession := opts.SessionID
	if strings.TrimSpace(sandboxSession) == "" {
		sandboxSession = runID
	}
	ctx = agentcore.WithSandboxSession(ctx, sandboxSession)
	if ss, ok := r.Sandbox.(agentcore.SessionSandbox); ok && opts.SessionID == "" {
		// No durable conversation to keep alive past this run: reap on completion.
		defer func() { _ = ss.CloseSession(sandboxSession) }()
	}

	// Resolve the model ladder: the agent's "run" task tier is the start, plus the
	// higher tiers as escalation rungs when model_fallback is on. The primary rung
	// drives the run; the rest are tried on a retryable provider error.
	start := TierFromName(taskMap[storage.TaskRun])
	ladder := tierSet.ladder(start, wsTiers.ModelFallback)
	primary := ladder[0]
	esc, err := buildRungs(ladder[1:])
	if err != nil {
		_ = r.Store.FinishAgentRun(ctx, runID, "error", err.Error(), 0, 0, 0)
		return storage.AgentRun{}, agentcore.RunResult{}, err
	}

	// Compaction runs on its own tier (the agent's "compaction" task), pinned into
	// the loop so the in-run summary call doesn't borrow whichever rung the run has
	// escalated to. Build a dedicated provider for it.
	compactTC := tierSet.resolve(TierFromName(taskMap[storage.TaskCompaction]))
	compactProvider, err := buildProvider(compactTC.Provider, compactTC.BaseURL, compactTC.APIKey)
	if err != nil {
		_ = r.Store.FinishAgentRun(ctx, runID, "error", err.Error(), 0, 0, 0)
		return storage.AgentRun{}, agentcore.RunResult{}, err
	}

	mem := NewPgMemory(r.Store, cfg.RedactPII)
	if emb := newEmbedder(primary.Provider, primary.BaseURL, primary.APIKey); emb != nil {
		mem.Embedder = emb
	}

	// Default the output cap so large artifacts (long documents, full HTML pages)
	// aren't truncated at the gateway's low default with stop_reason:"length".
	// A caller can still override per run via RunOptions.MaxTokens.
	maxTokens := opts.MaxTokens
	if maxTokens <= 0 {
		maxTokens = defaultRunMaxTokens
	}

	agent, err := Build(BuildParams{
		ProjectID:          opts.ProjectID,
		ScopeID:            scopeID,
		Provider:           primary.Provider,
		Model:              primary.Model,
		BaseURL:            primary.BaseURL,
		APIKey:             primary.APIKey,
		Trigger:            trigger,
		Escalation:         esc,
		ContextWindow:      EffectiveContextWindow(primary),
		CompactionProvider: compactProvider,
		CompactionModel:    compactTC.Model,
		Scopes:             ScopesFromMap(cfg.Scopes),
		// Verify-on-stop rail: a figure-shaped answer produced with zero evidence
		// tool executions re-opens the run once (verify or disclaim). nil when the
		// agent's scopes grant no read tools, so persona-only agents are untouched.
		FinishGuard: evidenceFinishGuard(ScopesFromMap(cfg.Scopes)),
		Goal:        opts.Goal,
		Soul:        def.SoulMD,
		Agents:      def.AgentsMD,
		Skills:      skills,
		SkillLoader: r.skillLoader(scopeID),
		Data:        r.Store,
		Memory:      mem,
		Notifier:    r.Notifier,
		RunID:       runID,
		Sandbox:     r.Sandbox,
		Credentials: creds,
		Tools:       runTools,
		Tracer:      r.Tracer,
		StepGate:    opts.StepGate,
		// Durable resume: key the append-only log on the run id (the FK that the
		// resume endpoint and the trace both use) — unless this run continues an
		// earlier run's session, in which case the ORIGINAL log keeps growing
		// under its own id and the loop recovers from it (retry-safe replay,
		// breaker re-applied, sub-agent reattach). nil store leaves runs in-memory.
		Session:       r.SessionStore,
		SessionID:     durableSession,
		ResumeSession: opts.ResumeSessionID != "",
		MaxTokens:     maxTokens,
		// Prompt caching: a stable per-agent key so the persona/skills system prefix
		// is reused across this agent's turns and runs. Empty store keys leave the
		// feature off for providers/compat servers that don't support it.
		PromptCacheKey:       scopeID,
		PromptCacheRetention: "short",
		// Live + per-turn seams.
		GetSteering:     getSteering,
		GetFollowUp:     getFollowUp,
		RefreshKey:      r.keyRefresher(opts.ProjectID),
		PrepareNextTurn: opts.PrepareNextTurn,
		BudgetGate:      budgetGate,
		// Built-in run todo list (goal stability across compaction) + optional
		// compaction-budget override. A fresh store per run scopes the plan to it.
		Todo:             todo.NewStore(),
		MaxContextTokens: r.MaxContextTokens,
		KeepRecentTokens: r.KeepRecentTokens,
		// Delegation: every solo agent may spawn ephemeral sub-agents (P1 of the
		// team architecture) under the default caps — depth 1, 8 per run, 48 KB
		// model-visible answer. Children inherit this agent's exact capabilities.
		// Delegates extend the same tool with granted teammate agents, each
		// running under its own identity.
		Subagents: &subagent.Plugin{},
		Delegates: delegates,
		// Oversized tool output is persisted rather than truncated away, so the
		// omitted middle of a large export or log is one read_spill call from the
		// model instead of a re-run of the tool. Fenced to the durable session, so
		// the locator stays valid across a resume and invalid everywhere else.
		Spill: r.SpillStore,
		// The "model-visible means logged" detector. Advisory: it reports and never
		// alters the run, which is exactly why it is safe to leave on — a resumed
		// run replaying a different conversation than the one that ran is a silent
		// failure otherwise, surfacing weeks later as a model that forgot a
		// correction it was given.
		ReportLogInvariant: func(v observe.LogInvariantViolation) {
			log.Printf("agentray: run %s log invariant: %v", runID, v)
		},
	})
	if err != nil {
		_ = r.Store.FinishAgentRun(ctx, runID, "error", err.Error(), 0, 0, 0)
		return storage.AgentRun{}, agentcore.RunResult{}, err
	}

	// Thread prior turns into a multi-turn run when the caller supplied history;
	// the current prompt is the task that drives skill selection + memory recall.
	// On a resume these seeds are a fallback only — the loop rebuilds history
	// from the continued session's log (ResumeRun verified it is non-empty).
	messages := append(append([]agentcore.Message{}, opts.History...), agentcore.Message{Role: agentcore.RoleUser, Content: opts.Prompt})

	// The model loop runs on runCtx (cancellable by Stop); everything after it —
	// the trace and the terminal row — runs on ctx, so a stopped run still writes
	// what it got instead of leaving a `running` row for the poller to find.
	var res agentcore.RunResult
	var runErr error
	switch {
	case sink != nil:
		res, runErr = agent.ContinueStream(runCtx, messages, opts.Prompt, sink)
	default:
		res, runErr = agent.Continue(runCtx, messages, opts.Prompt)
	}
	r.persistTrace(ctx, runID, res)

	status := "done"
	summary := res.Final
	switch {
	// `runErr != nil` is load-bearing: a cancel can land in the window between the
	// model loop returning a complete answer and this switch (persistTrace above is
	// two DB writes wide). Keying on the cause alone would relabel a finished run
	// as stopped, and the chat layer skips persistAssistantTurn on that sentinel —
	// so the answer the user just watched arrive would never reach the log.
	case runErr != nil && errors.Is(context.Cause(runCtx), ErrRunStopped):
		// A user stop is not a failure. Keep the partial answer as the summary so a
		// second tab (or a reload) reattaches to what actually streamed, and hand
		// the sentinel back so the chat layer skips appending a half-answer to the
		// conversation the model reads next turn.
		status = "stopped"
		runErr = ErrRunStopped
	case runErr != nil:
		status = "error"
		summary = runErr.Error()
	}
	// Surface a faulted durable log (pi's Faulted state, degraded to a flag): if
	// buffered session entries could not be persisted even by the trailing
	// flush, the run row must say so — a resume of this session would silently
	// miss its last turn(s). Prepended so truncation can't hide it.
	if res.UnpersistedEntries > 0 {
		summary = fmt.Sprintf("[warning: %d durable-log entries were not persisted — a resume may miss the last turn(s)]\n\n", res.UnpersistedEntries) + summary
	}
	_ = r.Store.FinishAgentRun(ctx, runID, status, truncate(summary, 4000), res.Usage.InputTokens, res.Usage.OutputTokens, res.Usage.CostUSD)

	if opts.Reflect && runErr == nil {
		// Reflection resolves the agent's "reflection" task tier (defaults to pro,
		// itself falling back to flash when pro is unconfigured). Best-effort.
		rt := tierSet.resolve(TierFromName(taskMap[storage.TaskReflection]))
		_ = r.reflect(ctx, reflectInput{
			ProjectID: opts.ProjectID, RunID: runID, Provider: rt.Provider,
			Model: rt.Model, BaseURL: rt.BaseURL, APIKey: rt.APIKey, Memory: mem, Result: res,
		})
	}

	run := storage.AgentRun{
		ID: runID, ProjectID: opts.ProjectID, Trigger: trigger, Status: status,
		Summary: summary, TokenInput: res.Usage.InputTokens, TokenOutput: res.Usage.OutputTokens,
		CostUSD: res.Usage.CostUSD,
	}
	return run, res, runErr
}

// isBackgroundTrigger reports whether a run is unattended — a scheduled tick, an
// inbound webhook, or a delegated hand-off. Such runs are rejected at admission
// when already over budget (no user is waiting to read a graceful wrap-up);
// interactive chat/manual runs instead get the mid-run graceful stop so the user
// still receives a partial answer.
func isBackgroundTrigger(trigger string) bool {
	switch trigger {
	case "scheduled", "webhook", "delegate":
		return true
	default:
		return false
	}
}

// delegateRunner backs one subagent.Delegate.Run closure: it executes the
// delegated task as a full run of the target agent through the same execute
// path a direct chat uses, so the target runs under its own persona, tools,
// policy, and secrets and gets its own persisted run row (trigger "delegate").
// The caller's ctx flows through unchanged — cancelling the parent run cancels
// the delegate, and the delegation depth on ctx keeps the target's own run
// from re-delegating past the cap (A→B→A recursion bottoms out).
func (r *Runner) delegateRunner(projectID, agentID string) func(ctx context.Context, task string, sink agentcore.StreamSink) (string, agentcore.Usage, error) {
	return func(ctx context.Context, task string, sink agentcore.StreamSink) (string, agentcore.Usage, error) {
		_, res, err := r.execute(ctx, RunOptions{
			ProjectID: projectID,
			AgentID:   agentID,
			Trigger:   "delegate",
			Prompt:    task,
		}, sink)
		return res.Final, res.Usage, err
	}
}

// keyRefresher builds the per-turn API-key resolver agentcore calls before each
// model call (Config.RefreshKey), so a BYO key rotated mid-run is picked up
// without killing a long run. It returns nil unless KeyRefresh is on, leaving the
// loop's fixed-key default.
//
// CRITICAL: the closure returns an error (never an empty string) when no tier
// matches the requested provider. agentcore's reason() applies the returned key
// unconditionally — a returned "" would blank a still-valid key. Returning an
// error makes the loop skip the update and keep the key it has.
func (r *Runner) keyRefresher(projectID string) func(context.Context, string) (string, error) {
	if !r.KeyRefresh {
		return nil
	}
	return func(ctx context.Context, provider string) (string, error) {
		// Re-resolve against the workspace tier pool — the same source a run's
		// ladder resolves from — so a rotated workspace key is picked up mid-run.
		wsID, err := r.Store.WorkspaceIDForProject(ctx, projectID)
		if err != nil {
			return "", err
		}
		wsTiers, keys, err := r.Store.WorkspaceTiersForRun(ctx, wsID)
		if err != nil {
			return "", err
		}
		ts := tierSetFromWorkspace(wsTiers, keys)
		want := normalizeProvider(provider)
		// Resolve every tier and return the freshest key for the matching provider.
		// A run uses one provider across its ladder rungs in practice; matching on
		// the normalized name keeps the OpenAI-compat vendor case working too.
		for _, tier := range []Tier{TierLite, TierFlash, TierPro} {
			tc := ts.resolve(tier)
			if normalizeProvider(tc.Provider) == want && tc.APIKey != "" {
				return tc.APIKey, nil
			}
		}
		return "", fmt.Errorf("agentruntime: no key for provider %q on key refresh", provider)
	}
}

// normalizeProvider folds the empty provider label to "openai" (the wire
// default) and lower-cases it, so a tier's stored provider matches the name an
// agentcore provider reports.
func normalizeProvider(p string) string {
	p = strings.ToLower(strings.TrimSpace(p))
	if p == "" {
		return "openai"
	}
	return p
}

// ResumeRun continues a crashed or interrupted run from its durable session log
// (agentcore P9). The session is the durable object and runs are attempts on
// it: resume opens a NEW run row (billing, trace) but the loop continues the
// ORIGINAL session — history is rebuilt from the log in-loop, the crashed run's
// disabled tools are re-applied, and dangling retry-safe calls are replayed
// with their original call IDs, so idempotency keys stay stable and a dangling
// spawn_subagent reattaches to its child's recorded work instead of redoing it.
// A run with no durable log, or one that already reached a final answer, is not
// resumable and returns an error.
//
// Resuming a run that was itself a resume follows the chain: the attempt row
// records the session it continued (session_id), so the original log is found
// no matter which attempt the caller points at. The caller has already been
// authorized for the run (GetAgentRun enforces project membership).
func (r *Runner) ResumeRun(ctx context.Context, userID, projectID, runID string) (storage.AgentRun, agentcore.RunResult, error) {
	if r.SessionStore == nil {
		return storage.AgentRun{}, agentcore.RunResult{}, fmt.Errorf("agentruntime: durable sessions are disabled; cannot resume")
	}
	run, _, err := r.Store.GetAgentRun(ctx, userID, projectID, runID)
	if err != nil {
		return storage.AgentRun{}, agentcore.RunResult{}, err
	}
	// A run that is still executing must not be resumed: two loops appending to
	// the same durable log would interleave entries and race the seq assignment.
	// (A crashed process leaves status "running" too — the reaper flips those to
	// "error" after the stale-run window, after which resume proceeds.)
	if run.Status == "running" {
		return storage.AgentRun{}, agentcore.RunResult{}, fmt.Errorf("agentruntime: run %s is still running; wait for it to finish (or be reaped) before resuming", runID)
	}
	sessionID := runID
	// The windowed read, same as the loop's: a suffix that reduces to the same
	// state, or the whole log when it would not. This pass only reduces the log
	// (to validate resumability and recover the task), and reduce restarts at the
	// newest checkpoint either way — so the window and the whole log give it the
	// identical answer, for a fraction of the read.
	log, err := agentcore.LoadResumeLog(ctx, r.SessionStore, sessionID)
	if err != nil {
		return storage.AgentRun{}, agentcore.RunResult{}, err
	}
	if len(log) == 0 && run.SessionID != "" {
		// This run has no log of its own but references a session (it was itself
		// a resume attempt, or a chat run whose log lives under the conversation
		// chain): continue that session instead.
		if chained, cerr := agentcore.LoadResumeLog(ctx, r.SessionStore, run.SessionID); cerr == nil && len(chained) > 0 {
			sessionID, log = run.SessionID, chained
		}
	}
	if len(log) == 0 {
		return storage.AgentRun{}, agentcore.RunResult{}, fmt.Errorf("agentruntime: run %s has no durable log to resume", runID)
	}
	// The loop redoes recovery in-session with the real toolset; this pass only
	// validates resumability and extracts the task for recall/skill selection.
	plan := agentcore.RecoverSession(log, nil, agentcore.RecoveryMarkInterrupted)
	if plan.Completed {
		return storage.AgentRun{}, agentcore.RunResult{}, fmt.Errorf("agentruntime: run %s already completed; nothing to resume", runID)
	}

	// The default agent's id equals the project id; pass "" in that case so the
	// scope resolves to the project (the original single-agent path).
	agentID := run.AgentID
	if agentID == projectID {
		agentID = ""
	}
	// Preserve the original trigger so the autonomy rail re-applies: a resumed
	// scheduled/webhook run is still unattended and must not regain
	// external-write tools by being relabeled "manual".
	trigger := run.Trigger
	if trigger == "" {
		trigger = "manual"
	}
	return r.Run(ctx, RunOptions{
		ProjectID:       projectID,
		AgentID:         agentID,
		Trigger:         trigger,
		Prompt:          lastUserPrompt(plan.Messages),
		ResumeSessionID: sessionID,
	})
}

// lastUserPrompt returns the most recent user message content, used as the
// resumed run's task for memory recall and skill selection. Empty when the
// transcript has no user turn.
func lastUserPrompt(messages []agentcore.Message) string {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == agentcore.RoleUser {
			return messages[i].Content
		}
	}
	return ""
}

// CheapProvider resolves the provider+model for the orchestrator's front-desk
// "triage" task — cheap, no-analytics intent classification and small-talk. It
// reads the workspace model pool and the default agent's task→tier map (triage
// runs at project/default-agent scope; the orchestrator classifier is shared, so
// per-non-default-agent triage tiering is out of scope). An unconfigured tier
// resolves back to flash, so the common single-key setup works with no extra
// config. Returns an error when the agent is disabled or no key is configured, so
// the caller degrades to a setup prompt rather than a dead end.
func (r *Runner) CheapProvider(ctx context.Context, projectID string) (agentcore.LLMProvider, string, error) {
	cfg, err := r.Store.AgentConfigForRun(ctx, projectID)
	if err != nil {
		return nil, "", err
	}
	if !cfg.Enabled {
		return nil, "", fmt.Errorf("agent is disabled for this project")
	}
	wsID, err := r.Store.WorkspaceIDForProject(ctx, projectID)
	if err != nil {
		return nil, "", err
	}
	wsTiers, keys, err := r.Store.WorkspaceTiersForRun(ctx, wsID)
	if err != nil {
		return nil, "", err
	}
	if keys["flash"] == "" {
		return nil, "", fmt.Errorf("no workspace model key configured")
	}
	// The default agent's id is the project id (scope == project for triage).
	taskMap, err := r.Store.TaskTiersForRun(ctx, projectID)
	if err != nil {
		return nil, "", err
	}
	tc := tierSetFromWorkspace(wsTiers, keys).resolve(TierFromName(taskMap[storage.TaskTriage]))
	// Trace the classifier's cheap calls too — they carry real (small) cost.
	prov, err := buildTracedProvider(tc.Provider, tc.BaseURL, tc.APIKey, r.Tracer)
	if err != nil {
		return nil, "", err
	}
	return prov, tc.Model, nil
}

// RunTierWindow reports the input context window, in tokens, of the model that
// answers this project's runs — the operator's per-tier override when set,
// otherwise whatever the ai package can determine from the model id.
//
// It exists so the conversation-level compaction trigger and the in-run
// compaction budget are capped by the SAME number. They shrink the same thread
// for the same model, and a disagreement between them means one of the two is
// wrong for every run: the conversation replays a history the model rejects, or
// it summarizes far earlier than it needed to.
//
// Best-effort by design — every caller is on a path that must not fail because a
// window could not be resolved, so an error answers 0 and the caller falls back
// to its own conservative default.
func (r *Runner) RunTierWindow(ctx context.Context, projectID string) int {
	if r == nil || r.Store == nil {
		return 0
	}
	wsID, err := r.Store.WorkspaceIDForProject(ctx, projectID)
	if err != nil {
		return 0
	}
	wsTiers, keys, err := r.Store.WorkspaceTiersForRun(ctx, wsID)
	if err != nil {
		return 0
	}
	taskMap, err := r.Store.TaskTiersForRun(ctx, projectID)
	if err != nil {
		return 0
	}
	return EffectiveContextWindow(tierSetFromWorkspace(wsTiers, keys).resolve(TierFromName(taskMap[storage.TaskRun])))
}

// loadSkills maps active stored skills into agentcore.Skill headers for the
// definition. The full body is loaded only for the selected skills via
// skillLoader, mirroring Claude Code's split metadata/content handling.
func (r *Runner) loadSkills(ctx context.Context, projectID string) ([]agentcore.Skill, error) {
	rows, err := r.Store.ActiveSkillHeadersForScope(ctx, projectID)
	if err != nil {
		return nil, err
	}
	out := make([]agentcore.Skill, 0, len(rows))
	for _, s := range rows {
		out = append(out, agentcore.Skill{ID: s.ID, Name: s.Name, Description: s.Description, Enabled: s.Enabled})
	}
	return out, nil
}

// skillLoader returns a deferred body loader bound to one scope. Only selected
// skills are materialized into the system prompt, keeping unneeded content out of
// the hot path.
func (r *Runner) skillLoader(scopeID string) agentcore.SkillLoader {
	return func(ctx context.Context, ids []string) (map[string]string, error) {
		return r.Store.SkillBodiesByID(ctx, scopeID, ids)
	}
}

// runCredentials resolves the credential resolver for a run. When the project
// has its own secrets they take precedence (a run-scoped vault built from them);
// otherwise the host-global resolver is used unchanged. Building the vault
// validates every secret, so a malformed entry returns an error and the caller
// fails the run closed rather than running with an unresolved {{cred:NAME}}.
// Pure (no Store/ctx) so the precedence + fail-closed behavior is unit-testable.
func runCredentials(global agentcore.CredentialResolver, secrets map[string]string) (agentcore.CredentialResolver, error) {
	if len(secrets) == 0 {
		return global, nil
	}
	vault, err := credential.FromMap(secrets)
	if err != nil {
		return nil, err
	}
	return vault, nil
}

// resolveRunTools computes a run's final selectable-tool set from the host-global
// default tool and the per-agent selections (AgentGarden §6). Per-agent
// selections take precedence: an enabled selection builds a per-agent tool
// (overriding a host-global default of the same name); a disabled selection
// suppresses that tool entirely; a tool the agent has not selected falls back to
// the host-global default. Building validates each selection's config, so a
// malformed one returns an error and the caller fails the run closed.
//
// One selection may expand to several tools (mcp: one config, N remote servers,
// each with its own tool list), which is why building takes a context — the
// expansion talks to those servers. The returned notes describe capabilities
// that were skipped without failing the run (an optional MCP server that would
// not answer); the caller reports them rather than losing them silently.
//
// Takes no Store, so the precedence + fail-closed behavior stays unit-testable.
func resolveRunTools(ctx context.Context, toolCtx ToolBuildContext, globalHTTP agentcore.Tool, selections []storage.AgentToolSelection) ([]agentcore.Tool, []string, error) {
	var tools []agentcore.Tool
	var notes []string
	decided := make(map[string]bool, len(selections))
	for _, sel := range selections {
		decided[sel.Name] = true
		if !sel.Enabled {
			continue
		}
		// Skip a selection for a registered tool whose host dependency is not
		// wired in this run path (e.g. run_shell enabled but this deployment has
		// no sandbox). This is a stale selection, not a malformed one: failing the
		// whole run would block unrelated work (an analytics question dying on a
		// missing shell), and the catalog hides such tools so the user often
		// cannot toggle them off. An unknown tool or bad config still fails closed
		// below.
		if IsRegisteredTool(sel.Name) && !ToolAvailable(toolCtx, sel.Name) {
			continue
		}
		built, buildNotes, err := BuildToolsWithContext(ctx, toolCtx, sel.Name, sel.ConfigJSON)
		if err != nil {
			return nil, nil, fmt.Errorf("tool %q: %w", sel.Name, err)
		}
		tools = append(tools, built...)
		notes = append(notes, buildNotes...)
	}
	// The host-global default fills in only for a tool the agent hasn't decided.
	if globalHTTP != nil && !decided[globalHTTP.Name()] {
		tools = append(tools, globalHTTP)
	}
	return tools, notes, nil
}

// applyAutonomyRail enforces the hard unattended-publish gate: tools whose
// catalog spec is external-write are stripped from background-trigger runs
// (scheduled/webhook/delegate — no human watching the output) unless the
// project's autonomy is 'auto', the explicit opt-in rung. Interactive chat and
// manual runs pass through untouched at every autonomy, as does every
// non-external-write tool. This is enforcement in code, not persona prose —
// but it governs only catalog tools marked ExternalWrite (currently
// http_request). Network-capable sandbox tools (computer_use, browser_use)
// are deliberately unmarked so existing scheduled agents keep working; for
// agents granted those, unattended-publish restraint remains persona-governed
// plus whatever egress allowlist the host configures.
// Pure (no Store/ctx) so the trigger×autonomy matrix is unit-testable.
func applyAutonomyRail(tools []agentcore.Tool, trigger, autonomy string) []agentcore.Tool {
	if !isBackgroundTrigger(trigger) || autonomy == storage.AutonomyAuto {
		return tools
	}
	kept := make([]agentcore.Tool, 0, len(tools))
	for _, t := range tools {
		if ToolExternalWrite(t.Name()) {
			continue
		}
		kept = append(kept, t)
	}
	return kept
}

// persistTrace writes each tool-call projection to agent_tool_calls (§5.2, §9).
func (r *Runner) persistTrace(ctx context.Context, runID string, res agentcore.RunResult) {
	for _, t := range res.Tools {
		meta := t.ResultMeta
		if t.Reason != "" {
			meta = strings.TrimSpace(meta + " blocked:" + t.Reason)
		}
		if t.Error != "" {
			meta = strings.TrimSpace(meta + " error:" + t.Error)
		}
		_ = r.Store.RecordAgentToolCall(ctx, runID, storage.AgentToolCall{
			Tool: t.Tool, ArgsJSON: t.Args, Allowed: t.Allowed, ResultMeta: truncate(meta, 1000),
		})
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// storageSkill builds a reflect-proposed skill row.
func storageSkill(name, description, body string) storage.AgentSkill {
	return storage.AgentSkill{Name: name, Description: description, Body: body}
}

// tierSetFromWorkspace assembles the TierSet from the workspace model pool and
// the decrypted per-tier keys (keyed "lite"/"flash"/"pro"). The base
// provider/model/base_url columns are the flash tier; lite/pro use their own
// columns and key. A tier with no decrypted key is left unconfigured and
// resolves back to flash at call time.
func tierSetFromWorkspace(cfg storage.WorkspaceModelTiers, keys map[string]string) TierSet {
	return TierSet{
		TierFlash: TierConfig{Provider: cfg.Provider, Model: cfg.Model, BaseURL: cfg.BaseURL, APIKey: keys["flash"], ContextWindow: cfg.ContextWindow},
		TierLite:  TierConfig{Provider: cfg.LiteProvider, Model: cfg.LiteModel, BaseURL: cfg.LiteBaseURL, APIKey: keys["lite"], ContextWindow: cfg.LiteContextWindow},
		TierPro:   TierConfig{Provider: cfg.ProProvider, Model: cfg.ProModel, BaseURL: cfg.ProBaseURL, APIKey: keys["pro"], ContextWindow: cfg.ProContextWindow},
	}
}
