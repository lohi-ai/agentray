package agentruntime

import (
	"context"
	"errors"
	"time"

	"github.com/lohi-ai/agentray/agentcore"
	"github.com/lohi-ai/agentray/ai"
)

type nativeLadderRun struct {
	policy    agentcore.RetryPolicy
	open      func(context.Context, nativeBoundRung, int, int) (*ai.AssistantMessageEventStream, error)
	observe   func(context.Context, int, ai.FallbackAttempt) error
	committed func(int)
	wait      func(context.Context, time.Duration) error
}

// runNativeLadder binds the AI fallback provider to durable session selection.
// Retry, replay safety and escalation are exclusively owned by ai.
func (s *PiSession) runNativeLadder(ctx context.Context, out *ai.AssistantMessageEventStream, run nativeLadderRun) (ai.AttemptOutcome, error) {
	ladder := s.config.nativeLadder
	if ladder == nil || run.open == nil {
		return ai.AttemptOutcome{}, errors.New("native ladder run requires bound rungs and provider admission")
	}
	initial := ladder.selection()
	provider := ai.FallbackProvider{Retry: run.policy, Wait: run.wait}
	return provider.Run(ctx, out, ai.FallbackRequest{
		Start: initial.Rung, Candidates: len(ladder.rungs), Validate: s.failure,
		Open: func(ctx context.Context, index, number int) (*ai.AssistantMessageEventStream, error) {
			if err := s.failure(); err != nil {
				return nil, &ai.PreparationError{Cause: err}
			}
			candidate := ladder.rungs[index]
			candidate.model = append([]byte(nil), candidate.model...)
			candidate.config.Options = append([]byte(nil), candidate.config.Options...)
			scoped, err := ladder.attemptContext(ctx, index, initial.Generation)
			if err != nil {
				return nil, &ai.PreparationError{Cause: err}
			}
			return run.open(scoped, candidate, index, number)
		},
		Commit: func(ctx context.Context, index int) error {
			if err := s.selectNativeRung(ctx, initial.Generation, index); err != nil {
				return err
			}
			if run.committed != nil {
				run.committed(index)
			}
			return nil
		},
		Observe: func(ctx context.Context, index int, attempt ai.FallbackAttempt) error {
			if s.config.nativeAttemptObserved != nil {
				if err := s.config.nativeAttemptObserved(ladder.rungs[index], attempt); err != nil {
					return err
				}
			}
			if run.observe != nil {
				return run.observe(ctx, index, attempt)
			}
			return nil
		},
		BeforePublish: s.config.nativeTerminalPublished,
	})
}
