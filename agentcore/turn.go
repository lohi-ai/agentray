package agentcore

import (
	"context"
	"errors"
	"slices"
	"strings"
)

// streamAbort is the sentinel a StreamInterceptor raises to cut a turn's
// provider call mid-flight. It is NOT a provider failure: callRung must not
// spend a retry on it and reason must not escalate it — the loop catches it,
// appends the injection, and re-issues the same request itself.
type streamAbort struct {
	// inject is the correction the retried turn must see.
	inject []Message
	// usage is whatever the aborted attempt still spent: the tokens were
	// generated and billed even though the message is discarded, so the run's
	// accounting folds it in rather than letting a rule fire for free.
	usage Usage
}

func (e *streamAbort) Error() string { return "stream aborted by interceptor" }

// maxStreamAbortsPerTurn backstops a StreamInterceptor that never stops
// matching: past it the turn's provider call is retried once with interception
// disabled, so the stream completes unmodified. A guard that can spin the
// provider forever is an availability bug wearing a policy costume — the
// plugin's own per-turn cap is the real bound; this only covers a broken one.
const maxStreamAbortsPerTurn = 8

// One turn against the model: retry, escalation, streaming.
//
// The loop asks for a turn; this file decides which rung answers it and how
// hard to try. Keeping that here rather than in an LLMProvider is deliberate —
// a vendor adapter that owned its own retry would make failure behavior differ
// per provider, and the run's cost ceiling is the loop's to enforce.

// reason issues one turn against the rung currently in use. Two distinct
// recoveries are layered, in order, so a transient blip is no longer conflated
// with a capability shortfall:
//
//  1. Same-rung retry (callRung): a retryable failure (429/5xx/network) on the
//     *same* model is retried with exponential backoff, honoring any Retry-After,
//     before the rung is given up — a brief outage rides out in place, cheaply.
//  2. Escalation: only once a rung is exhausted (its retries spent, or a
//     non-retryable error) does the loop fall down the ladder to the next rung and
//     try the turn there, sticking with the first rung that works (*rung advances
//     in place). Cancellation is never retried or escalated.
func (a *Agent) reason(ctx context.Context, req ChatRequest, sink StreamSink, streams []StreamInterceptor, ladder []ModelRung, rung *int) (ChatResponse, error) {
	for {
		p := ladder[*rung]
		req.Model = p.Model

		// Re-resolve this rung's API key before the call so an expiring BYO token
		// doesn't kill a long run; applied only when the provider is a KeyUpdater.
		if a.refreshKey != nil {
			if key, kerr := a.refreshKey(ctx, p.Provider.Name()); kerr == nil {
				if u, ok := p.Provider.(KeyUpdater); ok {
					u.UpdateAPIKey(key)
				}
			}
		}

		resp, err := a.callRung(ctx, p, req, sink, streams)
		if err == nil {
			// after_provider_response observers see the raw response before its usage
			// is folded into the run total. Under HookThrow a failure aborts the turn.
			if herr := a.hooks.runAfterProviderResponse(ctx, resp); herr != nil {
				return ChatResponse{}, herr
			}
			return resp, nil
		}
		// A stream abort is the loop's own retry signal, not a provider failure:
		// it surfaces to the turn loop, which appends the injection and re-issues
		// the request itself. Escalating it would retry on a different model
		// against a conversation that never received the correction.
		var abort *streamAbort
		if errors.As(err, &abort) {
			return ChatResponse{}, err
		}
		// Don't escalate on cancellation, and stop when the ladder is exhausted.
		if ctx.Err() != nil || *rung+1 >= len(ladder) {
			return ChatResponse{}, err
		}
		*rung++ // escalate to the next rung and retry this turn
	}
}

// callRung issues the turn against one rung, retrying the same model on a
// transient failure per the run's RetryPolicy before surfacing the error to the
// escalation logic. The first attempt is immediate; each retry waits a backoff
// (Retry-After when the server supplied one, else exponential with jitter) that
// is cancellation-aware. A non-retryable error or a cancellation returns at once,
// spending no further attempts.
//
// streams carries the run's stream interceptors. On the streaming path they are
// consulted per content delta; on the non-streaming path they see the completed
// response once, so a rule still fires when no viewer is attached. A match
// returns the streamAbort sentinel — never retried here, never escalated.
func (a *Agent) callRung(ctx context.Context, p ModelRung, req ChatRequest, sink StreamSink, streams []StreamInterceptor) (ChatResponse, error) {
	var lastErr error
	for attempt := 0; attempt < a.retry.MaxAttempts; attempt++ {
		if attempt > 0 {
			if err := sleepBackoff(ctx, a.retry.delay(attempt-1, retryAfterOf(lastErr))); err != nil {
				return ChatResponse{}, lastErr // cancelled mid-backoff; report the provider failure
			}
		}
		var resp ChatResponse
		var err error
		if sink != nil {
			resp, err = a.streamTurn(ctx, p.Provider, req, sink, streams)
		} else {
			resp, err = p.Provider.Chat(ctx, req)
			// Non-streaming fallback: the interceptor sees the completed answer
			// once. It cannot cut the message mid-token — the tokens are already
			// spent — but it can still discard it and retry with the injection,
			// which is the half of the contract that changes what the model does.
			if err == nil && len(streams) > 0 {
				if d := interceptStreamDelta(ctx, streams, resp.Message.Content); d.Abort {
					err = &streamAbort{inject: d.Inject, usage: resp.Usage}
				}
			}
		}
		if err == nil {
			return resp, nil
		}
		// An interceptor abort is not a transient failure: spending a same-rung
		// retry on it would re-issue the identical request with no injection.
		var abort *streamAbort
		if errors.As(err, &abort) {
			return ChatResponse{}, err
		}
		lastErr = err
		// A cancellation or a non-retryable error won't improve with another attempt:
		// hand it to the escalation logic immediately.
		if ctx.Err() != nil || !IsRetryable(err) {
			return ChatResponse{}, err
		}
	}
	return ChatResponse{}, lastErr
}

