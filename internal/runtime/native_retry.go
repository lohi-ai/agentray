package agentruntime

import (
	"context"
	"errors"
	"time"

	"github.com/lohi-ai/agentray/agentcore"
	"github.com/lohi-ai/agentray/ai"
)

// nativeRetryAttempt is delivered after producer settlement, including failed
// attempts whose terminal event is withheld from the logical request. Observers
// can account for usage without inserting failed messages into the transcript.
type nativeRetryAttempt struct {
	number  int
	outcome nativeAttemptOutcome
	failure error
}

type nativeRungAttempts struct {
	policy agentcore.RetryPolicy
	open   func(context.Context, int) (*ai.AssistantMessageEventStream, error)
	commit func(context.Context) error
	// failure returns transport metadata observed for this completed attempt.
	// It must not infer HTTP status/Retry-After from a formatted error message.
	failure func(int) error
	observe func(context.Context, nativeRetryAttempt) error
	wait    func(context.Context, time.Duration) error
}

// run drains attempts on one rung, leaving the final outcome unpublished so the
// ladder can decide escalation. Once content commits, errors never replay. Host
// callback/protocol failures abort immediately. Each rung owns its retry budget.
func (r nativeRungAttempts) run(ctx context.Context, out *ai.AssistantMessageEventStream) (last nativeAttemptOutcome, err error) {
	if r.open == nil || r.commit == nil {
		return last, errors.New("native retries require provider and commit callbacks")
	}
	wait := r.wait
	if wait == nil {
		wait = waitNativeRetry
	}
	for number := 1; ; number++ {
		if err = ctx.Err(); err != nil {
			return last, err
		}
		attemptCtx, capture := ai.WithNativeProviderFailure(ctx)
		last, err = relayNativeAttempt(attemptCtx, out, func(ctx context.Context) (*ai.AssistantMessageEventStream, error) { return r.open(ctx, number) }, r.commit)
		failure := last.admissionError
		var preparation *nativePreparationError
		if err == nil && errors.As(failure, &preparation) {
			err = failure
		}
		if err != nil {
			failure = err
		} else if last.terminal.Type == "error" {
			failure = capture.Failure()
			if r.failure != nil {
				failure = r.failure(number)
			}
			if failure == nil {
				message := "native provider request failed"
				if last.terminal.Error != nil && last.terminal.Error.ErrorMessage != nil {
					message = *last.terminal.Error.ErrorMessage
				}
				failure = errors.New(message)
			}
			if capture.HostFailure() {
				err = failure
			}
		}
		if r.observe != nil {
			if observeErr := r.observe(context.WithoutCancel(ctx), nativeRetryAttempt{number: number, outcome: last, failure: failure}); observeErr != nil {
				return last, observeErr
			}
		}
		if err != nil {
			return last, err
		}
		if last.terminal.Reason == "aborted" || (last.terminal.Error != nil && last.terminal.Error.StopReason == "aborted") {
			return last, nil
		}
		if ctx.Err() != nil {
			return last, ctx.Err()
		}
		if last.committed || last.terminal.Type == "done" {
			return last, nil
		}
		delay, retry := r.policy.NextDelay(number, failure)
		if !retry {
			return last, nil
		}
		if err = wait(ctx, delay); err != nil {
			return last, err
		}
	}
}

func waitNativeRetry(ctx context.Context, delay time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return ctx.Err()
	}
}
