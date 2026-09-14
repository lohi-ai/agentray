package ingestion

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/lohi-ai/agentray/internal/dataplane/store"
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
// That mark is a claim about a FILE, though, and the durable outlives the file
// its acks were made against: a recreated volume, or a colour pointed at a
// never-before-existing DUCKDB_PATH, keeps the floor while the file starts
// empty, and everything published afterwards applies normally — so the file is
// not empty, it is missing exactly the range the floor claims. The store's own
// record is what catches that, and it is read once at boot, before this process
// applies anything of its own (bindStore → ReplayStoreBehind, and
// storage/duckdb_position.go for the record).
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
	// Missing counts messages this colour never applied. It is a stream
	// sequence count for the retention refusals (sequences below First that are
	// gone) and a delivery count for ReplayStoreBehind, which counts against the
	// consumer's own sequence space; either way non-zero means waiting cannot
	// help.
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
	// ReplayUnverified is this colour's retention-loss marker failing to read
	// (latchBootGap): whether it lost rows to retention could not be
	// established, and a colour whose coherence is unestablished must not take
	// traffic. A restart re-reads the marker. (A broker that cannot be sampled
	// at boot is not this: the worker refuses to start at all rather than
	// consume over a provenance it never established — see bindStore.)
	ReplayUnverified = "boot-unverified"
	// ReplayStoreBehind is the DuckDB file behind this colour failing to show
	// the range the durable's ack floor claims: the durable has acknowledged
	// messages this store never applied. Its cause is a store that was not the
	// one those acks were made against — a recreated volume, a new DUCKDB_PATH,
	// a stream recreated under a warm consumer — and no amount of waiting fixes
	// it, because the messages at fault were applied to some other file and are
	// not on the stream any more. The refusal is written into the store, so a
	// restart does not clear it; the operator's move is to reset the colour's
	// durable (or accept the loss by starting that colour on a store that has
	// nothing to lose) rather than to wait (see infra/gce/deploy.sh).
	ReplayStoreBehind = "store-behind"
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
// one concrete subject. Only the decidable half is answered: false means "not
// provably disjoint", never "overlapping", because the caller uses true to
// refuse a colour and a maybe must not refuse a working one.
//
// Two things are decidable. The length a subject must have: `*` is exactly one
// token and `>` is one or more, so a pattern with no `>` matches one length and
// a pattern with one matches a range — disjoint ranges cannot both match. And
// the tokens they share: `*` and `>` match anything, so only two different
// literals prove the patterns apart, and only if every token before them matched.
func subjectsDisjoint(a, b string) bool {
	at := strings.Split(a, ".")
	bt := strings.Split(b, ".")
	if disjointLengths(at, bt) {
		return true
	}
	for i := range min(len(at), len(bt)) {
		if at[i] == ">" || bt[i] == ">" {
			// `>` absorbs everything after it, so a difference further along
			// cannot be proven from here.
			return false
		}
		if at[i] == bt[i] || at[i] == "*" || bt[i] == "*" {
			continue
		}
		return true
	}
	return false
}

// disjointLengths reports whether no single subject length can satisfy both
// token lists.
func disjointLengths(at, bt []string) bool {
	amin, amax := tokenLengthRange(at)
	bmin, bmax := tokenLengthRange(bt)
	if amax != 0 && bmin > amax {
		return true
	}
	if bmax != 0 && amin > bmax {
		return true
	}
	return false
}

