package agentcore

import (
	"context"
	"strings"
	"testing"
)

// Stream-interceptor seam: the abort sentinel, the mid-turn retry, and the
// backstop that keeps a broken interceptor from spinning the provider forever.
// These tests pin the core contract a stream-rule plugin rides on.

// abortAll is a StreamInterceptor that aborts on every check — the broken
// interceptor the loop's backstop exists for.
type abortAll struct {
	calls int
}

func (a *abortAll) Name() string { return "abort_all" }
func (a *abortAll) InterceptStreamDelta(_ context.Context, _ string) StreamDecision {
	a.calls++
	return StreamDecision{Abort: true, Inject: []Message{{Role: RoleUser, Content: "stop that"}}}
}

type abortAllFactory struct{ inner *abortAll }

func (f abortAllFactory) Name() string { return "abort_all" }
func (f abortAllFactory) BeginRun(context.Context, RunInfo) (Extension, error) {
	return f.inner, nil
}

// matchOnce aborts the first time the accumulated text contains needle, then
// goes quiet — the well-behaved interceptor the retry path exists for.
type matchOnce struct {
	needle string
	fired  bool
}

func (m *matchOnce) Name() string { return "match_once" }
func (m *matchOnce) InterceptStreamDelta(_ context.Context, accumulated string) StreamDecision {
	if m.fired || !strings.Contains(accumulated, m.needle) {
		return StreamDecision{}
	}
	m.fired = true
	return StreamDecision{Abort: true, Inject: []Message{{Role: RoleUser, Content: "RULE: never say " + m.needle}}}
}

type matchOnceFactory struct{ inner *matchOnce }

func (f matchOnceFactory) Name() string { return "match_once" }
func (f matchOnceFactory) BeginRun(context.Context, RunInfo) (Extension, error) {
	return f.inner, nil
}

