package observe

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/lohi-ai/agentray/agentcore"
)

// The reconstruction half of the invariant.
//
// Membership — "every message the model saw reached the log" — is necessary but
// not sufficient, because resume does not read the log as a bag of messages: it
// FOLDS it. A log can hold every message and still fold into a different
// conversation, and when it does, the resumed run continues from that different
// conversation with nothing to say so. These tests hold the fold itself.

// logEntry is a message entry, the shape the loop buffers for every message.
func logEntry(role agentcore.Role, content string) agentcore.SessionEntry {
	return agentcore.SessionEntry{
		Kind:    agentcore.EntryMessage,
		Message: &agentcore.Message{Role: role, Content: content},
	}
}

// TestRoundTrip_CatchesACompactionTheLogCannotRebuild is the case that motivated
// this check, reproduced structurally: every message is in the log and correctly
// counted, but the fold no longer yields them because a compaction replaced
// them. Membership sees nothing wrong — which is exactly how an empty compaction
// bracket once hid a full-span replay behind a green invariant.
func TestRoundTrip_CatchesACompactionTheLogCannotRebuild(t *testing.T) {
	var violations []LogInvariantViolation
	li := newTracker(func(v LogInvariantViolation) { violations = append(violations, v) })

	// The log: two messages, then a compaction that replaced them with a summary.
	li.ObserveLogged(logEntry(agentcore.RoleUser, "task"))
	li.ObserveLogged(logEntry(agentcore.RoleAssistant, "work"))
	li.ObserveLogged(agentcore.SessionEntry{Kind: agentcore.EntryCompaction})
	li.ObserveLogged(agentcore.SessionEntry{
		Kind: agentcore.EntryCompaction, Final: true,
		Retained: []agentcore.Message{{Role: agentcore.RoleSystem, Content: "summary of the work"}},
	})

	// The live history the model is shown still holds the original messages —
	// the loop never adopted the compaction. Both messages ARE logged, so the
	// membership check is satisfied.
	live := []agentcore.Message{
		{Role: agentcore.RoleUser, Content: "task"},
		{Role: agentcore.RoleAssistant, Content: "work"},
	}
	li.check(1, live)
	if len(violations) != 0 {
		t.Fatalf("membership should pass here — that is the point: %+v", violations)
	}

	// Driven through ObserveMessages rather than by calling checkRoundTrip
	// directly, so this also pins the WIRING: a reconstruction check that is
	// never reached from the request phase is dead code, and a unit test that
	// calls it by hand would not notice.
	li.ObserveMessages(context.Background(), agentcore.PhaseRequest, 1, live)
	if len(violations) != 1 {
		t.Fatalf("reconstruction must catch what membership cannot, got %d violations", len(violations))
	}
	if !strings.Contains(violations[0].Detail, "does not rebuild this conversation") {
		t.Fatalf("violation should name the reconstruction failure: %q", violations[0].Detail)
	}
}

// TestRoundTrip_CatchesReorderedHistory pins the other way membership is blind:
// the same messages in a different order are the same multiset but not the same
// conversation, and a model resumed onto the wrong order reasons about a
// sequence of events that never happened.
func TestRoundTrip_CatchesReorderedHistory(t *testing.T) {
	var violations []LogInvariantViolation
	li := newTracker(func(v LogInvariantViolation) { violations = append(violations, v) })

	li.ObserveLogged(logEntry(agentcore.RoleUser, "first"))
	li.ObserveLogged(logEntry(agentcore.RoleAssistant, "second"))

	live := []agentcore.Message{
		{Role: agentcore.RoleAssistant, Content: "second"},
		{Role: agentcore.RoleUser, Content: "first"},
	}
	li.check(1, live)
	if len(violations) != 0 {
		t.Fatalf("membership is order-blind by design: %+v", violations)
	}
	li.checkRoundTrip(1, live)
	if len(violations) != 1 {
		t.Fatalf("reconstruction must catch reordering, got %d", len(violations))
	}
}

// TestRoundTrip_CatchesALogThatRebuildsTooMuch covers the direction the
// compaction bug actually took: the fold yields MORE than the model was shown,
// so the resumed run inherits history the live run had already shrunk away.
func TestRoundTrip_CatchesALogThatRebuildsTooMuch(t *testing.T) {
	var violations []LogInvariantViolation
	li := newTracker(func(v LogInvariantViolation) { violations = append(violations, v) })

	li.ObserveLogged(logEntry(agentcore.RoleUser, "task"))
	li.ObserveLogged(logEntry(agentcore.RoleAssistant, "work"))
	li.ObserveLogged(logEntry(agentcore.RoleAssistant, "more work"))

	live := []agentcore.Message{
		{Role: agentcore.RoleUser, Content: "task"},
		{Role: agentcore.RoleAssistant, Content: "work"},
	}
	li.checkRoundTrip(1, live)
	if len(violations) != 1 {
		t.Fatalf("expected one violation, got %d", len(violations))
	}
	if !strings.Contains(violations[0].Detail, "wrong length") {
		t.Fatalf("violation should name the length divergence: %q", violations[0].Detail)
	}
}

