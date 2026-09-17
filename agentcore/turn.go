package agentcore

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

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
func (a *Agent) reason(ctx context.Context, req ChatRequest, sink StreamSink, ladder []ModelRung, rung *int, frame func(snapshot string)) (ChatResponse, error) {
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

		resp, err := a.callRung(ctx, p, req, sink, frame)
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
func (a *Agent) callRung(ctx context.Context, p ModelRung, req ChatRequest, sink StreamSink, frame func(snapshot string)) (ChatResponse, error) {
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
			resp, err = a.streamTurn(ctx, p.Provider, req, sink, frame)
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
// the non-streaming turn. frame, when non-nil, receives a throttled snapshot of
// the text accumulated SO FAR — first delta, then per frameBytes — for the
// durable partial-frame record; it is per-attempt, so a retried stream starts
// its snapshots fresh rather than mixing two attempts' text.
func (a *Agent) streamTurn(ctx context.Context, provider LLMProvider, req ChatRequest, sink StreamSink, frame func(snapshot string)) (ChatResponse, error) {
	ch, err := provider.Stream(ctx, req)
	if err != nil {
		return ChatResponse{}, err
	}
	msg := Message{Role: RoleAssistant}
	var resp ChatResponse
	sinceFrame := 0
	for d := range ch {
		if d.Err != nil {
			return ChatResponse{}, d.Err
		}
		if d.ContentDelta != "" {
			msg.Content += d.ContentDelta
			sink(StreamEvent{Type: StreamToken, Token: d.ContentDelta})
			if frame != nil {
				sinceFrame += len(d.ContentDelta)
				if msg.Content == d.ContentDelta || sinceFrame >= frameBytes {
					frame(msg.Content)
					sinceFrame = 0
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

// isTruncatedStop reports whether the provider ended the response at its output
// limit rather than by choice. Stop reasons pass through vendor-verbatim, so
// this is a set: OpenAI-wire "length", Anthropic's "max_tokens", Codex's
// "incomplete". A truncated message's tool calls may carry silently incomplete
// arguments and are never executed (see the dispatch guard in loop.go).
func isTruncatedStop(reason string) bool {
	switch reason {
	case "length", "max_tokens", "incomplete":
		return true
	}
	return false
}

// ProviderError is the typed error a provider returns for an HTTP-level failure,
// so the loop can classify it (retryable status? honor Retry-After?) rather than
// string-matching a bare error (pi's isRetryableError, made structural where a
// status exists). A nil/zero Status marks a transport-level failure with no HTTP
// response.
type ProviderError struct {
	Provider   string        // provider name ("openai", "anthropic")
	Status     int           // HTTP status; 0 for a transport failure
	RetryAfter time.Duration // parsed Retry-After header; 0 if absent
	Message    string        // server-supplied detail
}

func (e *ProviderError) Error() string {
	if e.Status > 0 {
		if e.Message != "" {
			return fmt.Sprintf("%s: unexpected response (status %d): %s", e.Provider, e.Status, e.Message)
		}
		return fmt.Sprintf("%s: unexpected response (status %d)", e.Provider, e.Status)
	}
	if e.Message != "" {
		return fmt.Sprintf("%s: %s", e.Provider, e.Message)
	}
	return e.Provider + ": provider error"
}

// NewProviderError builds a ProviderError from an HTTP response, parsing
// Retry-After so the loop can pace a 429/503 backoff to the server's hint.
//
// It is exported because it is the contract between a provider and the loop's
// retry/escalation logic: IsRetryable classifies structurally first (status
// code, Retry-After), falling back to the message only where the status carries
// no signal. A provider that returns a plain error is silently un-retryable, so
// any provider — in this repo or out of it — builds its transport failures with
// this.
func NewProviderError(provider string, resp *http.Response, message string) *ProviderError {
	pe := &ProviderError{Provider: provider, Message: message}
	if resp != nil {
		pe.Status = resp.StatusCode
		pe.RetryAfter = parseRetryAfter(resp.Header.Get("Retry-After"))
	}
	return pe
}

// parseRetryAfter reads a Retry-After header in either form (delay-seconds or an
// HTTP date). An unparseable or absent value yields 0.
func parseRetryAfter(v string) time.Duration {
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil {
		if secs < 0 {
			return 0
		}
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

// exhaustedLimitPattern marks an account-level limit that no amount of retrying
// will clear: a spent quota, an empty balance, a billing problem. These arrive
// wearing the SAME 429 as an ordinary throttle, so status alone cannot tell them
// apart and a status-only classifier burns the whole ladder on a failure that is
// permanent until a human tops the account up. Checked before anything else, so
// it also overrides a retryable status. (pi's
// NON_RETRYABLE_PROVIDER_LIMIT_ERROR_PATTERN.)
var exhaustedLimitPattern = regexp.MustCompile(
	`(?i)insufficient_quota|quota exceeded|out of budget|billing|` +
		`monthly usage limit reached|available balance|usage limit`)

// retryableMessagePattern catches transient failures whose status code is
// useless — a gateway that wraps an upstream blip in a 200, or a transport error
// with no HTTP response at all. Deliberately narrower than pi's list: it covers
// the wordings actually observed against this stack rather than every provider
// pi supports, because a false positive here retries something permanent.
var retryableMessagePattern = regexp.MustCompile(
	`(?i)overloaded|rate.?limit|too many requests|service.?unavailable|` +
		`server.?error|internal.?error|provider.?returned.?error|` +
		`connection reset|connection refused|other side closed|fetch failed|` +
		`socket hang up|stream ended before|ended without|truncated`)

// retryableStatus reports whether an HTTP status is worth another attempt.
//
// Beyond the standard transient set this includes two ranges that a status-only
// switch misses. 520-524 are Cloudflare's origin-side codes (520 unknown error,
// 521 origin down, 522 timeout, 523 unreachable, 524 origin timeout): the edge
// is healthy and answering, so the failure is upstream of it and typically
// brief. 529 is Anthropic's "overloaded". Omitting these is not academic — the
// idle-game benchmark took a 521 and gave up without a single retry.
func retryableStatus(status int) bool {
	switch status {
	case http.StatusRequestTimeout, http.StatusTooManyRequests,
		http.StatusInternalServerError, http.StatusBadGateway,
		http.StatusServiceUnavailable, http.StatusGatewayTimeout,
		529: // Anthropic overloaded_error
		return true
	}
	// Cloudflare origin-side range.
	return status >= 520 && status <= 524
}

// IsRetryable reports whether err is a transient failure worth retrying the same
// model: a rate limit (429), a server-side 5xx (500/502/503/504), a Cloudflare
// origin error (520-524), an Anthropic overload (529), a request timeout (408),
// or a transport-level network blip. A cancellation is never retryable — the
// caller guards on ctx.Err() — and a client error (4xx other than 408/429) won't
// be fixed by retrying, so it falls through to escalation.
//
// An exhausted quota or a billing failure is never retryable even when it wears
// a retryable status, because the account, not the network, is what is broken.
//
// Exported alongside NewProviderError as the other half of the provider↔loop
// retry contract: a provider builds the error, the loop classifies it, and an
// out-of-tree provider can assert its own transport failures land on the right
// side of that line.
func IsRetryable(err error) bool {
	if err == nil {
		return false
	}
	var pe *ProviderError
	if errors.As(err, &pe) {
		// An account limit is permanent until a human acts on it; it outranks
		// both the status and the transient-wording check below.
		if exhaustedLimitPattern.MatchString(pe.Message) {
			return false
		}
		if pe.Status == 0 {
			return true // transport failure captured as a ProviderError
		}
		if retryableStatus(pe.Status) {
			return true
		}
		// The status was unhelpful (a gateway 200 wrapping an upstream failure,
		// say); fall back to what the provider actually said.
		return retryableMessagePattern.MatchString(pe.Message)
	}
	// Transport-level failures (connection reset, DNS, timeouts) arrive as
	// *url.Error / net.Error from the HTTP client; these are worth a retry.
	var ne net.Error
	if errors.As(err, &ne) {
		return true
	}
	var ue *url.Error
	if errors.As(err, &ue) {
		return true
	}
	return false
}

// retryAfterOf extracts a server-supplied Retry-After from a provider error, if
// any, so the backoff can honor it instead of the exponential schedule.
func retryAfterOf(err error) time.Duration {
	var pe *ProviderError
	if errors.As(err, &pe) {
		return pe.RetryAfter
	}
	return 0
}

// RetryPolicy bounds the same-model retry of a transient provider failure before
// the loop escalates down the model ladder. It is per-rung: each rung gets its
// own attempt budget, so a flaky rung is retried in place (cheap) before paying
// to escalate to a pricier one.
type RetryPolicy struct {
	MaxAttempts int           // total attempts per rung, including the first (>=1)
	BaseDelay   time.Duration // first backoff; doubles each attempt
	MaxDelay    time.Duration // cap on any single backoff
}

// DefaultRetryPolicy is a conservative same-rung backoff: three attempts with
// exponential delay capped at a few seconds, enough to ride out a brief 429/503
// without stalling a run.
func DefaultRetryPolicy() RetryPolicy {
	return RetryPolicy{MaxAttempts: 3, BaseDelay: 500 * time.Millisecond, MaxDelay: 8 * time.Second}
}

// normalized fills any zero field from the default so a partial override is safe.
func (rp RetryPolicy) normalized() RetryPolicy {
	d := DefaultRetryPolicy()
	if rp.MaxAttempts <= 0 {
		rp.MaxAttempts = d.MaxAttempts
	}
	if rp.BaseDelay <= 0 {
		rp.BaseDelay = d.BaseDelay
	}
	if rp.MaxDelay <= 0 {
		rp.MaxDelay = d.MaxDelay
	}
	return rp
}

// delay computes the backoff before attempt n (0-based: the wait before the
// first retry uses n=0). A server Retry-After wins when present; otherwise it is
// exponential with equal jitter, capped at MaxDelay.
func (rp RetryPolicy) delay(n int, retryAfter time.Duration) time.Duration {
	if retryAfter > 0 {
		if retryAfter > rp.MaxDelay {
			return rp.MaxDelay
		}
		return retryAfter
	}
	d := rp.BaseDelay << n
	if d <= 0 || d > rp.MaxDelay {
		d = rp.MaxDelay
	}
	// Equal jitter: half the window fixed, half random, to de-correlate retries.
	half := d / 2
	return half + time.Duration(rand.Int63n(int64(half)+1))
}

// sleepBackoff waits d, returning early with ctx.Err() if the run is cancelled
// during the wait — a backoff must never outlive the context it serves.
func sleepBackoff(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
