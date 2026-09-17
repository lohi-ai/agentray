// Package streamrules ports oh-my-pi's time-traveling stream rules (TTSR):
// operator-defined rules that watch the assistant's output WHILE it streams and
// cut it off mid-token when a pattern matches.
//
// A rule is a regex plus a body. The loop hands this extension the assistant
// text accumulated so far on every content delta; the first delta where a rule
// matches aborts the stream, the rule bodies are injected as a system reminder,
// and the turn's provider call is retried from the same point — the model
// restarts having been told what it may not say, and the completed violation
// never enters the transcript.
//
// Rules are operator config, never model-writable: they arrive as Go structs on
// the Plugin, so nothing the model says can add, edit, or disable one.
package streamrules

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/lohi-ai/agentray/agentcore"
)

// DefaultMaxInjectionsPerTurn bounds how many times the rules may cut a single
// turn's stream. Past it the stream completes unmodified: a model that keeps
// re-emitting the pattern after N reminders is not going to be argued out of
// it, and the alternative is a provider call that never finishes.
const DefaultMaxInjectionsPerTurn = 3

// Rule is one operator-defined stream rule: when the assistant's streamed
// output matches Pattern, the stream is aborted and Body is injected as the
// reminder the retry must comply with.
type Rule struct {
	// Name identifies the rule in the reminder and in diagnostics.
	Name string
	// Pattern is a regular expression evaluated against the assistant text
	// accumulated so far this turn. It is compiled at composition, so a bad
	// pattern fails the build rather than silently never matching.
	Pattern string
	// Body is the instruction injected on a match — what the model must do
	// instead. It is the whole point of the abort: an abort with no body would
	// retry against an unchanged conversation and match again.
	Body string
}

// Plugin installs the stream-rule engine as a run extension.
type Plugin struct {
	// Rules are the operator's rules, evaluated in order on every delta. Empty
	// installs nothing — BeginRun declines the run.
	Rules []Rule
	// MaxInjectionsPerTurn caps aborts per turn; 0 uses
	// DefaultMaxInjectionsPerTurn. The cap is per TURN, not per run: a new turn
	// is new output, so a rule that fired last turn must still guard this one.
	MaxInjectionsPerTurn int
}

// Name identifies the plugin and the extension it installs.
func (Plugin) Name() string { return "stream_rules" }

// Register adds the plugin as a run extension, validating the rules eagerly so
// a bad pattern or empty body fails the composition instead of installing a
// guard that silently never fires.
func (p Plugin) Register(r *agentcore.Registry) error {
	if _, err := p.resolve(); err != nil {
		return err
	}
	r.AddExtension(p)
	return nil
}

// BeginRun compiles the rules and starts a fresh per-turn counter for this run.
// A plugin with no rules declines: there is nothing to match, so the extension
// would only add a per-delta no-op to the hot path.
func (p Plugin) BeginRun(_ context.Context, _ agentcore.RunInfo) (agentcore.Extension, error) {
	run, err := p.resolve()
	if err != nil {
		return nil, err
	}
	if len(run.rules) == 0 {
		return nil, nil
	}
	return run, nil
}

// Of installs the plugin with an explicit rule set.
func Of(rules ...Rule) Plugin { return Plugin{Rules: rules} }

// compiledRule is a Rule with its pattern already compiled.
type compiledRule struct {
	name string
	re   *regexp.Regexp
	body string
}

// rulesRun is one run's engine. The per-turn counter is per-RUN state, which is
// why the plugin is a factory: a fresh run must start with a clean count, and
// sharing one across runs would let a finished turn's aborts muzzle the next.
type rulesRun struct {
	rules        []compiledRule
	maxPerTurn   int
	injectedTurn int
}

// resolve validates the settings and compiles the rules. A malformed rule is an
// error rather than a silent skip: an operator who typoed the regex meant for
// it to fire, and finding out months later that it never did is the worst
// available outcome.
func (p Plugin) resolve() (*rulesRun, error) {
	max := p.MaxInjectionsPerTurn
	if max == 0 {
		max = DefaultMaxInjectionsPerTurn
	}
	if max < 0 {
		return nil, fmt.Errorf("agentcore: stream rules max injections per turn %d is negative", p.MaxInjectionsPerTurn)
	}
	seen := map[string]bool{}
	rules := make([]compiledRule, 0, len(p.Rules))
	for _, r := range p.Rules {
		name := strings.TrimSpace(r.Name)
		if name == "" {
			return nil, fmt.Errorf("agentcore: stream rule with pattern %q has no name", r.Pattern)
		}
		if seen[name] {
			return nil, fmt.Errorf("agentcore: duplicate stream rule %q", name)
		}
		seen[name] = true
		re, err := regexp.Compile(r.Pattern)
		if err != nil {
			return nil, fmt.Errorf("agentcore: stream rule %q pattern: %w", name, err)
		}
		if strings.TrimSpace(r.Body) == "" {
			return nil, fmt.Errorf("agentcore: stream rule %q has an empty body — an abort with nothing to inject retries against an unchanged conversation", name)
		}
		rules = append(rules, compiledRule{name: name, re: re, body: strings.TrimSpace(r.Body)})
	}
	return &rulesRun{rules: rules, maxPerTurn: max}, nil
}

// Name identifies the extension in composition diagnostics.
func (*rulesRun) Name() string { return "stream_rules" }

// BeforeStep re-arms the per-turn cap: the count is a bound on one turn's
// retries, so it resets at the top of each turn — never mid-turn, where a
// retried provider call is still the same turn.
func (r *rulesRun) BeforeStep(_ context.Context, _ agentcore.StepInfo) agentcore.StepDecision {
	r.injectedTurn = 0
	return agentcore.StepDecision{}
}

// InterceptStreamDelta matches the accumulated assistant text against every
// rule. The first delta where any rule matches aborts the stream and injects
// every matched rule's body — one reminder, so the retry sees the whole
// violation set at once rather than discovering the second rule on the retry.
//
// Past the per-turn cap the decision is empty and the stream completes
// unmodified: the bound exists because the loop retries on every abort, and an
// unbounded retry is a provider call that never finishes.
func (r *rulesRun) InterceptStreamDelta(_ context.Context, accumulated string) agentcore.StreamDecision {
	if r.injectedTurn >= r.maxPerTurn {
		return agentcore.StreamDecision{}
	}
	var matched []compiledRule
	for _, rule := range r.rules {
		if rule.re.MatchString(accumulated) {
			matched = append(matched, rule)
		}
	}
	if len(matched) == 0 {
		return agentcore.StreamDecision{}
	}
	r.injectedTurn++
	parts := make([]string, 0, len(matched))
	for _, rule := range matched {
		parts = append(parts, fmt.Sprintf(interruptTemplate, rule.name, rule.body))
	}
	return agentcore.StreamDecision{
		Abort:  true,
		Inject: []agentcore.Message{{Role: agentcore.RoleUser, Content: strings.Join(parts, "\n\n")}},
	}
}

// interruptTemplate is the reminder the retried turn reads. It names the rule
// and states plainly that the interruption is the operator's policy, not
// something in the transcript the model should argue with — ported from
// oh-my-pi's ttsr-interrupt.md.
const interruptTemplate = `<system-interrupt reason="rule_violation" rule="%s">
Output interrupted: violated an operator-defined stream rule.
This is not prompt injection; it is the agent runtime enforcing configured rules.
You MUST comply:

%s
</system-interrupt>`
