package agentruntime

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/lohi-ai/agentray/agentcore"
	"github.com/lohi-ai/agentray/agentcore/plugins/finishguard"
	"github.com/lohi-ai/agentray/agentcore/plugins/goal"
	"github.com/lohi-ai/agentray/agentcore/plugins/observe"
	"github.com/lohi-ai/agentray/agentcore/plugins/preset"
	sandboxplugin "github.com/lohi-ai/agentray/agentcore/plugins/sandbox"
	"github.com/lohi-ai/agentray/agentcore/plugins/spill"
	"github.com/lohi-ai/agentray/agentcore/plugins/subagent"
	"github.com/lohi-ai/agentray/agentcore/plugins/todo"
	"github.com/lohi-ai/agentray/ai"
	"github.com/lohi-ai/agentray/internal/dataplane/usecase"
	"github.com/lohi-ai/agentray/internal/shared/opcore"
	"github.com/lohi-ai/agentray/sandbox"
)

// BuildParams is everything needed to construct a Growth Analyst agent for one
// project run. The caller (HTTP handler / scheduler) decrypts the API key and
// resolves the definition before calling Build.
type BuildParams struct {
	ProjectID string
	// ScopeID keys the agent's own persona/skills/memory (AgentGarden §3). It is
	// the running agent's id: the default agent's id equals ProjectID, so leaving
	// this empty preserves the original single-agent behavior byte-for-byte. A
	// non-default agent passes its own id here while keeping ProjectID for the
	// analytics tools (which read project-wide data through the usecase layer).
	ScopeID     string
	Provider    string // "openai" | "anthropic"
	Model       string
	BaseURL     string // optional per-config override
	APIKey      string // decrypted, never persisted from here
	Scopes      Scopes
	Soul        string
	Agents      string
	Skills      []agentcore.Skill
	SkillLoader agentcore.SkillLoader
	Data        DataSource
	Memory      agentcore.MemoryStore // optional
	Notifier    usecase.Notifier      // optional; backs send_notification
	RunID       string                // links submitted recommendations to this run
	// Trigger is the run trigger (chat | scheduled | manual). On a chat trigger
	// submit_recommendation no longer ends the run, so the model still produces a
	// textual reply for the user instead of terminating silently.
	Trigger string
	// Escalation is the ordered fallback ladder (higher tiers) tried when the
	// primary provider/model errors. Built by the runner from the workspace's
	// per-tier model pool; empty disables fallback.
	Escalation []agentcore.ModelRung
	// ContextWindow is the primary model's input window in tokens, which caps the
	// compaction budget. Built by the runner from the primary tier (operator
	// override, else the ai package's answer for the model id); 0 leaves
	// MaxContextTokens to stand alone.
	ContextWindow int
	// CompactionProvider + CompactionModel pin the in-loop compaction summary call
	// to the agent's "compaction" task tier instead of borrowing the active rung.
	// Both unset keeps agentcore's default (the active rung summarizes).
	CompactionProvider agentcore.LLMProvider
	CompactionModel    string
	// Sandbox is the optional isolation substrate for untrusted-code tools. When
	// set, selectable sandbox tools such as run_shell can be built and a runtime
	// injection guard is installed. nil — the default — leaves the analytics-only
	// agent unchanged.
	Sandbox agentcore.Sandbox
	// Credentials is the optional secret vault (governance F7). When set, the
	// agent can reference a secret as {{cred:NAME}} in a tool argument and have
	// it resolved at the trust boundary, so the model never sees the literal.
	// nil — the default — passes tool arguments through unchanged.
	Credentials agentcore.CredentialResolver
	// HTTPTool is the optional outbound http_request capability (the worked
	// consumer of the credential vault). When set it is added to the ToolSet and
	// permitted in the policy, exactly like the sandbox's run_shell. nil — the
	// default — leaves the agent with no outbound HTTP surface.
	HTTPTool agentcore.Tool
	// Tools are the per-agent selectable tools resolved from the registry
	// (AgentGarden §6). Each is added to the ToolSet and permitted in the policy
	// just like HTTPTool, so the model is shown only the tools this agent was
	// granted. The runner folds the host-global HTTPTool default into this slice,
	// so a single tool of a given name appears regardless of source. Empty — the
	// default — leaves the agent with no selectable tools.
	Tools []agentcore.Tool
	// Tracer is the optional per-LLM-call trace sink. When set, every model call
	// in this run (and its escalation rungs) emits a TraceRecord with the request
	// messages, response, tokens, and computed cost. nil — the default — still
	// prices each call (filling Usage.CostUSD) but emits no trace.
	Tracer observe.Sink
	// StepGate is the optional pause-before-each-turn hook for the Lab's explain
	// mode. When set it is passed straight to agentcore.Config.StepGate, which
	// blocks each turn until the consumer permits it; nil — the default — keeps a
	// run continuous (production behavior). It changes nothing else about the run.
	StepGate func(ctx context.Context, turn int) error
	// Session + SessionID make the run durable (agentcore P9): the loop appends an
	// ordered entry log under SessionID so a crashed/compacted run can be reduced
	// and resumed. nil store — the default — keeps the run in-memory only.
	Session   agentcore.SessionStore
	SessionID string
	// ResumeSession makes the run continue the existing log at SessionID instead
	// of opening a fresh one: history is rebuilt from the log, dangling
	// retry-safe calls are replayed with their original ids, the crashed run's
	// disabled tools are re-applied, and a completed log returns its recorded
	// answer without a provider call. See agentcore.Config.ResumeSession.
	ResumeSession bool
	// SeedDisabledTools pre-disables tools in the run's circuit breaker. Empty —
	// the default — starts every tool enabled. (A resume no longer needs this:
	// ResumeSession recovers the disabled set from the log itself.)
	SeedDisabledTools []string
	// MaxTokens caps the model's output tokens per turn. 0 — the default — uses
	// the provider's own default. Set a generous value for agents that emit large
	// artifacts so the gateway cap doesn't truncate output.
	MaxTokens int
	// PromptCacheKey / PromptCacheRetention opt the run's provider calls into
	// prompt caching under a stable key (typically the agent scope), so the
	// persona/skills system prefix is reused across turns and runs. Empty key —
	// the default — leaves caching off.
	PromptCacheKey       string
	PromptCacheRetention string
	// GetSteering / GetFollowUp source agentcore's steering and follow-up queues:
	// steering is drained at the top of each turn (a mid-run correction), follow-up
	// when the run would otherwise stop (continue the same bounded run). nil — the
	// default — leaves the loop with no live control.
	GetSteering func(ctx context.Context) []agentcore.Message
	GetFollowUp func(ctx context.Context) []agentcore.Message
	// RefreshKey re-resolves the rung's API key before each turn (rotation-safe
	// long runs). It MUST return an error (never an empty string) on no-match: the
	// loop applies the returned key unconditionally, so "" would blank a valid key.
	// nil — the default — keeps the key fixed for the whole run.
	RefreshKey func(ctx context.Context, provider string) (string, error)
	// PrepareNextTurn is the per-turn save-point seam (agentcore P7): after each
	// turn the returned TurnState drives the next one. nil keeps the run static.
	PrepareNextTurn func(ctx context.Context, state agentcore.TurnState) agentcore.TurnState
	// BudgetGate is the per-turn budget ceiling check (#4): consulted with the
	// run's accumulated usage, returning true triggers a graceful stop. nil leaves
	// the run uncapped.
	BudgetGate func(ctx context.Context, u agentcore.Usage) bool
	// Todo, when set, gives the run a built-in todo list: the update_plan tool is
	// added + permitted, and a context hook pins the live plan into every request
	// so a long run stays on its goal across compaction. nil leaves the agent
	// without the tool (unchanged behavior).
	Todo *todo.Store
	// MaxContextTokens overrides the loop's soft compaction budget (the context
	// size above which old turns are summarized). 0 keeps agentcore's default
	// (200k). A small value is mainly a test/operations knob to exercise or tune
	// compaction without a 200k-token transcript.
	MaxContextTokens int
	// KeepRecentTokens overrides how much recent context compaction keeps verbatim.
	// 0 keeps agentcore's default (20k). Must be below MaxContextTokens for the LLM
	// summary path to engage; mainly a test/operations knob.
	KeepRecentTokens int
	// Subagents, when non-nil, enables the built-in spawn_subagent delegation
	// tool (ARCHITECT-AGENT-TEAM P1): the model may fork an ephemeral child that
	// inherits this agent's tools/policy/definition, runs with isolated history,
	// and returns only its final answer. The tool name is added to the policy
	// allow-list, exactly like update_plan. nil leaves the agent solo.
	Subagents *subagent.Plugin
	// Delegates are the named other agents this one may route a spawn_subagent
	// task to (cross-agent delegation). The runner backs each Run closure by
	// executing the target agent's own full run path — its persona, tools,
	// policy, and secrets — so a delegate never inherits the caller's
	// capabilities. Effective only when Subagents is non-nil.
	Delegates []subagent.Delegate
	// ReasoningEffort, when set ("low" | "medium" | "high"), is passed through
	// to reasoning models on every turn (OpenAI-wire reasoning_effort).
	ReasoningEffort string
	// OutputSchema, when non-nil, constrains every text answer to the given
	// JSON Schema at the provider (structured outputs). For verdict-shaped
	// agents (moderation / classification presets); nil leaves output free.
	OutputSchema *agentcore.OutputSchema
	// FinishGuard, when set, is consulted when the model produces a final
	// answer (agentcore's verify-on-stop): a non-empty return re-opens the run
	// with a bounded synthetic follow-up. The runner wires the evidence guard
	// here (see evidence_guard.go); nil accepts every finish.
	FinishGuard finishguard.Guard
	// Goal, when non-empty, activates agentcore's run-level goal gate (Claude
	// Code /goal analog): the completion contract lands in the system prompt
	// and a finish without a STATUS: DONE / STATUS: BLOCKED sentinel re-opens
	// the run. Empty — the default — leaves runs ungated.
	Goal string
	// ReviseGoal offers the gated model the update_goal tool, letting it restate
	// what finishing means when the work shows the condition to be wrong. Every
	// revision and its justification are appended to the durable session log as
	// an EntryGoal, which is the accountability record that makes the tool safe
	// to hand over.
	//
	// Off by default, and that default is the honest one: it gives the model the
	// key to its own gate. Effective only when Goal is non-empty — a tool that
	// revises nothing is not worth showing.
	ReviseGoal bool
	// Spill persists a tool result too large to sit inline and hands the model a
	// locator for the rest, so an oversized query export or build log is one
	// read_spill call away instead of a re-run of the tool. It MUST be durable:
	// the locator is written to the session log, so a store that dies with the
	// process would make a resumed run read its own spill back as not-found. nil
	// — the default — leaves the loop's head+tail truncation in place.
	Spill spill.SpillStore
	// ReportLogInvariant receives each divergence between what the model was
	// shown and what reached the durable log — the check that catches resume
	// corruption at the turn it is introduced rather than in a resumed run weeks
	// later. It is advisory: the detector never alters the run, so the right sink
	// is an error-level log. nil — the default — installs no detector.
	ReportLogInvariant func(observe.LogInvariantViolation)
}

