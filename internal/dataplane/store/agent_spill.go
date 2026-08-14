package storage

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// This file holds durable spill storage — the full text of a tool result too
// large to sit inline in the model's context. The loop replaces such a result
// with a bounded preview plus a locator and WRITES THAT LOCATOR TO THE DURABLE
// SESSION LOG, which is what forces the storage to be durable too: an in-process
// store would mint a locator that survives in the log but not in memory, so a
// resumed run reads its own spilled output back as not-found.
//
// Rows hang off agent_runs exactly like agent_session_log, so a deleted run
// takes its spilled artifacts with it (the retention policy is the cascade —
// spill has no independent lifetime). Like the session log this layer is
// agent-agnostic: the agentcore <-> storage mapping lives in the consumer's
// adapter so storage keeps no agentcore import. Here an artifact is a locator, a
// session key, and opaque bytes.

// AgentSpillArtifact is one saved oversized tool result.
type AgentSpillArtifact struct {
	ID string `json:"id"`
	// RunID is the ROOT run the artifact hangs off (FK + cascade); SessionKey is
	// the fence. They differ for a sub-agent child session, whose key is
	// "<runID>/<toolCallID>" — the same split agent_session_log makes.
	RunID      string `json:"run_id"`
	SessionKey string `json:"session_key"`
	// Locator is the opaque model-facing handle, unique across sessions (the
	// consumer mints it from a digest that covers the session id).
	Locator string `json:"locator"`
	// ToolName / CallID identify the call that produced the text. Descriptive
	// only — never consulted for access control.
	ToolName string `json:"tool_name"`
	CallID   string `json:"call_id"`
	// Label is a short human tag for the artifact ("result").
	Label string `json:"label"`
	// Content is the full text, stored verbatim. Bytes is its octet length,
	// denormalized so a read never has to measure the TOASTed column.
	Content   string    `json:"content"`
	Bytes     int       `json:"bytes"`
	CreatedAt time.Time `json:"created_at"`
}

// AgentSpillWindow is a byte window of a stored artifact, plus the artifact's
// full size. Content is raw bytes: the slice is cut at arbitrary byte offsets,
// so it may begin or end mid-rune and the consumer snaps it.
type AgentSpillWindow struct {
	Content []byte
	Offset  int
	Total   int
}

// ErrAgentSpillNotFound is returned for an unknown locator. The caller maps it
// to its own not-found so a locator is never an existence oracle.
var ErrAgentSpillNotFound = errors.New("storage: spill artifact not found")

// migrateAgentSpill creates the durable spill table. Kept out of migrateAgent
// so the durability layer evolves independently of the AgentGarden entity
// migration, and called next to migrateAgentSessionLog because the two share a
// lifetime: a locator is only meaningful while the log that mentions it exists.
//
// New table, so CREATE TABLE IF NOT EXISTS is the whole migration — nothing here
// rewrites an existing one.
func (s *Store) migrateAgentSpill(ctx context.Context) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS agent_spill (
	id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
	run_id UUID NOT NULL REFERENCES agent_runs(id) ON DELETE CASCADE,
	session_key TEXT NOT NULL DEFAULT '',
	locator TEXT NOT NULL,
	tool_name VARCHAR(128) NOT NULL DEFAULT '',
	call_id VARCHAR(128) NOT NULL DEFAULT '',
	label VARCHAR(64) NOT NULL DEFAULT '',
	content TEXT NOT NULL DEFAULT '',
	bytes INT NOT NULL DEFAULT 0,
	created_at TIMESTAMPTZ NOT NULL DEFAULT now()
)`,
		// Every read is a point lookup on the locator, and the same locator must
		// never be minted twice — re-saving one is an idempotent overwrite, not a
		// second row.
		`CREATE UNIQUE INDEX IF NOT EXISTS agent_spill_locator_key ON agent_spill (locator)`,
		// The fence check reads (locator, session_key); the run index serves
		// cascade-adjacent listing and any future TTL sweep.
		`CREATE INDEX IF NOT EXISTS agent_spill_run_idx ON agent_spill (run_id, created_at DESC)`,
	}
	for _, stmt := range stmts {
		if _, err := s.pg.Exec(ctx, stmt); err != nil {
			return err
		}
	}
	return nil
}

// SaveAgentSpill persists one oversized tool result, returning its exact byte
// size. Saving the same locator twice overwrites rather than conflicting: the
// locator is a digest of the call that produced the text, so a replayed
// retry-safe call after a resume legitimately re-saves the same artifact.
//
// No RBAC: called by the runtime's spill adapter, not by a user.
func (s *Store) SaveAgentSpill(ctx context.Context, a AgentSpillArtifact) (int, error) {
	key := a.SessionKey
	if key == "" {
		key = a.RunID
	}
	var bytes int
	err := s.pg.QueryRow(ctx, `
INSERT INTO agent_spill (run_id, session_key, locator, tool_name, call_id, label, content, bytes)
VALUES ($1, $2, $3, $4, $5, $6, $7, octet_length($7))
ON CONFLICT (locator) DO UPDATE SET
	content = EXCLUDED.content,
	bytes = EXCLUDED.bytes
RETURNING bytes`, a.RunID, key, a.Locator, a.ToolName, a.CallID, a.Label, a.Content).Scan(&bytes)
	return bytes, err
}

// AgentSpillWindowAt returns a bounded BYTE window of a stored artifact without
// reading the whole column: substring() over the bytea conversion pushes the cut
// into Postgres, so paging a 40 MB export costs one page per call rather than
// 40 MB per call. offset/limit are clamped by Postgres itself (a window past the
// end returns empty), and the returned bytes are not rune-aligned — the caller
// snaps them.
//
// No RBAC: the caller's fence is OwnsAgentSpill below.
func (s *Store) AgentSpillWindowAt(ctx context.Context, locator string, offset, limit int) (AgentSpillWindow, error) {
	if offset < 0 {
		offset = 0
	}
	if limit < 0 {
		limit = 0
	}
	var w AgentSpillWindow
	// substring() is 1-indexed over the byte string.
	err := s.pg.QueryRow(ctx, `
SELECT substring(convert_to(content, 'UTF8') FROM $2 FOR $3), octet_length(content)
FROM agent_spill
WHERE locator = $1`, locator, offset+1, limit).Scan(&w.Content, &w.Total)
	if errors.Is(err, pgx.ErrNoRows) {
		return AgentSpillWindow{}, ErrAgentSpillNotFound
	}
	if err != nil {
		return AgentSpillWindow{}, err
	}
	w.Offset = offset
	if w.Offset > w.Total {
		w.Offset = w.Total
	}
	return w, nil
}

// OwnsAgentSpill reports whether a locator was minted for the given session. It
// is the read fence: the retrieval tool consults it before serving any bytes, so
// one agent cannot read another's spilled output even by guessing a locator.
// An unknown locator and one belonging to another session are indistinguishable
// here — both are false.
func (s *Store) OwnsAgentSpill(ctx context.Context, locator, sessionKey string) (bool, error) {
	var ok bool
	err := s.pg.QueryRow(ctx, `
SELECT true FROM agent_spill WHERE locator = $1 AND session_key = $2`, locator, sessionKey).Scan(&ok)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return ok, nil
}
