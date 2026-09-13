package ingestion

import (
	"context"
	"fmt"
	"log"
	"strings"

	"github.com/nats-io/nats.go/jetstream"
)

// Blue-green readiness: a colour is "caught up" when everything published to the
// ingest stream that this colour is offered has been applied to ITS DuckDB file,
// and only then may a deploy point traffic at it.
//
// Why the ack floor is the applied mark: the worker acknowledges a message only
// after its DuckDB write landed (batcher.flush → settleFailure; the connector
// branch in queue.go settles the same way). So the consumer's AckFloor.Stream —
// "the message before the first unacknowledged message" — is the highest
// sequence this colour has durably applied, contiguously, across events AND
// connector rows, because one durable consumer carries both subjects.
//
// The predicate has to ask two questions of the broker's OWN configuration
// before it trusts either number, because a consumer is filter-scoped and the
// sequence counters are not (see streamCarries):
//
//   - wired — does the stream carry this colour's subjects? If it does not, the
//     consumer will never be offered another row, so "no work pending" describes
//     a pipeline that is dead. (Its filter can still match messages the stream
//     retained under the old subjects, so the counts may not be zero yet — which
//     is precisely why the refusal cannot rest on them.) Refusing is the only
//     honest answer, and it is the failure the gate exists to catch: a green
//     healthcheck in front of a colour that will never see another row.
//   - dedicated — does this consumer see the stream's whole sequence space?
//     Only then is comparing a stream-wide number against a consumer-local one
//     sound. Neither shipped env sets INGEST_STREAM_NAME (infra/gce/{dev,prod}/
//     app.env), so both default to AGENTRAY_EVENTS on the shared VM broker while
//     publishing on different subjects, and EnsureStreams rewrites that one
//     stream's subject list to its own env's on every boot (measured: a second
//     CreateOrUpdateStream with different subjects succeeds, after which
//     publishing on the first env's subject fails with "no response from
//     stream"). A consumer therefore sees a LastSeq that counts sequences it is
//     never offered, so `applied >= head` can never come true and FirstSeq walks
//     past the applied mark when only the OTHER env's messages age out — one
//     branch blocking every deploy forever, the other reporting unrecoverable
//     loss of rows that were never this colour's. Neither number is used there;
//     NumPending and NumAckPending are counted in the consumer's own
//     (filter-local) sequence space, so "nothing matching my filter is left to
//     deliver and nothing delivered is left to apply" is a proof that this
//     colour holds everything it can receive.
//
// The price of that is named rather than hidden: on a stream whose subject list
// is broader than this consumer's filter, a retention loss of this colour's OWN
// undelivered messages is indistinguishable from the other env's traffic aging
// out, so it is not detected — and refusing on the unprovable reading would
// wedge every deploy the moment the neighbour is quiet. The sound repair is a
// stream per environment (INGEST_STREAM_NAME), not a looser predicate. On the
// shipped topology the question does not arise: EnsureStreams rewrites the
// stream to one env's subjects, so a colour is either dedicated (it owns the
// stream) or unwired (refused), never both.
//
// The third number, State.FirstSeq, distinguishes "still replaying" from "can
// never catch up". LimitsPolicy purges by age regardless of ack state (30 days,
// jetstream.go), so a colour parked longer than that window finds messages above
// its applied mark already gone. That is unrecoverable loss and no amount of
// waiting fixes it: the gate must refuse rather than serve a colour with a hole.
// A colour that has never applied anything (AckFloor 0 — first boot, fresh
// durable) is exempt from that: it has no history to lose, and everything the
// stream still holds IS its history. The exemption has one edge, and it refuses:
// a stream whose retained window is empty (first > head) while it has carried
// messages (head > 0) holds nothing for this colour to replay and nothing for it
// to catch up to, so a colour that has applied nothing would serve an empty file
// beside a sibling holding the history. Refusing is the safe direction there,
// and the documented recovery is an operator decision to reset both colours,
// not a repair this predicate performs.
type ReplayVerdict struct {
	// Ready is true only when this colour has applied everything it can receive.
	Ready bool `json:"ready"`
	// Applied is this colour's applied high-water mark (consumer ack floor).
	Applied uint64 `json:"applied,omitempty"`
	// Head is the stream's highest sequence. Stream-wide, so on a stream shared
	// with another environment it can exceed what this consumer will ever be
	// offered — which is why it is only compared when the colour is dedicated.
	Head uint64 `json:"head,omitempty"`
	// First is the stream's lowest retained sequence — the purge floor.
	First uint64 `json:"first,omitempty"`
	// Missing counts sequences below First that this colour never applied;
	// non-zero means it cannot catch up by waiting.
	Missing uint64 `json:"missing,omitempty"`
	// Pending is matching work not yet delivered; AckPending is delivered work
	// not yet applied. Diagnostics — they are also the satisfiability half of
	// the predicate.
	Pending    uint64 `json:"pending,omitempty"`
	AckPending int    `json:"ack_pending,omitempty"`
	// Reason is one of the Replay* values below — plus ReplayUnavailable, which
	// the /readyz handler substitutes when ReplayStatus itself errors and there
	// is no verdict to report at all.
	Reason string `json:"reason,omitempty"`
}

