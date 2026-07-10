package agentcore

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
)

// Agent is a configured runtime instance: a provider + model, an injected
// ToolSet, a Policy, lifecycle Hooks, an optional MemoryStore, and an
// AgentDefinition. It is product-agnostic — the same Agent type powers the
// Growth Analyst and any future consumer.
type Agent struct {
	provider LLMProvider
	model    string
	tools    *ToolSet
	policy   Policy
	hooks    Hooks
	memory   MemoryStore // optional; nil disables recall/persistence
	def      AgentDefinition
	limits   Limits
	env      Env
	// compaction tunes how the loop summarizes a long transcript once it
	// approaches limits.MaxContextTokens.
	compaction CompactionSettings
	// compactionProvider + compactionModel, when both set, pin the compaction
	// summary call to a dedicated tier (e.g. a cheap "lite" rung) instead of
	// borrowing whichever rung the run has escalated to. Leaving either unset
	// preserves the default: compaction uses the active rung's model.
	compactionProvider LLMProvider
	compactionModel    string
	// refreshKey, when set, re-resolves the provider's API key before each turn
	// (pi's per-turn getApiKey) so expiring BYO tokens don't kill long runs. The
	// returned key is applied only if the provider implements KeyUpdater.
	refreshKey func(ctx context.Context, provider string) (string, error)
	// escalation is the ordered fallback ladder tried when the primary
	// provider/model errors (a retryable failure, not a cancellation). It is
	// product-agnostic: a consumer's tier system maps onto it, but agentcore only
	// sees an ordered list of provider+model rungs.
	escalation []ModelRung
	// getSteering, when set, is drained at the top of every turn: any messages it
	// returns are threaded into the conversation before the model reasons, so a
	// user can inject a mid-run correction honored on the next turn (pi's steering
	// queue). The consumer owns the source (channel, DB, SSE input).
	getSteering func(ctx context.Context) []Message
	// getFollowUp, when set, is drained once the model produces a final answer: if
	// it returns messages, they are appended and the loop restarts instead of
	// returning, so a conversation continues within one bounded run (pi's
	// follow-up queue).
	getFollowUp func(ctx context.Context) []Message
	// prepareNextTurn, when set, is called after each completed turn with the
	// current TurnState; the returned state (model / tools / system) drives the
	// next turn without mutating the in-flight one. nil keeps the run static.
	prepareNextTurn func(ctx context.Context, state TurnState) TurnState
	// budgetGate, when set, is consulted at the top of each turn with the run's
	// accumulated usage. Returning true triggers a graceful stop (#4): the loop
	// injects a final "budget exhausted — summarize and stop" user turn, strips
	// tools so the model can only write a wrap-up, and returns with StopReason
	// "budget_exhausted". The consumer owns the ceiling + spend lookup; agentcore
	// only sees the boolean verdict against the running Usage.
	budgetGate func(ctx context.Context, u Usage) bool
	// session + sessionID, when both set, make the run durable: the loop appends
	// typed entries (messages, compaction brackets, leaf) to the append-only log
	// so a crashed or compacted run can be reduced and resumed (P9). nil disables
	// durability — the run is purely in-memory.
	session   SessionStore
	sessionID string
	// stepGate, when set, is called at the top of every turn before any work
	// (compaction, steering, reason) happens. It blocks until the consumer permits
	// the turn to proceed, returning a non-nil error to halt the run. This is how
	// the Lab's explain mode pauses a live run before each step without changing
	// any other run behavior: gating, secret resolution, budgets, and escalation
	// all still run after the gate releases. nil (the default) never pauses, so
	// production runs are unaffected.
	stepGate func(ctx context.Context, turn int) error
	// running is the single-flight guard (pi's phase machine, reduced to a binary
	// busy/idle for our run-only surface): a run sets it via CAS and clears it on
	// exit, so a second concurrent Prompt on the same Agent fails fast with ErrBusy
	// instead of racing on the shared run state. A hook may still drive a *different*
	// Agent instance reentrantly; only the same instance is single-flighted.
	running int32
	// retry bounds the same-model backoff retry of a transient provider failure
	// (429/5xx/network blip) before the loop escalates down the ladder, so a brief
	// outage no longer jumps straight to a pricier rung or aborts the run.
	retry RetryPolicy
	// cacheKey + cacheRetention, when cacheKey is non-empty, opt every provider call
	// in the run into prompt caching (OpenAI prompt_cache_key; Anthropic cache_control
	// on the system prefix). Empty (the default) leaves caching off, so providers and
	// compat servers that don't support it are unaffected.
	cacheKey       string
	cacheRetention string
	// seedDisabledTools pre-populates the circuit breaker's disabled set at run
	// start, so a tool disabled in a crashed run stays disabled when that run is
	// resumed (the disable survives via the durable log → RecoverSession). Empty —
	// the default — starts every tool enabled.
	seedDisabledTools []string
	// maxTokens caps the model's output tokens per turn. 0 — the default — lets
	// the provider apply its own default (the gateway's cap), which can truncate
	// large outputs with stop_reason:"length". Set a generous value for agents
	// that emit big artifacts (long documents, full HTML pages).
	maxTokens int
	// reasoningEffort, when set, is passed through to providers that support a
	// reasoning/thinking-effort knob ("low" | "medium" | "high"; OpenAI-wire
	// reasoning_effort). Providers without the knob ignore it. Empty — the
	// default — sends nothing, so strict compat servers are unaffected.
	reasoningEffort string
	// subagents, when non-nil, enables the built-in spawn_subagent delegation
	// tool (still policy-gated): the loop advertises it while the run's
	// delegation depth (carried on ctx, see DelegationDepth) is below MaxDepth,
	// and each spawn forks an ephemeral child inheriting this agent's
	// capabilities with isolated history. nil — the default — leaves the agent
	// solo.
	subagents *SubagentSettings
	// delegates are the named other agents this agent may hand a task to via
	// spawn_subagent's agent parameter (cross-agent delegation, ARCHITECT-
	// AGENT-TEAM P3 pulled forward). Each entry is an opaque closure injected by
	// the consumer — agentcore never loads or builds another agent itself, so it
	// stays product-agnostic. Empty leaves only self-delegation.
	delegates []Delegate
	// childUsage accumulates the usage of sub-agent runs spawned during the
	// current run (written by spawn_subagent, possibly from parallel tool
	// goroutines); runLoop folds and resets it into the RunResult so a parent
	// run's accounting includes what its children spent.
	childMu    sync.Mutex
	childUsage Usage
}

