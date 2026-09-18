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

// committedStreamError marks a provider stream that failed after content was
// already delivered to the caller's sink. Retrying that request — on the same
// model or an escalation rung — is no longer replay-safe: the replacement
// answer would be appended to text the caller has already rendered. The cause
// stays wrapped for errors.Is/errors.As, and usage preserves whatever the
// failed attempt reported so a partial response is not free.
type committedStreamError struct {
	cause error
	usage Usage
}

func (e *committedStreamError) Error() string {
	return fmt.Sprintf("provider stream failed after emitting output: %v", e.cause)
}

func (e *committedStreamError) Unwrap() error { return e.cause }

// maxStreamAbortsPerTurn backstops a StreamInterceptor that never stops
// matching: past it the turn's provider call is retried once with interception
// disabled, so the stream completes unmodified. A guard that can spin the
// provider forever is an availability bug wearing a policy costume — the
// plugin's own per-turn cap is the real bound; this only covers a broken one.
const maxStreamAbortsPerTurn = 8

// unknownProviderImageLimit is the portable floor for an image-capable path
// whose vendor metadata does not publish a request-wide image cap. It matches
// the strictest mainstream compatible endpoints seen in OMP's provider budget;
// known adapters advertise their larger limits through ModelCapabilities.
const unknownProviderImageLimit = 5

// ModelCapabilityError reports a request that a provider/model pair is known
// not to accept. It is intentionally non-retryable: repeating the same rung
// cannot add a missing capability, while reason may still advance to a capable
// fallback rung.
type ModelCapabilityError struct {
	Provider string
	Model    string
	Feature  string
}

func (e *ModelCapabilityError) Error() string {
	return fmt.Sprintf("provider %s model %s does not support %s", e.Provider, e.Model, e.Feature)
}

// requestForCapabilities applies only explicit negative capability knowledge.
// Unknown means preserve the request, which keeps arbitrary compatible/local
// endpoints backward compatible. Provider hints may be removed here without
// weakening loop-owned enforcement (for example OutputSchema is still checked
// by Agent.outputValidator after the response).
func requestForCapabilities(provider LLMProvider, model string, discovered ModelCapabilities, req ChatRequest) (ChatRequest, error) {
	caps := CapabilitiesOf(provider, model).Overlay(discovered)
	if len(req.Tools) > 0 && caps.Tools == CapabilityUnsupported {
		return ChatRequest{}, &ModelCapabilityError{Provider: provider.Name(), Model: model, Feature: "native tool calls"}
	}
	if caps.ReasoningEffort == CapabilityUnsupported {
		req.ReasoningEffort = ""
	}
	if caps.StructuredOutput == CapabilityUnsupported {
		req.OutputSchema = nil
	}
	if caps.PromptCaching == CapabilityUnsupported {
		req.CacheKey = ""
		req.CacheRetention = ""
	}
	if caps.ImageInput == CapabilityUnsupported {
		req.Messages = withoutImageContent(req.Messages)
	} else {
		limit := caps.MaxInputImages
		if limit <= 0 {
			limit = unknownProviderImageLimit
		}
		req.Messages = clampImageContent(req.Messages, limit)
	}
	return req, nil
}

// clampImageContent drops the oldest image parts until the complete outgoing
// request fits the active provider/model limit. It is copy-on-write: escalation
// to a later rung starts from the unmodified canonical transcript and may apply
// a different limit. Text parts and tool-call linkage stay intact, and every
// affected message gets a visible breadcrumb instead of losing images silently.
func clampImageContent(messages []Message, limit int) []Message {
	if limit < 0 {
		limit = 0
	}
	total := 0
	for _, message := range messages {
		for _, part := range message.ContentParts {
			if part.Type == ContentPartImage {
				total++
			}
		}
	}
	drop := total - limit
	if drop <= 0 {
		return messages
	}

	var out []Message
	for i, message := range messages {
		if drop == 0 {
			break
		}
		omitted := 0
		kept := make([]ContentPart, 0, len(message.ContentParts))
		for _, part := range message.ContentParts {
			if part.Type == ContentPartImage && drop > 0 {
				drop--
				omitted++
				continue
			}
			kept = append(kept, part)
		}
		if omitted == 0 {
			continue
		}
		if out == nil {
			out = append([]Message(nil), messages...)
		}
		replacement := message
		replacement.ContentParts = kept
		note := fmt.Sprintf("[%d older image attachment(s) omitted: provider request limit is %d]", omitted, limit)
		if replacement.Content == "" {
			replacement.Content = note
		} else {
			replacement.Content += "\n" + note
		}
		out[i] = replacement
	}
	if out == nil {
		return messages
	}
	return out
}

