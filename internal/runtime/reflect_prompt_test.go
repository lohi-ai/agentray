package agentruntime

// What the reflection pass is allowed to READ.
//
// reflect() caps its own output at 1024 tokens, which reads like the pass is
// token-bounded. It was not: the prompt rendered one line per tool call, and the
// tool trace is bounded only by Limits.MaxToolCalls — a number a long autonomous
// run raises into the thousands. So the input grew with how long the agent had
// worked, and the pass broke on precisely the runs worth reflecting on: a
// 4200-turn run produced roughly a megabyte of prompt, past any model's window.
//
// These tests hold the two halves of the replacement. The prompt must stay
// bounded no matter how long the run was, and being bounded must not cost the
// pass the signal it exists to find — which failures repeated, and which tools
// carried the run.

import (
	"fmt"
	"strings"
	"testing"

	"github.com/lohi-ai/agentray/agentcore"
)

// longRunResult builds a run of the shape a real long task produces: thousands
// of successful calls to a workhorse tool, a handful of real failures, and one
// tool that was blocked every time it was tried.
func longRunResult(calls int) agentcore.RunResult {
	res := agentcore.RunResult{Final: "Audited every shard and filed one report per region."}
	for i := range calls {
		t := agentcore.ToolTrace{
			Tool:    "run_sql",
			Args:    fmt.Sprintf(`{"sql":"SELECT %d FROM ledger_shard WHERE region = 'apac' AND status = 'open'"}`, i),
			Allowed: true,
		}
		switch {
		case i%700 == 3:
			t.Error = "query timed out after 30s"
		case i%500 == 11:
			t.Tool, t.Allowed, t.Reason = "delete_rows", false, "scope not granted"
		}
		res.Tools = append(res.Tools, t)
	}
	return res
}

func TestReflectPromptStaysBoundedAsTheRunGrows(t *testing.T) {
	short := len(reflectUserPrompt(longRunResult(20)))
	long := len(reflectUserPrompt(longRunResult(4200)))

	t.Logf("prompt bytes: 20 calls -> %d, 4200 calls -> %d", short, long)

	// The ceiling is generous on purpose — the point is that it is a ceiling at
	// all. One line per call at 4200 calls is ~500 KiB; anything in that
	// neighbourhood means the prompt is still a function of run length.
	const ceiling = 32 * 1024
	if long > ceiling {
		t.Fatalf("a 4200-call run renders a %d B prompt (ceiling %d B): the reflection input still "+
			"scales with the length of the run, so the pass fails on the runs with the most to learn from",
			long, ceiling)
	}

	// A 210x longer run may cost more prompt — the tally gains rows, the failure
	// list fills up — but not proportionally more.
	if long > 8*short {
		t.Fatalf("210x the tool calls cost %.1fx the prompt (%d B -> %d B): the growth is not bounded",
			float64(long)/float64(short), short, long)
	}
}

func TestReflectPromptKeepsTheSignalItBounds(t *testing.T) {
	p := reflectUserPrompt(longRunResult(4200))

	// The pass is asked for "a guardrail for a repeated failure". It cannot
	// propose one without the failure's own words, so the error text and the
	// block reason have to survive the bounding.
	for _, want := range []string{
		"query timed out after 30s",
		"scope not granted",
		"Audited every shard",
	} {
		if !strings.Contains(p, want) {
			t.Fatalf("the bounded prompt dropped signal the pass needs: %q\n---\n%s", want, truncate(p, 2000))
		}
	}

	// The tally is what makes bounding safe: a tool that failed six times is one
	// line that SAYS six, not six lines the window has to hold. Without the
	// counts, a truncated list would look like an isolated incident.
	if !strings.Contains(p, "run_sql: 4191 calls, 6 errored") {
		t.Fatalf("no per-tool tally in the prompt, so a bounded sample cannot show what repeated:\n%s", truncate(p, 2000))
	}
	if !strings.Contains(p, "delete_rows: 9 calls, 9 blocked") {
		t.Fatalf("the tally lost the blocked tool, which is exactly the repeated failure a guardrail "+
			"would be written from:\n%s", truncate(p, 2000))
	}
}

// A short run should read the way it always did: every call, in order, nothing
// elided. The bounding is for the runs that need it and must not tax the rest.
func TestReflectPromptShowsAShortRunWhole(t *testing.T) {
	p := reflectUserPrompt(longRunResult(20))
	if strings.Contains(p, "Most recent") {
		t.Fatalf("a 20-call run was truncated; only long runs should be:\n%s", p)
	}
	if got := strings.Count(p, "run_sql("); got != 20 {
		t.Fatalf("want all 20 calls listed, got %d:\n%s", got, p)
	}
}
