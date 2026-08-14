// Package ai is the LLM provider layer (the analogue of @earendil-works/pi-ai).
//
// It owns the wire: the OpenAI chat-completions protocol (also used for Google
// Gemini and any OpenAI-compatible base URL) and the Anthropic Messages
// protocol, plus the embeddings endpoint behind agentcore.Embedder. A vendor is
// config — vendor id + key + optional base URL + a Compat entry — not a new
// codec. agentcore holds only the agentcore.LLMProvider interface and never
// imports this package, which is what keeps the runtime free of vendor
// specifics.
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
package ai