// Replay reason values, also the /readyz vocabulary an operator reads when a
// deploy is refused.
const (
	ReplayCaughtUp  = "caught-up"
	ReplayBehind    = "replaying"
	ReplayPurgedGap = "purged-gap"
	// ReplayStreamMismatch is the stream not carrying this colour's subjects:
	// the consumer is offered nothing, so it cannot be caught up with anything.
	// The operator's remedy is the stream's subject list, not more waiting.
	ReplayStreamMismatch = "stream-mismatch"
	// ReplayUnverified is the boot sample (latchBootGap) failing to read the
	// broker: whether this colour lost rows to retention at boot could not be
	// established, and a colour whose coherence is unestablished must not take
	// traffic. A restart re-samples.
	ReplayUnverified = "boot-unverified"
	// ReplayUnavailable is not produced here: the readyz handler sets it when the
	// probe could not read the broker at all, which is the same refusal one layer
	// up. It lives with the others because /readyz is one vocabulary.
	ReplayUnavailable = "unavailable"
)

// EvaluateReplay is the whole predicate, as a pure function of what the broker
// reports, so every branch is testable without a broker. wired and dedicated
// come from streamCarries and describe the broker's own configuration.
//
// Branch order is load-bearing. A stream that does not carry this colour's
// subjects makes every other number meaningless, so it is checked first. The gap
// branch comes next, before either caught-up shortcut: measured on a real
// broker, when messages age out under MaxAge while a colour is parked its
// undelivered work drops to zero and the stream's FirstSeq moves above its
// applied mark — so "nothing is pending" is true of a colour that is missing
// rows. Checking the gap first is what makes the loss visible; checking
// readiness first would report that colour caught-up.
func EvaluateReplay(applied, head, first uint64, pending uint64, ackPending int, wired, dedicated bool) ReplayVerdict {
	v := ReplayVerdict{Applied: applied, Head: head, First: first, Pending: pending, AckPending: ackPending}
	if !wired {
		v.Reason = ReplayStreamMismatch
		return v
	}
	if dedicated {
		if applied > 0 && first > applied+1 {
			v.Missing = first - applied - 1
			v.Reason = ReplayPurgedGap
			return v
		}
		// The exemption's edge: a fresh durable (applied == 0) over a stream
		// whose retained window is already empty (first > head, head > 0). There
		// is nothing to replay and nothing to catch up to, so the pending proof
		// below would report caught-up for a colour that will hold nothing its
		// sibling has — every one of those sequences is missing from its file.
		if applied == 0 && head > 0 && first > head {
			v.Missing = head
			v.Reason = ReplayPurgedGap
			return v
		}
	}
	if (dedicated && applied >= head) || (pending == 0 && ackPending == 0) {
		v.Ready = true
		v.Reason = ReplayCaughtUp
		return v
	}
	v.Reason = ReplayBehind
	return v
}

// streamCarries answers the two questions the predicate needs about the
// broker's configuration, from the stream's subject list and the filter the
// consumer was created with:
//
//	wired     — the stream can deliver this consumer's subjects at all. False
//	            means it receives nothing, so every other number is vacuous.
//	dedicated — the consumer sees the stream's whole sequence space, which is
//	            what makes stream-wide comparisons exact.
//
// Both are decided subject by subject rather than by comparing lists, so a
// wildcard on ONE side is still answered exactly — the literal side decides it,
// including when the wildcard provably cannot match. Only two wildcards leave a
// question open, and that one is answered in the direction that keeps a deploy
// moving (wired) rather than one that wedges it. A consumer with no filter is
// offered everything the stream holds, so it is both.
func streamCarries(streamSubjects, filterSubjects []string) (wired, dedicated bool) {
	if len(filterSubjects) == 0 {
		return true, true
	}
	for _, f := range filterSubjects {
		if !filterCarried(streamSubjects, f) {
			// The consumer is configured to receive a subject the stream cannot
			// deliver: that half of the pipeline can never deliver.
			return false, false
		}
	}
	for _, s := range streamSubjects {
		if !subjectLiteral(s) {
			// A wildcard stream subject covers subjects this function cannot
			// enumerate, so it cannot be shown to be this consumer's alone.
			return true, false
		}
		if !filterCovers(filterSubjects, s) {
			// The stream holds subjects this consumer is not offered, so its ack
			// floor and the stream's head count different things.
			return true, false
		}
	}
	return true, true
}

