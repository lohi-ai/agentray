package storage

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// This file holds the conversation store (DESIGN-CONVERSATION-STORE.md): the
// durable, append-only, conversation-scoped entry log three machines share. It
// generalizes agent_session_log (which is per-run) to a conversation scope, adds
// an explicit movable leaf pointer, and a parent_id tree so concurrent writers
// become resolvable branches rather than lost writes.
//
// Two projections derive from one source: the human view (message-kind entries
// rendered as chat) and the model context (folded by agentruntime.BuildHistory).
// Storage stays agent-agnostic: an entry is an opaque kind + JSON payload +
// ordering, exactly like agent_session_log.

// AgentConversation is one durable thread. leaf_entry_id is the movable pointer
// to "where the conversation currently is"; the server is the only writer of it
// (advanced inside the append transaction), which is what makes multi-writer
// resolution tractable — see AppendConversationEntry.
type AgentConversation struct {
	ID          string    `json:"id"`
	ProjectID   string    `json:"project_id"`
	AgentID     string    `json:"agent_id"`
	Title       string    `json:"title"`
	LeafEntryID string    `json:"leaf_entry_id,omitempty"`
	CreatedBy   string    `json:"created_by"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// AgentConversationEntry is one immutable typed entry. PayloadJSON is the
// consumer-marshalled body; storage never interprets it. Seq is the per-
// conversation sync cursor (clients ask for entries > seq); ParentID carries the
// tree (for a linear thread it is just the previous entry's id).
type AgentConversationEntry struct {
	ID             string    `json:"id"`
	ConversationID string    `json:"conversation_id"`
	ParentID       string    `json:"parent_id,omitempty"`
	Seq            int64     `json:"seq"`
	Kind           string    `json:"kind"` // message | compaction | tool_trace | step | model_change | branch_summary | leaf
	Role           string    `json:"role"` // user | assistant | system (message kind)
	// AgentID stamps which agent authored/handled this entry. The acting agent is
	// chosen per message (a conversation can switch agents mid-thread; the switch
	// only affects new entries), so it lives on the entry, not the conversation.
	// Empty for entries written before this column existed and for the project's
	// default agent (which has no distinct id).
	AgentID        string    `json:"agent_id,omitempty"`
	AuthorUserID   string    `json:"author_user_id,omitempty"`
	RunID          string    `json:"run_id,omitempty"`
	Turn           int       `json:"turn"`
	PayloadJSON    string    `json:"payload_json"`
	TokenEstimate  int       `json:"token_estimate"`
	CreatedAt      time.Time `json:"created_at"`
}

// migrateAgentConversations creates the conversation store tables. Kept out of
// migrateAgent so the conversation layer evolves independently; idempotent CREATE
// TABLE IF NOT EXISTS per the repo convention. Called from Store.migrate after
// migrateAgentSessionLog (entries reference agent_runs).
func (s *Store) migrateAgentConversations(ctx context.Context) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS agent_conversations (
	id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
	project_id UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
	agent_id UUID NOT NULL,
	title TEXT NOT NULL DEFAULT '',
	leaf_entry_id UUID,
	created_by UUID NOT NULL,
	created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
)`,
		`CREATE INDEX IF NOT EXISTS agent_conversations_project_idx
ON agent_conversations (project_id, updated_at DESC)`,
		`CREATE TABLE IF NOT EXISTS agent_conversation_entries (
	id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
	conversation_id UUID NOT NULL REFERENCES agent_conversations(id) ON DELETE CASCADE,
	parent_id UUID REFERENCES agent_conversation_entries(id),
	seq BIGINT NOT NULL,
	kind VARCHAR(32) NOT NULL DEFAULT '',
	role VARCHAR(16) NOT NULL DEFAULT '',
	agent_id UUID,
	author_user_id UUID,
	run_id UUID REFERENCES agent_runs(id) ON DELETE SET NULL,
	turn INT NOT NULL DEFAULT 0,
	payload_json JSONB NOT NULL DEFAULT '{}'::jsonb,
	token_estimate INT NOT NULL DEFAULT 0,
	created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	UNIQUE (conversation_id, seq)
)`,
		`CREATE INDEX IF NOT EXISTS agent_conv_entries_conv_idx
ON agent_conversation_entries (conversation_id, seq ASC)`,
		// Per-entry acting agent (per-message agent override). Nullable: existing rows
		// and default-agent turns carry NULL.
		`ALTER TABLE agent_conversation_entries ADD COLUMN IF NOT EXISTS agent_id UUID`,
	}
	for _, stmt := range stmts {
		if _, err := s.pg.Exec(ctx, stmt); err != nil {
			return err
		}
	}
	return nil
}

