package agentruntime

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/lohi-ai/agentray/agentcore"
	"github.com/lohi-ai/agentray/ai"
)

func TestNativeRungRetriesPreserveUsageAndLastPointers(t *testing.T) {
	ctx := context.Background()
	out := ai.NewAssistantMessageEventStream()
	failure := &agentcore.ProviderError{Status: 429, Message: "busy", RetryAfter: time.Second}
	failed := &ai.Message{Role: "assistant", StopReason: "error", Usage: &ai.Usage{Input: 2}}
	success := &ai.Message{Role: "assistant", StopReason: "stop", Usage: &ai.Usage{Input: 3}}
	calls, commits, observed, waits := 0, 0, 0, 0
	usage := float64(0)
	runner := nativeRungAttempts{
		policy: agentcore.RetryPolicy{MaxAttempts: 3, BaseDelay: time.Millisecond, MaxDelay: 7 * time.Millisecond},
		open: func(ctx context.Context, n int) (*ai.AssistantMessageEventStream, error) {
			calls++
			if n < 3 {
				return attemptFixture(ai.AssistantMessageEvent{Type: "start", Partial: failed}, ai.AssistantMessageEvent{Type: "error", Error: failed})(ctx)
			}
			return attemptFixture(ai.AssistantMessageEvent{Type: "start", Partial: success}, ai.AssistantMessageEvent{Type: "done", Message: success})(ctx)
		},
		failure: func(int) error { return failure },
		commit:  func(context.Context) error { commits++; assertAttemptEmpty(t, out); return nil },
		wait: func(_ context.Context, d time.Duration) error {
			waits++
			if d != 7*time.Millisecond {
				t.Errorf("Retry-After cap lost: %v", d)
			}
			assertAttemptEmpty(t, out)
			return nil
		},
		observe: func(_ context.Context, attempt nativeRetryAttempt) error {
			observed++
			if attempt.number != observed {
				t.Error("attempt numbering changed")
			}
			if attempt.outcome.terminal.Error != nil {
				usage += attempt.outcome.terminal.Error.Usage.Input
				if attempt.failure != failure {
					t.Error("typed failure lost")
				}
			} else {
				usage += attempt.outcome.terminal.Message.Usage.Input
			}
			return nil
		},
	}
	result, err := runner.run(ctx, out)
	if err != nil || calls != 3 || commits != 1 || observed != 3 || waits != 2 || usage != 7 {
		t.Fatalf("retry accounting: calls=%d commits=%d observed=%d waits=%d usage=%v err=%v", calls, commits, observed, waits, usage, err)
	}
	if err = result.publish(out); err != nil {
		t.Fatal(err)
	}
	got, err := out.Result(ctx)
	if err != nil || got != success {
		t.Fatal("retry copied final message", err)
	}
}

func TestNativeRungRetryStopsAtRequiredBoundaries(t *testing.T) {
	for _, mode := range []string{"budget", "permanent", "untyped", "visible", "aborted", "commit", "protocol", "observer", "cancel-wait"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			out := ai.NewAssistantMessageEventStream()
			failure := &agentcore.ProviderError{Status: 503, Message: "overloaded"}
			message := &ai.Message{Role: "assistant", StopReason: "error"}
			calls, waits := 0, 0
			sentinel := errors.New("host failure")
			runner := nativeRungAttempts{
				policy: agentcore.RetryPolicy{MaxAttempts: 2, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond},
				open: func(ctx context.Context, _ int) (*ai.AssistantMessageEventStream, error) {
					calls++
					events := []ai.AssistantMessageEvent{{Type: "start", Partial: message}}
					if mode == "visible" || mode == "commit" {
						events = append(events, ai.AssistantMessageEvent{Type: "text_delta", Delta: "x", Partial: message})
					}
					if mode == "protocol" {
						return attemptFixture()(ctx)
					}
					if mode == "aborted" {
						message.StopReason = "aborted"
					}
					return attemptFixture(append(events, ai.AssistantMessageEvent{Type: "error", Reason: message.StopReason, Error: message})...)(ctx)
				},
				failure: func(int) error {
					if mode == "permanent" {
						return &agentcore.ProviderError{Status: 403, Message: "denied"}
					}
					if mode == "untyped" {
						return errors.New("503 overloaded Retry-After: 1")
					}
					return failure
				},
				commit: func(context.Context) error {
					if mode == "commit" {
						return sentinel
					}
					return nil
				},
				observe: func(context.Context, nativeRetryAttempt) error {
					if mode == "observer" {
						return sentinel
					}
					return nil
				},
				wait: func(ctx context.Context, _ time.Duration) error {
					waits++
					if mode == "cancel-wait" {
						cancel()
						return ctx.Err()
					}
					return nil
				},
			}
			last, err := runner.run(ctx, out)
			want := 1
			if mode == "budget" {
				want = 2
			}
			if calls != want {
				t.Fatalf("calls=%d want=%d", calls, want)
			}
			switch mode {
			case "commit", "observer":
				if !errors.Is(err, sentinel) {
					t.Fatal("host error lost", err)
				}
			case "protocol":
				if err == nil {
					t.Fatal("malformed stream retried")
				}
			case "cancel-wait":
				if !errors.Is(err, context.Canceled) || waits != 1 || last.terminal.Error != message {
					t.Fatal("cancelled retry lost last failure", err)
				}
			default:
				if err != nil {
					t.Fatal(err)
				}
			}
			if mode == "visible" && !last.committed {
				t.Fatal("visible content marked replayable")
			}
		})
	}
}

func TestNativeRungAdmissionRetriesAndCancelledWait(t *testing.T) {
	out := ai.NewAssistantMessageEventStream()
	failure := &agentcore.ProviderError{Status: 500, Message: "temporary", RetryAfter: time.Millisecond}
	calls := 0
	runner := nativeRungAttempts{policy: agentcore.RetryPolicy{MaxAttempts: 2}, open: func(context.Context, int) (*ai.AssistantMessageEventStream, error) { calls++; return nil, failure }, commit: func(context.Context) error { t.Error("admission failure committed"); return nil }}
	result, err := runner.run(context.Background(), out)
	if err != nil || calls != 2 || result.admissionError != failure || result.publish(out) != failure {
		t.Fatal("admission retry contract changed", calls, err)
	}
	assertAttemptEmpty(t, out)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err = waitNativeRetry(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled backoff waited", err)
	}
}
