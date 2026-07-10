package agentcore

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
)

// ToolSpawnSubagent is the stable name of the built-in delegation tool
// (ARCHITECT-AGENT-TEAM P1): the model forks an ephemeral child agent for one
// self-contained task and receives only its final answer, keeping the child's
// exploration (tool churn, dead ends, large intermediate results) out of the
// parent's context window.
const ToolSpawnSubagent = "spawn_subagent"

// Sub-agent defaults (ARCHITECT-AGENT-TEAM suggested caps).
const (
	defaultSubagentMaxDepth       = 1         // only the top-level agent may spawn
	defaultSubagentMaxPerRun      = 8         // children per parent run
	defaultSubagentMaxOutputBytes = 48 * 1024 // model-visible answer per child
)

// SubagentSettings caps the delegation surface. A child inherits the parent's
// provider, model ladder, tools, policy, hooks, memory, and definition — it can
// never widen access — and runs with isolated history, so only its final answer
// (truncated to MaxOutputBytes) returns to the parent.
type SubagentSettings struct {
	// MaxDepth is how many nesting levels may spawn: 1 (the default) lets the
	// top-level agent spawn children but forbids grandchildren.
	MaxDepth int
	// MaxPerRun caps how many children one run may spawn in total.
	MaxPerRun int
	// MaxOutputBytes caps the child answer surfaced to the parent model.
	MaxOutputBytes int
}

// normalized fills zero fields with the defaults.
func (s SubagentSettings) normalized() SubagentSettings {
	if s.MaxDepth <= 0 {
		s.MaxDepth = defaultSubagentMaxDepth
	}
	if s.MaxPerRun <= 0 {
		s.MaxPerRun = defaultSubagentMaxPerRun
	}
	if s.MaxOutputBytes <= 0 {
		s.MaxOutputBytes = defaultSubagentMaxOutputBytes
	}
	return s
}

// Delegate is one named other agent this agent may hand a task to (cross-agent
// delegation). agentcore never builds the target itself: Run is an opaque
// closure the consumer injects, executing the target agent under its own
// identity — its persona, tools, policy, and secrets, not the caller's. The
// closure receives the caller's ctx (so cancelling the parent cancels the
// delegate, and the delegation depth carried on ctx caps recursion across
// agents) and an optional sink for live tool-activity notes; it returns the
// target's final answer plus its token usage for parent-run accounting.
type Delegate struct {
	// Name is the stable identifier the model selects with (the target agent's
	// human name). Matched case-insensitively.
	Name string
	// Description is a one-line hint helping the model pick the right teammate.
	Description string
	// Run executes the delegated task on the target agent.
	Run func(ctx context.Context, task string, sink StreamSink) (string, Usage, error)
}

// delegationDepthKey carries how many delegation hops deep the current run is.
// Depth travels on ctx — not on the Agent — because a cross-agent delegate is a
// freshly built Agent whose struct fields know nothing of the caller; ctx is
// the only thread that survives the hop, so it is what stops A→B→A recursion.
type delegationDepthKey struct{}

// DelegationDepth returns the current delegation depth (0 for a top-level run).
func DelegationDepth(ctx context.Context) int {
	if v, ok := ctx.Value(delegationDepthKey{}).(int); ok {
		return v
	}
	return 0
}

// WithDelegationDepth returns ctx marked as depth hops deep. Consumers normally
// never call this — the spawn tool wraps the child ctx itself — but a consumer
// embedding a run inside another delegation system may seed a floor.
func WithDelegationDepth(ctx context.Context, depth int) context.Context {
	return context.WithValue(ctx, delegationDepthKey{}, depth)
}

