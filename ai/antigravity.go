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
	"time"

	"github.com/google/uuid"
	"github.com/lohi-ai/agentray/agentcore"
)

const (
	// Antigravity's Cloud Code Assist endpoints: the daily host first, the
	// sandbox as failover. A transport failure or 5xx before the first stream
	// event retries on the next endpoint; once events flow the endpoint is
	// committed.
	antigravityDailyEndpoint   = "https://daily-cloudcode-pa.googleapis.com"
	antigravitySandboxEndpoint = "https://daily-cloudcode-pa.sandbox.googleapis.com"
	// AntigravityUserAgent mirrors the real antigravity/hub client; the backend
	// gates model availability on the version it carries. Exported because the
	// OAuth login path (internal/oauth) must present the same fingerprint — a
	// version bump in one place only would desynchronize login from chat.
	AntigravityUserAgent = "antigravity/hub/2.8.0 (aidev_client; os_type=darwin; arch=arm64; cl=963137146)"
)

// AntigravityProvider speaks Google's Cloud Code Assist internal API — the
// backend behind the Antigravity subscription — authenticated with a Google
// OAuth access token plus the account's Cloud Code project id. The wire is a
// Gemini-shaped generateContent envelope wrapped in an agent request
// (project/requestId/labels/session bookkeeping the real client sends).
type AntigravityProvider struct {
	// BaseURL, when set, replaces the endpoint failover list with a single
	// endpoint (tests, proxies). Empty tries daily then sandbox.
	BaseURL string
	// HTTP serves the list-models path.
	HTTP *http.Client
	// StreamHTTP serves the SSE path. Nil falls back to HTTP.
	StreamHTTP *http.Client

	// tok is the account credential applied per request by the pooled
	// wrapper; ProjectID rides the request envelope, AccessToken the Bearer
	// header.
	tok OAuthToken
}

// NewAntigravityProvider builds the Antigravity wire client. Credentials
// arrive per request via applyOAuthToken — there is no static key.
func NewAntigravityProvider() *AntigravityProvider {
	return &AntigravityProvider{
		HTTP:       NewChatHTTPClient(0),
		StreamHTTP: NewStreamHTTPClient(0),
	}
}

// applyOAuthToken returns a per-call clone carrying the account credential
// (pooledProvider's oauthTokenApplier seam) so concurrent calls never share
// the token field.
func (p *AntigravityProvider) applyOAuthToken(tok OAuthToken) agentcore.LLMProvider {
	c := *p
	c.tok = tok
	return &c
}

func (p *AntigravityProvider) Name() string        { return VendorGoogleAntigravity }
func (p *AntigravityProvider) SupportsTools() bool { return true }

func (p *AntigravityProvider) ModelCapabilities(model string) agentcore.ModelCapabilities {
	return CapabilitiesFor(p.Name(), model)
}

// streamHTTP is the client the SSE path uses: StreamHTTP when set, otherwise
// whatever the caller put on HTTP.
func (p *AntigravityProvider) streamHTTP() *http.Client {
	if p.StreamHTTP != nil {
		return p.StreamHTTP
	}
	return p.HTTP
}

// endpoints resolves the failover list: an explicit BaseURL pins one endpoint,
// otherwise daily then sandbox.
func (p *AntigravityProvider) endpoints() []string {
	if base := strings.TrimRight(strings.TrimSpace(p.BaseURL), "/"); base != "" {
		return []string{base}
	}
	return []string{antigravityDailyEndpoint, antigravitySandboxEndpoint}
}

// --- wire types (Cloud Code Assist / generateContent envelope) ---

type agPart struct {
	Text             string              `json:"text,omitempty"`
	Thought          bool                `json:"thought,omitempty"`
	FunctionCall     *agFunctionCall     `json:"functionCall,omitempty"`
	FunctionResponse *agFunctionResponse `json:"functionResponse,omitempty"`
}

type agFunctionCall struct {
	Name string         `json:"name"`
	Args map[string]any `json:"args,omitempty"`
	ID   string         `json:"id,omitempty"`
}

type agFunctionResponse struct {
	Name     string         `json:"name"`
	Response map[string]any `json:"response"`
	ID       string         `json:"id,omitempty"`
}

type agContent struct {
	Role  string   `json:"role"`
	Parts []agPart `json:"parts"`
}

