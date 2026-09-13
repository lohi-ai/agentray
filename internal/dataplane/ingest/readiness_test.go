package ingestion

import "testing"

// The readiness predicate decides whether a blue-green deploy may point traffic
// at a colour, so every branch is pinned here: the two ways to be caught up, the
// two ways to be behind, and the branch that must never converge.
func TestEvaluateReplay(t *testing.T) {
	tests := []struct {
		name       string
		applied    uint64
		head       uint64
		first      uint64
		pending    uint64
		ackPending int
		wantReady  bool
		wantReason string
		wantMiss   uint64
	}{
		{
			name:       "fresh stream, nothing ever published",
			applied:    0,
			head:       0,
			first:      0,
			wantReady:  true,
			wantReason: ReplayCaughtUp,
		},
		{
			name:       "applied mark covers the head",
			applied:    40,
			head:       40,
			first:      1,
			wantReady:  true,
			wantReason: ReplayCaughtUp,
		},
		{
			name: "stream shared with another env: head counts foreign sequences, filter has no work left",
			// applied < head, but nothing matches this consumer's filter — the
			// sequence comparison can never come true here, and refusing would
			// block every deploy on the shipped dev/prod topology.
			applied:    7,
			head:       900,
			first:      1,
			wantReady:  true,
			wantReason: ReplayCaughtUp,
		},
		{
			name:       "backlog still to deliver",
			applied:    7,
			head:       40,
			first:      1,
			pending:    33,
			wantReady:  false,
			wantReason: ReplayBehind,
		},
		{
			name:       "delivered but not applied yet",
			applied:    7,
			head:       40,
			first:      1,
			ackPending: 3,
			wantReady:  false,
			wantReason: ReplayBehind,
		},
		{
			name: "colour parked past the retention window: gap above its applied mark",
			// Sequences 8..99 were purged before this colour applied them. Waiting
			// cannot help, so the gate must refuse and say why.
			applied:    7,
			head:       140,
			first:      100,
			pending:    41,
			wantReady:  false,
			wantReason: ReplayPurgedGap,
			wantMiss:   92,
		},
		{
			name: "retention evicted the whole backlog: nothing pending, but the purge frontier is past my mark",
			// The measured shape of the loss: the parked colour's undelivered
			// messages age out, so pending drops to zero while its applied mark
			// stays behind. "No work pending" must not be read as caught up here.
			applied:    3,
			head:       6,
			first:      7,
			wantReady:  false,
			wantReason: ReplayPurgedGap,
			wantMiss:   3,
		},
		{
			name: "fresh durable over a purged prefix is not a gap for that colour",
			// applied == 0: it has no history to lose, so the retained window is
			// its whole history and it just replays it.
			applied:    0,
			head:       140,
			first:      100,
			pending:    41,
			wantReady:  false,
			wantReason: ReplayBehind,
		},
		{
			name:       "purged exactly up to the applied mark is not a gap",
			applied:    99,
			head:       140,
			first:      100,
			pending:    41,
			wantReady:  false,
			wantReason: ReplayBehind,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := EvaluateReplay(tt.applied, tt.head, tt.first, tt.pending, tt.ackPending)
			if got.Ready != tt.wantReady || got.Reason != tt.wantReason {
				t.Fatalf("verdict = %+v, want ready=%v reason=%q", got, tt.wantReady, tt.wantReason)
			}
			if got.Missing != tt.wantMiss {
				t.Fatalf("missing = %d, want %d", got.Missing, tt.wantMiss)
			}
		})
	}
}
