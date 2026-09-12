package opcore

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Client is the CLI transport: the user-side adapter that calls a remote
// AgentRay server's operation endpoints over HTTP. It holds only a base URL and
// an API key — no database or queue handle — mirroring the agent's
// least-privilege boundary on the user's machine (agent -> cli -> API).
type Client struct {
	BaseURL string
	APIKey  string
	Prefix  string // endpoint group, e.g. "/api/op"
	HTTP    *http.Client
}

// NewClient builds a Client with sane defaults (60s timeout, /api/op prefix).
func NewClient(baseURL, apiKey string) *Client {
	return &Client{
		BaseURL: strings.TrimRight(baseURL, "/"),
		APIKey:  apiKey,
		Prefix:  "/api/op",
		HTTP:    &http.Client{Timeout: 60 * time.Second},
	}
}

// Call POSTs the input JSON to <BaseURL><Prefix>/<op> and returns the raw
// response body. A non-2xx status is returned as an error carrying the body.
func (c *Client) Call(ctx context.Context, op string, input []byte) ([]byte, error) {
	if len(input) == 0 {
		input = []byte("{}")
	}
	url := c.BaseURL + c.Prefix + "/" + op
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(input))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.APIKey != "" {
		// Management credentials (agm_…) travel as Bearer — the resolver only
		// accepts them there, never as a project key. Everything else stays on
		// X-API-Key so capture/legacy keys are unchanged.
		if strings.HasPrefix(c.APIKey, "agm_") {
			req.Header.Set("Authorization", "Bearer "+c.APIKey)
		} else {
			req.Header.Set("X-API-Key", c.APIKey)
		}
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		// The server answers operation failures as {"error","code"} — decode
		// the typed body so callers (and the CLI's stderr) keep the kind
		// instead of a bare status line.
		var ebody opErrorBody
		if json.Unmarshal(body, &ebody) == nil && ebody.Error != "" {
			if ebody.Code != "" {
				return nil, &OpError{Kind: ErrorKind(ebody.Code), Message: ebody.Error}
			}
			return nil, fmt.Errorf("op %s failed (%d): %s", op, resp.StatusCode, ebody.Error)
		}
		return nil, fmt.Errorf("op %s failed (%d): %s", op, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return body, nil
}