type agGenerationConfig struct {
	MaxOutputTokens int               `json:"maxOutputTokens,omitempty"`
	Temperature     float64           `json:"temperature,omitempty"`
	ThinkingConfig  *agThinkingConfig `json:"thinkingConfig,omitempty"`
}

type agThinkingConfig struct {
	IncludeThoughts bool `json:"includeThoughts"`
}

type agFunctionDeclaration struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters"`
}

type agTool struct {
	FunctionDeclarations []agFunctionDeclaration `json:"functionDeclarations"`
}

type agToolConfig struct {
	FunctionCallingConfig struct {
		Mode string `json:"mode"`
	} `json:"functionCallingConfig"`
}

type agInnerRequest struct {
	Contents          []agContent         `json:"contents"`
	SystemInstruction *agContent          `json:"systemInstruction,omitempty"`
	GenerationConfig  *agGenerationConfig `json:"generationConfig,omitempty"`
	Tools             []agTool            `json:"tools,omitempty"`
	ToolConfig        *agToolConfig       `json:"toolConfig,omitempty"`
	Labels            map[string]string   `json:"labels,omitempty"`
}

// agRequest is the outer agent envelope the Antigravity backend expects around
// the generateContent request.
type agRequest struct {
	Project     string         `json:"project"`
	Model       string         `json:"model"`
	UserAgent   string         `json:"userAgent"`
	RequestType string         `json:"requestType"`
	RequestID   string         `json:"requestId"`
	Request     agInnerRequest `json:"request"`
}

// encode maps the neutral request onto the Antigravity envelope. The envelope
// fields (requestId, labels) mirror the real antigravity/hub client: requestId
// is agent/<agent>/<unixms>/<trajectory>/<step> and last_step_index trails the
// step by one.
func (p *AntigravityProvider) encode(req agentcore.ChatRequest) agRequest {
	isClaude := strings.HasPrefix(req.Model, "claude-")
	trajectory := uuid.NewString()
	const step = 2

	inner := agInnerRequest{
		Contents: p.convertMessages(req),
		Labels: map[string]string{
			"used_claude":              fmt.Sprintf("%t", isClaude),
			"used_claude_conservative": fmt.Sprintf("%t", isClaude),
			"trajectory_id":            trajectory,
			"last_step_index":          fmt.Sprintf("%d", step-1),
		},
	}

	var systemParts []string
	for _, m := range req.Messages {
		if m.Role == agentcore.RoleSystem && m.Content != "" {
			systemParts = append(systemParts, m.Content)
		}
	}
	if len(systemParts) > 0 {
		// Antigravity tags the system instruction with role "user" to mirror
		// the real client.
		parts := make([]agPart, 0, len(systemParts))
		for _, text := range systemParts {
			parts = append(parts, agPart{Text: text})
		}
		inner.SystemInstruction = &agContent{Role: "user", Parts: parts}
	}

	maxOut := 65536
	if isClaude {
		// Claude routes on daily-cloudcode-pa reject maxOutputTokens > 64000.
		maxOut = 64000
	}
	gen := &agGenerationConfig{MaxOutputTokens: maxOut}
	if req.Temperature > 0 {
		gen.Temperature = req.Temperature
	}
	if effort := strings.TrimSpace(req.ReasoningEffort); effort != "" && effort != "none" {
		gen.ThinkingConfig = &agThinkingConfig{IncludeThoughts: true}
	}
	inner.GenerationConfig = gen

	if len(req.Tools) > 0 {
		decls := make([]agFunctionDeclaration, 0, len(req.Tools))
		for _, s := range req.Tools {
			params := s.Parameters
			if params == nil {
				params = map[string]any{"type": "object", "properties": map[string]any{}}
			}
			decls = append(decls, agFunctionDeclaration{
				Name: s.Name, Description: s.Description, Parameters: params,
			})
		}
		inner.Tools = []agTool{{FunctionDeclarations: decls}}
	}
	// Antigravity's default tool mode is VALIDATED; Claude routes force it even
	// with no tools declared.
	if len(inner.Tools) > 0 || isClaude {
		tc := &agToolConfig{}
		tc.FunctionCallingConfig.Mode = "VALIDATED"
		inner.ToolConfig = tc
	}

	return agRequest{
		Project:     p.tok.ProjectID,
		Model:       req.Model,
		UserAgent:   "antigravity",
		RequestType: "agent",
		RequestID:   fmt.Sprintf("agent/%s/%d/%s/%d", uuid.NewString(), time.Now().UnixMilli(), trajectory, step),
		Request:     inner,
	}
}

