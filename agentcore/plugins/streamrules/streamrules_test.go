package streamrules

import (
	"context"
	"strings"
	"testing"

	"github.com/lohi-ai/agentray/agentcore"
)

// The plugin half of the contract: which text trips a rule, what the injection
// says, and the per-turn bound that keeps a stubborn model from spinning the
// provider call forever. The loop-side mechanics (cancel, discard, retry,
// persist) are pinned in agentcore/streamintercept_test.go.

func run(t *testing.T, p Plugin) *rulesRun {
	t.Helper()
	ext, err := p.BeginRun(context.Background(), agentcore.RunInfo{Owner: "run_1"})
	if err != nil {
		t.Fatalf("BeginRun: %v", err)
	}
	r, ok := ext.(*rulesRun)
	if !ok {
		t.Fatalf("BeginRun returned %T, want *rulesRun", ext)
	}
	return r
}

// A matching pattern aborts and injects the rule's body as one reminder.
func TestRuleMatchAbortsWithInjection(t *testing.T) {
	r := run(t, Of(Rule{
		Name:    "no-secrets",
		Pattern: `sk-[a-z0-9]+`,
		Body:    "Never print API keys.",
	}))
	d := r.InterceptStreamDelta(context.Background(), "here is the key: sk-abc123")
	if !d.Abort {
		t.Fatal("matching rule did not abort")
	}
	if len(d.Inject) != 1 {
		t.Fatalf("inject = %d messages, want 1", len(d.Inject))
	}
	body := d.Inject[0].Content
	if !strings.Contains(body, `rule="no-secrets"`) || !strings.Contains(body, "Never print API keys.") {
		t.Fatalf("injection does not name the rule and carry its body: %q", body)
	}
	if d.Inject[0].Role != agentcore.RoleUser {
		t.Fatalf("injection role = %q, want user — it is a reminder, not the model's own text", d.Inject[0].Role)
	}
}

// Non-matching text is a pass-through: no abort, no injection.
func TestRuleNoMatchPassesThrough(t *testing.T) {
	r := run(t, Of(Rule{Name: "no-secrets", Pattern: `sk-[a-z0-9]+`, Body: "x"}))
	for _, text := range []string{"", "partial", "partial output with no key"} {
		if d := r.InterceptStreamDelta(context.Background(), text); d.Abort {
			t.Fatalf("non-matching text %q aborted", text)
		}
	}
}

// Every rule that matches the same buffer joins the one injection — the retry
// sees the whole violation set at once.
func TestAllMatchedRulesInjectTogether(t *testing.T) {
	r := run(t, Of(
		Rule{Name: "a", Pattern: `banana`, Body: "no bananas"},
		Rule{Name: "b", Pattern: `forbidden`, Body: "no forbidden things"},
		Rule{Name: "c", Pattern: `nomatch`, Body: "unrelated"},
	))
	d := r.InterceptStreamDelta(context.Background(), "the forbidden banana")
	if !d.Abort {
		t.Fatal("no abort")
	}
	if len(d.Inject) != 1 {
		t.Fatalf("inject = %d messages, want 1 combined reminder", len(d.Inject))
	}
	body := d.Inject[0].Content
	if !strings.Contains(body, `rule="a"`) || !strings.Contains(body, `rule="b"`) {
		t.Fatalf("injection missing a matched rule: %q", body)
	}
	if strings.Contains(body, `rule="c"`) {
		t.Fatalf("injection names a rule that did not match: %q", body)
	}
}

// The per-turn cap is the real bound: once it is spent the stream completes
// unmodified, and the next turn re-arms it.
func TestPerTurnCapBoundsInjections(t *testing.T) {
	r := run(t, Plugin{
		Rules:                []Rule{{Name: "a", Pattern: `banana`, Body: "no"}},
		MaxInjectionsPerTurn: 2,
	})
	for i := range 2 {
		if d := r.InterceptStreamDelta(context.Background(), "banana"); !d.Abort {
			t.Fatalf("injection %d did not fire under the cap", i+1)
		}
	}
	if d := r.InterceptStreamDelta(context.Background(), "banana"); d.Abort {
		t.Fatal("cap reached but the rule still aborted — the stream must pass through")
	}
	// A new turn is new output: the cap resets.
	r.BeforeStep(context.Background(), agentcore.StepInfo{Turn: 2})
	if d := r.InterceptStreamDelta(context.Background(), "banana"); !d.Abort {
		t.Fatal("cap did not reset on the next turn")
	}
}

// A plugin with no rules declines the run rather than adding a per-delta no-op
// to the hot path.
func TestNoRulesDeclinesTheRun(t *testing.T) {
	ext, err := Plugin{}.BeginRun(context.Background(), agentcore.RunInfo{Owner: "run_1"})
	if err != nil {
		t.Fatalf("BeginRun: %v", err)
	}
	if ext != nil {
		t.Fatalf("empty plugin returned %T, want nil (declined)", ext)
	}
}

// Malformed rules fail at composition, not silently at runtime.
func TestInvalidRulesFailComposition(t *testing.T) {
	cases := []struct {
		name string
		p    Plugin
	}{
		{"unnamed", Plugin{Rules: []Rule{{Pattern: "x", Body: "b"}}}},
		{"duplicate name", Plugin{Rules: []Rule{
			{Name: "a", Pattern: "x", Body: "b"},
			{Name: "a", Pattern: "y", Body: "b"},
		}}},
		{"bad regex", Plugin{Rules: []Rule{{Name: "a", Pattern: "[", Body: "b"}}}},
		{"empty body", Plugin{Rules: []Rule{{Name: "a", Pattern: "x", Body: "  "}}}},
		{"negative cap", Plugin{Rules: []Rule{{Name: "a", Pattern: "x", Body: "b"}}, MaxInjectionsPerTurn: -1}},
	}
	for _, tc := range cases {
		if _, err := tc.p.BeginRun(context.Background(), agentcore.RunInfo{Owner: "run_1"}); err == nil {
			t.Fatalf("%s: BeginRun succeeded, want a composition error", tc.name)
		}
	}
}
