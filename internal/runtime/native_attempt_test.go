package agentruntime

import (
	"context"
	"errors"
	"testing"

	"github.com/2found/2ai/agentcore"
	"github.com/2found/2ai/ai"
)

func attemptFixture(events ...ai.AssistantMessageEvent) func(context.Context) (*ai.AssistantMessageEventStream, error) {
	return func(ctx context.Context) (*ai.AssistantMessageEventStream, error) {
		source := ai.NewAssistantMessageEventStreamFor(ctx)
		for _, event := range events {
			source.Push(event)
		}
		source.End()
		return source, nil
	}
}
func assertAttemptEmpty(t *testing.T, out *ai.AssistantMessageEventStream) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, ok, err := out.Next(ctx); ok || !errors.Is(err, context.Canceled) {
		t.Fatal("uncommitted event escaped", ok, err)
	}
}

func TestNativeAttemptRelayDurableModelBeforeToolEvents(t *testing.T) {
	for _, failWrite := range []bool{false, true} {
		name := "committed"
		if failWrite {
			name = "append-failed"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			memory := agentcore.NewMemorySessionStore()
			var store agentcore.SessionStore = memory
			if failWrite {
				store = &nativeSelectionFailStore{MemorySessionStore: memory, selectionError: errors.New("append failed")}
			}
			ladder := testNativeLadder(t)
			binding, _, stream := ladder.sessionBinding()
			session, err := NewPiSession(ctx, PiSessionConfig{Pi: binding, NativeStream: stream, nativeLadder: ladder, Store: store, SessionID: name})
			if err != nil {
				t.Fatal(err)
			}
			defer session.Close()
			out := ai.NewAssistantMessageEventStream()
			result, err := (ai.FallbackProvider{}).Run(session.ctx, out, ai.FallbackRequest{Candidates: 1, Open: func(ctx context.Context, _, _ int) (*ai.AssistantMessageEventStream, error) {
				return attemptFixture(ai.AssistantMessageEvent{Type: "toolcall_start"}, ai.AssistantMessageEvent{Type: "done", Message: &ai.Message{Role: "assistant", StopReason: "toolUse"}})(ctx)
			}, Commit: func(ctx context.Context, _ int) error { return session.selectNativeRung(ctx, 0, 1) }})
			if failWrite {
				if err == nil || result.Committed {
					t.Fatal("failed append admitted tool event")
				}
				assertAttemptEmpty(t, out)
				return
			}
			if err != nil || !result.Committed {
				t.Fatal("selection failed", err)
			}
			event, ok, err := out.Next(ctx)
			if err != nil || !ok || event.Type != "toolcall_start" {
				t.Fatal("tool event lost", err)
			}
			entries, err := memory.Log(ctx, name)
			if err != nil {
				t.Fatal(err)
			}
			if entries[len(entries)-1].Kind != agentcore.EntryPiModelSelection || ladder.selection().Rung != 1 {
				t.Fatal("tool event preceded durable selection")
			}
			if _, err := recoverPiState(entries); err != nil {
				t.Fatal(err)
			}
		})
	}
}
