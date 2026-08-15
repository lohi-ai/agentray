package agentcore

import (
	"context"
	"errors"
)

// Composition: turning a set of plugins into a runnable Agent.
//
// The plugins themselves do NOT live here — each is its own package under
// agentcore/plugins/ (loop, model, policy, session, spill, jobs, observe, …).
// That split is deliberate and load-bearing:
//
//   - This package stays a leaf. It knows the Plugin interface and the Registry
//     API, and nothing about which plugins exist. Adding a capability is a new
//     folder, never an edit here.
//   - A plugin is a normal package with a normal import. A plugin written
//     outside this repo is indistinguishable from a built-in one — same
//     interface, same Registry API, same standing.
//
// Go's import graph is what forces the shape: agentcore/plugins/* import
// agentcore, so agentcore cannot import them back. New(Config) therefore keeps
// its own applier (ApplyConfig below) rather than reaching for the plugin
// packages. Both write through the same exported Registry setters — the setter
// is the single implementation of every rule — and
// agentcore/plugins/preset carries a parity test proving
// Build(preset.Plugins(cfg)...) and New(cfg) produce the same agent.

// Build composes an Agent from plugins.
//
// Registration order does not affect behavior: seams are keyed and hooks are
// prioritized, so a caller may reorder the list freely. A plugin that fails
// aborts the whole composition — a half-built agent is never returned.
func Build(plugins ...Plugin) (*Agent, error) {
	r, err := BuildRegistry(plugins...)
	if err != nil {
		return nil, err
	}
	return r.build()
}

// BuildRegistry composes the registry WITHOUT building an Agent, so a caller can
// inspect the composition (Describe, Provider, Plugins) or Unload a plugin
// first. Call Agent() on the result to finish.
func BuildRegistry(plugins ...Plugin) (*Registry, error) {
	r := newRegistry()
	for _, p := range plugins {
		if p == nil {
			continue
		}
		if err := r.register(p); err != nil {
			return nil, err
		}
	}
	return r, nil
}

// Agent assembles the composed Agent from a registry.
func (r *Registry) Agent() (*Agent, error) { return r.build() }

// configPlugin applies a whole Config through the Registry API, so New(Config)
// is a plugin composition like any other rather than a second construction
// path with its own rules.
type configPlugin struct{ cfg Config }

func (configPlugin) Name() string { return "config" }

func (p configPlugin) Register(r *Registry) error { return r.ApplyConfig(p.cfg) }

