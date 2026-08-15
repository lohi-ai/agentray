package agentcore

import (
	"context"
	"sync"
)

// MemorySessionStore is an in-process, append-only SessionStore.
//
// It makes a run resumable within one process — a compacted transcript can
// still be reduced back, and the log invariant has something to check against —
// but it does NOT survive a restart. Use it for tests, for local development,
// and for single-process runs where a crash means the work is gone anyway.
// Anything that must outlive the process needs a durable store.
//
// Safe for concurrent use: a run appends from the loop while a job or a
// session_query reads.
type MemorySessionStore struct {
	mu  sync.Mutex
	log map[string][]SessionEntry
}

// NewMemorySessionStore returns an empty in-process session store.
func NewMemorySessionStore() *MemorySessionStore {
	return &MemorySessionStore{log: map[string][]SessionEntry{}}
}

// Append records one entry, assigning its sequence number.
func (m *MemorySessionStore) Append(_ context.Context, id string, e SessionEntry) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.log == nil {
		m.log = map[string][]SessionEntry{}
	}
	e.Seq = len(m.log[id])
	m.log[id] = append(m.log[id], e)
	return nil
}

// Log returns a copy of the ordered entry log for a session, so a caller
// iterating it cannot be raced by a concurrent append.
func (m *MemorySessionStore) Log(_ context.Context, id string) ([]SessionEntry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]SessionEntry, len(m.log[id]))
	copy(out, m.log[id])
	return out, nil
}

// LogFrom returns the session's entries with Seq >= sinceSeq, in order. The
// in-memory store gains nothing from a windowed read (the slice is already in
// hand), but implementing the capability is what lets the resume path be
// exercised end to end without a database — the alternative is a windowing rule
// that only ever runs in production.
func (m *MemorySessionStore) LogFrom(_ context.Context, id string, sinceSeq int) ([]SessionEntry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []SessionEntry
	for _, e := range m.log[id] {
		if e.Seq >= sinceSeq {
			out = append(out, e)
		}
	}
	return out, nil
}

// CheckpointSeq reports the newest self-contained checkpoint and whether the log
// has ever branched. Both are scans here; a real store answers them with an
// index.
func (m *MemorySessionStore) CheckpointSeq(_ context.Context, id string) (int, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	seq, branched := 0, false
	for _, e := range m.log[id] {
		if e.Kind == EntryLeafMove {
			branched = true
		}
		if e.Kind == EntryCompaction && e.Final && e.Retained != nil && e.State != nil {
			seq = e.Seq
		}
	}
	return seq, branched, nil
}

// Sessions returns the ids that have at least one entry, in no particular
// order. Useful for a local resume picker.
func (m *MemorySessionStore) Sessions() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, 0, len(m.log))
	for id := range m.log {
		out = append(out, id)
	}
	return out
}
