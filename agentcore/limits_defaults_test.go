package agentcore

import "testing"

// TestDefaultLimitsCannotStarveTheirOwnTurns guards the pairing between the two
// ceilings, not their absolute values. A turn that calls a tool spends one of
// each, so MaxToolCalls below MaxTurns makes the tool ceiling the real turn
// ceiling — the run dies of a limit nobody was tuning, and the reason reported
// is the wrong one.
func TestDefaultLimitsCannotStarveTheirOwnTurns(t *testing.T) {
	d := DefaultLimits()
	if d.MaxToolCalls < d.MaxTurns {
		t.Fatalf("MaxToolCalls (%d) < MaxTurns (%d): every turn may call a tool, so the tool ceiling would bind first", d.MaxToolCalls, d.MaxTurns)
	}
}

// TestDefaultLimitsClearTheMeasuredFloor pins the finding that motivated the
// raise: a data agent answering one real question spent more than 12 turns on
// schema discovery, exploratory queries and a correction before writing the
// answer, and failed 2 of 3 first runs. Anything back at or below that floor is
// a regression to a measured-broken configuration, whatever the reasoning.
func TestDefaultLimitsClearTheMeasuredFloor(t *testing.T) {
	const failedAtTurns = 12
	for _, tc := range []struct {
		name string
		got  int
		min  int
	}{
		{"turns", DefaultLimits().MaxTurns, failedAtTurns * 2},
		{"tool calls", DefaultLimits().MaxToolCalls, failedAtTurns * 2},
	} {
		if tc.got < tc.min {
			t.Errorf("default max %s = %d, want >= %d (12 turns was measured insufficient for a data agent)", tc.name, tc.got, tc.min)
		}
	}
}

// TestDefaultLimitsLeaveTheContextKnobsAlone: the turn raise was not licence to
// move the byte/token budgets with it — those are sized by the model's window
// and the transcript, not by how many steps an answer takes.
func TestDefaultLimitsLeaveTheContextKnobsAlone(t *testing.T) {
	d := DefaultLimits()
	if d.MaxToolResultLen != defaultMaxToolResultBytes {
		t.Errorf("MaxToolResultLen = %d, want the package default %d", d.MaxToolResultLen, defaultMaxToolResultBytes)
	}
	if d.MaxContextTokens != defaultContextTokenBudget {
		t.Errorf("MaxContextTokens = %d, want the package default %d", d.MaxContextTokens, defaultContextTokenBudget)
	}
}