// streamTurn consumes the provider's delta channel for one turn, forwarding
// content fragments to the sink as they arrive and accumulating the full
// assistant message (text + tool calls + usage) so the Act path is identical to
// the non-streaming turn.
//
// When stream interceptors are installed the provider call runs on a cancelable
// child context: the first interceptor to abort cancels it — cutting the HTTP
// stream mid-token — and the partial message is discarded into the streamAbort
// sentinel. The channel is drained after cancel so a provider that does not
// select on ctx.Done (the scripted test providers) still finishes its goroutine
// instead of leaking it against a full buffer.
func (a *Agent) streamTurn(ctx context.Context, provider LLMProvider, req ChatRequest, sink StreamSink, streams []StreamInterceptor) (ChatResponse, error) {
	streamCtx := ctx
	cancel := context.CancelFunc(nil)
	if len(streams) > 0 {
		streamCtx, cancel = context.WithCancel(ctx)
		defer cancel()
	}
	ch, err := provider.Stream(streamCtx, req)
	if err != nil {
		return ChatResponse{}, err
	}
	msg := Message{Role: RoleAssistant}
	var resp ChatResponse
	for d := range ch {
		if d.Err != nil {
			return ChatResponse{}, d.Err
		}
		if d.ContentDelta != "" {
			msg.Content += d.ContentDelta
			sink(StreamEvent{Type: StreamToken, Token: d.ContentDelta})
			if len(streams) > 0 {
				if dec := interceptStreamDelta(ctx, streams, msg.Content); dec.Abort {
					cancel()
					for range ch {
					}
					return ChatResponse{}, &streamAbort{inject: dec.Inject, usage: resp.Usage}
				}
			}
		}
		if d.ToolCall != nil {
			msg.ToolCalls = append(msg.ToolCalls, *d.ToolCall)
		}
		// Usage is not the Done delta's private property: providers report it in
		// pieces (input up front, output at the end) and as running totals, so
		// take the newest non-zero value of each field wherever it arrives. A
		// last-write-wins assignment on Done alone zeroes the whole turn's spend
		// for any provider that reports early — and the run's budget gate meters
		// on that number, so the loss is invisible until a run overshoots.
		resp.Usage = mergeUsage(resp.Usage, d.Usage)
		if d.Done {
			resp.StopReason = d.StopReason
		}
	}
	resp.Message = msg
	return resp, nil
}


// filterSchemas keeps only the schemas whose name is in permitted.
func filterSchemas(all []ToolSchema, permitted []string) []ToolSchema {
	keep := make(map[string]bool, len(permitted))
	for _, n := range permitted {
		keep[n] = true
	}
	out := make([]ToolSchema, 0, len(all))
	for _, s := range all {
		if keep[s.Name] {
			out = append(out, s)
		}
	}
	return out
}

// retainedTranscript is what a completed compaction stores on its durable
// entry: the transcript it left behind, minus the run's own system prompt.
//
// The prompt is excluded because it is not part of the conversation — every run
// rebuilds it from the definition, recalled memory, and the extensions' prompt
// sections, then prepends it (a resumed run re-states the same contract). The
// durable log has never carried it, so storing it here would give a resumed run
// two system prompts: the stored one and the freshly derived one.
//
// Matching on the exact prompt content rather than "drop the first message" is
// what keeps this honest when the leading system message is something else —
// a goal pin promoted into the head by an earlier compaction, or a caller whose
// seed history opens with its own system message. Those belong to the
// conversation and must survive.
func retainedTranscript(messages []Message, system string) []Message {
	if len(messages) > 0 && system != "" &&
		messages[0].Role == RoleSystem && messages[0].Content == system {
		messages = messages[1:]
	}
	// Clone: the caller keeps mutating res.Messages, and a durable entry must be
	// a snapshot of the moment it was written.
	return slices.Clone(messages)
}

// endsWithUserText reports whether the last user message in messages already
// says exactly text. It is what keeps a retried resume from appending the same
// instruction twice: the second attempt recovers a log that already ends with
// it, and a duplicate would be folded into the pin as if the user had said it
// again.
func endsWithUserText(messages []Message, text string) bool {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == RoleUser {
			return strings.TrimSpace(messages[i].Content) == text
		}
	}
	return false
}

// lastAssistantText returns the content of the most recent assistant message.
func lastAssistantText(messages []Message) string {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == RoleAssistant && messages[i].Content != "" {
			return messages[i].Content
		}
	}
	return ""
}
