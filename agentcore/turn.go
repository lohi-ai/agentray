package agentcore

import "context"

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
func (a *Agent) reason(ctx context.Context, req ChatRequest, sink StreamSink, ladder []ModelRung, rung *int) (ChatResponse, error) {
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

		resp, err := a.callRung(ctx, p, req, sink)
		if err == nil {
			// after_provider_response observers see the raw response before its usage
			// is folded into the run total. Under HookThrow a failure aborts the turn.
			if herr := a.hooks.runAfterProviderResponse(ctx, resp); herr != nil {
				return ChatResponse{}, herr
			}
			return resp, nil
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
func (a *Agent) callRung(ctx context.Context, p ModelRung, req ChatRequest, sink StreamSink) (ChatResponse, error) {
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
			resp, err = a.streamTurn(ctx, p.Provider, req, sink)
		} else {
			resp, err = p.Provider.Chat(ctx, req)
		}
		if err == nil {
			return resp, nil
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
func (a *Agent) streamTurn(ctx context.Context, provider LLMProvider, req ChatRequest, sink StreamSink) (ChatResponse, error) {
	ch, err := provider.Stream(ctx, req)
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
		}
		if d.ToolCall != nil {
			msg.ToolCalls = append(msg.ToolCalls, *d.ToolCall)
		}
		if d.Done {
			resp.StopReason = d.StopReason
			resp.Usage = d.Usage
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

// lastAssistantText returns the content of the most recent assistant message.
func lastAssistantText(messages []Message) string {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == RoleAssistant && messages[i].Content != "" {
			return messages[i].Content
		}
	}
	return ""
}