// fork builds the ephemeral child Agent for one delegated task. The child
// inherits every capability-bearing field verbatim (provider, ladder, tools,
// policy — including the permission gate already installed in hooks — memory,
// definition, limits, env, compaction, retry, caching, output cap) so scope can
// only narrow, never widen. It deliberately drops the run-control seams: no
// durable session (isolated history), no steering/follow-up queues, no step
// gate, no PrepareNextTurn — a child is one bounded task, not a conversation.
// Depth is not a field: it rides the ctx (see DelegationDepth), so the loop
// stops advertising spawn_subagent past MaxDepth for forks and cross-agent
// delegates alike.
func (a *Agent) fork() *Agent {
	return &Agent{
		provider:           a.provider,
		model:              a.model,
		tools:              a.tools,
		policy:             a.policy,
		hooks:              a.hooks,
		memory:             a.memory,
		def:                a.def,
		limits:             a.limits,
		env:                a.env,
		compaction:         a.compaction,
		compactionProvider: a.compactionProvider,
		compactionModel:    a.compactionModel,
		refreshKey:         a.refreshKey,
		escalation:         a.escalation,
		retry:              a.retry,
		cacheKey:           a.cacheKey,
		cacheRetention:     a.cacheRetention,
		maxTokens:          a.maxTokens,
		reasoningEffort:    a.reasoningEffort,
		subagents:          a.subagents,
		delegates:          a.delegates,
	}
}

// withSubagent returns a copy of base with the built-in spawn_subagent tool
// added, leaving base untouched (same pattern as withReadSkill). The tool is
// constructed per run so its spawn counter is naturally run-scoped.
func withSubagent(base *ToolSet, parent *Agent) *ToolSet {
	clone := &ToolSet{
		order: append([]string{}, base.order...),
		byKey: make(map[string]Tool, len(base.byKey)+1),
	}
	for k, v := range base.byKey {
		clone.byKey[k] = v
	}
	clone.Add(&subagentTool{parent: parent, settings: parent.subagents.normalized()})
	return clone
}

// subagentTool is the model-facing delegation tool. It is subject to the normal
// permission gate (unlike read_skill): the consumer must both enable
// Config.Subagents and permit spawn_subagent in its policy.
type subagentTool struct {
	parent   *Agent
	settings SubagentSettings
	spawned  int32 // atomic; children spawned this run
}

func (t *subagentTool) Name() string { return ToolSpawnSubagent }

// Parallel opts spawn calls into concurrent batch execution: each child is a
// fresh Agent instance driving its own isolated history, so a fan-out turn
// ("spawn three children, then synthesize") runs them concurrently.
func (t *subagentTool) Parallel() bool { return true }

func (t *subagentTool) Schema() ToolSchema {
	props := map[string]any{
		"task": map[string]any{
			"type":        "string",
			"description": "The complete, self-contained task for the sub-agent, including what to return as its final answer.",
		},
		"context": map[string]any{
			"type":        "string",
			"description": "Optional background the sub-agent needs (identifiers, constraints, prior findings). It sees nothing else from this conversation.",
		},
	}
	desc := "Delegate one self-contained task to an ephemeral sub-agent and get back only its final answer. " +
		"The sub-agent has the same tools and permissions as you but a fresh, isolated context — its intermediate work never enters yours. " +
		"Use it for exploration or noisy multi-step work whose details you don't need (research a question, scan data broadly, produce an artifact), " +
		"NOT for quick single-tool lookups you can do yourself. State the task fully and self-contained: the sub-agent sees nothing of this conversation " +
		"except what you put in task and context."
	if roster := t.parent.delegates; len(roster) > 0 {
		var lines []string
		for _, d := range roster {
			line := d.Name
			if d.Description != "" {
				line += " — " + d.Description
			}
			lines = append(lines, line)
		}
		props["agent"] = map[string]any{
			"type": "string",
			"description": "Which agent runs the task. Omit (or \"self\") to fork a clone of yourself. " +
				"Available teammates (each runs under its OWN persona, tools, and permissions — pick the one whose specialty matches the task): " +
				strings.Join(lines, "; "),
		}
		desc += " You may also route the task to a named teammate agent via the agent parameter."
	}
	return ToolSchema{
		Name:        ToolSpawnSubagent,
		Description: desc,
		Parameters: map[string]any{
			"type":       "object",
			"properties": props,
			"required":   []string{"task"},
		},
	}
}

// subagentArgs is the decoded argument shape.
type subagentArgs struct {
	Task    string `json:"task"`
	Context string `json:"context"`
	Agent   string `json:"agent"`
}

