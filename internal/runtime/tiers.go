package agentruntime

import (
	"fmt"
	"strings"

	"github.com/lohi-ai/agentray/agentcore"
	"github.com/lohi-ai/agentray/agentcore/plugins/observe"
	"github.com/lohi-ai/agentray/ai"
	"github.com/lohi-ai/agentray/internal/dataplane/store"
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

// TierConfig is one tier's provider settings. APIKey is decrypted at call time
// and never persisted from here. Per-tier fields are overrides: a blank field
// inherits the flash default at resolution time, so the common "one provider +
// key, different model per tier" setup needs the key entered only once.
type TierConfig struct {
	Provider string
	Model    string
	BaseURL  string
	APIKey   string
	// ProviderID is the workspace_providers row this tier draws from ("" for
	// legacy provider-string tiers and the hosted default). OAuth vendors need
	// it: their credential is the account pool hanging off that row.
	ProviderID string
	// TokenSource, when set, is the OAuth account pool the wire client pulls a
	// live access token from per request. Nil for API-key vendors.
	TokenSource ai.TokenSource
	// ContextWindow is the operator's override for this model's input window in
	// tokens, capping the compaction budget. 0 means "work it out", and
	// EffectiveContextWindow does. An override exists because no catalog can
	// know a self-hosted endpoint's window, and being wrong high there means the
	// run dies at the provider rather than compacting.
	ContextWindow int
	// FallbackModel is an optional second model on the SAME provider the run
	// retries on when the primary model fails. It is the tier's whole fallback
	// mechanism — there is no cross-tier escalation.
	FallbackModel string
}

// EffectiveContextWindow is the window actually applied to a tier: the
// operator's override when they set one, otherwise whatever the ai package can
// determine from the model id, otherwise 0 for "unknown" — which leaves the
// configured compaction budget to stand alone.
func EffectiveContextWindow(tc TierConfig) int {
	if tc.ContextWindow > 0 {
		return tc.ContextWindow
	}
	return ai.ContextWindowFor(tc.Provider, tc.Model)
}

// ModelTier is a tier fully resolved for use: the primary model plus, when the
// workspace's model_fallback switch is on and one is configured, a fallback
// model of the same provider. It is the only thing a consumer of the tier pool
// sees — callers ask a TierSet for a ModelTier and never touch TierConfig
// merging, provider rows, or fallback rules themselves.
type ModelTier struct {
	TierConfig
}

// EffectiveWindow is the input window applied to the primary model.
func (t ModelTier) EffectiveWindow() int { return EffectiveContextWindow(t.TierConfig) }

// RawProvider builds the tier's provider undecorated. The run loop needs the
// raw client because the composition's monitor plugin prices and traces every
// rung itself — wrapping here would double-count.
func (t ModelTier) RawProvider() (agentcore.LLMProvider, error) {
	return buildProvider(t.Provider, t.BaseURL, t.APIKey, t.TokenSource)
}

// TracedProvider builds the tier's provider wrapped for pricing + tracing —
// the shape one-shot callers (advisor, reflection, triage, authoring) want,
// since they run outside the run composition's monitor plugin.
func (t ModelTier) TracedProvider(tracer observe.Sink) (agentcore.LLMProvider, error) {
	return buildTracedProvider(t.Provider, t.BaseURL, t.APIKey, t.TokenSource, tracer)
}

// Rungs builds the model ladder for a run on this tier: the primary model
// first, then the fallback model as a second rung of the same provider when
// one is configured. The fallback rung's window is derived from its own model
// id — the operator's ContextWindow override describes the primary model and
// must not leak onto a different one.
func (t ModelTier) Rungs() ([]agentcore.ModelRung, error) {
	prov, err := t.RawProvider()
	if err != nil {
		return nil, err
	}
	rungs := []agentcore.ModelRung{{
		Provider:      prov,
		Model:         t.Model,
		ContextWindow: t.EffectiveWindow(),
	}}
	if fb := strings.TrimSpace(t.FallbackModel); fb != "" && fb != t.Model {
		rungs = append(rungs, agentcore.ModelRung{
			Provider:      prov,
			Model:         fb,
			ContextWindow: ai.ContextWindowFor(t.Provider, fb),
		})
	}
	return rungs, nil
}

// TierSet is the workspace's tier pool resolved for one run: the per-tier
// configs plus the workspace's model_fallback switch. flash is the
// always-present default whose fields every other tier inherits unless
// overridden.
type TierSet struct {
	tiers    map[Tier]TierConfig
	fallback bool
}

// For resolves a task's tier into a ready-to-use ModelTier: flash inheritance
// applied, fallback model attached when the workspace switch allows it.
func (ts TierSet) For(tier Tier) ModelTier {
	tc := ts.resolve(tier)
	if !ts.fallback {
		tc.FallbackModel = ""
	}
	return ModelTier{TierConfig: tc}
}

// KeyFor returns the freshest API key configured for a provider name — the
// per-turn key-refresh seam, so a BYO key rotated mid-run is picked up without
// killing the run. Names are canonicalized through ai.NormalizeVendor, so a
// tier stored as "gemini" still matches a provider that reports Name()
// "google". It answers an error (never an empty string) when no tier matches:
// agentcore applies a returned key unconditionally, and "" would blank a
// still-valid key.
func (ts TierSet) KeyFor(provider string) (string, error) {
	want := ai.NormalizeVendor(provider)
	for _, tier := range []Tier{TierLite, TierFlash, TierPro} {
		tc := ts.resolve(tier)
		if ai.NormalizeVendor(tc.Provider) == want && tc.APIKey != "" {
			return tc.APIKey, nil
		}
	}
	return "", fmt.Errorf("agentruntime: no key for provider %q on key refresh", provider)
}

// resolve returns the effective config for a tier by merging its non-empty
// overrides over the flash default. A tier with no overrides resolves to
// exactly flash.
func (ts TierSet) resolve(tier Tier) TierConfig {
	flash := ts.tiers[TierFlash]
	c, ok := ts.tiers[tier]
	if !ok || tier == TierFlash {
		return flash
	}
	out := flash
	if strings.TrimSpace(c.Provider) != "" {
		out.Provider = c.Provider
	}
	if strings.TrimSpace(c.ProviderID) != "" {
		out.ProviderID = c.ProviderID
		out.TokenSource = c.TokenSource
		out.APIKey = ""
	} else if strings.TrimSpace(c.Provider) != "" {
		// A legacy tier that names a provider string but no provider row is a
		// different credential than flash's — never inherit flash's pool, and
		// never inherit its key either: a key minted for one provider
		// authenticates nothing at another.
		out.ProviderID = ""
		out.TokenSource = nil
		out.APIKey = ""
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
	// The fallback model belongs to the tier's provider. A tier that points at
	// a different provider cannot keep flash's fallback — that model may not
	// exist there — but a model-only override on the same provider inherits it.
	if strings.TrimSpace(c.FallbackModel) != "" {
		out.FallbackModel = c.FallbackModel
	} else if strings.TrimSpace(c.ProviderID) != "" || strings.TrimSpace(c.Provider) != "" {
		out.FallbackModel = ""
	}
	// The window is the one field that must NOT simply inherit flash's value: it
	// describes a specific model, so a tier pointing at a different model would
	// otherwise silently adopt a number belonging to another one — and inheriting
	// a larger window is exactly the direction that kills a run. Blank on a tier
	// that changed the model means "unknown", which resolves from the tier's own
	// model instead.
	switch {
	case c.ContextWindow > 0:
		out.ContextWindow = c.ContextWindow
	case strings.TrimSpace(c.Model) != "" || strings.TrimSpace(c.Provider) != "" || strings.TrimSpace(c.BaseURL) != "":
		out.ContextWindow = 0
	}
	return out
}

// TierSetFromWorkspace assembles a TierSet from the workspace model tier pool and
// decrypted per-tier keys (keyed "lite"/"flash"/"pro"). The base
// provider/model/base_url columns are the flash tier; lite/pro use their own
// columns and key. A tier with no decrypted key is left unconfigured and
// resolves back to flash at call time. poolFor resolves a provider row id to
// its OAuth account pool (oauth.Manager.Pool); pass nil where no pool exists —
// OAuth vendors then build without a TokenSource and fail at call time with a
// clear error rather than silently sending no credential.
func TierSetFromWorkspace(cfg storage.WorkspaceModelTiers, keys map[string]string, poolFor func(providerID string) ai.TokenSource) TierSet {
	// Only OAuth vendors draw from an account pool; an API-key provider row id
	// must not produce a TokenSource or the wire client would ignore its key.
	src := func(vendor, providerID string) ai.TokenSource {
		if poolFor == nil || providerID == "" || !ai.IsOAuthVendor(vendor) {
			return nil
		}
		return poolFor(providerID)
	}
	return TierSet{
		tiers: map[Tier]TierConfig{
			TierFlash: {Provider: cfg.Provider, Model: cfg.Model, BaseURL: cfg.BaseURL, APIKey: keys["flash"], ProviderID: cfg.FlashProviderID, TokenSource: src(cfg.Provider, cfg.FlashProviderID), ContextWindow: cfg.ContextWindow, FallbackModel: cfg.FallbackModel},
			TierLite:  {Provider: cfg.LiteProvider, Model: cfg.LiteModel, BaseURL: cfg.LiteBaseURL, APIKey: keys["lite"], ProviderID: cfg.LiteProviderID, TokenSource: src(cfg.LiteProvider, cfg.LiteProviderID), ContextWindow: cfg.LiteContextWindow, FallbackModel: cfg.LiteFallbackModel},
			TierPro:   {Provider: cfg.ProProvider, Model: cfg.ProModel, BaseURL: cfg.ProBaseURL, APIKey: keys["pro"], ProviderID: cfg.ProProviderID, TokenSource: src(cfg.ProProvider, cfg.ProProviderID), ContextWindow: cfg.ProContextWindow, FallbackModel: cfg.ProFallbackModel},
		},
		fallback: cfg.ModelFallback,
	}
}

// DefaultAuthoringTier is the workspace tier used for authoring helpers that are
// intentionally Pro-only in the first pass.
const DefaultAuthoringTier = TierPro
