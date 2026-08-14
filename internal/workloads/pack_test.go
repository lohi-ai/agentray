package workloads

import (
	"strings"
	"testing"
)

func TestCatalogInvariants(t *testing.T) {
	packs := All()
	if len(packs) == 0 {
		t.Fatal("expected at least one pack")
	}
	seen := map[string]bool{}
	for _, p := range packs {
		if p.Slug == "" {
			t.Errorf("pack %q has an empty slug", p.Name)
		}
		if seen[p.Slug] {
			t.Errorf("duplicate pack slug %q", p.Slug)
		}
		seen[p.Slug] = true
		if p.Name == "" || p.Tagline == "" || p.Description == "" || p.Category == "" {
			t.Errorf("pack %q is missing display copy", p.Slug)
		}
		if p.SoulMD == "" || p.AgentsMD == "" {
			t.Errorf("pack %q is missing its persona (soul/agents md)", p.Slug)
		}
		if len(p.Scopes) == 0 {
			t.Errorf("pack %q grants no scopes", p.Slug)
		}
		if !p.Scopes["analyze_build"] {
			t.Errorf("pack %q does not grant analyze_build (chart/dashboard authoring)", p.Slug)
		}
		for _, sk := range p.Skills {
			if sk.Name == "" || sk.Description == "" || sk.Body == "" {
				t.Errorf("pack %q has an incomplete skill %q", p.Slug, sk.Name)
			}
		}
	}
}

func TestDefaultPackIsGrowthLead(t *testing.T) {
	p, ok := Default()
	if !ok {
		t.Fatal("default pack must resolve")
	}
	if p.Slug != DefaultSlug || p.Category != CategoryGrowth {
		t.Fatalf("default pack = %+v", p)
	}
	if !p.Scopes["growth_suggest"] {
		t.Error("growth-lead must grant growth_suggest")
	}
}

func TestBySlugMiss(t *testing.T) {
	if _, ok := BySlug("does-not-exist"); ok {
		t.Fatal("expected miss")
	}
}

func TestRegisterDuplicatePanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("duplicate Register must panic")
		}
	}()
	Register(Pack{Slug: DefaultSlug, Category: CategoryGrowth})
}

func TestGrowthLeadLoopIsComplete(t *testing.T) {
	p := MustBySlug("growth-lead")
	for _, marker := range []string{"Measure", "Diagnose", "Decide", "Learn", "Report", "send_notification"} {
		if !strings.Contains(p.AgentsMD, marker) {
			t.Errorf("growth-lead scheduled loop persona is missing %q", marker)
		}
	}
	want := map[string]bool{
		"pmf-scorecard": false, "weakest-link-triage": false, "experiment-design": false,
		"experiment-readout": false, "capability-request": false, "cycle-readout": false,
	}
	for _, sk := range p.Skills {
		if _, tracked := want[sk.Name]; tracked {
			want[sk.Name] = true
		}
	}
	for name, found := range want {
		if !found {
			t.Errorf("growth-lead is missing the %q skill", name)
		}
	}
}

func TestOperatorAndSupportHaveNoShippedPacks(t *testing.T) {
	if packs := ByCategory(CategoryOperator); len(packs) != 0 {
		t.Errorf("operator packs must stay empty until a reusable pack exists; got %d", len(packs))
	}
	if packs := ByCategory(CategorySupport); len(packs) != 0 {
		t.Errorf("support packs must stay empty; support is a future channel+pack, not a backend")
	}
}
