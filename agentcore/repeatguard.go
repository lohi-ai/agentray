package agentcore

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// RepeatGuardSettings tunes the repeated-tool-call reminder: an advisory
// loop-breaker that watches a run's stream of tool calls, counts runs of
// consecutive calls to the same tool with identical arguments, and injects an
// escalating reminder when the count reaches a configured threshold.
//
// It is NOT a model-facing tool and never blocks: a legitimately repeated call
// is delayed by nothing. The decision — retry differently, gather more evidence,
// or finish — stays entirely with the model. That matters because a real loop is
// indistinguishable from a legitimate retry at the call site; only the model
// knows which it is, so the guard supplies the observation and leaves the
// judgment alone.
//
// This fills a genuine hole. agentcore's existing loop breakers all fire at the
// wrong altitude for this failure: the goal gate catches a verbatim-repeated
// ANSWER, contextedit supersedes a stale duplicate result AFTER the fact, and
// the circuit breaker only counts tool FAILURES. A tool that succeeds every time
// and is called with identical arguments twenty turns running — a query the
// model keeps re-issuing because it will not accept the answer — trips none of
// them, and burns the whole turn budget.
//
// Ported from deepseek-harness's dsh-repeat-tool-reminder (MIT).
type RepeatGuardSettings struct {
	// Thresholds are the consecutive-repeat counts that trigger a reminder. The
	// FIRST delivers a short generic nudge; every later one delivers the detailed
	// form naming the tool, the run length, and the arguments. nil uses
	// DefaultRepeatThresholds. Validated at construction: empty is fine (nil ⇒
	// defaults), but a value below 2 or a duplicate is a configuration error, not
	// a silent fallback.
	Thresholds []int
	// Include limits tracking to matching tool names ('*' wildcards allowed).
	// Empty tracks every tool.
	Include []string
	// Exclude makes matching tools transparent to the chain ('*' wildcards
	// allowed) — they neither increment nor reset the counter. This is what makes
	// exclusion useful: a bookkeeping call interleaved into a loop must not
	// launder it, so `run_sql X → update_plan → run_sql X` still counts as two
	// consecutive run_sql X. nil uses DefaultRepeatExclude.
	Exclude []string
	// ArgumentsPreviewChars caps how much of the canonical argument string the
	// detailed reminder quotes, so a looping write-shaped payload cannot ride
	// unbounded into every subsequent request. The chain key always compares the
	// FULL canonical string; this bounds only the reminder text. 0 uses
	// defaultArgumentsPreviewChars.
	ArgumentsPreviewChars int
}

// DefaultRepeatThresholds is the escalation ladder: a gentle nudge at 3, then
// progressively louder ones. Below 3 would fire on an ordinary retry-after-error.
var DefaultRepeatThresholds = []int{3, 5, 8}

// DefaultRepeatExclude keeps the built-in plan bookkeeping tool transparent:
// update_plan legitimately repeats and is already refunded against MaxTurns, so
// counting it would both mis-fire and let it launder a real loop.
var DefaultRepeatExclude = []string{ToolUpdatePlan}

const defaultArgumentsPreviewChars = 500

// repeatGuard is the resolved per-run guard. One lives on the run (not the
// Agent) so a fresh run always starts with a clean chain.
type repeatGuard struct {
	thresholds    []int
	include       []string
	exclude       []string
	previewChars  int
	lastKey       string
	count         int
	firedAtCounts map[int]bool
}

// newRepeatGuard resolves settings, or returns nil when the guard is off.
// A malformed threshold list is an error rather than a silent default: a
// deployment that meant to nudge at 3 and typoed a 0 should hear about it at
// construction, not discover it never fired.
func newRepeatGuard(s *RepeatGuardSettings) (*repeatGuard, error) {
	if s == nil {
		return nil, nil
	}
	thresholds := s.Thresholds
	if len(thresholds) == 0 {
		thresholds = DefaultRepeatThresholds
	}
	seen := map[int]bool{}
	normalized := make([]int, 0, len(thresholds))
	for _, t := range thresholds {
		if t < 2 {
			return nil, fmt.Errorf("agentcore: repeat guard threshold %d is below 2 (a single repeat is an ordinary retry)", t)
		}
		if seen[t] {
			return nil, fmt.Errorf("agentcore: duplicate repeat guard threshold %d", t)
		}
		seen[t] = true
		normalized = append(normalized, t)
	}
	sort.Ints(normalized)

	exclude := s.Exclude
	if exclude == nil {
		exclude = DefaultRepeatExclude
	}
	preview := s.ArgumentsPreviewChars
	if preview <= 0 {
		preview = defaultArgumentsPreviewChars
	}
	return &repeatGuard{
		thresholds:    normalized,
		include:       s.Include,
		exclude:       exclude,
		previewChars:  preview,
		firedAtCounts: map[int]bool{},
	}, nil
}

