package agentcore

// cacheanchor.go — provider-neutral prompt-cache breakpoint placement. The
// loop, not any single provider, knows which prefixes of a request are stable
// across turns; it expresses that as Message.CacheAnchor marks on the outgoing
// request view, and each provider maps the marks onto its own mechanism
// (Anthropic: explicit cache_control breakpoints) or ignores them (providers
// with implicit prefix caching). New placement policies belong here — never in
// a provider's encode.

// markCacheAnchors returns the request view of msgs with anchors placed by the
// current policy: one moving anchor on the final message, so the whole
// transcript-so-far becomes the cached prefix and the next turn re-reads it as
// a cache hit. A caller that doesn't opt into caching (empty cacheKey) gets
// msgs back untouched.
//
// The slice (and any anchored element) is copied before marking, so persisted
// history never carries anchors and stale marks from earlier turns can never
// accumulate into more breakpoints than a provider allows.
func markCacheAnchors(msgs []Message, cacheKey string) []Message {
	if cacheKey == "" || len(msgs) == 0 {
		return msgs
	}
	out := make([]Message, len(msgs))
	copy(out, msgs)
	for i := range out {
		out[i].CacheAnchor = false
	}
	out[len(out)-1].CacheAnchor = true
	return out
}
