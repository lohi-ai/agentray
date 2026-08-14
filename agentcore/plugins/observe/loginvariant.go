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
// is precisely why it is safe to leave on in production: a hash per message per
// turn, and a report the consumer routes to its own error sink.
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

// logInvariant tracks which messages have a durable counterpart. It is a
// multiset of message fingerprints: a message may legitimately appear twice
// (the same tool result, the same nudge), and both occurrences must be logged.
type logInvariant struct {
	counts map[string]int
	report func(LogInvariantViolation)
}

// Name identifies the extension in composition diagnostics.
func (*logInvariant) Name() string { return "log_invariant" }

// ObserveLogged records that a message reached durable storage. The loop calls
// this for EVERY entry it appends, which is what makes the check trustworthy:
// the plugin cannot be told about a write that did not happen, and cannot miss
// one that did.
func (li *logInvariant) ObserveLogged(e agentcore.SessionEntry) {
	if e.Kind == agentcore.EntryMessage && e.Message != nil {
		li.note(*e.Message)
	}
}

// ObserveMessages is the other half: the loop reports what the model is about
// to be SHOWN, and the phase says how to treat it.
//
//   - append / external_input — new material; nothing to check yet.
//   - rebase — a deliberate, log-reproducible rewrite (a bracketed
//     compaction). Reset to it, or the invariant fires on exactly the
//     transform it is supposed to permit.
//   - request — the moment of truth, immediately before the provider call.
func (li *logInvariant) ObserveMessages(_ context.Context, phase agentcore.ObservePhase, turn int, msgs []agentcore.Message) {
	switch phase {
	case agentcore.PhaseRebase:
		li.rebase(msgs)
	case agentcore.PhaseRequest:
		li.check(turn, msgs)
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

// messageFingerprint identifies a message for the invariant. Role, tool name,
// and tool-call id are part of the identity: two tool results with identical
// text but different call ids are different messages, and conflating them would
// let a genuinely unlogged one hide behind a logged twin.
func messageFingerprint(m agentcore.Message) string {
	sum := sha256.Sum256([]byte(m.Content))
	return string(m.Role) + "\x00" + m.Name + "\x00" + m.ToolCallID + "\x00" + hex.EncodeToString(sum[:8])
}