// tokenLengthRange is the range of subject lengths a pattern matches, with max 0
// meaning unbounded.
func tokenLengthRange(tokens []string) (int, int) {
	for i, token := range tokens {
		if token == ">" {
			return i + 1, 0
		}
	}
	return len(tokens), len(tokens)
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

// ingestLoss is the on-disk record of a proven retention loss: what was missing
// when it was found, and when. Small on purpose — it is a refusal, not a repair.
type ingestLoss struct {
	Missing    uint64 `json:"missing"`
	DetectedAt string `json:"detected_at"`
	Durable    string `json:"durable,omitempty"`
}

// readLatch returns a loss recorded by an earlier process of this colour, so a
// restart cannot forget one. Three outcomes, and the two failures do not
// collapse: no marker is the ordinary case; a marker that exists but cannot be
// read is a colour that recorded a loss we can no longer size, which refuses as
// "unverified" rather than passing as whole. A file that is absent is the only
// reason to keep the boot sample as the sole judge.
func (ss *StreamSet) readLatch() (missing uint64, found bool, unreadable bool) {
	if ss.LossMarkerPath == "" {
		return 0, false, false
	}
	raw, err := os.ReadFile(ss.LossMarkerPath)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("ingestion: retention loss marker %s unreadable: %v", ss.LossMarkerPath, err)
			return 0, false, true
		}
		return 0, false, false
	}
	var loss ingestLoss
	if err := json.Unmarshal(raw, &loss); err != nil {
		log.Printf("ingestion: retention loss marker %s malformed: %v", ss.LossMarkerPath, err)
		return 0, false, true
	}
	return loss.Missing, loss.Missing > 0, false
}

// recordLatch writes the loss next to this colour's DuckDB file, through a
// temporary file and a rename so a process killed mid-write cannot leave a torn
// marker behind: a half-written marker is unreadable, and unreadable now refuses
// (see readLatch), which would turn this container's rename into a wrong-shaped
// refusal for every later boot. A write that fails leaves the in-process latch
// standing (this process still refuses); it is logged because the NEXT process
// will not know.
func (ss *StreamSet) recordLatch(missing uint64) {
	if ss.LossMarkerPath == "" {
		return
	}
	body, err := json.Marshal(ingestLoss{
		Missing:    missing,
		DetectedAt: time.Now().UTC().Format(time.RFC3339),
		Durable:    ss.Durable,
	})
	if err != nil {
		log.Printf("ingestion: cannot encode retention loss: %v", err)
		return
	}
	tmp, err := os.CreateTemp(filepath.Dir(ss.LossMarkerPath), ".ingest-loss-*")
	if err == nil {
		var writeErr error
		if _, writeErr = tmp.Write(append(body, '\n')); writeErr == nil {
			writeErr = tmp.Sync()
		}
		if closeErr := tmp.Close(); writeErr == nil {
			writeErr = closeErr
		}
		if writeErr == nil {
			writeErr = os.Rename(tmp.Name(), ss.LossMarkerPath)
		}
		if writeErr != nil {
			_ = os.Remove(tmp.Name())
			err = writeErr
		}
	}
	if err != nil {
		log.Printf("ingestion: cannot record retention loss at %s: %v — reset the stream's consumers or delete the colour's volume", ss.LossMarkerPath, err)
	}
}

// latchBootGap is a loss this colour detected when it booted: sequences above
// its applied mark that are no longer in the stream. It is latched because the
// live signal is only visible while the colour is behind — once it applies the
// retained window its applied mark passes the purge frontier and the broker
// reports nothing wrong (measured: after an explicit Purge the consumer's ack
// floor is even advanced over the purged range). Sampling once at boot is the
// moment the question is well posed: "what had I applied, and what is still
// here?". A colour that lost rows must never be switched to, so the latch keeps
// the refusal instead of letting it evaporate as the replay progresses — and
// because the operator's next move after a refusal is usually a restart, the
// loss is also written beside this colour's DuckDB file, where it survives the
// process that found it. A restart re-samples, and can still find nothing.
//
// The sample is read once at boot (bootSample) and passed in, because the store
// binding asks a question of the same two readings; a sample that cannot be read
// at all stops the boot there rather than reaching this function, so the two
// latches below are the only refusals it can add.
// The comparison itself is only made on a dedicated stream — on a shared one it
// would latch another environment's purge as this colour's loss, permanently,
// which is the opposite of fail-safe.
func (ss *StreamSet) latchBootGap(cinfo *jetstream.ConsumerInfo, sinfo *jetstream.StreamInfo) {
	switch missing, found, unreadable := ss.readLatch(); {
	case unreadable:
		ss.bootUnverified.Store(true)
		log.Printf("ingestion: this colour has a retention loss marker at %s that cannot be read; /readyz refuses until an operator accepts the loss (delete the marker) or resets the colour", ss.LossMarkerPath)
		return
	case found:
		ss.bootGap.Store(missing)
		log.Printf("ingestion: this colour recorded a retention loss of %d message(s) at %s; /readyz refuses until an operator accepts it (see infra/gce/deploy.sh)", missing, ss.LossMarkerPath)
		return
	}
	if _, dedicated := streamCarries(sinfo.Config.Subjects, consumerFilterSubjects(cinfo)); !dedicated {
		return
	}
	missing := uint64(0)
	switch {
	case cinfo.AckFloor.Stream == 0:
		// A durable that has applied nothing has no history to lose, so a
		// purge frontier above its mark is not its loss — UNLESS the stream
		// has already lost everything it ever held (first past last): then
		// there is nothing retained for this colour to replay and nothing to
		// catch up to, and a colour that has applied nothing would serve an
		// empty file beside a sibling holding the history. Latched for the
		// same reason the gap below is: the live predicate sees a caught-up
		// colour the moment the first new message is applied (its mark then
		// covers the whole retained window).
		if sinfo.State.LastSeq > 0 && sinfo.State.FirstSeq > sinfo.State.LastSeq {
			missing = sinfo.State.LastSeq
		}
	case sinfo.State.FirstSeq > cinfo.AckFloor.Stream+1:
		missing = sinfo.State.FirstSeq - cinfo.AckFloor.Stream - 1
	}
	if missing > 0 {
		ss.bootGap.Store(missing)
		ss.recordLatch(missing)
		log.Printf("ingestion: retention loss: %d message(s) this colour never applied are no longer in %s; /readyz refuses until an operator accepts it", missing, ss.Ingest.CachedInfo().Config.Name)
	}
}

