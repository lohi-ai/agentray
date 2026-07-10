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

// TestPricingChargesCacheTokensSeparately verifies cache reads bill at the
// discounted rate and cache writes at the premium, instead of the full input
// rate — the whole point of the long-session caching steal.
func TestPricingChargesCacheTokensSeparately(t *testing.T) {
	p := Pricing{"m": {InputPerM: 10, OutputPerM: 30}}

	full := p.Cost("m", Usage{InputTokens: 1_000_000})
	if full != 10 {
		t.Fatalf("full input cost = %v, want 10", full)
	}
	// A million cache-read tokens must cost a fraction of a million fresh ones.
	read := p.Cost("m", Usage{CacheReadTokens: 1_000_000})
	if read != 10*defaultCacheReadMultiplier {
		t.Fatalf("cache-read cost = %v, want %v", read, 10*defaultCacheReadMultiplier)
	}
	// Cache writes carry a small premium over fresh input.
	write := p.Cost("m", Usage{CacheWriteTokens: 1_000_000})
	if write != 10*defaultCacheWriteMultiplier {
		t.Fatalf("cache-write cost = %v, want %v", write, 10*defaultCacheWriteMultiplier)
	}
	// Explicit cache rates override the derived defaults.
	pe := Pricing{"m": {InputPerM: 10, OutputPerM: 30, CacheReadPerM: 0.5}}
	if got := pe.Cost("m", Usage{CacheReadTokens: 1_000_000}); got != 0.5 {
		t.Fatalf("explicit cache-read cost = %v, want 0.5", got)
	}
}

// TestOpenAIUsageNormalizesCachedTokens verifies the OpenAI adapter pulls cached
// tokens out of prompt_tokens so InputTokens stays full-price-only and the cached
// portion is reported separately.
func TestOpenAIUsageNormalizesCachedTokens(t *testing.T) {
	u := oaiUsage{PromptTokens: 1000, CompletionTokens: 200}
	u.PromptTokensDetails.CachedTokens = 800
	got := u.usage()
	if got.InputTokens != 200 {
		t.Fatalf("InputTokens = %d, want 200 (1000 total - 800 cached)", got.InputTokens)
	}
	if got.CacheReadTokens != 800 {
		t.Fatalf("CacheReadTokens = %d, want 800", got.CacheReadTokens)
	}
	if got.OutputTokens != 200 {
		t.Fatalf("OutputTokens = %d, want 200", got.OutputTokens)
	}
}

// TestAnthropicUsageMapsCacheCounters verifies the Anthropic adapter maps its
// two cache counters onto the neutral read/write fields without touching
// input_tokens (which already excludes the cached prefix).
func TestAnthropicUsageMapsCacheCounters(t *testing.T) {
	got := antUsage{
		InputTokens:              50,
		OutputTokens:             10,
		CacheReadInputTokens:     400,
		CacheCreationInputTokens: 120,
	}.usage()
	want := Usage{InputTokens: 50, OutputTokens: 10, CacheReadTokens: 400, CacheWriteTokens: 120}
	if got != want {
		t.Fatalf("usage = %+v, want %+v", got, want)
	}
}

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
		if got := isRetryable(c.err); got != c.want {
			t.Fatalf("isRetryable(%v) = %v, want %v", c.err, got, c.want)
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
