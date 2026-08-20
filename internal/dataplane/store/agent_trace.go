package storage

import (
	"context"
	"time"
)

// This file holds the per-LLM-call trace store — the deepest tier of agent
// observability (one row per Chat/stream turn: the messages sent, the response,
// tokens, est. cost, latency). It is agent-agnostic: rows hang off agent_runs,
// which carry agent_id, so every agentcore-based agent is observable with no
// per-agent code. The agentcore <-> storage mapping (Message/ToolCall JSON) is
// done by the consumer's trace sink so storage stays free of any agentcore
// import, mirroring agent_runtime.go.

// AgentLLMCall is one persisted LLM call within a run. Messages/ToolCalls are
// opaque JSON the consumer owns; storage never interprets them.
//
// MessagesJSON is stored as a DELTA against the previous call of the same
// session, not as the whole request. A long run's requests are ~99% redundant —
// each one is the previous one plus the turn's new messages — so storing every
// request whole made the trace the largest thing in the database by a wide
// margin: a 4,200-turn run wrote 51 MiB of request messages against an 8 MiB
// session log for the same run. KeepPrefix/BaseSeq encode the delta (see
// RecordAgentLLMCall); storage never interprets it, and the consumer's
// reconstruction turns a page of rows back into per-call context.
type AgentLLMCall struct {
	ID    string `json:"id"`
	RunID string `json:"run_id"`
	// SessionKey identifies which agent produced the call: the run's own key for
	// the top-level agent, "<runID>/<toolCallID>" for a spawned sub-agent. Before
	// this existed every child's calls landed in the parent's flat list,
	// indistinguishable from the parent's own turns — a run that delegated 300
	// tasks read as one agent that had inexplicably done everything itself.
	SessionKey string `json:"session_key"`
	// Depth is the delegation depth of the producing agent (0 = top level), so a
	// client can render the call tree without parsing session keys.
	Depth int `json:"depth"`
	// Seq orders the calls within a session and is the cursor for pagination.
	Seq      int    `json:"seq"`
	Provider string `json:"provider"`
	Model    string `json:"model"`
	// MessagesJSON holds only the messages after KeepPrefix — the delta. A row
	// with BaseSeq 0 is a KEYFRAME and carries the whole request.
	MessagesJSON string `json:"messages_json"`
	// BaseSeq names the call this one's context extends (0 = keyframe), and
	// KeepPrefix how many of that call's messages are retained in front of the
	// delta. Reconstruction is context(n) = context(BaseSeq)[:KeepPrefix] + delta.
	BaseSeq       int      `json:"base_seq"`
	KeepPrefix    int      `json:"keep_prefix"`
	Tools         []string `json:"tools"`           // tool names advertised this turn
	Response      string   `json:"response"`        // assistant text returned
	ToolCallsJSON string   `json:"tool_calls_json"` // tool calls the model requested
	// ToolGatesJSON is the gate outcome of each tool call this turn requested
	// (opaque JSON: [{call_id, allowed, reason, error}]). Empty ('[]') on rows
	// written before the column existed and on the INSERT — Monitor records the
	// LLM call before the dispatcher has a verdict, so persistTrace fills this
	// with a follow-up UPDATE. Constant default, not a rewrite.
	ToolGatesJSON string  `json:"tool_gates_json"`
	StopReason    string  `json:"stop_reason"`
	TokenInput    int     `json:"token_input"`
	TokenOutput   int     `json:"token_output"`
	CostUSD       float64 `json:"cost_usd"`
	// CostUnpriced is true when CostUSD does not reflect a real total — the
	// model this call billed against had no entry in the price table, so the
	// cost could not be computed and CostUSD stays 0 as a placeholder, not a
	// fact. A reader must render "unpriced", never "$0.00", when this is true.
	CostUnpriced bool      `json:"cost_unpriced"`
	LatencyMS    int       `json:"latency_ms"`
	Streamed     bool      `json:"streamed"`
	Error        string    `json:"error,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
}

// migrateAgentTrace creates the per-LLM-call trace table. Kept out of
// migrateAgent (agent.go) so the observability layer evolves independently of
// the AgentGarden entity migration. Idempotent CREATE TABLE IF NOT EXISTS per
// the repo convention; called from Store.migrate after migrateAgent.
func (s *Store) migrateAgentTrace(ctx context.Context) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS agent_llm_calls (
	id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
	run_id UUID NOT NULL REFERENCES agent_runs(id) ON DELETE CASCADE,
	provider VARCHAR(64) NOT NULL DEFAULT '',
	model VARCHAR(128) NOT NULL DEFAULT '',
	messages_json JSONB NOT NULL DEFAULT '[]'::jsonb,
	tools TEXT[] NOT NULL DEFAULT '{}',
	response TEXT NOT NULL DEFAULT '',
	tool_calls_json JSONB NOT NULL DEFAULT '[]'::jsonb,
	stop_reason VARCHAR(32) NOT NULL DEFAULT '',
	token_input INT NOT NULL DEFAULT 0,
	token_output INT NOT NULL DEFAULT 0,
	cost_usd DOUBLE PRECISION NOT NULL DEFAULT 0,
	cost_unpriced BOOLEAN NOT NULL DEFAULT false,
	latency_ms INT NOT NULL DEFAULT 0,
	streamed BOOLEAN NOT NULL DEFAULT false,
	error TEXT NOT NULL DEFAULT '',
	created_at TIMESTAMPTZ NOT NULL DEFAULT now()
)`,
		`CREATE INDEX IF NOT EXISTS agent_llm_calls_run_idx ON agent_llm_calls (run_id, created_at ASC)`,
		// Upgrade in place. session_key backfills from run_id (every pre-existing
		// row was written by a top-level agent under its run key), and the delta
		// columns default to the keyframe encoding — base_seq 0, keep_prefix 0 —
		// which reads back as "messages_json is the whole request". That is
		// exactly what an old row holds, so historical traces reconstruct
		// correctly with no backfill.
		`ALTER TABLE agent_llm_calls ADD COLUMN IF NOT EXISTS session_key TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE agent_llm_calls ADD COLUMN IF NOT EXISTS depth INT NOT NULL DEFAULT 0`,
		`ALTER TABLE agent_llm_calls ADD COLUMN IF NOT EXISTS seq INT NOT NULL DEFAULT 0`,
		`ALTER TABLE agent_llm_calls ADD COLUMN IF NOT EXISTS base_seq INT NOT NULL DEFAULT 0`,
		`ALTER TABLE agent_llm_calls ADD COLUMN IF NOT EXISTS keep_prefix INT NOT NULL DEFAULT 0`,
		// Constant default (not a rewrite): existing rows read as "priced", which
		// is the honest answer for cost_usd 0 rows written before this column
		// existed — mixing "genuinely free" and "unpriced" for pre-existing rows
		// is an acceptable historical gap; every row written from here on is
		// stamped with the real answer at record time.
		`ALTER TABLE agent_llm_calls ADD COLUMN IF NOT EXISTS cost_unpriced BOOLEAN NOT NULL DEFAULT false`,
		// Gate outcomes cannot ride the INSERT: the LLM-call row is written at
		// provider-return, before tools run. persistTrace UPDATEs this column
		// after the dispatcher has a verdict. Constant default so historical
		// rows read as "no gates recorded" (FoldSteps keeps Allowed true)
		// without rewriting the table.
		`ALTER TABLE agent_llm_calls ADD COLUMN IF NOT EXISTS tool_gates_json JSONB NOT NULL DEFAULT '[]'::jsonb`,
		// Deliberately NOT backfilled: an UPDATE over every historical row would
		// rewrite the largest table in the database while deploy holds the
		// migration open. Legacy rows keep session_key '' and are read as the
		// run's own session, which is what they are.
		`CREATE INDEX IF NOT EXISTS agent_llm_calls_run_seq_idx ON agent_llm_calls (run_id, seq ASC, created_at ASC)`,
	}
	for _, stmt := range stmts {
		if _, err := s.pg.Exec(ctx, stmt); err != nil {
			return err
		}
	}
	return nil
}

