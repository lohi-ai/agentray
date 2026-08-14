package agentcore

import (
	"strings"
	"testing"
)

func mustGuard(t *testing.T, s *RepeatGuardSettings) *repeatGuard {
	t.Helper()
	g, err := newRepeatGuard(s)
	if err != nil {
		t.Fatalf("newRepeatGuard: %v", err)
	}
	if g == nil {
		t.Fatal("expected a guard")
	}
	return g
}

func call(name, args string) ToolCall { return ToolCall{Name: name, Arguments: args} }

func TestRepeatGuard_FiresAtEachThresholdOnce(t *testing.T) {
	g := mustGuard(t, &RepeatGuardSettings{Thresholds: []int{3, 5}})
	c := call("run_sql", `{"q":"select 1"}`)

	if got := g.observe([]ToolCall{c}); got != "" {
		t.Fatalf("call 1 must not nudge, got %q", got)
	}
	if got := g.observe([]ToolCall{c}); got != "" {
		t.Fatalf("call 2 must not nudge, got %q", got)
	}
	third := g.observe([]ToolCall{c})
	if third != repeatReminderShort {
		t.Fatalf("call 3 must deliver the short nudge, got %q", third)
	}
	if got := g.observe([]ToolCall{c}); got != "" {
		t.Fatalf("call 4 is between thresholds, got %q", got)
	}
	fifth := g.observe([]ToolCall{c})
	if !strings.Contains(fifth, "consecutive_calls: 5") || !strings.Contains(fifth, "run_sql") {
		t.Fatalf("call 5 must deliver the detailed nudge, got %q", fifth)
	}
	// Past the last threshold it goes quiet rather than nagging every turn.
	for i := 0; i < 5; i++ {
		if got := g.observe([]ToolCall{c}); got != "" {
			t.Fatalf("beyond the last threshold must be silent, got %q", got)
		}
	}
}

func TestRepeatGuard_ArgumentOrderDoesNotBreakTheChain(t *testing.T) {
	g := mustGuard(t, &RepeatGuardSettings{Thresholds: []int{3}})
	// Same call, keys shuffled — models do this constantly. Treating these as
	// different calls would make the guard miss most real loops.
	seq := []string{
		`{"a":1,"b":{"x":true,"y":2}}`,
		`{"b":{"y":2,"x":true},"a":1}`,
		`{"a":1,"b":{"x":true,"y":2}}`,
	}
	var last string
	for _, args := range seq {
		last = g.observe([]ToolCall{call("t", args)})
	}
	if last != repeatReminderShort {
		t.Fatalf("key order must not break the chain; got %q", last)
	}
}

func TestRepeatGuard_DifferentArgumentsResetTheChain(t *testing.T) {
	g := mustGuard(t, &RepeatGuardSettings{Thresholds: []int{3}})
	g.observe([]ToolCall{call("t", `{"q":1}`)})
	g.observe([]ToolCall{call("t", `{"q":1}`)})
	g.observe([]ToolCall{call("t", `{"q":2}`)}) // different -> reset to 1
	if got := g.observe([]ToolCall{call("t", `{"q":2}`)}); got != "" {
		t.Fatalf("count should be 2 after the reset, got %q", got)
	}
	if got := g.observe([]ToolCall{call("t", `{"q":2}`)}); got != repeatReminderShort {
		t.Fatalf("the new chain should nudge at 3, got %q", got)
	}
}

func TestRepeatGuard_ExcludedToolIsTransparentNotAReset(t *testing.T) {
	// This is the whole point of exclusion: a bookkeeping call interleaved into
	// a loop must not launder it.
	g := mustGuard(t, &RepeatGuardSettings{Thresholds: []int{3}, Exclude: []string{ToolUpdatePlan}})
	c := call("run_sql", `{"q":1}`)
	plan := call(ToolUpdatePlan, `{"items":[]}`)

	g.observe([]ToolCall{c})
	g.observe([]ToolCall{plan})
	g.observe([]ToolCall{c})
	g.observe([]ToolCall{plan})
	if got := g.observe([]ToolCall{c}); got != repeatReminderShort {
		t.Fatalf("interleaved excluded calls must not reset the chain, got %q", got)
	}
}

func TestRepeatGuard_IncludeLimitsTracking(t *testing.T) {
	g := mustGuard(t, &RepeatGuardSettings{Thresholds: []int{3}, Include: []string{"run_*"}, Exclude: []string{}})
	// An untracked tool never nudges no matter how often it repeats.
	for i := 0; i < 10; i++ {
		if got := g.observe([]ToolCall{call("web_fetch", `{}`)}); got != "" {
			t.Fatalf("web_fetch is outside Include, got %q", got)
		}
	}
	c := call("run_sql", `{"q":1}`)
	g.observe([]ToolCall{c})
	g.observe([]ToolCall{c})
	if got := g.observe([]ToolCall{c}); got != repeatReminderShort {
		t.Fatalf("included tool should nudge, got %q", got)
	}
}

