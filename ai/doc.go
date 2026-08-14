// Package ai is the LLM provider layer (the analogue of @earendil-works/pi-ai).
//
// A Collection holds one or many registered providers. Each provider owns its
// auth, its live model list (from the vendor list-models HTTP API), and
// Chat/Stream. A request for a model is served by the owning provider.
//
// Wire protocols stay shared: OpenAI chat-completions (also used for Google
// Gemini and any OpenAI-compatible base URL) and Anthropic Messages. Providers
// are config (vendor + key + optional base URL), not a new codec.
package ai
