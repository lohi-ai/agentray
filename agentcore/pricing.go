package agentcore

import "strings"

// ModelPrice is the per-million-token price of a model, in USD. Providers quote
// input and output tokens separately, so we keep them separate and let the
// caller weigh them by the run's actual usage. Cache-read/write rates are
// optional: when left zero they derive from InputPerM by the standard provider
// multipliers (a cache hit is far cheaper than fresh input; an Anthropic cache
// write carries a small premium), so the default table stays terse while cache
// cost is still priced honestly.
type ModelPrice struct {
	InputPerM  float64 `json:"input_per_m"`  // USD per 1M input tokens
	OutputPerM float64 `json:"output_per_m"` // USD per 1M output tokens
	// CacheReadPerM / CacheWritePerM are optional explicit cache rates; zero means
	// "derive from InputPerM" (see Cost).
	CacheReadPerM  float64 `json:"cache_read_per_m,omitempty"`
	CacheWritePerM float64 `json:"cache_write_per_m,omitempty"`
}

// Default cache multipliers applied to InputPerM when a model has no explicit
// cache rate. A cache hit bills at a fraction of fresh input; an Anthropic cache
// write bills at a small premium over it. List prices drift, so these are honest
// defaults, not guarantees — override per model via CacheReadPerM/CacheWritePerM.
const (
	defaultCacheReadMultiplier  = 0.10
	defaultCacheWriteMultiplier = 1.25
)

// Pricing maps a model name to its price. Lookup is exact first, then by longest
// matching prefix, so a table keyed on a family ("gpt-4o", "claude-3-5-sonnet")
// still prices a dated or suffixed variant ("gpt-4o-2024-08-06"). A model with
// no entry prices at zero — the cost field stays an honest 0 rather than a guess.
type Pricing map[string]ModelPrice

// Cost weighs a usage record against the model's price. Unknown model → 0.
// Cache-read and cache-write tokens are priced as their own categories (so a long
// run whose stable prefix is served from cache is billed at the discounted rate,
// not the full input rate); absent explicit cache rates they derive from InputPerM
// by the standard multipliers.
func (p Pricing) Cost(model string, u Usage) float64 {
	mp, ok := p.lookup(model)
	if !ok {
		return 0
	}
	readPerM := mp.CacheReadPerM
	if readPerM == 0 {
		readPerM = mp.InputPerM * defaultCacheReadMultiplier
	}
	writePerM := mp.CacheWritePerM
	if writePerM == 0 {
		writePerM = mp.InputPerM * defaultCacheWriteMultiplier
	}
	return float64(u.InputTokens)/1e6*mp.InputPerM +
		float64(u.OutputTokens)/1e6*mp.OutputPerM +
		float64(u.CacheReadTokens)/1e6*readPerM +
		float64(u.CacheWriteTokens)/1e6*writePerM
}

// lookup resolves a model to a price: exact match, else the longest key that is
// a prefix of the model name (case-insensitive).
func (p Pricing) lookup(model string) (ModelPrice, bool) {
	if mp, ok := p[model]; ok {
		return mp, true
	}
	name := strings.ToLower(strings.TrimSpace(model))
	var best string
	for key := range p {
		k := strings.ToLower(key)
		if strings.HasPrefix(name, k) && len(k) > len(best) {
			best = key
		}
	}
	if best == "" {
		return ModelPrice{}, false
	}
	return p[best], true
}

// DefaultPricing is the built-in price table (USD per 1M tokens), keyed by model
// family so dated variants resolve by prefix. These are list prices and drift;
// they are a code-defined default, overridable per Runner via WithPricing. Keep
// entries sorted by family for easy auditing.
func DefaultPricing() Pricing {
	return Pricing{
		// OpenAI
		"gpt-4o-mini":  {InputPerM: 0.15, OutputPerM: 0.60},
		"gpt-4o":       {InputPerM: 2.50, OutputPerM: 10.00},
		"gpt-4.1-mini": {InputPerM: 0.40, OutputPerM: 1.60},
		"gpt-4.1-nano": {InputPerM: 0.10, OutputPerM: 0.40},
		"gpt-4.1":      {InputPerM: 2.00, OutputPerM: 8.00},
		"o4-mini":      {InputPerM: 1.10, OutputPerM: 4.40},
		"o3-mini":      {InputPerM: 1.10, OutputPerM: 4.40},
		// Anthropic
		"claude-3-5-haiku":  {InputPerM: 0.80, OutputPerM: 4.00},
		"claude-3-5-sonnet": {InputPerM: 3.00, OutputPerM: 15.00},
		"claude-3-7-sonnet": {InputPerM: 3.00, OutputPerM: 15.00},
		"claude-haiku-4":    {InputPerM: 1.00, OutputPerM: 5.00},
		"claude-sonnet-4":   {InputPerM: 3.00, OutputPerM: 15.00},
		"claude-opus-4":     {InputPerM: 15.00, OutputPerM: 75.00},
		// Google Gemini (OpenAI-compat routers)
		"gemini-2.0-flash": {InputPerM: 0.10, OutputPerM: 0.40},
		"gemini-2.5-flash": {InputPerM: 0.30, OutputPerM: 2.50},
		"gemini-2.5-pro":   {InputPerM: 1.25, OutputPerM: 10.00},
	}
}