// A matching rule aborts the stream mid-token, the partial text is discarded,
// the injection is appended and persisted, and the turn retries in place.
func TestStreamRuleAbortsInjectsAndRetries(t *testing.T) {
	faux := NewFauxProvider(
		AssistantText("the forbidden word is banana"),
		AssistantText("a clean answer"),
	)
	ic := &matchOnce{needle: "banana"}
	store := newMemSessionStore()
	agent, err := New(Config{
		Provider:   faux,
		Model:      "test",
		Session:    store,
		SessionID:  "s-streamrule",
		Extensions: []ExtensionFactory{matchOnceFactory{inner: ic}},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	var visible string
	res, err := agent.PromptStream(context.Background(), "say something", func(ev StreamEvent) {
		if ev.Type == StreamToken {
			visible += ev.Token
		}
	})
	if err != nil {
		t.Fatalf("PromptStream: %v", err)
	}
	if res.Final != "a clean answer" {
		t.Fatalf("final = %q, want the retried answer", res.Final)
	}
	if visible != "a clean answer" {
		t.Fatalf("visible output = %q, want only the accepted retry", visible)
	}
	if res.Turns != 1 {
		t.Fatalf("turns = %d, want 1 — a stream retry is the same turn, not a new one", res.Turns)
	}
	// The provider saw two calls: the aborted attempt and the retry.
	if len(faux.Recorded) != 2 {
		t.Fatalf("provider calls = %d, want 2", len(faux.Recorded))
	}
	// The retry's request must carry the injection — that is the whole point of
	// the abort.
	last := faux.Recorded[1].Messages[len(faux.Recorded[1].Messages)-1]
	if !strings.Contains(last.Content, "RULE: never say banana") {
		t.Fatalf("retry request missing injection; last message = %q", last.Content)
	}
	// The aborted partial text must not be in the transcript.
	for _, m := range res.Messages {
		if strings.Contains(m.Content, "banana") && m.Role == RoleAssistant {
			t.Fatalf("aborted partial answer survived in history: %q", m.Content)
		}
	}
	// The injection is durable: the log must carry it so a resume replays the
	// conversation the retry was actually given.
	log, _ := store.Log(context.Background(), "s-streamrule")
	var logged bool
	for _, e := range log {
		if e.Kind == EntryMessage && e.Message != nil && strings.Contains(e.Message.Content, "RULE: never say banana") {
			logged = true
		}
	}
	if !logged {
		t.Fatalf("injection missing from durable log (%d entries)", len(log))
	}
	rs := ReduceSession(log)
	var inHistory bool
	for _, m := range rs.Messages {
		if strings.Contains(m.Content, "RULE: never say banana") {
			inHistory = true
		}
	}
	if !inHistory {
		t.Fatalf("injection missing from reduced resume history")
	}
}

// No match: the stream completes untouched and the interceptor costs one cheap
// check per delta — no abort, no retry, no injection.
func TestStreamRuleNoMatchPassesThrough(t *testing.T) {
	faux := NewFauxProvider(AssistantText("a clean answer"))
	ic := &matchOnce{needle: "banana"}
	agent, err := New(Config{
		Provider:   faux,
		Model:      "test",
		Extensions: []ExtensionFactory{matchOnceFactory{inner: ic}},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	res, err := agent.PromptStream(context.Background(), "say something", func(StreamEvent) {})
	if err != nil {
		t.Fatalf("PromptStream: %v", err)
	}
	if res.Final != "a clean answer" {
		t.Fatalf("final = %q", res.Final)
	}
	if len(faux.Recorded) != 1 {
		t.Fatalf("provider calls = %d, want 1 — a non-matching rule must not retry", len(faux.Recorded))
	}
}

// An interceptor that never stops matching hits the loop's backstop: after
// maxStreamAbortsPerTurn the turn is retried once with interception disabled
// and the stream completes unmodified.
func TestStreamAbortBackstopPassesThrough(t *testing.T) {
	faux := NewFauxProvider() // script exhausted: every call returns "(end)"
	ic := &abortAll{}
	agent, err := New(Config{
		Provider:   faux,
		Model:      "test",
		Extensions: []ExtensionFactory{abortAllFactory{inner: ic}},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	res, err := agent.PromptStream(context.Background(), "go", func(StreamEvent) {})
	if err != nil {
		t.Fatalf("PromptStream: %v", err)
	}
	if res.Final != "(end)" {
		t.Fatalf("final = %q, want the unmodified completion", res.Final)
	}
	// maxStreamAbortsPerTurn aborted attempts + one interception-free retry.
	if got, want := len(faux.Recorded), maxStreamAbortsPerTurn+1; got != want {
		t.Fatalf("provider calls = %d, want %d (backstop must cap the retries)", got, want)
	}
	// Every aborted attempt's injection is still persisted — the log records
	// what the run actually did.
	if ic.calls == 0 {
		t.Fatal("interceptor was never consulted")
	}
}

// Non-streaming fallback: with no sink the interceptor sees the completed
// response once, discards it, and retries with the injection — the half of the
// contract that changes what the model does.
func TestStreamRuleNonStreamingFallback(t *testing.T) {
	faux := NewFauxProvider(
		AssistantText("the forbidden word is banana"),
		AssistantText("a clean answer"),
	)
	ic := &matchOnce{needle: "banana"}
	agent, err := New(Config{
		Provider:   faux,
		Model:      "test",
		Extensions: []ExtensionFactory{matchOnceFactory{inner: ic}},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	res, err := agent.Prompt(context.Background(), "say something")
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if res.Final != "a clean answer" {
		t.Fatalf("final = %q, want the retried answer", res.Final)
	}
	if len(faux.Recorded) != 2 {
		t.Fatalf("provider calls = %d, want 2", len(faux.Recorded))
	}
	last := faux.Recorded[1].Messages[len(faux.Recorded[1].Messages)-1]
	if !strings.Contains(last.Content, "RULE: never say banana") {
		t.Fatalf("retry request missing injection; last message = %q", last.Content)
	}
}

// An abort with nothing to inject is ignored: retrying against an unchanged
// conversation would re-emit the same tokens and abort again.
func TestStreamAbortWithoutInjectionIsIgnored(t *testing.T) {
	faux := NewFauxProvider(AssistantText("banana"))
	bare := &abortAllBare{}
	agent, err := New(Config{
		Provider:   faux,
		Model:      "test",
		Extensions: []ExtensionFactory{abortAllBareFactory{inner: bare}},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	res, err := agent.PromptStream(context.Background(), "go", func(StreamEvent) {})
	if err != nil {
		t.Fatalf("PromptStream: %v", err)
	}
	if res.Final != "banana" {
		t.Fatalf("final = %q — an injection-less abort must not retry", res.Final)
	}
	if len(faux.Recorded) != 1 {
		t.Fatalf("provider calls = %d, want 1", len(faux.Recorded))
	}
}

type abortAllBare struct{}

func (a *abortAllBare) Name() string { return "abort_bare" }
func (a *abortAllBare) InterceptStreamDelta(_ context.Context, _ string) StreamDecision {
	return StreamDecision{Abort: true} // no Inject: treated as no opinion
}

type abortAllBareFactory struct{ inner *abortAllBare }

func (f abortAllBareFactory) Name() string { return "abort_bare" }
func (f abortAllBareFactory) BeginRun(context.Context, RunInfo) (Extension, error) {
	return f.inner, nil
}
