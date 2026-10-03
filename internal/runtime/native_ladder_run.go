package agentruntime

import (
	"context"
	"errors"
	"time"

	"github.com/lohi-ai/agentray/agentcore"
	"github.com/lohi-ai/agentray/ai"
)

type nativeLadderRun struct {
	policy  agentcore.RetryPolicy
	open    func(context.Context, nativeBoundRung, int, int) (*ai.AssistantMessageEventStream, error)
	observe func(context.Context, int, nativeRetryAttempt) error
	// committed runs after durable model selection and before any visible event.
	committed func(int)
	wait      func(context.Context, time.Duration) error
}

// runNativeLadder coordinates one logical request under the session's durable
// selection fence. Candidate rungs are private to their attempts; only commit
// publishes a new active binding. The caller owns out's producer lifetime.
func (s *PiSession) runNativeLadder(ctx context.Context, out *ai.AssistantMessageEventStream, run nativeLadderRun) (nativeAttemptOutcome, error) {
	ladder := s.config.nativeLadder
	if ladder == nil || run.open == nil {
		return nativeAttemptOutcome{}, errors.New("native ladder run requires bound rungs and provider admission")
	}
	if err := s.failure(); err != nil {
		return nativeAttemptOutcome{}, err
	}
	initial := ladder.selection()
	for index := initial.Rung; index < len(ladder.rungs); index++ {
		// Bindings are immutable after construction. Each retry gets fresh JSON
		// so request preparation cannot mutate the next attempt's binding.
		rung := ladder.rungs[index]
		attempts := nativeRungAttempts{
			policy: run.policy,
			open: func(ctx context.Context, number int) (*ai.AssistantMessageEventStream, error) {
				if err := s.failure(); err != nil {
					return nil, &nativePreparationError{cause: err}
				}
				candidate := rung
				candidate.model = append([]byte(nil), rung.model...)
				candidate.config.Options = append([]byte(nil), rung.config.Options...)
				scoped, err := ladder.attemptContext(ctx, index, initial.Generation)
				if err != nil {
					return nil, &nativePreparationError{cause: err}
				}
				return run.open(scoped, candidate, index, number)
			},
			commit: func(ctx context.Context) error {
				if err := s.selectNativeRung(ctx, initial.Generation, index); err != nil {
					return err
				}
				if run.committed != nil {
					run.committed(index)
				}
				return nil
			},
			wait: run.wait,
		}
		if run.observe != nil || s.config.nativeAttemptObserved != nil {
			attempts.observe = func(ctx context.Context, attempt nativeRetryAttempt) error {
				if s.config.nativeAttemptObserved != nil {
					if err := s.config.nativeAttemptObserved(rung, attempt); err != nil {
						return err
					}
				}
				if run.observe != nil {
					return run.observe(ctx, index, attempt)
				}
				return nil
			}
		}
		result, err := attempts.run(ctx, out)
		if err != nil {
			return result, err
		}
		if err = s.failure(); err != nil {
			return result, err
		}
		aborted := result.terminal.Reason == "aborted" || (result.terminal.Error != nil && result.terminal.Error.StopReason == "aborted")
		if result.committed || result.terminal.Type == "done" || aborted || index+1 == len(ladder.rungs) {
			if result.admissionError == nil && s.config.nativeTerminalPublished != nil {
				if err := s.config.nativeTerminalPublished(result); err != nil {
					return result, err
				}
			}
			return result, result.publish(out)
		}
		if err = ctx.Err(); err != nil {
			return result, err
		}
	}
	return nativeAttemptOutcome{}, errors.New("native ladder has no active rung")
}
