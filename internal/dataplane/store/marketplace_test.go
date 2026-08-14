package storage

import "testing"

func TestPackCatalogHook(t *testing.T) {
	t.Cleanup(func() { SetPackCatalog(nil, nil) })
	if got := AgentPresets(); len(got) != 0 {
		t.Fatalf("unwired catalog should be empty, got %d", len(got))
	}
	if _, ok := AgentPresetBySlug("growth-lead"); ok {
		t.Fatal("unwired lookup should miss")
	}

	SetPackCatalog(
		func() []AgentPreset {
			return []AgentPreset{{Slug: "growth-lead", Name: "Growth Lead", Category: "growth"}}
		},
		func(slug string) (AgentPreset, bool) {
			if slug == "growth-lead" {
				return AgentPreset{Slug: slug, Name: "Growth Lead"}, true
			}
			return AgentPreset{}, false
		},
	)
	if len(AgentPresets()) != 1 {
		t.Fatalf("wired catalog: got %d", len(AgentPresets()))
	}
	if _, ok := AgentPresetBySlug("growth-lead"); !ok {
		t.Fatal("wired lookup missed growth-lead")
	}
	if _, ok := AgentPresetBySlug("nope"); ok {
		t.Fatal("wired lookup should miss unknown slugs")
	}
}
