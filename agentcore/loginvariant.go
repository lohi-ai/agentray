package agentcore

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
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
// rewrites (the context editor's supersession prune, and compaction, which is
// bracketed in the log) rebase the tracker rather than trip it — those ARE
// reconstructable from the log, just not byte-identically.
//
// Adopted from deepseek-harness, which asserts the same property at runtime
// ("Model-visible means logged", docs/architecture.md).

// LogInvariantViolation describes one divergence between the live history and
// the durable log.
type LogInvariantViolation struct {
	// Turn is the run turn at which the check ran.
	Turn int
	// Role and Excerpt identify the unlogged message.
	Role    Role
	Excerpt string
	// Detail is a human-readable explanation.
	Detail string
}

func (v LogInvariantViolation) Error() string {
	return fmt.Sprintf("agentcore: model-visible message was never logged (turn %d, role %s): %s", v.Turn, v.Role, v.Detail)
}

// logInvariant tracks which messages have a durable counterpart. It is a
// multiset of message fingerprints: a message may legitimately appear twice
// (the same tool result, the same nudge), and both occurrences must be logged.
type logInvariant struct {
	counts map[string]int
	report func(LogInvariantViolation)
}

// newLogInvariant returns a tracker, or nil when the check is off. A nil
// tracker's methods are no-ops, so the loop never branches on it.
func newLogInvariant(enabled bool, report func(LogInvariantViolation)) *logInvariant {
	if !enabled || report == nil {
		return nil
	}
	return &logInvariant{counts: map[string]int{}, report: report}
}

// note records that a message reached durable storage (or was recovered from
// it, which is the same thing for this invariant).
func (li *logInvariant) note(m Message) {
	if li == nil {
		return
	}
	li.counts[messageFingerprint(m)]++
}

// noteAll records a recovered or seeded history.
func (li *logInvariant) noteAll(msgs []Message) {
	if li == nil {
		return
	}
	for _, m := range msgs {
		li.note(m)
	}
}

// rebase resets the tracker to a history the loop deliberately rewrote in a way
// the log can reproduce — the context editor's deterministic prune, or a
// compaction whose bracket IS in the log. Without this the invariant would fire
// on exactly the two transforms it is supposed to permit.
func (li *logInvariant) rebase(msgs []Message) {
	if li == nil {
		return
	}
	li.counts = make(map[string]int, len(msgs))
	li.noteAll(msgs)
}

// check verifies every message in the live history has a durable counterpart,
// reporting the first that does not. It reports at most one violation per call
// so a systemic divergence does not flood the consumer's error sink.
func (li *logInvariant) check(turn int, msgs []Message) {
	if li == nil {
		return
	}
	seen := make(map[string]int, len(msgs))
	for _, m := range msgs {
		fp := messageFingerprint(m)
		seen[fp]++
		if seen[fp] > li.counts[fp] {
			li.report(LogInvariantViolation{
				Turn:    turn,
				Role:    m.Role,
				Excerpt: truncateBytes(m.Content, 200),
				Detail: fmt.Sprintf("history contains %d copies of this %s message but the durable log records %d; "+
					"a resumed run would replay a different conversation", seen[fp], m.Role, li.counts[fp]),
			})
			return
		}
	}
}

// messageFingerprint identifies a message for the invariant. Role, tool name,
// and tool-call id are part of the identity: two tool results with identical
// text but different call ids are different messages, and conflating them would
// let a genuinely unlogged one hide behind a logged twin.
func messageFingerprint(m Message) string {
	sum := sha256.Sum256([]byte(m.Content))
	return string(m.Role) + "\x00" + m.Name + "\x00" + m.ToolCallID + "\x00" + hex.EncodeToString(sum[:8])
}
