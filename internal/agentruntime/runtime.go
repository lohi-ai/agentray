package agentruntime

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/lohi-ai/agentray/agentcore"
	"github.com/lohi-ai/agentray/internal/opcore"
	"github.com/lohi-ai/agentray/sandbox"
	"github.com/lohi-ai/agentray/internal/usecase"
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
	Tracer agentcore.TraceSink
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
	// SeedDisabledTools pre-disables tools in the run's circuit breaker. A resume
	// passes the tools that were disabled in the crashed run so a broken tool stays
	// disabled across the restart. Empty — the default — starts every tool enabled.
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
	Todo *agentcore.TodoStore
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
	Subagents *agentcore.SubagentSettings
	// Delegates are the named other agents this one may route a spawn_subagent
	// task to (cross-agent delegation). The runner backs each Run closure by
	// executing the target agent's own full run path — its persona, tools,
	// policy, and secrets — so a delegate never inherits the caller's
	// capabilities. Effective only when Subagents is non-nil.
	Delegates []agentcore.Delegate
	// ReasoningEffort, when set ("low" | "medium" | "high"), is passed through
	// to reasoning models on every turn (OpenAI-wire reasoning_effort).
	ReasoningEffort string
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
		return agentcore.NewOpenAIEmbedder(apiKey, resolveBaseURL(baseURL), "")
	case "anthropic":
		return nil
	default:
		// OpenAI-compatible vendors serve /embeddings at the same base_url.
		if strings.TrimSpace(baseURL) != "" {
			return agentcore.NewOpenAIEmbedder(apiKey, baseURL, "")
		}
		return nil
	}
}

// NewTierProvider builds an LLMProvider for one tier's settings, applying the
// same routing as a run (OpenAI wire / Anthropic / OpenAI-compatible vendor).
// Exported for the config-test endpoint so a connectivity check uses the exact
// provider a real run would.
func NewTierProvider(provider, baseURL, apiKey string) (agentcore.LLMProvider, error) {
	// A connectivity check needs no trace sink; calls are still priced.
	return buildProvider(provider, baseURL, apiKey, nil)
}

// buildProvider constructs an LLMProvider for one tier's settings, applying the
// §13.1 base_url precedence for the OpenAI wire and routing any non-anthropic,
// non-openai label as an OpenAI-compatible vendor (base_url + default compat).
// Shared by the primary rung and every escalation rung.
func buildProvider(provider, baseURL, apiKey string, tracer agentcore.TraceSink) (agentcore.LLMProvider, error) {
	var (
		prov agentcore.LLMProvider
		err  error
	)
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "", "openai":
		prov, err = agentcore.NewProvider(agentcore.ProviderSpec{
			Name: "openai", APIKey: apiKey, BaseURL: resolveBaseURL(baseURL),
		})
	case "anthropic":
		prov, err = agentcore.NewProvider(agentcore.ProviderSpec{
			Name: "anthropic", APIKey: apiKey, BaseURL: strings.TrimSpace(baseURL),
		})
	default:
		// OpenAI-compatible vendor (e.g. a router): OpenAI wire at a custom
		// base_url with default compat. base_url is required and validated by
		// NewProvider.
		prov, err = agentcore.NewProvider(agentcore.ProviderSpec{
			Name: provider, APIKey: apiKey, BaseURL: strings.TrimSpace(baseURL), Compat: agentcore.DefaultCompat(),
		})
	}
	if err != nil {
		return nil, err
	}
	// Always wrap: this is the single place every model call is priced (filling
	// Usage.CostUSD) and, when a sink is wired, traced. A nil tracer still prices.
	return agentcore.NewTracingProvider(prov, agentcore.DefaultPricing(), tracer), nil
}

// buildRungs turns resolved tier configs into agentcore escalation rungs (used
// for the tiers above the primary).
func buildRungs(tcs []TierConfig, tracer agentcore.TraceSink) ([]agentcore.ModelRung, error) {
	rungs := make([]agentcore.ModelRung, 0, len(tcs))
	for _, tc := range tcs {
		prov, err := buildProvider(tc.Provider, tc.BaseURL, tc.APIKey, tracer)
		if err != nil {
			return nil, err
		}
		rungs = append(rungs, agentcore.ModelRung{Provider: prov, Model: tc.Model})
	}
	return rungs, nil
}

// Build wires a Growth Analyst agentcore.Agent: analytics tools for the project,
// a scope-derived permission policy, the per-project definition, and the OpenAI
// provider. Adding a second consumer reuses agentcore with a different ToolSet +
// Policy and zero core edits (§5 boundary).
func Build(p BuildParams) (*agentcore.Agent, error) {
	if p.APIKey == "" {
		return nil, fmt.Errorf("agentruntime: missing API key")
	}
	if p.Data == nil {
		return nil, fmt.Errorf("agentruntime: missing data source")
	}
	llm, err := buildProvider(p.Provider, p.BaseURL, p.APIKey, p.Tracer)
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

	// Default-deny allow-list from the enabled scopes. Selectable runtime tools add
	// their own names below; a wired sandbox alone does not expose run_shell.
	names := ScopeToolNames(p.Scopes)
	cfg := agentcore.Config{
		Provider:           llm,
		Model:              p.Model,
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
	// Build a custom Env only when a capability is wired; nil cfg.Env keeps the
	// analytics-only agent byte-for-byte unchanged.
	if p.Sandbox != nil || p.Credentials != nil {
		env := agentcore.DefaultEnv()
		if p.Sandbox != nil {
			env.Sandbox = p.Sandbox
		}
		env.Credentials = p.Credentials
		cfg.Env = &env
	}
	if p.HTTPTool != nil {
		names = append(names, p.HTTPTool.Name())
	}
	for _, t := range p.Tools {
		if t != nil {
			names = append(names, t.Name())
		}
	}
	if p.Todo != nil {
		names = append(names, agentcore.ToolUpdatePlan)
	}
	if p.Subagents != nil {
		cfg.Subagents = p.Subagents
		cfg.Delegates = p.Delegates
		names = append(names, agentcore.ToolSpawnSubagent)
	}
	cfg.ReasoningEffort = p.ReasoningEffort
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
	return agentcore.New(cfg)
}

// buildToolsAndHooks assembles the agent's ToolSet from the shared opcore/usecase
// registry, bound to one project/run, plus the terminate hook. Every operation is
// registered as a tool; the Policy (not the ToolSet) decides which the model is
// shown. The terminate hook ends the run after a terminal op (submit_recommendation)
// — except on a chat trigger, where the model must still reply to the user, so
// the run continues past the recommendation instead of stopping silently.
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
	// A wired sandbox installs the runtime injection guard for risky selectable
	// tools (such as run_shell). Tool exposure itself stays policy/catalog driven.
	if p.Sandbox != nil {
		hooks.Before = append(hooks.Before, sandbox.NewInjectionGuard().Hook())
	}
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
	// Built-in run todo list: the update_plan tool writes the plan, and a context
	// hook pins the live plan into every request so it survives compaction and
	// keeps a long run anchored to its goal.
	if p.Todo != nil {
		ts.Add(agentcore.NewTodoTool(p.Todo))
		hooks.Context = append(hooks.Context, agentcore.TodoContextHook(p.Todo))
	}
	return ts, hooks
}
