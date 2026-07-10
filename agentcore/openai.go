package agentcore

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	defaultOpenAIBaseURL    = "https://api.openai.com/v1"
	defaultOpenAIEmbedModel = "text-embedding-3-small"
)

// Compat is the per-vendor capability table. Most "providers" are the OpenAI
// completions API at a different base URL plus a few flags (pi-ai pattern), so
// an OpenAI-compatible vendor is config, not new code.
type Compat struct {
	// MaxTokensField is "max_tokens" or "max_completion_tokens".
	MaxTokensField string
	// SupportsTools indicates the vendor accepts the tools/tool_calls fields.
	SupportsTools bool
}

// DefaultCompat is the stock OpenAI behavior.
func DefaultCompat() Compat {
	return Compat{MaxTokensField: "max_tokens", SupportsTools: true}
}

// OpenAIProvider speaks the OpenAI chat-completions wire format. base_url
// resolution (per-config -> OPENAI_BASE_URL env -> vendor default) is performed
// by the caller; this struct receives the resolved BaseURL.
type OpenAIProvider struct {
	APIKey  string
	BaseURL string
	Compat  Compat
	HTTP    *http.Client
}

// NewOpenAIProvider builds a provider. An empty baseURL falls back to the
// vendor default; an empty Compat falls back to DefaultCompat.
func NewOpenAIProvider(apiKey, baseURL string, compat Compat) *OpenAIProvider {
	if strings.TrimSpace(baseURL) == "" {
		baseURL = defaultOpenAIBaseURL
	}
	if compat.MaxTokensField == "" {
		compat = DefaultCompat()
	}
	return &OpenAIProvider{
		APIKey:  apiKey,
		BaseURL: strings.TrimRight(baseURL, "/"),
		Compat:  compat,
		HTTP:    &http.Client{Timeout: 120 * time.Second},
	}
}

func (p *OpenAIProvider) Name() string        { return "openai" }
func (p *OpenAIProvider) SupportsTools() bool { return p.Compat.SupportsTools }

// UpdateAPIKey swaps the key used for subsequent requests (KeyUpdater), letting
// the loop refresh an expiring BYO token between turns.
func (p *OpenAIProvider) UpdateAPIKey(key string) {
	if key != "" {
		p.APIKey = key
	}
}

// --- wire types (OpenAI chat-completions) ---

type oaiMessage struct {
	Role       string        `json:"role"`
	Content    string        `json:"content,omitempty"`
	ToolCalls  []oaiToolCall `json:"tool_calls,omitempty"`
	ToolCallID string        `json:"tool_call_id,omitempty"`
	Name       string        `json:"name,omitempty"`
}

type oaiToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type oaiTool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string         `json:"name"`
		Description string         `json:"description"`
		Parameters  map[string]any `json:"parameters"`
	} `json:"function"`
}

type oaiRequest struct {
	Model         string            `json:"model"`
	Messages      []oaiMessage      `json:"messages"`
	Tools         []oaiTool         `json:"tools,omitempty"`
	MaxTokens     int               `json:"max_tokens,omitempty"`
	Stream        bool              `json:"stream,omitempty"`
	StreamOptions *oaiStreamOptions `json:"stream_options,omitempty"`
	// PromptCacheKey routes this call to a shared cached prefix (OpenAI prompt
	// caching). Sent only when the neutral request opts in, so strict
	// OpenAI-compatible servers that reject unknown fields are never sent it.
	PromptCacheKey string `json:"prompt_cache_key,omitempty"`
	// ReasoningEffort asks a reasoning model for that much thinking per turn
	// ("low" | "medium" | "high"). Sent only when the neutral request sets it.
	ReasoningEffort string `json:"reasoning_effort,omitempty"`
}

type oaiStreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type oaiResponse struct {
	Choices []struct {
		Message      oaiMessage `json:"message"`
		FinishReason string     `json:"finish_reason"`
	} `json:"choices"`
	Usage oaiUsage `json:"usage"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// oaiUsage mirrors OpenAI's usage block, including the cached-token detail.
// prompt_tokens is the *total* input (cached + fresh); cached_tokens is the
// portion served from the prompt cache. usage maps these onto the neutral Usage
// so InputTokens stays full-price-only.
type oaiUsage struct {
	PromptTokens        int `json:"prompt_tokens"`
	CompletionTokens    int `json:"completion_tokens"`
	PromptTokensDetails struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
}

// usage normalizes an OpenAI usage block: cached tokens are pulled out of the
// full prompt total so InputTokens counts only fresh, full-price input.
func (u oaiUsage) usage() Usage {
	cached := u.PromptTokensDetails.CachedTokens
	if cached > u.PromptTokens {
		cached = u.PromptTokens
	}
	return Usage{
		InputTokens:     u.PromptTokens - cached,
		OutputTokens:    u.CompletionTokens,
		CacheReadTokens: cached,
	}
}

// Chat performs one non-streaming completion.
func (p *OpenAIProvider) Chat(ctx context.Context, req ChatRequest) (ChatResponse, error) {
	body := p.encode(req)
	raw, err := json.Marshal(body)
	if err != nil {
		return ChatResponse{}, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.BaseURL+"/chat/completions", bytes.NewReader(raw))
	if err != nil {
		return ChatResponse{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+p.APIKey)

	resp, err := p.HTTP.Do(httpReq)
	if err != nil {
		return ChatResponse{}, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)

	// Some OpenAI-compatible gateways (e.g. 9router routing summarizer calls to a
	// cheap streamed model) reply with text/event-stream even when stream=false
	// was requested. Detect that and fold the SSE chunks into a single response
	// rather than failing the JSON decode and degrading the caller.
	if isSSEResponse(resp.Header.Get("Content-Type"), data) {
		return decodeSSEResponse(p, resp, data)
	}

	var decoded oaiResponse
	if err := json.Unmarshal(data, &decoded); err != nil {
		return ChatResponse{}, fmt.Errorf("decode openai response (status %d): %w", resp.StatusCode, err)
	}
	if decoded.Error != nil {
		return ChatResponse{}, newProviderError(p.Name(), resp, decoded.Error.Message)
	}
	if resp.StatusCode >= 400 || len(decoded.Choices) == 0 {
		return ChatResponse{}, newProviderError(p.Name(), resp, "unexpected response")
	}

	choice := decoded.Choices[0]
	msg := Message{Role: RoleAssistant, Content: choice.Message.Content}
	for _, tc := range choice.Message.ToolCalls {
		msg.ToolCalls = append(msg.ToolCalls, ToolCall{
			ID: tc.ID, Name: tc.Function.Name, Arguments: tc.Function.Arguments,
		})
	}
	return ChatResponse{
		Message:    msg,
		StopReason: choice.FinishReason,
		Usage:      decoded.Usage.usage(),
	}, nil
}

// isSSEResponse reports whether a (nominally non-streaming) completion reply is
// actually an SSE stream: either the content-type advertises it or the body
// begins with an SSE "data:" frame.
func isSSEResponse(contentType string, data []byte) bool {
	if strings.Contains(strings.ToLower(contentType), "text/event-stream") {
		return true
	}
	return strings.HasPrefix(strings.TrimSpace(string(data)), "data:")
}

// decodeSSEResponse folds an SSE chat-completions stream (already buffered into
// data) into a single ChatResponse: concatenating content deltas, reassembling
// tool-call fragments keyed by index, and keeping the last finish_reason/usage.
func decodeSSEResponse(p *OpenAIProvider, resp *http.Response, data []byte) (ChatResponse, error) {
	toolAcc := map[int]*ToolCall{}
	var order []int
	var content strings.Builder
	var stopReason string
	var usage Usage
	var gotChunk bool

	sc := bufio.NewScanner(bytes.NewReader(data))
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
		var chunk oaiStreamChunk
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			continue // tolerate keep-alive/comment lines
		}
		gotChunk = true
		if chunk.Usage != nil {
			usage = chunk.Usage.usage()
		}
		if len(chunk.Choices) == 0 {
			continue
		}
		choice := chunk.Choices[0]
		content.WriteString(choice.Delta.Content)
		for _, tc := range choice.Delta.ToolCalls {
			acc, ok := toolAcc[tc.Index]
			if !ok {
				acc = &ToolCall{}
				toolAcc[tc.Index] = acc
				order = append(order, tc.Index)
			}
			if tc.ID != "" {
				acc.ID = tc.ID
			}
			if tc.Function.Name != "" {
				acc.Name = tc.Function.Name
			}
			acc.Arguments += tc.Function.Arguments
		}
		if choice.FinishReason != "" {
			stopReason = choice.FinishReason
		}
	}
	if !gotChunk {
		return ChatResponse{}, newProviderError(p.Name(), resp, "empty SSE response")
	}

	msg := Message{Role: RoleAssistant, Content: content.String()}
	for _, idx := range order {
		msg.ToolCalls = append(msg.ToolCalls, *toolAcc[idx])
	}
	return ChatResponse{Message: msg, StopReason: stopReason, Usage: usage}, nil
}

// --- streaming wire types (OpenAI chat-completions, stream=true) ---

type oaiStreamChunk struct {
	Choices []struct {
		Delta struct {
			Content   string `json:"content"`
			ToolCalls []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *oaiUsage `json:"usage"`
}

// Stream performs a real token-streaming completion: it sets stream=true and
// emits one ChatDelta per server-sent content chunk, accumulating fragmented
// tool-call deltas (which arrive piecewise, keyed by index) into whole ToolCalls
// flushed before the terminal Done delta. The loop forwards content deltas to a
// live SSE sink; tool execution is unchanged.
func (p *OpenAIProvider) Stream(ctx context.Context, req ChatRequest) (<-chan ChatDelta, error) {
	body := p.encode(req)
	body.Stream = true
	body.StreamOptions = &oaiStreamOptions{IncludeUsage: true}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.BaseURL+"/chat/completions", bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	httpReq.Header.Set("Authorization", "Bearer "+p.APIKey)

	resp, err := p.HTTP.Do(httpReq)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		data, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, newProviderError(p.Name(), resp, strings.TrimSpace(string(data)))
	}

	ch := make(chan ChatDelta, 16)
	go func() {
		defer close(ch)
		defer resp.Body.Close()

		// Tool-call fragments arrive across chunks keyed by index; accumulate
		// them in order and flush whole calls once the stream completes.
		toolAcc := map[int]*ToolCall{}
		var order []int
		var stopReason string
		var usage Usage

		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for sc.Scan() {
			line := sc.Text()
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			payload := strings.TrimSpace(line[len("data:"):])
			if payload == "" {
				continue
			}
			if payload == "[DONE]" {
				break
			}
			var chunk oaiStreamChunk
			if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
				continue // tolerate keep-alive/comment lines
			}
			if chunk.Usage != nil {
				usage = chunk.Usage.usage()
			}
			if len(chunk.Choices) == 0 {
				continue
			}
			choice := chunk.Choices[0]
			if choice.Delta.Content != "" {
				ch <- ChatDelta{ContentDelta: choice.Delta.Content}
			}
			for _, tc := range choice.Delta.ToolCalls {
				acc, ok := toolAcc[tc.Index]
				if !ok {
					acc = &ToolCall{}
					toolAcc[tc.Index] = acc
					order = append(order, tc.Index)
				}
				if tc.ID != "" {
					acc.ID = tc.ID
				}
				if tc.Function.Name != "" {
					acc.Name = tc.Function.Name
				}
				acc.Arguments += tc.Function.Arguments
			}
			if choice.FinishReason != "" {
				stopReason = choice.FinishReason
			}
		}
		if err := sc.Err(); err != nil {
			ch <- ChatDelta{Done: true, Err: err}
			return
		}
		for _, idx := range order {
			tc := *toolAcc[idx]
			ch <- ChatDelta{ToolCall: &tc}
		}
		ch <- ChatDelta{Done: true, StopReason: stopReason, Usage: usage}
	}()
	return ch, nil
}

// --- embeddings (semantic memory recall, §14.7) ---

// OpenAIEmbedder calls the OpenAI /embeddings endpoint, reusing the project's
// BYO key and base_url. It backs the agentcore.Embedder seam so semantic recall
// works for the OpenAI-wire provider family (OpenAI + compatible vendors) with
// no extra credentials.
type OpenAIEmbedder struct {
	APIKey  string
	BaseURL string
	Model   string
	HTTP    *http.Client
}

// NewOpenAIEmbedder builds an embedder. Empty baseURL/model fall back to the
// OpenAI defaults.
func NewOpenAIEmbedder(apiKey, baseURL, model string) *OpenAIEmbedder {
	if strings.TrimSpace(baseURL) == "" {
		baseURL = defaultOpenAIBaseURL
	}
	if strings.TrimSpace(model) == "" {
		model = defaultOpenAIEmbedModel
	}
	return &OpenAIEmbedder{
		APIKey:  apiKey,
		BaseURL: strings.TrimRight(baseURL, "/"),
		Model:   model,
		HTTP:    &http.Client{Timeout: 60 * time.Second},
	}
}

type oaiEmbedRequest struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

type oaiEmbedResponse struct {
	Data []struct {
		Index     int       `json:"index"`
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// Embed returns one vector per input string, index-aligned.
func (e *OpenAIEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	raw, err := json.Marshal(oaiEmbedRequest{Model: e.Model, Input: texts})
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, e.BaseURL+"/embeddings", bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+e.APIKey)

	resp, err := e.HTTP.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)

	var decoded oaiEmbedResponse
	if err := json.Unmarshal(data, &decoded); err != nil {
		return nil, fmt.Errorf("decode embeddings response (status %d): %w", resp.StatusCode, err)
	}
	if decoded.Error != nil {
		return nil, fmt.Errorf("openai embeddings: %s", decoded.Error.Message)
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("openai embeddings: unexpected response (status %d)", resp.StatusCode)
	}
	out := make([][]float32, len(texts))
	for _, d := range decoded.Data {
		if d.Index >= 0 && d.Index < len(out) {
			out[d.Index] = d.Embedding
		}
	}
	return out, nil
}

// encode maps the neutral ChatRequest onto the OpenAI wire format, honoring the
// compat table.
func (p *OpenAIProvider) encode(req ChatRequest) oaiRequest {
	out := oaiRequest{Model: req.Model, MaxTokens: req.MaxTokens, PromptCacheKey: req.CacheKey, ReasoningEffort: req.ReasoningEffort}
	for _, m := range req.Messages {
		om := oaiMessage{Role: string(m.Role), Content: m.Content, ToolCallID: m.ToolCallID, Name: m.Name}
		for _, tc := range m.ToolCalls {
			var otc oaiToolCall
			otc.ID = tc.ID
			otc.Type = "function"
			otc.Function.Name = tc.Name
			otc.Function.Arguments = tc.Arguments
			om.ToolCalls = append(om.ToolCalls, otc)
		}
		out.Messages = append(out.Messages, om)
	}
	if p.Compat.SupportsTools {
		for _, s := range req.Tools {
			var ot oaiTool
			ot.Type = "function"
			ot.Function.Name = s.Name
			ot.Function.Description = s.Description
			ot.Function.Parameters = s.Parameters
			out.Tools = append(out.Tools, ot)
		}
	}
	return out
}
