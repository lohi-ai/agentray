// Package ai is the LLM provider layer (the analogue of @earendil-works/pi-ai).
//
// It owns the wire: OpenAI Chat Completions (also used for Google Gemini and
// arbitrary compatible base URLs), the public OpenAI Responses API, Anthropic
// Messages, and the subscription OAuth backends, plus the embeddings endpoint
// behind agentcore.Embedder. agentcore holds only the agentcore.LLMProvider
// interface and never imports this package, which keeps the runtime free of
// vendor specifics.
//
// Two constructors, at two levels:
//
//   - NewClient(ClientSpec) returns a bare agentcore.LLMProvider — Chat and
//     Stream. That is everything a run needs, so this is what the agent runtime
//     calls.
//   - New(Spec) returns a Provider: a client plus the identity (id, vendor,
//     display name) and the live model list a workspace-managed provider needs.
//     It builds on NewClient, so a run and a list-models call cannot drift apart
//     on credentials or base URL.
//
// A Collection holds one or many registered Providers and routes a request for
// a model to the provider that owns it.
//
// Errors from a wire client are built with agentcore.NewProviderError so the
// loop's agentcore.IsRetryable can classify them structurally (status code,
// Retry-After) rather than by string-matching a message. A provider that
// returns a plain error is silently un-retryable — the run will escalate to a
// pricier rung instead of retrying a 429.
//
// Providers may keep private, conversation-scoped records in
// agentcore.ProviderSession. The OpenAI-compatible adapter uses it to remember
// explicit optional-parameter rejections across rebuilt clients. The public
// Responses adapter additionally retains prefix-checked previous_response_id
// chains; pooled OAuth providers use the account-reset contract so account-bound
// handles are invalidated without discarding endpoint-wide compatibility
// lessons. State is an optimization: a missing registry causes a full transcript
// replay but cannot change request correctness.
package ai