// CreateConversation opens an empty thread for an agent and returns it. agentID
// empty defaults to the project's default agent (id == project_id), matching the
// run-creation convention. Member-readable: any project member may open a thread.
func (s *Store) CreateConversation(ctx context.Context, userID, projectID, agentID, title string) (AgentConversation, error) {
	project, err := s.ProjectByIDForUser(ctx, userID, projectID)
	if err != nil {
		return AgentConversation{}, err
	}
	if agentID == "" {
		agentID = project.ID
	}
	var conv AgentConversation
	err = s.pg.QueryRow(ctx, `
INSERT INTO agent_conversations (project_id, agent_id, title, created_by)
VALUES ($1, $2, $3, $4)
RETURNING id::text, project_id::text, agent_id::text, title, coalesce(leaf_entry_id::text,''), created_by::text, created_at, updated_at`,
		project.ID, agentID, title, userID).
		Scan(&conv.ID, &conv.ProjectID, &conv.AgentID, &conv.Title, &conv.LeafEntryID, &conv.CreatedBy, &conv.CreatedAt, &conv.UpdatedAt)
	return conv, err
}

// GetConversation loads a thread (member-readable) by id, scoped to the project so
// a member of one project can't read another's conversation.
func (s *Store) GetConversation(ctx context.Context, userID, projectID, convID string) (AgentConversation, error) {
	project, err := s.ProjectByIDForUser(ctx, userID, projectID)
	if err != nil {
		return AgentConversation{}, err
	}
	var conv AgentConversation
	err = s.pg.QueryRow(ctx, `
SELECT id::text, project_id::text, agent_id::text, title, coalesce(leaf_entry_id::text,''), created_by::text, created_at, updated_at
FROM agent_conversations WHERE id = $1 AND project_id = $2`, convID, project.ID).
		Scan(&conv.ID, &conv.ProjectID, &conv.AgentID, &conv.Title, &conv.LeafEntryID, &conv.CreatedBy, &conv.CreatedAt, &conv.UpdatedAt)
	return conv, err
}

