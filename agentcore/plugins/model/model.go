// Package model installs which model answers a turn: the primary
// provider/model, the escalation ladder walked when a rung errors, the
// same-model retry policy, the per-turn key refresh for rotating BYO
// credentials, and the decoding knobs.
package model

import (
	"context"

	"github.com/lohi-ai/agentray/agentcore"
)

// Plugin installs the model spine. Provider and Model are required.
type Plugin struct {
	Provider agentcore.LLMProvider
	Model    string
	// Escalation is the ordered fallback ladder tried when the primary rung
	// fails retryably. The run sticks with the working rung for later turns.
	Escalation []agentcore.ModelRung
	// Retry bounds the same-model backoff before escalating, so a brief 429
	// does not jump straight to a pricier rung.
	Retry *agentcore.RetryPolicy
	// RefreshKey re-resolves the provider's API key before each turn, so an
	// expiring BYO token does not kill a long run.
	RefreshKey func(ctx context.Context, provider string) (string, error)
	// MaxTokens caps output tokens per turn (0 = the provider's own default,
	// which can truncate large artifacts with stop_reason "length").
	MaxTokens int
	// ReasoningEffort ("low" | "medium" | "high") is passed to providers that
	// have the knob and ignored by those that do not.
	ReasoningEffort string
	// OutputSchema constrains every text answer to a JSON Schema at the
	// provider. For verdict-shaped agents, not general chat.
	OutputSchema *agentcore.OutputSchema
	// PromptCacheKey opts every call in the run into prompt caching;
	// CacheRetention hints the window ("" | "short" | "long" | "24h").
	PromptCacheKey string
	CacheRetention string
}

// Name identifies the plugin.
func (Plugin) Name() string { return "model" }

// Register claims the model seam and the decoding knobs it owns.
func (p Plugin) Register(r *agentcore.Registry) error {
	if err := r.SetModel(p.Provider, p.Model); err != nil {
		return err
	}
	for _, set := range []func() error{
		func() error { return apply(p.Escalation != nil, func() error { return r.SetEscalation(p.Escalation) }) },
		func() error {
			return apply(p.Retry != nil, func() error { return r.SetRetry(*p.Retry) })
		},
		func() error { return apply(p.RefreshKey != nil, func() error { return r.SetRefreshKey(p.RefreshKey) }) },
		func() error { return apply(p.MaxTokens != 0, func() error { return r.SetMaxTokens(p.MaxTokens) }) },
		func() error {
			return apply(p.ReasoningEffort != "", func() error { return r.SetReasoningEffort(p.ReasoningEffort) })
		},
		func() error {
			return apply(p.OutputSchema != nil, func() error { return r.SetOutputSchema(p.OutputSchema) })
		},
		func() error {
			return apply(p.PromptCacheKey != "", func() error { return r.SetPromptCache(p.PromptCacheKey, p.CacheRetention) })
		},
	} {
		if err := set(); err != nil {
			return err
		}
	}
	return nil
}

// apply runs fn only when the field was actually set, so an unset knob leaves
// its seam free for another plugin to own.
func apply(cond bool, fn func() error) error {
	if !cond {
		return nil
	}
	return fn()
}
