package agentcore

import "testing"

// TestGoalFromLogClearsAtLeaf pins the rule that keeps a chat continuation from
// inheriting a finished run's gate: the goal belongs to the run that recorded
// it, and EntryLeaf ends that run.
//
// Without this, a second prompt on the same session would resume gated by a
// goal the user already satisfied, and the model would be told to keep working
// on it forever.
func TestGoalFromLogClearsAtLeaf(t *testing.T) {
	cases := []struct {
		name    string
		entries []SessionEntry
		want    string
	}{
		{"no entries", nil, ""},
		{"goal only", []SessionEntry{{Kind: EntryGoal, Goal: "ship it"}}, "ship it"},
		{
			"last goal wins",
			[]SessionEntry{{Kind: EntryGoal, Goal: "first"}, {Kind: EntryGoal, Goal: "second"}},
			"second",
		},
		{
			"leaf clears the finished run's goal",
			[]SessionEntry{{Kind: EntryGoal, Goal: "ship it"}, {Kind: EntryLeaf}},
			"",
		},
		{
			"a run chained after a leaf carries its own goal",
			[]SessionEntry{
				{Kind: EntryGoal, Goal: "old"},
				{Kind: EntryLeaf},
				{Kind: EntryGoal, Goal: "new"},
			},
			"new",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := goalFromLog(tc.entries); got != tc.want {
				t.Fatalf("goalFromLog = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestGoalFromLogMatchesRecoverSession keeps the two folds from drifting: the
// loop resolves the goal from goalFromLog before extensions begin, while
// RecoverSession derives it again for the recovery plan. If they ever disagree,
// a resumed run would be gated by one condition and reason about another.
func TestGoalFromLogMatchesRecoverSession(t *testing.T) {
	entries := []SessionEntry{
		{Kind: EntryGoal, Goal: "old"},
		{Kind: EntryLeaf},
		{Kind: EntryGoal, Goal: "current"},
		{Kind: EntryMessage, Message: &Message{Role: RoleUser, Content: "go"}},
	}
	plan := RecoverSession(entries, NewToolSet(), RecoveryMarkInterrupted)
	if got := goalFromLog(entries); got != plan.Goal {
		t.Fatalf("goalFromLog = %q but RecoverSession = %q", got, plan.Goal)
	}
}
