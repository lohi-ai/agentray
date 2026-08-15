package agentruntime

import (
	"strings"
	"testing"
)

// An operator who typed a number meant it: no catalog can know a self-hosted
// endpoint's window, and this override is the only way to tell the loop.
func TestEffectiveContextWindowPrefersTheOperatorsOverride(t *testing.T) {
	tc := TierConfig{Provider: "anthropic", Model: "claude-sonnet-4", ContextWindow: 64_000}
	if got := EffectiveContextWindow(tc); got != 64_000 {
		t.Fatalf("EffectiveContextWindow = %d, want the operator's 64000", got)
	}
}

// Without an override the window comes from the ai package, so the common case
// — an owner who picked a model from the list and typed nothing else — is
// already correct rather than falling back to the raw configured ceiling.
func TestEffectiveContextWindowFallsBackToTheModelCatalog(t *testing.T) {
	if got := EffectiveContextWindow(TierConfig{Provider: "anthropic", Model: "claude-sonnet-4-20250514"}); got != 200_000 {
		t.Fatalf("claude window = %d, want 200000 from the ai package", got)
	}
	if got := EffectiveContextWindow(TierConfig{Provider: "openai-compat", Model: "some-local-model"}); got != 0 {
		t.Fatalf("unknown model window = %d, want 0 so the configured budget stands alone", got)
	}
}

// The inheritance trap. Every other TierConfig field sensibly inherits flash's
// value when blank — that is what makes "one provider + key, different model per
// tier" cheap to configure. The window must NOT, because it describes a specific
// model: a pro tier pointing at a 32k model would otherwise inherit flash's
// 1M-token window and never compact, which is the original bug reintroduced one
// layer up.
func TestATierThatChangesModelDoesNotInheritTheWindow(t *testing.T) {
	ts := TierSet{
		TierFlash: TierConfig{Provider: "google", Model: "gemini-1.5-pro", ContextWindow: 2_000_000},
		TierPro:   TierConfig{Model: "gpt-4"},
	}
	got := ts.resolve(TierPro)
	if got.Model != "gpt-4" {
		t.Fatalf("model = %q, want the pro override", got.Model)
	}
	if got.ContextWindow != 0 {
		t.Fatalf("pro inherited flash's window (%d); a window belongs to the model it was measured on", got.ContextWindow)
	}
	// ...and having refused the inherited number, it resolves its own.
	if w := EffectiveContextWindow(got); w != 8_192 {
		t.Fatalf("resolved window = %d, want gpt-4's 8192", w)
	}
}

// The other half: a tier that overrides nothing IS flash, so it keeps flash's
// window. Dropping it here would make every unconfigured tier lose a number it
// legitimately shares.
func TestATierThatOverridesNothingKeepsTheWindow(t *testing.T) {
	ts := TierSet{
		TierFlash: TierConfig{Provider: "google", Model: "gemini-1.5-pro", ContextWindow: 2_000_000},
		TierLite:  TierConfig{},
	}
	if got := ts.resolve(TierLite); got.ContextWindow != 2_000_000 {
		t.Fatalf("window = %d, want flash's 2000000 for a tier that changed nothing", got.ContextWindow)
	}
}

// A tier may also override the window alone — same model, an operator who knows
// their endpoint serves it truncated.
func TestATierMayOverrideTheWindowAlone(t *testing.T) {
	ts := TierSet{
		TierFlash: TierConfig{Provider: "anthropic", Model: "claude-sonnet-4", ContextWindow: 200_000},
		TierPro:   TierConfig{ContextWindow: 120_000},
	}
	got := ts.resolve(TierPro)
	if got.Model != "claude-sonnet-4" {
		t.Fatalf("model = %q, want the inherited flash model", got.Model)
	}
	if got.ContextWindow != 120_000 {
		t.Fatalf("window = %d, want the tier's own 120000", got.ContextWindow)
	}
}

// The ladder is why the window lives per-rung. Escalation across models with
// different windows must produce different budgets, or the run carries the
// primary's headroom onto a model that cannot hold it.
func TestBuildRungsCarriesEachTiersOwnWindow(t *testing.T) {
	rungs, err := buildRungs([]TierConfig{
		{Provider: "anthropic", Model: "claude-sonnet-4", APIKey: "k"},
		{Provider: "openai", Model: "gpt-4", APIKey: "k"},
		{Provider: "openai-compat", Model: "mystery", BaseURL: "http://localhost:1", APIKey: "k"},
	})
	if err != nil {
		t.Fatalf("buildRungs: %v", err)
	}
	want := []int{200_000, 8_192, 0}
	if len(rungs) != len(want) {
		t.Fatalf("got %d rungs, want %d", len(rungs), len(want))
	}
	for i, w := range want {
		if rungs[i].ContextWindow != w {
			t.Errorf("rung %d (%s) window = %d, want %d", i, rungs[i].Model, rungs[i].ContextWindow, w)
		}
	}
}

// The production path is preset.Full, NOT agentcore.New(Config) — the preset
// builds the model plugin from Config itself, so a field can be threaded all
// the way to Config and still be dropped one layer further in. That is exactly
// what happened here, and only a test that goes through Build catches it.
func TestBuildCarriesTheContextWindowIntoTheComposition(t *testing.T) {
	p := representativeBuildParams()
	p.ContextWindow = 32_000
	agent, err := Build(p)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	desc := agent.Describe()
	if !strings.Contains(desc, "context_window:        32000") {
		t.Fatalf("the composed agent lost the context window:\n%s", desc)
	}
	// And the budget it will actually compact against is capped by it, not the
	// 50000 the representative params configure.
	if !strings.Contains(desc, "budget=16000") {
		t.Fatalf("the compaction budget ignored the window:\n%s", desc)
	}
}
