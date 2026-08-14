package agentruntime

import (
	"context"
	"testing"

	"github.com/lohi-ai/agentray/agentcore"
)

func TestEvidenceFinishGuard(t *testing.T) {
	ctx := context.Background()
	guard := evidenceFinishGuard(Scopes{DataQuality: true})
	if guard == nil {
		t.Fatal("data_quality grants read tools; guard must be non-nil")
	}

	t.Run("numeric answer with no tools nudges", func(t *testing.T) {
		got := guard(ctx, agentcore.FinishState{Final: "You had 1,204 signups last week."})
		if got == "" {
			t.Fatal("expected a nudge for an unbacked numeric answer")
		}
	})

	t.Run("evidence tool execution accepts", func(t *testing.T) {
		st := agentcore.FinishState{
			Final: "You had 1,204 signups last week.",
			Tools: []agentcore.ToolTrace{{Tool: ToolRunSQL, Allowed: true}},
		}
		if got := guard(ctx, st); got != "" {
			t.Fatalf("run_sql executed — finish must be accepted, got %q", got)
		}
	})

	t.Run("blocked or failed tool is not evidence", func(t *testing.T) {
		st := agentcore.FinishState{
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
		st := agentcore.FinishState{
			Final: "Filed a recommendation citing a 12% drop.",
			Tools: []agentcore.ToolTrace{{Tool: ToolSubmitRec, Allowed: true}},
		}
		if got := guard(ctx, st); got == "" {
			t.Fatal("submit_recommendation is a write, not evidence")
		}
	})

	t.Run("prose answer passes unguarded", func(t *testing.T) {
		if got := guard(ctx, agentcore.FinishState{Final: "Usage is trending up; no anomalies."}); got != "" {
			t.Fatalf("digit-free prose must pass, got %q", got)
		}
	})

	t.Run("delegated evidence via spawn_subagent accepts", func(t *testing.T) {
		st := agentcore.FinishState{
			Final: "The child agent counted 4,812 events yesterday.",
			Tools: []agentcore.ToolTrace{{Tool: agentcore.ToolSpawnSubagent, Allowed: true}},
		}
		if got := guard(ctx, st); got != "" {
			t.Fatalf("a delegated child ran the read tools — finish must be accepted, got %q", got)
		}
	})

	t.Run("list numbering is not a figure", func(t *testing.T) {
		st := agentcore.FinishState{Final: "1. Open the Dashboards tab\n2. Click New Chart"}
		if got := guard(ctx, st); got != "" {
			t.Fatalf("markdown step numbering must not count as figures, got %q", got)
		}
	})

	t.Run("dates are not figures", func(t *testing.T) {
		st := agentcore.FinishState{Final: "The schema has had no revenue column since the 2026-07-01 migration."}
		if got := guard(ctx, st); got != "" {
			t.Fatalf("calendar dates must not count as figures, got %q", got)
		}
	})

	t.Run("one nudge only", func(t *testing.T) {
		if got := guard(ctx, agentcore.FinishState{Final: "still 1,204", Nudges: 1}); got != "" {
			t.Fatalf("second consultation must accept, got %q", got)
		}
	})

	t.Run("no read scopes disables guard", func(t *testing.T) {
		if g := evidenceFinishGuard(Scopes{}); g != nil {
			t.Fatal("no scopes -> nil guard")
		}
	})
}
