package storage

import (
	"context"
	"errors"
	"time"
)

// operators.go — the project-wide read model behind /operations.
//
// It stores nothing. An OPERATOR is not a new object type: it is one
// agent_triggers row, read next to the agent (or the team whose lead that agent
// is) that answers it and the runs it has produced. The definition the whole
// surface rests on is that an agent with no trigger is a teammate you message,
// and an agent with a trigger is work that happens whether or not anyone opens
// the tab — so the trigger, not the agent, is the unit.
//
// Two sources are unioned, for the same reason the scheduler scans two:
// agent_triggers (AgentGarden §7) and the legacy agent_configs.schedule_cron
// that still fires the default agent. A list built from the trigger table alone
// would show a workspace "nothing runs unattended" while its default agent
// wakes up every morning — the one failure this screen exists to prevent.

// Operator sources. A trigger-backed operator is fully controllable here; a
// config-backed one is the legacy project schedule, which is edited where it
// lives (the agent's own setup) and only surfaced here so the list is complete.
const (
	OperatorSourceTrigger = "trigger"
	OperatorSourceConfig  = "config"
)

// Operator is one standing unit of unattended work.
type Operator struct {
	ID     string `json:"id"`
	Source string `json:"source"`
	// Name is the owner's label. Empty on a row nobody has named yet; the UI
	// falls back to the agent's name rather than showing a cron expression as a
	// title.
	Name           string `json:"name"`
	Kind           string `json:"kind"` // schedule | webhook
	Enabled        bool   `json:"enabled"`
	Cron           string `json:"cron"`
	WebhookToken   string `json:"webhook_token"`
	PromptTemplate string `json:"prompt_template"`
	HMACSecretName string `json:"hmac_secret_name"`

	AgentID   string `json:"agent_id"`
	AgentName string `json:"agent_name"`
	// AgentEnabled is separate from Enabled: an armed trigger on a paused agent
	// fires nothing, and reporting it as "Armed" is the product telling the owner
	// that work is happening when it is not.
	AgentEnabled bool `json:"agent_enabled"`
	// TeamID/TeamName are set when this agent is a team's lead — the trigger then
	// runs the whole team through teamlead.go, so the honest label is the team.
	TeamID   string `json:"team_id,omitempty"`
	TeamName string `json:"team_name,omitempty"`

	RunCount     int        `json:"run_count"`
	RunningCount int        `json:"running_count"`
	Runs24h      int        `json:"runs_24h"`
	Errors24h    int        `json:"errors_24h"`
	Cost24h      float64    `json:"cost_24h"`
	LastRunAt    *time.Time `json:"last_run_at,omitempty"`
	LastStatus   string     `json:"last_status"`
	LastSummary  string     `json:"last_summary"`
	// ConsecutiveFailures counts error runs since the last successful one. It is
	// what turns "the last run failed" into "it has been failing since Tuesday",
	// which is the difference between a blip and an operator nobody is watching.
	ConsecutiveFailures int `json:"consecutive_failures"`
	// SharedHistory marks the case the run table cannot disambiguate: a run
	// records the CHANNEL that started it, not which trigger, so two schedules on
	// one agent read the same history. The UI must then say "this agent's
	// scheduled runs" rather than claim the numbers belong to this operator
	// alone.
	SharedHistory bool `json:"shared_history"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// runTriggerFor maps an operator kind to the `trigger` value the runtime writes
// on the runs it produces (runtime/scheduler.go publishes "scheduled").
func runTriggerFor(kind string) string {
	if kind == TriggerWebhook {
		return "webhook"
	}
	return "scheduled"
}

var errNoSuchOperator = errors.New("no operator with that id in this project")

// ErrNoSuchOperator reports whether err is the not-found from OperatorByID, so
// the HTTP layer answers 404 rather than 500.
func ErrNoSuchOperator(err error) bool { return errors.Is(err, errNoSuchOperator) }

// operatorSelect reads every trigger in the project with its agent, its team (if
// the agent leads one) and its run rollup.
//
// The run aggregates are scoped to agent × channel, which is as precise as the
// run table allows — see Operator.SharedHistory. They are bounded by the
// project+started_at index the monitor read already relies on.
const operatorSelect = `
SELECT t.id::text, t.name, t.kind, t.enabled, t.cron, t.webhook_token, t.prompt_template, t.hmac_secret_name,
       t.created_at, t.updated_at,
       a.id::text, a.name, a.enabled,
       coalesce(tm.id::text, ''), coalesce(tm.name, ''),
       coalesce(agg.run_count, 0), coalesce(agg.running_count, 0), coalesce(agg.runs_24h, 0),
       coalesce(agg.errors_24h, 0), coalesce(agg.cost_24h, 0),
       agg.last_run_at, coalesce(agg.last_status, ''), coalesce(agg.last_summary, ''),
       coalesce(fail.streak, 0),
       (SELECT count(*) > 1 FROM agent_triggers t2 WHERE t2.scope_id = t.scope_id AND t2.kind = t.kind)
FROM agent_triggers t
JOIN agents a ON a.id = t.scope_id
LEFT JOIN teams tm ON tm.lead_agent_id = a.id AND tm.project_id = a.project_id
LEFT JOIN LATERAL (
  SELECT count(*) AS run_count,
         count(*) FILTER (WHERE r.status = 'running') AS running_count,
         count(*) FILTER (WHERE r.started_at > now() - interval '24 hours') AS runs_24h,
         count(*) FILTER (WHERE r.status = 'error' AND r.started_at > now() - interval '24 hours') AS errors_24h,
         coalesce(sum(r.cost_usd) FILTER (WHERE r.started_at > now() - interval '24 hours'), 0) AS cost_24h,
         max(r.started_at) AS last_run_at,
         (array_agg(r.status ORDER BY r.started_at DESC))[1] AS last_status,
         (array_agg(r.summary ORDER BY r.started_at DESC))[1] AS last_summary
  FROM agent_runs r
  WHERE r.project_id = a.project_id AND coalesce(r.agent_id, r.project_id) = a.id
    AND r.trigger = CASE t.kind WHEN 'webhook' THEN 'webhook' ELSE 'scheduled' END
) agg ON true
LEFT JOIN LATERAL (
  SELECT count(*) AS streak
  FROM agent_runs r
  WHERE r.project_id = a.project_id AND coalesce(r.agent_id, r.project_id) = a.id
    AND r.trigger = CASE t.kind WHEN 'webhook' THEN 'webhook' ELSE 'scheduled' END
    AND r.status = 'error'
    AND r.started_at > coalesce((
      SELECT max(ok.started_at) FROM agent_runs ok
      WHERE ok.project_id = a.project_id AND coalesce(ok.agent_id, ok.project_id) = a.id
        AND ok.trigger = CASE t.kind WHEN 'webhook' THEN 'webhook' ELSE 'scheduled' END
        AND ok.status <> 'error'
    ), '-infinity'::timestamptz)
) fail ON true`

func scanOperator(row rowScanner) (Operator, error) {
	var op Operator
	err := row.Scan(&op.ID, &op.Name, &op.Kind, &op.Enabled, &op.Cron, &op.WebhookToken,
		&op.PromptTemplate, &op.HMACSecretName, &op.CreatedAt, &op.UpdatedAt,
		&op.AgentID, &op.AgentName, &op.AgentEnabled, &op.TeamID, &op.TeamName,
		&op.RunCount, &op.RunningCount, &op.Runs24h, &op.Errors24h, &op.Cost24h,
		&op.LastRunAt, &op.LastStatus, &op.LastSummary, &op.ConsecutiveFailures, &op.SharedHistory)
	op.Source = OperatorSourceTrigger
	return op, err
}

// ListOperators returns everything that runs unattended in one project: every
// agent_triggers row, plus the legacy project schedule when one is set.
// Member-readable, like the agent monitor it sits beside.
func (s *Store) ListOperators(ctx context.Context, userID, projectID string) ([]Operator, error) {
	project, err := s.ProjectByIDForUser(ctx, userID, projectID)
	if err != nil {
		return nil, err
	}
	rows, err := s.pg.Query(ctx, operatorSelect+`
WHERE a.project_id = $1
ORDER BY t.created_at ASC`, project.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Operator{}
	for rows.Next() {
		op, err := scanOperator(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, op)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	legacy, err := s.legacyScheduleOperator(ctx, project.ID)
	if err != nil {
		return nil, err
	}
	if legacy != nil {
		out = append(out, *legacy)
	}
	return out, nil
}

// OperatorByID reads one operator. The legacy id shape is handled here too, so
// the detail page and the run control have one lookup rather than two.
func (s *Store) OperatorByID(ctx context.Context, userID, projectID, id string) (Operator, error) {
	project, err := s.ProjectByIDForUser(ctx, userID, projectID)
	if err != nil {
		return Operator{}, err
	}
	if id == legacyOperatorID(project.ID) {
		legacy, err := s.legacyScheduleOperator(ctx, project.ID)
		if err != nil {
			return Operator{}, err
		}
		if legacy == nil {
			return Operator{}, errNoSuchOperator
		}
		return *legacy, nil
	}
	if !looksLikeUUID(id) {
		return Operator{}, errNoSuchOperator
	}
	row := s.pg.QueryRow(ctx, operatorSelect+`
WHERE a.project_id = $1 AND t.id = $2`, project.ID, id)
	op, err := scanOperator(row)
	if err != nil {
		// A trigger in another project is not this project's business: it reads as
		// absent, never as a permission error naming a row the caller cannot see.
		return Operator{}, errNoSuchOperator
	}
	return op, nil
}

// legacyOperatorID is the synthetic id of the project-level schedule. It is
// deliberately not a uuid so it can never collide with a trigger id, and so a
// reader can tell at a glance which control surface an id belongs to.
func legacyOperatorID(projectID string) string { return "config:" + projectID }

// legacyScheduleOperator projects agent_configs.schedule_cron — the schedule
// that predates agent_triggers and still fires the default agent — as an
// operator row.
//
// It is read-only from this surface: its cron and its on/off live on the
// project's agent config (autonomy rung included), and quietly rewriting that
// from a list of operators would change how every unattended run in the project
// is gated. The row exists so the list is honest about what runs; the edit
// happens where the setting actually is.
func (s *Store) legacyScheduleOperator(ctx context.Context, projectID string) (*Operator, error) {
	op := Operator{
		ID:      legacyOperatorID(projectID),
		Source:  OperatorSourceConfig,
		Kind:    TriggerSchedule,
		Name:    "Project schedule",
		AgentID: projectID,
	}
	var autonomy string
	err := s.pg.QueryRow(ctx, `
SELECT c.schedule_cron, c.enabled, c.autonomy,
       coalesce(a.name, 'Default agent'), coalesce(a.enabled, true)
FROM agent_configs c
LEFT JOIN agents a ON a.id = c.project_id
WHERE c.project_id = $1 AND c.schedule_cron <> ''`, projectID).
		Scan(&op.Cron, &op.Enabled, &autonomy, &op.AgentName, &op.AgentEnabled)
	if err != nil {
		// No config row, or no cron on it: there is no legacy schedule. Not an
		// error — most projects have none.
		return nil, nil
	}
	// The scheduler only fires this source on the 'scheduled' or 'auto' rung, so
	// a cron sitting under 'suggest' is armed in the config and dead in practice.
	// Report what actually happens.
	if autonomy != "scheduled" && autonomy != "auto" {
		op.Enabled = false
	}
	agg, err := s.operatorRunRollup(ctx, projectID, projectID, "scheduled")
	if err != nil {
		return nil, err
	}
	op.RunCount, op.RunningCount, op.Runs24h = agg.RunCount, agg.RunningCount, agg.Runs24h
	op.Errors24h, op.Cost24h = agg.Errors24h, agg.Cost24h
	op.LastRunAt, op.LastStatus, op.LastSummary = agg.LastRunAt, agg.LastStatus, agg.LastSummary
	op.ConsecutiveFailures = agg.ConsecutiveFailures
	// The default agent may also carry its own schedule triggers, and those runs
	// are indistinguishable from this one's.
	op.SharedHistory = true
	return &op, nil
}

// operatorRunRollup is the same aggregate the list computes inline, for the one
// row that has no trigger to join from.
func (s *Store) operatorRunRollup(ctx context.Context, projectID, agentID, trigger string) (Operator, error) {
	var op Operator
	err := s.pg.QueryRow(ctx, `
SELECT count(*),
       count(*) FILTER (WHERE r.status = 'running'),
       count(*) FILTER (WHERE r.started_at > now() - interval '24 hours'),
       count(*) FILTER (WHERE r.status = 'error' AND r.started_at > now() - interval '24 hours'),
       coalesce(sum(r.cost_usd) FILTER (WHERE r.started_at > now() - interval '24 hours'), 0),
       max(r.started_at),
       coalesce((array_agg(r.status ORDER BY r.started_at DESC))[1], ''),
       coalesce((array_agg(r.summary ORDER BY r.started_at DESC))[1], ''),
       (SELECT count(*) FROM agent_runs e
         WHERE e.project_id = $1 AND coalesce(e.agent_id, e.project_id) = $2 AND e.trigger = $3
           AND e.status = 'error'
           AND e.started_at > coalesce((SELECT max(ok.started_at) FROM agent_runs ok
             WHERE ok.project_id = $1 AND coalesce(ok.agent_id, ok.project_id) = $2 AND ok.trigger = $3
               AND ok.status <> 'error'), '-infinity'::timestamptz))
FROM agent_runs r
WHERE r.project_id = $1 AND coalesce(r.agent_id, r.project_id) = $2 AND r.trigger = $3`,
		projectID, agentID, trigger).
		Scan(&op.RunCount, &op.RunningCount, &op.Runs24h, &op.Errors24h, &op.Cost24h,
			&op.LastRunAt, &op.LastStatus, &op.LastSummary, &op.ConsecutiveFailures)
	return op, err
}

// OperatorRuns returns the run history behind one operator — the agent's runs on
// that channel, newest first. Scoped by agent × channel for the reason
// SharedHistory documents.
func (s *Store) OperatorRuns(ctx context.Context, userID, projectID, id string, limit int) ([]AgentRun, error) {
	op, err := s.OperatorByID(ctx, userID, projectID, id)
	if err != nil {
		return nil, err
	}
	project, err := s.ProjectByIDForUser(ctx, userID, projectID)
	if err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 200 {
		limit = 25
	}
	rows, err := s.pg.Query(ctx, `
SELECT id::text, project_id::text, coalesce(agent_id, project_id)::text, trigger, status,
       token_input, token_output, cost_usd, summary, started_at, finished_at
FROM agent_runs
WHERE project_id = $1 AND coalesce(agent_id, project_id) = $2 AND trigger = $3
ORDER BY started_at DESC LIMIT $4`, project.ID, op.AgentID, runTriggerFor(op.Kind), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AgentRun{}
	for rows.Next() {
		var r AgentRun
		if err := rows.Scan(&r.ID, &r.ProjectID, &r.AgentID, &r.Trigger, &r.Status, &r.TokenInput,
			&r.TokenOutput, &r.CostUSD, &r.Summary, &r.StartedAt, &r.FinishedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
