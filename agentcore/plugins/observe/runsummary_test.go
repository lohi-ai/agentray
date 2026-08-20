package observe_test

import (
	"context"
	"reflect"
	"testing"

	"github.com/lohi-ai/agentray/agentcore"
	"github.com/lohi-ai/agentray/agentcore/plugins/observe"
)

// scriptTool is a minimal Tool whose outcome the test picks: it either answers
// or fails.
type scriptTool struct {
	name string
	fail string // non-empty => Run returns this as an error
}

func (s scriptTool) Name() string { return s.name }

func (s scriptTool) Schema() agentcore.ToolSchema {
	return agentcore.ToolSchema{
		Name:        s.name,
		Description: "test tool " + s.name,
		Parameters:  map[string]any{"type": "object", "properties": map[string]any{}},
	}
}

func (s scriptTool) Run(context.Context, string) (string, error) {
	if s.fail != "" {
		return "", errString(s.fail)
	}
	return "ok", nil
}

type errString string

func (e errString) Error() string { return string(e) }

func toolCallResponse(calls ...agentcore.ToolCall) agentcore.ChatResponse {
	return agentcore.ChatResponse{
		Message:    agentcore.Message{Role: agentcore.RoleAssistant, ToolCalls: calls},
		StopReason: "tool_calls",
		Usage:      agentcore.Usage{InputTokens: 10, OutputTokens: 5},
	}
}