// ListConversations returns recent threads for a project, newest activity first
// (member-readable).
func (s *Store) ListConversations(ctx context.Context, userID, projectID string, limit int) ([]AgentConversation, error) {
	project, err := s.ProjectByIDForUser(ctx, userID, projectID)
	if err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.pg.Query(ctx, `
SELECT id::text, project_id::text, agent_id::text, title, coalesce(leaf_entry_id::text,''), created_by::text, created_at, updated_at
FROM agent_conversations WHERE project_id = $1 ORDER BY updated_at DESC LIMIT $2`, project.ID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AgentConversation{}
	for rows.Next() {
		var conv AgentConversation
		if err := rows.Scan(&conv.ID, &conv.ProjectID, &conv.AgentID, &conv.Title, &conv.LeafEntryID, &conv.CreatedBy, &conv.CreatedAt, &conv.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, conv)
	}
	return out, rows.Err()
}

// SetConversationTitle renames a thread (member-readable; titling is benign).
func (s *Store) SetConversationTitle(ctx context.Context, userID, projectID, convID, title string) error {
	project, err := s.ProjectByIDForUser(ctx, userID, projectID)
	if err != nil {
		return err
	}
	_, err = s.pg.Exec(ctx, `
UPDATE agent_conversations SET title = $3, updated_at = now() WHERE id = $1 AND project_id = $2`,
		convID, project.ID, title)
	return err
}

// SetConversationAgent repoints a thread's current agent (the per-message agent
// switch). Only future entries are affected: past entries keep the agent_id they
// were stamped with, so the human view still shows who handled each turn. agentID
// empty resets to the project's default agent. Member-readable, like titling.
func (s *Store) SetConversationAgent(ctx context.Context, userID, projectID, convID, agentID string) error {
	project, err := s.ProjectByIDForUser(ctx, userID, projectID)
	if err != nil {
		return err
	}
	if agentID == "" {
		agentID = project.ID
	}
	_, err = s.pg.Exec(ctx, `
UPDATE agent_conversations SET agent_id = $3, updated_at = now() WHERE id = $1 AND project_id = $2`,
		convID, project.ID, agentID)
	return err
}

// AppendConversationEntry records one entry and advances the conversation leaf in
// a single transaction — the one hard concurrency rule from the design (§7.2):
// only the server advances leaf_entry_id, and it does so atomically with the
// insert. The next per-conversation seq is assigned the same way agent_session_log
// does (COALESCE(MAX(seq),0)+1), with UNIQUE(conversation_id, seq) as the backstop.
//
// When ParentID is empty the new entry is parented to the conversation's current
// leaf, so a strictly linear thread needs no parent bookkeeping from the caller.
// Two concurrent appends to the same parent both succeed; the later commit wins
// the leaf and the earlier one remains a reachable branch (last-writer advances
// leaf). No RBAC: callers (the chat handler, already authed; the runtime sink) own
// authorization. Returns the inserted entry with its assigned id and seq.
func (s *Store) AppendConversationEntry(ctx context.Context, e AgentConversationEntry) (AgentConversationEntry, error) {
	payload := e.PayloadJSON
	if payload == "" {
		payload = "{}"
	}
	tx, err := s.pg.Begin(ctx)
	if err != nil {
		return AgentConversationEntry{}, err
	}
	defer tx.Rollback(ctx)

	// Parent defaults to the current leaf (linear append). nullableUUID keeps empty
	// strings out of the UUID columns (parent_id, author_user_id, run_id).
	parent := e.ParentID
	if parent == "" {
		if err := tx.QueryRow(ctx, `
SELECT coalesce(leaf_entry_id::text,'') FROM agent_conversations WHERE id = $1`, e.ConversationID).Scan(&parent); err != nil {
			return AgentConversationEntry{}, err
		}
	}

	var out AgentConversationEntry
	err = tx.QueryRow(ctx, `
INSERT INTO agent_conversation_entries
	(conversation_id, parent_id, seq, kind, role, agent_id, author_user_id, run_id, turn, payload_json, token_estimate)
VALUES (
	$1, $2,
	(SELECT COALESCE(MAX(seq), 0) + 1 FROM agent_conversation_entries WHERE conversation_id = $1),
	$3, $4, $5, $6, $7, $8, $9::jsonb, $10
)
RETURNING id::text, conversation_id::text, coalesce(parent_id::text,''), seq, kind, role,
	coalesce(agent_id::text,''), coalesce(author_user_id::text,''), coalesce(run_id::text,''), turn, payload_json::text, token_estimate, created_at`,
		e.ConversationID, nullableUUID(parent), e.Kind, e.Role, nullableUUID(e.AgentID), nullableUUID(e.AuthorUserID),
		nullableUUID(e.RunID), e.Turn, payload, e.TokenEstimate).
		Scan(&out.ID, &out.ConversationID, &out.ParentID, &out.Seq, &out.Kind, &out.Role,
			&out.AgentID, &out.AuthorUserID, &out.RunID, &out.Turn, &out.PayloadJSON, &out.TokenEstimate, &out.CreatedAt)
	if err != nil {
		return AgentConversationEntry{}, err
	}

	if _, err := tx.Exec(ctx, `
UPDATE agent_conversations SET leaf_entry_id = $2, updated_at = now() WHERE id = $1`,
		e.ConversationID, out.ID); err != nil {
		return AgentConversationEntry{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return AgentConversationEntry{}, err
	}
	return out, nil
}

// PathToLeaf returns the entries on the path from root to the conversation's
// current leaf, in seq order. It walks parent_id from the leaf back to the root,
// so a branch that lost the leaf race is excluded — the model context follows the
// winning line. No RBAC: the caller (BuildHistory, already authed via the route)
// owns authorization.
func (s *Store) PathToLeaf(ctx context.Context, convID string) ([]AgentConversationEntry, error) {
	rows, err := s.pg.Query(ctx, `
WITH RECURSIVE path AS (
	SELECT e.* FROM agent_conversation_entries e
	JOIN agent_conversations c ON c.leaf_entry_id = e.id
	WHERE c.id = $1
	UNION ALL
	SELECT p.* FROM agent_conversation_entries p
	JOIN path ON path.parent_id = p.id
)
SELECT id::text, conversation_id::text, coalesce(parent_id::text,''), seq, kind, role,
	coalesce(agent_id::text,''), coalesce(author_user_id::text,''), coalesce(run_id::text,''), turn, payload_json::text, token_estimate, created_at
FROM path ORDER BY seq ASC`, convID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanConversationEntries(rows)
}

// GetConversationEntry returns a single entry by id within a conversation. The
// fork/regenerate navigation uses it to resolve an entry's parent (the branch
// point) before repointing the leaf. No RBAC: the caller authorizes the
// conversation first.
func (s *Store) GetConversationEntry(ctx context.Context, convID, entryID string) (AgentConversationEntry, error) {
	var e AgentConversationEntry
	err := s.pg.QueryRow(ctx, `
SELECT id::text, conversation_id::text, coalesce(parent_id::text,''), seq, kind, role,
	coalesce(agent_id::text,''), coalesce(author_user_id::text,''), coalesce(run_id::text,''), turn, payload_json::text, token_estimate, created_at
FROM agent_conversation_entries WHERE conversation_id = $1 AND id = $2`, convID, entryID).
		Scan(&e.ID, &e.ConversationID, &e.ParentID, &e.Seq, &e.Kind, &e.Role,
			&e.AgentID, &e.AuthorUserID, &e.RunID, &e.Turn, &e.PayloadJSON, &e.TokenEstimate, &e.CreatedAt)
	if err != nil {
		return AgentConversationEntry{}, err
	}
	return e, nil
}

// SetConversationLeaf repoints a conversation's movable leaf to an existing entry
// (or to the root, when entryID is empty). This is the one primitive the
// fork/regenerate navigation needs on top of the existing tree: move the leaf to
// a branch point's parent, then the next AppendConversationEntry parents off it
// and forks a new line — while the abandoned line stays reachable (every branch is
// still returned by ConversationEntries; only PathToLeaf follows the winning
// line). The target must belong to the conversation. No RBAC: caller authorizes
// the conversation first.
func (s *Store) SetConversationLeaf(ctx context.Context, convID, entryID string) error {
	if entryID != "" {
		var ok bool
		if err := s.pg.QueryRow(ctx, `
SELECT EXISTS(SELECT 1 FROM agent_conversation_entries WHERE conversation_id = $1 AND id = $2)`,
			convID, entryID).Scan(&ok); err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("entry %s is not in conversation %s", entryID, convID)
		}
	}
	_, err := s.pg.Exec(ctx, `
UPDATE agent_conversations SET leaf_entry_id = $2, updated_at = now() WHERE id = $1`,
		convID, nullableUUID(entryID))
	return err
}

// ConversationEntries returns entries with seq > since in seq order — the sync /
// resume read path (clients poll or reconnect with their last seq). since <= 0
// returns the whole log. This returns ALL entries (every branch), not just the
// leaf path, so the human view can render concurrent replies; the model context
// uses PathToLeaf instead. Member-readable.
func (s *Store) ConversationEntries(ctx context.Context, userID, projectID, convID string, since int64) ([]AgentConversationEntry, error) {
	if _, err := s.GetConversation(ctx, userID, projectID, convID); err != nil {
		return nil, err
	}
	rows, err := s.pg.Query(ctx, `
SELECT id::text, conversation_id::text, coalesce(parent_id::text,''), seq, kind, role,
	coalesce(agent_id::text,''), coalesce(author_user_id::text,''), coalesce(run_id::text,''), turn, payload_json::text, token_estimate, created_at
FROM agent_conversation_entries
WHERE conversation_id = $1 AND seq > $2
ORDER BY seq ASC`, convID, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanConversationEntries(rows)
}

func scanConversationEntries(rows pgx.Rows) ([]AgentConversationEntry, error) {
	out := []AgentConversationEntry{}
	for rows.Next() {
		var e AgentConversationEntry
		if err := rows.Scan(&e.ID, &e.ConversationID, &e.ParentID, &e.Seq, &e.Kind, &e.Role,
			&e.AgentID, &e.AuthorUserID, &e.RunID, &e.Turn, &e.PayloadJSON, &e.TokenEstimate, &e.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// nullableUUID maps an empty string to a SQL NULL so optional UUID columns
// (parent_id, author_user_id, run_id) aren't fed "" (an invalid UUID).
func nullableUUID(v string) any {
	if v == "" {
		return nil
	}
	return v
}
