package storage

import (
	"testing"
	"time"
)

func day(y int, m time.Month, d int) time.Time {
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

// A plain day-aligned aggregate over a valid window is rollup-eligible and yields
// the half-open [from, to) day bounds.
func TestAgentRollupWindowEligible(t *testing.T) {
	from, to := day(2026, 7, 1), day(2026, 7, 8)
	gotFrom, gotTo, ok := agentRollupWindow(EventFilter{From: from, To: to})
	if !ok {
		t.Fatal("want eligible")
	}
	if !gotFrom.Equal(from) || !gotTo.Equal(to) {
		t.Fatalf("bounds mismatch: got [%v,%v)", gotFrom, gotTo)
	}
}

// Any per-row predicate forces the raw path — the day-grained rollup can't answer it.
func TestAgentRollupWindowRejectsRowFilters(t *testing.T) {
	base := EventFilter{From: day(2026, 7, 1), To: day(2026, 7, 8)}
	cases := map[string]EventFilter{
		"distinct_id": {From: base.From, To: base.To, DistinctID: "u1"},
		"session_id":  {From: base.From, To: base.To, SessionID: "s1"},
		"search":      {From: base.From, To: base.To, Search: "foo"},
		"event_name":  {From: base.From, To: base.To, EventName: "agent.turn"},
		"agent_id":    {From: base.From, To: base.To, AgentID: "a1"},
		"model_name":  {From: base.From, To: base.To, ModelName: "claude"},
		"error_only":  {From: base.From, To: base.To, ErrorOnly: true},
	}
	for name, f := range cases {
		if _, _, ok := agentRollupWindow(f); ok {
			t.Errorf("%s: want raw fallback, got rollup-eligible", name)
		}
	}
}

// Non-midnight boundaries and empty/inverted windows fall back to raw.
func TestAgentRollupWindowRejectsUnalignedWindows(t *testing.T) {
	cases := map[string]EventFilter{
		"from not midnight": {From: day(2026, 7, 1).Add(3 * time.Hour), To: day(2026, 7, 8)},
		"to not midnight":   {From: day(2026, 7, 1), To: day(2026, 7, 8).Add(90 * time.Minute)},
		"zero from":         {To: day(2026, 7, 8)},
		"zero to":           {From: day(2026, 7, 1)},
		"inverted":          {From: day(2026, 7, 8), To: day(2026, 7, 1)},
		"empty":             {From: day(2026, 7, 1), To: day(2026, 7, 1)},
	}
	for name, f := range cases {
		if _, _, ok := agentRollupWindow(f); ok {
			t.Errorf("%s: want raw fallback, got rollup-eligible", name)
		}
	}
}