// convertMessages maps the neutral transcript onto Cloud Code Assist contents.
// Consecutive tool results merge into one user content's parts — the API
// requires all function responses for a turn in a single user turn.
func (p *AntigravityProvider) convertMessages(req agentcore.ChatRequest) []agContent {
	var contents []agContent
	// tool result messages carry only ToolCallID; the functionResponse needs
	// the tool NAME, so remember the name each emitted call id maps to.
	callNames := map[string]string{}

	for _, m := range req.Messages {
		switch m.Role {
		case agentcore.RoleSystem:
			// hoisted into systemInstruction by encode
		case agentcore.RoleTool:
			name := m.Name
			if name == "" {
				name = callNames[m.ToolCallID]
			}
			text := messageText(m)
			if images := messageImages(m); len(images) > 0 {
				text = textWithImageNotice(text, len(images), false)
			}
			part := agPart{FunctionResponse: &agFunctionResponse{
				Name:     name,
				Response: map[string]any{"result": text},
				ID:       m.ToolCallID,
			}}
			if n := len(contents); n > 0 && contents[n-1].Role == "user" && hasFunctionResponse(contents[n-1]) {
				contents[n-1].Parts = append(contents[n-1].Parts, part)
			} else {
				contents = append(contents, agContent{Role: "user", Parts: []agPart{part}})
			}
		case agentcore.RoleAssistant:
			var parts []agPart
			if m.Content != "" {
				parts = append(parts, agPart{Text: m.Content})
			}
			for _, tc := range m.ToolCalls {
				callNames[tc.ID] = tc.Name
				var args map[string]any
				if json.Unmarshal([]byte(tc.Arguments), &args) != nil || args == nil {
					args = map[string]any{}
				}
				parts = append(parts, agPart{FunctionCall: &agFunctionCall{
					Name: tc.Name, Args: args, ID: tc.ID,
				}})
			}
			if len(parts) > 0 {
				contents = append(contents, agContent{Role: "model", Parts: parts})
			}
		default: // user
			text := messageText(m)
			if images := messageImages(m); len(images) > 0 {
				text = textWithImageNotice(text, len(images), false)
			}
			if strings.TrimSpace(text) == "" {
				continue
			}
			contents = append(contents, agContent{Role: "user", Parts: []agPart{{Text: text}}})
		}
	}
	return contents
}

func hasFunctionResponse(c agContent) bool {
	for _, part := range c.Parts {
		if part.FunctionResponse != nil {
			return true
		}
	}
	return false
}

// --- streaming wire types ---

// agStreamChunk is one SSE data payload: either a generateContent response
// fragment or an in-band stream error.
type agStreamChunk struct {
	Response *struct {
		Candidates []struct {
			Content struct {
				Role  string `json:"role"`
				Parts []struct {
					Text         string `json:"text"`
					Thought      bool   `json:"thought"`
					FunctionCall *struct {
						Name string         `json:"name"`
						Args map[string]any `json:"args"`
						ID   string         `json:"id"`
					} `json:"functionCall"`
				} `json:"parts"`
			} `json:"content"`
			FinishReason string `json:"finishReason"`
		} `json:"candidates"`
		UsageMetadata *struct {
			PromptTokenCount        int `json:"promptTokenCount"`
			CandidatesTokenCount    int `json:"candidatesTokenCount"`
			CachedContentTokenCount int `json:"cachedContentTokenCount"`
			ThoughtsTokenCount      int `json:"thoughtsTokenCount"`
		} `json:"usageMetadata"`
	} `json:"response"`
	Error *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Status  string `json:"status"`
	} `json:"error"`
}

