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
	defaultAnthropicBaseURL = "https://api.anthropic.com"
	anthropicVersion        = "2023-06-01"
	anthropicDefaultTokens  = 4096
	// anthropicExtendedCacheBeta is the beta opt-in required for the 1-hour cache
	// window; the default 5-minute window needs no beta header.
	anthropicExtendedCacheBeta = "extended-cache-ttl-2025-04-11"
)

// antCacheTTL maps the neutral CacheRetention hint onto an Anthropic cache TTL:
// a long/24h hint asks for the extended 1-hour window, anything else uses the
// default 5-minute window (empty TTL).
func antCacheTTL(retention string) string {
	switch retention {
	case "long", "24h":
		return "1h"
	default:
		return ""
	}
}

// usesExtendedCache reports whether the request opts into the 1-hour cache
// window, which requires the extended-cache beta header.
func usesExtendedCache(req ChatRequest) bool {
	return req.CacheKey != "" && antCacheTTL(req.CacheRetention) == "1h"
}

// AnthropicProvider speaks the Anthropic Messages API. The wire format diverges
// from OpenAI completions (top-level system, tool_use/tool_result content
// blocks, x-api-key auth), so per the spec it gets its own implementation
// rather than a compat entry — and proves the LLMProvider seam (§12 AC: a second
// provider needs no edits to agent.go).
type AnthropicProvider struct {
	APIKey  string
	BaseURL string
	HTTP    *http.Client
}

// NewAnthropicProvider builds a provider; an empty baseURL uses the vendor
// default.
func NewAnthropicProvider(apiKey, baseURL string) *AnthropicProvider {
	if strings.TrimSpace(baseURL) == "" {
		baseURL = defaultAnthropicBaseURL
	}
	return &AnthropicProvider{
		APIKey:  apiKey,
		BaseURL: strings.TrimRight(baseURL, "/"),
		HTTP:    &http.Client{Timeout: 120 * time.Second},
	}
}

// UpdateAPIKey swaps the key used for subsequent requests (KeyUpdater), letting
// the loop refresh an expiring BYO token between turns.
func (p *AnthropicProvider) UpdateAPIKey(key string) {
	if key != "" {
		p.APIKey = key
	}
}

func (p *AnthropicProvider) Name() string        { return "anthropic" }
func (p *AnthropicProvider) SupportsTools() bool { return true }

// --- wire types (Anthropic Messages) ---

type antContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
	// tool_use
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
	// tool_result
	ToolUseID string `json:"tool_use_id,omitempty"`
	Content   string `json:"content,omitempty"`
	// CacheControl marks this block as a prompt-cache breakpoint. encode stamps
	// it on the final block of the final message (the "moving breakpoint"), so
	// an agent loop's whole transcript-so-far is read from cache next turn.
	CacheControl *antCacheControl `json:"cache_control,omitempty"`
}

type antMessage struct {
	Role    string            `json:"role"`
	Content []antContentBlock `json:"content"`
}

// antCacheControl marks a prefix block as cacheable (Anthropic prompt caching).
// Type is always "ephemeral"; TTL is "" (the default 5-minute window) or "1h"
// (the extended window, which also needs the extended-cache-ttl beta header).
type antCacheControl struct {
	Type string `json:"type"`
	TTL  string `json:"ttl,omitempty"`
}

// antSystemBlock is the structured form of the system prompt, used only when
// caching is requested so the stable system prefix can carry cache_control.
type antSystemBlock struct {
	Type         string           `json:"type"`
	Text         string           `json:"text"`
	CacheControl *antCacheControl `json:"cache_control,omitempty"`
}

type antTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	InputSchema map[string]any `json:"input_schema"`
}

type antRequest struct {
	Model     string `json:"model"`
	MaxTokens int    `json:"max_tokens"`
	// System is a plain string, or — when caching is requested — a []antSystemBlock
	// whose last block carries cache_control. any keeps the common (uncached) path
	// emitting the same bare string the API has always received.
	System   any          `json:"system,omitempty"`
	Messages []antMessage `json:"messages"`
	Tools    []antTool    `json:"tools,omitempty"`
	Stream   bool         `json:"stream,omitempty"`
}

// antUsage mirrors Anthropic's usage block. input_tokens already *excludes* the
// cached prefix, so it maps straight onto the neutral InputTokens; the two cache
// counters map onto CacheReadTokens (a cache hit) and CacheWriteTokens (cache
// creation).
type antUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
}

func (u antUsage) usage() Usage {
	return Usage{
		InputTokens:      u.InputTokens,
		OutputTokens:     u.OutputTokens,
		CacheReadTokens:  u.CacheReadInputTokens,
		CacheWriteTokens: u.CacheCreationInputTokens,
	}
}

