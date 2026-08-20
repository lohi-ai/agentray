package agentruntime

import (
	"testing"
	"time"

	"github.com/lohi-ai/agentray/internal/dataplane/store"
)

// TestRankByVectorOrdersBySimilarity verifies semantic (not lexical) ranking:
// the row whose embedding is closest to the query vector ranks first even though
// a lexically-unrelated row shares no words with the query. This is the property
// vector recall adds over the ILIKE keyword path.
func TestRankByVectorOrdersBySimilarity(t *testing.T) {
	// Query points "up". The relevant memory's vector is near it; the distractor
	// points away, so cosine — not keyword overlap — decides the order. The
	// distractor sits just above recallCosineFloor on purpose: this test is about
	// ORDERING, and a distractor below the floor would be dropped before ranking
	// ever ran (the floor gets its own test below).
	query := []float32{1, 1, 0}
	rows := []storage.AgentMemoryRow{
		{ID: "distractor", Content: "users love the new export button", Embedding: []float32{1, -0.2, 0}},
		{ID: "relevant", Content: "signup funnel drop-off spiked", Embedding: []float32{0.95, 1.05, 0}},
		{ID: "neutral", Content: "weekly active flat", Embedding: []float32{1, 0.1, 0}},
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
// that clears the relevance floor (the candidate set is already bounded upstream
// by vectorCandidateCap). Both rows are deliberately on-topic: "all" has always
// meant "all the rows ranking produced", and ranking now produces only the rows
// that are actually about the query.
func TestRankByVectorZeroLimitReturnsAll(t *testing.T) {
	query := []float32{1, 0}
	rows := []storage.AgentMemoryRow{
		{ID: "a", Embedding: []float32{1, 0}},
		{ID: "b", Embedding: []float32{1, 1}},
	}
	if got := rankByVector(query, rows, 0); len(got) != 2 {
		t.Fatalf("expected all embedded rows above the floor with limit<=0, got %d", len(got))
	}
}

// TestRankByVectorDropsBelowFloor pins the relevance floor. Before it, an
// off-topic task still returned the eight highest-cosine rows in the scope
// however low those cosines were, and they then sat in the system prefix for the
// whole run. A row the query is orthogonal to is not a low-ranked memory — it is
// not a memory about this query at all.
func TestRankByVectorDropsBelowFloor(t *testing.T) {
	query := []float32{1, 0}
	rows := []storage.AgentMemoryRow{
		{ID: "on-topic", Embedding: []float32{1, 0.1}},
		{ID: "orthogonal", Embedding: []float32{0, 1}},
		{ID: "opposed", Embedding: []float32{-1, 0}},
	}
	ranked := rankByVector(query, rows, 8)
	if len(ranked) != 1 || ranked[0].ID != "on-topic" {
		t.Fatalf("ranked %v, want only the on-topic row — the floor is not dropping unrelated memories", ids(ranked))
	}

	// Everything below the floor means no vector ranking at all, which the
	// caller reads as "fall back to keyword recall" rather than as an empty
	// answer. That is the pre-existing contract, not a new failure mode.
	if got := rankByVector(query, rows[1:], 8); len(got) != 0 {
		t.Fatalf("ranked %v, want nothing when every candidate is below the floor", ids(got))
	}
}

// TestRankByVectorConfidenceBreaksTie: confidence is written on every memory
// (0.7 when the model chose to remember it, 0.6 when the reflection pass
// inferred it) and, until now, read by nothing. It settles rows the vector
// cannot separate — and only those, since the weight is bounded.
func TestRankByVectorConfidenceBreaksTie(t *testing.T) {
	query := []float32{1, 0}
	seen := time.Now()
	rows := []storage.AgentMemoryRow{
		{ID: "inferred", Embedding: []float32{1, 0}, Confidence: 0.6, LastSeenAt: seen},
		{ID: "asserted", Embedding: []float32{1, 0}, Confidence: 0.7, LastSeenAt: seen},
	}
	ranked := rankByVector(query, rows, 2)
	if len(ranked) != 2 || ranked[0].ID != "asserted" {
		t.Fatalf("ranked %v, want the model-asserted memory ahead of the inferred one at equal cosine", ids(ranked))
	}

	// Bounded, not dominant: an unset/legacy confidence of 0 must not zero a row
	// out of recall, it must only be discounted.
	legacy := []storage.AgentMemoryRow{
		{ID: "legacy", Embedding: []float32{1, 0}, Confidence: 0, LastSeenAt: seen},
	}
	if got := rankByVector(query, legacy, 2); len(got) != 1 {
		t.Fatalf("a legacy row with no confidence was dropped from recall entirely")
	}
}

// TestRankByVectorRecencyNeverOutranksRelevance pins the bound on the recency
// term. Recency settles near-ties; it must never promote a loosely-related
// memory over a materially more relevant one, which is why the multiplier is
// (0.7 + 0.3*decay) rather than the decay itself.
func TestRankByVectorRecencyNeverOutranksRelevance(t *testing.T) {
	query := []float32{1, 0}
	now := time.Now()
	rows := []storage.AgentMemoryRow{
		{ID: "fresh-but-loose", Embedding: []float32{1, 1.5}, Confidence: 0.7, LastSeenAt: now},
		{ID: "stale-but-relevant", Embedding: []float32{1, 0}, Confidence: 0.7, LastSeenAt: now.Add(-365 * 24 * time.Hour)},
	}
	ranked := rankByVector(query, rows, 2)
	if len(ranked) != 2 || ranked[0].ID != "stale-but-relevant" {
		t.Fatalf("ranked %v, want the year-old on-topic memory first — recency is outranking relevance", ids(ranked))
	}

	// At equal cosine and confidence, though, recency does decide — and it reads
	// last SEEN, so a memory the agent re-confirmed counts as recent even when it
	// was first written long ago.
	tie := []storage.AgentMemoryRow{
		{ID: "old", Embedding: []float32{1, 0}, Confidence: 0.7, LastSeenAt: now.Add(-120 * 24 * time.Hour)},
		{ID: "reconfirmed", Embedding: []float32{1, 0}, Confidence: 0.7, CreatedAt: now.Add(-365 * 24 * time.Hour), LastSeenAt: now},
	}
	if got := rankByVector(query, tie, 2); len(got) != 2 || got[0].ID != "reconfirmed" {
		t.Fatalf("ranked %v, want the re-confirmed memory first at equal cosine", ids(got))
	}
}

// TestRecencyDecayUnknownAgeIsNotFresh: rows written before last_seen_at existed
// carry a zero timestamp. Such a row must fall back to created_at and, failing
// that, score as fully decayed — an unknown age must not outrank a known-recent
// memory.
func TestRecencyDecayUnknownAgeIsNotFresh(t *testing.T) {
	now := time.Now()
	if got := recencyDecay(storage.AgentMemoryRow{}, now); got != 0 {
		t.Errorf("recencyDecay(no timestamps) = %v, want 0", got)
	}
	if got := recencyDecay(storage.AgentMemoryRow{CreatedAt: now}, now); got != 1 {
		t.Errorf("recencyDecay(created now, never re-seen) = %v, want 1 via the created_at fallback", got)
	}
	halved := recencyDecay(storage.AgentMemoryRow{LastSeenAt: now.Add(-recallHalfLife)}, now)
	if halved < 0.49 || halved > 0.51 {
		t.Errorf("recencyDecay(one half-life old) = %v, want ~0.5", halved)
	}
}

func ids(rows []storage.AgentMemoryRow) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.ID)
	}
	return out
}
