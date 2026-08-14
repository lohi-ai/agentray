package sandbox

import (
	"context"
	"strings"
	"testing"

	"github.com/lohi-ai/agentray/agentcore"
)

// The whole reason the sandboxed request is built as a curl config on stdin is
// that a resolved {{cred:NAME}} must not be readable from inside the container.
// argv is world-readable via /proc/<pid>/cmdline and env via
// /proc/<pid>/environ, so this asserts the secret appears in neither.
func TestSandboxHTTPKeepsCredentialOffArgvAndEnv(t *testing.T) {
	const secret = "sk-live-not-in-argv"
	stub := &stubSandbox{result: agentcore.SandboxResult{
		Stdout: "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\n\r\n{\"ok\":true}",
	}}
	tool := NewHTTPRequestTool(stub, WithHTTPAllowHosts([]string{"api.example.com"}))

	out, err := tool.Run(context.Background(),
		`{"method":"POST","url":"https://api.example.com/v1/x","headers":{"Authorization":"Bearer `+secret+`"},"body":"{\"a\":1}"}`)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(out, "status: 200 OK") || !strings.Contains(out, `{"ok":true}`) {
		t.Fatalf("output = %q", out)
	}
	for i, arg := range stub.last.Argv {
		if strings.Contains(arg, secret) {
			t.Fatalf("credential leaked into argv[%d] = %q", i, arg)
		}
	}
	for k, v := range stub.last.Env {
		if strings.Contains(v, secret) {
			t.Fatalf("credential leaked into env %s", k)
		}
	}
	if !strings.Contains(stub.last.Stdin, secret) {
		t.Fatal("the credential should reach curl on stdin")
	}
	if !strings.Contains(stub.last.Stdin, `header = "Authorization: Bearer `+secret+`"`) {
		t.Fatalf("curl config missing the header line:\n%s", stub.last.Stdin)
	}
	if !strings.Contains(stub.last.Stdin, `request = "POST"`) {
		t.Fatalf("curl config missing the method:\n%s", stub.last.Stdin)
	}
}

// Egress must be confined to the tool's own allowlist. Handing the container an
// open network would be strictly worse than the host path it replaced, since
// the host path at least re-checks the resolved IP.
func TestSandboxHTTPConfinesEgressToAllowlist(t *testing.T) {
	stub := &stubSandbox{result: agentcore.SandboxResult{Stdout: "HTTP/1.1 204 No Content\r\n\r\n"}}
	tool := NewHTTPRequestTool(stub, WithHTTPAllowHosts([]string{"api.example.com", "cdn.example.com"}))

	if _, err := tool.Run(context.Background(), `{"url":"https://api.example.com/ping"}`); err != nil {
		t.Fatalf("Run: %v", err)
	}
	lim := stub.last.Constraints
	if !lim.Network {
		t.Fatal("the request needs network in the container")
	}
	if strings.Join(lim.NetworkAllow, ",") != "api.example.com,cdn.example.com" {
		t.Fatalf("NetworkAllow = %v, want the tool's allowlist", lim.NetworkAllow)
	}
}

// The allowlist check still happens in-process, before anything is executed: a
// disallowed host must never reach the sandbox at all.
func TestSandboxHTTPStillEnforcesAllowlistBeforeExec(t *testing.T) {
	stub := &stubSandbox{}
	tool := NewHTTPRequestTool(stub, WithHTTPAllowHosts([]string{"api.example.com"}))
	if _, err := tool.Run(context.Background(), `{"url":"https://evil.example/"}`); err == nil {
		t.Fatal("expected the allowlist to reject the host")
	}
	if stub.last.Argv != nil {
		t.Fatal("a disallowed host must not reach the sandbox")
	}
}

