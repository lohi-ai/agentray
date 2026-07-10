package agentcore

import (
	"context"
	"fmt"
)

// BeforeToolCall fires after a tool's arguments are schema-validated and before
// execution. Returning a blocked Decision stops the call; the reason is fed
// back to the model. The permission gate is itself a BeforeToolCall hook;
// consumers may add more (PII redaction, cost ceilings, human-approval gates).
type BeforeToolCall func(ctx context.Context, call ToolCall) Decision

// AfterToolCall fires after a tool executes. It may annotate or rewrite the
// result string, and may set terminate to end the run cleanly (a terminal tool
// such as submit_recommendation/finish).
type AfterToolCall func(ctx context.Context, call ToolCall, result string, runErr error) (rewritten string, terminate bool)

// ContextHook transforms the message list immediately before a provider request
// (pi's `context` event). It sees a copy of the live history and returns the
// view the model should reason over — e.g. redaction, trimming, injecting a
// reminder. Returning nil keeps the input unchanged. It does not mutate the
// persisted run history; only the outgoing request is affected.
type ContextHook func(ctx context.Context, msgs []Message) []Message

// ProviderRequestHook inspects or rewrites the assembled ChatRequest right
// before it is sent (pi's `before_provider_request`). Use it to pin a stop
// sequence, cap tools, or tee the request to a logger. Returning a zero-value
// request (no messages) is treated as "no change".
type ProviderRequestHook func(ctx context.Context, req ChatRequest) ChatRequest

// MessageEndHook observes a completed assistant message (pi's `message_end`).
// It is read-only: a return value is not threaded back. Use it for metrics,
// audit, or surfacing the turn to an external sink.
type MessageEndHook func(ctx context.Context, msg Message)

// ProviderResponseHook observes a completed provider response at the raw call
// boundary (pi's `after_provider_response`), before its usage is folded into the
// run total. Read-only: use it to meter tokens/cost or inspect the stop reason
// per provider call — earlier and more granular than MessageEnd, which fires
// once the assistant message is assembled and appended. A payload-rewrite seam
// already exists as ProviderRequestHook (our ChatRequest is the assembled
// payload), so there is deliberately no separate before_provider_payload hook;
// rewriting the outgoing request is what BeforeProviderRequest is for.
type ProviderResponseHook func(ctx context.Context, resp ChatResponse)

// HookErrorPolicy governs what happens when a hook handler panics or returns an
// error. The default (continue) keeps a single bad hook from taking down a run.
type HookErrorPolicy string

const (
	// HookContinue logs/attributes the failure and proceeds (default). A
	// panicking observer never aborts the run.
	HookContinue HookErrorPolicy = "continue"
	// HookThrow aborts the run, surfacing the attributed hook error from the
	// loop. Opt-in, for consumers that treat a hook failure as fatal.
	HookThrow HookErrorPolicy = "throw"
)

// Hooks is the ordered set of lifecycle interceptors for a run. The tool hooks
// (Before/After) gate and rewrite tool execution; the typed event hooks
// (Context/BeforeProviderRequest/MessageEnd) participate in or observe the
// reasoning turn. All handlers are panic-hardened per ErrorPolicy.
type Hooks struct {
	Before []BeforeToolCall
	After  []AfterToolCall

	// Context runs in order before every provider request, each transforming the
	// message view the next one sees (a reducer over the message list).
	Context []ContextHook
	// BeforeProviderRequest runs in order on the assembled request, each seeing
	// the previous one's output.
	BeforeProviderRequest []ProviderRequestHook
	// MessageEnd observers run in order after each assistant message completes.
	MessageEnd []MessageEndHook
	// AfterProviderResponse observers run in order on each successful provider
	// response, at the raw call boundary (before usage accumulation).
	AfterProviderResponse []ProviderResponseHook

	// ErrorPolicy governs handler panics/errors. Zero value == HookContinue.
	ErrorPolicy HookErrorPolicy
	// OnError, when set, receives every attributed hook failure (source + error)
	// regardless of policy, so a consumer can log/meter it. Source is a stable
	// identifier like "context[1]" or "before_tool_call[0]".
	OnError func(source string, err error)
}

