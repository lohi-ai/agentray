package agentcore

// cacheanchor.go — provider-neutral prompt-cache breakpoint placement. The
// loop, not any single provider, knows which prefixes of a request are stable
// across turns; it expresses that as Message.CacheAnchor marks on the outgoing
// request view, and each provider maps the marks onto its own mechanism
// (Anthropic: explicit cache_control breakpoints) or ignores them (providers
// with implicit prefix caching). New placement policies belong here — never in
// a provider's encode.

// markCacheAnchors returns the request view of msgs with one anchor placed at
// the end of the request's APPEND-ONLY prefix.
//
// A breakpoint tells the provider to store everything up to and including that
// message. The entry is worth something only if it is still a prefix of the next
// turn's request; otherwise the run pays the cache-WRITE premium every turn and
// never reads one back.
//
// The anchor used to go on the final message, which is right for a request that
// is nothing but the conversation — that grows by appending, so the whole of
// turn N is a prefix of turn N+1. It is wrong as soon as a ContextHook appends
// something. A hook's output shapes the request WITHOUT entering persisted
// history (that is the point of the seam: the todo plugin re-renders the live
// plan into every request, retrieval injects fresh context), so the trailer is
// regenerated each turn and sits exactly where the anchor was going. The cached
// prefix then ends inside content guaranteed to differ, and every entry misses.
// Measured on a 300-turn run with the plan pinned: **7 of 299 cache entries were
// still a prefix of the next request**, while 90% of the request bytes were
// unchanged — the reuse was there and the breakpoint was past it.
//
// So the anchor goes at the end of what the loop KNOWS is append-only: the
// persisted history, identified as the common prefix of history and the
// post-hook request. That handles a trailer (anchor lands on the last real
// message) and a hook that rewrites an earlier message (anchor stops before it,
// conservative but re-readable) with the same rule, and needs no state.
//
// The slice (and any anchored element) is copied before marking, so persisted
// history never carries anchors and stale marks from earlier turns can never
// accumulate into more breakpoints than a provider allows.
func markCacheAnchors(msgs []Message, history []Message, cacheKey string) []Message {
	if cacheKey == "" || len(msgs) == 0 {
		return msgs
	}
	out := make([]Message, len(msgs))
	copy(out, msgs)
	for i := range out {
		out[i].CacheAnchor = false
	}
	idx := stablePrefixLen(history, out) - 1
	if idx < 0 {
		// Nothing is known-stable (a hook rewrote the very first message, or
		// there is no history yet). Anchoring anywhere would write an entry that
		// cannot be read, so write none.
		return out
	}
	out[idx].CacheAnchor = true
	return out
}

// stablePrefixLen returns how many leading messages the persisted history and
// the outgoing request agree on. Everything before that point is append-only by
// construction and will still be there, unchanged, next turn.
func stablePrefixLen(history, req []Message) int {
	n := 0
	for n < len(history) && n < len(req) && sameForCache(history[n], req[n]) {
		n++
	}
	return n
}

// sameForCache compares two messages on what a provider actually serializes, so
// a difference the wire never sees (CacheAnchor itself) does not break a prefix.
func sameForCache(a, b Message) bool {
	if a.Role != b.Role || a.Content != b.Content || a.Name != b.Name || a.ToolCallID != b.ToolCallID {
		return false
	}
	if len(a.ToolCalls) != len(b.ToolCalls) {
		return false
	}
	for i := range a.ToolCalls {
		if a.ToolCalls[i] != b.ToolCalls[i] {
			return false
		}
	}
	return true
}
