package agentcore

import (
	"context"
	"errors"
	"testing"
)

// errProvider always fails, to drive the failure-synthesis path.
type errProvider struct{}

func (errProvider) Name() string { return "err" }
func (errProvider) Chat(context.Context, ChatRequest) (ChatResponse, error) {
	return ChatResponse{}, errors.New("provider exploded")
}
func (errProvider) Stream(context.Context, ChatRequest) (<-chan ChatDelta, error) {
	return nil, errors.New("provider exploded")
}
func (errProvider) SupportsTools() bool { return true }

// TestBusyGuardRejectsConcurrentRun verifies one Agent instance runs one run at
// a time: a reentrant Prompt on the *same* agent fails fast with ErrBusy instead
// of racing on the shared run state.
func TestBusyGuardRejectsConcurrentRun(t *testing.T) {
	var agent *Agent
	var reentryErr error
	reenter := func(ctx context.Context, _ Message) {
		_, reentryErr = agent.Prompt(ctx, "again")
	}
	var err error
	agent, err = New(Config{
		Provider: NewFauxProvider(AssistantText("done")),
		Model:    "test",
		Hooks:    Hooks{MessageEnd: []MessageEndHook{reenter}},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := agent.Prompt(context.Background(), "go"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if !errors.Is(reentryErr, ErrBusy) {
		t.Fatalf("a reentrant run on the same agent must return ErrBusy, got %v", reentryErr)
	}
}

// TestFailureMessageSynthesizedOnProviderError verifies an aborting run still
// produces a clean lifecycle: a synthesized assistant failure message plus
// message_end / turn_end / agent_end events, while the error reaches the caller.
func TestFailureMessageSynthesizedOnProviderError(t *testing.T) {
	var seen []StreamEventType
	sink := func(ev StreamEvent) { seen = append(seen, ev.Type) }

	agent, err := New(Config{Provider: errProvider{}, Model: "test"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	res, err := agent.PromptStream(context.Background(), "go", sink)
	if err == nil {
		t.Fatalf("a provider error must surface to the caller")
	}
	last := res.Messages[len(res.Messages)-1]
	if last.Role != RoleAssistant || last.Error == "" {
		t.Fatalf("expected a synthesized assistant failure message, got %+v", last)
	}
	if res.StopReason != "error" {
		t.Fatalf("stop reason = %q, want error", res.StopReason)
	}
	has := func(want StreamEventType) bool {
		for _, e := range seen {
			if e == want {
				return true
			}
		}
		return false
	}
	for _, want := range []StreamEventType{StreamMessageEnd, StreamTurnEnd, StreamAgentEnd} {
		if !has(want) {
			t.Fatalf("failure lifecycle missing %q in %v", want, seen)
		}
	}
}

// TestSavePointFlushesPerTurn verifies durable writes are buffered and committed
// atomically at each turn boundary, emitting one save_point per turn, and the
// committed log still reduces to a completed run.
func TestSavePointFlushesPerTurn(t *testing.T) {
	store := newMemSessionStore()
	faux := NewFauxProvider(
		AssistantToolCall("c1", "noop", `{}`),
		AssistantText("done"),
	)
	var saves int
	sink := func(ev StreamEvent) {
		if ev.Type == StreamSavePoint {
			saves++
		}
	}
	agent, err := New(Config{
		Provider:  faux,
		Model:     "test",
		Tools:     NewToolSet(noopTool{}),
		Policy:    NewAllowList("noop"),
		Session:   store,
		SessionID: "s1",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := agent.PromptStream(context.Background(), "go", sink); err != nil {
		t.Fatalf("PromptStream: %v", err)
	}
	// One save-point per turn: the tool-call turn, then the final-answer turn.
	if saves != 2 {
		t.Fatalf("save-points = %d, want 2 (one per turn)", saves)
	}
	log, _ := store.Log(context.Background(), "s1")
	if rs := ReduceSession(log); !rs.Completed {
		t.Fatalf("committed log should reduce to a completed run: %+v", rs)
	}
}

// TestAfterProviderResponseObserved verifies the raw-boundary observer fires
// once per provider call, before usage accumulation.
func TestAfterProviderResponseObserved(t *testing.T) {
	var calls int
	hook := func(context.Context, ChatResponse) { calls++ }
	agent, err := New(Config{
		Provider: NewFauxProvider(AssistantText("done")),
		Model:    "test",
		Hooks:    Hooks{AfterProviderResponse: []ProviderResponseHook{hook}},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := agent.Prompt(context.Background(), "go"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if calls != 1 {
		t.Fatalf("after_provider_response observed %d times, want 1", calls)
	}
}
