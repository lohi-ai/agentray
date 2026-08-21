package agentruntime

import (
	"fmt"
	"strings"
	"testing"

	"github.com/lohi-ai/agentray/agentcore"
)

// TestCeilingOverridesReachTheRun walks the last hop of the operator knob: a
// BuildParams ceiling has to land in the agent's agentcore.Limits, or the env
// var an operator exported is a number nothing reads.
//
// The "turns only" and "tool calls only" rows are the defect. Limits used to be
// built only when MaxContextTokens was set, so an operator who raised just the
// turn ceiling got a run that still stopped where it always had — with no error,
// no log line, and a config file that looked applied.
func TestCeilingOverridesReachTheRun(t *testing.T) {
	def := agentcore.DefaultLimits()
	for _, tc := range []struct {
		name             string
		turns            int
		toolCalls        int
		contextTokens    int
		wantTurns        int
		wantToolCalls    int
		wantContextToken int
	}{
		{
			name:      "no override keeps every agentcore default",
			wantTurns: def.MaxTurns, wantToolCalls: def.MaxToolCalls, wantContextToken: def.MaxContextTokens,
		},
		{
			name:      "turns only — the override that used to be dropped",
			turns:     60,
			wantTurns: 60, wantToolCalls: def.MaxToolCalls, wantContextToken: def.MaxContextTokens,
		},
		{
			name:      "tool calls only — dropped by the same shape",
			toolCalls: 100,
			wantTurns: def.MaxTurns, wantToolCalls: 100, wantContextToken: def.MaxContextTokens,
		},
		{
			name:          "context budget only — the path that always worked",
			contextTokens: 50000,
			wantTurns:     def.MaxTurns, wantToolCalls: def.MaxToolCalls, wantContextToken: 50000,
		},
		{
			name:  "all three, and none of them clobbers another",
			turns: 60, toolCalls: 100, contextTokens: 50000,
			wantTurns: 60, wantToolCalls: 100, wantContextToken: 50000,
		},
		{
			name:  "zero is the sentinel for 'keep the default', not a ceiling of zero",
			turns: 0, toolCalls: 0, contextTokens: 0,
			wantTurns: def.MaxTurns, wantToolCalls: def.MaxToolCalls, wantContextToken: def.MaxContextTokens,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := representativeBuildParams()
			p.MaxTurns, p.MaxToolCalls, p.MaxContextTokens = tc.turns, tc.toolCalls, tc.contextTokens
			p.KeepRecentTokens = 0 // compaction keeps its own default; only the ceilings are under test

			agent, err := Build(p)
			if err != nil {
				t.Fatalf("Build: %v", err)
			}
			want := fmt.Sprintf("turns=%d tools=%d ctx=%d result=%d",
				tc.wantTurns, tc.wantToolCalls, tc.wantContextToken, def.MaxToolResultLen)
			if got := limitsLineOf(agent.Describe()); got != want {
				t.Fatalf("the run's limits are not what the operator asked for\n got: %s\nwant: %s", got, want)
			}
		})
	}
}

// TestRunnerCeilingOptions: the Runner is where the process-wide operator config
// is held, and a non-positive value must stay a no-op — app.New only calls these
// when the env var is set, but an option that wrote 0 through would turn a typo
// into a run that cannot take a single turn.
func TestRunnerCeilingOptions(t *testing.T) {
	for _, tc := range []struct {
		name          string
		opts          []RunnerOption
		wantTurns     int
		wantToolCalls int
	}{
		{name: "unset leaves the sentinel"},
		{name: "turns", opts: []RunnerOption{WithMaxTurns(60)}, wantTurns: 60},
		{name: "tool calls", opts: []RunnerOption{WithMaxToolCalls(100)}, wantToolCalls: 100},
		{
			name:      "both",
			opts:      []RunnerOption{WithMaxTurns(60), WithMaxToolCalls(100)},
			wantTurns: 60, wantToolCalls: 100,
		},
		{name: "zero is ignored", opts: []RunnerOption{WithMaxTurns(0), WithMaxToolCalls(0)}},
		{name: "negative is ignored", opts: []RunnerOption{WithMaxTurns(-1), WithMaxToolCalls(-1)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := NewRunner(nil, tc.opts...)
			if r.MaxTurns != tc.wantTurns {
				t.Errorf("Runner.MaxTurns = %d, want %d", r.MaxTurns, tc.wantTurns)
			}
			if r.MaxToolCalls != tc.wantToolCalls {
				t.Errorf("Runner.MaxToolCalls = %d, want %d", r.MaxToolCalls, tc.wantToolCalls)
			}
		})
	}
}

// limitsLineOf returns the value of the "limits:" line of a Describe() dump.
func limitsLineOf(dump string) string {
	for _, line := range strings.Split(dump, "\n") {
		if rest, ok := strings.CutPrefix(line, "limits:"); ok {
			return strings.TrimSpace(rest)
		}
	}
	return ""
}