func (t *subagentTool) Run(ctx context.Context, args string) (string, error) {
	return t.RunStreaming(ctx, args, nil)
}

// RunStreaming spawns and drives one child run — a fork of the parent by
// default, or a named delegate agent when the agent argument selects one. When
// emit is set (a streamed parent run), the child's tool activity is forwarded
// as brief partial notes so a live viewer (chat, Lab) sees the delegation
// working rather than a silent long call. The child shares the parent's ctx,
// so cancelling the parent run cancels in-flight children (team-architecture
// safety rule), and the ctx carries the incremented delegation depth so a
// delegate cannot re-delegate past MaxDepth (A→B→A recursion stops).
func (t *subagentTool) RunStreaming(ctx context.Context, args string, emit func(partial string)) (string, error) {
	var in subagentArgs
	if err := json.Unmarshal([]byte(args), &in); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	task := strings.TrimSpace(in.Task)
	if task == "" {
		return "", fmt.Errorf("task is required")
	}
	// Defense in depth: the loop already stops advertising the tool at MaxDepth,
	// but the ctx check also protects consumers that seed a depth floor.
	depth := DelegationDepth(ctx)
	if depth >= t.settings.MaxDepth {
		return "", fmt.Errorf("delegation depth exhausted (max %d) — finish the work yourself", t.settings.MaxDepth)
	}
	if n := atomic.AddInt32(&t.spawned, 1); int(n) > t.settings.MaxPerRun {
		return "", fmt.Errorf("sub-agent budget exhausted (%d per run) — finish the remaining work yourself", t.settings.MaxPerRun)
	}
	ctx = WithDelegationDepth(ctx, depth+1)

	prompt := task
	if c := strings.TrimSpace(in.Context); c != "" {
		prompt = task + "\n\nContext:\n" + c
	}

	// Resolve the target: self-fork by default, a granted teammate when named.
	label := "sub-agent"
	var delegate *Delegate
	if who := strings.TrimSpace(in.Agent); who != "" && !strings.EqualFold(who, "self") {
		for i := range t.parent.delegates {
			if strings.EqualFold(t.parent.delegates[i].Name, who) {
				delegate = &t.parent.delegates[i]
				break
			}
		}
		if delegate == nil {
			return "", fmt.Errorf("unknown agent %q — use \"self\" or one of the teammates listed in the tool description", who)
		}
		label = delegate.Name
	}

	// Forward the child's tool activity as partial notes on a streamed run.
	var sink StreamSink
	if emit != nil {
		sink = func(ev StreamEvent) {
			if ev.Type == StreamToolExecStart && ev.Tool != nil {
				emit(fmt.Sprintf("[%s] running %s", label, ev.Tool.Tool))
			}
		}
	}

	var final string
	var err error
	if delegate != nil {
		var usage Usage
		final, usage, err = delegate.Run(ctx, prompt, sink)
		// Fold the delegate's spend into the parent run before handling the
		// error, so even a failed delegate's tokens/cost are accounted.
		t.parent.addChildUsage(usage)
		if err != nil {
			return "", fmt.Errorf("agent %s failed: %w", delegate.Name, err)
		}
	} else {
		child := t.parent.fork()
		var res RunResult
		res, err = child.runLoop(ctx, []Message{{Role: RoleUser, Content: prompt}}, task, sink)
		// Fold the child's spend before handling the error (a child's own
		// children are already folded into res.Usage by its runLoop, recursively).
		t.parent.addChildUsage(res.Usage)
		if err != nil {
			return "", fmt.Errorf("sub-agent failed: %w", err)
		}
		final = res.Final
		if strings.TrimSpace(final) == "" {
			return "", fmt.Errorf("sub-agent produced no answer (stop reason: %s)", res.StopReason)
		}
	}
	final = strings.TrimSpace(final)
	if final == "" {
		return "", fmt.Errorf("agent %s produced no answer", label)
	}
	return truncateMiddle(final, t.settings.MaxOutputBytes), nil
}
