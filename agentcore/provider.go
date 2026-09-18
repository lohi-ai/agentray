// The wire seam: the vocabulary of one model call.
//
// LLMProvider is the only way the loop reaches a model, and it is deliberately
// small enough that an implementation is a translation layer and nothing more —
// no retry, no caching policy, no escalation. Those are the loop's, so that
// changing vendors cannot change how the agent behaves under failure. The
// package doc, including the boundary rules this file rests on, is in doc.go.
package agentcore

import (
	"context"
	"fmt"
)

// CapabilitySupport is a tri-state answer about one model feature. Unknown is
// deliberately distinct from unsupported: third-party and local providers
// frequently omit capability metadata, and treating absence as false would
// silently remove working features after an upgrade.
type CapabilitySupport string

const (
	CapabilityUnknown     CapabilitySupport = ""
	CapabilitySupported   CapabilitySupport = "supported"
	CapabilityUnsupported CapabilitySupport = "unsupported"
)

// ModelCapabilities describes request features a provider/model pair accepts.
// It is provider-neutral policy data, not a catalog: adapters may derive it
// from their wire contract, live discovery, or both. Unknown fields preserve
// the request exactly as authored.
type ModelCapabilities struct {
	Tools             CapabilitySupport `json:"tools,omitempty"`
	ToolChoice        CapabilitySupport `json:"tool_choice,omitempty"`
	ReasoningEffort   CapabilitySupport `json:"reasoning_effort,omitempty"`
	ImageInput        CapabilitySupport `json:"image_input,omitempty"`
	StructuredOutput  CapabilitySupport `json:"structured_output,omitempty"`
	PromptCaching     CapabilitySupport `json:"prompt_caching,omitempty"`
	StatefulResponses CapabilitySupport `json:"stateful_responses,omitempty"`
}

// Overlay returns c with every known field from newer replacing it. This is
// used when live model metadata refines conservative adapter defaults.
func (c ModelCapabilities) Overlay(newer ModelCapabilities) ModelCapabilities {
	if newer.Tools != CapabilityUnknown {
		c.Tools = newer.Tools
	}
	if newer.ToolChoice != CapabilityUnknown {
		c.ToolChoice = newer.ToolChoice
	}
	if newer.ReasoningEffort != CapabilityUnknown {
		c.ReasoningEffort = newer.ReasoningEffort
	}
	if newer.ImageInput != CapabilityUnknown {
		c.ImageInput = newer.ImageInput
	}
	if newer.StructuredOutput != CapabilityUnknown {
		c.StructuredOutput = newer.StructuredOutput
	}
	if newer.PromptCaching != CapabilityUnknown {
		c.PromptCaching = newer.PromptCaching
	}
	if newer.StatefulResponses != CapabilityUnknown {
		c.StatefulResponses = newer.StatefulResponses
	}
	return c
}

// Validate rejects capability values outside the wire contract. It is used at
// configuration boundaries so an arbitrary string cannot become a silently
// ignored fourth state.
func (c ModelCapabilities) Validate() error {
	for name, value := range map[string]CapabilitySupport{
		"tools": c.Tools, "tool_choice": c.ToolChoice,
		"reasoning_effort": c.ReasoningEffort, "image_input": c.ImageInput,
		"structured_output": c.StructuredOutput, "prompt_caching": c.PromptCaching,
		"stateful_responses": c.StatefulResponses,
	} {
		if value != CapabilityUnknown && value != CapabilitySupported && value != CapabilityUnsupported {
			return fmt.Errorf("%s capability must be supported, unsupported, or empty", name)
		}
	}
	return nil
}

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
	// Directive marks a user message as something the HUMAN asked for: the run's
	// task, or a correction they steered in mid-run. It separates those from the
	// user-role messages the framework synthesizes on its own — a goal-gate
	// nudge, a budget wrap-up, an extension's injection — which look identical
	// to a provider and must not be mistaken for what the run is FOR.
	//
	// It exists because a long run's requirement is not fixed. Compaction pins
	// the objective so successive lossy summaries cannot erode it, and a pin
	// built from "the first user message" pins the requirement the user has
	// since changed — the one thing worse than forgetting the objective is
	// remembering a superseded one verbatim while the correction decays. The
	// loop stamps this at the two places human input enters (the seed task, the
	// steering and follow-up queues), so compaction can keep the pin current
	// without guessing from message text.
	//
	// Persisted, so a resumed run rebuilds the same pin. False on a message from
	// an older log predates the field; compaction falls back to the first user
	// message there, which is what it always did.
	Directive bool `json:"directive,omitempty"`
	// CacheAnchor marks this message as a prompt-cache breakpoint candidate:
	// "the prefix ending here is stable — cache it". Placement is decided by the
	// loop (markCacheAnchors), never by a provider; each provider maps anchors
	// onto its native mechanism (Anthropic: cache_control on the message's last
	// block) or ignores them (OpenAI/Gemini cache implicitly by prefix). Request-
	// scoped only — never persisted, so it is excluded from JSON.
	CacheAnchor bool `json:"-"`
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
	// CostUnpriced is true when CostUSD is NOT a real total — some or all of the
	// tokens above were billed by a model with no entry in the price table, so
	// the price lookup returned "unknown" rather than "zero". Zero-value default
	// (false) means "priced": every Usage built by hand (tests, synthetic zero-
	// cost turns, error responses) is trusted as accurate unless the one place
	// that actually resolves a price (observe.tracingProvider.price) says
	// otherwise. A consumer MUST check this before rendering CostUSD as a dollar
	// figure — "$0.00" and "we don't know" must never look the same to a reader
	// deciding whether to trust the number.
	CostUnpriced bool `json:"cost_unpriced,omitempty"`
}

