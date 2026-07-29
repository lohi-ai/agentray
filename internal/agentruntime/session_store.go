package agentruntime

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/lohi-ai/agentray/agentcore"
	"github.com/lohi-ai/agentray/internal/storage"
)

// pgSessionStore is the Postgres-backed agentcore.SessionStore: it persists a
// run's append-only log to agent_session_log so a crashed or compacted run can be
// reduced and resumed (agentcore's durability seam). It lives here, in the
// consumer, because it is the one place that may import both agentcore and
// storage (storage never imports agentcore) — mirroring storeTraceSink.
//
// The agentcore SessionEntry is marshalled whole into the row's JSON payload, so
// every typed field (the message, model, summary, compaction markers) round-trips
// without storage needing to understand any of them; kind/turn are also lifted
// into columns for cheap ordering and filtering. The sessionID the loop passes is
// the run id (the same id the trace sink keys on), so the durable log and the
// per-LLM-call trace attribute to the same run — or, for a spawned sub-agent, the
// derived "<runID>/<toolCallID>" child key, which still attributes (and cascades)
// to the root run via its UUID prefix.
type pgSessionStore struct {
	store *storage.Store
}

// NewSessionStore returns a SessionStore that writes durable run logs to Postgres.
func NewSessionStore(store *storage.Store) agentcore.SessionStore {
	return &pgSessionStore{store: store}
}

// rootRunID extracts the agent_runs UUID a session key hangs off. A run's own
// log is keyed by its run id verbatim; a sub-agent child session is keyed
// "<runID>/<toolCallID>" (agentcore's deterministic child-session ids), so the
// prefix before the first "/" is the root run for the FK/cascade.
func rootRunID(sessionID string) string {
	if i := strings.IndexByte(sessionID, '/'); i >= 0 {
		return sessionID[:i]
	}
	return sessionID
}

// Append persists one entry. The store assigns the per-session sequence number;
// the returned seq is discarded here because the loop never reads it back mid-run
// (resume reads the whole ordered log). Best-effort is the loop's contract — a
// durability write must never break a run — but we surface the error so a failing
// store is visible to the (best-effort) caller.
func (s *pgSessionStore) Append(ctx context.Context, sessionID string, entry agentcore.SessionEntry) error {
	payload, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	_, err = s.store.AppendAgentSessionEntry(ctx, storage.AgentSessionEntry{
		RunID:       rootRunID(sessionID),
		SessionKey:  sessionID,
		Kind:        string(entry.Kind),
		Turn:        entry.Turn,
		PayloadJSON: string(payload),
	})
	return err
}

// Log returns the full ordered entry log for a run, mapping each stored row back
// to the agentcore SessionEntry by unmarshalling its payload and stamping the
// store-assigned Seq. A malformed payload degrades to an empty entry carrying
// only kind/turn/seq rather than failing the whole reduce, mirroring how the Lab
// trace fold tolerates a bad row.
func (s *pgSessionStore) Log(ctx context.Context, sessionID string) ([]agentcore.SessionEntry, error) {
	rows, err := s.store.AgentSessionLog(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	out := make([]agentcore.SessionEntry, 0, len(rows))
	for _, r := range rows {
		out = append(out, sessionEntryFromRow(r))
	}
	return out, nil
}

// sessionEntryFromRow reconstructs an agentcore SessionEntry from a stored row.
// Pure (no DB) so it is unit-testable: the payload carries the full entry; Seq is
// authoritative from the row. A bad payload yields an entry with just the row's
// kind/turn/seq.
func sessionEntryFromRow(r storage.AgentSessionEntry) agentcore.SessionEntry {
	var e agentcore.SessionEntry
	if err := json.Unmarshal([]byte(r.PayloadJSON), &e); err != nil {
		e = agentcore.SessionEntry{Kind: agentcore.SessionEntryKind(r.Kind), Turn: r.Turn}
	}
	e.Seq = r.Seq
	return e
}
