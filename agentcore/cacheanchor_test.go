package agentcore

import "testing"

func TestMarkCacheAnchorsStampsFinalMessage(t *testing.T) {
	msgs := []Message{
		{Role: RoleSystem, Content: "sys"},
		{Role: RoleUser, Content: "task"},
		{Role: RoleAssistant, Content: "done"},
	}
	out := markCacheAnchors(msgs, "session-1")
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

func TestMarkCacheAnchorsClearsStaleMarks(t *testing.T) {
	// A hook (or a bug) leaving anchors on history must not accumulate into
	// more breakpoints than a provider allows.
	msgs := []Message{
		{Role: RoleUser, Content: "a", CacheAnchor: true},
		{Role: RoleAssistant, Content: "b", CacheAnchor: true},
		{Role: RoleUser, Content: "c"},
	}
	out := markCacheAnchors(msgs, "k")
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
	out := markCacheAnchors(msgs, "")
	if out[0].CacheAnchor {
		t.Fatal("no cacheKey must mean no anchors")
	}
}

// TestAnthropicEncodeHonorsAnchors: the provider translates loop-placed
// anchors instead of deciding placement itself; system-role anchors (hoisted
// into the system block) are skipped, and only the last 3 anchors survive so
// the request stays within Anthropic's 4-breakpoint cap.
func TestAnthropicEncodeHonorsAnchors(t *testing.T) {
	p := NewAnthropicProvider("k", "")
	req := ChatRequest{
		Model:    "claude-x",
		CacheKey: "s",
		Messages: []Message{
			{Role: RoleSystem, Content: "sys", CacheAnchor: true}, // hoisted — no attachment point
			{Role: RoleUser, Content: "u1", CacheAnchor: true},
			{Role: RoleAssistant, Content: "a1", CacheAnchor: true},
			{Role: RoleUser, Content: "u2", CacheAnchor: true},
			{Role: RoleAssistant, Content: "a2", CacheAnchor: true},
			{Role: RoleUser, Content: "u3"},
		},
	}
	out := p.encode(req)
	var marked []int
	for i, m := range out.Messages {
		if m.Content[len(m.Content)-1].CacheControl != nil {
			marked = append(marked, i)
		}
	}
	// Four non-system anchors → capped to the newest three (indices 1,2,3 of
	// out.Messages: a1, u2, a2). u3 carries no anchor and must stay unmarked.
	if len(marked) != 3 || marked[0] != 1 || marked[1] != 2 || marked[2] != 3 {
		t.Fatalf("marked = %v, want [1 2 3]", marked)
	}
}

// TestAnthropicEncodeAnchorFallback: with caching on but no anchors (provider
// used standalone), the classic moving breakpoint on the final message must
// still apply.
func TestAnthropicEncodeAnchorFallback(t *testing.T) {
	p := NewAnthropicProvider("k", "")
	out := p.encode(ChatRequest{
		Model:    "claude-x",
		CacheKey: "s",
		Messages: []Message{
			{Role: RoleUser, Content: "u1"},
			{Role: RoleAssistant, Content: "a1"},
		},
	})
	last := out.Messages[len(out.Messages)-1]
	if last.Content[len(last.Content)-1].CacheControl == nil {
		t.Fatal("fallback moving breakpoint missing on final message")
	}
	first := out.Messages[0]
	if first.Content[len(first.Content)-1].CacheControl != nil {
		t.Fatal("fallback must not mark earlier messages")
	}
}
