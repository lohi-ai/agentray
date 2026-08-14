package httptool

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
	"unicode"

	"golang.org/x/net/html"

	"github.com/lohi-ai/agentray/agentcore"
)

// ToolWebFetch is the stable name of the open web-fetch tool. Like http_request
// it must be permitted by policy before the model sees it.
const ToolWebFetch = "web_fetch"

const (
	webFetchTimeout      = 20 * time.Second
	webFetchMaxBodyBytes = 512 * 1024
	webFetchMaxRedirects = 5
)

// WebFetchTool fetches an arbitrary public URL and returns its readable text
// (Claude Code's WebFetch). It is the open-egress counterpart to http_request:
// where http_request is host-allowlisted for talking to specific APIs, web_fetch
// is meant for reading the open web, so it has no host allowlist. SSRF is still
// closed off at the IP layer — the same guarded dialer as http_request re-checks
// every resolved address (including each redirect hop) and refuses
// loopback / private / link-local / metadata, so "no allowlist" does not mean
// "can reach internal services". HTML is reduced to text to keep results small.
type WebFetchTool struct {
	client       *http.Client
	maxBodyBytes int64
}

// NewWebFetch builds a WebFetchTool with the SSRF-guarded dialer installed. It
// follows a bounded number of redirects because every hop is re-validated by the
// dialer at connect time, unlike http_request which cannot (its allowlist can't
// re-check a redirected host).
func NewWebFetch() *WebFetchTool {
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	return &WebFetchTool{
		maxBodyBytes: webFetchMaxBodyBytes,
		client: &http.Client{
			Timeout: webFetchTimeout,
			CheckRedirect: func(_ *http.Request, via []*http.Request) error {
				if len(via) >= webFetchMaxRedirects {
					return fmt.Errorf("stopped after %d redirects", webFetchMaxRedirects)
				}
				return nil
			},
			Transport: &http.Transport{
				DialContext:           guardedDialFunc(dialer, blockedIP),
				TLSHandshakeTimeout:   10 * time.Second,
				ResponseHeaderTimeout: webFetchTimeout,
				MaxIdleConns:          10,
			},
		},
	}
}

func (t *WebFetchTool) Name() string   { return ToolWebFetch }
func (t *WebFetchTool) Parallel() bool { return true }

func (t *WebFetchTool) Schema() agentcore.ToolSchema {
	return agentcore.ToolSchema{
		Name: ToolWebFetch,
		Description: "Fetch a public web page over HTTPS and return its readable text content. " +
			"HTML is stripped to text; non-HTML text is returned as-is. Use this to read documentation, " +
			"articles, or API docs. Internal/loopback/private addresses are refused. For authenticated " +
			"calls to a specific API, use http_request instead.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"url": map[string]any{"type": "string", "description": "Absolute https:// URL to fetch."},
			},
			"required": []string{"url"},
		},
	}
}

func (t *WebFetchTool) Run(ctx context.Context, args string) (string, error) {
	var in struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal([]byte(args), &in); err != nil {
		return "", fmt.Errorf("web_fetch: invalid arguments: %w", err)
	}
	u, err := parseAbsoluteURL(in.URL)
	if err != nil {
		return "", fmt.Errorf("web_fetch: %w", err)
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return "", fmt.Errorf("web_fetch: unsupported scheme %q", u.Scheme)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", fmt.Errorf("web_fetch: %w", err)
	}
	req.Header.Set("User-Agent", "agentray-web-fetch/1.0")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,text/plain,*/*")

	resp, err := t.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("web_fetch: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, t.maxBodyBytes))
	ct := resp.Header.Get("Content-Type")

	var content string
	if isHTML(ct, body) {
		content = htmlToText(body)
	} else {
		content = string(body)
	}
	content = strings.TrimSpace(content)

	var b strings.Builder
	fmt.Fprintf(&b, "url: %s\nstatus: %s\n", u.String(), resp.Status)
	if ct != "" {
		fmt.Fprintf(&b, "content-type: %s\n", ct)
	}
	if content != "" {
		fmt.Fprintf(&b, "content:\n%s", content)
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

func isHTML(contentType string, body []byte) bool {
	if strings.Contains(strings.ToLower(contentType), "html") {
		return true
	}
	head := strings.ToLower(string(body[:min(len(body), 512)]))
	return strings.Contains(head, "<html") || strings.Contains(head, "<!doctype html")
}

// htmlToText extracts visible text from an HTML document, dropping script/style
// content and collapsing whitespace so the result is compact for the model.
func htmlToText(body []byte) string {
	doc, err := html.Parse(strings.NewReader(string(body)))
	if err != nil {
		return string(body)
	}
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			switch n.Data {
			case "script", "style", "noscript", "head", "svg":
				return
			}
		}
		if n.Type == html.TextNode {
			text := strings.TrimSpace(n.Data)
			if text != "" {
				b.WriteString(text)
				b.WriteString(" ")
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
		// Break lines after common block elements so output stays readable.
		if n.Type == html.ElementNode {
			switch n.Data {
			case "p", "div", "br", "li", "tr", "h1", "h2", "h3", "h4", "h5", "h6", "section", "article":
				b.WriteString("\n")
			}
		}
	}
	walk(doc)
	return collapseBlankLines(b.String())
}

func collapseBlankLines(s string) string {
	var out []string
	blank := 0
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimRightFunc(line, unicode.IsSpace)
		if strings.TrimSpace(line) == "" {
			blank++
			if blank > 1 {
				continue
			}
			out = append(out, "")
			continue
		}
		blank = 0
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}
