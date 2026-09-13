package ingestion

import (
	"context"
	"fmt"
)

// Blue-green readiness: a colour is "caught up" when everything published to the
// ingest stream has been applied to ITS DuckDB file, and only then may a deploy
// point traffic at it.
//
// Why the ack floor is the applied mark: the worker acknowledges a message only
// after its DuckDB write landed (batcher.flush → settleFailure; the connector
// branch in queue.go settles the same way). So the consumer's AckFloor.Stream —
// "the message before the first unacknowledged message" — is exactly the highest
// sequence this colour has durably applied, contiguously, across events AND
// connector rows, because one durable consumer carries both subjects.
//
// Comparison is against StreamInfo.State.LastSeq, read *after* the consumer
// info: sequences only grow, so "applied >= head as of a moment after the
// consumer read" is a sound statement about that instant and never a clock race.
//
// That comparison alone is NOT satisfiable on this deployment, which is why the
// predicate also accepts the consumer-local proof. Neither shipped env sets
// INGEST_STREAM_NAME (infra/gce/{dev,prod}/app.env), so both use the default
// AGENTRAY_EVENTS stream on the VM's broker while publishing on different
// subjects — and EnsureStreams rewrites that one stream's subject list to its own
// env's on every boot (measured: a second CreateOrUpdateStream with different
// subjects succeeds, after which publishing on the first env's subject fails with
// "no response from stream"). A consumer therefore sees a LastSeq that counts
// sequences it is never offered, so `applied >= head` can never come true and the
// gate would refuse every deploy forever. NumPending and NumAckPending have no
// such problem: they are counted in the consumer's own (filter-local) sequence
// space, so "nothing matching my filter is left to deliver and nothing delivered
// is left to apply" is a proof that this colour holds everything it can receive.
// Keep both: the sequence comparison stays exact on a dedicated stream, and the
// pending check keeps the gate satisfiable where they are shared.
//
// The third number, State.FirstSeq, distinguishes "still replaying" from "can
// never catch up". LimitsPolicy purges by age regardless of ack state (30 days,
// jetstream.go), so a colour parked longer than that window finds messages above
// its applied mark already gone. That is unrecoverable loss and no amount of
// waiting fixes it: the gate must refuse rather than serve a colour with a hole.
// A colour that has never applied anything (AckFloor 0 — first boot, fresh
// durable) is exempt on purpose: it has no history to lose, and everything the
// stream still holds IS its history. (The exemption's edge case — a fresh
// durable on a stream whose entire backlog has aged out — never becomes ready,
// because there is nothing left to replay and nothing to catch up to. That is
// the safe direction, and the documented recovery is an operator decision to
// reset both colours, not a repair this predicate performs.)
type ReplayVerdict struct {
	// Ready is true only when this colour has applied everything it can receive.
	Ready bool `json:"ready"`
	// Applied is this colour's applied high-water mark (consumer ack floor).
	Applied uint64 `json:"applied"`
	// Head is the stream's highest sequence, read after Applied. Stream-wide, so
	// on a stream shared with another environment it can exceed what this
	// consumer will ever be offered.
	Head uint64 `json:"head"`
	// First is the stream's lowest retained sequence — the purge floor.
	First uint64 `json:"first"`
	// Missing counts sequences below First that this colour never applied;
	// non-zero means it cannot catch up by waiting.
	Missing uint64 `json:"missing"`
	// Pending is matching work not yet delivered; AckPending is delivered work
	// not yet applied. Diagnostics — they are also the satisfiability half of
	// the predicate.
	Pending    uint64 `json:"pending"`
	AckPending int    `json:"ack_pending"`
	// Reason is "caught-up", "replaying" or "purged-gap".
	Reason string `json:"reason"`
}

// Replay reason values, also the /readyz vocabulary an operator reads when a
// deploy is refused.
const (
	ReplayCaughtUp  = "caught-up"
	ReplayBehind    = "replaying"
	ReplayPurgedGap = "purged-gap"
)

// EvaluateReplay is the whole predicate, as a pure function of what the broker
// reports, so every branch is testable without a broker.
//
// The gap branch comes FIRST, before either caught-up shortcut. Measured on a
// real broker: when messages age out under MaxAge while a colour is parked, its
// undelivered work drops to zero and the stream's FirstSeq moves above its
// applied mark — so "nothing is pending" is true of a colour that is missing
// rows. Checking the gap first is what makes the loss visible; checking
// readiness first would report that colour caught-up.
func EvaluateReplay(applied, head, first uint64, pending uint64, ackPending int) ReplayVerdict {
	v := ReplayVerdict{Applied: applied, Head: head, First: first, Pending: pending, AckPending: ackPending}
	if applied > 0 && first > applied+1 {
		v.Missing = first - applied - 1
		v.Reason = ReplayPurgedGap
		return v
	}
	if applied >= head || (pending == 0 && ackPending == 0) {
		v.Ready = true
		v.Reason = ReplayCaughtUp
		return v
	}
	v.Reason = ReplayBehind
	return v
}

// bootGapMissing is a loss this colour detected when it booted: sequences above
// its applied mark that are no longer in the stream. It is latched because the
// live signal is only visible while the colour is behind — once it applies the
// retained window its applied mark passes the purge frontier and the broker
// reports nothing wrong (measured: after an explicit Purge the consumer's ack
// floor is even advanced over the purged range). Sampling once at boot is the
// moment the question is well posed: "what had I applied, and what is still
// here?". A colour that lost rows must never be switched to, so the latch keeps
// the refusal for the life of the process instead of letting it evaporate as
// the replay progresses.
func (ss *StreamSet) latchBootGap(ctx context.Context) {
	consumer, err := ss.Ingest.Consumer(ctx, ss.durableName())
	if err != nil {
		// No consumer yet: a colour that has never consumed has no history to
		// lose, and everything retained is its history.
		return
	}
	cinfo, err := consumer.Info(ctx)
	if err != nil {
		return
	}
	if cinfo.AckFloor.Stream == 0 {
		return
	}
	sinfo, err := ss.Ingest.Info(ctx)
	if err != nil {
		return
	}
	if sinfo.State.FirstSeq > cinfo.AckFloor.Stream+1 {
		ss.bootGap.Store(sinfo.State.FirstSeq - cinfo.AckFloor.Stream - 1)
	}
}

// ReplayStatus reads the live consumer and stream state and evaluates the
// predicate. An error means "cannot tell", which the caller must treat as not
// ready: a colour whose coherence cannot be established must not take traffic.
func (ss *StreamSet) ReplayStatus(ctx context.Context) (ReplayVerdict, error) {
	consumer, err := ss.Ingest.Consumer(ctx, ss.durableName())
	if err != nil {
		return ReplayVerdict{}, fmt.Errorf("ingest consumer %q: %w", ss.durableName(), err)
	}
	cinfo, err := consumer.Info(ctx)
	if err != nil {
		return ReplayVerdict{}, fmt.Errorf("consumer info: %w", err)
	}
	// Head is read AFTER the consumer: a later LastSeq can only make the verdict
	// more conservative, never less.
	sinfo, err := ss.Ingest.Info(ctx)
	if err != nil {
		return ReplayVerdict{}, fmt.Errorf("stream info: %w", err)
	}
	v := EvaluateReplay(
		cinfo.AckFloor.Stream,
		sinfo.State.LastSeq,
		sinfo.State.FirstSeq,
		cinfo.NumPending,
		cinfo.NumAckPending,
	)
	if missing := ss.bootGap.Load(); missing > 0 {
		v.Ready = false
		v.Reason = ReplayPurgedGap
		if missing > v.Missing {
			v.Missing = missing
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
