package ai

import (
	"testing"

	"github.com/lohi-ai/agentray/agentcore"
)

// Anchor placement is the LOOP's (agentcore/cacheanchor.go); translating a
// placed anchor into Anthropic's cache_control blocks is this package's. These
// pin the second half.

// TestAnthropicEncodeHonorsAnchors: the provider translates loop-placed
// anchors instead of deciding placement itself; system-role anchors (hoisted
// into the system block) are skipped, and only the last 3 anchors survive so
// the request stays within Anthropic's 4-breakpoint cap.
func TestAnthropicEncodeHonorsAnchors(t *testing.T) {
	p := NewAnthropicProvider("k", "")
	req := agentcore.ChatRequest{
		Model:    "claude-x",
		CacheKey: "s",
		Messages: []agentcore.Message{
			{Role: agentcore.RoleSystem, Content: "sys", CacheAnchor: true}, // hoisted — no attachment point
			{Role: agentcore.RoleUser, Content: "u1", CacheAnchor: true},
			{Role: agentcore.RoleAssistant, Content: "a1", CacheAnchor: true},
			{Role: agentcore.RoleUser, Content: "u2", CacheAnchor: true},
			{Role: agentcore.RoleAssistant, Content: "a2", CacheAnchor: true},
			{Role: agentcore.RoleUser, Content: "u3"},
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
	out := p.encode(agentcore.ChatRequest{
		Model:    "claude-x",
		CacheKey: "s",
		Messages: []agentcore.Message{
			{Role: agentcore.RoleUser, Content: "u1"},
			{Role: agentcore.RoleAssistant, Content: "a1"},
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