// addChildUsage folds one child run's usage into the parent's accumulator.
func (a *Agent) addChildUsage(u Usage) {
	a.childMu.Lock()
	a.childUsage.InputTokens += u.InputTokens
	a.childUsage.OutputTokens += u.OutputTokens
	a.childUsage.CacheReadTokens += u.CacheReadTokens
	a.childUsage.CacheWriteTokens += u.CacheWriteTokens
	a.childUsage.CostUSD += u.CostUSD
	a.childMu.Unlock()
}

// takeChildUsage returns and resets the accumulated child usage.
func (a *Agent) takeChildUsage() Usage {
	a.childMu.Lock()
	u := a.childUsage
	a.childUsage = Usage{}
	a.childMu.Unlock()
	return u
}

// peekChildUsage returns the accumulated child usage WITHOUT resetting it, so the
// mid-run budget gate can meter sub-agent spend the loop has not yet folded into
// the run's RunResult (takeChildUsage does that once, on run exit).
func (a *Agent) peekChildUsage() Usage {
	a.childMu.Lock()
	u := a.childUsage
	a.childMu.Unlock()
	return u
}

// addUsage returns u plus v across every metered dimension.
func addUsage(u, v Usage) Usage {
	u.InputTokens += v.InputTokens
	u.OutputTokens += v.OutputTokens
	u.CacheReadTokens += v.CacheReadTokens
	u.CacheWriteTokens += v.CacheWriteTokens
	u.CostUSD += v.CostUSD
	return u
}

// ErrBusy is returned by a run entry point (Prompt/Continue/…) when the Agent is
// already running. One Agent instance drives one run at a time; spin up a second
// instance (or wait for the first to finish) for concurrent work.
var ErrBusy = errors.New("agentcore: agent is busy with another run")

// tryAcquire claims the single-flight slot, reporting whether it was free.
func (a *Agent) tryAcquire() bool { return atomic.CompareAndSwapInt32(&a.running, 0, 1) }

// release frees the single-flight slot for the next run.
func (a *Agent) release() { atomic.StoreInt32(&a.running, 0) }

// ModelRung is one provider+model the loop may fall back to when the rung above
// it errors. Consumers build the ladder (e.g. lite→flash→pro); agentcore just
// walks it.
type ModelRung struct {
	Provider LLMProvider
	Model    string
}

// TurnState is the per-turn save-point: the model, tools, and system prompt that
// will drive the next provider request. After each turn the loop hands the
// current state to a consumer's PrepareNextTurn hook, which may return a modified
// copy; the change applies to the next turn only and never mutates the in-flight
// request (pi's prepareNextTurn). Messages is supplied read-only for the hook to
// inspect — returning a different slice does not replace the loop's history.
type TurnState struct {
	Model    string
	Tools    *ToolSet
	System   string
	Messages []Message
}