// filterCarried reports whether some subject on the stream can deliver one
// consumer filter subject.
func filterCarried(streamSubjects []string, filter string) bool {
	for _, s := range streamSubjects {
		if subjectLiteral(filter) {
			// A literal filter is answered by whichever side is a pattern.
			if subjectMatches(s, filter) {
				return true
			}
			continue
		}
		if !subjectLiteral(s) {
			// Two patterns. They can still be PROVABLY disjoint (a token that
			// differs on both sides), and that case is a dead pipeline rather
			// than an unknown one; short of proof the consumer is assumed
			// reachable, because refusing on a maybe would wedge a deploy.
			if !subjectsDisjoint(s, filter) {
				return true
			}
			continue
		}
		if subjectMatches(filter, s) {
			return true
		}
	}
	return false
}

// subjectsDisjoint reports whether two subject patterns cannot both match any
// one concrete subject. Only the decidable half is answered: matching stops as
// soon as a token makes overlap possible, so false means "not provably
// disjoint", never "overlapping". A `*` or `>` on either side always leaves
// overlap possible.
func subjectsDisjoint(a, b string) bool {
	at := strings.Split(a, ".")
	bt := strings.Split(b, ".")
	for i := 0; i < len(at) && i < len(bt); i++ {
		if at[i] == ">" || bt[i] == ">" {
			return false
		}
		if at[i] == "*" || bt[i] == "*" || at[i] == bt[i] {
			return false
		}
		return true
	}
	return false
}

// filterCovers reports whether some consumer filter is offered a literal
// stream subject.
func filterCovers(filterSubjects []string, stream string) bool {
	for _, f := range filterSubjects {
		if subjectMatches(f, stream) {
			return true
		}
	}
	return false
}

// subjectLiteral reports whether a subject is a concrete name rather than a
// pattern over tokens.
func subjectLiteral(subject string) bool {
	return !strings.ContainsAny(subject, "*>")
}

// subjectMatches applies the NATS subject grammar: tokens are dot-separated,
// `*` stands for exactly one token and `>` for one or more trailing tokens.
// pattern is the pattern; subject the concrete name being tested.
func subjectMatches(pattern, subject string) bool {
	pt := strings.Split(pattern, ".")
	st := strings.Split(subject, ".")
	for i, p := range pt {
		if p == ">" {
			// `>` needs at least one token left to consume.
			return len(st) > i
		}
		if i >= len(st) {
			return false
		}
		if p != "*" && p != st[i] {
			return false
		}
	}
	return len(pt) == len(st)
}

// consumerFilterSubjects is the consumer's filter in the one form both
// streamCarries and the broker's config expose: the multi-filter field when it
// is set, otherwise the single-filter one. A consumer that reports neither is
// filterless — it is offered the whole stream.
func consumerFilterSubjects(cinfo *jetstream.ConsumerInfo) []string {
	if len(cinfo.Config.FilterSubjects) > 0 {
		return cinfo.Config.FilterSubjects
	}
	if cinfo.Config.FilterSubject != "" {
		return []string{cinfo.Config.FilterSubject}
	}
	return nil
}

// bootSampleAttempts bounds the retry around the boot sample's reads. The
// sample runs on a connection that just created this consumer, so a failure is
// far more likely to be a broker still settling (a leader election, a slow
// first API call after connect) than a real outage; refusing readiness for the
// life of the process over one of those would cost a deploy that re-running
// would not fix. Past the retries the sample is treated as unreadable.
const bootSampleAttempts = 3