// Stream POSTs the envelope to {endpoint}/v1internal:streamGenerateContent?alt=sse,
// failing over to the sandbox endpoint when the daily host fails at transport
// level or with a 5xx before the first event. Text parts (thought != true)
// stream as content deltas; functionCall parts arrive whole; usageMetadata
// carries the token accounting.
func (p *AntigravityProvider) Stream(ctx context.Context, req agentcore.ChatRequest) (<-chan agentcore.ChatDelta, error) {
	raw, err := json.Marshal(p.encode(req))
	if err != nil {
		return nil, err
	}

	endpoints := p.endpoints()
	var resp *http.Response
	var lastErr error
	for i, endpoint := range endpoints {
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
			endpoint+"/v1internal:streamGenerateContent?alt=sse", bytes.NewReader(raw))
		if err != nil {
			return nil, err
		}
		httpReq.Header.Set("Authorization", "Bearer "+p.tok.AccessToken)
		httpReq.Header.Set("Content-Type", "application/json")
		httpReq.Header.Set("Accept", "text/event-stream")
		httpReq.Header.Set("User-Agent", AntigravityUserAgent)

		resp, err = p.streamHTTP().Do(httpReq)
		if err != nil {
			// Transport failure before the first event: try the next endpoint.
			lastErr = err
			resp = nil
			continue
		}
		if resp.StatusCode >= 500 && i < len(endpoints)-1 {
			// 5xx before the first event: the next endpoint may be healthy.
			data, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			lastErr = agentcore.NewProviderError(p.Name(), resp, strings.TrimSpace(string(data)))
			resp = nil
			continue
		}
		if resp.StatusCode >= 400 {
			data, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			return nil, agentcore.NewProviderError(p.Name(), resp, strings.TrimSpace(string(data)))
		}
		lastErr = nil
		break
	}
	if resp == nil {
		if lastErr == nil {
			lastErr = fmt.Errorf("no endpoint answered")
		}
		return nil, lastErr
	}

	ch := make(chan agentcore.ChatDelta, 16)
	go func() {
		defer close(ch)
		defer resp.Body.Close()

		var stopReason string
		var usage agentcore.Usage
		callSeq := 0

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
			var chunk agStreamChunk
			if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
				continue
			}
			if chunk.Error != nil {
				ch <- agentcore.ChatDelta{Done: true, Err: p.streamError(chunk.Error.Code, chunk.Error.Status, chunk.Error.Message)}
				return
			}
			r := chunk.Response
			if r == nil {
				continue
			}
			for _, cand := range r.Candidates {
				for _, part := range cand.Content.Parts {
					if part.FunctionCall != nil {
						fc := part.FunctionCall
						id := fc.ID
						if id == "" {
							callSeq++
							id = fmt.Sprintf("ag-call-%d", callSeq)
						}
						args, _ := json.Marshal(fc.Args)
						tc := agentcore.ToolCall{ID: id, Name: fc.Name, Arguments: string(args)}
						ch <- agentcore.ChatDelta{ToolCall: &tc}
						continue
					}
					// thought parts are the model's reasoning, not user-visible
					// text; only plain text streams as content.
					if part.Text != "" && !part.Thought {
						ch <- agentcore.ChatDelta{ContentDelta: part.Text}
					}
				}
				if cand.FinishReason != "" {
					stopReason = mapAntigravityStopReason(cand.FinishReason)
				}
			}
			if u := r.UsageMetadata; u != nil {
				// promptTokenCount includes the cached prefix; the neutral
				// contract keeps InputTokens full-price-only.
				usage = agentcore.Usage{
					InputTokens:     u.PromptTokenCount - u.CachedContentTokenCount,
					OutputTokens:    u.CandidatesTokenCount + u.ThoughtsTokenCount,
					CacheReadTokens: u.CachedContentTokenCount,
				}
			}
		}
		if err := sc.Err(); err != nil {
			ch <- agentcore.ChatDelta{Done: true, Err: err}
			return
		}
		ch <- agentcore.ChatDelta{Done: true, StopReason: stopReason, Usage: usage}
	}()
	return ch, nil
}

// streamError maps an in-band stream error onto a ProviderError with the HTTP
// status the code implies, so the retry ladder classifies it structurally:
// RESOURCE_EXHAUSTED is a 429, UNAUTHENTICATED a 401, anything else a 500.
func (p *AntigravityProvider) streamError(code int, status, message string) error {
	httpStatus := 500
	switch status {
	case "RESOURCE_EXHAUSTED":
		httpStatus = 429
	case "UNAUTHENTICATED":
		httpStatus = 401
	}
	if message == "" {
		message = status
	}
	if message == "" {
		message = "antigravity stream error"
	}
	return &agentcore.ProviderError{Provider: p.Name(), Status: httpStatus, Message: message}
}

