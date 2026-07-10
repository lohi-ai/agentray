package agentruntime

import (
	"testing"

	"github.com/lohi-ai/agentray/agentcore"
)

func TestProgressNote(t *testing.T) {
	if got := progressNote("run_sql"); got != "Crunching the numbers…" {
		t.Fatalf("run_sql note = %q", got)
	}
	if got := progressNote("some_future_tool"); got != "Working on it…" {
		t.Fatalf("unknown tool should use the generic phrase, got %q", got)
	}
}

func TestCardFromMessages_InsightSeries(t *testing.T) {
	content := `{"type":"timeseries","title":"Signups","metric":"users","series":[{"hour":"2026-06-01T10:00:00Z","count":5},{"hour":"2026-06-01T11:00:00Z","count":8}]}`
	card := cardFromMessages([]agentcore.Message{
		{Role: agentcore.RoleUser, Content: "how many signups?"},
		{Role: agentcore.RoleTool, Name: "run_insight", Content: content},
	})
	if card == nil || card.Kind != "series" {
		t.Fatalf("expected a series card, got %+v", card)
	}
	if card.Title != "Signups" || len(card.Points) != 2 || card.Points[1].Value != 8 {
		t.Fatalf("unexpected card: %+v", card)
	}
}

func TestCardFromMessages_Funnel(t *testing.T) {
	content := `{"type":"funnel","title":"Checkout","funnel":[{"step":1,"event_name":"view","users":100,"conversion":1},{"step":2,"event_name":"buy","users":20,"conversion":0.2}]}`
	card := cardFromMessages([]agentcore.Message{{Role: agentcore.RoleTool, Name: "run_insight", Content: content}})
	if card == nil || card.Kind != "stat" || len(card.Stats) != 2 {
		t.Fatalf("expected a 2-row stat card, got %+v", card)
	}
	if card.Stats[1].Label != "buy" || card.Stats[1].Value != "20 (20%)" {
		t.Fatalf("unexpected funnel stat: %+v", card.Stats[1])
	}
}

func TestCardFromMessages_SQLScalar(t *testing.T) {
	card := cardFromMessages([]agentcore.Message{{Role: agentcore.RoleTool, Name: "run_sql", Content: `{"rows":[{"total":42}]}`}})
	if card == nil || card.Kind != "stat" || len(card.Stats) != 1 || card.Stats[0].Value != "42" {
		t.Fatalf("expected a single stat of 42, got %+v", card)
	}
}

func TestCardFromMessages_SQLTable(t *testing.T) {
	content := `{"rows":[{"channel":"paid","signups":30},{"channel":"organic","signups":12}]}`
	card := cardFromMessages([]agentcore.Message{{Role: agentcore.RoleTool, Name: "run_sql", Content: content}})
	if card == nil || card.Kind != "series" || len(card.Points) != 2 {
		t.Fatalf("expected a 2-point series, got %+v", card)
	}
	if card.Points[0].Label != "paid" || card.Points[0].Value != 30 {
		t.Fatalf("unexpected series point: %+v", card.Points[0])
	}
}

func TestCardFromMessages_None(t *testing.T) {
	// A wide/ambiguous SQL result yields no card (prose fallback).
	content := `{"rows":[{"a":1,"b":2,"c":3}]}`
	if card := cardFromMessages([]agentcore.Message{{Role: agentcore.RoleTool, Name: "run_sql", Content: content}}); card != nil {
		t.Fatalf("expected no card for a wide row, got %+v", card)
	}
}
