package agentruntime

import (
	"context"

	"github.com/lohi-ai/agentray/agentcore"
	"github.com/lohi-ai/agentray/agentcore/engine"
	nativehost "github.com/lohi-ai/agentray/agentcore/host"
	"github.com/lohi-ai/agentray/ai"
)

// admitNativeRequest binds session preparation and durable model selection to
// shared admission. Only physical provider attempts are traced/accounted.
func (s *PiSession) admitNativeRequest(ctx context.Context, source engine.Request, options map[string]any, policy agentcore.RetryPolicy) (*engine.RequestAdmission, error) {
	return nativehost.Admit(ctx, func(ctx context.Context, out *ai.AssistantMessageEventStream, accept func(engine.Request)) (engine.Request, ai.AttemptOutcome, error) {
		var prepared nativePreparedAttempt
		final, err := s.runNativeLadder(ctx, out, nativeLadderRun{
			policy: policy,
			open: func(ctx context.Context, rung nativeBoundRung, _, _ int) (*ai.AssistantMessageEventStream, error) {
				var err error
				prepared, err = s.prepareNativeAttempt(ctx, rung, source, options)
				if err != nil {
					return nil, err
				}
				return s.openNativeAttempt(ctx, rung, prepared.transcript, prepared.options)
			},
			committed: func(int) { accept(prepared.request) },
		})
		return prepared.request, final, err
	})
}
