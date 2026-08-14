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
	"strconv"
	"time"
)

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
