package observe

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"github.com/lohi-ai/agentray/agentcore"
)

// The "model-visible means logged" invariant.
//
// agentcore's durable resume is only as good as the log's completeness: a
// resumed run rebuilds its history from the append-only log, so ANY message the
// model saw that never reached the log makes the resumed conversation different
// from the one that actually ran. The model is then asked to continue reasoning
// it never did.
//
// That failure is silent by construction. Nothing breaks at write time; the
// divergence only shows up much later, in a resumed run, as a model that has
// inexplicably forgotten a correction it was given. It is also easy to
// introduce: every feature that injects a synthetic message — steering,
// follow-ups, goal nudges, verify-on-stop nudges, repeat-tool reminders,
// background-job completion notices — has to remember to persist it, and the
// compiler cannot help.
//
// So the loop can be asked to check it: every message in the live history must
// be accounted for by something durable. Deliberate, deterministic history
// rewrites (compaction, which is bracketed in the log) rebase the tracker
// rather than trip it — those ARE reconstructable from the log, just not
// byte-identically.
//
// Adopted from deepseek-harness, which asserts the same property at runtime
// ("Model-visible means logged", docs/architecture.md).

// LogInvariant reports every divergence between the live history and the
// durable log. It never alters the run — it is a detector, not a guard, which
// is why it is safe to leave on in production: it can only produce a report the
// consumer routes to its own error sink.
//
// It asks two questions per turn, in increasing strength. Membership: did every
// message the model saw reach the log? Reconstruction: does folding that log
// rebuild the same conversation? The second subsumes nothing — a log can hold
// every message and still fold into a different conversation — and it is the
// one that matters, because resume folds.
//
// Cost is a full fold plus a fingerprint per message, per request: ~70µs at 50
// entries, ~0.8ms at 250, ~8ms at 1000 (BenchmarkRoundTrip). That is linear in
// a run's own length and therefore quadratic across it, but it is measured
// against provider calls that take seconds, so the plugin still costs a
// rounding error of a turn. Reach for the numbers before enabling it on a
// process running many thousand-entry sessions at once.
//
// It only makes sense on a DURABLE run; on an in-memory one there is no log to
// diverge from, so the plugin declines the run rather than reporting every
// message as unlogged.
type LogInvariant struct {
	// Report receives each violation. Required — a plugin with nowhere to send
	// a finding would spend the work and throw the answer away.
	Report func(LogInvariantViolation)
}

// Name identifies the plugin and the extension it installs.
func (LogInvariant) Name() string { return "log_invariant" }

// Register adds the plugin as a run extension.
func (p LogInvariant) Register(r *agentcore.Registry) error {
	if p.Report == nil {
		return nil
	}
	r.AddExtension(p)
	return nil
}

// BeginRun starts a tracker for a durable run, and declines a run with no log.
func (p LogInvariant) BeginRun(_ context.Context, info agentcore.RunInfo) (agentcore.Extension, error) {
	if p.Report == nil || !info.Durable {
		return nil, nil
	}
	return &logInvariant{counts: map[string]int{}, report: p.Report}, nil
}

// LogInvariantViolation describes one divergence between the live history and
// the durable log.
type LogInvariantViolation struct {
	// Turn is the run turn at which the check ran.
	Turn int
	// Role and Excerpt identify the unlogged message.
	Role    agentcore.Role
	Excerpt string
	// Detail is a human-readable explanation.
	Detail string
}

func (v LogInvariantViolation) Error() string {
	return fmt.Sprintf("agentcore: model-visible message was never logged (turn %d, role %s): %s", v.Turn, v.Role, v.Detail)
}

// logInvariant holds two checks of increasing strength over the same run.
//
// counts is the membership check: a multiset of message fingerprints, where a
// message may legitimately appear twice (the same tool result, the same nudge)
// and both occurrences must be logged.
//
// entries is the reconstruction check. Membership is necessary but not
// sufficient — a log can contain every message the model saw and still rebuild
// a DIFFERENT conversation from them, because resume does not read the log as a
// bag of messages: it folds it. Compaction is the case that proves the gap. Its
// bracket used to record only that a compaction happened, so every message was
// present and correctly counted while the fold replayed the entire span the
// summary had already replaced. The membership check was structurally blind to
// it. Keeping the observed entries lets the plugin run the real fold and
// compare, which is the question that actually matters: not "was it written?"
// but "does what was written rebuild what ran?".
//
// entries is pruned at every checkpoint, which is what keeps the check
// affordable on a long run — see prune.
type logInvariant struct {
	counts  map[string]int
	entries []agentcore.SessionEntry
	// branched records that the log is no longer a single chain. The fold walks
	// the ACTIVE branch, so once a leaf move exists the entries before a
	// checkpoint may still be reachable and dropping them would change the
	// answer. Pruning stops for the rest of the run — correctness first, and a
	// branched run pays the old cost.
	branched bool
	report   func(LogInvariantViolation)
}