// web_fetch has no allowlist of its own, so it must pin the container's egress
// to the host it was asked to fetch rather than leaving the network open.
func TestSandboxWebFetchPinsEgressToRequestedHost(t *testing.T) {
	stub := &stubSandbox{result: agentcore.SandboxResult{
		Stdout: "HTTP/2 200\r\nContent-Type: text/plain\r\n\r\nhello there",
	}}
	out, err := NewWebFetchTool(stub).Run(context.Background(), `{"url":"https://docs.example.org/guide"}`)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if strings.Join(stub.last.Constraints.NetworkAllow, ",") != "docs.example.org" {
		t.Fatalf("NetworkAllow = %v, want just the requested host", stub.last.Constraints.NetworkAllow)
	}
	if !strings.Contains(stub.last.Stdin, "location\n") {
		t.Fatal("web_fetch follows redirects, so the curl config needs `location`")
	}
	// curl's HTTP/2 status line has no reason phrase; the tool synthesizes one so
	// the rendering matches what net/http would have produced on the host path.
	if !strings.Contains(out, "status: 200 OK") || !strings.Contains(out, "hello there") {
		t.Fatalf("output = %q", out)
	}
}

// The default sandbox image is busybox-based and has no curl. That must surface
// as an explicit, actionable error, never as a silent empty body.
func TestSandboxHTTPReportsMissingCurl(t *testing.T) {
	stub := &stubSandbox{result: agentcore.SandboxResult{ExitCode: 127, Stderr: "curl: not found"}}
	tool := NewHTTPRequestTool(stub, WithHTTPAllowHosts([]string{"api.example.com"}))
	_, err := tool.Run(context.Background(), `{"url":"https://api.example.com/x"}`)
	if err == nil || !strings.Contains(err.Error(), "no curl") {
		t.Fatalf("error = %v, want an explicit missing-curl message", err)
	}
}

// curl config values are double-quoted with a small escape set; a body carrying
// a quote, a backslash or a newline has to survive it intact.
func TestCurlQuoteEscapes(t *testing.T) {
	got := curlQuote("a\"b\\c\nd\te")
	want := `"a\"b\\c\nd\te"`
	if got != want {
		t.Fatalf("curlQuote = %s, want %s", got, want)
	}
}

// curl --include prints a header block per hop. Only the last one describes the
// response the model should see, and the body must not absorb the earlier ones.
func TestParseCurlResponseSkipsInterimBlocks(t *testing.T) {
	raw := "HTTP/1.1 100 Continue\r\n\r\n" +
		"HTTP/1.1 301 Moved Permanently\r\nLocation: https://b.example/\r\n\r\n" +
		"HTTP/1.1 200 OK\r\nContent-Type: text/html\r\n\r\n<html>body</html>"
	resp, body, err := parseCurlResponse(raw, 0)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if resp.StatusCode != 200 || resp.Status != "200 OK" {
		t.Fatalf("status = %d %q, want 200 OK", resp.StatusCode, resp.Status)
	}
	if resp.Header.Get("Content-Type") != "text/html" {
		t.Fatalf("content-type = %q", resp.Header.Get("Content-Type"))
	}
	if string(body) != "<html>body</html>" {
		t.Fatalf("body = %q", body)
	}
}

func TestParseCurlResponseTruncatesBody(t *testing.T) {
	raw := "HTTP/1.1 200 OK\r\n\r\n" + strings.Repeat("x", 100)
	_, body, err := parseCurlResponse(raw, 10)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(body) != 10 {
		t.Fatalf("body length = %d, want 10", len(body))
	}
}

// Refusing to run without an egress allowlist is the fail-closed default: an
// empty list means "open network" to the backend.
func TestSandboxHTTPRefusesEmptyAllowlist(t *testing.T) {
	_, _, err := execSandboxHTTP(context.Background(), &stubSandbox{}, "http_request", sandboxHTTPRequest{
		URL: "https://example.com/",
	})
	if err == nil || !strings.Contains(err.Error(), "egress allowlist") {
		t.Fatalf("error = %v, want a refusal to run without an allowlist", err)
	}
}
