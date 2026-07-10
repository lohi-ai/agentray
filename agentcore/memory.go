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