// Name identifies the extension in composition diagnostics.
func (*logInvariant) Name() string { return "log_invariant" }

// ObserveLogged records that a message reached durable storage. The loop calls
// this for EVERY entry it appends, which is what makes the check trustworthy:
// the plugin cannot be told about a write that did not happen, and cannot miss
// one that did.
func (li *logInvariant) ObserveLogged(e agentcore.SessionEntry) {
	// Every entry is kept, not just the message-bearing ones: the fold is driven
	// by compaction brackets, leaf moves, and branch summaries as much as by
	// messages, and dropping them would make the reconstruction check disagree
	// with resume for reasons that are the plugin's fault rather than the log's.
	li.entries = append(li.entries, e)
	if e.Kind == agentcore.EntryMessage && e.Message != nil {
		li.note(*e.Message)
	}
	li.prune(e)
}

// prune drops the entries a fold would never read again.
//
// The reconstruction check re-runs the real fold on every request, so its cost
// is the length of the entry list — and that list was the whole run. On a
// 4,200-turn run that is 10,600 entries folded 4,200 times: quadratic, ~40s of
// CPU spent proving the same prefix over and over, on a check that is supposed
// to be cheap enough to leave on.
//
// It does not have to be. A completed compaction that carries a retained
// transcript is a point the fold RESTARTS from: everything older is what the
// summary now stands for, and ReduceSession discards it the moment it arrives
// there. So the entries before the newest such checkpoint cannot affect the
// result, and keeping them buys nothing but time and memory. Dropping them makes
// the check O(window) — the same fold, over the only entries that can change its
// answer.
//
// This is the same property the windowed resume rests on, applied to the same
// fold: if pruning here could change the answer, a windowed resume would be
// wrong too.
func (li *logInvariant) prune(e agentcore.SessionEntry) {
	if e.Kind == agentcore.EntryLeafMove {
		li.branched = true
		return
	}
	if li.branched || e.Kind != agentcore.EntryCompaction || !e.Final || e.Retained == nil {
		return
	}
	// Keep the checkpoint itself: it carries the retained transcript the fold
	// restarts from, so dropping it would drop the history rather than the
	// history's redundant prefix.
	li.entries = append(li.entries[:0:0], e)
}

// ObserveMessages is the other half: the loop reports what the model is about
// to be SHOWN, and the phase says how to treat it.
//
//   - append — a history the loop RECOVERED from the durable log and is
//     adopting wholesale. It came out of the log, so it satisfies both checks
//     by definition and must be seeded into them; without this a resumed run
//     reports its entire recovered history as unlogged on its first turn.
//   - external_input — new material; nothing to check yet.
//   - rebase — a deliberate, log-reproducible rewrite (a bracketed
//     compaction). Reset to it, or the invariant fires on exactly the
//     transform it is supposed to permit.
//   - request — the moment of truth, immediately before the provider call.
func (li *logInvariant) ObserveMessages(_ context.Context, phase agentcore.ObservePhase, turn int, msgs []agentcore.Message) {
	switch phase {
	case agentcore.PhaseAppend:
		li.seedRecovered(msgs)
	case agentcore.PhaseRebase:
		li.rebase(msgs)
	case agentcore.PhaseRequest:
		li.check(turn, msgs)
		li.checkRoundTrip(turn, msgs)
	}
}

// seedRecovered adopts a history the loop rebuilt from the durable log.
//
// The messages are counted, and they are also replayed into the entry list as
// the message entries they were before recovery reduced them back to messages.
// The plugin never read the store, so this synthetic prefix is how the
// reconstruction check gets the run's inherited past; the entries the loop
// appends from here chain onto it exactly as they chain onto the real log.
func (li *logInvariant) seedRecovered(msgs []agentcore.Message) {
	li.noteAll(msgs)
	for i := range msgs {
		m := msgs[i]
		li.entries = append(li.entries, agentcore.SessionEntry{
			Kind: agentcore.EntryMessage, Message: &m,
		})
	}
}

// note records that a message reached durable storage (or was recovered from
// it, which is the same thing for this invariant).
func (li *logInvariant) note(m agentcore.Message) {
	li.counts[messageFingerprint(m)]++
}

// noteAll records a recovered or seeded history.
func (li *logInvariant) noteAll(msgs []agentcore.Message) {
	for _, m := range msgs {
		li.note(m)
	}
}