// ApplyConfig writes an entire Config into the registry.
//
// It is the Config-shaped front door to the same setters the granular plugins
// use. Every rule (validation, defaults, seam claiming, undo) lives in the
// setter, so this function is pure field routing: if it and a plugin package
// ever disagree, the disagreement is about WHICH field, never about what the
// field means.
//
// Only non-zero fields are applied, so a Config leaves unclaimed the seams it
// says nothing about — which is what lets ApplyConfig be combined with extra
// plugins in one composition.
func (r *Registry) ApplyConfig(cfg Config) error {
	// --- spine ---
	if err := r.SetModel(cfg.Provider, cfg.Model); err != nil {
		return err
	}
	if err := setIf(cfg.Escalation != nil, func() error { return r.SetEscalation(cfg.Escalation) }); err != nil {
		return err
	}
	if err := setIf(cfg.ContextWindow > 0, func() error { return r.SetContextWindow(cfg.ContextWindow) }); err != nil {
		return err
	}
	if err := setIf(cfg.Retry != nil, func() error { return r.SetRetry(*orZero(cfg.Retry)) }); err != nil {
		return err
	}
	if err := setIf(cfg.RefreshKey != nil, func() error { return r.SetRefreshKey(cfg.RefreshKey) }); err != nil {
		return err
	}
	if err := setIf(cfg.MaxTokens != 0, func() error { return r.SetMaxTokens(cfg.MaxTokens) }); err != nil {
		return err
	}
	if err := setIf(cfg.ReasoningEffort != "", func() error { return r.SetReasoningEffort(cfg.ReasoningEffort) }); err != nil {
		return err
	}
	if err := setIf(cfg.OutputSchema != nil, func() error { return r.SetOutputSchema(cfg.OutputSchema) }); err != nil {
		return err
	}
	if err := setIf(cfg.PromptCacheKey != "", func() error {
		return r.SetPromptCache(cfg.PromptCacheKey, cfg.PromptCacheRetention)
	}); err != nil {
		return err
	}

	// --- identity ---
	// AgentDefinition holds slices, so it is not comparable; a definition-shaped
	// seam is claimed only when the caller actually authored one.
	if err := setIf(!cfg.Definition.IsZero(), func() error { return r.SetDefinition(cfg.Definition) }); err != nil {
		return err
	}
	if err := setIf(cfg.Limits != nil, func() error { return r.SetLimits(*orZero(cfg.Limits)) }); err != nil {
		return err
	}
	if err := setIf(cfg.Env != nil, func() error { return r.SetEnv(*orZero(cfg.Env)) }); err != nil {
		return err
	}

	// --- tools + governance ---
	if cfg.Tools != nil {
		r.AddTools(toolsOf(cfg.Tools)...)
	}
	if err := r.UsePolicy(cfg.Policy); err != nil {
		return err
	}
	r.AddHooks(PriorityDefault, cfg.Hooks)
	if err := setIf(cfg.Goal != "", func() error { return r.SetGoal(cfg.Goal) }); err != nil {
		return err
	}
	if err := setIf(cfg.BudgetGate != nil, func() error { return r.SetBudgetGate(cfg.BudgetGate) }); err != nil {
		return err
	}
	if err := setIf(cfg.StepGate != nil, func() error { return r.SetStepGate(cfg.StepGate) }); err != nil {
		return err
	}

	// --- durability + context ---
	if err := setIf(cfg.Session != nil && cfg.SessionID != "", func() error {
		return r.SetSession(cfg.Session, cfg.SessionID, cfg.ResumeSession)
	}); err != nil {
		return err
	}
	if err := setIf(len(cfg.SeedDisabledTools) > 0, func() error { return r.SetSeedDisabledTools(cfg.SeedDisabledTools) }); err != nil {
		return err
	}
	if err := setIf(cfg.Memory != nil, func() error { return r.SetMemory(cfg.Memory) }); err != nil {
		return err
	}
	if err := setIf(cfg.Compaction != nil, func() error { return r.SetCompaction(*orZero(cfg.Compaction)) }); err != nil {
		return err
	}
	if err := setIf(cfg.CompactionProvider != nil && cfg.CompactionModel != "", func() error {
		return r.SetCompactionModel(cfg.CompactionProvider, cfg.CompactionModel)
	}); err != nil {
		return err
	}
	if err := setIf(cfg.Compactor != nil, func() error { return r.SetCompactor(cfg.Compactor) }); err != nil {
		return err
	}

	// --- steering ---
	if err := setIf(cfg.GetSteeringMessages != nil, func() error { return r.SetSteeringSource(cfg.GetSteeringMessages) }); err != nil {
		return err
	}
	if err := setIf(cfg.GetFollowUpMessages != nil, func() error { return r.SetFollowUpSource(cfg.GetFollowUpMessages) }); err != nil {
		return err
	}
	if err := setIf(cfg.PrepareNextTurn != nil, func() error { return r.SetPrepareNextTurn(cfg.PrepareNextTurn) }); err != nil {
		return err
	}

	// --- extensions ---
	// Every remaining capability arrives as an ExtensionFactory. Config does not
	// know what any of them are, which is the point: adding one is a new package
	// under agentcore/plugins/, never a new field here.
	for _, f := range cfg.Extensions {
		if f != nil {
			r.AddExtension(f)
		}
	}
	return nil
}