// resolveBaseURL applies the §13.1 precedence: per-config base_url ->
// OPENAI_BASE_URL env -> vendor default (handled downstream by the provider).
func resolveBaseURL(configBaseURL string) string {
	if v := strings.TrimSpace(configBaseURL); v != "" {
		return v
	}
	return strings.TrimSpace(os.Getenv("OPENAI_BASE_URL"))
}

// newEmbedder builds the optional semantic-recall Embedder from the project's
// BYO key. Only the OpenAI-wire family exposes a /embeddings endpoint, so
// Anthropic (and any vendor without a base_url) returns nil — memory then falls
// back to keyword recall with zero config.
func newEmbedder(provider, baseURL, apiKey string) agentcore.Embedder {
	if apiKey == "" {
		return nil
	}
	switch provider {
	case "", "openai":
		return ai.NewOpenAIEmbedder(apiKey, resolveBaseURL(baseURL), "")
	case "anthropic":
		return nil
	default:
		// OpenAI-compatible vendors serve /embeddings at the same base_url.
		if strings.TrimSpace(baseURL) != "" {
			return ai.NewOpenAIEmbedder(apiKey, baseURL, "")
		}
		return nil
	}
}

// NewTierProvider builds an LLMProvider for one tier's settings, applying the
// same routing as a run (OpenAI wire / Anthropic / OpenAI-compatible vendor).
// Exported for the config-test endpoint so a connectivity check uses the exact
// provider a real run would.
func NewTierProvider(provider, baseURL, apiKey string) (agentcore.LLMProvider, error) {
	// A connectivity check is not a run and has no trace to attribute; calls are
	// still priced.
	return buildTracedProvider(provider, baseURL, apiKey, nil)
}

