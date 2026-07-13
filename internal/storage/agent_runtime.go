package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
)

// This file holds the runtime persistence for the agent module (§9, §14.7,
// §14.10): runs + tool-call traces, working-memory sessions, long-term memory,
// recommendations, and skills. It deliberately mirrors the repo's raw-pgx,
// inline-SQL convention; the agentcore <-> storage mapping lives in
// agentruntime/store_pg.go so agentcore stays free of any storage import.

// AgentRun is one persisted run (§9 agent_runs).
type AgentRun struct {
	ID          string     `json:"id"`
	ProjectID   string     `json:"project_id"`
	AgentID     string     `json:"agent_id"`
	Trigger     string     `json:"trigger"` // chat | scheduled | manual | webhook
	Status      string     `json:"status"`  // running | done | error
	TokenInput  int        `json:"token_input"`
	TokenOutput int        `json:"token_output"`
	CostUSD     float64    `json:"cost_usd"` // summed model cost for the run (§ tracing)
	Summary     string     `json:"summary"`
	StartedAt   time.Time  `json:"started_at"`
	FinishedAt  *time.Time `json:"finished_at,omitempty"`
}

// AgentToolCall is one persisted tool execution (§9 agent_tool_calls).
type AgentToolCall struct {
	ID         string    `json:"id"`
	RunID      string    `json:"run_id"`
	Tool       string    `json:"tool"`
	ArgsJSON   string    `json:"args_json"`
	Allowed    bool      `json:"allowed"`
	ResultMeta string    `json:"result_meta"`
	DurationMS int       `json:"duration_ms"`
	CreatedAt  time.Time `json:"created_at"`
}

// CreateAgentRun opens a run row and returns its id. No RBAC: callers are the
// chat handler (already authed) or the scheduler (system). agentID records which
// agent produced the run; an empty agentID defaults to the project's default
// agent (id == project_id), keeping the original single-agent path unchanged.
// sessionID is the client conversation id a chat run belongs to (empty for
// scheduled/webhook runs), so the client can reattach to the run after leaving.
func (s *Store) CreateAgentRun(ctx context.Context, projectID, agentID, trigger, sessionID string) (string, error) {
	if agentID == "" {
		agentID = projectID
	}
	var id string
	err := s.pg.QueryRow(ctx, `
INSERT INTO agent_runs (project_id, agent_id, trigger, status, session_id) VALUES ($1, $2, $3, 'running', $4)
RETURNING id::text`, projectID, agentID, trigger, sessionID).Scan(&id)
	return id, err
}

// LatestRunForSession returns the most recent run for a conversation (member-
// readable), letting a returning client hydrate the answer of a run it streamed
// before navigating away. Returns pgx.ErrNoRows when the session has no run yet.
func (s *Store) LatestRunForSession(ctx context.Context, userID, projectID, sessionID string) (AgentRun, error) {
	project, err := s.ProjectByIDForUser(ctx, userID, projectID)
	if err != nil {
		return AgentRun{}, err
	}
	var r AgentRun
	err = s.pg.QueryRow(ctx, `
SELECT id::text, project_id::text, coalesce(agent_id, project_id)::text, trigger, status, token_input, token_output, cost_usd, summary, started_at, finished_at
FROM agent_runs WHERE project_id = $1 AND session_id = $2 ORDER BY started_at DESC LIMIT 1`, project.ID, sessionID).
		Scan(&r.ID, &r.ProjectID, &r.AgentID, &r.Trigger, &r.Status, &r.TokenInput, &r.TokenOutput, &r.CostUSD, &r.Summary, &r.StartedAt, &r.FinishedAt)
	if err != nil {
		return AgentRun{}, err
	}
	return r, nil
}

