package bench_test

// Live benchmark: an agentcore agent solves the Base −2 Calculator problem
// (PROBLEM.md) end to end against the same hidden judge the Claude Code
// harness was scored on (results/claude-code.json). The agent gets exactly two
// tools — run_program to debug and submit_solution to be judged — and iterates
// until every hidden test passes or its budget runs out. The run's attempts,
// turns, token usage, and wall time land in results/agentcore.json, and the
// first passing source in results/agentcore-solution.go, for comparison.
//
// Gated on the operator's OpenAI-compatible endpoint (same convention as
// agentcore's real-provider tests):
//
//	AGENTRAY_TEST_OPENAI_BASE_URL
//	AGENTRAY_TEST_OPENAI_API_KEY
//	AGENTRAY_TEST_OPENAI_MODEL
//
// Run: go test -timeout 30m -run TestBench_AgentcoreSolvesProblem ./bench/

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lohi-ai/agentray/agentcore"
	"github.com/lohi-ai/agentray/ai"
	"github.com/lohi-ai/agentray/bench/judge"
)

// attemptRecord is one submit_solution call's outcome, kept for the report.
type attemptRecord struct {
	Attempt  int    `json:"attempt"`
	Compiled bool   `json:"compiled"`
	Passed   int    `json:"passed"`
	Total    int    `json:"total"`
	Error    string `json:"error,omitempty"`
}

// submitTool judges a full candidate source against the hidden case set and
// returns the same Verdict.Format feedback string the human solver received.
type submitTool struct {
	mu          sync.Mutex
	attempts    []attemptRecord
	passingSrc  string
	maxAttempts int
}

func (s *submitTool) Name() string { return "submit_solution" }
func (s *submitTool) Schema() agentcore.ToolSchema {
	return agentcore.ToolSchema{
		Name: "submit_solution",
		Description: "Judge a complete single-file Go program against ALL hidden tests. " +
			"Returns PASS, or the first failing case (input, expected, got). " +
			"source must be the ENTIRE program (package main, stdlib only).",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"source": map[string]any{"type": "string", "description": "complete Go source of the solution"},
			},
			"required": []any{"source"},
		},
	}
}

func (s *submitTool) Run(ctx context.Context, args string) (string, error) {
	var in struct {
		Source string `json:"source"`
	}
	if err := json.Unmarshal([]byte(args), &in); err != nil {
		return "", err
	}
	s.mu.Lock()
	n := len(s.attempts) + 1
	s.mu.Unlock()
	if n > s.maxAttempts {
		return "attempt budget exhausted: no more submissions accepted", nil
	}
	v, err := judge.Judge(ctx, in.Source)
	if err != nil {
		s.mu.Lock()
		s.attempts = append(s.attempts, attemptRecord{Attempt: n, Error: err.Error()})
		s.mu.Unlock()
		return "", err
	}
	rec := attemptRecord{Attempt: n, Compiled: v.Compiled, Passed: v.Passed, Total: v.Total}
	s.mu.Lock()
	s.attempts = append(s.attempts, rec)
	if v.AllPassed() && s.passingSrc == "" {
		s.passingSrc = in.Source
	}
	s.mu.Unlock()
	return v.Format(), nil
}

// probeTool compiles and runs a candidate on ONE custom input — the debugging
// channel, mirroring the human solver's ability to run the program locally.
type probeTool struct {
	mu    sync.Mutex
	calls int
}

func (p *probeTool) Name() string { return "run_program" }
func (p *probeTool) Schema() agentcore.ToolSchema {
	return agentcore.ToolSchema{
		Name: "run_program",
		Description: "Compile a complete single-file Go program and run it once with the " +
			"given stdin input. Returns stdout/stderr — use it to debug before submitting.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"source": map[string]any{"type": "string", "description": "complete Go source"},
				"input":  map[string]any{"type": "string", "description": "stdin to feed the program (one line)"},
			},
			"required": []any{"source", "input"},
		},
	}
}

func (p *probeTool) Run(ctx context.Context, args string) (string, error) {
	var in struct {
		Source string `json:"source"`
		Input  string `json:"input"`
	}
	if err := json.Unmarshal([]byte(args), &in); err != nil {
		return "", err
	}
	p.mu.Lock()
	p.calls++
	p.mu.Unlock()
	return judge.RunProgram(ctx, in.Source, in.Input), nil
}