// RecordAgentLLMCall persists one LLM-call trace, assigning the next per-run
// sequence number atomically (COALESCE(MAX(seq))+1 in the INSERT), the same way
// the session log does. No RBAC: called by the runtime trace sink, not a user.
// Best-effort at the call site — tracing must never break a run — so the caller
// drops the error. Returns the assigned seq, which the sink chains the next
// call's delta onto.
//
// The seq is per RUN rather than per session so one cursor pages a whole run,
// children included. Delta chains still never cross sessions: the sink only
// diffs a call against the previous call of its own session, and a session's
// first call is always a keyframe.
func (s *Store) RecordAgentLLMCall(ctx context.Context, c AgentLLMCall) (int, error) {
	msgs := c.MessagesJSON
	if msgs == "" {
		msgs = "[]"
	}
	calls := c.ToolCallsJSON
	if calls == "" {
		calls = "[]"
	}
	tools := c.Tools
	if tools == nil {
		tools = []string{}
	}
	key := c.SessionKey
	if key == "" {
		key = c.RunID
	}
	var seq int
	err := s.pg.QueryRow(ctx, `
INSERT INTO agent_llm_calls (
	run_id, session_key, depth, seq, base_seq, keep_prefix,
	provider, model, messages_json, tools, response, tool_calls_json,
	stop_reason, token_input, token_output, cost_usd, cost_unpriced, latency_ms, streamed, error
) VALUES (
	$1, $2, $3,
	(SELECT COALESCE(MAX(seq), 0) + 1 FROM agent_llm_calls WHERE run_id = $1),
	$4, $5, $6, $7, $8::jsonb, $9, $10, $11::jsonb, $12, $13, $14, $15, $16, $17, $18, $19
)
RETURNING seq`,
		c.RunID, key, c.Depth, c.BaseSeq, c.KeepPrefix,
		c.Provider, c.Model, msgs, tools, c.Response, calls,
		c.StopReason, c.TokenInput, c.TokenOutput, c.CostUSD, c.CostUnpriced, c.LatencyMS, c.Streamed, c.Error).Scan(&seq)
	return seq, err
}

