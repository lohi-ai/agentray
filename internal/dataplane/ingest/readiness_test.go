package ingestion

import (
	"testing"
)

// The readiness predicate decides whether a blue-green deploy may point traffic
// at a colour, so every branch is pinned here: the two ways to be caught up, the
// two ways to be behind, the branch that must never converge, and the two ways
// the broker's own configuration disqualifies the rest of the arithmetic.
func TestEvaluateReplay(t *testing.T) {
	tests := []struct {
		name       string
		applied    uint64
		head       uint64
		first      uint64
		pending    uint64
		ackPending int
		wired      bool
		dedicated  bool
		wantReady  bool
		wantReason string
		wantMiss   uint64
	}{
		{
			name:       "fresh stream, nothing ever published",
			applied:    0,
			head:       0,
			first:      0,
			wired:      true,
			dedicated:  true,
			wantReady:  true,
			wantReason: ReplayCaughtUp,
		},
		{
			name:       "applied mark covers the head",
			applied:    40,
			head:       40,
			first:      1,
			wired:      true,
			dedicated:  true,
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
			wired:      true,
			dedicated:  false,
			wantReady:  true,
			wantReason: ReplayCaughtUp,
		},
		{
			name: "another env's sequences age out on the shared stream: not this colour's loss",
			// Same shape as the purged-gap case below, but the stream carries
			// subjects this consumer is not offered, so the purge frontier says
			// nothing about this colour's rows. Latching a refusal here would
			// wedge every deploy for the life of the process.
			applied:    7,
			head:       140,
			first:      100,
			wired:      true,
			dedicated:  false,
			wantReady:  true,
			wantReason: ReplayCaughtUp,
		},
		{
			name:       "backlog still to deliver",
			applied:    7,
			head:       40,
			first:      1,
			pending:    33,
			wired:      true,
			dedicated:  true,
			wantReady:  false,
			wantReason: ReplayBehind,
		},
		{
			name:       "delivered but not applied yet",
			applied:    7,
			head:       40,
			first:      1,
			ackPending: 3,
			wired:      true,
			dedicated:  true,
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
			wired:      true,
			dedicated:  true,
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
			wired:      true,
			dedicated:  true,
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
			wired:      true,
			dedicated:  true,
			wantReady:  false,
			wantReason: ReplayBehind,
		},
		{
			name:       "purged exactly up to the applied mark is not a gap",
			applied:    99,
			head:       140,
			first:      100,
			pending:    41,
			wired:      true,
			dedicated:  true,
			wantReady:  false,
			wantReason: ReplayBehind,
		},
		{
			name: "stream does not carry this colour's subjects: nothing it reports can make it ready",
			// The consumer is offered nothing, so pending/ackPending are vacuously
			// zero. Reporting caught-up here is a green healthcheck in front of a
			// pipeline that will never deliver another row.
			applied:    7,
			head:       900,
			first:      1,
			wired:      false,
			dedicated:  false,
			wantReady:  false,
			wantReason: ReplayStreamMismatch,
		},
		{
			name:       "unwired refuses even when the numbers look caught up",
			applied:    40,
			head:       40,
			first:      1,
			wired:      false,
			dedicated:  false,
			wantReady:  false,
			wantReason: ReplayStreamMismatch,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := EvaluateReplay(tt.applied, tt.head, tt.first, tt.pending, tt.ackPending, tt.wired, tt.dedicated)
			if got.Ready != tt.wantReady || got.Reason != tt.wantReason {
				t.Fatalf("verdict = %+v, want ready=%v reason=%q", got, tt.wantReady, tt.wantReason)
			}
			if got.Missing != tt.wantMiss {
				t.Fatalf("missing = %d, want %d", got.Missing, tt.wantMiss)
			}
		})
	}
}

// streamCarries is what stops the predicate reading a stream-wide number as if
// it were the colour's own: a consumer is filter-scoped and the stream is not.
func TestStreamCarries(t *testing.T) {
	dev := []string{"agentray.events.ingest.dev", "agentray.events.ingest.dev.connectors"}
	prod := []string{"agentray.events.ingest.prod", "agentray.events.ingest.prod.connectors"}

	tests := []struct {
		name           string
		streamSubjects []string
		filterSubjects []string
		wantWired      bool
		wantDedicated  bool
	}{
		{
			name:           "shipped shape: the stream holds exactly what the colour consumes",
			streamSubjects: dev,
			filterSubjects: dev,
			wantWired:      true,
			wantDedicated:  true,
		},
		{
			name:           "another environment rewrote the stream's subjects",
			streamSubjects: prod,
			filterSubjects: dev,
			wantWired:      false,
			wantDedicated:  false,
		},
		{
			name:           "one env's connector subject is missing from the stream",
			streamSubjects: []string{dev[0]},
			filterSubjects: dev,
			wantWired:      false,
			wantDedicated:  false,
		},
		{
			name:           "shared stream still carries this colour's events: wired, not dedicated",
			streamSubjects: append(append([]string{}, dev...), prod...),
			filterSubjects: dev,
			wantWired:      true,
			wantDedicated:  false,
		},
		{
			name:           "a consumer with no filter is offered the whole stream",
			streamSubjects: dev,
			filterSubjects: nil,
			wantWired:      true,
			wantDedicated:  true,
		},
		{
			name:           "a wildcard on either side cannot be expanded, so neither claim is made",
			streamSubjects: []string{"agentray.events.>"},
			filterSubjects: dev,
			wantWired:      true,
			wantDedicated:  false,
		},
		{
			name:           "wildcard filter is not treated as provably unwired",
			streamSubjects: dev,
			filterSubjects: []string{"agentray.events.ingest.>"},
			wantWired:      true,
			wantDedicated:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			wired, dedicated := streamCarries(tt.streamSubjects, tt.filterSubjects)
			if wired != tt.wantWired || dedicated != tt.wantDedicated {
				t.Fatalf("streamCarries(%v, %v) = wired=%v dedicated=%v, want %v/%v",
					tt.streamSubjects, tt.filterSubjects, wired, dedicated, tt.wantWired, tt.wantDedicated)
			}
		})
	}
}

// streamCarries' verdicts feed EvaluateReplay, so the end-to-end shape of the
// shared-stream false refusal is pinned too: dev's two subjects, prod's on the
// same stream, nothing of dev's left to deliver, and a purge frontier that
// belongs entirely to prod.
func TestSharedStreamIsNotReadAsLoss(t *testing.T) {
	dev := []string{"agentray.events.ingest.dev", "agentray.events.ingest.dev.connectors"}
	prod := []string{"agentray.events.ingest.prod", "agentray.events.ingest.prod.connectors"}
	wired, dedicated := streamCarries(append(append([]string{}, dev...), prod...), dev)
	v := EvaluateReplay(7, 140, 100, 0, 0, wired, dedicated)
	if !v.Ready || v.Reason != ReplayCaughtUp {
		t.Fatalf("verdict = %+v, want caught-up: a foreign purge is not this colour's loss", v)
	}
}
