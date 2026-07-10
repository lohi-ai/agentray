package agentcore

import (
	"context"
	"testing"
)

// TestLifecycleEventOrder verifies a streamed run with one tool turn followed by
// a final answer emits the granular lifecycle events in the documented order,
// and that the back-compat token/tool events still appear.
func TestLifecycleEventOrder(t *testing.T) {
	faux := NewFauxProvider(
		AssistantToolCall("c1", "noop", `{}`),
		AssistantText("all done"),
	)
	agent, err := New(Config{
		Provider: faux,
		Model:    "test",
		Tools:    NewToolSet(noopTool{}),
		Policy:   NewAllowList("noop"),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	var events []StreamEventType
	sink := func(ev StreamEvent) { events = append(events, ev.Type) }
	if _, err := agent.PromptStream(context.Background(), "go", sink); err != nil {
		t.Fatalf("PromptStream: %v", err)
	}

	// Reduce to the lifecycle skeleton (drop token/tool/message_update noise) and
	// assert the boundary order.
	want := []StreamEventType{
		StreamAgentStart,
		StreamTurnStart, StreamMessageStart, StreamMessageEnd,
		StreamToolExecStart, StreamToolExecEnd, StreamTurnEnd,
		StreamTurnStart, StreamMessageStart, StreamMessageEnd, StreamTurnEnd,
		StreamAgentEnd,
	}
	var skeleton []StreamEventType
	keep := map[StreamEventType]bool{
		StreamAgentStart: true, StreamTurnStart: true, StreamMessageStart: true,
		StreamMessageEnd: true, StreamToolExecStart: true, StreamToolExecEnd: true,
		StreamTurnEnd: true, StreamAgentEnd: true,
	}
	for _, e := range events {
		if keep[e] {
			skeleton = append(skeleton, e)
		}
	}
	if len(skeleton) != len(want) {
		t.Fatalf("lifecycle skeleton = %v\nwant %v", skeleton, want)
	}
	for i := range want {
		if skeleton[i] != want[i] {
			t.Fatalf("event %d = %q, want %q\nfull: %v", i, skeleton[i], want[i], skeleton)
		}
	}

	// Back-compat: the StreamTool event (completed trace) is still emitted.
	var sawTool bool
	for _, e := range events {
		if e == StreamTool {
			sawTool = true
		}
	}
	if !sawTool {
		t.Fatalf("back-compat StreamTool event missing: %v", events)
	}
}
