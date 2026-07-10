// Package agentcore is a reusable, product-agnostic agent runtime: the turn
// loop, tool calling, hooks, permissions, LLM providers (bring-your-own-key),
// memory, and the Agent Definition. It knows nothing about analytics or any
// other product. A consumer injects a ToolSet, a Policy, a MemoryStore, and an
// AgentDefinition; agentcore drives them.
//
// Boundary rule: this package imports nothing from consumer packages
// (agentruntime) or storage. Everything product-specific enters through the
// interfaces defined here.
package agentcore

import "context"

// Role identifies the author of a Message.
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// Message is one entry in a conversation. ToolCalls is set on assistant
// messages that request tool execution; ToolCallID links a tool result back to
// the call that produced it.
type Message struct {
	Role       Role       `json:"role"`
	Content    string     `json:"content"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	Name       string     `json:"name,omitempty"` // tool name for tool-result messages
	// Usage is the provider-reported token usage for the turn that produced this
	// message. Set only on assistant messages, and only when the provider
	// reported it. Compaction prefers this over a byte heuristic to decide when
	// the context window is filling (pi's usage-based estimateContextTokens).
	Usage *Usage `json:"usage,omitempty"`
	// Error, when set, marks a synthesized failure turn: an empty-content
	// assistant message the loop appends when a run aborts on a provider or hook
	// error, so a subscriber always sees a clean message/turn lifecycle (pi's
	// createFailureMessage). It carries the failure reason; it is not produced by
	// the model.
	Error string `json:"error,omitempty"`
}

// ToolCall is a model request to invoke a tool with JSON arguments.
type ToolCall struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"` // raw JSON, validated before execution
}

// ToolSchema is the JSON-schema advertisement of a tool to the model.
type ToolSchema struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"` // JSON Schema object
}

// Usage carries token/cost accounting surfaced from a provider response.
//
// Cache tokens are kept as their own categories, never folded into InputTokens,
// so cost is honest on long runs where a large stable prefix is served from the
// provider's prompt cache (pi's cacheRead/cacheWrite accounting). The neutral
// contract is: InputTokens counts only full-price uncached input; CacheReadTokens
// is the prefix served from cache (billed at a steep discount); CacheWriteTokens
// is the prefix written into the cache this call (Anthropic's premium cache
// creation). Each provider normalizes its own wire format onto these fields — e.g.
// OpenAI reports prompt_tokens *including* cached, so its adapter subtracts the
// cached portion to keep InputTokens full-price-only.
type Usage struct {
	InputTokens      int     `json:"input_tokens"`
	OutputTokens     int     `json:"output_tokens"`
	CacheReadTokens  int     `json:"cache_read_tokens,omitempty"`
	CacheWriteTokens int     `json:"cache_write_tokens,omitempty"`
	CostUSD          float64 `json:"cost_usd"`
}

// ChatRequest is one provider call: the message history plus the tool schemas
// the model may call (already filtered to enabled scopes by the loop).
type ChatRequest struct {
	Model       string       `json:"model"`
	Messages    []Message    `json:"messages"`
	Tools       []ToolSchema `json:"tools,omitempty"`
	Temperature float64      `json:"temperature,omitempty"`
	MaxTokens   int          `json:"max_tokens,omitempty"`
	// CacheKey, when set, opts this call into provider prompt caching: a provider
	// that supports it reuses a cached prefix across calls sharing the key (OpenAI's
	// prompt_cache_key; Anthropic marks the stable prefix with cache_control). It is
	// opt-in and empty by default, so providers and OpenAI-compatible servers that
	// don't recognize it are unaffected — long sessions that set it turn the growing,
	// stable prefix into a cheap cache-read instead of re-billing it every turn.
	CacheKey string `json:"cache_key,omitempty"`
	// CacheRetention hints how long the provider should retain the cached prefix
	// ("" | "short" | "long" | "24h"). Providers that don't support it ignore it.
	CacheRetention string `json:"cache_retention,omitempty"`
	// ReasoningEffort, when set ("low" | "medium" | "high"), asks a reasoning
	// model to spend that much thinking per turn. Mapped to the OpenAI wire's
	// reasoning_effort; providers without the knob ignore it. Empty sends
	// nothing, so strict compat servers are unaffected.
	ReasoningEffort string `json:"reasoning_effort,omitempty"`
}

// ChatResponse is one assistant turn. StopReason is the model's explicit reason
// for stopping ("stop", "tool_calls", "length", ...), mirrored from pi-ai.
type ChatResponse struct {
	Message    Message `json:"message"`
	StopReason string  `json:"stop_reason"`
	Usage      Usage   `json:"usage"`
}

// ChatDelta is a streamed increment. Done marks the final delta and carries the
// terminal StopReason + Usage.
type ChatDelta struct {
	ContentDelta string    `json:"content_delta,omitempty"`
	ToolCall     *ToolCall `json:"tool_call,omitempty"`
	Done         bool      `json:"done,omitempty"`
	StopReason   string    `json:"stop_reason,omitempty"`
	Usage        Usage     `json:"usage,omitempty"`
	Err          error     `json:"-"`
}

// LLMProvider is the narrow multi-provider seam. Starting with OpenAI; adding a
// vendor is additive (a new implementation or a compat entry), never a change
// to agent.go.
type LLMProvider interface {
	Name() string
	Chat(ctx context.Context, req ChatRequest) (ChatResponse, error)
	Stream(ctx context.Context, req ChatRequest) (<-chan ChatDelta, error)
	SupportsTools() bool
}

// KeyUpdater is an optional LLMProvider capability: a provider that holds a
// mutable API key may have it re-resolved before each turn (pi's per-turn
// getApiKey), so long autonomous runs survive expiring BYO OAuth/short-lived
// tokens. Providers that don't implement it keep the key they were built with.
type KeyUpdater interface {
	UpdateAPIKey(key string)
}
