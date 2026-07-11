package agentcore

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// A 5xx from a gateway frequently carries a non-JSON body (an HTML/text error
// page). Chat() must classify by HTTP status BEFORE decoding, so the failure
// surfaces as a retryable *ProviderError rather than a plain (non-retryable)
// decode error — otherwise a transient outage permanently drops the turn.
func TestChat_Non2xxWithNonJSONBodyIsRetryableProviderError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("<html><body>error: 502 Bad Gateway</body></html>"))
	}))
	defer srv.Close()

	p := NewOpenAIProvider("k", srv.URL, DefaultCompat())
	_, err := p.Chat(context.Background(), ChatRequest{Model: "m", Messages: []Message{{Role: RoleUser, Content: "hi"}}})
	if err == nil {
		t.Fatal("expected an error on a 502 response")
	}

	var pe *ProviderError
	if !errors.As(err, &pe) {
		t.Fatalf("error is not a *ProviderError (retry classification would miss it): %v", err)
	}
	if pe.Status != http.StatusBadGateway {
		t.Fatalf("ProviderError.Status = %d, want 502", pe.Status)
	}
	if !isRetryable(err) {
		t.Fatalf("a 502 must be retryable, got isRetryable=false for %v", err)
	}
}

// A structured JSON error body (well-formed 4xx/5xx) still surfaces its message,
// and the status still drives retry classification: a 400 is a ProviderError but
// not retryable, a 429 is retryable.
func TestChat_JSONErrorBodyClassifiedByStatus(t *testing.T) {
	cases := []struct {
		status      int
		wantRetry   bool
		wantMessage string
	}{
		{http.StatusBadRequest, false, "bad model"},
		{http.StatusTooManyRequests, true, "slow down"},
	}
	for _, c := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(c.status)
			_, _ = w.Write([]byte(`{"error":{"message":"` + c.wantMessage + `"}}`))
		}))
		p := NewOpenAIProvider("k", srv.URL, DefaultCompat())
		_, err := p.Chat(context.Background(), ChatRequest{Model: "m", Messages: []Message{{Role: RoleUser, Content: "hi"}}})
		srv.Close()

		var pe *ProviderError
		if !errors.As(err, &pe) {
			t.Fatalf("status %d: not a *ProviderError: %v", c.status, err)
		}
		if pe.Status != c.status {
			t.Fatalf("status %d: ProviderError.Status = %d", c.status, pe.Status)
		}
		if pe.Message != c.wantMessage {
			t.Fatalf("status %d: message = %q, want %q", c.status, pe.Message, c.wantMessage)
		}
		if isRetryable(err) != c.wantRetry {
			t.Fatalf("status %d: isRetryable = %v, want %v", c.status, isRetryable(err), c.wantRetry)
		}
	}
}