// SweepStaleRuns marks runs stuck in 'running' past olderThan as errored, so a
// run whose process died (or whose detached context hit its ceiling without
// persisting a terminal status) doesn't linger forever in the UI. Returns the
// number of rows swept. System-level (no RBAC): the scheduler calls it.
func (s *Store) SweepStaleRuns(ctx context.Context, olderThan time.Duration) (int64, error) {
	tag, err := s.pg.Exec(ctx, `
UPDATE agent_runs
SET status = 'error', summary = CASE WHEN summary = '' THEN 'run timed out' ELSE summary END, finished_at = now()
WHERE status = 'running' AND started_at < now() - $1::interval`,
		fmt.Sprintf("%d seconds", int64(olderThan.Seconds())))
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// FinishAgentRun closes a run with status, summary, summed token usage, and the
// summed model cost in USD (from the tracing/pricing layer).
func (s *Store) FinishAgentRun(ctx context.Context, runID, status, summary string, tokenIn, tokenOut int, costUSD float64) error {
	_, err := s.pg.Exec(ctx, `
UPDATE agent_runs SET status = $2, summary = $3, token_input = $4, token_output = $5, cost_usd = $6, finished_at = now()
WHERE id = $1`, runID, status, summary, tokenIn, tokenOut, costUSD)
	return err
}

// RecordAgentToolCall persists one tool-call trace projection.
func (s *Store) RecordAgentToolCall(ctx context.Context, runID string, tc AgentToolCall) error {
	args := tc.ArgsJSON
	if args == "" {
		args = "{}"
	}
	_, err := s.pg.Exec(ctx, `
INSERT INTO agent_tool_calls (run_id, tool, args_json, allowed, result_meta, duration_ms)
VALUES ($1, $2, $3::jsonb, $4, $5, $6)`,
		runID, tc.Tool, args, tc.Allowed, tc.ResultMeta, tc.DurationMS)
	return err
}

// ListAgentRuns returns recent runs for a project (member-readable).
func (s *Store) ListAgentRuns(ctx context.Context, userID, projectID string, limit int) ([]AgentRun, error) {
	project, err := s.ProjectByIDForUser(ctx, userID, projectID)
	if err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.pg.Query(ctx, `
SELECT id::text, project_id::text, coalesce(agent_id, project_id)::text, trigger, status, token_input, token_output, cost_usd, summary, started_at, finished_at
FROM agent_runs WHERE project_id = $1 ORDER BY started_at DESC LIMIT $2`, project.ID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AgentRun{}
	for rows.Next() {
		var r AgentRun
		if err := rows.Scan(&r.ID, &r.ProjectID, &r.AgentID, &r.Trigger, &r.Status, &r.TokenInput, &r.TokenOutput, &r.CostUSD, &r.Summary, &r.StartedAt, &r.FinishedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// GetAgentRun returns a run plus its tool-call trace (member-readable).
func (s *Store) GetAgentRun(ctx context.Context, userID, projectID, runID string) (AgentRun, []AgentToolCall, error) {
	project, err := s.ProjectByIDForUser(ctx, userID, projectID)
	if err != nil {
		return AgentRun{}, nil, err
	}
	var r AgentRun
	err = s.pg.QueryRow(ctx, `
SELECT id::text, project_id::text, coalesce(agent_id, project_id)::text, trigger, status, token_input, token_output, cost_usd, summary, started_at, finished_at
FROM agent_runs WHERE id = $1 AND project_id = $2`, runID, project.ID).
		Scan(&r.ID, &r.ProjectID, &r.AgentID, &r.Trigger, &r.Status, &r.TokenInput, &r.TokenOutput, &r.CostUSD, &r.Summary, &r.StartedAt, &r.FinishedAt)
	if err != nil {
		return AgentRun{}, nil, err
	}
	rows, err := s.pg.Query(ctx, `
SELECT id::text, run_id::text, tool, args_json::text, allowed, result_meta, duration_ms, created_at
FROM agent_tool_calls WHERE run_id = $1 ORDER BY created_at ASC`, runID)
	if err != nil {
		return r, nil, err
	}
	defer rows.Close()
	calls := []AgentToolCall{}
	for rows.Next() {
		var tc AgentToolCall
		if err := rows.Scan(&tc.ID, &tc.RunID, &tc.Tool, &tc.ArgsJSON, &tc.Allowed, &tc.ResultMeta, &tc.DurationMS, &tc.CreatedAt); err != nil {
			return r, nil, err
		}
		calls = append(calls, tc)
	}
	return r, calls, rows.Err()
}

// --- Working-memory sessions (§14.7) ---

// AgentSession is the persisted message history of one run/thread. Messages are
// stored as an opaque JSON blob; the consumer owns the message shape.
type AgentSession struct {
	ID        string    `json:"id"`
	ScopeID   string    `json:"scope_id"`
	ParentID  string    `json:"parent_id,omitempty"`
	Messages  []byte    `json:"messages"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// CreateAgentSession opens an empty working-memory thread.
func (s *Store) CreateAgentSession(ctx context.Context, scopeID string) (AgentSession, error) {
	var out AgentSession
	err := s.pg.QueryRow(ctx, `
INSERT INTO agent_sessions (scope_id, messages) VALUES ($1, '[]'::jsonb)
RETURNING id::text, scope_id::text, coalesce(parent_id::text,''), messages, created_at, updated_at`, scopeID).
		Scan(&out.ID, &out.ScopeID, &out.ParentID, &out.Messages, &out.CreatedAt, &out.UpdatedAt)
	return out, err
}

// GetAgentSession loads a thread by id.
func (s *Store) GetAgentSession(ctx context.Context, id string) (AgentSession, error) {
	var out AgentSession
	err := s.pg.QueryRow(ctx, `
SELECT id::text, scope_id::text, coalesce(parent_id::text,''), messages, created_at, updated_at
FROM agent_sessions WHERE id = $1`, id).
		Scan(&out.ID, &out.ScopeID, &out.ParentID, &out.Messages, &out.CreatedAt, &out.UpdatedAt)
	return out, err
}

// SaveAgentSession persists the message history of a thread.
func (s *Store) SaveAgentSession(ctx context.Context, sess AgentSession) error {
	msgs := sess.Messages
	if len(msgs) == 0 {
		msgs = []byte("[]")
	}
	_, err := s.pg.Exec(ctx, `
UPDATE agent_sessions SET messages = $2::jsonb, updated_at = now() WHERE id = $1`, sess.ID, string(msgs))
	return err
}

// ForkAgentSession branches a thread so the agent can explore without losing the
// trunk (pi SessionRepo.fork).
func (s *Store) ForkAgentSession(ctx context.Context, id string) (AgentSession, error) {
	var out AgentSession
	err := s.pg.QueryRow(ctx, `
INSERT INTO agent_sessions (scope_id, parent_id, messages)
SELECT scope_id, id, messages FROM agent_sessions WHERE id = $1
RETURNING id::text, scope_id::text, coalesce(parent_id::text,''), messages, created_at, updated_at`, id).
		Scan(&out.ID, &out.ScopeID, &out.ParentID, &out.Messages, &out.CreatedAt, &out.UpdatedAt)
	return out, err
}

// --- Long-term memory (§14.7) ---

// AgentMemoryRow is one durable memory entry. Embedding is the optional semantic
// vector used by vector recall; nil/empty means the row predates embeddings or
// no embedder was configured, and it is recalled by keyword instead.
type AgentMemoryRow struct {
	ID         string    `json:"id"`
	ScopeID    string    `json:"scope_id"`
	Kind       string    `json:"kind"`
	Content    string    `json:"content"`
	Tags       []string  `json:"tags"`
	Confidence float64   `json:"confidence"`
	SourceRun  string    `json:"source_run_id"`
	CreatedAt  time.Time `json:"created_at"`
	Embedding  []float32 `json:"-"`
}

// RememberAgentMemory persists a memory entry. Caller redacts PII first (§7) and
// supplies an optional embedding for semantic recall.
func (s *Store) RememberAgentMemory(ctx context.Context, m AgentMemoryRow) error {
	var srun any
	if m.SourceRun != "" {
		srun = m.SourceRun
	}
	tags := m.Tags
	if tags == nil {
		tags = []string{}
	}
	var embedding any // nil => SQL NULL => keyword-only recall
	if len(m.Embedding) > 0 {
		b, err := json.Marshal(m.Embedding)
		if err != nil {
			return err
		}
		embedding = string(b)
	}
	_, err := s.pg.Exec(ctx, `
INSERT INTO agent_memory (scope_id, kind, content, tags, confidence, source_run_id, embedding)
VALUES ($1, $2, $3, $4, $5, $6, $7)`, m.ScopeID, m.Kind, m.Content, tags, m.Confidence, srun, embedding)
	return err
}

// RecallAgentMemoryCandidates returns recent entries that have an embedding, for
// Go-side cosine ranking (vector recall, §14.7). Bounded so ranking stays cheap;
// pgvector + an ANN index is the scale upgrade when per-scope memory grows large.
func (s *Store) RecallAgentMemoryCandidates(ctx context.Context, scopeID string, max int) ([]AgentMemoryRow, error) {
	if max <= 0 || max > 1000 {
		max = 500
	}
	rows, err := s.pg.Query(ctx, `
SELECT id::text, scope_id::text, kind, content, tags, confidence,
       coalesce(source_run_id::text,''), created_at, embedding
FROM agent_memory
WHERE scope_id = $1 AND embedding IS NOT NULL
ORDER BY created_at DESC LIMIT $2`, scopeID, max)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanMemoryRowsWithEmbedding(rows)
}

// memoryStopwords are high-frequency function words dropped from a recall query
// so a natural-language question reduces to its content terms instead of matching
// on noise. The platform is bilingual (Vietnamese + English) and may add more
// languages, so the set covers both and is extended per language as needed;
// missing a stopword only adds mild over-match (ranking still favours the most
// on-topic row), while keeping the list lean avoids dropping real content terms.
var memoryStopwords = map[string]bool{
	// English
	"the": true, "and": true, "for": true, "are": true, "was": true, "were": true,
	"who": true, "what": true, "when": true, "where": true, "why": true, "how": true,
	"with": true, "this": true, "that": true, "from": true, "into": true, "about": true,
	"please": true, "tell": true, "give": true, "show": true, "your": true, "you": true,
	"can": true, "did": true, "does": true, "has": true, "had": true, "have": true,
	"our": true, "any": true, "all": true, "use": true, "using": true, "should": true,
	"which": true, "their": true, "there": true, "then": true, "than": true,
	"is": true, "of": true, "to": true, "an": true, "in": true, "on": true, "at": true,
	"by": true, "or": true, "as": true, "it": true, "be": true, "we": true, "do": true,
	"my": true, "me": true, "so": true, "if": true, "no": true, "up": true,
	// Vietnamese
	"và": true, "là": true, "của": true, "có": true, "không": true, "cho": true,
	"một": true, "các": true, "những": true, "được": true, "người": true, "này": true,
	"đó": true, "thì": true, "mà": true, "ra": true, "vào": true, "lên": true,
	"khi": true, "nếu": true, "để": true, "với": true, "từ": true, "về": true,
	"bị": true, "đã": true, "sẽ": true, "đang": true, "rất": true, "cũng": true,
	"nên": true, "hay": true, "hoặc": true, "tôi": true, "bạn": true, "chúng": true,
	"ai": true, "gì": true, "nào": true, "sao": true, "bao": true, "theo": true,
	"trong": true, "ngoài": true, "trên": true, "dưới": true, "cái": true, "con": true,
	"làm": true, "đến": true, "cùng": true, "bởi": true, "hơn": true, "như": true,
}

// extractKeywords reduces a free-text recall query to its distinct, lower-cased
// content terms: split on any non-letter/non-number rune (Unicode-aware, so
// Vietnamese diacritics and any future language's letters are preserved), drop
// stopwords and single-rune tokens, then de-duplicate preserving order. The
// minimum length is 2 runes — not 3 — precisely so meaningful short Vietnamese
// words (mẹ, vợ, nợ, Hà, Lê) survive; English short function words are handled by
// the stopword set instead of by length. Pure (no IO) so the tokenizer is tested
// without Postgres. An all-stopword query yields no keywords, which the caller
// treats as "no usable terms" and degrades to recency.
func extractKeywords(query string) []string {
	fields := strings.FieldsFunc(query, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsNumber(r)
	})
	seen := map[string]bool{}
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		w := strings.ToLower(f)
		if utf8.RuneCountInString(w) < 2 || memoryStopwords[w] || seen[w] {
			continue
		}
		seen[w] = true
		out = append(out, w)
	}
	return out
}

// RecallAgentMemory returns entries relevant to query by case-insensitive
// keyword match on content or tags. The query is tokenized into content terms
// (extractKeywords); a row matches when it contains ANY term, and rows are
// ranked by how many distinct terms they match (relevance) then recency, so the
// most on-topic memory surfaces first. An empty query — or one that reduces to
// no usable terms — returns the most recent entries (recency fallback).
// Vector recall (when embeddings exist) is a higher-relevance path layered above
// this in the consumer; this keyword path is the always-available floor.
func (s *Store) RecallAgentMemory(ctx context.Context, scopeID, query string, limit int) ([]AgentMemoryRow, error) {
	if limit <= 0 || limit > 50 {
		limit = 8
	}
	keywords := extractKeywords(query)
	if len(keywords) == 0 {
		// No usable terms: most-recent entries for the scope.
		rows, err := s.pg.Query(ctx, `
SELECT id::text, scope_id::text, kind, content, tags, confidence, coalesce(source_run_id::text,''), created_at
FROM agent_memory
WHERE scope_id = $1
ORDER BY created_at DESC LIMIT $2`, scopeID, limit)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		return scanMemoryRows(rows)
	}

	// Build a per-keyword "matches content or any tag" predicate, OR'd for the
	// WHERE filter and summed (each ::int) for the relevance score in ORDER BY.
	// Keywords are bound as parameters $2..$N+1 ($1 is scope, last is the limit).
	args := make([]any, 0, len(keywords)+2)
	args = append(args, scopeID)
	conds := make([]string, 0, len(keywords))
	for i, kw := range keywords {
		p := i + 2 // $2, $3, ...
		args = append(args, kw)
		conds = append(conds, fmt.Sprintf(
			"(content ILIKE '%%'||$%d||'%%' OR EXISTS (SELECT 1 FROM unnest(tags) t WHERE t ILIKE '%%'||$%d||'%%'))",
			p, p))
	}
	args = append(args, limit)
	limitParam := len(keywords) + 2 // $N+1

	where := strings.Join(conds, " OR ")
	score := make([]string, 0, len(conds))
	for _, c := range conds {
		score = append(score, "("+c+")::int")
	}
	scoreExpr := strings.Join(score, " + ")

	q := fmt.Sprintf(`
SELECT id::text, scope_id::text, kind, content, tags, confidence, coalesce(source_run_id::text,''), created_at
FROM agent_memory
WHERE scope_id = $1 AND (%s)
ORDER BY (%s) DESC, created_at DESC LIMIT $%d`, where, scoreExpr, limitParam)

	rows, err := s.pg.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanMemoryRows(rows)
}

// ListAgentMemory returns recent entries for a scope (member-readable).
func (s *Store) ListAgentMemory(ctx context.Context, userID, projectID string, limit int) ([]AgentMemoryRow, error) {
	project, err := s.ProjectByIDForUser(ctx, userID, projectID)
	if err != nil {
		return nil, err
	}
	return s.RecallAgentMemory(ctx, project.ID, "", limit)
}

// DeleteAgentMemory removes one entry (owner/admin only).
func (s *Store) DeleteAgentMemory(ctx context.Context, userID, projectID, id string) error {
	project, err := s.ProjectByIDForUser(ctx, userID, projectID)
	if err != nil {
		return err
	}
	canManage, err := s.userCanManageWorkspace(ctx, userID, project.WorkspaceID)
	if err != nil {
		return err
	}
	if !canManage {
		return errAgentForbidden
	}
	_, err = s.pg.Exec(ctx, `DELETE FROM agent_memory WHERE id = $1 AND scope_id = $2`, id, project.ID)
	return err
}

func scanMemoryRows(rows pgx.Rows) ([]AgentMemoryRow, error) {
	out := []AgentMemoryRow{}
	for rows.Next() {
		var m AgentMemoryRow
		if err := rows.Scan(&m.ID, &m.ScopeID, &m.Kind, &m.Content, &m.Tags, &m.Confidence, &m.SourceRun, &m.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// scanMemoryRowsWithEmbedding scans the keyword columns plus the JSONB embedding
// (parsed into a float vector) for the vector-recall candidate query.
func scanMemoryRowsWithEmbedding(rows pgx.Rows) ([]AgentMemoryRow, error) {
	out := []AgentMemoryRow{}
	for rows.Next() {
		var m AgentMemoryRow
		var emb []byte
		if err := rows.Scan(&m.ID, &m.ScopeID, &m.Kind, &m.Content, &m.Tags, &m.Confidence, &m.SourceRun, &m.CreatedAt, &emb); err != nil {
			return nil, err
		}
		if len(emb) > 0 {
			_ = json.Unmarshal(emb, &m.Embedding) // best-effort; bad vector → keyword fallback
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// --- Recommendations (§9 agent_recommendations) ---

// AgentRecommendation is one growth recommendation.
type AgentRecommendation struct {
	ID           string    `json:"id"`
	ProjectID    string    `json:"project_id"`
	RunID        string    `json:"run_id,omitempty"`
	Category     string    `json:"category"`
	Title        string    `json:"title"`
	Rationale    string    `json:"rationale"`
	EvidenceJSON string    `json:"evidence_json"`
	ImpactScore  float64   `json:"impact_score"`
	Status       string    `json:"status"`
	AckNote      string    `json:"ack_note"`
	CreatedAt    time.Time `json:"created_at"`
}

// CreateRecommendation persists a recommendation and returns its id.
func (s *Store) CreateRecommendation(ctx context.Context, rec AgentRecommendation) (string, error) {
	evidence := rec.EvidenceJSON
	if evidence == "" {
		evidence = "{}"
	}
	category := rec.Category
	if category == "" {
		category = "growth"
	}
	var runArg any
	if rec.RunID != "" {
		runArg = rec.RunID
	}
	var id string
	err := s.pg.QueryRow(ctx, `
INSERT INTO agent_recommendations (project_id, run_id, category, title, rationale, evidence_json, impact_score)
VALUES ($1, $2, $3, $4, $5, $6::jsonb, $7)
RETURNING id::text`, rec.ProjectID, runArg, category, rec.Title, rec.Rationale, evidence, rec.ImpactScore).Scan(&id)
	return id, err
}

// ListRecommendations returns open-first recommendations ranked by impact.
func (s *Store) ListRecommendations(ctx context.Context, userID, projectID string) ([]AgentRecommendation, error) {
	project, err := s.ProjectByIDForUser(ctx, userID, projectID)
	if err != nil {
		return nil, err
	}
	rows, err := s.pg.Query(ctx, `
SELECT id::text, project_id::text, coalesce(run_id::text,''), category, title, rationale,
       evidence_json::text, impact_score, status, ack_note, created_at
FROM agent_recommendations WHERE project_id = $1
ORDER BY (status = 'open') DESC, impact_score DESC, created_at DESC`, project.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AgentRecommendation{}
	for rows.Next() {
		var r AgentRecommendation
		if err := rows.Scan(&r.ID, &r.ProjectID, &r.RunID, &r.Category, &r.Title, &r.Rationale,
			&r.EvidenceJSON, &r.ImpactScore, &r.Status, &r.AckNote, &r.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// AckRecommendation accepts or dismisses a recommendation with a note (§13.2).
func (s *Store) AckRecommendation(ctx context.Context, userID, projectID, id, status, note string) error {
	project, err := s.ProjectByIDForUser(ctx, userID, projectID)
	if err != nil {
		return err
	}
	if status != "accepted" && status != "dismissed" {
		return errors.New("status must be accepted or dismissed")
	}
	_, err = s.pg.Exec(ctx, `
UPDATE agent_recommendations SET status = $3, ack_note = $4 WHERE id = $1 AND project_id = $2`,
		id, project.ID, status, note)
	return err
}

// --- Skills (§14.3, §14.10) ---

// AgentSkill is a stored playbook.
type AgentSkill struct {
	ID          string    `json:"id"`
	ScopeID     string    `json:"scope_id"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	Body        string    `json:"body"`
	Enabled     bool      `json:"enabled"`
	Status      string    `json:"status"` // active | proposed
	Origin      string    `json:"origin"` // user | reflect
	UpdatedAt   time.Time `json:"updated_at"`
}

// ActiveSkillHeadersForScope returns enabled, active skill metadata for a scope
// without loading the body. The run path selects against these headers first,
// then fetches full content only for the chosen skills.
func (s *Store) ActiveSkillHeadersForScope(ctx context.Context, scopeID string) ([]AgentSkill, error) {
	rows, err := s.pg.Query(ctx, `
SELECT id::text, scope_id::text, name, description, '' AS body, enabled, status, origin, updated_at
FROM agent_skills WHERE scope_id = $1 AND status = 'active' AND enabled = true
ORDER BY name ASC`, scopeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanSkillRows(rows)
}

// SkillBodiesByID returns the full markdown body for the given active skill ids
// within one scope. Missing ids are skipped so callers can tolerate concurrent
// deletes/edits without failing the whole run.
func (s *Store) SkillBodiesByID(ctx context.Context, scopeID string, ids []string) (map[string]string, error) {
	out := make(map[string]string, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := s.pg.Query(ctx, `
SELECT id::text, body
FROM agent_skills
WHERE scope_id = $1 AND status = 'active' AND enabled = true AND id = ANY($2)
`, scopeID, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, body string
		if err := rows.Scan(&id, &body); err != nil {
			return nil, err
		}
		out[id] = body
	}
	return out, rows.Err()
}

// ListAgentSkills returns every skill (active + proposed) for a project member.
func (s *Store) ListAgentSkills(ctx context.Context, userID, projectID, agentID string) ([]AgentSkill, error) {
	_, scopeID, err := s.agentScope(ctx, userID, projectID, agentID)
	if err != nil {
		return nil, err
	}
	rows, err := s.pg.Query(ctx, `
SELECT id::text, scope_id::text, name, description, body, enabled, status, origin, updated_at
FROM agent_skills WHERE scope_id = $1 ORDER BY status DESC, name ASC`, scopeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanSkillRows(rows)
}

// UpsertAgentSkill creates or updates a user-authored skill (owner/admin). When
// id is empty a new skill is created. origin is forced to 'user', status active.
func (s *Store) UpsertAgentSkill(ctx context.Context, userID, projectID, agentID string, sk AgentSkill) (AgentSkill, error) {
	project, scopeID, err := s.agentScope(ctx, userID, projectID, agentID)
	if err != nil {
		return AgentSkill{}, err
	}
	canManage, err := s.userCanManageWorkspace(ctx, userID, project.WorkspaceID)
	if err != nil {
		return AgentSkill{}, err
	}
	if !canManage {
		return AgentSkill{}, errAgentForbidden
	}
	var out AgentSkill
	if sk.ID == "" {
		err = s.pg.QueryRow(ctx, `
INSERT INTO agent_skills (scope_id, name, description, body, enabled, status, origin)
VALUES ($1, $2, $3, $4, $5, 'active', 'user')
RETURNING id::text, scope_id::text, name, description, body, enabled, status, origin, updated_at`,
			scopeID, sk.Name, sk.Description, sk.Body, sk.Enabled).
			Scan(&out.ID, &out.ScopeID, &out.Name, &out.Description, &out.Body, &out.Enabled, &out.Status, &out.Origin, &out.UpdatedAt)
	} else {
		err = s.pg.QueryRow(ctx, `
UPDATE agent_skills SET name = $3, description = $4, body = $5, enabled = $6, updated_at = now()
WHERE id = $1 AND scope_id = $2
RETURNING id::text, scope_id::text, name, description, body, enabled, status, origin, updated_at`,
			sk.ID, scopeID, sk.Name, sk.Description, sk.Body, sk.Enabled).
			Scan(&out.ID, &out.ScopeID, &out.Name, &out.Description, &out.Body, &out.Enabled, &out.Status, &out.Origin, &out.UpdatedAt)
	}
	if err != nil {
		return AgentSkill{}, err
	}
	_ = s.recordWorkspaceAudit(ctx, project.WorkspaceID, userID, "agent.skill.update", "project", project.ID, project.Name, "{}")
	return out, nil
}

// DeleteAgentSkill removes a skill (owner/admin).
func (s *Store) DeleteAgentSkill(ctx context.Context, userID, projectID, agentID, id string) error {
	project, scopeID, err := s.agentScope(ctx, userID, projectID, agentID)
	if err != nil {
		return err
	}
	canManage, err := s.userCanManageWorkspace(ctx, userID, project.WorkspaceID)
	if err != nil {
		return err
	}
	if !canManage {
		return errAgentForbidden
	}
	_, err = s.pg.Exec(ctx, `DELETE FROM agent_skills WHERE id = $1 AND scope_id = $2`, id, scopeID)
	return err
}

// ApproveAgentSkill flips a reflect-proposed skill to active (owner/admin). This
// is the only path by which a self-proposed skill becomes live (§14.9).
func (s *Store) ApproveAgentSkill(ctx context.Context, userID, projectID, agentID, id string) error {
	project, scopeID, err := s.agentScope(ctx, userID, projectID, agentID)
	if err != nil {
		return err
	}
	canManage, err := s.userCanManageWorkspace(ctx, userID, project.WorkspaceID)
	if err != nil {
		return err
	}
	if !canManage {
		return errAgentForbidden
	}
	_, err = s.pg.Exec(ctx, `
UPDATE agent_skills SET status = 'active', enabled = true, updated_at = now()
WHERE id = $1 AND scope_id = $2 AND status = 'proposed'`, id, scopeID)
	return err
}

// ProposeAgentSkill records a reflect-pass skill proposal (status proposed,
// origin reflect). System-initiated; no RBAC. Skipped silently if a same-named
// skill already exists for the scope to avoid proposal spam.
func (s *Store) ProposeAgentSkill(ctx context.Context, scopeID string, sk AgentSkill) error {
	_, err := s.pg.Exec(ctx, `
INSERT INTO agent_skills (scope_id, name, description, body, enabled, status, origin)
SELECT $1, $2, $3, $4, false, 'proposed', 'reflect'
WHERE NOT EXISTS (SELECT 1 FROM agent_skills WHERE scope_id = $1 AND name = $2)`,
		scopeID, sk.Name, sk.Description, sk.Body)
	return err
}

func scanSkillRows(rows pgx.Rows) ([]AgentSkill, error) {
	out := []AgentSkill{}
	for rows.Next() {
		var sk AgentSkill
		if err := rows.Scan(&sk.ID, &sk.ScopeID, &sk.Name, &sk.Description, &sk.Body, &sk.Enabled, &sk.Status, &sk.Origin, &sk.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, sk)
	}
	return out, rows.Err()
}

// ScheduledAgentProjects returns project ids whose config is scheduled-capable
// (autonomy 'scheduled' or the higher 'auto' rung) and the agent enabled, with
// their cron expression — the candidate set the scheduler evaluates each tick.
func (s *Store) ScheduledAgentProjects(ctx context.Context) (map[string]string, error) {
	rows, err := s.pg.Query(ctx, `
SELECT project_id::text, schedule_cron FROM agent_configs
WHERE enabled = true AND autonomy IN ('scheduled', 'auto') AND schedule_cron <> ''`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var id, cron string
		if err := rows.Scan(&id, &cron); err != nil {
			return nil, err
		}
		out[id] = cron
	}
	return out, rows.Err()
}

// AgentConfigForRun loads a project's full config for a system-initiated run
// (scheduler), bypassing per-user RBAC since there is no requesting user.
func (s *Store) AgentConfigForRun(ctx context.Context, projectID string) (AgentConfig, error) {
	return s.AgentConfigForRunAgent(ctx, projectID, projectID)
}

// AgentConfigForRunAgent resolves project-level run gates plus the selected
// agent's capability scopes. Missing per-agent capabilities inherit the project
// config so existing/default agents keep their current access.
func (s *Store) AgentConfigForRunAgent(ctx context.Context, projectID, scopeID string) (AgentConfig, error) {
	cfg, err := s.readAgentConfig(ctx, projectID)
	if err != nil {
		return AgentConfig{}, err
	}
	scopes, err := s.AgentCapabilitiesForRun(ctx, projectID, scopeID)
	if err != nil {
		return AgentConfig{}, err
	}
	cfg.Scopes = scopes
	return cfg, nil
}

// AgentDefinitionForRun loads SOUL/AGENTS for a system-initiated run, keyed by
// the running agent's scope id (the default agent's scope id equals its project
// id, so the original single-agent path is unchanged).
func (s *Store) AgentDefinitionForRun(ctx context.Context, scopeID string) (AgentDefinition, error) {
	def := AgentDefinition{ScopeID: scopeID}
	err := s.pg.QueryRow(ctx, `SELECT soul_md, agents_md FROM agent_definitions WHERE scope_id = $1`, scopeID).
		Scan(&def.SoulMD, &def.AgentsMD)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return AgentDefinition{}, err
	}
	return def, nil
}
