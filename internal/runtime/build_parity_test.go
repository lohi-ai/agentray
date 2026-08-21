package agentruntime

import (
	"context"
	"strings"
	"testing"

	"github.com/lohi-ai/agentray/agentcore"
	"github.com/lohi-ai/agentray/agentcore/plugins/finishguard"
	"github.com/lohi-ai/agentray/agentcore/plugins/observe"
	"github.com/lohi-ai/agentray/agentcore/plugins/spill"
	"github.com/lohi-ai/agentray/agentcore/plugins/subagent"
	"github.com/lohi-ai/agentray/agentcore/plugins/todo"
	"github.com/lohi-ai/agentray/internal/dataplane/store"
)

// buildParityGolden is the composition Build produced BEFORE it was migrated off
// agentcore.New onto plugin composition, captured from the running code for the
// representative BuildParams below.
//
// It is the whole safety net for that migration. Twenty-odd BuildParams fields
// each route to a seam, and a mapping silently dropped in the rewrite — the
// escalation ladder, the cache key, a limits override — would produce an agent
// that still runs and is quietly less capable. Byte-comparing the seam dump is
// what turns that class of bug into a failing test.
//
// The extensions line is deliberately NOT part of the golden: the migration's
// stated purpose was to add capabilities there. It is asserted separately below.
const buildParityGolden = `driver:                react
model:                 gpt-5
max_tokens:            4096
reasoning_effort:      high
output_schema:         -
prompt_cache:          agent-1
refresh_key:           set
retry:                 3 attempts
scope:                 agent-1
skills:                0
limits:                turns=24 tools=40 ctx=50000 result=24576
sandbox:               set
policy:                *agentcore.AllowList
goal:                  STATUS: DONE appears
budget_gate:           set
step_gate:             set
session:               set
session_resume:        false
seed_disabled:         broken
memory:                -
context_window:        0
compaction:            keep_recent=5000 budget=50000
compactor:             summary
compaction_model:      -
steering:              set
follow_up:             set
prepare_next_turn:     set
tools:                 activity_summary, recent_events, persons, explore_events, run_sql, run_insight, run_funnel, run_retention, list_dashboards, create_dashboard, create_chart, submit_recommendation, propose_test, test_status, list_tests, remember, send_notification, http_request, run_shell, update_plan
hooks:                 before=2 after=1 context=1 turn_start=0 turn_end=0 message_end=0 provider=0 agent_end=0
hook_error_policy:
`

// TestBuildMatchesPreMigrationComposition is the golden: every seam the old
// Config hand-off claimed, the plugin composition must claim identically —
// including the tool order the model is shown and the hook counts (the policy
// gate + the injection guard before, the terminate hook after, the plan pin on
// context).
func TestBuildMatchesPreMigrationComposition(t *testing.T) {
	agent, err := Build(representativeBuildParams())
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	got := withoutExtensionsLine(agent.Describe())
	if got != buildParityGolden {
		t.Fatalf("the composition drifted from the pre-migration agent.\n--- want ---\n%s\n--- got ---\n%s", buildParityGolden, got)
	}
}

// TestBuildInstallsTheShippedCapabilities is the other half: the migration was
// worth doing only if the plugins that were shipped-but-unwired are now actually
// part of the product's agent.
func TestBuildInstallsTheShippedCapabilities(t *testing.T) {
	agent, err := Build(representativeBuildParams())
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	exts := extensionsLineOf(agent.Describe())
	for _, want := range []string{
		// newly wired by preset.Full
		"spill", "jobs", "repeat_guard", "session_query", "log_invariant",
		// pre-existing, must survive
		"goal", "finish_guard", "subagent",
	} {
		if !strings.Contains(exts, want) {
			t.Fatalf("%q is not installed: %s", want, exts)
		}
	}

	// Order is the contract between the two stop interceptors: the goal gate
	// must be consulted before the finish guard, because an unmet goal makes any
	// verification pass on that same answer moot.
	if strings.Index(exts, "goal") > strings.Index(exts, "finish_guard") {
		t.Fatalf("the goal gate no longer precedes the finish guard: %s", exts)
	}
}

