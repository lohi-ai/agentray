// Package preset composes agentcore's default agent out of the plugin
// packages.
//
// This is the "build agentcore from its own plugins" step made literal:
// Plugins(cfg) returns the plugin list that reproduces agentcore.New(cfg), and
// preset_test.go proves the two agree field by field. If the plugin surface
// ever drifts from Config, that test fails.
//
// Full(cfg, opts) is that same list plus the capabilities Config has no field
// for — spill, jobs, the repeat guard, session retrieval, the observers — and
// is what a real deployment composes.
//
// Use either as the starting point for a custom agent. Take the list, drop or
// replace the entries you want to change, append your own, and hand the result
// to agentcore.Build:
//
//	ps := preset.Full(cfg, preset.Options{Spill: myStore})
//	ps = append(ps, myDriverPlugin{}, myCapability{})  // e.g. r.SetDriver(...)
//	agent, err := agentcore.Build(ps...)
//
// Control flow needs no entry here: Build installs agentcore.DefaultDriver
// when no plugin claims the driver seam, and a composition that wants a
// different loop registers a plugin whose Register calls r.SetDriver.
package preset

import (
	"github.com/lohi-ai/agentray/agentcore"
	"github.com/lohi-ai/agentray/agentcore/plugins/budget"
	"github.com/lohi-ai/agentray/agentcore/plugins/compaction"
	"github.com/lohi-ai/agentray/agentcore/plugins/definition"
	"github.com/lohi-ai/agentray/agentcore/plugins/goal"
	"github.com/lohi-ai/agentray/agentcore/plugins/hooks"
	"github.com/lohi-ai/agentray/agentcore/plugins/jobs"
	"github.com/lohi-ai/agentray/agentcore/plugins/memory"
	"github.com/lohi-ai/agentray/agentcore/plugins/model"
	"github.com/lohi-ai/agentray/agentcore/plugins/observe"
	"github.com/lohi-ai/agentray/agentcore/plugins/policy"
	"github.com/lohi-ai/agentray/agentcore/plugins/repeatguard"
	"github.com/lohi-ai/agentray/agentcore/plugins/session"
	"github.com/lohi-ai/agentray/agentcore/plugins/sessionquery"
	"github.com/lohi-ai/agentray/agentcore/plugins/spill"
	"github.com/lohi-ai/agentray/agentcore/plugins/steering"
	"github.com/lohi-ai/agentray/agentcore/plugins/tools"
)

// Plugins returns the plugin set that reproduces agentcore.New(cfg).
//
// Every entry is a real plugin from its own package — there is no hidden core
// doing the work behind them.
func Plugins(cfg agentcore.Config) []agentcore.Plugin {
	list := []agentcore.Plugin{
		// spine — the driver seam is left unclaimed; Build defaults it to the
		// built-in reason→act loop, and a custom driver claims it via SetDriver.
		model.Plugin{
			Provider:        cfg.Provider,
			Model:           cfg.Model,
			ContextWindow:   cfg.ContextWindow,
			Escalation:      cfg.Escalation,
			Retry:           cfg.Retry,
			RefreshKey:      cfg.RefreshKey,
			MaxTokens:       cfg.MaxTokens,
			ReasoningEffort: cfg.ReasoningEffort,
			OutputSchema:    cfg.OutputSchema,
			PromptCacheKey:  cfg.PromptCacheKey,
			CacheRetention:  cfg.PromptCacheRetention,
		},
		definition.Plugin{Definition: cfg.Definition, Limits: cfg.Limits, Env: cfg.Env},

		// tools + governance
		tools.FromSet(cfg.Tools),
		policy.Plugin{Policy: cfg.Policy},
		hooks.Of(cfg.Hooks),
		goal.Plugin{Goal: cfg.Goal},
		budget.Plugin{Gate: cfg.BudgetGate, Step: cfg.StepGate},

		// durability + context
		session.Plugin{
			Store:             cfg.Session,
			ID:                cfg.SessionID,
			Resume:            cfg.ResumeSession,
			SeedDisabledTools: cfg.SeedDisabledTools,
		},
		memory.Plugin{Store: cfg.Memory},
		compaction.Plugin{Settings: cfg.Compaction, Provider: cfg.CompactionProvider, Model: cfg.CompactionModel, Strategy: cfg.Compactor},

		// steering
		steering.Plugin{
			Steer:           cfg.GetSteeringMessages,
			FollowUp:        cfg.GetFollowUpMessages,
			PrepareNextTurn: cfg.PrepareNextTurn,
		},
	}

	// Extensions are pass-through: Config carries them already built (they are
	// values from the plugin packages), and every one is an agentcore.Plugin
	// too, so the composition is one flat list. Nothing here inspects what an
	// extension does — that is precisely the property the split bought.
	for _, f := range cfg.Extensions {
		if p, ok := f.(agentcore.Plugin); ok {
			list = append(list, p)
			continue
		}
		list = append(list, extensionPlugin{f})
	}
	return list
}