// buildTracedProvider builds a provider for a call made OUTSIDE a composition —
// a connectivity probe, the triage classifier, the reflect pass. Nothing will
// decorate it, so it is priced and traced here.
//
// Inside a composition, use buildProvider: the monitor plugin decorates every
// rung once, and wrapping twice would double-price the call and emit two trace
// rows per turn.
func buildTracedProvider(provider, baseURL, apiKey string, tracer observe.Sink) (agentcore.LLMProvider, error) {
	prov, err := buildProvider(provider, baseURL, apiKey)
	if err != nil {
		return nil, err
	}
	return observe.Wrap(prov, observe.DefaultPricing(), tracer), nil
}

// buildProvider constructs an LLMProvider for one tier's settings, applying the
// §13.1 base_url precedence for the OpenAI wire and routing any non-anthropic,
// non-openai label as an OpenAI-compatible vendor (base_url + default compat).
// Shared by the primary rung and every escalation rung.
//
// It returns the RAW provider. Pricing and tracing are contributed once, by the
// monitor plugin in the composition, which decorates every rung the run can
// reach — primary, escalation, and compaction alike. Wrapping here as well would
// price each call twice and emit two trace rows per turn.
func buildProvider(provider, baseURL, apiKey string) (agentcore.LLMProvider, error) {
	var (
		prov agentcore.LLMProvider
		err  error
	)
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "", "openai":
		prov, err = ai.NewClient(ai.ClientSpec{
			Name: "openai", APIKey: apiKey, BaseURL: resolveBaseURL(baseURL),
		})
	case "anthropic":
		prov, err = ai.NewClient(ai.ClientSpec{
			Name: "anthropic", APIKey: apiKey, BaseURL: strings.TrimSpace(baseURL),
		})
	default:
		// OpenAI-compatible vendor (e.g. a router): OpenAI wire at a custom
		// base_url with default compat. base_url is required and validated by
		// NewProvider.
		prov, err = ai.NewClient(ai.ClientSpec{
			Name: provider, APIKey: apiKey, BaseURL: strings.TrimSpace(baseURL), Compat: ai.DefaultCompat(),
		})
	}
	if err != nil {
		return nil, err
	}
	return prov, nil
}