// UsePolicy installs the permission gate AND the BeforeToolCall hook that
// enforces it, at PriorityGate so it is consulted before any consumer hook
// whatever order plugins were listed in.
//
// This pairing is the whole trust boundary, so it is one call: a composition
// cannot install the policy and forget the hook that reads it.
func (r *Registry) UsePolicy(p Policy) error {
	if p == nil {
		p = DenyAll{}
	}
	if err := r.SetPolicy(p); err != nil {
		return err
	}
	r.AddHooks(PriorityGate, Hooks{
		Before: []BeforeToolCall{func(ctx context.Context, call ToolCall) Decision { return p.Allow(ctx, call) }},
	})
	return nil
}

// setIf applies fn only when cond holds, so an unset Config field leaves its
// seam unclaimed rather than claiming it with a zero value.
func setIf(cond bool, fn func() error) error {
	if !cond {
		return nil
	}
	return fn()
}

// orZero dereferences an optional pointer field.
func orZero[T any](p *T) *T {
	if p == nil {
		var zero T
		return &zero
	}
	return p
}

// toolsOf flattens a ToolSet into a slice in registration order, so the model's
// advertised tool order survives composition unchanged.
func toolsOf(ts *ToolSet) []Tool {
	names := ts.Names()
	out := make([]Tool, 0, len(names))
	for _, name := range names {
		if t, ok := ts.Get(name); ok {
			out = append(out, t)
		}
	}
	return out
}

// build turns the composed registry into a runnable Agent.
func (r *Registry) build() (*Agent, error) {
	if r.driver == nil {
		// The loop is a seam, but an agent without one is not an agent. Default
		// rather than fail: every composition wants a driver, and only the rare
		// one wants a different driver.
		r.driver = DefaultDriver()
	}
	if r.provider == nil {
		return nil, errors.New("agentcore: provider is required")
	}
	if r.model == "" {
		return nil, errors.New("agentcore: model is required")
	}

	// Provider decorators are applied once, here, over every rung the run can
	// reach. Doing it at compose time rather than in the loop is what keeps the
	// loop unaware that decoration exists at all.
	a := &Agent{
		extensions:         r.extensions,
		driver:             r.driver,
		provider:           wrapProvider(r.providerWrappers, r.provider),
		model:              r.model,
		tools:              r.tools,
		policy:             r.policy,
		hooks:              r.mergedHooks(),
		memory:             r.memory,
		def:                r.definition,
		limits:             r.limits,
		env:                r.resolvedEnv(),
		compaction:         r.compaction,
		compactor:          r.compactor,
		compactionProvider: wrapProvider(r.providerWrappers, r.compactionRung.Provider),
		compactionModel:    r.compactionRung.Model,
		refreshKey:         r.refreshKey,
		escalation:         wrapRungs(r.providerWrappers, r.escalation),
		contextWindow:      r.contextWindow,
		getSteering:        r.getSteering,
		getFollowUp:        r.getFollowUp,
		goal:               r.goal,
		prepareNextTurn:    r.prepareNextTurn,
		budgetGate:         r.budgetGate,
		session:            r.session,
		sessionID:          r.sessionID,
		resumeSession:      r.promptResume,
		stepGate:           r.stepGate,
		retry:              r.retry,
		cacheKey:           r.cacheKey,
		cacheRetention:     r.cacheRetention,
		seedDisabledTools:  r.seedDisabled,
		maxTokens:          r.maxTokens,
		reasoningEffort:    r.reasoningEffort,
		outputSchema:       r.outputSchema,
	}
	return a, nil
}

// wrapProvider applies every contributed decorator to one provider, in
// registration order: the first-registered wrapper ends up innermost.
func wrapProvider(ws []ProviderWrapper, p LLMProvider) LLMProvider {
	if p == nil {
		return nil
	}
	for _, w := range ws {
		if w != nil {
			p = w(p)
		}
	}
	return p
}

// wrapRungs decorates the escalation ladder, so a run that escalates does not
// silently stop being traced, priced, or rate limited.
func wrapRungs(ws []ProviderWrapper, rungs []ModelRung) []ModelRung {
	if len(ws) == 0 || len(rungs) == 0 {
		return rungs
	}
	out := make([]ModelRung, len(rungs))
	copy(out, rungs)
	for i := range out {
		out[i].Provider = wrapProvider(ws, out[i].Provider)
	}
	return out
}
