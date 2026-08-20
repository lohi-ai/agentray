package agentcore

import (
	"fmt"
	"strings"
	"testing"
)

// The recalled-memory block must not re-pay tokens for paraphrases of the same
// fact, but must keep genuinely distinct facts.
func TestDedupRecalledDropsParaphrasesKeepsDistinct(t *testing.T) {
	recalled := []MemoryEntry{
		{Kind: "learning", Content: "Top page by traffic is the homepage ('/'), followed by the novel details page for Kiem Lai ('/truyen/kiem-lai'), user profile ('/profile'), and admin novels manager ('/admin/novels')."},
		{Kind: "learning", Content: "The top page by traffic is the homepage ('/'), followed by the novel page '/truyen/kiem-lai' and user profile '/profile'."},
		{Kind: "learning", Content: "Top pages by traffic this week are the homepage (/), followed by the novel details page for kiem-lai (/truyen/kiem-lai), /profile, and admin pages (/admin/novels)."},
		{Kind: "preference", Content: "The user prefers stacked-area charts for time-series dashboards."},
	}
	got := dedupRecalled(recalled)
	if len(got) < 2 {
		t.Fatalf("dedup collapsed too much: %d kept", len(got))
	}
	// The distinct preference fact must survive.
	foundPref := false
	for _, m := range got {
		if strings.Contains(m.Content, "stacked-area") {
			foundPref = true
		}
	}
	if !foundPref {
		t.Fatal("dedup dropped a distinct fact (stacked-area preference)")
	}
	// At least one of the three traffic paraphrases must be dropped.
	if len(got) >= len(recalled) {
		t.Fatalf("expected paraphrases to be deduped, kept all %d", len(got))
	}
}

// Vietnamese paraphrases that differ only by diacritics / minor wording must be
// recognised as the same fact (accent folding), not kept as separate bullets.
func TestDedupRecalledFoldsVietnameseAccents(t *testing.T) {
	recalled := []MemoryEntry{
		{Kind: "learning", Content: "Trang có nhiều lượt truy cập nhất là trang chủ, sau đó là trang truyện Kiếm Lai và trang hồ sơ người dùng."},
		{Kind: "learning", Content: "Trang co nhieu luot truy cap nhat la trang chu, sau do la trang truyen Kiem Lai va trang ho so nguoi dung."},
		{Kind: "preference", Content: "Người dùng thích biểu đồ vùng xếp chồng cho dữ liệu theo thời gian."},
	}
	got := dedupRecalled(recalled)
	if len(got) != 2 {
		t.Fatalf("expected accent-only paraphrase to collapse to 2 bullets, got %d", len(got))
	}
}

func TestDedupRecalledNoFalsePositives(t *testing.T) {
	recalled := []MemoryEntry{
		{Kind: "learning", Content: "D7 retention is 22 percent for new readers."},
		{Kind: "learning", Content: "The payment conversion funnel drops most at the checkout step."},
		{Kind: "learning", Content: "Mobile accounts for 80 percent of sessions."},
	}
	if got := dedupRecalled(recalled); len(got) != 3 {
		t.Fatalf("distinct facts were wrongly deduped: %d/3 kept", len(got))
	}
}

// The recalled block is assembled into the system prefix and therefore charged
// on every turn of the run, so its size has to be bounded — nothing did that
// before. These pin both clamps, and that they clamp SIZE only: the block's
// heading and caveat wording are untouched.
func TestRecalledBlockClampsPerEntry(t *testing.T) {
	long := strings.Repeat("a", maxRecallEntryBytes*3)
	got := buildSystemPrompt(AgentDefinition{}, []MemoryEntry{{Kind: "fact", Content: long}}, nil)
	if !strings.Contains(got, "# Recalled memory") {
		t.Fatal("the recalled block is missing entirely")
	}
	if strings.Contains(got, long) {
		t.Fatal("a single memory was injected at full length; the per-entry clamp did not apply")
	}
	if !strings.Contains(got, "…[truncated]") {
		t.Error("the clamped entry does not say it was truncated")
	}
	// One over-long memory must not cost more than its own budget plus the
	// bullet's own few bytes of framing.
	if n := len(got); n > maxRecallEntryBytes+len(responseFormattingGuidance)+512 {
		t.Errorf("prompt is %d bytes for one clamped memory — the clamp is not bounding the entry", n)
	}
}

func TestRecalledBlockClampsTotalSize(t *testing.T) {
	// Distinct facts (dedup must not be what removes them), each near the
	// per-entry cap, so together they blow the block budget several times over.
	var recalled []MemoryEntry
	for i := 0; i < 40; i++ {
		recalled = append(recalled, MemoryEntry{
			Kind:    "fact",
			Content: fmt.Sprintf("fact number %d: ", i) + strings.Repeat(string(rune('a'+i%26)), maxRecallEntryBytes/2),
		})
	}
	got := buildSystemPrompt(AgentDefinition{}, recalled, nil)
	bullets := strings.Count(got, "\n- (fact) ")
	if bullets == 0 {
		t.Fatal("the whole block was clamped away; the budget must still admit what fits")
	}
	if bullets == len(recalled) {
		t.Fatal("every memory was injected; the block budget did not apply")
	}
	if n := len(got); n > maxRecallBlockBytes+len(responseFormattingGuidance)+2048 {
		t.Errorf("prompt is %d bytes, want the recalled block held near its %d-byte budget", n, maxRecallBlockBytes)
	}
}

// Clamping runs AFTER dedup, so a paraphrase cannot spend budget a distinct
// fact further down the list needed. Without that ordering the near-duplicates
// below would consume the block and the distinct fact would be cut.
func TestRecalledBlockClampsAfterDedup(t *testing.T) {
	filler := strings.Repeat("the homepage is the top page by traffic and always has been. ", 8)
	var recalled []MemoryEntry
	for i := 0; i < 30; i++ {
		recalled = append(recalled, MemoryEntry{Kind: "learning", Content: filler})
	}
	const distinct = "checkout conversion drops most at the address step"
	recalled = append(recalled, MemoryEntry{Kind: "fact", Content: distinct})

	got := buildSystemPrompt(AgentDefinition{}, recalled, nil)
	if !strings.Contains(got, distinct) {
		t.Fatal("thirty paraphrases of one learning spent the whole block budget and cut the distinct fact")
	}
}
