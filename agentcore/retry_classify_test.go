package agentcore

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// providerErr builds a ProviderError with a real *http.Response so the test
// exercises the same construction path a provider uses.
func providerErr(status int, message string) *ProviderError {
	rec := httptest.NewRecorder()
	rec.WriteHeader(status)
	return NewProviderError("test", rec.Result(), message)
}

// Cloudflare answers from the edge with a 52x when the *origin* is what failed,
// so the edge is healthy and the failure is usually brief. Treating those as
// permanent is not academic: the idle-game benchmark took a 521 and gave up
// without spending a single retry.
func TestIsRetryable_CloudflareOriginErrors(t *testing.T) {
	for _, status := range []int{520, 521, 522, 523, 524} {
		if !IsRetryable(providerErr(status, "error code: "+http.StatusText(status))) {
			t.Errorf("status %d classified as permanent; Cloudflare origin errors are transient", status)
		}
	}
	// 525/526 are TLS handshake failures between edge and origin — a
	// misconfiguration, not a blip, so they must stay non-retryable.
	for _, status := range []int{525, 526} {
		if IsRetryable(providerErr(status, "")) {
			t.Errorf("status %d classified as transient; a TLS misconfiguration will not fix itself", status)
		}
	}
}

// Anthropic signals overload with 529, which is outside the standard 5xx set a
// status switch usually covers.
func TestIsRetryable_AnthropicOverloaded(t *testing.T) {
	if !IsRetryable(providerErr(529, `{"type":"overloaded_error"}`)) {
		t.Fatal("529 overloaded_error classified as permanent")
	}
}

// A spent quota and an ordinary throttle both arrive as 429. Retrying the former
// burns the whole ladder on something only a human can fix, and delays the clear
// error the operator needs to see.
func TestIsRetryable_ExhaustedQuotaOutranksRetryableStatus(t *testing.T) {
	permanent := []string{
		`{"error":{"code":"insufficient_quota","message":"You exceeded your current quota"}}`,
		"429 quota exceeded",
		"Monthly usage limit reached",
		"your available balance is too low",
		"billing hard limit reached",
	}
	for _, msg := range permanent {
		if IsRetryable(providerErr(http.StatusTooManyRequests, msg)) {
			t.Errorf("account limit %q classified as retryable", msg)
		}
	}
	// A plain throttle with no quota wording stays retryable — that is the case
	// the 429 rung exists for.
	if !IsRetryable(providerErr(http.StatusTooManyRequests, "rate limit exceeded, please slow down")) {
		t.Fatal("an ordinary 429 throttle is no longer retryable")
	}
}

// Some gateways wrap an upstream failure in a 200 or a 400, where the status
// describes their own handshake rather than the outcome. The message is then the
// only signal there is.
func TestIsRetryable_MessageFallbackWhenStatusIsUnhelpful(t *testing.T) {
	transient := []string{
		"Provider returned error",
		"upstream connection reset by peer",
		"stream ended before message_stop",
		"truncated SSE response: no content, tool calls, or finish_reason",
		"the model is overloaded, try again",
	}
	for _, msg := range transient {
		if !IsRetryable(providerErr(http.StatusOK, msg)) {
			t.Errorf("transient wording %q classified as permanent", msg)
		}
	}

	// A genuine client error must not be rescued by the fallback: retrying a
	// malformed request reproduces it exactly.
	permanent := []string{
		"invalid_request_error: messages[0].content is required",
		"model `does-not-exist` not found",
		"authentication_error: invalid x-api-key",
	}
	for _, msg := range permanent {
		if IsRetryable(providerErr(http.StatusBadRequest, msg)) {
			t.Errorf("client error %q classified as retryable", msg)
		}
	}
}

// The existing contract must not regress: a transport failure has no response at
// all, and that is the case the whole Status-0 convention exists to carry.
func TestIsRetryable_TransportAndStandardStatusesUnchanged(t *testing.T) {
	if !IsRetryable(NewProviderError("test", nil, "connection severed")) {
		t.Fatal("a Status 0 transport failure is no longer retryable")
	}
	for _, status := range []int{408, 429, 500, 502, 503, 504} {
		if !IsRetryable(providerErr(status, "")) {
			t.Errorf("status %d is no longer retryable", status)
		}
	}
	for _, status := range []int{400, 401, 403, 404, 422} {
		if IsRetryable(providerErr(status, "")) {
			t.Errorf("client error %d became retryable", status)
		}
	}
	if IsRetryable(nil) {
		t.Fatal("nil error is retryable")
	}
	if IsRetryable(errors.New("some unclassified failure")) {
		t.Fatal("a bare error became retryable; providers must build ProviderError")
	}
}

// The quota override has to survive wrapping, since the loop sees errors that
// have been annotated with turn context on the way up.
func TestIsRetryable_ClassifiesThroughWrappedErrors(t *testing.T) {
	wrapped := errors.Join(errors.New("provider chat (turn 4)"),
		providerErr(http.StatusTooManyRequests, "insufficient_quota"))
	if IsRetryable(wrapped) {
		t.Fatal("wrapped quota error classified as retryable")
	}
	wrapped = errors.Join(errors.New("provider chat (turn 4)"), providerErr(521, ""))
	if !IsRetryable(wrapped) {
		t.Fatal("wrapped Cloudflare 521 classified as permanent")
	}
}
