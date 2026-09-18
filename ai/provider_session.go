package ai

import (
	"errors"
	"net/http"
	"strings"
	"sync"

	"github.com/lohi-ai/agentray/agentcore"
)

const openAIAdaptiveStatePrefix = "openai-chat-adaptive\x00"

// openAIAdaptiveState holds endpoint-scoped lessons, not account-scoped state:
// a gateway that rejects prompt_cache_key keeps rejecting it after an API key
// or OAuth account changes. It therefore deliberately does not implement
// agentcore.AccountScopedProviderState.
type openAIAdaptiveState struct {
	mu     sync.RWMutex
	models map[string]agentcore.ModelCapabilities
	closed bool
}

func newOpenAIAdaptiveState() agentcore.ProviderSessionState {
	return &openAIAdaptiveState{models: make(map[string]agentcore.ModelCapabilities)}
}

func (s *openAIAdaptiveState) Close() {
	s.mu.Lock()
	s.closed = true
	s.models = nil
	s.mu.Unlock()
}

func (s *openAIAdaptiveState) apply(req agentcore.ChatRequest) agentcore.ChatRequest {
	s.mu.RLock()
	caps := s.models[req.Model]
	s.mu.RUnlock()
	if caps.ReasoningEffort == agentcore.CapabilityUnsupported {
		req.ReasoningEffort = ""
	}
	if caps.StructuredOutput == agentcore.CapabilityUnsupported {
		req.OutputSchema = nil
	}
	if caps.PromptCaching == agentcore.CapabilityUnsupported {
		req.CacheKey = ""
		req.CacheRetention = ""
	}
	return req
}

func (s *openAIAdaptiveState) remember(model string, learned agentcore.ModelCapabilities) {
	if learned == (agentcore.ModelCapabilities{}) {
		return
	}
	s.mu.Lock()
	if !s.closed {
		s.models[model] = s.models[model].Overlay(learned)
	}
	s.mu.Unlock()
}

func (p *OpenAIProvider) adaptiveRequest(req agentcore.ChatRequest) (*openAIAdaptiveState, agentcore.ChatRequest) {
	if req.ProviderSession == nil {
		return nil, req
	}
	key := openAIAdaptiveStatePrefix + p.Name() + "\x00" + strings.TrimRight(p.BaseURL, "/")
	state, _ := req.ProviderSession.State(key, newOpenAIAdaptiveState).(*openAIAdaptiveState)
	if state == nil {
		return nil, req
	}
	return state, state.apply(req)
}

// withoutRejectedOpenAIHint recognizes only an explicit unsupported-parameter
// response. It never treats an arbitrary 400 as capability discovery: in
// particular, an invalid JSON schema stays a caller-visible error instead of
// silently weakening the request contract.
func withoutRejectedOpenAIHint(req agentcore.ChatRequest, err error) (agentcore.ChatRequest, agentcore.ModelCapabilities, bool) {
	var pe *agentcore.ProviderError
	if !errors.As(err, &pe) || (pe.Status != http.StatusBadRequest && pe.Status != http.StatusUnprocessableEntity) {
		return req, agentcore.ModelCapabilities{}, false
	}
	message := strings.ToLower(pe.Message)
	unsupported := strings.Contains(message, "unsupported") ||
		strings.Contains(message, "not supported") ||
		strings.Contains(message, "unknown parameter") ||
		strings.Contains(message, "unknown field") ||
		strings.Contains(message, "unrecognized") ||
		strings.Contains(message, "unexpected keyword") ||
		strings.Contains(message, "extra inputs are not permitted")
	if !unsupported {
		return req, agentcore.ModelCapabilities{}, false
	}
	if req.ReasoningEffort != "" && containsAny(message, "reasoning_effort", "reasoning effort") {
		req.ReasoningEffort = ""
		return req, agentcore.ModelCapabilities{ReasoningEffort: agentcore.CapabilityUnsupported}, true
	}
	if req.CacheKey != "" && containsAny(message, "prompt_cache_key", "prompt cache", "prompt caching") {
		req.CacheKey, req.CacheRetention = "", ""
		return req, agentcore.ModelCapabilities{PromptCaching: agentcore.CapabilityUnsupported}, true
	}
	if req.OutputSchema != nil && containsAny(message, "response_format", "structured output", "json_schema") {
		req.OutputSchema = nil
		return req, agentcore.ModelCapabilities{StructuredOutput: agentcore.CapabilityUnsupported}, true
	}
	return req, agentcore.ModelCapabilities{}, false
}

func containsAny(value string, needles ...string) bool {
	for _, needle := range needles {
		if strings.Contains(value, needle) {
			return true
		}
	}
	return false
}

// oauthAccountBinding is stored in the provider session so a newly-built
// pooledProvider can still detect that this logical conversation moved to a
// sibling account between runs. It owns no external resource.
type oauthAccountBinding struct {
	mu      sync.Mutex
	account string
	closed  bool
}

func (s *oauthAccountBinding) Close() {
	s.mu.Lock()
	s.closed = true
	s.account = ""
	s.mu.Unlock()
}

func (s *oauthAccountBinding) switched(account string) bool {
	if account == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	changed := s.account != "" && s.account != account
	s.account = account
	return changed
}

func bindOAuthProviderSession(session *agentcore.ProviderSession, vendor, scope string, tok OAuthToken) {
	if session == nil {
		return
	}
	state, _ := session.State("oauth-account\x00"+vendor+"\x00"+scope, func() agentcore.ProviderSessionState {
		return &oauthAccountBinding{}
	}).(*oauthAccountBinding)
	if state != nil && state.switched(tok.AccountID) {
		session.ResetAccountScoped()
	}
}