func TestRepeatGuard_CountsWithinOneBatch(t *testing.T) {
	// A model that emits the same call three times in ONE assistant turn is
	// looping just as hard as one that spreads them over three turns.
	g := mustGuard(t, &RepeatGuardSettings{Thresholds: []int{3}})
	c := call("t", `{"q":1}`)
	if got := g.observe([]ToolCall{c, c, c}); got != repeatReminderShort {
		t.Fatalf("expected a nudge from a single batch, got %q", got)
	}
}

func TestRepeatGuard_ResetClearsTheChain(t *testing.T) {
	g := mustGuard(t, &RepeatGuardSettings{Thresholds: []int{3}})
	c := call("t", `{"q":1}`)
	g.observe([]ToolCall{c})
	g.observe([]ToolCall{c})
	g.reset() // a steer or follow-up arrived
	if got := g.observe([]ToolCall{c}); got != "" {
		t.Fatalf("reset must clear the count, got %q", got)
	}
}

func TestRepeatGuard_ArgumentPreviewIsBoundedButDetectionIsNot(t *testing.T) {
	long := strings.Repeat("z", 4000)
	g := mustGuard(t, &RepeatGuardSettings{Thresholds: []int{2, 3}, ArgumentsPreviewChars: 50})
	c := call("write", `{"body":"`+long+`"}`)
	g.observe([]ToolCall{c})
	g.observe([]ToolCall{c}) // threshold 2 -> short form
	detailed := g.observe([]ToolCall{c})
	if !strings.Contains(detailed, "more chars)") {
		t.Fatalf("preview should report omission, got %q", detailed)
	}
	if len(detailed) > 1000 {
		t.Fatalf("detailed reminder is unbounded (%d bytes) — it rides into every later request", len(detailed))
	}
	// Detection still used the FULL argument string: a call differing only past
	// the preview cap must break the chain.
	other := call("write", `{"body":"`+long+`DIFFERENT"}`)
	g.reset()
	g.observe([]ToolCall{c})
	if got := g.observe([]ToolCall{other}); got != "" {
		t.Fatalf("a call differing beyond the preview cap must reset, got %q", got)
	}
}

func TestRepeatGuard_NonJSONArgumentsStillChain(t *testing.T) {
	g := mustGuard(t, &RepeatGuardSettings{Thresholds: []int{3}})
	c := call("t", "not json at all")
	g.observe([]ToolCall{c})
	g.observe([]ToolCall{c})
	if got := g.observe([]ToolCall{c}); got != repeatReminderShort {
		t.Fatalf("unparseable arguments must still chain by raw text, got %q", got)
	}
}

func TestRepeatGuard_RejectsBadThresholds(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   []int
	}{
		{"below two", []int{1}},
		{"zero", []int{0, 3}},
		{"negative", []int{-2}},
		{"duplicate", []int{3, 3}},
	} {
		if _, err := newRepeatGuard(&RepeatGuardSettings{Thresholds: tc.in}); err == nil {
			t.Fatalf("%s: expected a configuration error", tc.name)
		}
	}
	// And New surfaces it rather than constructing a guard that never fires.
	_, err := New(Config{Provider: &FauxProvider{}, Model: "m", RepeatGuard: &RepeatGuardSettings{Thresholds: []int{1}}})
	if err == nil {
		t.Fatal("New must reject an invalid repeat-guard threshold")
	}
}

func TestRepeatGuard_DefaultsApplied(t *testing.T) {
	g := mustGuard(t, &RepeatGuardSettings{})
	if len(g.thresholds) != len(DefaultRepeatThresholds) || g.thresholds[0] != 3 {
		t.Fatalf("thresholds = %v, want %v", g.thresholds, DefaultRepeatThresholds)
	}
	if g.previewChars != defaultArgumentsPreviewChars {
		t.Fatalf("previewChars = %d", g.previewChars)
	}
	// update_plan is excluded by default.
	if g.tracks(ToolUpdatePlan) {
		t.Fatal("update_plan should be excluded by default")
	}
}

func TestRepeatGuard_NilIsOff(t *testing.T) {
	g, err := newRepeatGuard(nil)
	if err != nil || g != nil {
		t.Fatalf("nil settings must yield no guard (got %v, %v)", g, err)
	}
	// A nil guard is safe to call — the loop does not branch on it.
	if got := g.observe([]ToolCall{call("t", "{}")}); got != "" {
		t.Fatalf("nil guard must be silent, got %q", got)
	}
	g.reset()
}

func TestMatchToolPattern(t *testing.T) {
	cases := []struct {
		pattern, name string
		want          bool
	}{
		{"run_sql", "run_sql", true},
		{"run_sql", "run_sqlx", false},
		{"run_*", "run_sql", true},
		{"run_*", "web_fetch", false},
		{"*_sql", "run_sql", true},
		{"*_sql", "run_sqlx", false},
		{"*", "anything", true},
		{"mcp_*", "run_sql", false},
		{"a*c", "abc", true},
		{"a*c", "abd", false},
	}
	for _, c := range cases {
		if got := matchToolPattern(c.pattern, c.name); got != c.want {
			t.Errorf("matchToolPattern(%q, %q) = %v, want %v", c.pattern, c.name, got, c.want)
		}
	}
}