// AttachAgentLLMCallGates writes each tool's gate outcome onto the LLM-call
// row that requested it. Matching is by ToolCall.ID inside tool_calls_json
// against the gate's call_id; unmatched gates are dropped rather than attached
// to the wrong turn. One UPDATE for the run — not a per-call rewrite.
func (s *Store) AttachAgentLLMCallGates(ctx context.Context, runID, gatesJSON string) error {
	if gatesJSON == "" {
		gatesJSON = "[]"
	}
	_, err := s.pg.Exec(ctx, `
UPDATE agent_llm_calls c
SET tool_gates_json = COALESCE((
	SELECT jsonb_agg(g)
	FROM jsonb_array_elements($2::jsonb) g
	WHERE EXISTS (
		SELECT 1 FROM jsonb_array_elements(c.tool_calls_json) tc
		WHERE tc->>'id' = g->>'call_id'
	)
), '[]'::jsonb)
WHERE c.run_id = $1`, runID, gatesJSON)
	return err
}

// llmCallColumns is the projection shared by the two read paths. session_key
// falls back to the run id for rows written before the column existed.
const llmCallColumns = `c.id::text, c.run_id::text,
       COALESCE(NULLIF(c.session_key, ''), c.run_id::text), c.depth, c.seq,
       c.provider, c.model, c.base_seq, c.keep_prefix, c.tools,
       c.response, c.tool_calls_json::text, c.tool_gates_json::text, c.stop_reason, c.token_input, c.token_output,
       c.cost_usd, c.cost_unpriced, c.latency_ms, c.streamed, c.error, c.created_at`

// scanLLMCall reads one row of llmCallColumns, optionally with the messages
// delta appended to the projection.
func scanLLMCall(rows interface{ Scan(...any) error }, withMessages bool) (AgentLLMCall, error) {
	var c AgentLLMCall
	dest := []any{&c.ID, &c.RunID, &c.SessionKey, &c.Depth, &c.Seq,
		&c.Provider, &c.Model, &c.BaseSeq, &c.KeepPrefix, &c.Tools,
		&c.Response, &c.ToolCallsJSON, &c.ToolGatesJSON, &c.StopReason, &c.TokenInput, &c.TokenOutput,
		&c.CostUSD, &c.CostUnpriced, &c.LatencyMS, &c.Streamed, &c.Error, &c.CreatedAt}
	if withMessages {
		dest = append(dest, &c.MessagesJSON)
	}
	err := rows.Scan(dest...)
	return c, err
}

// ListAgentLLMCallMetrics returns one page of a run's LLM calls WITHOUT the
// request messages — model, tokens, cost, latency, and the session/depth that
// says which agent made the call.
//
// The messages are omitted rather than optional because this is the list view,
// and a list that carried them is what made the run detail endpoint unusable: a
// 5,154-call run answered a single click with tens of megabytes. A client that
// wants the conversation asks the Lab fold for it, one step at a time.
//
// Paging is by seq cursor: pass afterSeq 0 for the first page and the last row's
// Seq for the next.
func (s *Store) ListAgentLLMCallMetrics(ctx context.Context, userID, projectID, runID string, afterSeq, limit int) ([]AgentLLMCall, error) {
	project, err := s.ProjectByIDForUser(ctx, userID, projectID)
	if err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	rows, err := s.pg.Query(ctx, `
SELECT `+llmCallColumns+`
FROM agent_llm_calls c
JOIN agent_runs r ON r.id = c.run_id
WHERE c.run_id = $1 AND r.project_id = $2 AND c.seq > $3
ORDER BY c.seq ASC, c.created_at ASC
LIMIT $4`, runID, project.ID, afterSeq, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AgentLLMCall{}
	for rows.Next() {
		c, err := scanLLMCall(rows, false)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// AgentLLMCallTrace returns a run's full ordered trace INCLUDING the messages
// delta, for the Lab fold to reconstruct per-step context from.
//
// It is deliberately unpaged: the fold is a fold — a step's skills-loaded set
// and cumulative cost depend on every step before it — so it needs the run in
// order. What changed is the cost of doing that: the deltas make this read
// roughly one fiftieth of what the same query moved when every row carried the
// whole request. The fold's OUTPUT is paged by the caller.
func (s *Store) AgentLLMCallTrace(ctx context.Context, userID, projectID, runID string) ([]AgentLLMCall, error) {
	project, err := s.ProjectByIDForUser(ctx, userID, projectID)
	if err != nil {
		return nil, err
	}
	rows, err := s.pg.Query(ctx, `
SELECT `+llmCallColumns+`, c.messages_json::text
FROM agent_llm_calls c
JOIN agent_runs r ON r.id = c.run_id
WHERE c.run_id = $1 AND r.project_id = $2
ORDER BY c.seq ASC, c.created_at ASC`, runID, project.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AgentLLMCall{}
	for rows.Next() {
		c, err := scanLLMCall(rows, true)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