// bootSample reads the broker state the boot decisions need — the durable's own
// state and the stream's — and it is the ONE reading point for both, so a
// failure to read is decided once rather than once per question. It retries:
// the sample runs on a connection that just created this consumer, so a failure
// is far more likely to be a broker still settling (a leader election, a slow
// first API call after connect) than a real outage, and refusing readiness for
// the life of the process over one of those would cost a deploy that re-running
// would not fix. Past the retries the sample is treated as unreadable.
func (ss *StreamSet) bootSample(ctx context.Context, consumer jetstream.Consumer) (*jetstream.ConsumerInfo, *jetstream.StreamInfo, error) {
	for attempt := 1; ; attempt++ {
		cinfo, err := consumer.Info(ctx)
		if err == nil {
			sinfo, streamErr := ss.Ingest.Info(ctx)
			if streamErr == nil {
				return cinfo, sinfo, nil
			}
			err = fmt.Errorf("stream info: %w", streamErr)
		} else {
			err = fmt.Errorf("consumer info: %w", err)
		}
		if attempt >= bootSampleAttempts {
			return nil, nil, err
		}
		sleepBeforeBootSampleRetry(attempt)
	}
}

// positionStore is the store-side surface the readiness binding needs: the
// record of where a colour's own writes have carried its DuckDB file along the
// durable stream, and the two facts a boot writes into it (see bindStore).
// *storage.Store and *storage.DuckDB both satisfy it, and ingestStore embeds it,
// so a worker cannot be wired to a store that would leave its readiness claim
// unprovable.
type positionStore interface {
	AppliedPosition(ctx context.Context, durable string) (storage.AppliedPosition, error)
	AdoptPosition(ctx context.Context, durable string, seq uint64) error
	RefusePosition(ctx context.Context, durable string, missing uint64) error
}