// emitErr applies the error policy to one attributed handler failure: it always
// reports via OnError (if set), then either swallows it (continue) or returns a
// wrapped error for the loop to abort on (throw).
func (h Hooks) emitErr(source string, err error) error {
	if err == nil {
		return nil
	}
	if h.OnError != nil {
		h.OnError(source, err)
	}
	if h.ErrorPolicy == HookThrow {
		return fmt.Errorf("hook %s: %w", source, err)
	}
	return nil
}

// safe runs fn, converting a panic into an error so one reckless handler cannot
// crash the run goroutine. The recovered value is wrapped for attribution.
func safe(fn func()) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v", r)
		}
	}()
	fn()
	return nil
}

// runBefore evaluates the before-hooks in order, short-circuiting on the first
// block. A panicking before-hook is attributed and skipped (treated as
// non-blocking under HookContinue); under HookThrow it propagates as a block so
// the call is refused rather than silently executed.
func (h Hooks) runBefore(ctx context.Context, call ToolCall) Decision {
	for i, hook := range h.Before {
		var d Decision = Allowed()
		if perr := safe(func() { d = hook(ctx, call) }); perr != nil {
			if e := h.emitErr(fmt.Sprintf("before_tool_call[%d]", i), perr); e != nil {
				return Blocked(e.Error())
			}
			continue
		}
		if !d.Allow {
			return d
		}
	}
	return Allowed()
}

// runAfter threads the result through the after-hooks in order; terminate is
// sticky once any hook sets it. A panicking after-hook is attributed and
// skipped (the prior result is kept); under HookThrow the run is unaffected
// here — after-hooks observe/annotate and never block — but the failure is
// still reported via OnError.
func (h Hooks) runAfter(ctx context.Context, call ToolCall, result string, runErr error) (string, bool) {
	terminate := false
	for i, hook := range h.After {
		var rewritten string
		var t bool
		if perr := safe(func() { rewritten, t = hook(ctx, call, result, runErr) }); perr != nil {
			_ = h.emitErr(fmt.Sprintf("after_tool_call[%d]", i), perr)
			continue
		}
		result = rewritten
		terminate = terminate || t
	}
	return result, terminate
}

// runContext folds the context hooks over the message list, each seeing the
// previous one's output. A panicking hook is attributed and skipped (its input
// is passed through). Under HookThrow a failure aborts via a returned error.
func (h Hooks) runContext(ctx context.Context, msgs []Message) ([]Message, error) {
	for i, hook := range h.Context {
		var out []Message
		source := fmt.Sprintf("context[%d]", i)
		if perr := safe(func() { out = hook(ctx, msgs) }); perr != nil {
			if e := h.emitErr(source, perr); e != nil {
				return msgs, e
			}
			continue
		}
		if out != nil {
			msgs = out
		}
	}
	return msgs, nil
}

// runBeforeProviderRequest folds the request hooks over the assembled request.
// A hook that returns a request with no messages is treated as "no change".
func (h Hooks) runBeforeProviderRequest(ctx context.Context, req ChatRequest) (ChatRequest, error) {
	for i, hook := range h.BeforeProviderRequest {
		var out ChatRequest
		source := fmt.Sprintf("before_provider_request[%d]", i)
		if perr := safe(func() { out = hook(ctx, req) }); perr != nil {
			if e := h.emitErr(source, perr); e != nil {
				return req, e
			}
			continue
		}
		if len(out.Messages) > 0 {
			req = out
		}
	}
	return req, nil
}

// runMessageEnd dispatches the read-only message_end observers in order. A
// panicking observer is attributed and skipped; under HookThrow a failure
// aborts via a returned error.
func (h Hooks) runMessageEnd(ctx context.Context, msg Message) error {
	for i, hook := range h.MessageEnd {
		source := fmt.Sprintf("message_end[%d]", i)
		if perr := safe(func() { hook(ctx, msg) }); perr != nil {
			if e := h.emitErr(source, perr); e != nil {
				return e
			}
		}
	}
	return nil
}

// runAfterProviderResponse dispatches the read-only after_provider_response
// observers in order, mirroring runMessageEnd's panic/error policy.
func (h Hooks) runAfterProviderResponse(ctx context.Context, resp ChatResponse) error {
	for i, hook := range h.AfterProviderResponse {
		source := fmt.Sprintf("after_provider_response[%d]", i)
		if perr := safe(func() { hook(ctx, resp) }); perr != nil {
			if e := h.emitErr(source, perr); e != nil {
				return e
			}
		}
	}
	return nil
}
