package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Alerting (#1). Three layers: condition storage (this file), an evaluation
// worker (internal/alerting, driven off the scheduler tick), and delivery
// (channels, doubling as the send_notification agent tool). A rule names a
// metric source, a condition, a schedule, and the channels to fan out to; it
// fires only on the ok→firing edge (last_state), so a sustained breach does not
// re-notify every tick. Follows the agent_triggers.go conventions: raw pgx,
// inline SQL, scope checks via project/workspace ownership.

var errAlertInvalidSource = errors.New("alert source_kind must be 'insight', 'sql', or 'agent_ops'")
var errAlertInvalidOp = errors.New("alert condition op must be 'gt', 'lt', or 'z_score'")

// AlertCondition is the threshold test for a rule. For gt/lt, Value is the
// bound. For z_score, Value is the sigma multiplier (e.g. 3), Window the number
// of prior buckets to build the baseline from, and MinEvents a mandatory floor
// below which the anomaly test is suppressed (low-volume metrics are noisy and
// would over-fire).
type AlertCondition struct {
	Op        string  `json:"op"` // gt | lt | z_score
	Value     float64 `json:"value"`
	Window    int     `json:"window,omitempty"`
	MinEvents int     `json:"min_events,omitempty"`
}

// AlertRule is one saved condition. SourceKind + SourceRef name what to measure;
// Condition how; Channels where to deliver. LastState edge-triggers delivery.
type AlertRule struct {
	ID         string         `json:"id"`
	ProjectID  string         `json:"project_id"`
	Name       string         `json:"name"`
	SourceKind string         `json:"source_kind"` // insight | sql | agent_ops
	SourceRef  string         `json:"source_ref"`  // chart/saved-query id or ops metric name
	Condition  AlertCondition `json:"condition"`
	Schedule   string         `json:"schedule_cron"`
	Channels   []string       `json:"channels"` // alert_channels ids
	Enabled    bool           `json:"enabled"`
	LastEvalAt *time.Time     `json:"last_eval_at,omitempty"`
	LastState  string         `json:"last_state"` // ok | firing
	CreatedAt  time.Time      `json:"created_at"`
}

// AlertChannel is a delivery target. Config holds the webhook URL / email address
// / Slack webhook; secrets route through the credential vault ({{cred:NAME}}) and
// are resolved only at the trust boundary in delivery.
type AlertChannel struct {
	ID          string          `json:"id"`
	WorkspaceID string          `json:"workspace_id"`
	Kind        string          `json:"kind"` // slack | email | webhook
	Name        string          `json:"name"`
	Config      json.RawMessage `json:"config"`
	CreatedAt   time.Time       `json:"created_at"`
}

// AlertEvent is one firing/recovery record.
type AlertEvent struct {
	ID      string          `json:"id"`
	RuleID  string          `json:"rule_id"`
	FiredAt time.Time       `json:"fired_at"`
	State   string          `json:"state"` // firing | ok
	Value   float64         `json:"value"`
	Payload json.RawMessage `json:"payload"`
}

func (s *Store) migrateAlerts(ctx context.Context) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS alert_channels (
	id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
	workspace_id UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
	kind VARCHAR(16) NOT NULL,
	name VARCHAR(255) NOT NULL,
	config JSONB NOT NULL DEFAULT '{}'::jsonb,
	created_at TIMESTAMPTZ NOT NULL DEFAULT now()
)`,
		`CREATE INDEX IF NOT EXISTS alert_channels_workspace_idx ON alert_channels (workspace_id)`,
		`CREATE TABLE IF NOT EXISTS alert_rules (
	id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
	project_id UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
	name VARCHAR(255) NOT NULL,
	source_kind VARCHAR(16) NOT NULL,
	source_ref TEXT NOT NULL DEFAULT '',
	condition JSONB NOT NULL DEFAULT '{}'::jsonb,
	schedule_cron VARCHAR(64) NOT NULL DEFAULT '*/5 * * * *',
	channels JSONB NOT NULL DEFAULT '[]'::jsonb,
	enabled BOOLEAN NOT NULL DEFAULT true,
	last_eval_at TIMESTAMPTZ,
	last_state VARCHAR(8) NOT NULL DEFAULT 'ok',
	created_at TIMESTAMPTZ NOT NULL DEFAULT now()
)`,
		`CREATE INDEX IF NOT EXISTS alert_rules_project_idx ON alert_rules (project_id)`,
		`CREATE TABLE IF NOT EXISTS alert_events (
	id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
	rule_id UUID NOT NULL REFERENCES alert_rules(id) ON DELETE CASCADE,
	fired_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	state VARCHAR(8) NOT NULL,
	value DOUBLE PRECISION NOT NULL DEFAULT 0,
	payload JSONB NOT NULL DEFAULT '{}'::jsonb
)`,
		`CREATE INDEX IF NOT EXISTS alert_events_rule_fired_idx ON alert_events (rule_id, fired_at DESC)`,
	}
	for _, stmt := range stmts {
		if _, err := s.pg.Exec(ctx, stmt); err != nil {
			return err
		}
	}
	return nil
}

func validateAlertSource(kind string) error {
	switch kind {
	case "insight", "sql", "agent_ops":
		return nil
	default:
		return errAlertInvalidSource
	}
}

func validateAlertOp(op string) error {
	switch op {
	case "gt", "lt", "z_score":
		return nil
	default:
		return errAlertInvalidOp
	}
}

// validateChannelsInWorkspace rejects a rule that references a channel outside its
// own workspace. Without this a workspace admin could attach an arbitrary channel
// UUID (belonging to another tenant) and fan deliveries out to that tenant's
// configured Slack/webhook/email endpoint — fanOut resolves ids with no workspace
// filter, so isolation must be enforced here at write time.
func (s *Store) validateChannelsInWorkspace(ctx context.Context, workspaceID string, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	var n int
	if err := s.pg.QueryRow(ctx, `
SELECT count(DISTINCT id) FROM alert_channels
WHERE workspace_id = $1 AND id = ANY($2)`, workspaceID, ids).Scan(&n); err != nil {
		return err
	}
	if n != len(uniqueStrings(ids)) {
		return fmt.Errorf("one or more channels do not exist in this workspace")
	}
	return nil
}

// uniqueStrings returns the distinct non-empty values of ids, preserving order.
func uniqueStrings(ids []string) []string {
	seen := make(map[string]struct{}, len(ids))
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}

// --- rules CRUD ---

// ListAlertRules returns a project's rules (member-readable).
func (s *Store) ListAlertRules(ctx context.Context, userID, projectID string) ([]AlertRule, error) {
	if _, err := s.ProjectByIDForUser(ctx, userID, projectID); err != nil {
		return nil, err
	}
	rows, err := s.pg.Query(ctx, `
SELECT id::text, project_id::text, name, source_kind, source_ref, condition, schedule_cron,
       channels, enabled, last_eval_at, last_state, created_at
FROM alert_rules WHERE project_id = $1 ORDER BY created_at DESC`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanAlertRules(rows)
}

func scanAlertRules(rows pgx.Rows) ([]AlertRule, error) {
	out := make([]AlertRule, 0)
	for rows.Next() {
		var r AlertRule
		var cond, chans []byte
		if err := rows.Scan(&r.ID, &r.ProjectID, &r.Name, &r.SourceKind, &r.SourceRef, &cond,
			&r.Schedule, &chans, &r.Enabled, &r.LastEvalAt, &r.LastState, &r.CreatedAt); err != nil {
			return nil, err
		}
		_ = json.Unmarshal(cond, &r.Condition)
		_ = json.Unmarshal(chans, &r.Channels)
		out = append(out, r)
	}
	return out, rows.Err()
}

// CreateAlertRule stores a new rule (owner/admin only).
func (s *Store) CreateAlertRule(ctx context.Context, userID, projectID string, r AlertRule) (AlertRule, error) {
	project, err := s.ProjectByIDForUser(ctx, userID, projectID)
	if err != nil {
		return AlertRule{}, err
	}
	canManage, err := s.userCanManageWorkspace(ctx, userID, project.WorkspaceID)
	if err != nil {
		return AlertRule{}, err
	}
	if !canManage {
		return AlertRule{}, errAgentForbidden
	}
	if err := validateAlertSource(r.SourceKind); err != nil {
		return AlertRule{}, err
	}
	if err := validateAlertOp(r.Condition.Op); err != nil {
		return AlertRule{}, err
	}
	if r.Condition.Op == "z_score" && r.Condition.MinEvents <= 0 {
		return AlertRule{}, fmt.Errorf("z_score alerts require min_events > 0 to avoid over-firing on low-volume metrics")
	}
	if err := s.validateChannelsInWorkspace(ctx, project.WorkspaceID, r.Channels); err != nil {
		return AlertRule{}, err
	}
	if r.Schedule == "" {
		r.Schedule = "*/5 * * * *"
	}
	cond, _ := json.Marshal(r.Condition)
	chans, _ := json.Marshal(r.Channels)
	var out AlertRule
	var oCond, oChans []byte
	err = s.pg.QueryRow(ctx, `
INSERT INTO alert_rules (project_id, name, source_kind, source_ref, condition, schedule_cron, channels, enabled)
VALUES ($1, $2, $3, $4, $5::jsonb, $6, $7::jsonb, $8)
RETURNING id::text, project_id::text, name, source_kind, source_ref, condition, schedule_cron, channels, enabled, last_eval_at, last_state, created_at`,
		projectID, r.Name, r.SourceKind, r.SourceRef, cond, r.Schedule, chans, r.Enabled).
		Scan(&out.ID, &out.ProjectID, &out.Name, &out.SourceKind, &out.SourceRef, &oCond, &out.Schedule,
			&oChans, &out.Enabled, &out.LastEvalAt, &out.LastState, &out.CreatedAt)
	if err != nil {
		return AlertRule{}, err
	}
	_ = json.Unmarshal(oCond, &out.Condition)
	_ = json.Unmarshal(oChans, &out.Channels)
	_ = s.recordWorkspaceAudit(ctx, project.WorkspaceID, userID, "alert.rule.create", "project", project.ID, project.Name, "{}")
	return out, nil
}

// UpdateAlertRule overwrites a rule's editable fields (owner/admin only).
func (s *Store) UpdateAlertRule(ctx context.Context, userID, projectID, ruleID string, r AlertRule) error {
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
	if err := validateAlertSource(r.SourceKind); err != nil {
		return err
	}
	if err := validateAlertOp(r.Condition.Op); err != nil {
		return err
	}
	if err := s.validateChannelsInWorkspace(ctx, project.WorkspaceID, r.Channels); err != nil {
		return err
	}
	cond, _ := json.Marshal(r.Condition)
	chans, _ := json.Marshal(r.Channels)
	_, err = s.pg.Exec(ctx, `
UPDATE alert_rules SET name=$3, source_kind=$4, source_ref=$5, condition=$6::jsonb,
	schedule_cron=$7, channels=$8::jsonb, enabled=$9
WHERE project_id=$1 AND id=$2`,
		projectID, ruleID, r.Name, r.SourceKind, r.SourceRef, cond, r.Schedule, chans, r.Enabled)
	return err
}

// DeleteAlertRule removes a rule (owner/admin only).
func (s *Store) DeleteAlertRule(ctx context.Context, userID, projectID, ruleID string) error {
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
	_, err = s.pg.Exec(ctx, `DELETE FROM alert_rules WHERE project_id=$1 AND id=$2`, projectID, ruleID)
	return err
}

// ListAlertEvents returns a rule's recent firing history (member-readable).
func (s *Store) ListAlertEvents(ctx context.Context, userID, projectID, ruleID string, limit int) ([]AlertEvent, error) {
	if _, err := s.ProjectByIDForUser(ctx, userID, projectID); err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.pg.Query(ctx, `
SELECT e.id::text, e.rule_id::text, e.fired_at, e.state, e.value, e.payload
FROM alert_events e JOIN alert_rules r ON r.id = e.rule_id
WHERE e.rule_id = $1 AND r.project_id = $2
ORDER BY e.fired_at DESC LIMIT $3`, ruleID, projectID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]AlertEvent, 0)
	for rows.Next() {
		var e AlertEvent
		if err := rows.Scan(&e.ID, &e.RuleID, &e.FiredAt, &e.State, &e.Value, &e.Payload); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// --- channels CRUD ---

// ListAlertChannels returns a workspace's channels (member-readable). The config
// is returned as stored (webhook URLs may be {{cred:NAME}} placeholders, not
// live secrets).
func (s *Store) ListAlertChannels(ctx context.Context, userID, workspaceID string) ([]AlertChannel, error) {
	member, err := s.userInWorkspace(ctx, userID, workspaceID)
	if err != nil {
		return nil, err
	}
	if !member {
		return nil, errAgentForbidden
	}
	rows, err := s.pg.Query(ctx, `
SELECT id::text, workspace_id::text, kind, name, config, created_at
FROM alert_channels WHERE workspace_id = $1 ORDER BY created_at DESC`, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]AlertChannel, 0)
	for rows.Next() {
		var ch AlertChannel
		if err := rows.Scan(&ch.ID, &ch.WorkspaceID, &ch.Kind, &ch.Name, &ch.Config, &ch.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, ch)
	}
	return out, rows.Err()
}

// CreateAlertChannel stores a delivery channel (owner/admin only).
func (s *Store) CreateAlertChannel(ctx context.Context, userID, workspaceID string, ch AlertChannel) (AlertChannel, error) {
	canManage, err := s.userCanManageWorkspace(ctx, userID, workspaceID)
	if err != nil {
		return AlertChannel{}, err
	}
	if !canManage {
		return AlertChannel{}, errAgentForbidden
	}
	switch ch.Kind {
	case "slack", "email", "webhook":
	default:
		return AlertChannel{}, fmt.Errorf("alert channel kind must be 'slack', 'email', or 'webhook'")
	}
	if len(ch.Config) == 0 {
		ch.Config = json.RawMessage(`{}`)
	}
	var out AlertChannel
	err = s.pg.QueryRow(ctx, `
INSERT INTO alert_channels (workspace_id, kind, name, config)
VALUES ($1, $2, $3, $4::jsonb)
RETURNING id::text, workspace_id::text, kind, name, config, created_at`,
		workspaceID, ch.Kind, ch.Name, []byte(ch.Config)).
		Scan(&out.ID, &out.WorkspaceID, &out.Kind, &out.Name, &out.Config, &out.CreatedAt)
	return out, err
}

// DeleteAlertChannel removes a channel (owner/admin only).
func (s *Store) DeleteAlertChannel(ctx context.Context, userID, workspaceID, channelID string) error {
	canManage, err := s.userCanManageWorkspace(ctx, userID, workspaceID)
	if err != nil {
		return err
	}
	if !canManage {
		return errAgentForbidden
	}
	_, err = s.pg.Exec(ctx, `DELETE FROM alert_channels WHERE workspace_id=$1 AND id=$2`, workspaceID, channelID)
	return err
}

// --- evaluation-support reads (internal, used by the alerting worker) ---

// ClaimAlertRuleForEval attempts to claim a rule for evaluation at now, returning
// true only if this caller won the claim. A conditional UPDATE on last_eval_at is
// a cheap optimistic lock so that if the service ever runs >1 replica two ticks
// don't both evaluate (the agent scheduler shares this single-writer assumption).
// due is the earliest last_eval_at that still permits a re-claim (now minus a
// small window), so a crashed evaluation is retried on the next tick.
func (s *Store) ClaimAlertRuleForEval(ctx context.Context, ruleID string, now, due time.Time) (bool, error) {
	tag, err := s.pg.Exec(ctx, `
UPDATE alert_rules SET last_eval_at = $2
WHERE id = $1 AND (last_eval_at IS NULL OR last_eval_at < $3)`, ruleID, now, due)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// DueAlertRules returns all enabled rules across projects with their schedule, for
// the worker to match against the current minute. Internal trusted path (no RBAC)
// — called only by the in-process evaluator.
func (s *Store) DueAlertRules(ctx context.Context) ([]AlertRule, error) {
	rows, err := s.pg.Query(ctx, `
SELECT id::text, project_id::text, name, source_kind, source_ref, condition, schedule_cron,
       channels, enabled, last_eval_at, last_state, created_at
FROM alert_rules WHERE enabled ORDER BY created_at ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanAlertRules(rows)
}

