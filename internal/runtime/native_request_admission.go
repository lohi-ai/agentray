package agentruntime

import (
	"context"
	"fmt"
	"sync"

	"github.com/lohi-ai/agentray/agentcore"
	"github.com/lohi-ai/agentray/agentcore/engine"
	"github.com/lohi-ai/agentray/ai"
)

// admitNativeRequest waits only until the candidate is committed (or the last
// failure settles). The engine receives its prepared context before consuming
// the stream, while the provider can continue producing live deltas. Only each
// provider attempt is observed; there is no extra logical-request trace.
func (s *PiSession) admitNativeRequest(ctx context.Context, source engine.Request, options map[string]any, policy agentcore.RetryPolicy) (*engine.RequestAdmission, error) {
	type admission struct {
		request engine.Request
		err     error
	}
	ready := make(chan admission, 1)
	done := make(chan struct{})
	var once sync.Once
	var final nativeAttemptOutcome
	var failure error
	notify := func(request engine.Request, err error) {
		once.Do(func() { ready <- admission{request: request, err: err} })
	}
	wait := func(ctx context.Context) error {
		select {
		case <-done:
			return failure
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	out := ai.NewAssistantMessageEventStreamWithHooks(ai.AssistantStreamHooks{
		Result: func(ctx context.Context) (*ai.Message, error) {
			if err := wait(ctx); err != nil {
				return nil, err
			}
			return nativeAttemptMessage(final), nil
		},
		AfterEnd: wait,
	})
	go func() {
		var prepared nativePreparedAttempt
		defer func() {
			if value := recover(); value != nil {
				failure = fmt.Errorf("native request admission panic: %v", value)
			}
			out.End()
			close(done)
			// A terminal failure without visible content never commits a model,
			// but still belongs to the last successfully prepared request.
			notify(prepared.request, failure)
		}()
		final, failure = s.runNativeLadder(ctx, out, nativeLadderRun{
			policy: policy,
			open: func(ctx context.Context, rung nativeBoundRung, _, _ int) (*ai.AssistantMessageEventStream, error) {
				var err error
				prepared, err = s.prepareNativeAttempt(ctx, rung, source, options)
				if err != nil {
					return nil, err
				}
				return s.openNativeAttempt(ctx, rung, prepared.transcript, prepared.options)
			},
			committed: func(int) { notify(prepared.request, nil) },
		})
	}()
	selected := <-ready
	if selected.err != nil {
		return nil, selected.err
	}
	return &engine.RequestAdmission{Request: selected.request, Stream: out}, nil
}