// mapAntigravityStopReason folds the generateContent finish reasons onto the
// neutral stop reasons (mirrors the reference's mapStopReasonString).
func mapAntigravityStopReason(reason string) string {
	switch reason {
	case "STOP":
		return "stop"
	case "MAX_TOKENS":
		return "length"
	default:
		return "error"
	}
}

// Chat consumes the SSE stream to completion and returns the assembled
// response — the backend only streams, so there is no second wire path.
func (p *AntigravityProvider) Chat(ctx context.Context, req agentcore.ChatRequest) (agentcore.ChatResponse, error) {
	return chatViaStream(ctx, p, req)
}

// listAntigravityModels calls POST {endpoint}/v1internal:fetchAvailableModels
// with the account's project, trying the same endpoint failover as a chat
// request. The payload's models field has been observed both as a map keyed by
// model id and as a list of {name|model|id} entries; both parse.
func (p *AntigravityProvider) listAntigravityModels(ctx context.Context, client HTTPDoer, tok OAuthToken) ([]Model, error) {
	if client == nil {
		client = defaultHTTP()
	}
	body, err := json.Marshal(map[string]string{"project": tok.ProjectID})
	if err != nil {
		return nil, err
	}
	var lastErr error
	for _, endpoint := range p.endpoints() {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost,
			endpoint+"/v1internal:fetchAvailableModels", bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+tok.AccessToken)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json")
		req.Header.Set("User-Agent", AntigravityUserAgent)

		data, status, err := doJSON(ctx, client, req)
		if err != nil {
			lastErr = err
			continue
		}
		if status >= 400 {
			lastErr = &agentcore.ProviderError{Provider: p.Name(), Status: status, Message: strings.TrimSpace(string(data))}
			continue
		}
		models, err := parseAntigravityModels(data)
		if err != nil {
			lastErr = err
			continue
		}
		return models, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("list antigravity models: no endpoint answered")
	}
	return nil, lastErr
}

// parseAntigravityModels accepts the fetchAvailableModels payload in both
// observed shapes: models as a map keyed by model id (each entry carrying
// displayName/maxTokens), or models as a list whose entries name the model
// under name, model, or id.
func parseAntigravityModels(data []byte) ([]Model, error) {
	var raw struct {
		Models json.RawMessage `json:"models"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("list antigravity models: decode: %w", err)
	}
	if len(raw.Models) == 0 {
		return nil, fmt.Errorf("list antigravity models: no models in response")
	}

	// Map form: {"models": {"<id>": {displayName, maxTokens, ...}}}.
	var asMap map[string]struct {
		Model          string `json:"model"`
		MaxTokens      int    `json:"maxTokens"`
		ContextWindow  int    `json:"context_window"`
		ContextWindow2 int    `json:"contextWindow"`
	}
	if err := json.Unmarshal(raw.Models, &asMap); err == nil && asMap != nil {
		out := make([]Model, 0, len(asMap))
		for id, m := range asMap {
			if mid := strings.TrimSpace(m.Model); mid != "" {
				id = mid
			}
			if id = strings.TrimSpace(id); id != "" {
				out = append(out, Model{ID: id, ContextWindow: firstPositive(m.MaxTokens, m.ContextWindow, m.ContextWindow2)})
			}
		}
		return out, nil
	}

	// List form: {"models": [{"name": "models/<id>" | "<id>", ...}]}.
	var asList []struct {
		Name           string `json:"name"`
		Model          string `json:"model"`
		ID             string `json:"id"`
		MaxTokens      int    `json:"maxTokens"`
		ContextWindow  int    `json:"context_window"`
		ContextWindow2 int    `json:"contextWindow"`
	}
	if err := json.Unmarshal(raw.Models, &asList); err != nil {
		return nil, fmt.Errorf("list antigravity models: decode models: %w", err)
	}
	out := make([]Model, 0, len(asList))
	for _, m := range asList {
		id := m.Model
		if id == "" {
			id = m.ID
		}
		if id == "" {
			id = m.Name
		}
		id = strings.TrimPrefix(strings.TrimSpace(id), "models/")
		if id != "" {
			out = append(out, Model{ID: id, ContextWindow: firstPositive(m.MaxTokens, m.ContextWindow, m.ContextWindow2)})
		}
	}
	return out, nil
}