// rebase resets the tracker to a history the loop deliberately rewrote in a way
// the log can reproduce — a compaction whose bracket IS in the log. Without
// this the invariant would fire on exactly the transform it is supposed to
// permit.
func (li *logInvariant) rebase(msgs []agentcore.Message) {
	li.counts = make(map[string]int, len(msgs))
	li.noteAll(msgs)
}

// check verifies every message in the live history has a durable counterpart,
// reporting the first that does not. It reports at most one violation per call
// so a systemic divergence does not flood the consumer's error sink.
func (li *logInvariant) check(turn int, msgs []agentcore.Message) {
	seen := make(map[string]int, len(msgs))
	for i, m := range msgs {
		// The leading system prompt is DERIVED, not logged: the loop rebuilds it
		// from the definition, recalled memory, and skills on every turn, and
		// rewrites it in place when it changes. A resumed run re-derives the same
		// prompt, so its absence from the log is not a divergence — it is the one
		// message the log deliberately does not carry.
		if i == 0 && m.Role == agentcore.RoleSystem {
			continue
		}
		fp := messageFingerprint(m)
		seen[fp]++
		if seen[fp] > li.counts[fp] {
			li.report(LogInvariantViolation{
				Turn:    turn,
				Role:    m.Role,
				Excerpt: agentcore.TruncateBytes(m.Content, 200),
				Detail: fmt.Sprintf("history contains %d copies of this %s message but the durable log records %d; "+
					"a resumed run would replay a different conversation", seen[fp], m.Role, li.counts[fp]),
			})
			return
		}
	}
}

// checkRoundTrip is the stronger half: it runs the SAME fold a resumed run
// would run and compares the result against the history the model is about to
// be shown. Membership asks whether each message reached the log;
// reconstruction asks whether the log rebuilds the conversation — and a log can
// pass the first while failing the second, which is how an empty compaction
// bracket hid a full-span replay behind a green invariant.
//
// It reports the first divergence and stops, so one structural fault does not
// produce a violation per message for the rest of the run.
func (li *logInvariant) checkRoundTrip(turn int, msgs []agentcore.Message) {
	live := msgs
	// Same exemption the membership check makes: the leading system prompt is
	// derived per run, never logged, and re-derived identically on resume.
	if len(live) > 0 && live[0].Role == agentcore.RoleSystem {
		live = live[1:]
	}
	rebuilt := agentcore.ReduceSession(li.entries).Messages

	n := min(len(live), len(rebuilt))
	for i := range n {
		if messageFingerprint(live[i]) == messageFingerprint(rebuilt[i]) {
			continue
		}
		li.report(LogInvariantViolation{
			Turn:    turn,
			Role:    live[i].Role,
			Excerpt: agentcore.TruncateBytes(live[i].Content, 200),
			Detail: fmt.Sprintf("the durable log does not rebuild this conversation: at position %d the live history "+
				"holds a %s message but folding the log yields a %s message; a resumed run would continue from the latter",
				i, live[i].Role, rebuilt[i].Role),
		})
		return
	}
	if len(live) != len(rebuilt) {
		li.report(LogInvariantViolation{
			Turn:    turn,
			Role:    roleAt(live, n),
			Excerpt: excerptAt(live, rebuilt, n),
			Detail: fmt.Sprintf("the durable log does not rebuild this conversation: the live history holds %d messages "+
				"but folding the log yields %d; a resumed run would continue from a conversation of the wrong length",
				len(live), len(rebuilt)),
		})
	}
}

// roleAt names the role at the first position the two histories stopped
// agreeing about, for a violation whose fault is a length difference.
func roleAt(msgs []agentcore.Message, i int) agentcore.Role {
	if i < len(msgs) {
		return msgs[i].Role
	}
	return ""
}

// excerptAt quotes whichever history still has a message at the divergence
// point — the extra one when the log is longer, the missing one when it is
// shorter — since that is the message a reader needs to see.
func excerptAt(live, rebuilt []agentcore.Message, i int) string {
	if i < len(live) {
		return agentcore.TruncateBytes(live[i].Content, 200)
	}
	if i < len(rebuilt) {
		return agentcore.TruncateBytes(rebuilt[i].Content, 200)
	}
	return ""
}

// messageFingerprint identifies a message for the invariant. Role, tool name,
// and tool-call id are part of the identity: two tool results with identical
// text but different call ids are different messages, and conflating them would
// let a genuinely unlogged one hide behind a logged twin.
func messageFingerprint(m agentcore.Message) string {
	sum := sha256.Sum256([]byte(m.Content))
	return string(m.Role) + "\x00" + m.Name + "\x00" + m.ToolCallID + "\x00" + hex.EncodeToString(sum[:8])
}