// RecordAlertEvent appends a firing/recovery event and updates the rule's edge
// state in one call (internal trusted path). state is 'firing' or 'ok'.
func (s *Store) RecordAlertEvent(ctx context.Context, ruleID, state string, value float64, payload json.RawMessage) error {
	if len(payload) == 0 {
		payload = json.RawMessage(`{}`)
	}
	if _, err := s.pg.Exec(ctx, `
INSERT INTO alert_events (rule_id, state, value, payload) VALUES ($1, $2, $3, $4::jsonb)`,
		ruleID, state, value, []byte(payload)); err != nil {
		return err
	}
	_, err := s.pg.Exec(ctx, `UPDATE alert_rules SET last_state = $2 WHERE id = $1`, ruleID, state)
	return err
}

// AlertChannelsByID resolves a set of channel ids to full channel rows (internal
// trusted path for delivery).
func (s *Store) AlertChannelsByID(ctx context.Context, ids []string) ([]AlertChannel, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := s.pg.Query(ctx, `
SELECT id::text, workspace_id::text, kind, name, config, created_at
FROM alert_channels WHERE id = ANY($1)`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]AlertChannel, 0)
	for rows.Next() {
		var ch AlertChannel
		if err := rows.Scan(&ch.ID, &ch.WorkspaceID, &ch.Kind, &ch.Name, &ch.Config, &ch.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, ch)
	}
	return out, rows.Err()
}

// WorkspaceChannelByName resolves a channel by human name within a workspace, for
// the send_notification agent tool (which addresses channels by name, not id).
func (s *Store) WorkspaceChannelByName(ctx context.Context, workspaceID, name string) (AlertChannel, error) {
	var ch AlertChannel
	err := s.pg.QueryRow(ctx, `
SELECT id::text, workspace_id::text, kind, name, config, created_at
FROM alert_channels WHERE workspace_id = $1 AND name = $2 LIMIT 1`, workspaceID, name).
		Scan(&ch.ID, &ch.WorkspaceID, &ch.Kind, &ch.Name, &ch.Config, &ch.CreatedAt)
	return ch, err
}
