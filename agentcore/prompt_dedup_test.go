package agentcore

import (
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
