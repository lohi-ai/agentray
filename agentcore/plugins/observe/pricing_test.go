package observe

import (
	"testing"

	"github.com/lohi-ai/agentray/agentcore"
)

// TestPricingChargesCacheTokensSeparately verifies cache reads bill at the
// discounted rate and cache writes at the premium, instead of the full input
// rate — the whole point of the long-session caching steal.
func TestPricingChargesCacheTokensSeparately(t *testing.T) {
	p := Pricing{"m": {InputPerM: 10, OutputPerM: 30}}

	full := p.Cost("m", agentcore.Usage{InputTokens: 1_000_000})
	if full != 10 {
		t.Fatalf("full input cost = %v, want 10", full)
	}
	// A million cache-read tokens must cost a fraction of a million fresh ones.
	read := p.Cost("m", agentcore.Usage{CacheReadTokens: 1_000_000})
	if read != 10*defaultCacheReadMultiplier {
		t.Fatalf("cache-read cost = %v, want %v", read, 10*defaultCacheReadMultiplier)
	}
	// Cache writes carry a small premium over fresh input.
	write := p.Cost("m", agentcore.Usage{CacheWriteTokens: 1_000_000})
	if write != 10*defaultCacheWriteMultiplier {
		t.Fatalf("cache-write cost = %v, want %v", write, 10*defaultCacheWriteMultiplier)
	}
	// Explicit cache rates override the derived defaults.
	pe := Pricing{"m": {InputPerM: 10, OutputPerM: 30, CacheReadPerM: 0.5}}
	if got := pe.Cost("m", agentcore.Usage{CacheReadTokens: 1_000_000}); got != 0.5 {
		t.Fatalf("explicit cache-read cost = %v, want 0.5", got)
	}
}