// Config wires an Agent. Provider, Model, Tools, and Policy are required; the
// rest have safe defaults (DenyAll policy, no memory, DefaultLimits, DefaultEnv).
type Config struct {
	Provider   LLMProvider
	Model      string
	Tools      *ToolSet
	Policy     Policy
	Hooks      Hooks
	Memory     MemoryStore
	Definition AgentDefinition
	Limits     *Limits
	Env        *Env
	// Compaction overrides the default compaction settings (recent-token budget
	// kept verbatim). nil uses DefaultCompactionSettings().
	Compaction *CompactionSettings
	// CompactionProvider + CompactionModel, when both set, pin the in-loop
	// compaction summary call to a dedicated tier instead of borrowing the run's
	// active escalation rung. Leaving either unset keeps today's behavior (the
	// active rung summarizes). The consumer's tier system maps a "compaction"
	// task kind onto these.
	CompactionProvider LLMProvider
	CompactionModel    string
	// RefreshKey is an optional per-turn API-key resolver. It is invoked before
	// each turn with the provider name; a non-empty result is pushed into the
	// provider via KeyUpdater. Use it for short-lived / rotating BYO credentials.
	RefreshKey func(ctx context.Context, provider string) (string, error)
	// Escalation is an optional ordered fallback ladder. When the primary
	// Provider/Model errors on a turn, the loop retries that turn down the ladder
	// before giving up, then sticks with the working rung for later turns.
	Escalation []ModelRung
	// GetSteeringMessages is an optional callback drained at the top of each turn;
	// returned messages are injected before the model reasons (mid-run steering).
	GetSteeringMessages func(ctx context.Context) []Message
	// GetFollowUpMessages is an optional callback drained when the agent would
	// stop; returned messages restart the loop instead of ending the run.
	GetFollowUpMessages func(ctx context.Context) []Message
	// PrepareNextTurn is an optional save-point hook called after each turn; the
	// returned TurnState (model / tools / system) drives the next turn. nil keeps
	// the model, tools, and prompt fixed for the whole run.
	PrepareNextTurn func(ctx context.Context, state TurnState) TurnState
	// BudgetGate is an optional per-turn ceiling check (#4). Consulted with the
	// run's accumulated usage at the top of each turn; returning true triggers a
	// one-turn graceful stop that summarizes and halts. nil leaves the run
	// uncapped (bounded only by MaxTurns / MaxToolCalls).
	BudgetGate func(ctx context.Context, u Usage) bool
	// Session + SessionID enable durable, resumable runs (P9): when both are set
	// the loop appends typed entries to the append-only SessionStore. Leaving
	// either unset keeps the run in-memory only.
	Session   SessionStore
	SessionID string
	// StepGate is an optional pause-before-each-turn hook. When set, the loop calls
	// it at the top of every turn and blocks until it returns; a non-nil error
	// halts the run. The Lab's explain mode uses it to step a live run; leaving it
	// nil keeps runs continuous (the production default).
	StepGate func(ctx context.Context, turn int) error
	// Retry overrides the same-model backoff policy applied before escalation. nil
	// uses DefaultRetryPolicy(); a partial override fills its zero fields from it.
	Retry *RetryPolicy
	// PromptCacheKey, when set, opts every provider call into prompt caching under
	// this key (typically the session id). PromptCacheRetention hints the window
	// ("" | "short" | "long" | "24h"). Empty key leaves caching off — the default.
	PromptCacheKey       string
	PromptCacheRetention string
	// SeedDisabledTools pre-disables the named tools for this run's circuit
	// breaker. A resume passes the tools that were disabled in the crashed run
	// (recovered from its durable log) so a persistently broken tool is not
	// retried from scratch after resume. Empty starts every tool enabled.
	SeedDisabledTools []string
	// MaxTokens caps the model's output tokens per turn. 0 lets the provider use
	// its own default. Set this for agents that emit large artifacts so the
	// gateway's default cap doesn't truncate output with stop_reason:"length".
	MaxTokens int
	// ReasoningEffort, when set ("low" | "medium" | "high"), asks reasoning
	// models to spend that much thinking effort per turn (OpenAI-wire
	// reasoning_effort). Providers without the knob ignore it; empty sends
	// nothing.
	ReasoningEffort string
	// Subagents, when non-nil, enables the built-in spawn_subagent tool (the
	// tool must additionally be permitted by Policy): the model may delegate a
	// self-contained task to an ephemeral child agent that inherits this agent's
	// capabilities, runs with isolated history, and returns only its final
	// answer. Zero-value settings apply the defaults (depth 1, 8 per run, 48 KB
	// answer cap). nil — the default — leaves the agent solo.
	Subagents *SubagentSettings
	// Delegates names the other agents this one may invoke through
	// spawn_subagent's agent parameter. Each Delegate.Run closure executes the
	// target agent under its own identity (persona, tools, policy) — the
	// consumer backs it; agentcore only routes the task and enforces the shared
	// caps (depth, per-run budget, output size). Requires Subagents non-nil.
	Delegates []Delegate
}

