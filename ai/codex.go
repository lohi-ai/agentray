package ai

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/lohi-ai/agentray/agentcore"
)

const (
	// codexBaseURL is the ChatGPT backend the Codex OAuth subscription speaks:
	// the Responses API under /backend-api/codex, not api.openai.com.
	codexBaseURL = "https://chatgpt.com/backend-api"
	// codexClientVersion is the pinned @openai/codex client version. The backend
	// version-gates model availability against it on both /models and
	// /responses, so an older pin silently hides newer SKUs.
	codexClientVersion = "0.153.0"
)

// CodexProvider speaks the ChatGPT Codex backend: the Responses API at
// chatgpt.com/backend-api/codex/responses, authenticated with a ChatGPT OAuth
// access token rather than an OpenAI API key. The wire is Responses-shaped
// (input items, function_call/function_call_output pairs, SSE event types
// under response.*), but the backend is stricter than the public API: it
// rejects sampling parameters outright, requires stream:true, and wants the
// codex client headers (originator, version, OpenAI-Beta) on every call.
type CodexProvider struct {
	// BaseURL overrides the backend origin; empty uses codexBaseURL. The
	// /codex/responses suffix is appended by responsesURL, so a test server
	// can point BaseURL at itself.
	BaseURL string
	// HTTP serves the list-models path.
	HTTP *http.Client
	// StreamHTTP serves the SSE path (the only chat transport this backend
	// offers). Nil falls back to HTTP.
	StreamHTTP *http.Client

	// tok is the account credential applied per request by the pooled
	// wrapper. The whole OAuthToken is kept because ProviderAccountID rides
	// the wire as the chatgpt-account-id header.
	tok OAuthToken
}

// NewCodexProvider builds the Codex wire client. Credentials arrive per
// request via applyOAuthToken — there is no static key.
func NewCodexProvider() *CodexProvider {
	return &CodexProvider{
		HTTP:       NewChatHTTPClient(0),
		StreamHTTP: NewStreamHTTPClient(0),
	}
}

// applyOAuthToken installs the account credential for the next request
// (pooledProvider's oauthTokenApplier seam).
func (p *CodexProvider) applyOAuthToken(tok OAuthToken) { p.tok = tok }

func (p *CodexProvider) Name() string        { return VendorOpenAICodex }
func (p *CodexProvider) SupportsTools() bool { return true }

// streamHTTP is the client the SSE path uses: StreamHTTP when set, otherwise
// whatever the caller put on HTTP.
func (p *CodexProvider) streamHTTP() *http.Client {
	if p.StreamHTTP != nil {
		return p.StreamHTTP
	}
	return p.HTTP
}

// responsesURL resolves the Responses endpoint from BaseURL the way the
// reference client does: a base already ending in /codex/responses or /codex
// is respected, anything else gets /codex/responses appended.
func (p *CodexProvider) responsesURL() string {
	base := strings.TrimRight(strings.TrimSpace(p.BaseURL), "/")
	if base == "" {
		base = codexBaseURL
	}
	if strings.HasSuffix(base, "/codex/responses") {
		return base
	}
	if strings.HasSuffix(base, "/codex") {
		return base + "/responses"
	}
	return base + "/codex/responses"
}

// setHeaders applies the Codex client fingerprint: Bearer auth, the account id
// header when the token carries one, and the version/originator pair the
// backend gates on.
func (p *CodexProvider) setHeaders(req *http.Request) {
	req.Header.Set("Authorization", "Bearer "+p.tok.AccessToken)
	if p.tok.ProviderAccountID != "" {
		req.Header.Set("chatgpt-account-id", p.tok.ProviderAccountID)
	}
	req.Header.Set("OpenAI-Beta", "responses=experimental")
	req.Header.Set("originator", "omp")
	req.Header.Set("version", codexClientVersion)
}

// --- wire types (Codex Responses) ---

