package ingestion

import (
	"testing"

	"github.com/nats-io/nats.go/jetstream"
)

// The readiness predicate decides whether a blue-green deploy may point traffic
// at a colour, so every branch is pinned here: the two ways to be caught up, the
// two ways to be behind, the branch that must never converge, and the ways the
// broker's own configuration disqualifies the rest of the arithmetic.
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
			name: "shared stream, filter has no work left: the pending proof carries the verdict",
			// applied < head and head counts the other env's sequences, but this
			// consumer has nothing outstanding. Pinned as the shape the pending
			// shortcut must keep answering ready on a stream that is not this
			// colour's own.
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
			// The same shape as the purged-gap case below, but the stream carries
			// subjects this consumer is not offered, so the purge frontier says
			// nothing this consumer can act on. Latching a refusal here would
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
			// applied == 0 and there are still retained messages: it has no
			// history to lose, so the retained window is its whole history and it
			// just replays it.
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
			name: "fresh durable over a stream with nothing retained cannot catch up",
			// The exemption's edge: there is nothing left to replay and nothing to
			// catch up to, so a colour that has applied nothing would serve an
			// empty file beside a sibling holding the history. The pending proof
			// would call this caught-up; the purge floor is the only number that
			// sees it.
			applied:    0,
			head:       30000,
			first:      30001,
			wired:      true,
			dedicated:  true,
			wantReady:  false,
			wantReason: ReplayPurgedGap,
			wantMiss:   30000,
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
			name:           "a wildcard over this colour's subjects still covers them",
			streamSubjects: []string{"agentray.events.>"},
			filterSubjects: dev,
			wantWired:      true,
			wantDedicated:  false,
		},
		{
			name: "a wildcard that does not cover them is a mismatch, not an unknown",
			// The literal side decides this one: `agentray.events.prod.>` can
			// never deliver `agentray.events.ingest.dev`, so the consumer is
			// offered nothing and the pending counts stay vacuously zero.
			streamSubjects: []string{"agentray.events.prod.>"},
			filterSubjects: dev,
			wantWired:      false,
			wantDedicated:  false,
		},
		{
			name:           "a wildcard filter that covers every stream subject is dedicated",
			streamSubjects: dev,
			filterSubjects: []string{"agentray.events.ingest.>"},
			wantWired:      true,
			wantDedicated:  true,
		},
		{
			name:           "two patterns cannot be compared, so neither claim is made",
			streamSubjects: []string{"agentray.events.>"},
			filterSubjects: []string{"agentray.events.ingest.>"},
			wantWired:      true,
			wantDedicated:  false,
		},
		{
			name: "two patterns that provably cannot overlap are a mismatch, not an unknown",
			// A differing token on both sides is decidable: `other.>` can never
			// deliver `agentray.events.ingest.>`'s subjects, so calling it wired
			// would green-light a dead pipeline.
			streamSubjects: []string{"other.>"},
			filterSubjects: []string{"agentray.events.ingest.>"},
			wantWired:      false,
			wantDedicated:  false,
		},
		{
			name:           "a pattern that might overlap stays wired",
			streamSubjects: []string{"agentray.>"},
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

// The NATS subject grammar the two questions above are decided with.
func TestSubjectMatches(t *testing.T) {
	tests := []struct {
		pattern string
		subject string
		want    bool
	}{
		{pattern: "a.b.c", subject: "a.b.c", want: true},
		{pattern: "a.b.c", subject: "a.b.d", want: false},
		{pattern: "a.b.c", subject: "a.b", want: false},
		{pattern: "a.b", subject: "a.b.c", want: false},
		{pattern: "a.*.c", subject: "a.b.c", want: true},
		{pattern: "a.*.c", subject: "a.b.d", want: false},
		{pattern: "a.*", subject: "a.b", want: true},
		{pattern: "a.*", subject: "a.b.c", want: false},
		{pattern: "a.>", subject: "a.b", want: true},
		{pattern: "a.>", subject: "a.b.c", want: true},
		{pattern: "a.>", subject: "a", want: false},
		{pattern: ">", subject: "a", want: true},
		{pattern: "agentray.events.>", subject: "agentray.events.ingest.dev", want: true},
		{pattern: "agentray.events.prod.>", subject: "agentray.events.ingest.dev", want: false},
	}

	for _, tt := range tests {
		if got := subjectMatches(tt.pattern, tt.subject); got != tt.want {
			t.Errorf("subjectMatches(%q, %q) = %v, want %v", tt.pattern, tt.subject, got, tt.want)
		}
	}
}

// A durable created by an older single-filter binary reports FilterSubject
// rather than FilterSubjects. Reading it as "no filter" would claim the consumer
// is offered the whole stream and re-enable the stream-wide comparisons the
// wiring questions exist to suppress.
func TestConsumerFilterSubjects(t *testing.T) {
	tests := []struct {
		name string
		info jetstream.ConsumerInfo
		want []string
	}{
		{
			name: "multi-filter",
			info: jetstream.ConsumerInfo{Config: jetstream.ConsumerConfig{
				FilterSubjects: []string{"a", "b"},
			}},
			want: []string{"a", "b"},
		},
		{
			name: "single-filter fallback",
			info: jetstream.ConsumerInfo{Config: jetstream.ConsumerConfig{FilterSubject: "a"}},
			want: []string{"a"},
		},
		{
			name: "filterless consumer",
			info: jetstream.ConsumerInfo{Config: jetstream.ConsumerConfig{}},
			want: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := consumerFilterSubjects(&tt.info)
			if len(got) != len(tt.want) {
				t.Fatalf("consumerFilterSubjects = %v, want %v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Fatalf("consumerFilterSubjects = %v, want %v", got, tt.want)
				}
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
