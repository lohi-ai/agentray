package agentruntime

import (
	"testing"

	"github.com/lohi-ai/agentray/internal/storage"
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

// tierSetFromWorkspace maps the workspace tier columns + decrypted per-tier keys
// onto a TierSet, and the result must behave like any other TierSet under resolve.
func TestTierSetFromWorkspace(t *testing.T) {
	cfg := storage.WorkspaceModelTiers{
		Provider: "openai", Model: "gpt-4o", BaseURL: "https://flash",
		LiteProvider: "openai", LiteModel: "gpt-4o-mini",
		ProProvider: "anthropic", ProModel: "claude-opus", ProBaseURL: "https://pro",
	}
	keys := map[string]string{"flash": "fk", "lite": "lk", "pro": "pk"}
	ts := tierSetFromWorkspace(cfg, keys)

	if got := ts.resolve(TierFlash); got.Model != "gpt-4o" || got.APIKey != "fk" || got.BaseURL != "https://flash" {
		t.Errorf("flash = %+v, want gpt-4o/fk/https://flash", got)
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
	ts := tierSetFromWorkspace(cfg, keys)
	if got := ts.resolve(TierPro); got.Model != "gpt-4o" || got.APIKey != "fk" {
		t.Errorf("unconfigured pro = %+v, want it to inherit flash", got)
	}
}

// An unconfigured tier resolves to flash; a configured one keeps its own config.
func TestResolveFallsBackToFlash(t *testing.T) {
	flash := tc("openai", "gpt-4o", "flash-key")
	ts := TierSet{
		TierFlash: flash,
		TierLite:  TierConfig{}, // unconfigured (no key)
		TierPro:   tc("anthropic", "claude-opus", "pro-key"),
	}
	if got := ts.resolve(TierLite); got != flash {
		t.Errorf("resolve(lite) = %+v, want flash %+v", got, flash)
	}
	if got := ts.resolve(TierPro); got.Model != "claude-opus" {
		t.Errorf("resolve(pro) = %+v, want pro config", got)
	}
}

// Fallback off → ladder is just the start tier.
func TestLadderNoFallback(t *testing.T) {
	ts := TierSet{
		TierFlash: tc("openai", "gpt-4o", "flash-key"),
		TierPro:   tc("anthropic", "claude-opus", "pro-key"),
	}
	got := ts.ladder(TierFlash, false)
	if len(got) != 1 || got[0].Model != "gpt-4o" {
		t.Fatalf("ladder(flash,false) = %+v, want [flash]", got)
	}
}

// Fallback on from flash → flash then the configured pro; lite is below flash so
// it is never part of an upward ladder.
func TestLadderEscalatesUpward(t *testing.T) {
	ts := TierSet{
		TierLite:  tc("openai", "gpt-4o-mini", "lite-key"),
		TierFlash: tc("openai", "gpt-4o", "flash-key"),
		TierPro:   tc("anthropic", "claude-opus", "pro-key"),
	}
	got := ts.ladder(TierFlash, true)
	if len(got) != 2 {
		t.Fatalf("ladder(flash,true) len = %d, want 2: %+v", len(got), got)
	}
	if got[0].Model != "gpt-4o" || got[1].Model != "claude-opus" {
		t.Errorf("ladder(flash,true) = %+v, want [flash, pro]", got)
	}
}

// Starting at lite with fallback walks lite→flash→pro.
func TestLadderFromLiteWalksAll(t *testing.T) {
	ts := TierSet{
		TierLite:  tc("openai", "gpt-4o-mini", "lite-key"),
		TierFlash: tc("openai", "gpt-4o", "flash-key"),
		TierPro:   tc("anthropic", "claude-opus", "pro-key"),
	}
	got := ts.ladder(TierLite, true)
	if len(got) != 3 {
		t.Fatalf("ladder(lite,true) len = %d, want 3: %+v", len(got), got)
	}
	want := []string{"gpt-4o-mini", "gpt-4o", "claude-opus"}
	for i, w := range want {
		if got[i].Model != w {
			t.Errorf("ladder[%d].Model = %q, want %q", i, got[i].Model, w)
		}
	}
}

// An unconfigured pro tier that resolves back to flash must not add a redundant
// rung to a flash-start ladder.
func TestLadderDedupsResolvedFlash(t *testing.T) {
	ts := TierSet{
		TierFlash: tc("openai", "gpt-4o", "flash-key"),
		TierPro:   TierConfig{}, // unconfigured → resolves to flash
	}
	got := ts.ladder(TierFlash, true)
	if len(got) != 1 {
		t.Errorf("ladder(flash,true) with unconfigured pro = %+v, want [flash] only", got)
	}
}