// codexInputItem is one entry of the Responses `input` array: a message, a
// function_call, or a function_call_output.
type codexInputItem struct {
	Type    string         `json:"type"`
	Role    string         `json:"role,omitempty"`
	Content []codexContent `json:"content,omitempty"`
	CallID  string         `json:"call_id,omitempty"`
	Name    string         `json:"name,omitempty"`
	// Arguments is the function_call's raw JSON argument string.
	Arguments string `json:"arguments,omitempty"`
	// Output is the function_call_output's result text.
	Output string `json:"output,omitempty"`
}

type codexContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type codexTool struct {
	Type        string         `json:"type"` // always "function"
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters"`
}

type codexRequest struct {
	Model        string           `json:"model"`
	Instructions string           `json:"instructions,omitempty"`
	Input        []codexInputItem `json:"input"`
	Stream       bool             `json:"stream"` // always true: the backend only streams
	Store        bool             `json:"store"`  // always false: no server-side state
	Tools        []codexTool      `json:"tools,omitempty"`
	Reasoning    *codexReasoning  `json:"reasoning,omitempty"`
	// Sampling controls (temperature/top_p/max_output_tokens) are deliberately
	// absent: the Codex backend rejects every one with a 400.
}

type codexReasoning struct {
	Effort string `json:"effort"`
}