// New constructs an Agent from a Config, installing the permission gate as the
// first beforeToolCall hook so it always runs before consumer hooks.
func New(cfg Config) (*Agent, error) {
	if cfg.Provider == nil {
		return nil, errors.New("agentcore: provider is required")
	}
	if cfg.Model == "" {
		return nil, errors.New("agentcore: model is required")
	}
	if cfg.Tools == nil {
		cfg.Tools = NewToolSet()
	}
	if cfg.Policy == nil {
		cfg.Policy = DenyAll{}
	}
	limits := DefaultLimits()
	if cfg.Limits != nil {
		limits = *cfg.Limits
	}
	env := DefaultEnv()
	if cfg.Env != nil {
		env = *cfg.Env
	}
	compaction := DefaultCompactionSettings()
	if cfg.Compaction != nil {
		compaction = *cfg.Compaction
	}
	retry := DefaultRetryPolicy()
	if cfg.Retry != nil {
		retry = cfg.Retry.normalized()
	}

	// The permission gate is itself a beforeToolCall hook, run first.
	policy := cfg.Policy
	gate := func(ctx context.Context, call ToolCall) Decision { return policy.Allow(ctx, call) }
	hooks := cfg.Hooks
	hooks.Before = append([]BeforeToolCall{gate}, hooks.Before...)

	return &Agent{
		provider:           cfg.Provider,
		model:              cfg.Model,
		tools:              cfg.Tools,
		policy:             policy,
		hooks:              hooks,
		memory:             cfg.Memory,
		def:                cfg.Definition,
		limits:             limits,
		env:                env,
		compaction:         compaction,
		compactionProvider: cfg.CompactionProvider,
		compactionModel:    cfg.CompactionModel,
		refreshKey:         cfg.RefreshKey,
		escalation:         cfg.Escalation,
		getSteering:        cfg.GetSteeringMessages,
		getFollowUp:        cfg.GetFollowUpMessages,
		prepareNextTurn:    cfg.PrepareNextTurn,
		budgetGate:         cfg.BudgetGate,
		session:            cfg.Session,
		sessionID:          cfg.SessionID,
		stepGate:           cfg.StepGate,
		retry:              retry,
		cacheKey:           cfg.PromptCacheKey,
		cacheRetention:     cfg.PromptCacheRetention,
		seedDisabledTools:  cfg.SeedDisabledTools,
		maxTokens:          cfg.MaxTokens,
		reasoningEffort:    cfg.ReasoningEffort,
		subagents:          cfg.Subagents,
		delegates:          cfg.Delegates,
	}, nil
}

// Prompt runs a single interactive turn-loop from a user message and returns
// the result. task seeds skill selection and memory recall (defaults to the
// user input).
func (a *Agent) Prompt(ctx context.Context, userInput string) (RunResult, error) {
	return a.runLoop(ctx, []Message{{Role: RoleUser, Content: userInput}}, userInput, nil)
}

// PromptStream runs the same turn-loop but streams the assistant's tokens and
// tool-call traces to sink as they are produced, for a live (SSE) viewer. The
// returned RunResult is identical to Prompt's — streaming is additive.
func (a *Agent) PromptStream(ctx context.Context, userInput string, sink StreamSink) (RunResult, error) {
	return a.runLoop(ctx, []Message{{Role: RoleUser, Content: userInput}}, userInput, sink)
}

// Continue resumes from an existing message history (a working-memory thread).
func (a *Agent) Continue(ctx context.Context, history []Message, task string) (RunResult, error) {
	return a.runLoop(ctx, history, task, nil)
}

// ContinueStream resumes from an existing message history and streams the turn
// (tokens + tool traces) to sink, exactly like PromptStream does for a fresh
// prompt. Used by the chat path to thread prior conversation turns into a
// streamed reply.
func (a *Agent) ContinueStream(ctx context.Context, history []Message, task string, sink StreamSink) (RunResult, error) {
	return a.runLoop(ctx, history, task, sink)
}
