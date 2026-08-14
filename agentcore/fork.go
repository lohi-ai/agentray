package agentcore

// Forking is core because it is the ONE thing a delegation plugin cannot build
// from the outside: an Agent's capability-bearing fields are unexported, and
// they must stay that way — a child assembled field-by-field from a package
// that cannot see them would inherit whatever the author remembered, which is
// exactly how a child ends up with more scope than its parent.
//
// So the core answers "give me a child of yourself", and a plugin decides
// whether, when, and to whom to delegate. Core owns the inheritance rule;
// the plugin owns the policy.

// Fork builds an ephemeral child Agent for one delegated task.
//
// The child inherits every capability-bearing field verbatim — provider,
// ladder, tools, policy (including the permission gate already installed in
// hooks), memory, definition, limits, env, compaction, retry, caching, output
// cap, and the parent's extensions — so scope can only NARROW, never widen.
//
// It deliberately drops the run-control seams: no durable session, no
// steering/follow-up queues, no step gate, no PrepareNextTurn. A child is one
// bounded task, not a conversation.
//
// childSessionID, when non-empty AND the parent is durable, gives the child a
// durable log of its own and marks it resumable. Callers derive that id
// deterministically from the spawn (parent session + tool call id) so a
// replayed spawn REATTACHES — a completed child returns its recorded answer
// without re-running, and an interrupted one resumes from its own log —
// instead of duplicating the spend and the side effects. Empty leaves the
// child's history purely in memory.
//
// Depth is not a field: it rides the context (see WithDelegationDepth), so a
// cap holds across an A -> B -> A cycle the same as a straight chain.
func (a *Agent) Fork(childSessionID string) *Agent {
	child := &Agent{
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
		compactor:          a.compactor,
		refreshKey:         a.refreshKey,
		escalation:         a.escalation,
		retry:              a.retry,
		cacheKey:           a.cacheKey,
		cacheRetention:     a.cacheRetention,
		maxTokens:          a.maxTokens,
		reasoningEffort:    a.reasoningEffort,
		extensions:         a.extensions,
	}
	if childSessionID != "" && a.session != nil && a.sessionID != "" {
		child.session = a.session
		child.sessionID = childSessionID
		child.resumeSession = true
	}
	return child
}

// IsDurable reports whether this agent writes an append-only session log, which
// is what makes a deterministic child-session id worth deriving.
func (a *Agent) IsDurable() bool { return a.session != nil && a.sessionID != "" }

// SessionID returns the durable log this agent writes to, or "" when the run is
// purely in-memory.
func (a *Agent) SessionID() string { return a.sessionID }

// AddChildUsage folds a delegated run's usage into this agent's accumulator, so
// the parent's RunResult and its budget gate both account for what its children
// spent. A delegation plugin calls this for every child it drives — including
// one that FAILED, whose tokens were still burned.
func (a *Agent) AddChildUsage(u Usage) { a.addChildUsage(u) }
