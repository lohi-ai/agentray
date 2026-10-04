package bench_test

import (
	"github.com/lohi-ai/agentray/agentcore"
	"github.com/lohi-ai/agentray/telemetry/llm"
	"reflect"
	"testing"
)

// TestFoldRunTable covers the classifications that are awkward to provoke
// through a live loop — aborted calls, a failed provider call, the ghost-run
// predicate, and a tool the model invented that was never advertised.
func TestFoldRunTable(t *testing.T) {
	cases := []struct {
		name         string
		res          agentcore.RunResult
		records      []llm.TraceRecord
		wantTools    ToolCounters
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
			records:     []llm.TraceRecord{{Model: "m", Provider: "faux", Tools: []string{"a", "b"}}},
			wantTools:   ToolCounters{Total: 2, Aborted: 2},
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
			records:     []llm.TraceRecord{{Model: "m", Provider: "faux", Tools: []string{"a", "b", "c"}}},
			wantTools:   ToolCounters{Total: 3, Blocked: 3},
			wantInvoked: []string{"a", "b", "c"},
		},
		{
			name: "a tool the model invented is invoked but was never advertised",
			res: agentcore.RunResult{
				Tools: []agentcore.ToolTrace{{Tool: "hallucinated", Allowed: true, Error: "unknown tool"}},
			},
			records:     []llm.TraceRecord{{Model: "m", Provider: "faux", Tools: []string{"a"}}},
			wantTools:   ToolCounters{Total: 1, Error: 1},
			wantUnused:  []string{"a"},
			wantInvoked: []string{"hallucinated"},
		},
		{
			name: "ghost run: a failed provider call, no tools, no tokens",
			res:  agentcore.RunResult{},
			records: []llm.TraceRecord{
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
			records: []llm.TraceRecord{
				{Model: "m", Provider: "faux", Tools: []string{"a"}, Err: "connection reset"},
				{Model: "m", Provider: "faux", Tools: []string{"a"}},
			},
			wantTools:    ToolCounters{Total: 1, OK: 1},
			wantInvoked:  []string{"a"},
			wantFailedPC: 1,
		},
		{
			name: "not a ghost: cached tokens alone are still billable work",
			res: agentcore.RunResult{
				Usage: agentcore.Usage{CacheReadTokens: 4096},
			},
			records:      []llm.TraceRecord{{Model: "m", Provider: "faux", Err: "connection reset"}},
			wantFailedPC: 1,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			summary, coverage := FoldRun(tc.res, tc.records)
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
	records := []llm.TraceRecord{
		{Depth: 0, Model: "parent-model", Provider: "faux", Tools: []string{"spawn_subagent"}, StopReason: "tool_calls"},
		// The child's own calls, on the same sink.
		{Depth: 1, Model: "child-model", Provider: "faux", Tools: []string{"run_sql", "create_chart"}, StopReason: "stop", Err: "child blew up"},
	}

	summary, coverage := FoldRun(res, records)

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

// TestClassifyToolVocabularyIsClosed pins the mapping every counter depends on.
func TestClassifyToolVocabularyIsClosed(t *testing.T) {
	for _, tc := range []struct {
		trace agentcore.ToolTrace
		want  ToolStatus
	}{
		{agentcore.ToolTrace{Allowed: true}, ToolOK},
		{agentcore.ToolTrace{Allowed: true, Error: "boom"}, ToolError},
		{agentcore.ToolTrace{Allowed: false, Reason: "tool 'x' is not permitted by the current permission scopes"}, ToolBlocked},
		{agentcore.ToolTrace{Allowed: false, Reason: "aborted"}, ToolAborted},
		{agentcore.ToolTrace{Allowed: false, Reason: string(agentcore.ToolDenialAborted)}, ToolAborted},
		// A validation rejection carries an error but never executed, so it is a
		// refusal rather than a tool failure.
		{agentcore.ToolTrace{Allowed: false, Error: "invalid arguments"}, ToolBlocked},
	} {
		if got := ClassifyTool(tc.trace); got != tc.want {
			t.Errorf("ClassifyTool(%+v) = %q, want %q", tc.trace, got, tc.want)
		}
	}
}