// TestRoundTrip_QuietOnAFaithfulLog is the negative control: a log that folds
// back to exactly the live history reports nothing, including across a
// compaction that the log CAN rebuild. Without this, a check that reported
// unconditionally would also pass the tests above.
func TestRoundTrip_QuietOnAFaithfulLog(t *testing.T) {
	var violations []LogInvariantViolation
	li := newTracker(func(v LogInvariantViolation) { violations = append(violations, v) })

	li.ObserveLogged(logEntry(agentcore.RoleUser, "task"))
	li.ObserveLogged(logEntry(agentcore.RoleAssistant, "work"))
	li.ObserveLogged(agentcore.SessionEntry{Kind: agentcore.EntryCompaction})
	li.ObserveLogged(agentcore.SessionEntry{
		Kind: agentcore.EntryCompaction, Final: true,
		Retained: []agentcore.Message{{Role: agentcore.RoleSystem, Content: "summary"}},
	})
	li.ObserveLogged(logEntry(agentcore.RoleAssistant, "after compaction"))

	// The live history the loop actually holds after that compaction, with the
	// derived system prompt in front (never logged, exempt from both checks).
	live := []agentcore.Message{
		{Role: agentcore.RoleSystem, Content: "You are a helpful agent."},
		{Role: agentcore.RoleSystem, Content: "summary"},
		{Role: agentcore.RoleAssistant, Content: "after compaction"},
	}
	li.checkRoundTrip(1, live)
	if len(violations) != 0 {
		t.Fatalf("a faithful log must stay quiet: %+v", violations)
	}
}

// --- the resume regression --------------------------------------------------

type roundTripStore struct {
	mu  sync.Mutex
	log map[string][]agentcore.SessionEntry
}

func newRoundTripStore() *roundTripStore {
	return &roundTripStore{log: map[string][]agentcore.SessionEntry{}}
}

func (s *roundTripStore) Append(_ context.Context, id string, e agentcore.SessionEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	e.Seq = len(s.log[id])
	s.log[id] = append(s.log[id], e)
	return nil
}

func (s *roundTripStore) Log(_ context.Context, id string) ([]agentcore.SessionEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]agentcore.SessionEntry, len(s.log[id]))
	copy(out, s.log[id])
	return out, nil
}

type roundTripConfig struct{ cfg agentcore.Config }

func (roundTripConfig) Name() string { return "config" }

func (c roundTripConfig) Register(r *agentcore.Registry) error { return r.ApplyConfig(c.cfg) }

// TestLogInvariant_ResumedRunReportsNothing is a regression on a false positive
// the plugin shipped with: the loop hands a recovered history over as
// PhaseAppend precisely so the tracker can adopt it ("seed the tracker with it
// or every resumed run would report its whole history as unlogged"), and the
// plugin ignored that phase. Every resumed run therefore accused itself of
// losing the history it had just correctly recovered — noise on exactly the
// path the invariant exists to protect, which is also the path most likely to
// train an operator to ignore it.
func TestLogInvariant_ResumedRunReportsNothing(t *testing.T) {
	ctx := context.Background()
	store := newRoundTripStore()

	// An interrupted log: a prompt and one assistant turn, no leaf.
	for _, e := range []agentcore.SessionEntry{
		{Kind: agentcore.EntryMessage, ID: "a",
			Message: &agentcore.Message{Role: agentcore.RoleUser, Content: "do the thing"}},
		{Kind: agentcore.EntryMessage, ID: "b", ParentID: "a", Turn: 1,
			Message: &agentcore.Message{Role: agentcore.RoleAssistant, Content: "partial work"}},
	} {
		if err := store.Append(ctx, "s", e); err != nil {
			t.Fatal(err)
		}
	}

	var mu sync.Mutex
	var violations []LogInvariantViolation

	agent, err := agentcore.Build(
		roundTripConfig{cfg: agentcore.Config{
			Provider:      agentcore.NewFauxProvider(agentcore.AssistantText("finished")),
			Model:         "m",
			Session:       store,
			SessionID:     "s",
			ResumeSession: true,
		}},
		LogInvariant{Report: func(v LogInvariantViolation) {
			mu.Lock()
			violations = append(violations, v)
			mu.Unlock()
		}},
	)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if _, err := agent.Prompt(ctx, "do the thing"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(violations) != 0 {
		t.Fatalf("a resumed run must not accuse itself; got %d violation(s), first: %v",
			len(violations), violations[0])
	}
}

// BenchmarkRoundTrip measures what a turn pays for the reconstruction check.
// The plugin's own doc quotes these numbers to justify leaving it on, so they
// are worth being able to re-derive rather than trust.
func BenchmarkRoundTrip(b *testing.B) {
	for _, n := range []int{50, 250, 1000} {
		b.Run(fmt.Sprintf("entries=%d", n), func(b *testing.B) {
			li := newTracker(func(LogInvariantViolation) {})
			for i := range n {
				li.ObserveLogged(logEntry(agentcore.RoleAssistant,
					fmt.Sprintf("message %d with some realistic body text", i)))
			}
			live := agentcore.ReduceSession(li.entries).Messages
			ctx := context.Background()
			b.ResetTimer()
			for range b.N {
				li.ObserveMessages(ctx, agentcore.PhaseRequest, 1, live)
			}
		})
	}
}
