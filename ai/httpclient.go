package ai

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"time"
)

// Timeouts for the chat providers. These two knobs do different jobs, and
// collapsing them into one is what broke the collision/idle-game benchmarks.
//
// Go's http.Client.Timeout is absolute: it covers dialing, the response
// headers, AND the full body read. For a chat provider the body IS the model
// generating — a turn that streams a complete HTML page can legitimately spend
// minutes there. Sizing that cap like a handshake (the old 120s) does not
// protect against a dead gateway; it kills the slowest *healthy* turns, and
// because the kill is deterministic the retry ladder then reproduces it on
// every attempt (3x120s of dead wall clock before the turn finally fails).
//
// So the absolute cap is sized for the slowest legitimate turn, and the
// liveness check moves to ResponseHeaderTimeout — time to first response
// header, which is genuinely a handshake and completes before generation
// starts. A dead endpoint still fails in a minute; a slow one is left alone.
const (
	// DefaultChatTimeout bounds one chat exchange end to end, body included.
	DefaultChatTimeout = 10 * time.Minute
	// DefaultResponseHeaderTimeout bounds the wait for response headers — the
	// "is this gateway alive" check the absolute cap was mistakenly doing.
	DefaultResponseHeaderTimeout = 60 * time.Second
)

// NewChatHTTPClient builds the HTTP client the chat providers use: a generous
// absolute deadline for long generations plus a short header deadline so an
// unresponsive endpoint still fails fast. A zero timeout uses
// DefaultChatTimeout.
//
// Exported so a caller that knows its own latency envelope can size the cap
// itself and assign it to the provider's HTTP field, rather than inheriting a
// default chosen for the slowest case.
func NewChatHTTPClient(timeout time.Duration) *http.Client {
	if timeout <= 0 {
		timeout = DefaultChatTimeout
	}
	// Clone the stock transport so connection pooling, proxy support and HTTP/2
	// stay at their defaults; only the header deadline is ours.
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.ResponseHeaderTimeout = DefaultResponseHeaderTimeout
	return &http.Client{Timeout: timeout, Transport: tr}
}

// describeReadErr names which deadline actually killed a body read. Go reports
// both as the same string — "context deadline exceeded (Client.Timeout or
// context cancellation while reading body)" — which is useless when you are
// trying to work out whether the caller gave up or we hung up on a healthy
// stream. The two have opposite fixes, so say which one it was, and for our own
// deadline say what it was set to.
func describeReadErr(ctx context.Context, client *http.Client, readErr error) string {
	if readErr == nil {
		return ""
	}
	// The caller's context wins: if it is already done, its deadline is what the
	// transport observed, whatever the client cap happens to be.
	if cerr := ctx.Err(); cerr != nil {
		return fmt.Sprintf("caller context ended mid-body (%v)", cerr)
	}
	if isTimeout(readErr) && client != nil && client.Timeout > 0 {
		return fmt.Sprintf("client timeout of %s elapsed mid-body; raise it for long "+
			"generations (ai.NewChatHTTPClient) — the model was still streaming", client.Timeout)
	}
	return readErr.Error()
}

// isTimeout reports whether err is a deadline expiry rather than a severed
// connection, checking the net.Error contract and os.ErrDeadlineExceeded, which
// is what Client.Timeout surfaces on a body read.
func isTimeout(err error) bool {
	if errors.Is(err, os.ErrDeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var ne interface{ Timeout() bool }
	return errors.As(err, &ne) && ne.Timeout()
}
