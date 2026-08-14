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
type AgentLLMCall struct {
	ID            string    `json:"id"`
	RunID         string    `json:"run_id"`
	Provider      string    `json:"provider"`
	Model         string    `json:"model"`
	MessagesJSON  string    `json:"messages_json"`   // request messages sent to the model
	Tools         []string  `json:"tools"`           // tool names advertised this turn
	Response      string    `json:"response"`        // assistant text returned
	ToolCallsJSON string    `json:"tool_calls_json"` // tool calls the model requested
	StopReason    string    `json:"stop_reason"`
	TokenInput    int       `json:"token_input"`
	TokenOutput   int       `json:"token_output"`
	CostUSD       float64   `json:"cost_usd"`
	LatencyMS     int       `json:"latency_ms"`
	Streamed      bool      `json:"streamed"`
	Error         string    `json:"error,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
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
	latency_ms INT NOT NULL DEFAULT 0,
	streamed BOOLEAN NOT NULL DEFAULT false,
	error TEXT NOT NULL DEFAULT '',
	created_at TIMESTAMPTZ NOT NULL DEFAULT now()
)`,
		`CREATE INDEX IF NOT EXISTS agent_llm_calls_run_idx ON agent_llm_calls (run_id, created_at ASC)`,
	}
	for _, stmt := range stmts {
		if _, err := s.pg.Exec(ctx, stmt); err != nil {
			return err
		}
	}
	return nil
}

// RecordAgentLLMCall persists one LLM-call trace. No RBAC: called by the runtime
// trace sink, not a user. Best-effort at the call site — tracing must never break
// a run — so the caller drops the error.
func (s *Store) RecordAgentLLMCall(ctx context.Context, c AgentLLMCall) error {
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
	_, err := s.pg.Exec(ctx, `
INSERT INTO agent_llm_calls (
	run_id, provider, model, messages_json, tools, response, tool_calls_json,
	stop_reason, token_input, token_output, cost_usd, latency_ms, streamed, error
) VALUES ($1, $2, $3, $4::jsonb, $5, $6, $7::jsonb, $8, $9, $10, $11, $12, $13, $14)`,
		c.RunID, c.Provider, c.Model, msgs, tools, c.Response, calls,
		c.StopReason, c.TokenInput, c.TokenOutput, c.CostUSD, c.LatencyMS, c.Streamed, c.Error)
	return err
}

// ListAgentLLMCalls returns the LLM-call trace for a run in chronological order
// (member-readable). The run must belong to a project the user can access; the
// join through agent_runs enforces that without a separate ownership column.
func (s *Store) ListAgentLLMCalls(ctx context.Context, userID, projectID, runID string) ([]AgentLLMCall, error) {
	project, err := s.ProjectByIDForUser(ctx, userID, projectID)
	if err != nil {
		return nil, err
	}
	rows, err := s.pg.Query(ctx, `
SELECT c.id::text, c.run_id::text, c.provider, c.model, c.messages_json::text, c.tools,
       c.response, c.tool_calls_json::text, c.stop_reason, c.token_input, c.token_output,
       c.cost_usd, c.latency_ms, c.streamed, c.error, c.created_at
FROM agent_llm_calls c
JOIN agent_runs r ON r.id = c.run_id
WHERE c.run_id = $1 AND r.project_id = $2
ORDER BY c.created_at ASC`, runID, project.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AgentLLMCall{}
	for rows.Next() {
		var c AgentLLMCall
		if err := rows.Scan(&c.ID, &c.RunID, &c.Provider, &c.Model, &c.MessagesJSON, &c.Tools,
			&c.Response, &c.ToolCallsJSON, &c.StopReason, &c.TokenInput, &c.TokenOutput,
			&c.CostUSD, &c.LatencyMS, &c.Streamed, &c.Error, &c.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