// TestBuildWithoutOptionalCapabilities: the analytics-only agent — no sandbox,
// no vault, no plan, no delegation, no spill — must still compose, and must
// carry no trace of what it did not ask for.
func TestBuildWithoutOptionalCapabilities(t *testing.T) {
	p := representativeBuildParams()
	p.Sandbox = nil
	p.Credentials = nil
	p.Todo = nil
	p.Subagents = nil
	p.HTTPTool = nil
	p.Tools = nil
	p.Spill = nil
	p.FinishGuard = nil
	p.ReportLogInvariant = nil

	agent, err := Build(p)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	dump := agent.Describe()
	for _, gone := range []string{"run_shell", "http_request", todo.ToolName, subagent.ToolSpawnSubagent} {
		if strings.Contains(dump, gone) {
			t.Fatalf("%q survived a composition that never asked for it:\n%s", gone, dump)
		}
	}
	if strings.Contains(extensionsLineOf(dump), "spill") {
		t.Fatalf("spill installed with no durable store:\n%s", dump)
	}
	// The injection guard goes with the sandbox: no backend, no guard, so only
	// the permission gate remains on the before-hook.
	if !strings.Contains(dump, "before=1") {
		t.Fatalf("dropping the sandbox did not drop its guard:\n%s", dump)
	}
}

// TestBuildRejectsAnIncompleteRun keeps the two preconditions honest — they are
// the only things Build refuses, and a composition that silently accepted either
// would fail much later, inside a run.
func TestBuildRejectsAnIncompleteRun(t *testing.T) {
	p := representativeBuildParams()
	p.APIKey = ""
	if _, err := Build(p); err == nil {
		t.Fatal("Build accepted a run with no API key")
	}
	p = representativeBuildParams()
	p.Data = nil
	if _, err := Build(p); err == nil {
		t.Fatal("Build accepted a run with no data source")
	}
}

// representativeBuildParams exercises every branch of the BuildParams → plugin
// mapping, so parity is proven over the whole surface rather than over the
// fields that happened to be convenient.
//
// Data is a typed-nil *storage.Store: the tools are built from the shared opcore
// registry at composition time and never touch the database until a call runs,
// so a nil store satisfies the interface without standing up Postgres.
func representativeBuildParams() BuildParams {
	return BuildParams{
		ProjectID:            "proj-1",
		ScopeID:              "agent-1",
		Provider:             "openai",
		Model:                "gpt-5",
		APIKey:               "sk-test",
		Scopes:               Scopes{},
		Soul:                 "soul",
		Agents:               "agents",
		Data:                 (*storage.Store)(nil),
		RunID:                "run-1",
		Trigger:              "chat",
		Sandbox:              paritySandbox{},
		Credentials:          parityCredentials{},
		Session:              agentcore.NewMemorySessionStore(),
		SessionID:            "run-1",
		SeedDisabledTools:    []string{"broken"},
		MaxTokens:            4096,
		PromptCacheKey:       "agent-1",
		PromptCacheRetention: "short",
		Todo:                 todo.NewStore(),
		Subagents:            &subagent.Plugin{},
		Goal:                 "STATUS: DONE appears",
		FinishGuard:          func(context.Context, finishguard.State) string { return "" },
		MaxContextTokens:     50000,
		KeepRecentTokens:     5000,
		HTTPTool:             fakeTool{name: "http_request"},
		Tools:                []agentcore.Tool{fakeTool{name: "run_shell"}},
		ReasoningEffort:      "high",
		StepGate:             func(context.Context, int) error { return nil },
		BudgetGate:           func(context.Context, agentcore.Usage) bool { return false },
		GetSteering:          func(context.Context) []agentcore.Message { return nil },
		GetFollowUp:          func(context.Context) []agentcore.Message { return nil },
		RefreshKey:           func(context.Context, string) (string, error) { return "k", nil },
		PrepareNextTurn: func(_ context.Context, s agentcore.TurnState) agentcore.TurnState {
			return s
		},
		Spill:              spill.NewMemorySpillStore(),
		ReportLogInvariant: func(observe.LogInvariantViolation) {},
	}
}

type paritySandbox struct{}

func (paritySandbox) Exec(context.Context, agentcore.SandboxExec) (agentcore.SandboxResult, error) {
	return agentcore.SandboxResult{}, nil
}

type parityCredentials struct{}

func (parityCredentials) Resolve(_ context.Context, args string) (string, error) { return args, nil }

// extensionsLineOf returns the "extensions:" line of a Describe() dump.
func extensionsLineOf(dump string) string {
	for _, line := range strings.Split(dump, "\n") {
		if strings.HasPrefix(line, "extensions:") {
			return line
		}
	}
	return ""
}

// withoutExtensionsLine drops the extensions line so the rest can be compared
// against the golden, and trims each line's trailing padding — Describe pads to
// a fixed column, and a golden carrying invisible trailing spaces is a golden
// nobody can edit correctly.
func withoutExtensionsLine(dump string) string {
	var keep []string
	for _, line := range strings.Split(dump, "\n") {
		if !strings.HasPrefix(line, "extensions:") {
			keep = append(keep, strings.TrimRight(line, " "))
		}
	}
	return strings.Join(keep, "\n")
}
