package agentcore

import (
	"context"
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

// MemoryCurator is the OPTIONAL write-back half of the memory seam: a store
// that implements it lets the model revise what it previously remembered —
// update a stale fact, or retract one that turned out wrong. It is separate
// from MemoryStore (rather than new methods on it) so a consumer's existing
// store keeps satisfying the seam and simply does not offer the curation
// tool; the memory plugin discovers the capability by type assertion.
//
// Both methods are soft: a retracted row stays in the store and is filtered
// out of recall, so the history of having held the belief survives the
// retraction. There is no hard delete on this seam.
type MemoryCurator interface {
	// Supersede retracts the entry id within scopeID, optionally naming the
	// entry that replaces it ("" retracts without a successor). It must
	// report not-found when no live entry with that id exists in the scope —
	// a silent no-op would tell the model a memory is gone when it is not.
	Supersede(ctx context.Context, scopeID, id, replacementID string) error
	// Update replaces the content of the entry id within scopeID: the new
	// entry is written and the old one retracted to it, atomically, so a
	// failure never leaves a retracted memory with no successor.
	Update(ctx context.Context, scopeID, id string, entry MemoryEntry) error
}
