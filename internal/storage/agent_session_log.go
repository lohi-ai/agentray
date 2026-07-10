package storage

import (
	"context"
	"time"
)

// This file holds the durable session log — the append-only harness that makes a
// run resumable (agentcore's SessionStore seam). One row per loop event (a
// message reaching final form, a leaf, a compaction bracket, a model change),
// written in turn order and never mutated. It is agent-agnostic: rows hang off
// agent_runs, so every agentcore-based run is durable with no per-agent code.
//
// Like agent_llm_calls, the agentcore <-> storage mapping (the entry's typed
// fields) is done by the consumer's session-store adapter so storage keeps no
// agentcore import: here an entry is an opaque kind + JSON payload + ordering.

// AgentSessionEntry is one immutable record in a run's append-only log. Payload
// is the consumer-marshalled entry body; storage never interprets it.
type AgentSessionEntry struct {
	ID          string    `json:"id"`
	RunID       string    `json:"run_id"`
	Seq         int       `json:"seq"`
	Kind        string    `json:"kind"`
	Turn        int       `json:"turn"`
	PayloadJSON string    `json:"payload_json"`
	CreatedAt   time.Time `json:"created_at"`
}

// migrateAgentSessionLog creates the append-only durable-session table. Kept out
// of migrateAgent so the durability layer evolves independently of the
// AgentGarden entity migration. Idempotent CREATE TABLE IF NOT EXISTS per the
// repo convention; called from Store.migrate after migrateAgentTrace.
func (s *Store) migrateAgentSessionLog(ctx context.Context) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS agent_session_log (
	id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
	run_id UUID NOT NULL REFERENCES agent_runs(id) ON DELETE CASCADE,
	seq INT NOT NULL,
	kind VARCHAR(32) NOT NULL DEFAULT '',
	turn INT NOT NULL DEFAULT 0,
	payload_json JSONB NOT NULL DEFAULT '{}'::jsonb,
	created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	UNIQUE (run_id, seq)
)`,
		`CREATE INDEX IF NOT EXISTS agent_session_log_run_idx ON agent_session_log (run_id, seq ASC)`,
	}
	for _, stmt := range stmts {
		if _, err := s.pg.Exec(ctx, stmt); err != nil {
			return err
		}
	}
	return nil
}

// AppendAgentSessionEntry records one entry, assigning the next per-run sequence
// number atomically (COALESCE(MAX(seq))+1 in the INSERT). A run's loop appends
// serially from one goroutine, so the read-max-then-insert is contention-free in
// practice; the UNIQUE(run_id, seq) constraint is the backstop. No RBAC: called
// by the runtime session adapter, not a user. Returns the assigned seq.
func (s *Store) AppendAgentSessionEntry(ctx context.Context, e AgentSessionEntry) (int, error) {
	payload := e.PayloadJSON
	if payload == "" {
		payload = "{}"
	}
	var seq int
	err := s.pg.QueryRow(ctx, `
INSERT INTO agent_session_log (run_id, seq, kind, turn, payload_json)
VALUES (
	$1,
	(SELECT COALESCE(MAX(seq), 0) + 1 FROM agent_session_log WHERE run_id = $1),
	$2, $3, $4::jsonb
)
RETURNING seq`, e.RunID, e.Kind, e.Turn, payload).Scan(&seq)
	return seq, err
}

// AgentSessionLog returns a run's full ordered entry log (oldest first). No RBAC:
// the caller (runner resume path) has already authorized the run. The fold that
// rebuilds run state lives in the consumer; storage just returns the rows.
func (s *Store) AgentSessionLog(ctx context.Context, runID string) ([]AgentSessionEntry, error) {
	rows, err := s.pg.Query(ctx, `
SELECT id::text, run_id::text, seq, kind, turn, payload_json::text, created_at
FROM agent_session_log
WHERE run_id = $1
ORDER BY seq ASC`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AgentSessionEntry{}
	for rows.Next() {
		var e AgentSessionEntry
		if err := rows.Scan(&e.ID, &e.RunID, &e.Seq, &e.Kind, &e.Turn, &e.PayloadJSON, &e.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
