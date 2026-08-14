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