// encode maps the neutral request onto the Codex Responses body. System
// messages collapse into the top-level `instructions` field (never input
// items); tool exchanges become function_call / function_call_output pairs.
func (p *CodexProvider) encode(req agentcore.ChatRequest) codexRequest {
	out := codexRequest{Model: req.Model, Stream: true, Store: false}

	var systemParts []string
	for _, m := range req.Messages {
		switch m.Role {
		case agentcore.RoleSystem:
			if m.Content != "" {
				systemParts = append(systemParts, m.Content)
			}
		case agentcore.RoleTool:
			out.Input = append(out.Input, codexInputItem{
				Type:   "function_call_output",
				CallID: m.ToolCallID,
				Output: m.Content,
			})
		case agentcore.RoleAssistant:
			if m.Content != "" {
				out.Input = append(out.Input, codexInputItem{
					Type:    "message",
					Role:    "assistant",
					Content: []codexContent{{Type: "output_text", Text: m.Content}},
				})
			}
			for _, tc := range m.ToolCalls {
				args := tc.Arguments
				if strings.TrimSpace(args) == "" {
					args = "{}"
				}
				out.Input = append(out.Input, codexInputItem{
					Type:      "function_call",
					CallID:    tc.ID,
					Name:      tc.Name,
					Arguments: args,
				})
			}
		default: // user
			out.Input = append(out.Input, codexInputItem{
				Type:    "message",
				Role:    "user",
				Content: []codexContent{{Type: "input_text", Text: m.Content}},
			})
		}
	}
	out.Instructions = strings.Join(systemParts, "\n\n")

	for _, s := range req.Tools {
		params := s.Parameters
		if params == nil {
			params = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		out.Tools = append(out.Tools, codexTool{
			Type: "function", Name: s.Name, Description: s.Description, Parameters: params,
		})
	}
	if effort := strings.TrimSpace(req.ReasoningEffort); effort != "" {
		out.Reasoning = &codexReasoning{Effort: effort}
	}
	return out
}

// --- streaming wire types ---

// codexStreamEvent is the union of the Responses SSE event payloads consumed
// here. Each data line carries a discriminating "type" under the response.*
// namespace.
type codexStreamEvent struct {
	Type  string `json:"type"`
	Delta string `json:"delta,omitempty"` // response.output_text.delta
	Item  *struct {
		Type      string `json:"type"`
		CallID    string `json:"call_id"`
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"item,omitempty"` // response.output_item.done
	Response *struct {
		Status string `json:"status"`
		Usage  *struct {
			InputTokens        int `json:"input_tokens"`
			OutputTokens       int `json:"output_tokens"`
			InputTokensDetails *struct {
				CachedTokens int `json:"cached_tokens"`
			} `json:"input_tokens_details,omitempty"`
		} `json:"usage,omitempty"`
		Error *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error,omitempty"`
	} `json:"response,omitempty"` // response.completed / .done / .incomplete / .failed
	Error *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error,omitempty"` // error / response.failed
	Code    string `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
}

// Stream performs the only chat call this backend offers: a streamed Responses
// request. Text arrives as response.output_text.delta events; tool calls land
// whole on response.output_item.done; usage and the stop reason ride the
// terminal response.completed/done/incomplete event.
func (p *CodexProvider) Stream(ctx context.Context, req agentcore.ChatRequest) (<-chan agentcore.ChatDelta, error) {
	raw, err := json.Marshal(p.encode(req))
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.responsesURL(), bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	p.setHeaders(httpReq)
	httpReq.Header.Set("Accept", "text/event-stream")
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := p.streamHTTP().Do(httpReq)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		data, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, agentcore.NewProviderError(p.Name(), resp, strings.TrimSpace(string(data)))
	}

	ch := make(chan agentcore.ChatDelta, 16)
	go func() {
		defer close(ch)
		defer resp.Body.Close()

		var stopReason string
		var usage agentcore.Usage
		terminal := false

		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for sc.Scan() {
			line := sc.Text()
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			payload := strings.TrimSpace(line[len("data:"):])
			if payload == "" || payload == "[DONE]" {
				continue
			}
			var ev codexStreamEvent
			if err := json.Unmarshal([]byte(payload), &ev); err != nil {
				continue
			}
			switch ev.Type {
			case "response.output_text.delta":
				if ev.Delta != "" {
					ch <- agentcore.ChatDelta{ContentDelta: ev.Delta}
				}
			case "response.output_item.done":
				if ev.Item != nil && ev.Item.Type == "function_call" {
					args := ev.Item.Arguments
					if strings.TrimSpace(args) == "" {
						args = "{}"
					}
					ch <- agentcore.ChatDelta{ToolCall: &agentcore.ToolCall{
						ID: ev.Item.CallID, Name: ev.Item.Name, Arguments: args,
					}}
				}
			case "response.completed", "response.done", "response.incomplete":
				terminal = true
				if ev.Response != nil {
					stopReason = ev.Response.Status
					if u := ev.Response.Usage; u != nil {
						cached := 0
						if u.InputTokensDetails != nil {
							cached = u.InputTokensDetails.CachedTokens
						}
						// input_tokens includes the cached prefix; the neutral
						// contract keeps InputTokens full-price-only.
						usage = agentcore.Usage{
							InputTokens:     u.InputTokens - cached,
							OutputTokens:    u.OutputTokens,
							CacheReadTokens: cached,
						}
					}
				}
			case "response.failed", "error":
				ch <- agentcore.ChatDelta{Done: true, Err: p.eventError(&ev)}
				return
			}
		}
		if err := sc.Err(); err != nil {
			ch <- agentcore.ChatDelta{Done: true, Err: err}
			return
		}
		if !terminal {
			ch <- agentcore.ChatDelta{Done: true, Err: agentcore.NewProviderError(p.Name(), nil,
				"codex stream ended before terminal completion event")}
			return
		}
		ch <- agentcore.ChatDelta{Done: true, StopReason: stopReason, Usage: usage}
	}()
	return ch, nil
}

// eventError folds a response.failed / error SSE event into a provider error,
// preferring the nested error object's message and code.
func (p *CodexProvider) eventError(ev *codexStreamEvent) error {
	msg, code := "", ""
	if ev.Error != nil {
		msg, code = ev.Error.Message, ev.Error.Code
	}
	if msg == "" && ev.Response != nil && ev.Response.Error != nil {
		msg, code = ev.Response.Error.Message, ev.Response.Error.Code
	}
	if msg == "" {
		msg = ev.Message
	}
	if code == "" {
		code = ev.Code
	}
	if msg == "" {
		msg = "codex response failed"
	}
	if code != "" {
		msg = fmt.Sprintf("%s (code=%s)", msg, code)
	}
	return agentcore.NewProviderError(p.Name(), nil, msg)
}

// Chat consumes the SSE stream to completion and returns the assembled
// response — the backend has no non-streaming mode, so this is the simplest
// correct implementation rather than a second wire path.
func (p *CodexProvider) Chat(ctx context.Context, req agentcore.ChatRequest) (agentcore.ChatResponse, error) {
	ch, err := p.Stream(ctx, req)
	if err != nil {
		return agentcore.ChatResponse{}, err
	}
	var resp agentcore.ChatResponse
	resp.Message.Role = agentcore.RoleAssistant
	for d := range ch {
		if d.Err != nil {
			return agentcore.ChatResponse{}, d.Err
		}
		resp.Message.Content += d.ContentDelta
		if d.ToolCall != nil {
			resp.Message.ToolCalls = append(resp.Message.ToolCalls, *d.ToolCall)
		}
		if d.Usage.InputTokens != 0 || d.Usage.OutputTokens != 0 || d.Usage.CacheReadTokens != 0 {
			resp.Usage = d.Usage
		}
		if d.StopReason != "" {
			resp.StopReason = d.StopReason
		}
	}
	return resp, nil
}

// listCodexModels calls GET {base}/codex/models?client_version=…, falling back
// to {base}/models, with the same client headers as a chat request. The
// response is either {models:[…]} or {data:[…]}; each entry's slug (or id) is
// the model id and context_window, when present, is the input window.
func (p *CodexProvider) listCodexModels(ctx context.Context, client HTTPDoer, tok OAuthToken) ([]Model, error) {
	if client == nil {
		client = defaultHTTP()
	}
	base := strings.TrimRight(strings.TrimSpace(p.BaseURL), "/")
	if base == "" {
		base = codexBaseURL
	}
	var lastErr error
	for _, path := range []string{"/codex/models", "/models"} {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet,
			base+path+"?client_version="+codexClientVersion, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+tok.AccessToken)
		if tok.ProviderAccountID != "" {
			req.Header.Set("chatgpt-account-id", tok.ProviderAccountID)
		}
		req.Header.Set("OpenAI-Beta", "responses=experimental")
		req.Header.Set("originator", "omp")
		req.Header.Set("version", codexClientVersion)
		req.Header.Set("Accept", "application/json")

		data, status, err := doJSON(ctx, client, req)
		if err != nil {
			lastErr = err
			continue
		}
		if status == http.StatusUnauthorized || status == http.StatusForbidden {
			return nil, fmt.Errorf("list codex models: status %d: %s", status, strings.TrimSpace(string(data)))
		}
		if status >= 400 {
			lastErr = fmt.Errorf("list codex models: status %d: %s", status, strings.TrimSpace(string(data)))
			continue
		}
		models, err := parseCodexModels(data)
		if err != nil {
			lastErr = err
			continue
		}
		return models, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("list codex models: no endpoint answered")
	}
	return nil, lastErr
}

// parseCodexModels accepts both envelope shapes the backend has used:
// {models:[…]} and {data:[…]}, with the model id under slug or id.
func parseCodexModels(data []byte) ([]Model, error) {
	var decoded struct {
		Models []struct {
			Slug           string `json:"slug"`
			ID             string `json:"id"`
			ContextWindow  int    `json:"context_window"`
			ContextWindow2 int    `json:"contextWindow"`
		} `json:"models"`
		Data []struct {
			Slug           string `json:"slug"`
			ID             string `json:"id"`
			ContextWindow  int    `json:"context_window"`
			ContextWindow2 int    `json:"contextWindow"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &decoded); err != nil {
		return nil, fmt.Errorf("list codex models: decode: %w", err)
	}
	entries := decoded.Models
	if len(entries) == 0 {
		entries = decoded.Data
	}
	out := make([]Model, 0, len(entries))
	for _, m := range entries {
		id := m.Slug
		if id == "" {
			id = m.ID
		}
		if id = strings.TrimSpace(id); id != "" {
			out = append(out, Model{ID: id, ContextWindow: firstPositive(m.ContextWindow, m.ContextWindow2)})
		}
	}
	return out, nil
}
