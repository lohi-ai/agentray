package agentruntime

import (
	"context"
	"encoding/json"
	"sort"

	"github.com/lohi-ai/agentray/agentcore"
	"github.com/lohi-ai/agentray/internal/storage"
)

// PgMemory adapts *storage.Store to agentcore.MemoryStore. It lives in the
// consumer (not in storage) so agentcore and storage stay free of each other:
// agentcore defines the interface, storage exposes raw rows, this maps between
// them. PII redaction (§7) is applied here on the write path before persistence.
//
// Embedder is optional: when set, Remember stores a semantic vector and Recall
// ranks candidates by cosine similarity (vector recall, §14.7); when nil, both
// fall back to the keyword/tag path. The seam stays in the consumer so agentcore
// never imports storage.
type PgMemory struct {
	Store    *storage.Store
	Redact   bool               // redact known PII traits from memory content before persisting
	Denylist []string           // extra trait substrings to scrub (defaults applied if empty)
	Embedder agentcore.Embedder // optional; enables semantic recall
}

// NewPgMemory builds the adapter. redact toggles the default PII denylist.
func NewPgMemory(store *storage.Store, redact bool) *PgMemory {
	return &PgMemory{Store: store, Redact: redact}
}

var _ agentcore.MemoryStore = (*PgMemory)(nil)

// vectorCandidateCap bounds how many embedded rows are pulled for Go-side cosine
// ranking before falling back to keyword recall.
const vectorCandidateCap = 500

// Recall returns long-term entries relevant to query for a scope. With an
// Embedder it ranks embedded candidates by cosine similarity; it falls back to
// keyword/tag recall when no embedder is set, the query is empty, the embedding
// call fails, or no embedded candidates exist yet.
func (m *PgMemory) Recall(ctx context.Context, scopeID, query string, limit int) ([]agentcore.MemoryEntry, error) {
	if m.Embedder != nil && query != "" {
		if entries, ok := m.recallByVector(ctx, scopeID, query, limit); ok {
			return entries, nil
		}
	}
	rows, err := m.Store.RecallAgentMemory(ctx, scopeID, query, limit)
	if err != nil {
		return nil, err
	}
	return toEntries(rows), nil
}

// recallByVector embeds the query, pulls embedded candidates for the scope, and
// returns the top-`limit` by cosine. ok is false when it could not produce a
// vector ranking (caller then uses keyword recall).
func (m *PgMemory) recallByVector(ctx context.Context, scopeID, query string, limit int) ([]agentcore.MemoryEntry, bool) {
	vecs, err := m.Embedder.Embed(ctx, []string{query})
	if err != nil || len(vecs) == 0 || len(vecs[0]) == 0 {
		return nil, false
	}
	rows, err := m.Store.RecallAgentMemoryCandidates(ctx, scopeID, vectorCandidateCap)
	if err != nil || len(rows) == 0 {
		return nil, false
	}
	ranked := rankByVector(vecs[0], rows, limit)
	if len(ranked) == 0 {
		return nil, false
	}
	return toEntries(ranked), true
}

// rankByVector orders rows with embeddings by descending cosine similarity to
// query and returns the top `limit`. Pure (no IO) so it is unit-testable without
// Postgres or an LLM key.
func rankByVector(query []float32, rows []storage.AgentMemoryRow, limit int) []storage.AgentMemoryRow {
	type scored struct {
		row storage.AgentMemoryRow
		sim float64
	}
	cand := make([]scored, 0, len(rows))
	for _, r := range rows {
		if len(r.Embedding) == 0 {
			continue
		}
		cand = append(cand, scored{row: r, sim: agentcore.Cosine(query, r.Embedding)})
	}
	sort.SliceStable(cand, func(i, j int) bool { return cand[i].sim > cand[j].sim })
	if limit <= 0 || limit > len(cand) {
		limit = len(cand)
	}
	out := make([]storage.AgentMemoryRow, 0, limit)
	for i := 0; i < limit; i++ {
		out = append(out, cand[i].row)
	}
	return out
}

// toEntries maps storage rows into agentcore memory entries.
func toEntries(rows []storage.AgentMemoryRow) []agentcore.MemoryEntry {
	out := make([]agentcore.MemoryEntry, 0, len(rows))
	for _, r := range rows {
		out = append(out, agentcore.MemoryEntry{
			ID: r.ID, ScopeID: r.ScopeID, Kind: agentcore.MemoryKind(r.Kind),
			Content: r.Content, Tags: r.Tags, Confidence: r.Confidence,
			SourceRun: r.SourceRun, CreatedAt: r.CreatedAt,
		})
	}
	return out
}

// Remember persists a durable entry, redacting PII first when enabled (§7,
// §14.7: memory respects the trust boundary) and attaching a semantic embedding
// when an Embedder is configured.
func (m *PgMemory) Remember(ctx context.Context, e agentcore.MemoryEntry) error {
	content := e.Content
	if m.Redact {
		content = redactPII(content, m.Denylist)
	}
	row := storage.AgentMemoryRow{
		ScopeID: e.ScopeID, Kind: string(e.Kind), Content: content, Tags: e.Tags,
		Confidence: e.Confidence, SourceRun: e.SourceRun,
	}
	if m.Embedder != nil {
		if vecs, err := m.Embedder.Embed(ctx, []string{content}); err == nil && len(vecs) > 0 {
			row.Embedding = vecs[0]
		}
		// Embedding failure is non-fatal: the row persists without a vector and
		// is still recalled by keyword.
	}
	return m.Store.RememberAgentMemory(ctx, row)
}

// CreateSession starts a working-memory thread.
func (m *PgMemory) CreateSession(ctx context.Context, scopeID string) (agentcore.Session, error) {
	s, err := m.Store.CreateAgentSession(ctx, scopeID)
	if err != nil {
		return agentcore.Session{}, err
	}
	return toSession(s)
}

// SaveSession persists the message history of a thread.
func (m *PgMemory) SaveSession(ctx context.Context, s agentcore.Session) error {
	msgs, err := json.Marshal(s.Messages)
	if err != nil {
		return err
	}
	return m.Store.SaveAgentSession(ctx, storage.AgentSession{ID: s.ID, ScopeID: s.ScopeID, Messages: msgs})
}

// Fork branches a thread so the agent can explore without losing the trunk.
func (m *PgMemory) Fork(ctx context.Context, sessionID string) (agentcore.Session, error) {
	s, err := m.Store.ForkAgentSession(ctx, sessionID)
	if err != nil {
		return agentcore.Session{}, err
	}
	return toSession(s)
}

func toSession(s storage.AgentSession) (agentcore.Session, error) {
	out := agentcore.Session{
		ID: s.ID, ScopeID: s.ScopeID, ParentID: s.ParentID,
		CreatedAt: s.CreatedAt, UpdatedAt: s.UpdatedAt,
	}
	if len(s.Messages) > 0 {
		if err := json.Unmarshal(s.Messages, &out.Messages); err != nil {
			return agentcore.Session{}, err
		}
	}
	return out, nil
}