// TestFoldRunSeesBlockedCallsAndCoverage is the regression test for the two
// defects this file closes, driven through a REAL run so the facts come from
// the loop rather than from the test's imagination.
//
// It fails against the old hook-fed rollup on both counts: an After hook runs
// post-gate, so `denied_tool` was counted nowhere at all, and nothing anywhere
// computed advertised-vs-invoked, so `unused_tool` — paid for in schema tokens
// on every request — was invisible.
func TestFoldRunSeesBlockedCallsAndCoverage(t *testing.T) {
	prov := agentcore.NewFauxProvider(
		// One turn asking for two calls at once: one succeeds, one fails.
		toolCallResponse(
			agentcore.ToolCall{ID: "c1", Name: "ok_tool", Arguments: "{}"},
			agentcore.ToolCall{ID: "c2", Name: "broken_tool", Arguments: "{}"},
		),
		// A second turn asking for the tool a consumer hook refuses.
		toolCallResponse(agentcore.ToolCall{ID: "c3", Name: "denied_tool", Arguments: "{}"}),
		agentcore.ChatResponse{
			Message:    agentcore.Message{Role: agentcore.RoleAssistant, Content: "finished"},
			StopReason: "stop",
			Usage:      agentcore.Usage{InputTokens: 10, OutputTokens: 5},
		},
	)

	sink := &rungSink{}
	agent, err := agentcore.New(agentcore.Config{
		Provider: observe.Wrap(prov, observe.Pricing{}, sink),
		Model:    "test-model",
		Tools: agentcore.NewToolSet(
			scriptTool{name: "ok_tool"},
			scriptTool{name: "broken_tool", fail: "boom"},
			scriptTool{name: "denied_tool"},
			scriptTool{name: "unused_tool"},
		),
		// Every tool is permitted (so every tool is ADVERTISED, and paid for)…
		Policy: agentcore.NewAllowList("ok_tool", "broken_tool", "denied_tool", "unused_tool"),
		// …but a consumer gate refuses one of them at call time. This is the
		// shape the After hook cannot see.
		Hooks: agentcore.Hooks{
			Before: []agentcore.BeforeToolCall{func(_ context.Context, call agentcore.ToolCall) agentcore.Decision {
				if call.Name == "denied_tool" {
					return agentcore.Blocked("not permitted by this test's gate")
				}
				return agentcore.Allowed()
			}},
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	res, err := agent.Prompt(context.Background(), "go")
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}

	summary, coverage := observe.FoldRun(res, sink.records)

	if summary.Runs != 1 {
		t.Fatalf("Runs = %d, want 1", summary.Runs)
	}
	wantTools := observe.ToolCounters{Total: 3, OK: 1, Error: 1, Blocked: 1}
	got := summary.Tools
	got.TotalLatencyMS = 0 // wall-clock, not assertable
	if got != wantTools {
		t.Fatalf("Tools = %+v, want %+v", got, wantTools)
	}
	for name, want := range map[string]observe.ToolCounters{
		"ok_tool":     {Total: 1, OK: 1},
		"broken_tool": {Total: 1, Error: 1},
		"denied_tool": {Total: 1, Blocked: 1},
	} {
		c := summary.ByTool[name]
		c.TotalLatencyMS = 0
		if c != want {
			t.Errorf("ByTool[%q] = %+v, want %+v", name, c, want)
		}
	}
	if _, ok := summary.ByTool["unused_tool"]; ok {
		t.Errorf("ByTool has an entry for a tool that was never called: %+v", summary.ByTool)
	}
	if summary.ProviderCalls != 3 {
		t.Errorf("ProviderCalls = %d, want 3", summary.ProviderCalls)
	}
	if summary.FailedProviderCalls != 0 {
		t.Errorf("FailedProviderCalls = %d, want 0", summary.FailedProviderCalls)
	}
	if summary.Ghost {
		t.Errorf("a run that did real work was classified as a ghost")
	}
	if want := map[string]int{"tool_calls": 2, "stop": 1}; !reflect.DeepEqual(summary.ByStopReason, want) {
		t.Errorf("ByStopReason = %v, want %v", summary.ByStopReason, want)
	}

	wantCoverage := observe.RunCoverage{
		ToolsAvailable: []string{"broken_tool", "denied_tool", "ok_tool", "unused_tool"},
		ToolsInvoked:   []string{"broken_tool", "denied_tool", "ok_tool"},
		ToolsUnused:    []string{"unused_tool"},
		ModelsUsed:     []string{"test-model"},
		ProvidersUsed:  []string{"faux"},
	}
	if !reflect.DeepEqual(coverage, wantCoverage) {
		t.Fatalf("coverage = %+v, want %+v", coverage, wantCoverage)
	}
}

// TestFoldRunTable covers the classifications that are awkward to provoke
// through a live loop — aborted calls, a failed provider call, the ghost-run
// predicate, and a tool the model invented that was never advertised.
func TestFoldRunTable(t *testing.T) {
	cases := []struct {
		name         string
		res          agentcore.RunResult
		records      []observe.TraceRecord
		wantTools    observe.ToolCounters
		wantGhost    bool
		wantUnused   []string
		wantInvoked  []string
		wantFailedPC int
	}{
		{
			name: "aborted calls are their own status, not errors",
			res: agentcore.RunResult{
				Turns: 1,
				Tools: []agentcore.ToolTrace{
					{Tool: "a", Allowed: false, Reason: "aborted"},
					{Tool: "b", Allowed: false, Reason: "aborted"},
				},
			},
			records:     []observe.TraceRecord{{Model: "m", Provider: "faux", Tools: []string{"a", "b"}}},
			wantTools:   observe.ToolCounters{Total: 2, Aborted: 2},
			wantInvoked: []string{"a", "b"},
		},
		{
			name: "budget exhaustion and circuit-breaker refusals are blocked",
			res: agentcore.RunResult{
				Tools: []agentcore.ToolTrace{
					{Tool: "a", Allowed: false, Reason: "tool-call budget exhausted"},
					{Tool: "b", Allowed: false, Reason: "disabled after repeated failures"},
					{Tool: "c", Allowed: false, Error: "invalid arguments"},
				},
			},
			records:     []observe.TraceRecord{{Model: "m", Provider: "faux", Tools: []string{"a", "b", "c"}}},
			wantTools:   observe.ToolCounters{Total: 3, Blocked: 3},
			wantInvoked: []string{"a", "b", "c"},
		},
		{
			name: "a tool the model invented is invoked but was never advertised",
			res: agentcore.RunResult{
				Tools: []agentcore.ToolTrace{{Tool: "hallucinated", Allowed: true, Error: "unknown tool"}},
			},
			records:     []observe.TraceRecord{{Model: "m", Provider: "faux", Tools: []string{"a"}}},
			wantTools:   observe.ToolCounters{Total: 1, Error: 1},
			wantUnused:  []string{"a"},
			wantInvoked: []string{"hallucinated"},
		},
		{
			name: "ghost run: a failed provider call, no tools, no tokens",
			res:  agentcore.RunResult{},
			records: []observe.TraceRecord{
				{Model: "m", Provider: "faux", Tools: []string{"a"}, Err: "connection reset"},
			},
			wantGhost:    true,
			wantUnused:   []string{"a"},
			wantFailedPC: 1,
		},
		{
			name: "not a ghost: the provider failed but the run still did work",
			res: agentcore.RunResult{
				Tools: []agentcore.ToolTrace{{Tool: "a", Allowed: true}},
				Usage: agentcore.Usage{InputTokens: 100},
			},
			records: []observe.TraceRecord{
				{Model: "m", Provider: "faux", Tools: []string{"a"}, Err: "connection reset"},
				{Model: "m", Provider: "faux", Tools: []string{"a"}},
			},
			wantTools:    observe.ToolCounters{Total: 1, OK: 1},
			wantInvoked:  []string{"a"},
			wantFailedPC: 1,
		},
		{
			name: "not a ghost: cached tokens alone are still billable work",
			res: agentcore.RunResult{
				Usage: agentcore.Usage{CacheReadTokens: 4096},
			},
			records:      []observe.TraceRecord{{Model: "m", Provider: "faux", Err: "connection reset"}},
			wantFailedPC: 1,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			summary, coverage := observe.FoldRun(tc.res, tc.records)
			if summary.Tools != tc.wantTools {
				t.Errorf("Tools = %+v, want %+v", summary.Tools, tc.wantTools)
			}
			if summary.Ghost != tc.wantGhost {
				t.Errorf("Ghost = %v, want %v", summary.Ghost, tc.wantGhost)
			}
			if want := boolToInt(tc.wantGhost); summary.GhostRuns != want {
				t.Errorf("GhostRuns = %d, want %d", summary.GhostRuns, want)
			}
			if summary.FailedProviderCalls != tc.wantFailedPC {
				t.Errorf("FailedProviderCalls = %d, want %d", summary.FailedProviderCalls, tc.wantFailedPC)
			}
			if !reflect.DeepEqual(coverage.ToolsUnused, tc.wantUnused) {
				t.Errorf("ToolsUnused = %v, want %v", coverage.ToolsUnused, tc.wantUnused)
			}
			if !reflect.DeepEqual(coverage.ToolsInvoked, tc.wantInvoked) {
				t.Errorf("ToolsInvoked = %v, want %v", coverage.ToolsInvoked, tc.wantInvoked)
			}
		})
	}
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// TestFoldRunIgnoresDelegatedCalls guards the agentray-specific hazard omp has
// no equivalent of: a parent and its spawned children share one provider and
// one Sink, so an unfiltered fold would report the CHILD's advertised tools as
// the parent's, and the child's untouched ones as the parent's ToolsUnused —
// confidently wrong governance advice about a preset's scope list.
func TestFoldRunIgnoresDelegatedCalls(t *testing.T) {
	res := agentcore.RunResult{
		Turns: 1,
		Tools: []agentcore.ToolTrace{{Tool: "spawn_subagent", Allowed: true}},
		Usage: agentcore.Usage{InputTokens: 10},
	}
	records := []observe.TraceRecord{
		{Depth: 0, Model: "parent-model", Provider: "faux", Tools: []string{"spawn_subagent"}, StopReason: "tool_calls"},
		// The child's own calls, on the same sink.
		{Depth: 1, Model: "child-model", Provider: "faux", Tools: []string{"run_sql", "create_chart"}, StopReason: "stop", Err: "child blew up"},
	}

	summary, coverage := observe.FoldRun(res, records)

	if summary.ProviderCalls != 1 {
		t.Errorf("ProviderCalls = %d, want 1 (the child's call is not the parent's)", summary.ProviderCalls)
	}
	if summary.FailedProviderCalls != 0 {
		t.Errorf("FailedProviderCalls = %d, want 0 (the child's failure is not the parent's)", summary.FailedProviderCalls)
	}
	if want := []string{"spawn_subagent"}; !reflect.DeepEqual(coverage.ToolsAvailable, want) {
		t.Errorf("ToolsAvailable = %v, want %v", coverage.ToolsAvailable, want)
	}
	if coverage.ToolsUnused != nil {
		t.Errorf("ToolsUnused = %v, want none — the child's tools leaked into the parent", coverage.ToolsUnused)
	}
	if want := []string{"parent-model"}; !reflect.DeepEqual(coverage.ModelsUsed, want) {
		t.Errorf("ModelsUsed = %v, want %v", coverage.ModelsUsed, want)
	}
}

// TestAggregateRunSummaries folds N runs into the same shape, which is what
// lets a repeated bench task (game run1 + run2) become one row.
func TestAggregateRunSummaries(t *testing.T) {
	a := observe.RunSummary{
		Runs: 1, Turns: 3, ProviderCalls: 4, FailedProviderCalls: 1,
		ByStopReason: map[string]int{"stop": 1, "tool_calls": 3},
		Tools:        observe.ToolCounters{Total: 2, OK: 1, Blocked: 1},
		ByTool: map[string]observe.ToolCounters{
			"run_sql":     {Total: 1, OK: 1},
			"create_user": {Total: 1, Blocked: 1},
		},
	}
	b := observe.RunSummary{
		Runs: 1, Turns: 2, ProviderCalls: 2,
		ByStopReason: map[string]int{"stop": 1, "tool_calls": 1},
		Tools:        observe.ToolCounters{Total: 1, Error: 1},
		ByTool:       map[string]observe.ToolCounters{"run_sql": {Total: 1, Error: 1}},
	}
	ghost := observe.RunSummary{Runs: 1, ProviderCalls: 1, FailedProviderCalls: 1, Ghost: true, GhostRuns: 1}

	got := observe.AggregateRunSummaries(a, b, ghost)

	if got.Runs != 3 || got.GhostRuns != 1 {
		t.Fatalf("Runs/GhostRuns = %d/%d, want 3/1", got.Runs, got.GhostRuns)
	}
	if got.Ghost {
		t.Errorf("a rollup with two real runs in it must not be a ghost")
	}
	// The score denominator the ghost classifier exists to protect.
	if scoreable := got.Runs - got.GhostRuns; scoreable != 2 {
		t.Errorf("scoreable runs = %d, want 2", scoreable)
	}
	if got.Turns != 5 || got.ProviderCalls != 7 || got.FailedProviderCalls != 2 {
		t.Errorf("turns/calls/failed = %d/%d/%d, want 5/7/2", got.Turns, got.ProviderCalls, got.FailedProviderCalls)
	}
	if want := (observe.ToolCounters{Total: 3, OK: 1, Error: 1, Blocked: 1}); got.Tools != want {
		t.Errorf("Tools = %+v, want %+v", got.Tools, want)
	}
	if want := (observe.ToolCounters{Total: 2, OK: 1, Error: 1}); got.ByTool["run_sql"] != want {
		t.Errorf("ByTool[run_sql] = %+v, want %+v", got.ByTool["run_sql"], want)
	}
	if want := map[string]int{"stop": 2, "tool_calls": 4}; !reflect.DeepEqual(got.ByStopReason, want) {
		t.Errorf("ByStopReason = %v, want %v", got.ByStopReason, want)
	}

	// Same shape in, same shape out: folding in any grouping is identical.
	regrouped := observe.AggregateRunSummaries(observe.AggregateRunSummaries(a, b), ghost)
	if !reflect.DeepEqual(regrouped, got) {
		t.Errorf("aggregate is not associative:\n got %+v\nwant %+v", regrouped, got)
	}

	// An all-ghost rollup is itself a ghost: there is nothing real to score.
	if !observe.AggregateRunSummaries(ghost, ghost).Ghost {
		t.Errorf("a rollup of nothing but ghosts must classify as a ghost")
	}
	if observe.AggregateRunSummaries().Ghost {
		t.Errorf("an empty rollup must not classify as a ghost")
	}
}

// TestAggregateRunCoverageRecomputesUnused is the one place the aggregate is
// NOT a plain union: a tool unused in run 1 and called in run 2 is not unused
// across the pair, and unioning the per-run answers would claim it was.
func TestAggregateRunCoverageRecomputesUnused(t *testing.T) {
	first := observe.RunCoverage{
		ToolsAvailable: []string{"create_chart", "explore_events", "run_sql"},
		ToolsInvoked:   []string{"run_sql"},
		ToolsUnused:    []string{"create_chart", "explore_events"},
		ModelsUsed:     []string{"model-a"},
		ProvidersUsed:  []string{"faux"},
	}
	second := observe.RunCoverage{
		ToolsAvailable: []string{"create_chart", "explore_events", "run_sql"},
		ToolsInvoked:   []string{"create_chart", "run_sql"},
		ToolsUnused:    []string{"explore_events"},
		ModelsUsed:     []string{"model-b"},
		ProvidersUsed:  []string{"faux"},
	}

	got := observe.AggregateRunCoverage(first, second)
	want := observe.RunCoverage{
		ToolsAvailable: []string{"create_chart", "explore_events", "run_sql"},
		ToolsInvoked:   []string{"create_chart", "run_sql"},
		ToolsUnused:    []string{"explore_events"},
		ModelsUsed:     []string{"model-a", "model-b"},
		ProvidersUsed:  []string{"faux"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("aggregate coverage = %+v, want %+v", got, want)
	}
	if regrouped := observe.AggregateRunCoverage(observe.AggregateRunCoverage(first), second); !reflect.DeepEqual(regrouped, got) {
		t.Errorf("coverage aggregate is not associative:\n got %+v\nwant %+v", regrouped, got)
	}
}

// TestClassifyToolVocabularyIsClosed pins the mapping every counter depends on.
func TestClassifyToolVocabularyIsClosed(t *testing.T) {
	for _, tc := range []struct {
		trace agentcore.ToolTrace
		want  observe.ToolStatus
	}{
		{agentcore.ToolTrace{Allowed: true}, observe.ToolOK},
		{agentcore.ToolTrace{Allowed: true, Error: "boom"}, observe.ToolError},
		{agentcore.ToolTrace{Allowed: false, Reason: "tool 'x' is not permitted by the current permission scopes"}, observe.ToolBlocked},
		{agentcore.ToolTrace{Allowed: false, Reason: "aborted"}, observe.ToolAborted},
		{agentcore.ToolTrace{Allowed: false, Reason: string(agentcore.ToolDenialAborted)}, observe.ToolAborted},
		// A validation rejection carries an error but never executed, so it is a
		// refusal rather than a tool failure.
		{agentcore.ToolTrace{Allowed: false, Error: "invalid arguments"}, observe.ToolBlocked},
	} {
		if got := observe.ClassifyTool(tc.trace); got != tc.want {
			t.Errorf("ClassifyTool(%+v) = %q, want %q", tc.trace, got, tc.want)
		}
	}
}
