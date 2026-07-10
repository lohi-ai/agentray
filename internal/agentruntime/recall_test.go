package agentruntime

import (
	"testing"

	"github.com/lohi-ai/agentray/internal/storage"
)

// TestRankByVectorOrdersBySimilarity verifies semantic (not lexical) ranking:
// the row whose embedding is closest to the query vector ranks first even though
// a lexically-unrelated row shares no words with the query. This is the property
// vector recall adds over the ILIKE keyword path.
func TestRankByVectorOrdersBySimilarity(t *testing.T) {
	// Query points "up". The relevant memory's vector is near it; the distractor
	// points the other way, so cosine — not keyword overlap — decides the order.
	query := []float32{1, 1, 0}
	rows := []storage.AgentMemoryRow{
		{ID: "distractor", Content: "users love the new export button", Embedding: []float32{-1, -1, 0}},
		{ID: "relevant", Content: "signup funnel drop-off spiked", Embedding: []float32{0.95, 1.05, 0}},
		{ID: "neutral", Content: "weekly active flat", Embedding: []float32{0, 0, 1}},
		{ID: "no-embedding", Content: "legacy row", Embedding: nil},
	}

	ranked := rankByVector(query, rows, 2)
	if len(ranked) != 2 {
		t.Fatalf("expected top-2, got %d", len(ranked))
	}
	if ranked[0].ID != "relevant" {
		t.Fatalf("expected 'relevant' ranked first, got %q", ranked[0].ID)
	}
	for _, r := range ranked {
		if r.ID == "no-embedding" {
			t.Fatalf("rows without an embedding must be excluded from vector ranking")
		}
	}
}

// TestRankByVectorZeroLimitReturnsAll confirms limit<=0 ranks every embedded row
// (the candidate set is already bounded upstream by vectorCandidateCap).
func TestRankByVectorZeroLimitReturnsAll(t *testing.T) {
	query := []float32{1, 0}
	rows := []storage.AgentMemoryRow{
		{ID: "a", Embedding: []float32{1, 0}},
		{ID: "b", Embedding: []float32{0, 1}},
	}
	if got := rankByVector(query, rows, 0); len(got) != 2 {
		t.Fatalf("expected all embedded rows with limit<=0, got %d", len(got))
	}
}