type antResponse struct {
	Content    []antContentBlock `json:"content"`
	StopReason string            `json:"stop_reason"`
	Usage      antUsage          `json:"usage"`
	Error      *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// Chat performs one non-streaming Messages call.
func (p *AnthropicProvider) Chat(ctx context.Context, req ChatRequest) (ChatResponse, error) {
	body := p.encode(req)
	raw, err := json.Marshal(body)
	if err != nil {
		return ChatResponse{}, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.BaseURL+"/v1/messages", bytes.NewReader(raw))
	if err != nil {
		return ChatResponse{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-api-key", p.APIKey)
	httpReq.Header.Set("anthropic-version", anthropicVersion)
	if usesExtendedCache(req) {
		httpReq.Header.Set("anthropic-beta", anthropicExtendedCacheBeta)
	}

	resp, err := p.HTTP.Do(httpReq)
	if err != nil {
		return ChatResponse{}, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)

	var decoded antResponse
	if err := json.Unmarshal(data, &decoded); err != nil {
		return ChatResponse{}, fmt.Errorf("decode anthropic response (status %d): %w", resp.StatusCode, err)
	}
	if decoded.Error != nil {
		return ChatResponse{}, newProviderError(p.Name(), resp, decoded.Error.Message)
	}
	if resp.StatusCode >= 400 {
		return ChatResponse{}, newProviderError(p.Name(), resp, "unexpected response")
	}

	msg := Message{Role: RoleAssistant}
	for _, block := range decoded.Content {
		switch block.Type {
		case "text":
			msg.Content += block.Text
		case "tool_use":
			msg.ToolCalls = append(msg.ToolCalls, ToolCall{
				ID: block.ID, Name: block.Name, Arguments: string(block.Input),
			})
		}
	}
	return ChatResponse{
		Message:    msg,
		StopReason: decoded.StopReason,
		Usage:      decoded.Usage.usage(),
	}, nil
}

// --- streaming wire types (Anthropic Messages, stream=true) ---

// antStreamEvent is the union of the Messages SSE event payloads we consume.
// Each data line carries a discriminating "type"; we dispatch on it rather than
// the parallel `event:` line.
type antStreamEvent struct {
	Type  string `json:"type"`
	Index int    `json:"index"`
	// content_block_start
	ContentBlock *antContentBlock `json:"content_block,omitempty"`
	// content_block_delta
	Delta *struct {
		Type        string `json:"type"`
		Text        string `json:"text,omitempty"`
		PartialJSON string `json:"partial_json,omitempty"`
		StopReason  string `json:"stop_reason,omitempty"`
	} `json:"delta,omitempty"`
	// message_start / message_delta
	Message *struct {
		Usage antUsage `json:"usage"`
	} `json:"message,omitempty"`
	Usage *struct {
		OutputTokens int `json:"output_tokens"`
	} `json:"usage,omitempty"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// Stream performs a real token-streaming Messages call: text_delta blocks are
// forwarded as content deltas; tool_use blocks accumulate their input_json_delta
// fragments (keyed by content-block index) into whole ToolCalls flushed before
// the terminal Done delta. Proves the streaming seam holds for a second provider
// with no edits to the loop (§12 AC).
func (p *AnthropicProvider) Stream(ctx context.Context, req ChatRequest) (<-chan ChatDelta, error) {
	body := p.encode(req)
	body.Stream = true
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.BaseURL+"/v1/messages", bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	httpReq.Header.Set("x-api-key", p.APIKey)
	httpReq.Header.Set("anthropic-version", anthropicVersion)
	if usesExtendedCache(req) {
		httpReq.Header.Set("anthropic-beta", anthropicExtendedCacheBeta)
	}

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

		// Each content-block index is either text or a tool_use call assembled
		// from JSON fragments; track the in-progress tool calls by index.
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
			var ev antStreamEvent
			if err := json.Unmarshal([]byte(payload), &ev); err != nil {
				continue
			}
			switch ev.Type {
			case "message_start":
				if ev.Message != nil {
					// message_start carries the full input accounting, including the
					// cache-read/cache-write split; output accrues via message_delta.
					u := ev.Message.Usage.usage()
					usage.InputTokens = u.InputTokens
					usage.CacheReadTokens = u.CacheReadTokens
					usage.CacheWriteTokens = u.CacheWriteTokens
				}
			case "content_block_start":
				if ev.ContentBlock != nil && ev.ContentBlock.Type == "tool_use" {
					toolAcc[ev.Index] = &ToolCall{ID: ev.ContentBlock.ID, Name: ev.ContentBlock.Name}
					order = append(order, ev.Index)
				}
			case "content_block_delta":
				if ev.Delta == nil {
					continue
				}
				switch ev.Delta.Type {
				case "text_delta":
					if ev.Delta.Text != "" {
						ch <- ChatDelta{ContentDelta: ev.Delta.Text}
					}
				case "input_json_delta":
					if acc, ok := toolAcc[ev.Index]; ok {
						acc.Arguments += ev.Delta.PartialJSON
					}
				}
			case "message_delta":
				if ev.Delta != nil && ev.Delta.StopReason != "" {
					stopReason = ev.Delta.StopReason
				}
				if ev.Usage != nil {
					usage.OutputTokens = ev.Usage.OutputTokens
				}
			case "error":
				if ev.Error != nil {
					ch <- ChatDelta{Done: true, Err: fmt.Errorf("anthropic: %s", ev.Error.Message)}
					return
				}
			case "message_stop":
				// terminal; loop exits on scanner EOF
			}
		}
		if err := sc.Err(); err != nil {
			ch <- ChatDelta{Done: true, Err: err}
			return
		}
		for _, idx := range order {
			tc := *toolAcc[idx]
			if strings.TrimSpace(tc.Arguments) == "" {
				tc.Arguments = "{}"
			}
			ch <- ChatDelta{ToolCall: &tc}
		}
		ch <- ChatDelta{Done: true, StopReason: stopReason, Usage: usage}
	}()
	return ch, nil
}

// encode maps the neutral ChatRequest onto the Anthropic wire format: system
// messages collapse into the top-level system field; tool calls/results become
// tool_use/tool_result content blocks.
func (p *AnthropicProvider) encode(req ChatRequest) antRequest {
	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		maxTokens = anthropicDefaultTokens
	}
	out := antRequest{Model: req.Model, MaxTokens: maxTokens}

	var systemParts []string
	for _, m := range req.Messages {
		switch m.Role {
		case RoleSystem:
			if m.Content != "" {
				systemParts = append(systemParts, m.Content)
			}
		case RoleTool:
			out.Messages = append(out.Messages, antMessage{
				Role: "user",
				Content: []antContentBlock{{
					Type: "tool_result", ToolUseID: m.ToolCallID, Content: m.Content,
				}},
			})
		case RoleAssistant:
			blocks := []antContentBlock{}
			if m.Content != "" {
				blocks = append(blocks, antContentBlock{Type: "text", Text: m.Content})
			}
			for _, tc := range m.ToolCalls {
				input := json.RawMessage(tc.Arguments)
				if len(input) == 0 {
					input = json.RawMessage("{}")
				}
				blocks = append(blocks, antContentBlock{Type: "tool_use", ID: tc.ID, Name: tc.Name, Input: input})
			}
			if len(blocks) == 0 {
				blocks = append(blocks, antContentBlock{Type: "text", Text: ""})
			}
			out.Messages = append(out.Messages, antMessage{Role: "assistant", Content: blocks})
		default: // user
			out.Messages = append(out.Messages, antMessage{
				Role:    "user",
				Content: []antContentBlock{{Type: "text", Text: m.Content}},
			})
		}
	}
	// The system prompt is the largest stable prefix of a long run. When caching is
	// requested, send it as a structured block carrying cache_control so Anthropic
	// reuses it across turns; otherwise keep the bare-string form the API has always
	// received. An empty system stays omitted either way.
	if systemText := strings.Join(systemParts, "\n\n"); systemText != "" {
		if req.CacheKey != "" {
			out.System = []antSystemBlock{{
				Type:         "text",
				Text:         systemText,
				CacheControl: &antCacheControl{Type: "ephemeral", TTL: antCacheTTL(req.CacheRetention)},
			}}
		} else {
			out.System = systemText
		}
	}

	// Moving breakpoint: also mark the final block of the final message, so the
	// whole transcript-so-far becomes the cached prefix. Next turn re-sends the
	// same transcript plus one exchange; Anthropic prefix-matches the previous
	// breakpoint and bills everything before it as a cache read. Without this,
	// only tools+system are cached and a large first user message (an agent's
	// task + context payload) is re-billed in full every turn of the loop.
	if req.CacheKey != "" && len(out.Messages) > 0 {
		last := &out.Messages[len(out.Messages)-1]
		if n := len(last.Content); n > 0 {
			last.Content[n-1].CacheControl = &antCacheControl{Type: "ephemeral", TTL: antCacheTTL(req.CacheRetention)}
		}
	}

	for _, s := range req.Tools {
		params := s.Parameters
		if params == nil {
			params = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		out.Tools = append(out.Tools, antTool{Name: s.Name, Description: s.Description, InputSchema: params})
	}
	return out
}