// withoutImageContent makes an explicit negative image capability visible to
// the model instead of silently dropping tool output. It is copy-on-write so a
// failed rung cannot mutate the request later escalated to a vision-capable
// fallback model.
func withoutImageContent(messages []Message) []Message {
	var out []Message
	for i, message := range messages {
		images := 0
		kept := make([]ContentPart, 0, len(message.ContentParts))
		for _, part := range message.ContentParts {
			if part.Type == ContentPartImage {
				images++
				continue
			}
			kept = append(kept, part)
		}
		if images == 0 {
			continue
		}
		if out == nil {
			out = append([]Message(nil), messages...)
		}
		replacement := message
		replacement.ContentParts = kept
		note := fmt.Sprintf("[%d image output(s) omitted: provider/model path does not support image input]", images)
		if replacement.Content == "" {
			replacement.Content = note
		} else {
			replacement.Content += "\n" + note
		}
		out[i] = replacement
	}
	if out == nil {
		return messages
	}
	return out
}

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
func (a *Agent) reason(ctx context.Context, req ChatRequest, sink StreamSink, streams []StreamInterceptor, ladder []ModelRung, rung *int, frame func(snapshot string)) (ChatResponse, error) {
	for {
		p := ladder[*rung]
		callReq := req
		callReq.Model = p.Model
		callReq, err := requestForCapabilities(p.Provider, p.Model, p.Capabilities, callReq)

		// Re-resolve this rung's API key before the call so an expiring BYO token
		// doesn't kill a long run; applied only when the provider is a KeyUpdater.
		if err == nil && a.refreshKey != nil {
			if key, kerr := a.refreshKey(ctx, p.Provider.Name()); kerr == nil {
				if u, ok := p.Provider.(KeyUpdater); ok {
					u.UpdateAPIKey(key)
				}
			}
		}

		var resp ChatResponse
		if err == nil {
			resp, err = a.callRung(ctx, p, callReq, sink, streams, frame)
		}
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
		var committed *committedStreamError
		if errors.As(err, &abort) || errors.As(err, &committed) {
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
func (a *Agent) callRung(ctx context.Context, p ModelRung, req ChatRequest, sink StreamSink, streams []StreamInterceptor, frame func(snapshot string)) (ChatResponse, error) {
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
			resp, err = a.streamTurn(ctx, p.Provider, req, sink, streams, frame)
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
		var committed *committedStreamError
		if errors.As(err, &abort) || errors.As(err, &committed) {
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
// content fragments to the sink as they arrive when no interceptor is installed
// and holding an intercepted attempt until it is accepted. It accumulates the
// full assistant message (text + tool calls + usage) so the Act path is identical
// to the non-streaming turn. frame, when non-nil, receives a throttled snapshot of
// the text accumulated SO FAR — first delta, then per frameBytes — for the
// durable partial-frame record; it is per-attempt, so a retried stream starts
// its snapshots fresh rather than mixing two attempts' text.
//
// When stream interceptors are installed the provider call runs on a cancelable
// child context: the first interceptor to abort cancels it — cutting the HTTP
// stream mid-token — and the partial message is discarded into the streamAbort
// sentinel. The channel is drained after cancel so a provider that does not
// select on ctx.Done (the scripted test providers) still finishes its goroutine
// instead of leaking it against a full buffer.
func (a *Agent) streamTurn(ctx context.Context, provider LLMProvider, req ChatRequest, sink StreamSink, streams []StreamInterceptor, frame func(snapshot string)) (ChatResponse, error) {
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
	var heldTokens []string
	committed := false
	sinceFrame := 0
	for d := range ch {
		// Some providers attach their last known usage to the error delta. Merge
		// it before inspecting Err so a failed partial stream is still accounted.
		resp.Usage = mergeUsage(resp.Usage, d.Usage)
		if d.Err != nil {
			if committed {
				return ChatResponse{}, &committedStreamError{cause: d.Err, usage: resp.Usage}
			}
			return ChatResponse{}, d.Err
		}
		if d.ContentDelta != "" {
			msg.Content += d.ContentDelta
			if len(streams) > 0 {
				if dec := interceptStreamDelta(ctx, streams, msg.Content); dec.Abort {
					cancel()
					for range ch {
					}
					return ChatResponse{}, &streamAbort{inject: dec.Inject, usage: resp.Usage}
				}
			}
			if len(streams) > 0 {
				// A rule can match a later chunk, after earlier chunks already looked
				// harmless. Hold the attempt until it completes so an aborted draft
				// never leaks a prefix into the visible stream. Streams without rules
				// retain true token-by-token delivery.
				heldTokens = append(heldTokens, d.ContentDelta)
			} else {
				sink(StreamEvent{Type: StreamToken, Token: d.ContentDelta})
				committed = true
			}
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
		if d.Done {
			resp.StopReason = d.StopReason
		}
	}
	for _, token := range heldTokens {
		sink(StreamEvent{Type: StreamToken, Token: token})
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
