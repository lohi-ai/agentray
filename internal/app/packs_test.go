package app

import (
	"testing"

	"github.com/lohi-ai/agentray/internal/runtime"
)

// The marketplace route serves whatever marketplacePresets() returns, and that
// adapter is the only bridge between the workloads registry and the store DTO.
// It had no test, so a pack could register cleanly in workloads and still reach
// the catalog with an empty persona — the two fields packToPreset copies that
// nothing else in the request path would notice were blank.
func TestMarketplaceCatalogCarriesEveryPackWhole(t *testing.T) {
	presets := marketplacePresets()
	if len(presets) == 0 {
		t.Fatal("marketplace catalog is empty")
	}
	bySlug := map[string]bool{}
	for _, p := range presets {
		bySlug[p.Slug] = true
		if p.Name == "" || p.Category == "" || p.Tagline == "" {
			t.Errorf("preset %q reaches the catalog missing display copy", p.Slug)
		}
		if p.SoulMD == "" || p.AgentsMD == "" {
			t.Errorf("preset %q reaches the catalog with no persona to install", p.Slug)
		}
		for _, sk := range p.Skills {
			if sk.Name == "" || sk.Body == "" {
				t.Errorf("preset %q ships an empty skill %q", p.Slug, sk.Name)
			}
		}
	}
	// The roster a workspace can hire from, one per phase of a product's life:
	// product-scout before there is data, growth/marketing/data once it flows,
	// ops-watch once it has to stay up.
	for _, slug := range []string{"growth-lead", "data-analyst", "tracking-steward",
		"marketing-strategist", "marketing-lead", "insight-digest", "ops-watch",
		"product-scout"} {
		if !bySlug[slug] {
			t.Errorf("catalog is missing the %q pack", slug)
		}
	}
}

// A pack may ask for non-scope tools (workloads.Pack.Tools), and install grants
// them with an EMPTY config through UpsertAgentTool — which does not validate
// the name. So an unregistered name installs a dead row, and a *configurable*
// tool (http_request's host allowlist, mcp's server list) installs one that
// cannot build. workloads may not import the runtime registry; the composition
// root may, so this is the only place the two halves can be checked against
// each other.
func TestPackToolsExistAndNeedNoConfig(t *testing.T) {
	for _, p := range marketplacePresets() {
		for _, name := range p.Tools {
			if !agentruntime.IsRegisteredTool(name) {
				t.Errorf("pack %q requests unregistered tool %q", p.Slug, name)
				continue
			}
			for _, entry := range agentruntime.ToolCatalog() {
				if entry.Name == name && entry.Configurable {
					t.Errorf("pack %q requests configurable tool %q — install has no config to give it",
						p.Slug, name)
				}
			}
		}
	}
}

func TestMarketplaceLookupMatchesTheListing(t *testing.T) {
	got, ok := marketplacePresetBySlug("ops-watch")
	if !ok {
		t.Fatal("ops-watch does not resolve by slug — install would 404")
	}
	if got.Category != "operator" || len(got.Skills) == 0 || got.Scopes["monitor"] != true {
		t.Fatalf("ops-watch install payload = category %q, %d skills, monitor=%v",
			got.Category, len(got.Skills), got.Scopes["monitor"])
	}
	if _, ok := marketplacePresetBySlug("no-such-pack"); ok {
		t.Error("unknown slug must miss")
	}
}