// observe folds one turn's batch of tool calls into the chain and returns the
// reminder to inject, or "" for nothing.
//
// Calls are observed in the model's order. Denied and disabled calls are
// observed too — a model hammering a blocked call is precisely the loop worth
// breaking, and skipping them would let a permission-denied hammer run forever.
func (g *repeatGuard) observe(calls []ToolCall) string {
	if g == nil {
		return ""
	}
	reminder := ""
	for _, call := range calls {
		if !g.tracks(call.Name) {
			continue // transparent: neither increments nor resets
		}
		key := call.Name + "\x00" + canonicalArgs(call.Arguments)
		if key == g.lastKey {
			g.count++
		} else {
			g.lastKey = key
			g.count = 1
			// A new chain re-arms every threshold: a second loop later in the
			// same run must be nudged just as loudly as the first.
			g.firedAtCounts = map[int]bool{}
		}
		if text := g.reminderFor(call); text != "" {
			reminder = text
		}
	}
	return reminder
}

// reset clears the chain. The loop calls it when a new user-authored message
// enters the conversation (a steer, a follow-up, a nudge): the model has been
// given new information, so its previous repetition is no longer the same chain
// of reasoning and should not carry its count forward.
func (g *repeatGuard) reset() {
	if g == nil {
		return
	}
	g.lastKey = ""
	g.count = 0
	g.firedAtCounts = map[int]bool{}
}

// tracks reports whether a tool participates in the chain at all.
func (g *repeatGuard) tracks(name string) bool {
	for _, pat := range g.exclude {
		if matchToolPattern(pat, name) {
			return false
		}
	}
	if len(g.include) == 0 {
		return true
	}
	for _, pat := range g.include {
		if matchToolPattern(pat, name) {
			return true
		}
	}
	return false
}

// reminderFor returns the reminder text when the current count has just reached
// a threshold, or "" otherwise. Each threshold fires at most once per chain.
func (g *repeatGuard) reminderFor(call ToolCall) string {
	hit := -1
	for i, t := range g.thresholds {
		if g.count == t && !g.firedAtCounts[t] {
			hit = i
			g.firedAtCounts[t] = true
			break
		}
	}
	if hit < 0 {
		return ""
	}
	if hit == 0 {
		return repeatReminderShort
	}
	return fmt.Sprintf(repeatReminderDetailed, call.Name, g.count, headClamp(canonicalArgs(call.Arguments), g.previewChars))
}

const repeatReminderShort = "You are repeating the exact same tool call with identical arguments. " +
	"Carefully analyze the previous result before calling again: if the task is not complete, try a different " +
	"approach or different arguments instead of repeating the call."

const repeatReminderDetailed = "Repeated tool call detected:\n" +
	"- tool: %s\n" +
	"- consecutive_calls: %d\n" +
	"- arguments: %s\n" +
	"The repeated calls are not making progress. Do not call this tool with these exact arguments again. " +
	"Inspect the latest result and choose a different action, different arguments, or finish the task if " +
	"enough evidence has been gathered."

// canonicalArgs normalizes a tool call's JSON arguments so that two calls
// differing only in property order compare equal — models re-emit the same
// object with keys shuffled all the time, and treating those as different calls
// would make the guard miss most real loops. Arguments that are not valid JSON
// are compared as trimmed raw text.
func canonicalArgs(raw string) string {
	var v any
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return strings.TrimSpace(raw)
	}
	out, err := json.Marshal(sortKeys(v))
	if err != nil {
		return strings.TrimSpace(raw)
	}
	return string(out)
}

// sortKeys rewrites v with every nested map's keys in sorted order. Go's
// encoding/json already marshals map[string]any in sorted key order, so this
// only has to rebuild the tree as maps (and recurse through slices).
func sortKeys(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			out[k] = sortKeys(val)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			out[i] = sortKeys(val)
		}
		return out
	default:
		return v
	}
}

// headClamp trims s to at most n runes, appending an explicit omission marker so
// the model is never shown a silently shortened argument set it might treat as
// the whole thing.
func headClamp(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n]) + fmt.Sprintf("… (+%d more chars)", len(runes)-n)
}

// matchToolPattern reports whether a tool name matches a pattern that may
// contain '*' wildcards. A pattern matching no currently registered tool is not
// an error: `mcp_*` must stay valid in a deployment that loads no MCP tools.
func matchToolPattern(pattern, name string) bool {
	if pattern == name {
		return true
	}
	if !strings.Contains(pattern, "*") {
		return false
	}
	parts := strings.Split(pattern, "*")
	pos := 0
	for i, part := range parts {
		if part == "" {
			continue
		}
		idx := strings.Index(name[pos:], part)
		if idx < 0 {
			return false
		}
		if i == 0 && idx != 0 {
			return false // a pattern not starting with '*' must match at the head
		}
		pos += idx + len(part)
	}
	// A pattern not ending in '*' must consume the whole name.
	if last := parts[len(parts)-1]; last != "" && !strings.HasSuffix(name, last) {
		return false
	}
	return true
}