// ChatRequest is one provider call: the message history plus the tool schemas
// the model may call (already filtered to enabled scopes by the loop).
type ChatRequest struct {
	Model       string       `json:"model"`
	Messages    []Message    `json:"messages"`
	Tools       []ToolSchema `json:"tools,omitempty"`
	Temperature float64      `json:"temperature,omitempty"`
	MaxTokens   int          `json:"max_tokens,omitempty"`
	// SessionID is the stable logical-conversation identity providers may use
	// for request affinity or server-side turn chaining. It is deliberately
	// distinct from the durable run-log id: one user conversation spans several
	// short-lived Agent instances and run logs.
	SessionID string `json:"session_id,omitempty"`
	// ProviderSession carries provider-private mutable state across those Agent
	// instances. It is never serialized onto a provider wire; adapters opt into
	// concrete records through ProviderSession.State.
	ProviderSession *ProviderSession `json:"-"`
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
	// OutputSchema, when non-nil, constrains the model's text answer to the
	// given JSON Schema (grammar-constrained decoding). OpenAI maps it to
	// response_format json_schema with strict:true; Anthropic to the
	// structured-outputs output_format (plus its beta header). Providers
	// without the capability may ignore it; the loop validates every final text
	// answer locally before accepting it.
	// Intended for verdict-shaped agents (classification / moderation) — tool
	// calls are unaffected, but any plain-text turn must fit the schema, so
	// leave it nil for general chat agents.
	OutputSchema *OutputSchema `json:"output_schema,omitempty"`
}

// OutputSchema names a JSON Schema that constrains the assistant's text
// output. Name is required by the OpenAI wire (defaulted to "output" when
// empty); Schema is a draft-07-style JSON Schema object.
type OutputSchema struct {
	Name   string         `json:"name,omitempty"`
	Schema map[string]any `json:"schema"`
	// Strict opts into OpenAI's strict structured outputs (grammar-constrained
	// decoding). Set it ONLY when Schema fits OpenAI's strict subset (every
	// object needs additionalProperties:false and all properties required, no
	// unsupported keywords) — a non-conforming schema is rejected with a 400 on
	// every turn. The default (false) sends the schema in best-effort mode,
	// which accepts any valid JSON Schema and soft-degrades. Either way, the
	// loop independently validates the final answer before accepting it.
	Strict bool `json:"strict,omitempty"`
}

// ChatResponse is one assistant turn. StopReason is the model's explicit reason
// for stopping ("stop", "tool_calls", "length", ...), mirrored from pi-ai.
type ChatResponse struct {
	Message    Message `json:"message"`
	StopReason string  `json:"stop_reason"`
	Usage      Usage   `json:"usage"`
}

// ChatDelta is a streamed increment. Done marks the final delta and carries the
// terminal StopReason.
//
// Usage may ride ANY delta, not only the last one, and each field is read as a
// running total rather than an increment — which is how the wire formats
// actually report it (Anthropic states input tokens on message_start and
// cumulative output on message_delta; OpenAI sends one usage-only chunk after
// the chunk carrying finish_reason). The loop keeps the newest non-zero value of
// each field, so a provider that reports in pieces is still billed in full. A
// provider that reports everything on Done — both of this module's do — is the
// same case with one delta.
//
// Getting this wrong is silent: the turn still succeeds, the answer is still
// right, and only the number the budget gate meters on is zero.
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

// ModelCapabilityProvider is an optional LLMProvider capability. The model is
// explicit because one provider endpoint may serve heterogeneous local or
// routed models. Providers should return CapabilityUnknown for facts they do
// not know rather than guessing unsupported.
type ModelCapabilityProvider interface {
	ModelCapabilities(model string) ModelCapabilities
}

// CapabilitiesOf resolves the optional model-level contract and fills only the
// legacy provider-wide tools fact when the model answer is unknown. It is the
// single compatibility bridge for providers that predate ModelCapabilityProvider.
func CapabilitiesOf(provider LLMProvider, model string) ModelCapabilities {
	var caps ModelCapabilities
	if p, ok := provider.(ModelCapabilityProvider); ok {
		caps = p.ModelCapabilities(model)
	}
	if caps.Tools == CapabilityUnknown {
		if provider.SupportsTools() {
			caps.Tools = CapabilitySupported
		} else {
			caps.Tools = CapabilityUnsupported
		}
	}
	return caps
}

// KeyUpdater is an optional LLMProvider capability: a provider that holds a
// mutable API key may have it re-resolved before each turn (pi's per-turn
// getApiKey), so long autonomous runs survive expiring BYO OAuth/short-lived
// tokens. Providers that don't implement it keep the key they were built with.
type KeyUpdater interface {
	UpdateAPIKey(key string)
}
