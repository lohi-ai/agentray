package agentcore

import "testing"

// With no hook shaping the request, the request IS the history, so the anchor
// goes on the last message — the whole turn becomes the cached prefix and the
// next turn re-reads it.
func TestMarkCacheAnchorsStampsFinalMessage(t *testing.T) {
	msgs := []Message{
		{Role: RoleSystem, Content: "sys"},
		{Role: RoleUser, Content: "task"},
		{Role: RoleAssistant, Content: "done"},
	}
	out := markCacheAnchors(msgs, msgs, "session-1")
	if !out[len(out)-1].CacheAnchor {
		t.Fatal("final message must carry the moving anchor")
	}
	for i := 0; i < len(out)-1; i++ {
		if out[i].CacheAnchor {
			t.Fatalf("unexpected anchor on message %d", i)
		}
	}
	// The persisted history must never carry anchors.
	for i, m := range msgs {
		if m.CacheAnchor {
			t.Fatalf("input slice mutated: anchor on message %d", i)
		}
	}
}

// The case the placement policy exists for. A ContextHook shapes the request
// without entering persisted history, so its trailer is regenerated every turn.
// Anchoring on it caches a prefix that cannot be a prefix of the next request,
// and the run pays the cache-write premium forever without a single read back.
func TestMarkCacheAnchorsSkipsAHookInjectedTrailer(t *testing.T) {
	history := []Message{
		{Role: RoleSystem, Content: "sys"},
		{Role: RoleUser, Content: "task"},
		{Role: RoleAssistant, Content: "done"},
	}
	req := append(append([]Message(nil), history...),
		Message{Role: RoleSystem, Content: "[run plan]\n[~] step one"})

	out := markCacheAnchors(req, history, "k")
	if out[len(out)-1].CacheAnchor {
		t.Fatal("the anchor is on the regenerated trailer: every cache entry it writes is " +
			"unreadable on the next turn, because that trailer will have been re-rendered")
	}
	if !out[len(history)-1].CacheAnchor {
		t.Fatalf("the anchor should sit at the end of the append-only history (index %d): that is "+
			"the longest prefix guaranteed to still be a prefix next turn", len(history)-1)
	}
}

// A hook that rewrites an EARLIER message (redaction) breaks the append-only
// guarantee from that point on, so the anchor stops before it. Conservative, but
// what it writes can actually be read.
func TestMarkCacheAnchorsStopsAtARewrittenMessage(t *testing.T) {
	history := []Message{
		{Role: RoleSystem, Content: "sys"},
		{Role: RoleUser, Content: "my password is hunter2"},
		{Role: RoleAssistant, Content: "done"},
	}
	req := []Message{
		{Role: RoleSystem, Content: "sys"},
		{Role: RoleUser, Content: "my password is [redacted]"},
		{Role: RoleAssistant, Content: "done"},
	}
	out := markCacheAnchors(req, history, "k")
	if !out[0].CacheAnchor {
		t.Fatalf("the anchor should stop at the last message the request and the history agree on")
	}
	for i := 1; i < len(out); i++ {
		if out[i].CacheAnchor {
			t.Fatalf("anchored at %d, past the point where the request diverges from history", i)
		}
	}
}

func TestMarkCacheAnchorsClearsStaleMarks(t *testing.T) {
	// A hook (or a bug) leaving anchors on history must not accumulate into
	// more breakpoints than a provider allows.
	msgs := []Message{
		{Role: RoleUser, Content: "a", CacheAnchor: true},
		{Role: RoleAssistant, Content: "b", CacheAnchor: true},
		{Role: RoleUser, Content: "c"},
	}
	out := markCacheAnchors(msgs, msgs, "k")
	got := 0
	for _, m := range out {
		if m.CacheAnchor {
			got++
		}
	}
	if got != 1 || !out[2].CacheAnchor {
		t.Fatalf("want exactly one anchor on the final message, got %d", got)
	}
}

func TestMarkCacheAnchorsNoopWithoutCacheKey(t *testing.T) {
	msgs := []Message{{Role: RoleUser, Content: "a"}}
	out := markCacheAnchors(msgs, msgs, "")
	if out[0].CacheAnchor {
		t.Fatal("no cacheKey must mean no anchors")
	}
}

// With nothing known-stable there is no honest place to put a breakpoint, and
// writing one anyway costs the write premium for an entry that cannot be read.
func TestMarkCacheAnchorsWritesNothingWhenNothingIsStable(t *testing.T) {
	req := []Message{{Role: RoleUser, Content: "a"}}
	out := markCacheAnchors(req, nil, "k")
	for i, m := range out {
		if m.CacheAnchor {
			t.Fatalf("anchored at %d with no history to vouch for it", i)
		}
	}
}
