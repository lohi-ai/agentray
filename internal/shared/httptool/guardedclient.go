package httptool

import (
	"net"
	"net/http"
	"time"
)

// NewGuardedClient returns an http.Client that shares this package's SSRF
// backstop — every dial resolves the host and refuses the connection if any
// resolved IP is loopback/private/link-local (the cloud-metadata endpoint) —
// but WITHOUT a host allowlist. It is for outbound delivery to user-configured
// destinations (alert webhooks, Slack incoming-webhooks) where the host set is
// open-ended but the private-network SSRF guard must still hold. Redirects are
// not followed, closing the "3xx bounces to a blocked host" gap.
func NewGuardedClient(timeout time.Duration) *http.Client {
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	return &http.Client{
		Timeout:       timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Transport: &http.Transport{
			DialContext:           guardedDialFunc(dialer, blockedIP),
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: timeout,
			MaxIdleConns:          10,
		},
	}
}