// buildRungs turns resolved tier configs into agentcore escalation rungs (used
// for the tiers above the primary). The rungs are raw; the composition's monitor
// plugin decorates them, so a run that escalates does not silently stop being
// priced or traced.
func buildRungs(tcs []TierConfig) ([]agentcore.ModelRung, error) {
	rungs := make([]agentcore.ModelRung, 0, len(tcs))
	for _, tc := range tcs {
		prov, err := buildProvider(tc.Provider, tc.BaseURL, tc.APIKey)
		if err != nil {
			return nil, err
		}
		rungs = append(rungs, agentcore.ModelRung{
			Provider: prov,
			Model:    tc.Model,
			// Each rung carries its own window, so escalating from a large-window
			// model to a small one re-derives the compaction budget instead of
			// carrying the first rung's headroom onto a model that cannot hold it.
			ContextWindow: EffectiveContextWindow(tc),
		})
	}
	return rungs, nil
}

// revisableGoal reports whether this run installs the AGENT-revisable goal gate.
//
// Both halves of that decision read this one function, because either half alone
// is broken. The gate must be swapped into the plugin list (so update_goal is
// contributed at all) AND goal.ToolName must be in the allow-list (so the model
// is shown it). Installing the plugin without the name is the quiet failure:
// AllowList.PermittedTools filters every contributed tool through the list, so
// the tool is registered and then hidden, and nothing anywhere reports it.
//
// A revisable gate with no condition to revise is not a capability, so an empty
// Goal turns it off rather than offering a tool that edits nothing.
func revisableGoal(p BuildParams) bool { return p.ReviseGoal && p.Goal != "" }

// permittedToolNames is the run's default-deny allow-list: the scope-derived
// analytics tools, plus every tool a conditionally-installed plugin contributes.
//
// It is one function rather than a slice grown across Build because the list has
// to stay exhaustive. A plugin added to the composition without its name here
// still registers its tool and still never reaches the model — a capability that
// is present, permitted by nothing, and silent about it. Keeping the whole
// decision in one place is what makes that omission visible, and testable.
//
// A wired sandbox alone exposes nothing: selectable runtime tools (run_shell and
// friends) arrive through p.Tools and are named here like any other.
func permittedToolNames(p BuildParams) []string {
	names := ScopeToolNames(p.Scopes)
	if p.HTTPTool != nil {
		names = append(names, p.HTTPTool.Name())
	}
	for _, t := range p.Tools {
		if t != nil {
			names = append(names, t.Name())
		}
	}
	if p.Todo != nil {
		names = append(names, todo.ToolName)
	}
	if p.Subagents != nil {
		names = append(names, subagent.ToolSpawnSubagent)
	}
	if revisableGoal(p) {
		names = append(names, goal.ToolName)
	}
	return names
}