// bindStore binds this colour's readiness claim to the DuckDB file behind it. It
// runs at boot, before the worker consumes anything.
//
// Why the store: the durable's ack floor is a claim ABOUT a file — "these
// messages were applied" — but the durable outlives the file those acks were
// made against. Recreate a per-colour volume, or point a colour at a
// never-before-existing DUCKDB_PATH, and the consumer keeps its floor while the
// file starts empty. Everything published after that boot is applied normally,
// so the file is not empty — it is missing exactly the range the floor claims,
// and `applied >= head` is true of it. Only the store's own record can
// contradict the durable (see storage/duckdb_position.go).
//
// Why at boot: this process has applied nothing yet, so the record still shows
// where the file stood when the durable's floor was inherited. One message
// applied first would move the record past the hole and hide it — and because
// the refusal is written into the store, a restart cannot launder it either.
//
// Why consumer sequences: AckFloor.Consumer counts only the deliveries this
// colour was offered, so it is comparable to the position its writes recorded
// even when the stream is shared with another environment whose sequences are
// interleaved with ours. Stream sequences are not (see streamCarries).
//
// One migration edge, named rather than hidden: a file written by a build that
// kept no position record (this one) has history but no mark, and refusing it
// would wedge the deploy that ships this check on every colour that is already
// serving. Such a file is adopted at the durable's current position — the
// claim that its history is the history those acks were made for, which is the
// normal case for an upgrade and no worse than the check's absence for the
// abnormal one. A file created EMPTY is never adopted: that is the defect.
// A binding that cannot be established is NOT tolerated, and that is deliberate:
// this process consumes as soon as it is up, and every ack it makes moves the
// durable's floor. A boot that could not read the pair (sample unreadable), could
// not read the file's own record, or could not WRITE DOWN what it found, would go
// on to acknowledge a range whose provenance is unproven — and the moment one of
// its writes lands, the record matches the floor and the next boot reads the hole
// as coherence. So the failure is returned and the worker does not start: the
// container comes back and asks again. Nothing is lost by refusing to start — the
// stream is durable, the sibling colour keeps serving, and the broker was
// required for the consumer above anyway — while the alternative is the silent
// hole this whole record exists to catch.
func (ss *StreamSet) bindStore(ctx context.Context, cinfo *jetstream.ConsumerInfo) error {
	if ss.Positions == nil {
		return nil
	}
	durable := ss.durableName()
	floor := cinfo.AckFloor.Consumer
	pos, err := ss.Positions.AppliedPosition(ctx, durable)
	if err != nil {
		return fmt.Errorf("read the applied position this store holds for durable %q: %w", durable, err)
	}
	switch {
	case pos.RefusedMissing > 0:
		// An earlier boot proved the gap and wrote it down. Traffic applied since
		// must not launder it: the messages at fault are not on the stream any
		// more, so the file can never come to hold them.
		ss.storeGap.Store(pos.RefusedMissing)
		log.Printf("ingestion: this store recorded %d acknowledged message(s) it never applied; /readyz refuses until the colour's durable is reset or its volume replaced (see infra/gce/deploy.sh)", pos.RefusedMissing)
	case floor <= pos.Seq:
		// The file's own writes reach at least as far as the durable claims, or
		// the durable claims nothing yet.
	case !pos.Known && pos.HasIngest:
		if err := ss.Positions.AdoptPosition(ctx, durable, floor); err != nil {
			return fmt.Errorf("adopt the applied position of an existing store for durable %q: %w", durable, err)
		}
		log.Printf("ingestion: adopted the applied position of an existing store at consumer sequence %d for durable %q", floor, durable)
	default:
		missing := floor - pos.Seq
		ss.storeGap.Store(missing)
		if err := ss.Positions.RefusePosition(ctx, durable, missing); err != nil {
			return fmt.Errorf("write down the %d acknowledged message(s) this store (record %d) cannot show against durable %q's floor %d: %w",
				missing, pos.Seq, durable, floor, err)
		}
		log.Printf("ingestion: readiness refuses: durable %q has acknowledged %d message(s) this store never applied (its record reaches %d, the durable's floor is %d); switching traffic here would serve a file missing that range", durable, missing, pos.Seq, floor)
	}
	return nil
}

// sleepBeforeBootSampleRetry spaces the boot sample's retries. Back-to-back
// attempts span milliseconds, which does not cover the failure they exist for — a
// broker that is settling after a restart, a leader election, a first API call
// slower than usual — so each retry waits a little longer than the last. The
// container's healthcheck start_period (30s) is far larger than the whole budget
// here, so the wait costs the deploy nothing.
func sleepBeforeBootSampleRetry(attempt int) {
	time.Sleep(time.Duration(attempt) * time.Second)
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
		} else if missing := ss.storeGap.Load(); missing > 0 {
			// The store behind this colour cannot show the range the durable has
			// acknowledged. The live numbers cannot see this — that is the whole
			// defect — so the boot binding is the only thing that reports it.
			v.Ready = false
			v.Reason = ReplayStoreBehind
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
