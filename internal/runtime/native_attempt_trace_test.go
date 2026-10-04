package agentruntime

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/lohi-ai/agentray/agentcore"
	"github.com/lohi-ai/agentray/ai"
)

func TestNativeAttemptTracePreservesAdmissionFailure(t *testing.T) {
	ladder := testNativeLadder(t)
	failure := &agentcore.ProviderError{Status: 503, Message: "transport unavailable", RetryAfter: time.Second}
	calls := 0
	ladder.rungs[0].stream = func(context.Context, json.RawMessage, ai.TranscriptContext, map[string]any) (*ai.AssistantMessageEventStream, error) {
		calls++
		if calls == 1 {
			return nil, failure
		}
		return nativeChildResponse(ai.ContentBlock{Type: "text", Text: "ok"}), nil
	}
	binding, _, stream := ladder.sessionBinding()
	var traces []json.RawMessage
	binding.OnTrace = func(_ context.Context, raw json.RawMessage) {
		traces = append(traces, append(json.RawMessage(nil), raw...))
	}
	session, err := NewPiSession(context.Background(), PiSessionConfig{Pi: binding, NativeStream: stream, nativeLadder: ladder})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	out := ai.NewAssistantMessageEventStream()
	waits := 0
	_, err = session.runNativeLadder(session.ctx, out, nativeLadderRun{
		policy: agentcore.RetryPolicy{MaxAttempts: 2, MaxDelay: time.Millisecond},
		open: func(ctx context.Context, rung nativeBoundRung, _, _ int) (*ai.AssistantMessageEventStream, error) {
			return session.openNativeAttempt(ctx, rung, ai.TranscriptContext{}, nil)
		},
		wait: func(_ context.Context, delay time.Duration) error {
			waits++
			if delay != time.Millisecond {
				t.Error("typed Retry-After lost")
			}
			return nil
		},
		observe: func(_ context.Context, _ int, a nativeRetryAttempt) error {
			if a.number == 1 && a.failure != failure {
				t.Error("trace replaced admission error")
			}
			return nil
		},
	})
	if err != nil || calls != 2 || waits != 1 {
		t.Fatal("admission retry changed", calls, waits, err)
	}
	session.native.flushTraces()
	if len(traces) != 2 {
		t.Fatal("admission attempt trace missing", len(traces))
	}
	var first struct{ Error struct{ Message string } }
	if json.Unmarshal(traces[0], &first) != nil || first.Error.Message != failure.Error() {
		t.Fatal("trace error lost original admission failure")
	}
}
