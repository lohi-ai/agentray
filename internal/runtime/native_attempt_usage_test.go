package agentruntime

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/lohi-ai/agentray/agentcore"
	"github.com/lohi-ai/agentray/ai"
)

func usageFixture(stop string, input float64, cost float64) *ai.Message {
	return &ai.Message{Role: "assistant", Model: "fixture", API: "test", Provider: "test", Timestamp: 1, StopReason: stop, Content: ai.BlockContent(ai.ContentBlock{Type: "text", Text: "same response"}), Usage: &ai.Usage{Input: input, Output: 1, TotalTokens: input + 1, Cost: ai.UsageCost{Total: cost}}}
}
func usageStream(ctx context.Context, message *ai.Message) *ai.AssistantMessageEventStream {
	stream := ai.NewAssistantMessageEventStreamFor(ctx)
	stream.Push(ai.AssistantMessageEvent{Type: "start", Partial: message})
	if message.StopReason == "error" {
		stream.Push(ai.AssistantMessageEvent{Type: "error", Error: message})
	} else {
		stream.Push(ai.AssistantMessageEvent{Type: "done", Message: message})
	}
	stream.End()
	return stream
}

func TestNativeAttemptUsageCountsDiscardedAndIdenticalResponsesOnce(t *testing.T) {
	ladder := testNativeLadder(t)
	ladder.rungs[0].pricingKnown = false
	ladder.rungs[1].pricingKnown = true
	failed := usageFixture("error", 2, 0.2)
	success := usageFixture("stop", 3, 0.3)
	for i := range ladder.rungs {
		index := i
		ladder.rungs[i].stream = func(ctx context.Context, _ json.RawMessage, _ ai.TranscriptContext, _ map[string]any) (*ai.AssistantMessageEventStream, error) {
			if index == 0 {
				return usageStream(ctx, failed), nil
			}
			return usageStream(ctx, success), nil
		}
	}
	binding, _, stream := ladder.sessionBinding()
	projection := &piRunProjection{pricingKnown: false}
	session, err := NewPiSession(context.Background(), PiSessionConfig{Pi: binding, NativeStream: stream, nativeLadder: ladder, nativeAttemptObserved: projection.accountNativeAttempt, nativeTerminalPublished: projection.expectNativeTerminal})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	for i := 0; i < 2; i++ {
		out := ai.NewAssistantMessageEventStream()
		_, err := session.runNativeLadder(session.ctx, out, nativeLadderRun{policy: agentcore.RetryPolicy{MaxAttempts: 1}, open: func(ctx context.Context, rung nativeBoundRung, _, _ int) (*ai.AssistantMessageEventStream, error) {
			return session.openNativeAttempt(ctx, rung, ai.TranscriptContext{}, nil)
		}})
		if err != nil {
			t.Fatal(err)
		}
		message, err := out.SnapshotResult(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		// The engine owns this annotation; provider identity/content/usage remain exact.
		level := "off"
		message.ThinkingLevel = &level
		event, _ := json.Marshal(map[string]any{"type": "message_end", "message": message})
		if err = projection.event(event); err != nil {
			t.Fatal(err)
		}
	}
	usage := projection.result.Usage
	if usage.InputTokens != 8 || usage.OutputTokens != 3 || usage.CostUSD != 0.8 || !usage.CostUnpriced {
		t.Fatalf("discarded/identical responses charged incorrectly: %+v", usage)
	}
	if len(projection.nativeTerminal) != 0 {
		t.Fatal("terminal handoff not consumed")
	}
	// An ordinary assistant event still uses the existing accounting path.
	event, _ := json.Marshal(map[string]any{"type": "message_end", "message": success})
	if err = projection.event(event); err != nil {
		t.Fatal(err)
	}
	if projection.result.Usage.InputTokens != 11 {
		t.Fatal("ordinary response incorrectly deduplicated")
	}
}

func TestNativeAttemptUsageRejectsOverlapAndMismatchedTerminal(t *testing.T) {
	p := &piRunProjection{}
	message := usageFixture("stop", 3, 0.3)
	outcome := nativeAttemptOutcome{terminal: ai.AssistantMessageEvent{Type: "done", Message: message}}
	if err := p.accountNativeAttempt(nativeBoundRung{pricingKnown: true}, nativeRetryAttempt{outcome: outcome}); err != nil {
		t.Fatal(err)
	}
	if err := p.expectNativeTerminal(outcome); err != nil {
		t.Fatal(err)
	}
	if err := p.expectNativeTerminal(outcome); err == nil {
		t.Fatal("overlapping terminal handoff accepted")
	}
	changed := usageFixture("stop", 7, 0.7)
	event, _ := json.Marshal(map[string]any{"type": "message_end", "message": changed})
	if err := p.event(event); err == nil {
		t.Fatal("different usage accepted as accounted")
	}
	if p.result.Usage.InputTokens != 3 || p.result.Final != "" {
		t.Fatal("mismatch changed accounting/display")
	}
	event, _ = json.Marshal(map[string]any{"type": "message_end", "message": message})
	if err := p.event(event); err != nil {
		t.Fatal(err)
	}
	if p.result.Usage.InputTokens != 3 || p.result.Usage.CostUnpriced {
		t.Fatal("terminal charged twice or lost rung pricing")
	}
}

func TestNativeAttemptUsageThroughEngineMessageEnd(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ladder := testNativeLadder(t)
	ladder.rungs[0].pricingKnown = false
	ladder.rungs[1].pricingKnown = true
	failed, success := usageFixture("error", 2, 0.2), usageFixture("stop", 3, 0.3)
	for i := range ladder.rungs {
		index := i
		ladder.rungs[i].stream = func(ctx context.Context, _ json.RawMessage, _ ai.TranscriptContext, _ map[string]any) (*ai.AssistantMessageEventStream, error) {
			if index == 0 {
				return usageStream(ctx, failed), nil
			}
			return usageStream(ctx, success), nil
		}
	}
	binding, _, _ := ladder.sessionBinding()
	projection := &piRunProjection{pricingKnown: false}
	binding.OnEvent = func(_ context.Context, raw json.RawMessage) error { return projection.event(raw) }
	var session *PiSession
	logical := func(ctx context.Context, _ json.RawMessage, transcript ai.TranscriptContext, options map[string]any) (*ai.AssistantMessageEventStream, error) {
		out := ai.NewAssistantMessageEventStream()
		go func() {
			defer out.End()
			_, err := session.runNativeLadder(ctx, out, nativeLadderRun{policy: agentcore.RetryPolicy{MaxAttempts: 1}, open: func(ctx context.Context, rung nativeBoundRung, _, _ int) (*ai.AssistantMessageEventStream, error) {
				return session.openNativeAttempt(ctx, rung, transcript, options)
			}})
			if err != nil {
				t.Error("logical ladder failed", err)
				message := usageFixture("error", 0, 0)
				reason := err.Error()
				message.ErrorMessage = &reason
				out.Push(ai.AssistantMessageEvent{Type: "error", Error: message})
			}
		}()
		return out, nil
	}
	var err error
	session, err = NewPiSession(ctx, PiSessionConfig{Pi: binding, NativeStream: logical, nativeLadder: ladder, nativeAttemptObserved: projection.accountNativeAttempt, nativeTerminalPublished: projection.expectNativeTerminal})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	for i := 0; i < 2; i++ {
		if err = session.Prompt(ctx, json.RawMessage(`"repeat"`)); err != nil {
			t.Fatal(err)
		}
	}
	if projection.result.Usage.InputTokens != 8 || projection.result.Usage.OutputTokens != 3 || len(projection.nativeTerminal) != 0 {
		t.Fatal("engine message_end duplicated attempt usage", projection.result.Usage)
	}
	state, err := session.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var native struct{ Messages []ai.Message }
	if json.Unmarshal(state, &native) != nil {
		t.Fatal("invalid final state")
	}
	count := 0
	for _, message := range native.Messages {
		if message.Role == "assistant" {
			count++
			if message.StopReason == "error" {
				t.Fatal("discarded failure entered conversation")
			}
		}
	}
	if count != 2 {
		t.Fatal("successful responses missing from conversation", count)
	}
}