// Options configures the capabilities Full adds on top of Plugins. Every field
// is optional and every zero value degrades to "that capability is not
// installed" rather than to a broken one — a partially wired composition must
// lose a feature, never gain a silently wrong one.
type Options struct {
	// Spill persists an oversized tool result and hands the model a locator for
	// the rest. nil leaves spilling OFF (the loop's head+tail truncation stands),
	// which is the honest default: the locator is written to the durable session
	// log, so a store that cannot outlive the process would mint locators that a
	// resumed run reads back as not-found. Supply a durable store to turn it on.
	Spill spill.SpillStore
	// ReportLogInvariant receives each divergence between the live history and
	// the durable log — the "model-visible means logged" check that catches
	// resume corruption at the turn it is introduced rather than in a resumed run
	// weeks later. nil installs no detector. Advisory by design: it reports and
	// never alters the run, so an error-level log is the right sink in production.
	ReportLogInvariant func(observe.LogInvariantViolation)
	// Observe installs the run's outward watchers (turn boundaries, provider
	// calls, executed tool calls, run end). The zero value installs nothing, so a
	// composition can wire it unconditionally and let configuration decide what
	// is actually metered.
	Observe observe.Hooks
	// Jobs owns background work. nil uses a fresh in-process store per run —
	// correct for a single server, since a job cannot outlive the run that
	// started it anyway.
	Jobs jobs.JobStore
	// SessionQuery backs the session_query tool. nil searches the run's OWN
	// durable log, which needs no external index.
	SessionQuery sessionquery.SessionQuery
}

// Full is the default agent plus the capabilities that agentcore.Config has no
// field for: retrieval over the run's own history, background work, oversized
// output, the repeat-loop nudge, and the two observers.
//
// The split from Plugins is the point. Plugins is pinned to agentcore.New(cfg)
// parity — every entry there answers to a Config field, and preset_test proves
// the two cannot drift. The five below answer to no Config field at all; they
// are plugins in the sense the architecture means, added to a list. Full is
// where a real deployment starts; Plugins is where the parity proof lives.
//
// Everything here is still ejectable: preset.Without(preset.Full(cfg, o),
// "spill") is a complete agent minus spilling, with no trace of it left.
func Full(cfg agentcore.Config, o Options) []agentcore.Plugin {
	list := Plugins(cfg)
	if o.Spill != nil {
		list = append(list, spill.To(o.Spill))
	}
	list = append(list,
		jobs.Plugin{Store: o.Jobs},
		repeatguard.Default(),
		sessionquery.Plugin{Provider: o.SessionQuery},
		o.Observe,
	)
	if o.ReportLogInvariant != nil {
		list = append(list, observe.LogInvariant{Report: o.ReportLogInvariant})
	}
	return list
}

// extensionPlugin adapts a bare ExtensionFactory into a Plugin, for a consumer
// that implemented the run interface without also implementing Register.
type extensionPlugin struct{ f agentcore.ExtensionFactory }

func (p extensionPlugin) Name() string { return p.f.Name() }

func (p extensionPlugin) Register(r *agentcore.Registry) error {
	r.AddExtension(p.f)
	return nil
}

// New builds an agent from the default plugin set. It is agentcore.New spelled
// through the plugin packages.
func New(cfg agentcore.Config) (*agentcore.Agent, error) {
	return agentcore.Build(Plugins(cfg)...)
}

// Without removes the named plugins from a list, so a caller can replace one
// without rebuilding the rest by hand. Names that are not present are ignored —
// dropping something already absent is not an error.
func Without(list []agentcore.Plugin, names ...string) []agentcore.Plugin {
	drop := make(map[string]bool, len(names))
	for _, n := range names {
		drop[n] = true
	}
	out := make([]agentcore.Plugin, 0, len(list))
	for _, p := range list {
		if !drop[p.Name()] {
			out = append(out, p)
		}
	}
	return out
}

// Replace swaps the plugin with the same name as p, appending it when no such
// plugin is present. Use it to change one seam and keep the rest of a
// composition intact.
func Replace(list []agentcore.Plugin, p agentcore.Plugin) []agentcore.Plugin {
	out := Without(list, p.Name())
	return append(out, p)
}