// latchBootGap is a loss this colour detected when it booted: sequences above
// its applied mark that are no longer in the stream. It is latched because the
// live signal is only visible while the colour is behind — once it applies the
// retained window its applied mark passes the purge frontier and the broker
// reports nothing wrong (measured: after an explicit Purge the consumer's ack
// floor is even advanced over the purged range). Sampling once at boot is the
// moment the question is well posed: "what had I applied, and what is still
// here?". A colour that lost rows must never be switched to, so the latch keeps
// the refusal for the life of the process instead of letting it evaporate as
// the replay progresses.
//
// A sample that cannot be read at all latches ReplayUnverified rather than
// returning: the one reading that can prove a loss is gone forever once the
// replay advances, and "I could not tell" has to refuse for the same reason "I
// lost rows" does. A restart takes a fresh sample. The comparison itself is only
// made on a dedicated stream — on a shared one it would latch another
// environment's purge as this colour's loss, permanently, which is the opposite
// of fail-safe.
func (ss *StreamSet) latchBootGap(ctx context.Context, consumer jetstream.Consumer) {
	for attempt := 1; ; attempt++ {
		cinfo, err := consumer.Info(ctx)
		if err != nil {
			if attempt < bootSampleAttempts {
				continue
			}
			ss.bootUnverified.Store(true)
			log.Printf("ingestion: boot replay sample unreadable (consumer info: %v); /readyz will refuse until this colour restarts", err)
			return
		}
		if cinfo.AckFloor.Stream == 0 {
			return
		}
		sinfo, err := ss.Ingest.Info(ctx)
		if err != nil {
			if attempt < bootSampleAttempts {
				continue
			}
			ss.bootUnverified.Store(true)
			log.Printf("ingestion: boot replay sample unreadable (stream info: %v); /readyz will refuse until this colour restarts", err)
			return
		}
		if _, dedicated := streamCarries(sinfo.Config.Subjects, consumerFilterSubjects(cinfo)); !dedicated {
			return
		}
		if sinfo.State.FirstSeq > cinfo.AckFloor.Stream+1 {
			ss.bootGap.Store(sinfo.State.FirstSeq - cinfo.AckFloor.Stream - 1)
		}
		return
	}
}

// ReplayStatus reads the live consumer and stream state and evaluates the
// predicate. An error means "cannot tell", which the caller must treat as not
// ready: a colour whose coherence cannot be established must not take traffic.
func (ss *StreamSet) ReplayStatus(ctx context.Context) (ReplayVerdict, error) {
	// The stream is read FIRST, so the head it reports cannot include a message
	// published after the consumer read: whenever the pending proof below carries
	// the verdict, it must cover every sequence below Head, or "nothing pending"
	// would be true of a colour that has not applied a batch accepted inside this
	// very probe. FirstSeq only grows, so a purge landing between the two reads
	// is still visible to the next probe rather than being missed.
	sinfo, err := ss.Ingest.Info(ctx)
	if err != nil {
		return ReplayVerdict{}, fmt.Errorf("stream info: %w", err)
	}
	consumer, err := ss.Ingest.Consumer(ctx, ss.durableName())
	if err != nil {
		return ReplayVerdict{}, fmt.Errorf("ingest consumer %q: %w", ss.durableName(), err)
	}
	cinfo, err := consumer.Info(ctx)
	if err != nil {
		return ReplayVerdict{}, fmt.Errorf("consumer info: %w", err)
	}
	wired, dedicated := streamCarries(sinfo.Config.Subjects, consumerFilterSubjects(cinfo))
	v := EvaluateReplay(
		cinfo.AckFloor.Stream,
		sinfo.State.LastSeq,
		sinfo.State.FirstSeq,
		cinfo.NumPending,
		cinfo.NumAckPending,
		wired,
		dedicated,
	)
	// A latch may only DOWNGRADE a live verdict, never replace one that already
	// refuses: while the colour is replaying, "replaying" is the more useful
	// answer, and if the stream is miswired the mismatch diagnosis is the one
	// whose remedy actually unblocks the deploy. The latch exists for the moment
	// the live reading goes ready and would otherwise hide a loss.
	if v.Ready {
		// A latched boot gap is the more specific diagnosis, so it wins over an
		// unreadable sample when both are set.
		if missing := ss.bootGap.Load(); missing > 0 {
			v.Ready = false
			v.Reason = ReplayPurgedGap
			if missing > v.Missing {
				v.Missing = missing
			}
		} else if ss.bootUnverified.Load() {
			v.Ready = false
			v.Reason = ReplayUnverified
		}
	}
	return v, nil
}

// durableName is the consumer name this process consumes under. StartJetStreamWorker
// creates it; ReplayStatus reads it, so the default lives in one place.
func (ss *StreamSet) durableName() string {
	if ss.Durable == "" {
		return "agentray-ingestors"
	}
	return ss.Durable
}
