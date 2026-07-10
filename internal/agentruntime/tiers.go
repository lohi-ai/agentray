package agentruntime

import (
	"strings"

	"github.com/lohi-ai/agentray/internal/storage"
)

// Tier classifies how much model capability a unit of agent work needs. The
// workspace maps each tier to a concrete provider+model (per-tier BYO key,
// configured once for the whole workspace); each agent maps each task kind
// (triage/run/compaction/reflection) to a tier. The two layers are independent:
// the workspace owns the model pool, the agent owns which work draws from which
// tier of that pool.
type Tier string

const (
	TierLite  Tier = "lite"  // cheap, mechanical steps
	TierFlash Tier = "flash" // balanced default
	TierPro   Tier = "pro"   // deep reasoning
)

// orderedTiers is low→high; escalation always walks upward from the start tier.
var orderedTiers = []Tier{TierLite, TierFlash, TierPro}

var tierRank = map[Tier]int{TierLite: 0, TierFlash: 1, TierPro: 2}

// TierFromName maps a stored task→tier value ("lite"/"flash"/"pro") to a Tier,
// defaulting to flash for an empty or unrecognized name. The per-agent task map
// (storage.AgentTaskTiers) is the source of truth for which tier each task runs
// on; this is the adapter from its string values to the Tier type.
func TierFromName(name string) Tier {
	switch Tier(name) {
	case TierLite:
		return TierLite
	case TierPro:
		return TierPro
	default:
		return TierFlash
	}
}

// tiersAbove returns the tiers strictly higher than start, ascending — the
// escalation tail for a run that begins at start.
func tiersAbove(start Tier) []Tier {
	var out []Tier
	for _, t := range orderedTiers {
		if tierRank[t] > tierRank[start] {
			out = append(out, t)
		}
	}
	return out
}

// TierConfig is one tier's provider settings. APIKey is decrypted at call time
// and never persisted from here. Per-tier fields are overrides: a blank field
// inherits the flash default at resolution time, so the common "one provider +
// key, different model per tier" setup needs the key entered only once.
type TierConfig struct {
	Provider string
	Model    string
	BaseURL  string
	APIKey   string
}

// TierSet is the per-project tier→config mapping. flash is the always-present
// default whose fields every other tier inherits unless overridden.
type TierSet map[Tier]TierConfig

// resolve returns the effective config for a tier by merging its non-empty
// overrides over the flash default. A tier with no overrides resolves to exactly
// flash (and so dedups out of an escalation ladder).
func (ts TierSet) resolve(tier Tier) TierConfig {
	flash := ts[TierFlash]
	c, ok := ts[tier]
	if !ok || tier == TierFlash {
		return flash
	}
	out := flash
	if strings.TrimSpace(c.Provider) != "" {
		out.Provider = c.Provider
	}
	if strings.TrimSpace(c.Model) != "" {
		out.Model = c.Model
	}
	if strings.TrimSpace(c.BaseURL) != "" {
		out.BaseURL = c.BaseURL
	}
	if c.APIKey != "" {
		out.APIKey = c.APIKey
	}
	return out
}

// Resolve returns the effective config for a tier by merging its overrides over
// flash. Exported for callers that need one resolved tier outside runner.go.
func (ts TierSet) Resolve(tier Tier) TierConfig { return ts.resolve(tier) }

// TierSetFromWorkspace assembles a TierSet from the workspace model tier pool and
// decrypted per-tier keys. Exported for non-run paths that still need to resolve a
// tier, such as authoring helpers.
func TierSetFromWorkspace(cfg storage.WorkspaceModelTiers, keys map[string]string) TierSet {
	return tierSetFromWorkspace(cfg, keys)
}

// ResolveTierName maps a stored tier label (lite/flash/pro) to the runtime Tier.
func ResolveTierName(name string) Tier { return TierFromName(name) }

// DefaultAuthoringTier is the workspace tier used for authoring helpers that are
// intentionally Pro-only in the first pass.
const DefaultAuthoringTier = TierPro

// ladder returns the start tier's effective config plus, when fallback is on, the
// effective configs of every higher tier — deduplicated so an unconfigured tier
// that resolves back to flash doesn't add a redundant rung.
func (ts TierSet) ladder(start Tier, fallback bool) []TierConfig {
	out := []TierConfig{ts.resolve(start)}
	if !fallback {
		return out
	}
	seen := map[TierConfig]bool{out[0]: true}
	for _, t := range tiersAbove(start) {
		c := ts.resolve(t)
		if !seen[c] {
			seen[c] = true
			out = append(out, c)
		}
	}
	return out
}
