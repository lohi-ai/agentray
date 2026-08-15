package storage

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
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
	ID    string `json:"id"`
	RunID string `json:"run_id"`
	// SessionKey identifies the log this entry belongs to. For a run's own log it
	// equals RunID; a sub-agent child session appends under a derived key
	// ("<runID>/<toolCallID>") while RunID stays the root run, so the FK and its
	// cascade-delete still hang off agent_runs. Empty defaults to RunID.
	SessionKey  string    `json:"session_key"`
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
		// session_key is the log identity; run_id is the ROOT run the log hangs
		// off (FK + cascade). They differ for sub-agent child sessions, whose key
		// is "<runID>/<toolCallID>" — a derived key, not a run row — so the seq
		// uniqueness lives on session_key, not run_id.
		`CREATE TABLE IF NOT EXISTS agent_session_log (
	id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
	run_id UUID NOT NULL REFERENCES agent_runs(id) ON DELETE CASCADE,
	session_key TEXT NOT NULL DEFAULT '',
	seq INT NOT NULL,
	kind VARCHAR(32) NOT NULL DEFAULT '',
	turn INT NOT NULL DEFAULT 0,
	payload_json JSONB NOT NULL DEFAULT '{}'::jsonb,
	created_at TIMESTAMPTZ NOT NULL DEFAULT now()
)`,
		// Upgrade an existing table in place: add the key, backfill it from
		// run_id (pre-existing logs are all run-keyed), then move the seq
		// uniqueness from (run_id, seq) to (session_key, seq) so a child session
		// sharing the parent's run_id can hold its own seq sequence.
		`ALTER TABLE agent_session_log ADD COLUMN IF NOT EXISTS session_key TEXT NOT NULL DEFAULT ''`,
		`UPDATE agent_session_log SET session_key = run_id::text WHERE session_key = ''`,
		`ALTER TABLE agent_session_log DROP CONSTRAINT IF EXISTS agent_session_log_run_id_seq_key`,
		`CREATE UNIQUE INDEX IF NOT EXISTS agent_session_log_session_seq_key ON agent_session_log (session_key, seq)`,
		`CREATE INDEX IF NOT EXISTS agent_session_log_run_idx ON agent_session_log (run_id, seq ASC)`,
		// Resume reads the newest checkpoint and whether the log ever branched.
		// Both questions are about two rare kinds among a long run's thousands of
		// message rows, so the index is PARTIAL: it holds only those kinds, stays
		// tiny next to the table, and answers either question with one backwards
		// index scan instead of walking a whole session's log — which is the entire
		// point of a windowed resume.
		`CREATE INDEX IF NOT EXISTS agent_session_log_resume_idx
	ON agent_session_log (session_key, seq DESC)
	WHERE kind IN ('compaction', 'leaf_move')`,
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
	key := e.SessionKey
	if key == "" {
		key = e.RunID
	}
	var seq int
	err := s.pg.QueryRow(ctx, `
INSERT INTO agent_session_log (run_id, session_key, seq, kind, turn, payload_json)
VALUES (
	$1, $2,
	(SELECT COALESCE(MAX(seq), 0) + 1 FROM agent_session_log WHERE session_key = $2),
	$3, $4, $5::jsonb
)
RETURNING seq`, e.RunID, key, e.Kind, e.Turn, payload).Scan(&seq)
	return seq, err
}

// AgentSessionLog returns one session's full ordered entry log (oldest first),
// keyed on session_key (a run id, or a derived child-session key). No RBAC: the
// caller (runner resume path) has already authorized the run. The fold that
// rebuilds run state lives in the consumer; storage just returns the rows.
func (s *Store) AgentSessionLog(ctx context.Context, sessionKey string) ([]AgentSessionEntry, error) {
	rows, err := s.pg.Query(ctx, `
SELECT id::text, run_id::text, session_key, seq, kind, turn, payload_json::text, created_at
FROM agent_session_log
WHERE session_key = $1
ORDER BY seq ASC`, sessionKey)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanSessionEntries(rows)
}

// AgentSessionLogFrom returns the tail of one session's log — entries with
// seq >= sinceSeq, oldest first. It is the read a resume actually needs: a long
// run's log is mostly history its newest checkpoint already stands for, and
// reading all of it makes crash recovery slower the longer the run got.
//
// The bound is inclusive because the caller starts AT the checkpoint: that row
// carries the retained transcript and the run state, so excluding it would drop
// everything the window exists to preserve. Served by the same (session_key, seq)
// unique index that orders the full read.
func (s *Store) AgentSessionLogFrom(ctx context.Context, sessionKey string, sinceSeq int) ([]AgentSessionEntry, error) {
	rows, err := s.pg.Query(ctx, `
SELECT id::text, run_id::text, session_key, seq, kind, turn, payload_json::text, created_at
FROM agent_session_log
WHERE session_key = $1 AND seq >= $2
ORDER BY seq ASC`, sessionKey, sinceSeq)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanSessionEntries(rows)
}

// AgentSessionCheckpoint reports the two facts that decide whether a session can
// be resumed from a window: the seq of the newest SELF-CONTAINED checkpoint (a
// completed compaction carrying both a retained transcript and the run state),
// and whether the log has ever branched.
//
// Both are mechanical — no judgement about whether a window is safe lives here;
// the caller (agentcore.LoadResumeLog) owns that rule, so a second backend
// cannot get it subtly different. A session with no such checkpoint returns 0,
// which is unambiguous: seq starts at 1, and a compaction always follows the
// messages it summarizes.
func (s *Store) AgentSessionCheckpoint(ctx context.Context, sessionKey string) (int, bool, error) {
	var seq int
	var branched bool
	err := s.pg.QueryRow(ctx, `
SELECT
	COALESCE(MAX(seq) FILTER (
		WHERE kind = 'compaction'
			AND payload_json->>'final' = 'true'
			AND jsonb_exists(payload_json, 'retained')
			AND jsonb_exists(payload_json, 'state')
	), 0),
	COALESCE(bool_or(kind = 'leaf_move'), false)
FROM agent_session_log
WHERE session_key = $1 AND kind IN ('compaction', 'leaf_move')`, sessionKey).Scan(&seq, &branched)
	return seq, branched, err
}

func scanSessionEntries(rows pgx.Rows) ([]AgentSessionEntry, error) {
	out := []AgentSessionEntry{}
	for rows.Next() {
		var e AgentSessionEntry
		if err := rows.Scan(&e.ID, &e.RunID, &e.SessionKey, &e.Seq, &e.Kind, &e.Turn, &e.PayloadJSON, &e.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
