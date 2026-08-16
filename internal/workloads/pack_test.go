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

// Every other pack answers from the event store. product-scout is the one that
// must work with an empty dataplane, so the thing worth pinning is that its
// persona actually carries the pre-product job: web evidence, a falsifiable
// test, a pre-committed threshold, and the tracking plan that hands the owner
// to the rest of the product. A "validation" agent that quietly needs events
// would strand exactly the user it was added for.
func TestProductScoutValidatesWithoutAnEventStream(t *testing.T) {
	p := MustBySlug("product-scout")
	if p.Category != CategoryValidate {
		t.Errorf("product-scout category = %q, want validate", p.Category)
	}
	if p.Scopes["monitor"] {
		t.Error("product-scout grants monitor — there is no running product to watch")
	}
	// The evidence base. Without web_fetch it can only reflect the owner's own
	// assumptions back at them, which is the failure it exists to prevent.
	var hasFetch bool
	for _, name := range p.Tools {
		if name == "web_fetch" {
			hasFetch = true
		}
	}
	if !hasFetch {
		t.Error("product-scout does not request web_fetch — it has no source of evidence")
	}
	body := p.SoulMD + p.AgentsMD
	for _, sk := range p.Skills {
		body += sk.Body
	}
	for _, marker := range []string{
		"web_fetch",             // fetches real pages
		"hypothesis",            // labels what it did not fetch
		"threshold",             // pre-commits the kill/keep number
		"submit_recommendation", // files the decision
		"remember",              // carries the frame between sessions
		"propose_test",          // writes the threshold down as a row, not a sentence
		"test_status",           // reads the result against the number that was agreed
		// The tracking plan has to name the events the platform ACTUALLY sends.
		// It used to say page_view/signup, which nothing in AgentRay emits — a
		// threshold set on an event name the page never fires counts zero for
		// ever, and reads to the owner as "nobody wants this".
		"user.pageview", "waitlist.joined",
	} {
		if !strings.Contains(body, marker) {
			t.Errorf("product-scout persona never mentions %q", marker)
		}
	}
	for _, marker := range []string{"FRAME", "SCOUT", "SHARPEN", "TEST", "DECIDE"} {
		if !strings.Contains(p.AgentsMD, marker) {
			t.Errorf("product-scout loop persona is missing %q", marker)
		}
	}
	want := map[string]bool{
		"demand-evidence": false, "positioning-statement": false,
		"smoke-test-design": false, "tracking-plan": false,
	}
	for _, sk := range p.Skills {
		if _, tracked := want[sk.Name]; tracked {
			want[sk.Name] = true
		}
	}
	for name, found := range want {
		if !found {
			t.Errorf("product-scout is missing the %q skill", name)
		}
	}
}

// Marketing-first is the doctrine the validate job now runs on: put the message
// in front of a real audience BEFORE building, and let the response decide. That
// only works if the marketing teammate knows how to operate with no event
// history — and, more importantly, refuses to read one quiet post as proof that
// nobody wants the thing.
func TestMarketingLeadCanRunBeforeThereIsAProduct(t *testing.T) {
	p := MustBySlug("marketing-lead")
	var skill *Skill
	for i := range p.Skills {
		if p.Skills[i].Name == "marketing-first-test" {
			skill = &p.Skills[i]
		}
	}
	if skill == nil {
		t.Fatal("marketing-lead has no marketing-first-test skill — the validate job hires it for exactly this")
	}
	for _, marker := range []string{
		"utm_source",  // the result must be attributable per channel
		"test_status", // it reads the committed threshold rather than inventing one
		"variant",     // it compares messages, not just counts
		"Reddit",      // the channels an idea-stage owner can actually reach
		"TikTok",
	} {
		if !strings.Contains(skill.Body, marker) {
			t.Errorf("marketing-first-test never mentions %q", marker)
		}
	}
	// The guard against the doctrine's own failure mode: "we cannot market it"
	// is only a verdict on the idea once a real audience saw a clear message
	// more than once. Without this the product kills good ideas on a bad tweet.
	for _, marker := range []string{"tested anything", "not a verdict on"} {
		if !strings.Contains(skill.Body, marker) {
			t.Errorf("marketing-first-test must forbid killing an idea on one thin post; missing %q", marker)
		}
	}
}

// A pack's Tools are granted at install with an empty config, so a pack asking
// for a configurable tool (http_request's host allowlist, mcp's servers) would
// install a tool that cannot build. workloads may not import the runtime that
// owns the registry, so the name-level check lives here and the registry-level
// one lives in internal/app.
func TestPackToolsAreNamedNotConfigured(t *testing.T) {
	for _, p := range All() {
		for _, name := range p.Tools {
			if strings.TrimSpace(name) != name || name == "" {
				t.Errorf("pack %q requests a malformed tool name %q", p.Slug, name)
			}
		}
	}
}

func TestSupportHasNoShippedPacks(t *testing.T) {
	if packs := ByCategory(CategorySupport); len(packs) != 0 {
		t.Errorf("support packs must stay empty; support is a future channel+pack, not a backend")
	}
}

// An operator pack reads and reports; it does not reach other systems. That is
// what keeps it a preset rather than an internal/workloads/operator pack (see
// that package's doc.go): reach, not naming, is the line. A pack carries only
// scopes and skills, so the strongest thing a test can assert is that the
// category ships a watcher granted the monitor scope — the tools (activity_
// summary, recent_events) that the shipped presets otherwise never named.
func TestOperatorPacksAreMonitorScopedWatchers(t *testing.T) {
	packs := ByCategory(CategoryOperator)
	if len(packs) == 0 {
		t.Fatal("operator category ships no pack; ops-watch should be registered")
	}
	for _, p := range packs {
		if !p.Scopes["monitor"] {
			t.Errorf("operator pack %q does not grant monitor — it cannot watch anything", p.Slug)
		}
		if p.Slug == DefaultSlug {
			t.Errorf("operator pack %q must not be the default hire", p.Slug)
		}
	}
}

// The monitor scope's two tools were granted by every preset and named by none,
// so the whole operational half of the event store shipped invisible to agents.
// ops-watch is the reader for them; this test is the tripwire that keeps it one.
func TestOpsWatchReadsTheMonitorTools(t *testing.T) {
	p := MustBySlug("ops-watch")
	if p.Category != CategoryOperator {
		t.Errorf("ops-watch category = %q, want operator", p.Category)
	}
	body := p.SoulMD + p.AgentsMD
	for _, sk := range p.Skills {
		body += sk.Body
	}
	// The signals it exists to watch, and the tools that reach them.
	for _, marker := range []string{
		"activity_summary", "recent_events", "is_error", "latency_ms", "cost_usd",
		"send_notification", "submit_recommendation", "remember",
	} {
		if !strings.Contains(body, marker) {
			t.Errorf("ops-watch persona never mentions %q", marker)
		}
	}
	for _, marker := range []string{"SWEEP", "TRIAGE", "ESCALATE", "SEV1"} {
		if !strings.Contains(p.AgentsMD, marker) {
			t.Errorf("ops-watch loop persona is missing %q", marker)
		}
	}
	want := map[string]bool{
		"health-sweep": false, "error-triage": false,
		"spend-watch": false, "incident-readout": false,
	}
	for _, sk := range p.Skills {
		if _, tracked := want[sk.Name]; tracked {
			want[sk.Name] = true
		}
	}
	for name, found := range want {
		if !found {
			t.Errorf("ops-watch is missing the %q skill", name)
		}
	}
}