// Build wires a Growth Analyst agentcore.Agent: analytics tools for the project,
// a scope-derived permission policy, the per-project definition, and the OpenAI
// provider. Adding a second consumer reuses agentcore with a different ToolSet +
// Policy and zero core edits (§5 boundary).
//
// It is a PLUGIN COMPOSITION, not a Config hand-off. The default agent comes
// from preset.Full — the same list any other agentcore consumer starts from —
// and everything specific to this product is appended to it as a named plugin:
// the goal gate, the evidence finish guard, delegation, the run plan, the
// sandbox and its guard, the vault, cost accounting. The payoff is that the
// composition can be READ: agent.Describe() names which plugin owns which seam,
// and dropping a capability is one entry off a list rather than an edit to a
// build function that knows about all of them.
//
// BuildParams stays the external surface. Its fields are still routed through an
// agentcore.Config, because Config is exactly the vocabulary preset.Plugins
// mirrors — the migration moved where the capabilities come from, not how a
// caller describes a run.
func Build(p BuildParams) (*agentcore.Agent, error) {
	if p.APIKey == "" {
		return nil, fmt.Errorf("agentruntime: missing API key")
	}
	if p.Data == nil {
		return nil, fmt.Errorf("agentruntime: missing data source")
	}
	llm, err := buildProvider(p.Provider, p.BaseURL, p.APIKey)
	if err != nil {
		return nil, fmt.Errorf("agentruntime: %w", err)
	}

	// The persona/skills/memory scope defaults to the project (the default agent),
	// so an unset ScopeID preserves the original single-agent behavior exactly.
	scopeID := p.ScopeID
	if scopeID == "" {
		scopeID = p.ProjectID
	}

	tools, hooks := buildToolsAndHooks(p)

	names := permittedToolNames(p)
	cfg := agentcore.Config{
		Provider:           llm,
		Model:              p.Model,
		ContextWindow:      p.ContextWindow,
		Escalation:         p.Escalation,
		CompactionProvider: p.CompactionProvider,
		CompactionModel:    p.CompactionModel,
		Tools:              tools,
		Memory:             p.Memory,
		Hooks:              hooks,
		Definition: agentcore.AgentDefinition{
			ScopeID:     scopeID,
			Soul:        p.Soul,
			Agents:      p.Agents,
			Skills:      p.Skills,
			SkillLoader: p.SkillLoader,
		},
		StepGate: p.StepGate,
		// Durable resume, live control, and per-turn seams (P7/P9). All optional:
		// an empty/nil value here leaves the corresponding loop default in place, so
		// the analytics-only run is unchanged unless the runner wires these.
		Session:              p.Session,
		SessionID:            p.SessionID,
		ResumeSession:        p.ResumeSession,
		SeedDisabledTools:    p.SeedDisabledTools,
		MaxTokens:            p.MaxTokens,
		PromptCacheKey:       p.PromptCacheKey,
		PromptCacheRetention: p.PromptCacheRetention,
		GetSteeringMessages:  p.GetSteering,
		GetFollowUpMessages:  p.GetFollowUp,
		RefreshKey:           p.RefreshKey,
		PrepareNextTurn:      p.PrepareNextTurn,
		BudgetGate:           p.BudgetGate,
	}
	cfg.ReasoningEffort = p.ReasoningEffort
	cfg.OutputSchema = p.OutputSchema
	cfg.Goal = p.Goal
	cfg.Policy = agentcore.NewAllowList(names...)
	if p.MaxContextTokens > 0 {
		limits := agentcore.DefaultLimits()
		limits.MaxContextTokens = p.MaxContextTokens
		cfg.Limits = &limits
	}
	if p.KeepRecentTokens > 0 {
		cs := agentcore.DefaultCompactionSettings()
		cs.KeepRecentTokens = p.KeepRecentTokens
		cfg.Compaction = &cs
	}

	// The default agent, plus the capabilities Config has no field for. Spill is
	// the only one that needs infrastructure; without a durable store it stays
	// off and the loop's own truncation stands.
	list := preset.Full(cfg, preset.Options{
		Spill:              p.Spill,
		ReportLogInvariant: p.ReportLogInvariant,
	})

	// --- the product's own plugins -----------------------------------------

	// Cost accounting brackets every model call this composition can make — the
	// primary rung, each escalation rung, and the compaction rung. A nil tracer
	// still prices (so Usage.CostUSD is honest) and emits nothing.
	list = append(list, observe.Monitor{Sink: p.Tracer})

	// The goal gate is already in the list: preset's goal.Plugin registers the
	// durable half (the seam that records the condition) AND the gate extension,
	// so it lands ahead of the finish guard appended below. That order is the
	// contract — the first stop interceptor to re-open the run wins, and an unmet
	// goal makes any verification pass on that same answer moot.
	//
	// A revisable gate swaps that entry rather than adding one: two goal plugins
	// would both claim the durable goal seam. Replace moves the entry to the end
	// of the list as it stands, which is still ahead of the finish guard appended
	// just below — the only other stop interceptor in the composition, and the
	// one the ordering contract is about.
	if revisableGoal(p) {
		list = preset.Replace(list, goal.Plugin{Goal: p.Goal, Revisable: true})
	}
	if p.FinishGuard != nil {
		list = append(list, finishguard.Of(p.FinishGuard))
	}
	if p.Subagents != nil {
		sa := *p.Subagents
		sa.Delegates = p.Delegates
		list = append(list, sa)
	}
	if p.Todo != nil {
		// The tool and the context hook are one plugin: either alone is broken —
		// a plan the model never sees again, or a pin on a plan nothing can write.
		list = append(list, todo.With(p.Todo))
	}
	// A wired sandbox brings its own runtime injection guard for risky selectable
	// tools (run_shell and friends). One plugin carries both, so the substrate
	// can never be installed with nothing reading what is sent into it. Tool
	// exposure itself stays policy/catalog driven — a backend alone exposes
	// nothing.
	if p.Sandbox != nil {
		list = append(list, sandboxplugin.Guarded(p.Sandbox, sandbox.NewInjectionGuard().Hook()))
	}
	if p.Credentials != nil {
		list = append(list, sandboxplugin.Vault(p.Credentials))
	}
	return agentcore.Build(list...)
}

