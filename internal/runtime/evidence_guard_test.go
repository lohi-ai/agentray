package agentruntime

import (
	"context"
	"strings"
	"testing"

	"github.com/lohi-ai/agentray/agentcore"
	"github.com/lohi-ai/agentray/agentcore/plugins/finishguard"
	"github.com/lohi-ai/agentray/agentcore/plugins/subagent"
	"github.com/lohi-ai/agentray/sandbox"
)

func TestEvidenceFinishGuard(t *testing.T) {
	ctx := context.Background()
	guard := evidenceFinishGuard(Scopes{DataQuality: true}, nil)
	if guard == nil {
		t.Fatal("data_quality grants read tools; guard must be non-nil")
	}

	t.Run("numeric answer with no tools nudges", func(t *testing.T) {
		got := guard(ctx, finishguard.State{Final: "You had 1,204 signups last week."})
		if got == "" {
			t.Fatal("expected a nudge for an unbacked numeric answer")
		}
	})

	t.Run("evidence tool execution accepts", func(t *testing.T) {
		st := finishguard.State{
			Final: "You had 1,204 signups last week.",
			Tools: []agentcore.ToolTrace{{Tool: ToolRunSQL, Allowed: true}},
		}
		if got := guard(ctx, st); got != "" {
			t.Fatalf("run_sql executed — finish must be accepted, got %q", got)
		}
	})

	t.Run("blocked or failed tool is not evidence", func(t *testing.T) {
		st := finishguard.State{
			Final: "42 events",
			Tools: []agentcore.ToolTrace{
				{Tool: ToolRunSQL, Allowed: false, Reason: "denied"},
				{Tool: ToolExploreEvents, Allowed: true, Error: "timeout"},
			},
		}
		if got := guard(ctx, st); got == "" {
			t.Fatal("blocked/failed calls must not count as evidence")
		}
	})

	t.Run("write tool is not evidence", func(t *testing.T) {
		st := finishguard.State{
			Final: "Filed a recommendation citing a 12% drop.",
			Tools: []agentcore.ToolTrace{{Tool: ToolSubmitRec, Allowed: true}},
		}
		if got := guard(ctx, st); got == "" {
			t.Fatal("submit_recommendation is a write, not evidence")
		}
	})

	t.Run("prose answer passes unguarded", func(t *testing.T) {
		if got := guard(ctx, finishguard.State{Final: "Usage is trending up; no anomalies."}); got != "" {
			t.Fatalf("digit-free prose must pass, got %q", got)
		}
	})

	t.Run("delegated evidence via spawn_subagent accepts", func(t *testing.T) {
		st := finishguard.State{
			Final: "The child agent counted 4,812 events yesterday.",
			Tools: []agentcore.ToolTrace{{Tool: subagent.ToolSpawnSubagent, Allowed: true}},
		}
		if got := guard(ctx, st); got != "" {
			t.Fatalf("a delegated child ran the read tools — finish must be accepted, got %q", got)
		}
	})

	t.Run("list numbering is not a figure", func(t *testing.T) {
		st := finishguard.State{Final: "1. Open the Dashboards tab\n2. Click New Chart"}
		if got := guard(ctx, st); got != "" {
			t.Fatalf("markdown step numbering must not count as figures, got %q", got)
		}
	})

	t.Run("dates are not figures", func(t *testing.T) {
		st := finishguard.State{Final: "The schema has had no revenue column since the 2026-07-01 migration."}
		if got := guard(ctx, st); got != "" {
			t.Fatalf("calendar dates must not count as figures, got %q", got)
		}
	})

	t.Run("one nudge only", func(t *testing.T) {
		if got := guard(ctx, finishguard.State{Final: "still 1,204", Nudges: 1}); got != "" {
			t.Fatalf("second consultation must accept, got %q", got)
		}
	})

	t.Run("no read scopes disables guard", func(t *testing.T) {
		if g := evidenceFinishGuard(Scopes{}, nil); g != nil {
			t.Fatal("no scopes and no tools -> nil guard")
		}
	})
}

// A pre-product agent's evidence is the open web. web_fetch is granted through
// Pack.Tools, which no scope names, so the guard has to look past the scope map
// on both halves: it must ARM for such an agent, and it must CREDIT the fetch.
// Getting the second half wrong is worse than not guarding at all — it nudges an
// agent that did exactly what it was built to do.
func TestEvidenceFinishGuardCountsTheOpenWeb(t *testing.T) {
	ctx := context.Background()

	t.Run("web_fetch alone arms the guard", func(t *testing.T) {
		guard := evidenceFinishGuard(Scopes{}, []string{sandbox.ToolWebFetch})
		if guard == nil {
			t.Fatal("an agent granted web_fetch holds an evidence tool; guard must be non-nil")
		}
		if got := guard(ctx, finishguard.State{Final: "The market is growing 30% a year."}); got == "" {
			t.Fatal("an invented market figure with no fetch must still be nudged")
		}
	})

	t.Run("a fetched page is evidence", func(t *testing.T) {
		guard := evidenceFinishGuard(Scopes{DataQuality: true}, []string{sandbox.ToolWebFetch})
		st := finishguard.State{
			Final: "Their Premium plan is 99,000 VND/month (fetched from cookpad.com/vn/premium).",
			Tools: []agentcore.ToolTrace{{Tool: sandbox.ToolWebFetch, Allowed: true}},
		}
		if got := guard(ctx, st); got != "" {
			t.Fatalf("a price read off a page fetched this turn is checked evidence, got %q", got)
		}
	})
}

// The guard speaks to the model through a USER message, so the model's reply IS
// the answer the owner reads. The nudge therefore has to end in the answer even
// when the model concludes nothing needs fixing — otherwise the owner's question
// is silently answered with a self-audit.
func TestEvidenceNudgeDemandsTheWholeAnswerBack(t *testing.T) {
	lower := strings.ToLower(evidenceNudge)
	for _, want := range []string{
		"your next message is what the user reads",
		"complete answer",
		"send it again unchanged",
	} {
		if !strings.Contains(lower, want) {
			t.Errorf("nudge must tell the model its reply is the user-visible answer; missing %q", want)
		}
	}
	if !strings.Contains(lower, "not visible to the user") {
		t.Error("nudge must say it is not itself visible, or the model answers it as a question")
	}
}
