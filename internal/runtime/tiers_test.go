package agentruntime

import (
	"testing"

	"github.com/lohi-ai/agentray/agentcore"
	"github.com/lohi-ai/agentray/ai"
	"github.com/lohi-ai/agentray/internal/dataplane/store"
)

// tc is a shorthand for a configured tier (non-empty key) in these tests.
func tc(provider, model, key string) TierConfig {
	return TierConfig{Provider: provider, Model: model, APIKey: key}
}

func TestTierFromName(t *testing.T) {
	cases := map[string]Tier{
		"lite":    TierLite,
		"flash":   TierFlash,
		"pro":     TierPro,
		"":        TierFlash, // default
		"unknown": TierFlash, // default
	}
	for name, want := range cases {
		if got := TierFromName(name); got != want {
			t.Errorf("TierFromName(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestBuildProviderSelectsPublicOpenAIResponsesWire(t *testing.T) {
	provider, err := buildProvider(ai.VendorOpenAIResponses, "", "sk-test", nil, "row-1")
	if err != nil {
		t.Fatalf("buildProvider: %v", err)
	}
	responses, ok := provider.(*ai.OpenAIResponsesProvider)
	if !ok {
		t.Fatalf("provider = %T, want *ai.OpenAIResponsesProvider", provider)
	}
	if responses.Name() != ai.VendorOpenAIResponses || responses.APIKey != "sk-test" {
		t.Fatalf("responses provider = %+v", responses)
	}
}

func TestModelTierSelectsOpenAIWireFromModelCapabilities(t *testing.T) {
	for _, tc := range []struct {
		name          string
		caps          agentcore.ModelCapabilities
		wantResponses bool
	}{
		{name: "explicit support selects responses", caps: agentcore.ModelCapabilities{StatefulResponses: agentcore.CapabilitySupported}, wantResponses: true},
		{name: "unknown stays chat compatible"},
		{name: "explicit unsupported stays chat", caps: agentcore.ModelCapabilities{StatefulResponses: agentcore.CapabilityUnsupported}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tier := ModelTier{TierConfig: TierConfig{
				Provider: "openai", Model: "gpt-test", APIKey: "sk-test", Capabilities: tc.caps,
			}}
			provider, err := tier.RawProvider()
			if err != nil {
				t.Fatalf("RawProvider: %v", err)
			}
			_, responses := provider.(*ai.OpenAIResponsesProvider)
			if responses != tc.wantResponses {
				t.Fatalf("provider = %T, responses=%v want %v", provider, responses, tc.wantResponses)
			}
			if provider.Name() != "openai" {
				t.Fatalf("provider identity = %q, want openai", provider.Name())
			}
		})
	}
}

func TestModelTierUsesDifferentWiresForSameProviderFallback(t *testing.T) {
	tier := ModelTier{TierConfig: TierConfig{
		Provider: "openai", Model: "chat-model", APIKey: "sk-test", ProviderID: "row-1",
		Capabilities: agentcore.ModelCapabilities{StatefulResponses: agentcore.CapabilityUnsupported},
		Fallback: &TierConfig{
			Model:        "responses-model",
			Capabilities: agentcore.ModelCapabilities{StatefulResponses: agentcore.CapabilitySupported},
		},
	}}
	rungs, err := tier.Rungs()
	if err != nil {
		t.Fatalf("Rungs: %v", err)
	}
	if len(rungs) != 2 {
		t.Fatalf("rungs = %d, want 2", len(rungs))
	}
	if _, ok := rungs[0].Provider.(*ai.OpenAIProvider); !ok {
		t.Fatalf("primary provider = %T, want chat wire", rungs[0].Provider)
	}
	responses, ok := rungs[1].Provider.(*ai.OpenAIResponsesProvider)
	if !ok {
		t.Fatalf("fallback provider = %T, want responses wire", rungs[1].Provider)
	}
	if responses.Name() != "openai" || responses.APIKey != "sk-test" {
		t.Fatalf("fallback responses provider = %+v", responses)
	}
}

func TestModelTierReusesProviderWhenFallbackWireMatches(t *testing.T) {
	caps := agentcore.ModelCapabilities{StatefulResponses: agentcore.CapabilitySupported}
	tier := ModelTier{TierConfig: TierConfig{
		Provider: "openai", Model: "primary", APIKey: "sk-test", Capabilities: caps,
		Fallback: &TierConfig{Model: "fallback", Capabilities: caps},
	}}
	rungs, err := tier.Rungs()
	if err != nil {
		t.Fatalf("Rungs: %v", err)
	}
	if len(rungs) != 2 || rungs[0].Provider != rungs[1].Provider {
		t.Fatalf("matching wires should share provider instance: %+v", rungs)
	}
}

// tierSetFromWorkspace maps the workspace tier columns + decrypted per-tier keys
// onto a TierSet, and the result must behave like any other TierSet under resolve.
func TestTierSetFromWorkspace(t *testing.T) {
	cfg := storage.WorkspaceModelTiers{
		Provider: "openai", Model: "gpt-4o", BaseURL: "https://flash",
		Capabilities: agentcore.ModelCapabilities{Tools: agentcore.CapabilityUnsupported},
		LiteProvider: "openai", LiteModel: "gpt-4o-mini",
		ProProvider: "anthropic", ProModel: "claude-opus", ProBaseURL: "https://pro",
	}
	keys := map[string]string{"flash": "fk", "lite": "lk", "pro": "pk"}
	ts := TierSetFromWorkspace(cfg, keys, nil)

	if got := ts.resolve(TierFlash); got.Model != "gpt-4o" || got.APIKey != "fk" || got.BaseURL != "https://flash" {
		t.Errorf("flash = %+v, want gpt-4o/fk/https://flash", got)
	}
	if got := ts.resolve(TierFlash).Capabilities.Tools; got != agentcore.CapabilityUnsupported {
		t.Fatalf("flash tools capability = %q", got)
	}
	rungs, err := ts.For(TierFlash).Rungs()
	if err != nil {
		t.Fatalf("Rungs: %v", err)
	}
	if got := rungs[0].Capabilities.Tools; got != agentcore.CapabilityUnsupported {
		t.Fatalf("runtime rung tools capability = %q", got)
	}
	if got := ts.resolve(TierLite); got.Model != "gpt-4o-mini" || got.APIKey != "lk" {
		t.Errorf("lite = %+v, want gpt-4o-mini/lk", got)
	}
	if got := ts.resolve(TierPro); got.Provider != "anthropic" || got.Model != "claude-opus" || got.APIKey != "pk" {
		t.Errorf("pro = %+v, want anthropic/claude-opus/pk", got)
	}
}

// A workspace tier left unconfigured (no key) resolves to flash, proving the
// shared-pool TierSet honours the same inheritance the per-project one did.
func TestTierSetFromWorkspaceUnconfiguredTierInheritsFlash(t *testing.T) {
	cfg := storage.WorkspaceModelTiers{Provider: "openai", Model: "gpt-4o"}
	keys := map[string]string{"flash": "fk"} // lite/pro have no key
	ts := TierSetFromWorkspace(cfg, keys, nil)
	if got := ts.resolve(TierPro); got.Model != "gpt-4o" || got.APIKey != "fk" {
		t.Errorf("unconfigured pro = %+v, want it to inherit flash", got)
	}
}

// When a tier uses an OAuth provider, the token source pool is wired onto the
// resolved TierConfig even when no APIKey is in the map.
func TestTierSetFromWorkspaceOAuthTokenSource(t *testing.T) {
	cfg := storage.WorkspaceModelTiers{
		Provider: "openai", Model: "gpt-4o",
		FlashProviderID: "p-oauth-1",
		ProProvider:     "claude-code", ProModel: "claude-3-7-sonnet-20250219",
		ProProviderID: "p-oauth-2",
	}
	keys := map[string]string{"flash": "fk"} // pro has no static key
	poolCalled := map[string]bool{}
	poolFor := func(pID string) ai.TokenSource {
		poolCalled[pID] = true
		return nil
	}
	ts := TierSetFromWorkspace(cfg, keys, poolFor)
	pro := ts.resolve(TierPro)
	if pro.Model != "claude-3-7-sonnet-20250219" {
		t.Errorf("pro model = %q, want claude-3-7-sonnet-20250219", pro.Model)
	}
	if !poolCalled["p-oauth-2"] {
		t.Errorf("expected poolFor to be called for p-oauth-2")
	}
}

// An unconfigured tier resolves to flash; a configured one keeps its own config.
func TestResolveFallsBackToFlash(t *testing.T) {
	flash := tc("openai", "gpt-4o", "flash-key")
	ts := TierSet{tiers: map[Tier]TierConfig{
		TierFlash: flash,
		TierLite:  TierConfig{}, // unconfigured (no key)
		TierPro:   tc("anthropic", "claude-opus", "pro-key"),
	}}
	if got := ts.resolve(TierLite); got != flash {
		t.Errorf("resolve(lite) = %+v, want flash %+v", got, flash)
	}
	if got := ts.resolve(TierPro); got.Model != "claude-opus" {
		t.Errorf("resolve(pro) = %+v, want pro config", got)
	}
}

// A tier that names a different provider must not inherit flash's key — a key
// minted for one provider authenticates nothing at another.
func TestResolveClearsKeyAcrossProviders(t *testing.T) {
	ts := TierSet{tiers: map[Tier]TierConfig{
		TierFlash: tc("openai", "gpt-4o", "flash-key"),
		TierPro:   {Provider: "anthropic", Model: "claude-opus"},
	}}
	if got := ts.resolve(TierPro); got.APIKey != "" {
		t.Errorf("pro inherited flash's key %q across providers", got.APIKey)
	}
}

// Fallback off → the tier's rungs are just the primary model.
func TestForNoFallback(t *testing.T) {
	ts := TierSet{tiers: map[Tier]TierConfig{
		TierFlash: {Provider: "openai", Model: "gpt-4o", APIKey: "k", Fallback: &TierConfig{Model: "gpt-4o-mini"}},
	}}
	rungs, err := ts.For(TierFlash).Rungs()
	if err != nil {
		t.Fatalf("Rungs: %v", err)
	}
	if len(rungs) != 1 || rungs[0].Model != "gpt-4o" {
		t.Fatalf("rungs = %+v, want [gpt-4o]", rungs)
	}
}

// Fallback on → the tier's rungs are primary then its fallback model on the
// same provider — never another tier.
func TestForAddsInTierFallbackRung(t *testing.T) {
	ts := TierSet{
		fallback: true,
		tiers: map[Tier]TierConfig{
			TierFlash: {Provider: "openai", Model: "gpt-4o", APIKey: "k", Fallback: &TierConfig{Model: "gpt-4o-mini"}},
			TierPro:   tc("anthropic", "claude-opus", "pro-key"),
		},
	}
	rungs, err := ts.For(TierFlash).Rungs()
	if err != nil {
		t.Fatalf("Rungs: %v", err)
	}
	if len(rungs) != 2 || rungs[0].Model != "gpt-4o" || rungs[1].Model != "gpt-4o-mini" {
		t.Fatalf("rungs = %+v, want [gpt-4o, gpt-4o-mini]", rungs)
	}
	if rungs[0].Provider != rungs[1].Provider {
		t.Error("the fallback rung must share the primary's provider")
	}
	// A tier with no fallback model of its own stays a single rung — there is
	// no cross-tier escalation anymore.
	proRungs, err := ts.For(TierPro).Rungs()
	if err != nil {
		t.Fatalf("Rungs(pro): %v", err)
	}
	if len(proRungs) != 1 || proRungs[0].Model != "claude-opus" {
		t.Fatalf("pro rungs = %+v, want [claude-opus] only", proRungs)
	}
}

// A fallback equal to the primary adds no rung; a tier that moved providers
// drops an inherited fallback that may not exist there.
func TestForFallbackEdgeCases(t *testing.T) {
	ts := TierSet{
		fallback: true,
		tiers: map[Tier]TierConfig{
			TierFlash: {Provider: "openai", Model: "gpt-4o", APIKey: "k", Fallback: &TierConfig{Model: "gpt-4o"}},
			TierPro:   {Provider: "anthropic", Model: "claude-opus", APIKey: "k2"},
		},
	}
	rungs, err := ts.For(TierFlash).Rungs()
	if err != nil {
		t.Fatalf("Rungs: %v", err)
	}
	if len(rungs) != 1 {
		t.Fatalf("fallback == primary should dedup to one rung, got %+v", rungs)
	}
	if got := ts.For(TierPro).Fallback; got != nil {
		t.Errorf("pro inherited flash's fallback %q across providers", got)
	}
}

// A fallback that names a different provider row gets its own client — its
// credentials and derived window are its own, never the primary's. Two rows
// sharing vendor+baseURL but holding different keys still count as
// cross-provider (identity is the row, not the vendor string).
func TestForAddsCrossProviderFallbackRung(t *testing.T) {
	ts := TierSet{
		fallback: true,
		tiers: map[Tier]TierConfig{
			TierFlash: {
				Provider: "openai", Model: "gpt-5", APIKey: "k1", ProviderID: "row-a",
				Fallback: &TierConfig{Provider: "anthropic", Model: "claude-sonnet-4-5", APIKey: "k2", ProviderID: "row-b", Capabilities: agentcore.ModelCapabilities{Tools: agentcore.CapabilityUnsupported}},
			},
		},
	}
	rungs, err := ts.For(TierFlash).Rungs()
	if err != nil {
		t.Fatalf("Rungs: %v", err)
	}
	if len(rungs) != 2 || rungs[0].Model != "gpt-5" || rungs[1].Model != "claude-sonnet-4-5" {
		t.Fatalf("rungs = %+v, want [gpt-5, claude-sonnet-4-5]", rungs)
	}
	if rungs[0].Provider == rungs[1].Provider {
		t.Error("a cross-provider fallback must not reuse the primary's client")
	}
	if rungs[1].Capabilities.Tools != agentcore.CapabilityUnsupported {
		t.Fatalf("fallback rung lost capability snapshot: %+v", rungs[1].Capabilities)
	}

	// Same vendor+baseURL, different row → still a distinct client (own key).
	ts2 := TierSet{
		fallback: true,
		tiers: map[Tier]TierConfig{
			TierFlash: {
				Provider: "openai", Model: "gpt-5", APIKey: "k1", ProviderID: "row-a",
				Fallback: &TierConfig{Provider: "openai", Model: "gpt-5-mini", APIKey: "k-other", ProviderID: "row-c"},
			},
		},
	}
	rungs2, err := ts2.For(TierFlash).Rungs()
	if err != nil {
		t.Fatalf("Rungs: %v", err)
	}
	if len(rungs2) != 2 || rungs2[0].Provider == rungs2[1].Provider {
		t.Fatalf("same-vendor different-row fallback must build its own client: %+v", rungs2)
	}
}

// KeyFor must reach a provider used only as a fallback rung — a mid-run key
// refresh that can't find it would kill the rung it exists to save.
func TestKeyForReachesFallbackOnlyProvider(t *testing.T) {
	ts := TierSet{tiers: map[Tier]TierConfig{
		TierFlash: {
			Provider: "openai", Model: "gpt-5", APIKey: "k1",
			Fallback: &TierConfig{Provider: "anthropic", Model: "claude-sonnet-4-5", APIKey: "fb-key"},
		},
	}}
	got, err := ts.KeyFor("anthropic")
	if err != nil || got != "fb-key" {
		t.Fatalf("KeyFor(anthropic) = %q, %v — want the fallback row's key", got, err)
	}
}

// KeyFor answers the freshest key for a provider name across the pool, and
// errors (never "") when nothing matches. Names are canonicalized: the empty
// label folds to openai (the wire default) and matching is case-insensitive,
// so a key refresh matches the provider an agentcore provider reports.
func TestKeyFor(t *testing.T) {
	ts := TierSet{tiers: map[Tier]TierConfig{
		TierFlash: tc("openai", "gpt-4o", "flash-key"),
		TierPro:   tc("anthropic", "claude-opus", "pro-key"),
	}}
	if got, err := ts.KeyFor("openai"); err != nil || got != "flash-key" {
		t.Errorf("KeyFor(openai) = %q, %v; want flash-key", got, err)
	}
	if got, err := ts.KeyFor(""); err != nil || got != "flash-key" {
		t.Errorf("KeyFor(\"\") = %q, %v; want flash-key (empty folds to openai)", got, err)
	}
	if got, err := ts.KeyFor("Anthropic"); err != nil || got != "pro-key" {
		t.Errorf("KeyFor(Anthropic) = %q, %v; want pro-key (case-folded)", got, err)
	}
	if got, err := ts.KeyFor("google"); err == nil || got != "" {
		t.Errorf("KeyFor(google) = %q, %v; want an error, never an empty key", got, err)
	}
}
