package agentcore

import (
	"context"
	"math"
	"time"
)

// MemoryKind classifies a long-term memory entry.
type MemoryKind string

const (
	MemoryFact     MemoryKind = "fact"
	MemoryLearning MemoryKind = "learning"
	MemoryOutcome  MemoryKind = "outcome"
)

// MemoryEntry is one durable, distilled fact the agent carries across runs.
// Recalled into the Perceive step by tag/keyword match (v1) and injected after
// AGENTS.md. PII is redacted before persistence (§7).
type MemoryEntry struct {
	ID         string     `json:"id"`
	ScopeID    string     `json:"scope_id"`
	Kind       MemoryKind `json:"kind"`
	Content    string     `json:"content"`
	Tags       []string   `json:"tags"`
	Confidence float64    `json:"confidence"`
	SourceRun  string     `json:"source_run_id"`
	CreatedAt  time.Time  `json:"created_at"`
}

// Session is a working-memory thread: the message history of one run, persisted
// so a chat or long autonomous run can resume or be inspected.
type Session struct {
	ID        string    `json:"id"`
	ScopeID   string    `json:"scope_id"`
	ParentID  string    `json:"parent_id,omitempty"` // set when forked
	Messages  []Message `json:"messages"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// MemoryStore is the two-tier memory seam the consumer backs (Growth Analyst ->
// Postgres). Working memory is the SessionRepo half; long-term memory is the
// durable half. A nil MemoryStore is valid — the agent simply runs without
// recall or persistence.
type MemoryStore interface {
	// Recall returns long-term entries relevant to the query for a scope.
	Recall(ctx context.Context, scopeID, query string, limit int) ([]MemoryEntry, error)
	// Remember persists a long-term entry (PII already redacted by the caller).
	Remember(ctx context.Context, entry MemoryEntry) error

	// CreateSession starts a working-memory thread.
	CreateSession(ctx context.Context, scopeID string) (Session, error)
	// SaveSession persists the message history of a thread.
	SaveSession(ctx context.Context, s Session) error
	// Fork branches a thread so the agent can explore without losing the trunk.
	Fork(ctx context.Context, sessionID string) (Session, error)
}

// Embedder turns text into dense vectors for semantic memory recall (§14.7).
// It is the embedding analogue of LLMProvider: a narrow, product-agnostic seam
// the consumer backs with a real vendor (OpenAIEmbedder) or a test fake. A nil
// Embedder means the consumer falls back to keyword recall — embeddings are an
// additive relevance upgrade, never a hard dependency.
type Embedder interface {
	// Embed returns one vector per input string, index-aligned. All returned
	// vectors share the same dimension.
	Embed(ctx context.Context, texts []string) ([][]float32, error)
}

// Cosine is the cosine similarity of two equal-length vectors, in [-1, 1].
// Mismatched lengths or a zero-magnitude vector yield 0 (no signal) rather than
// NaN, so ranking degrades gracefully instead of panicking.
func Cosine(a, b []float32) float64 {
	if len(a) == 0 || len(a) != len(b) {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		x, y := float64(a[i]), float64(b[i])
		dot += x * y
		na += x * x
		nb += y * y
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}
