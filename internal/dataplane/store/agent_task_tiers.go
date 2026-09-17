package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// Task kinds — the 5 real LLM call sites where a tier choice takes effect. See
// internal/runtime: triage is the orchestrator front-desk classifier, run is
// the loop's primary reasoning turns, compaction is the in-loop summary call,
// reflection is the post-run reflect pass, and advisor is the reviewer that
// checks the answer before the run is allowed to finish.
const (
	TaskTriage     = "triage"
	TaskRun        = "run"
	TaskCompaction = "compaction"
	TaskReflection = "reflection"
	TaskAdvisor    = "advisor"
)

// AgentTaskTiers maps each task kind to one of the workspace model tiers
// (lite/flash/pro). It is the per-agent half of the model config: which workspace
// tier does each kind of work draw from.
type AgentTaskTiers map[string]string

// DefaultTaskTiers reproduces today's behavior (with compaction nudged down to
// lite, a deliberate cost improvement over borrowing the run's flash rung).
//
// The advisor defaults to pro because reviewing is the harder half of the job:
// it reads a whole run cold and has to be right about what is wrong with it,
// while the run itself had the context built up turn by turn. A reviewer weaker
// than the agent it reviews spends tokens to produce noise the agent then has
// to argue with. It costs nothing by default — the advisor is off unless the
// operator turns it on (see agent_advisor).
func DefaultTaskTiers() AgentTaskTiers {
	return AgentTaskTiers{
		TaskTriage:     "lite",
		TaskRun:        "flash",
		TaskCompaction: "lite",
		TaskReflection: "pro",
		TaskAdvisor:    "pro",
	}
}

var taskTierKinds = map[string]bool{TaskTriage: true, TaskRun: true, TaskCompaction: true, TaskReflection: true, TaskAdvisor: true}
var taskTierValues = map[string]bool{"lite": true, "flash": true, "pro": true}

// merge overlays the stored map over the defaults so a partial or absent row
// still resolves every kind — including one added after the row was written.
func (m AgentTaskTiers) merge() AgentTaskTiers {
	out := DefaultTaskTiers()
	for k, v := range m {
		if taskTierKinds[k] && taskTierValues[v] {
			out[k] = v
		}
	}
	return out
}

// GetAgentTaskTiers returns the merged task→tier map for an agent, for any
// project member.
func (s *Store) GetAgentTaskTiers(ctx context.Context, userID, projectID, agentID string) (AgentTaskTiers, error) {
	_, scopeID, err := s.agentScope(ctx, userID, projectID, agentID)
	if err != nil {
		return nil, err
	}
	stored, err := s.readAgentTaskTiers(ctx, scopeID)
	if err != nil {
		return nil, err
	}
	return stored.merge(), nil
}

// readAgentTaskTiers loads the raw stored map (empty when no row exists).
func (s *Store) readAgentTaskTiers(ctx context.Context, scopeID string) (AgentTaskTiers, error) {
	var raw []byte
	err := s.pg.QueryRow(ctx, `SELECT tiers FROM agent_task_tiers WHERE scope_id = $1`, scopeID).Scan(&raw)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return AgentTaskTiers{}, nil
		}
		return nil, err
	}
	out := AgentTaskTiers{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &out); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// UpsertAgentTaskTiers writes an agent's task→tier map; workspace owner/admin
// only. Unknown kinds or tier values are rejected.
func (s *Store) UpsertAgentTaskTiers(ctx context.Context, userID, projectID, agentID string, in AgentTaskTiers) (AgentTaskTiers, error) {
	project, scopeID, err := s.agentScope(ctx, userID, projectID, agentID)
	if err != nil {
		return nil, err
	}
	canManage, err := s.userCanManageWorkspace(ctx, userID, project.WorkspaceID)
	if err != nil {
		return nil, err
	}
	if !canManage {
		return nil, ErrAgentForbidden
	}

	clean := AgentTaskTiers{}
	for k, v := range in {
		if !taskTierKinds[k] {
			return nil, fmt.Errorf("unknown task kind %q", k)
		}
		if !taskTierValues[v] {
			return nil, fmt.Errorf("unknown tier %q for task %q", v, k)
		}
		clean[k] = v
	}
	payload, err := json.Marshal(clean)
	if err != nil {
		return nil, err
	}

	_, err = s.pg.Exec(ctx, `
INSERT INTO agent_task_tiers (scope_id, tiers) VALUES ($1, $2)
ON CONFLICT (scope_id) DO UPDATE SET tiers = EXCLUDED.tiers, updated_at = now()`, scopeID, payload)
	if err != nil {
		return nil, err
	}

	_ = s.recordWorkspaceAudit(ctx, project.WorkspaceID, userID, "agent.task_tiers.update", "agent", scopeID, "", string(payload))
	return clean.merge(), nil
}

// TaskTiersForRun resolves the merged task→tier map for a run (system path, no
// requesting user). A partial or absent row still resolves every kind.
func (s *Store) TaskTiersForRun(ctx context.Context, scopeID string) (AgentTaskTiers, error) {
	stored, err := s.readAgentTaskTiers(ctx, scopeID)
	if err != nil {
		return nil, err
	}
	return stored.merge(), nil
}