func TestBench_AgentcoreSolvesProblem(t *testing.T) {
	base := strings.TrimSpace(os.Getenv("AGENTRAY_TEST_OPENAI_BASE_URL"))
	key := strings.TrimSpace(os.Getenv("AGENTRAY_TEST_OPENAI_API_KEY"))
	model := strings.TrimSpace(os.Getenv("AGENTRAY_TEST_OPENAI_MODEL"))
	if base == "" || key == "" || model == "" {
		t.Skip("set AGENTRAY_TEST_OPENAI_BASE_URL, AGENTRAY_TEST_OPENAI_API_KEY, AGENTRAY_TEST_OPENAI_MODEL to run the live benchmark")
	}
	provider := ai.NewOpenAIProvider(key, base, ai.DefaultCompat())

	problem, err := os.ReadFile("PROBLEM.md")
	if err != nil {
		t.Fatalf("read PROBLEM.md: %v", err)
	}

	submit := &submitTool{maxAttempts: 10}
	probe := &probeTool{}

	limits := agentcore.DefaultLimits()
	limits.MaxTurns = 30
	limits.MaxToolCalls = 40

	agent, err := agentcore.New(agentcore.Config{
		Provider:  provider,
		Model:     model,
		Limits:    &limits,
		MaxTokens: 16000, // full Go sources are large; avoid stop_reason "length"
		// Match production (runner.go): a stable cache key so each turn's growing
		// stable prefix is a cheap cache read instead of full-price input.
		PromptCacheKey:       "bench-base2",
		PromptCacheRetention: "short",
		Tools:                agentcore.NewToolSet(submit, probe),
		Policy:               agentcore.NewAllowList("submit_solution", "run_program"),
		Definition: agentcore.AgentDefinition{
			Agents: "You are an expert competitive programmer writing Go. Solve the given problem " +
				"by iterating with your tools: optionally debug with run_program, then judge with " +
				"submit_solution. Every tool call must carry the COMPLETE single-file Go program " +
				"(package main, standard library only). If the judge reports a failure, analyze the " +
				"failing case and submit a fixed full program — do not give up while submissions " +
				"remain (max 10). When the judge replies PASS, respond with exactly: SOLVED. " +
				"Keep prose minimal; spend your effort on the code.",
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Minute)
	defer cancel()
	start := time.Now()
	res, err := agent.Prompt(ctx, string(problem)+
		"\n\nSolve this problem now. Iterate until submit_solution reports PASS.")
	wall := time.Since(start)
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}

	submit.mu.Lock()
	attempts := append([]attemptRecord(nil), submit.attempts...)
	passing := submit.passingSrc
	submit.mu.Unlock()
	probe.mu.Lock()
	probeCalls := probe.calls
	probe.mu.Unlock()

	solved := passing != ""
	t.Logf("solved=%v attempts=%d probes=%d turns=%d stop=%q wall=%s usage=%+v",
		solved, len(attempts), probeCalls, res.Turns, res.StopReason, wall.Round(time.Second), res.Usage)
	for _, a := range attempts {
		t.Logf("  attempt %d: compiled=%v passed=%d/%d %s", a.Attempt, a.Compiled, a.Passed, a.Total, a.Error)
	}

	// Persist the comparison record regardless of outcome.
	report := map[string]any{
		"solver":                  "agentcore",
		"model":                   model,
		"date":                    time.Now().Format("2006-01-02"),
		"solved":                  solved,
		"attempts":                attempts,
		"submission_count":        len(attempts),
		"run_program_debug_calls": probeCalls,
		"turns":                   res.Turns,
		"stop_reason":             res.StopReason,
		"wall_seconds":            wall.Seconds(),
		"usage":                   res.Usage,
		"final":                   res.Final,
	}
	if buf, jerr := json.MarshalIndent(report, "", "  "); jerr == nil {
		if werr := os.WriteFile("results/agentcore.json", buf, 0o644); werr != nil {
			t.Logf("could not save report: %v", werr)
		}
	}
	if passing != "" {
		if werr := os.WriteFile("results/agentcore-solution.go", []byte(passing), 0o644); werr != nil {
			t.Logf("could not save passing solution: %v", werr)
		}
	}

	if !solved {
		t.Fatalf("agentcore agent did not solve the problem within budget (attempts=%d, stop=%q)", len(attempts), res.StopReason)
	}
}