// buildToolsAndHooks assembles the agent's ToolSet from the shared opcore/usecase
// registry, bound to one project/run, plus the terminate hook. Every operation is
// registered as a tool; the Policy (not the ToolSet) decides which the model is
// shown. The terminate hook ends the run after a terminal op (submit_recommendation)
// — except on a chat trigger, where the model must still reply to the user, so
// the run continues past the recommendation instead of stopping silently.
//
// This is the product's own contribution, and it reaches the composition through
// the tools and hooks plugins (preset routes cfg.Tools / cfg.Hooks to them).
// Capabilities that own BOTH a tool and a hook — the run plan, the sandbox
// guard — are plugins of their own instead, so neither half can be wired without
// the other.
func buildToolsAndHooks(p BuildParams) (*agentcore.ToolSet, agentcore.Hooks) {
	reg := usecase.Registry()
	cc := opcore.CallContext{
		ProjectID: p.ProjectID,
		RunID:     p.RunID,
		Deps:      &usecase.Deps{Repo: p.Data, Memory: p.Memory, Notifier: p.Notifier},
	}
	tools := opcore.Tools(reg, cc)
	terminal := opcore.TerminalNames(reg)
	isChat := p.Trigger == "chat"

	terminate := func(_ context.Context, call agentcore.ToolCall, result string, _ error) (string, bool) {
		return result, !isChat && terminal[call.Name]
	}

	ts := agentcore.NewToolSet(tools...)
	hooks := agentcore.Hooks{After: []agentcore.AfterToolCall{terminate}}
	// The outbound HTTP tool is the legitimate-egress consumer of the credential
	// vault: it makes controlled requests to an allowlisted host, and the loop
	// resolves any {{cred:NAME}} in its arguments before it runs.
	if p.HTTPTool != nil {
		ts.Add(p.HTTPTool)
	}
	// Per-agent selectable tools (registry-built) are added the same way; the
	// runner has already folded in the host-global default, so this is the full
	// granted set.
	for _, t := range p.Tools {
		if t != nil {
			ts.Add(t)
		}
	}
	return ts, hooks
}
