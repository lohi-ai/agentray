package agentcore

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

// --- #1: cache-aware accounting ---
//
// This file keeps the LOOP's half: that cache token counts accumulate across
// turns and that retry/escalation layer correctly. Pricing moved to
// agentcore/plugins/observe (Monitor), and the per-vendor usage normalization
// that feeds both moved to ai/ with the wire code.

// TestRunSumsCacheTokens verifies the loop accumulates cache tokens across turns
// into the run total, so a consumer sees honest cache accounting end-to-end.
func TestRunSumsCacheTokens(t *testing.T) {
	r1 := AssistantText("done")
	r1.Usage = Usage{InputTokens: 5, OutputTokens: 3, CacheReadTokens: 100, CacheWriteTokens: 20}
	agent, err := New(Config{Provider: NewFauxProvider(r1), Model: "test"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	res, err := agent.Prompt(context.Background(), "go")
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if res.Usage.CacheReadTokens != 100 || res.Usage.CacheWriteTokens != 20 {
		t.Fatalf("run cache usage = read %d / write %d, want 100 / 20",
			res.Usage.CacheReadTokens, res.Usage.CacheWriteTokens)
	}
}

// --- #2: same-rung retry with backoff ---

// flakyProvider fails with a retryable ProviderError for the first failN calls,
// then succeeds. It counts every Chat attempt so a test can assert the retry took
// place on the same rung.
type flakyProvider struct {
	failN  int32
	calls  int32
	status int
}

func (f *flakyProvider) Name() string        { return "flaky" }
func (f *flakyProvider) SupportsTools() bool { return true }
func (f *flakyProvider) Chat(context.Context, ChatRequest) (ChatResponse, error) {
	n := atomic.AddInt32(&f.calls, 1)
	if n <= atomic.LoadInt32(&f.failN) {
		return ChatResponse{}, &ProviderError{Provider: "flaky", Status: f.status}
	}
	return AssistantText("recovered"), nil
}
func (f *flakyProvider) Stream(context.Context, ChatRequest) (<-chan ChatDelta, error) {
	return nil, errors.New("unused")
}

// fastRetry is a tiny backoff so retry tests don't sleep for real.
func fastRetry() *RetryPolicy {
	return &RetryPolicy{MaxAttempts: 3, BaseDelay: time.Millisecond, MaxDelay: 2 * time.Millisecond}
}

// TestSameRungRetrySucceedsAfterTransientError verifies a 503 on the same model
// is retried with backoff and recovers, without escalating or aborting.
func TestSameRungRetrySucceedsAfterTransientError(t *testing.T) {
	prov := &flakyProvider{failN: 2, status: http.StatusServiceUnavailable}
	agent, err := New(Config{Provider: prov, Model: "test", Retry: fastRetry()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	res, err := agent.Prompt(context.Background(), "go")
	if err != nil {
		t.Fatalf("Prompt should recover after retries: %v", err)
	}
	if res.Final != "recovered" {
		t.Fatalf("final = %q, want recovered", res.Final)
	}
	if got := atomic.LoadInt32(&prov.calls); got != 3 {
		t.Fatalf("provider called %d times, want 3 (2 failures + 1 success)", got)
	}
}

// TestRetryExhaustionSurfacesError verifies that once the per-rung attempt budget
// is spent on a persistently failing model (and there is no ladder to escalate
// to), the error surfaces — the loop doesn't retry forever.
func TestRetryExhaustionSurfacesError(t *testing.T) {
	prov := &flakyProvider{failN: 99, status: http.StatusTooManyRequests}
	agent, err := New(Config{Provider: prov, Model: "test", Retry: fastRetry()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := agent.Prompt(context.Background(), "go"); err == nil {
		t.Fatalf("a persistently failing provider must surface an error")
	}
	if got := atomic.LoadInt32(&prov.calls); got != 3 {
		t.Fatalf("provider called %d times, want 3 (MaxAttempts)", got)
	}
}

// TestNonRetryableErrorSkipsRetry verifies a client error (400) is not retried —
// it can't be fixed by trying the same model again, so it surfaces on the first
// attempt.
func TestNonRetryableErrorSkipsRetry(t *testing.T) {
	prov := &flakyProvider{failN: 99, status: http.StatusBadRequest}
	agent, err := New(Config{Provider: prov, Model: "test", Retry: fastRetry()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := agent.Prompt(context.Background(), "go"); err == nil {
		t.Fatalf("a 400 must surface as an error")
	}
	if got := atomic.LoadInt32(&prov.calls); got != 1 {
		t.Fatalf("provider called %d times, want 1 (no retry on non-retryable)", got)
	}
}

// TestRetryThenEscalate verifies the layering: a rung's retries are spent first,
// and only then does the loop escalate to the next rung, which succeeds.
func TestRetryThenEscalate(t *testing.T) {
	bad := &flakyProvider{failN: 99, status: http.StatusServiceUnavailable}
	good := NewFauxProvider(AssistantText("from rung 2"))
	agent, err := New(Config{
		Provider:   bad,
		Model:      "rung1",
		Retry:      fastRetry(),
		Escalation: []ModelRung{{Provider: good, Model: "rung2"}},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	res, err := agent.Prompt(context.Background(), "go")
	if err != nil {
		t.Fatalf("should escalate to a working rung: %v", err)
	}
	if res.Final != "from rung 2" {
		t.Fatalf("final = %q, want 'from rung 2'", res.Final)
	}
	if got := atomic.LoadInt32(&bad.calls); got != 3 {
		t.Fatalf("rung 1 retried %d times, want 3 before escalating", got)
	}
}

// TestRetryClassification spot-checks the retryable/non-retryable split that
// gates same-rung retry.
func TestRetryClassification(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{&ProviderError{Status: http.StatusTooManyRequests}, true},
		{&ProviderError{Status: http.StatusServiceUnavailable}, true},
		{&ProviderError{Status: http.StatusBadGateway}, true},
		{&ProviderError{Status: 0}, true}, // transport failure
		{&ProviderError{Status: http.StatusBadRequest}, false},
		{&ProviderError{Status: http.StatusUnauthorized}, false},
		{errors.New("plain"), false},
		{nil, false},
	}
	for _, c := range cases {
		if got := IsRetryable(c.err); got != c.want {
			t.Fatalf("IsRetryable(%v) = %v, want %v", c.err, got, c.want)
		}
	}
}

// TestParseRetryAfter verifies the Retry-After parser handles the delay-seconds
// form (the common case) and ignores garbage.
func TestParseRetryAfter(t *testing.T) {
	if got := parseRetryAfter("5"); got != 5*time.Second {
		t.Fatalf("parseRetryAfter(5) = %v, want 5s", got)
	}
	if got := parseRetryAfter(""); got != 0 {
		t.Fatalf("parseRetryAfter(empty) = %v, want 0", got)
	}
	if got := parseRetryAfter("garbage"); got != 0 {
		t.Fatalf("parseRetryAfter(garbage) = %v, want 0", got)
	}
}
