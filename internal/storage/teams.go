package storage

import (
	"context"
	"errors"
	"strings"
	"time"
)

// Agent teams (ARCHITECT-AGENT-TEAM P2 + the P3 orchestrator-skill half). A
// team groups existing AgentGarden agents around a shared kanban board and
// selects one member as lead. The lead — and only the lead — receives the
// injected orchestrator skill and the team_board tool at run time
// (agentruntime), and may delegate cards to the other members through the
// already-shipped spawn_subagent(agent=…) path. Membership itself grants no
// tools: a member agent's capability surface is untouched by joining a team.
//
// Table names are frozen here per the parent plan: teams / team_members /
// team_cards, mirroring the agents / agent_delegates storage conventions
// (control-plane ops authorize through the project + userCanManageWorkspace;
// run-path readers are trusted and re-validate liveness via joins).

var errTeamLeadNotMember = errors.New("lead must be a team member")
var errTeamMemberNotInProject = errors.New("agent does not operate in this project")

// Exported sentinels so the HTTP layer can map permission denials to 403 and
// missing rows to 404 instead of collapsing everything into 400.
var ErrTeamForbidden = errors.New("team permission denied")
var ErrTeamNotFound = errors.New("team not found")
var ErrTeamCardNotFound = errors.New("card not found")

// teamCardStatuses is the kanban column set, in board order.
var teamCardStatuses = []string{"backlog", "doing", "review", "done"}

var errInvalidCardStatus = errors.New("invalid card status (" + strings.Join(teamCardStatuses, "|") + ")")

// validTeamCardStatus reports whether status is a legal kanban column. Pure and
// unit-testable; every write path (control plane and the lead's team_board
// tool) funnels through it so an invalid column can never be stored.
func validTeamCardStatus(status string) bool {
	for _, s := range teamCardStatuses {
		if s == status {
			return true
		}
	}
	return false
}

// TeamCardStatuses returns the kanban column names in board order (for the UI
// and the team_board tool's schema copy).
func TeamCardStatuses() []string {
	out := make([]string, len(teamCardStatuses))
	copy(out, teamCardStatuses)
	return out
}

// Team is one agent team within a project.
type Team struct {
	ID          string    `json:"id"`
	ProjectID   string    `json:"project_id"`
	Name        string    `json:"name"`
	Slug        string    `json:"slug"`
	LeadAgentID string    `json:"lead_agent_id"`
	MemberCount int       `json:"member_count"`
	CardCount   int       `json:"card_count"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// TeamMember is one roster row with the agent identity resolved for display.
type TeamMember struct {
	AgentID  string `json:"agent_id"`
	Name     string `json:"name"`
	Slug     string `json:"slug"`
	Role     string `json:"role"`
	Position int    `json:"position"`
	Enabled  bool   `json:"enabled"`
	IsLead   bool   `json:"is_lead"`
}

// TeamCard is one kanban work item.
type TeamCard struct {
	ID              string    `json:"id"`
	TeamID          string    `json:"team_id"`
	Status          string    `json:"status"`
	Title           string    `json:"title"`
	Body            string    `json:"body"`
	AssigneeAgentID string    `json:"assignee_agent_id"`
	AssigneeName    string    `json:"assignee_name"`
	Position        int       `json:"position"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

// migrateTeams creates the team schema (P2). Additive only; no existing table
// is touched, so a rollback is dropping the three tables.
func (s *Store) migrateTeams(ctx context.Context) error {
	stmts := []string{
		// lead_agent_id is nullable (a team may exist before a lead is picked) and
		// deliberately not an FK: agents granted in from the workspace are valid
		// leads, and the run path re-validates liveness via joins anyway (a deleted
		// or disabled lead simply stops resolving as one, fail-closed).
		`CREATE TABLE IF NOT EXISTS teams (
	id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
	project_id UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
	name VARCHAR(128) NOT NULL DEFAULT '',
	slug VARCHAR(64) NOT NULL DEFAULT '',
	lead_agent_id UUID,
	created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	UNIQUE (project_id, slug)
)`,
		`CREATE INDEX IF NOT EXISTS teams_project_idx ON teams (project_id)`,
		// The run path resolves "which teams does this agent lead" per run.
		`CREATE INDEX IF NOT EXISTS teams_lead_idx ON teams (lead_agent_id) WHERE lead_agent_id IS NOT NULL`,
		`CREATE TABLE IF NOT EXISTS team_members (
	team_id UUID NOT NULL REFERENCES teams(id) ON DELETE CASCADE,
	agent_id UUID NOT NULL,
	role VARCHAR(64) NOT NULL DEFAULT '',
	position INT NOT NULL DEFAULT 0,
	created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	PRIMARY KEY (team_id, agent_id)
)`,
		`CREATE TABLE IF NOT EXISTS team_cards (
	id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
	team_id UUID NOT NULL REFERENCES teams(id) ON DELETE CASCADE,
	status VARCHAR(16) NOT NULL DEFAULT 'backlog',
	title VARCHAR(256) NOT NULL DEFAULT '',
	body TEXT NOT NULL DEFAULT '',
	assignee_agent_id UUID,
	position INT NOT NULL DEFAULT 0,
	created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
)`,
		`CREATE INDEX IF NOT EXISTS team_cards_team_idx ON team_cards (team_id, status, position)`,
	}
	for _, stmt := range stmts {
		if _, err := s.pg.Exec(ctx, stmt); err != nil {
			return err
		}
	}
	return nil
}

// teamProject authorizes a team-scoped request: the user must be a member of
// the team's project. Returns the project (for workspace/audit) alongside the
// team row. A team id from another project resolves to not-found, never to a
// cross-tenant read.
func (s *Store) teamProject(ctx context.Context, userID, projectID, teamID string) (Project, Team, error) {
	project, err := s.ProjectByIDForUser(ctx, userID, projectID)
	if err != nil {
		return Project{}, Team{}, err
	}
	var t Team
	var lead *string
	err = s.pg.QueryRow(ctx, `
SELECT id::text, project_id::text, name, slug, lead_agent_id::text, created_at, updated_at
FROM teams WHERE id = $1 AND project_id = $2`, teamID, project.ID).
		Scan(&t.ID, &t.ProjectID, &t.Name, &t.Slug, &lead, &t.CreatedAt, &t.UpdatedAt)
	if err != nil {
		return Project{}, Team{}, ErrTeamNotFound
	}
	if lead != nil {
		t.LeadAgentID = *lead
	}
	return project, t, nil
}

// canManageTeam authorizes a structural team write (create/update/delete team,
// membership, lead pick) — owner/admin only, because lead selection changes an
// agent's effective delegation surface. Card writes stay member-level.
func (s *Store) canManageTeam(ctx context.Context, userID string, project Project) error {
	canManage, err := s.userCanManageWorkspace(ctx, userID, project.WorkspaceID)
	if err != nil {
		return err
	}
	if !canManage {
		return ErrTeamForbidden
	}
	return nil
}

// ListTeams returns a project's teams (member counts resolved) for any member.
func (s *Store) ListTeams(ctx context.Context, userID, projectID string) ([]Team, error) {
	project, err := s.ProjectByIDForUser(ctx, userID, projectID)
	if err != nil {
		return nil, err
	}
	rows, err := s.pg.Query(ctx, `
SELECT t.id::text, t.project_id::text, t.name, t.slug, t.lead_agent_id::text,
       (SELECT count(*) FROM team_members m WHERE m.team_id = t.id),
       (SELECT count(*) FROM team_cards c WHERE c.team_id = t.id),
       t.created_at, t.updated_at
FROM teams t WHERE t.project_id = $1
ORDER BY t.name ASC`, project.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Team, 0)
	for rows.Next() {
		var t Team
		var lead *string
		if err := rows.Scan(&t.ID, &t.ProjectID, &t.Name, &t.Slug, &lead, &t.MemberCount, &t.CardCount, &t.CreatedAt, &t.UpdatedAt); err != nil {
			return nil, err
		}
		if lead != nil {
			t.LeadAgentID = *lead
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// CreateTeam adds a team to a project (owner/admin only). The slug is derived
// from the name (same rule as agents); uniqueness within the project is
// enforced by the table constraint.
func (s *Store) CreateTeam(ctx context.Context, userID, projectID, name string) (Team, error) {
	project, err := s.ProjectByIDForUser(ctx, userID, projectID)
	if err != nil {
		return Team{}, err
	}
	if err := s.canManageTeam(ctx, userID, project); err != nil {
		return Team{}, err
	}
	name = strings.TrimSpace(name)
	slug := slugify(name)
	if !validAgentSlug(slug) {
		return Team{}, errInvalidAgentSlug
	}
	var t Team
	err = s.pg.QueryRow(ctx, `
INSERT INTO teams (project_id, name, slug)
VALUES ($1, $2, $3)
RETURNING id::text, project_id::text, name, slug, created_at, updated_at`,
		project.ID, name, slug).
		Scan(&t.ID, &t.ProjectID, &t.Name, &t.Slug, &t.CreatedAt, &t.UpdatedAt)
	if err != nil {
		return Team{}, err
	}
	_ = s.recordWorkspaceAudit(ctx, project.WorkspaceID, userID, "team.create", "project", project.ID, project.Name, "{}")
	return t, nil
}

// UpdateTeam renames a team and/or picks its lead (owner/admin only). A nil
// leadAgentID keeps the current lead (partial update, same semantics as an
// empty name); an explicit empty string clears it; a non-empty lead must be a
// current member — the lead is what receives the orchestrator skill and board
// tool at run time, so it cannot point outside the roster.
func (s *Store) UpdateTeam(ctx context.Context, userID, projectID, teamID, name string, leadAgentID *string) (Team, error) {
	project, team, err := s.teamProject(ctx, userID, projectID, teamID)
	if err != nil {
		return Team{}, err
	}
	if err := s.canManageTeam(ctx, userID, project); err != nil {
		return Team{}, err
	}
	if name = strings.TrimSpace(name); name == "" {
		name = team.Name
	}
	var lead *string
	if leadAgentID == nil {
		if team.LeadAgentID != "" {
			keep := team.LeadAgentID
			lead = &keep
		}
	} else if next := strings.TrimSpace(*leadAgentID); next != "" {
		var isMember bool
		if err := s.pg.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM team_members WHERE team_id = $1 AND agent_id = $2)`,
			team.ID, next).Scan(&isMember); err != nil {
			return Team{}, err
		}
		if !isMember {
			return Team{}, errTeamLeadNotMember
		}
		lead = &next
	}
	var t Team
	var outLead *string
	err = s.pg.QueryRow(ctx, `
UPDATE teams SET name = $3, lead_agent_id = $4, updated_at = now()
WHERE id = $1 AND project_id = $2
RETURNING id::text, project_id::text, name, slug, lead_agent_id::text, created_at, updated_at`,
		team.ID, project.ID, name, lead).
		Scan(&t.ID, &t.ProjectID, &t.Name, &t.Slug, &outLead, &t.CreatedAt, &t.UpdatedAt)
	if err != nil {
		return Team{}, err
	}
	if outLead != nil {
		t.LeadAgentID = *outLead
	}
	_ = s.recordWorkspaceAudit(ctx, project.WorkspaceID, userID, "team.update", "project", project.ID, project.Name, "{}")
	return t, nil
}

// DeleteTeam removes a team, its roster, and its cards (owner/admin only; the
// child tables cascade).
func (s *Store) DeleteTeam(ctx context.Context, userID, projectID, teamID string) error {
	project, team, err := s.teamProject(ctx, userID, projectID, teamID)
	if err != nil {
		return err
	}
	if err := s.canManageTeam(ctx, userID, project); err != nil {
		return err
	}
	if _, err := s.pg.Exec(ctx, `DELETE FROM teams WHERE id = $1 AND project_id = $2`, team.ID, project.ID); err != nil {
		return err
	}
	_ = s.recordWorkspaceAudit(ctx, project.WorkspaceID, userID, "team.delete", "project", project.ID, project.Name, "{}")
	return nil
}

// GetTeam returns one team with its resolved roster for any project member.
func (s *Store) GetTeam(ctx context.Context, userID, projectID, teamID string) (Team, []TeamMember, error) {
	_, team, err := s.teamProject(ctx, userID, projectID, teamID)
	if err != nil {
		return Team{}, nil, err
	}
	rows, err := s.pg.Query(ctx, `
SELECT m.agent_id::text, a.name, a.slug, m.role, m.position, a.enabled
FROM team_members m
JOIN agents a ON a.id = m.agent_id
WHERE m.team_id = $1
ORDER BY m.position ASC, a.name ASC`, team.ID)
	if err != nil {
		return Team{}, nil, err
	}
	defer rows.Close()
	members := make([]TeamMember, 0)
	for rows.Next() {
		var m TeamMember
		if err := rows.Scan(&m.AgentID, &m.Name, &m.Slug, &m.Role, &m.Position, &m.Enabled); err != nil {
			return Team{}, nil, err
		}
		m.IsLead = m.AgentID == team.LeadAgentID
		members = append(members, m)
	}
	return team, members, rows.Err()
}

// UpsertTeamMember adds or updates one roster row (owner/admin only). The
// agent must operate in the team's project — same rule as a delegation grant.
// Nil role/position mean "keep the current value" on an existing row (partial
// update); a new row defaults to no role and the end of the roster.
func (s *Store) UpsertTeamMember(ctx context.Context, userID, projectID, teamID, agentID string, role *string, position *int) error {
	project, team, err := s.teamProject(ctx, userID, projectID, teamID)
	if err != nil {
		return err
	}
	if err := s.canManageTeam(ctx, userID, project); err != nil {
		return err
	}
	ok, err := s.agentInProject(ctx, project.ID, agentID)
	if err != nil {
		return err
	}
	if !ok && !isDefaultAgent(project.ID, agentID) {
		return errTeamMemberNotInProject
	}
	if role != nil {
		trimmed := strings.TrimSpace(*role)
		role = &trimmed
	}
	_, err = s.pg.Exec(ctx, `
INSERT INTO team_members (team_id, agent_id, role, position)
VALUES ($1, $2, COALESCE($3::text, ''),
	COALESCE($4::int, (SELECT COALESCE(MAX(position), 0) + 1 FROM team_members m WHERE m.team_id = $1)))
ON CONFLICT (team_id, agent_id) DO UPDATE SET
	role = COALESCE($3::text, team_members.role),
	position = COALESCE($4::int, team_members.position)`,
		team.ID, agentID, role, position)
	if err != nil {
		return err
	}
	_ = s.recordWorkspaceAudit(ctx, project.WorkspaceID, userID, "team.member.update", "project", project.ID, project.Name, "{}")
	return nil
}

// RemoveTeamMember drops one roster row (owner/admin only). Removing the
// current lead also clears the team's lead — a team never points at a lead
// outside its roster, and the ex-lead's orchestrator injection stops on its
// next run. The ex-member's cards go back to unassigned for the same reason:
// the board never references an agent outside the roster.
func (s *Store) RemoveTeamMember(ctx context.Context, userID, projectID, teamID, agentID string) error {
	project, team, err := s.teamProject(ctx, userID, projectID, teamID)
	if err != nil {
		return err
	}
	if err := s.canManageTeam(ctx, userID, project); err != nil {
		return err
	}
	if _, err := s.pg.Exec(ctx, `DELETE FROM team_members WHERE team_id = $1 AND agent_id = $2`, team.ID, agentID); err != nil {
		return err
	}
	if _, err := s.pg.Exec(ctx, `UPDATE teams SET lead_agent_id = NULL, updated_at = now() WHERE id = $1 AND lead_agent_id = $2`, team.ID, agentID); err != nil {
		return err
	}
	if _, err := s.pg.Exec(ctx, `UPDATE team_cards SET assignee_agent_id = NULL, updated_at = now() WHERE team_id = $1 AND assignee_agent_id = $2`, team.ID, agentID); err != nil {
		return err
	}
	_ = s.recordWorkspaceAudit(ctx, project.WorkspaceID, userID, "team.member.delete", "project", project.ID, project.Name, "{}")
	return nil
}

// ListTeamCards returns a team's board (assignee names resolved) for any
// project member, in column/position order.
func (s *Store) ListTeamCards(ctx context.Context, userID, projectID, teamID string) ([]TeamCard, error) {
	_, team, err := s.teamProject(ctx, userID, projectID, teamID)
	if err != nil {
		return nil, err
	}
	return s.teamCards(ctx, team.ID)
}

// teamCards is the shared board read (control plane and the lead's tool).
func (s *Store) teamCards(ctx context.Context, teamID string) ([]TeamCard, error) {
	rows, err := s.pg.Query(ctx, `
SELECT c.id::text, c.team_id::text, c.status, c.title, c.body,
       c.assignee_agent_id::text, COALESCE(a.name, ''), c.position, c.created_at, c.updated_at
FROM team_cards c
LEFT JOIN agents a ON a.id = c.assignee_agent_id
WHERE c.team_id = $1
ORDER BY c.status ASC, c.position ASC, c.created_at ASC`, teamID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]TeamCard, 0)
	for rows.Next() {
		var c TeamCard
		var assignee *string
		if err := rows.Scan(&c.ID, &c.TeamID, &c.Status, &c.Title, &c.Body, &assignee, &c.AssigneeName, &c.Position, &c.CreatedAt, &c.UpdatedAt); err != nil {
			return nil, err
		}
		if assignee != nil {
			c.AssigneeAgentID = *assignee
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// CreateTeamCard adds a backlog card (any project member — cards are work
// items, not capability changes).
func (s *Store) CreateTeamCard(ctx context.Context, userID, projectID, teamID, title, body string) (TeamCard, error) {
	_, team, err := s.teamProject(ctx, userID, projectID, teamID)
	if err != nil {
		return TeamCard{}, err
	}
	return s.createTeamCard(ctx, team.ID, title, body)
}

// createTeamCard is the shared insert (control plane and the lead's tool).
// New cards land at the end of the backlog column.
func (s *Store) createTeamCard(ctx context.Context, teamID, title, body string) (TeamCard, error) {
	title = strings.TrimSpace(title)
	if title == "" {
		return TeamCard{}, errors.New("card title is required")
	}
	var c TeamCard
	err := s.pg.QueryRow(ctx, `
INSERT INTO team_cards (team_id, status, title, body, position)
VALUES ($1, 'backlog', $2, $3,
	(SELECT COALESCE(MAX(position), 0) + 1 FROM team_cards WHERE team_id = $1 AND status = 'backlog'))
RETURNING id::text, team_id::text, status, title, body, position, created_at, updated_at`,
		teamID, title, body).
		Scan(&c.ID, &c.TeamID, &c.Status, &c.Title, &c.Body, &c.Position, &c.CreatedAt, &c.UpdatedAt)
	return c, err
}

// TeamCardUpdate is the mutable slice of a card. Nil fields are left unchanged.
type TeamCardUpdate struct {
	Status          *string `json:"status"`
	Title           *string `json:"title"`
	Body            *string `json:"body"`
	AssigneeAgentID *string `json:"assignee_agent_id"` // "" clears the assignee
}

// UpdateTeamCard moves/edits a card (any project member). Status must be a
// legal column; a non-empty assignee must be on the team's roster.
func (s *Store) UpdateTeamCard(ctx context.Context, userID, projectID, teamID, cardID string, in TeamCardUpdate) (TeamCard, error) {
	_, team, err := s.teamProject(ctx, userID, projectID, teamID)
	if err != nil {
		return TeamCard{}, err
	}
	return s.updateTeamCard(ctx, team.ID, cardID, in)
}

// updateTeamCard is the shared card write (control plane and the lead's tool).
// The team id is always required alongside the card id so a card can never be
// reached through a foreign team.
func (s *Store) updateTeamCard(ctx context.Context, teamID, cardID string, in TeamCardUpdate) (TeamCard, error) {
	if in.Status != nil && !validTeamCardStatus(*in.Status) {
		return TeamCard{}, errInvalidCardStatus
	}
	if in.Title != nil && strings.TrimSpace(*in.Title) == "" {
		return TeamCard{}, errors.New("card title is required")
	}
	if in.AssigneeAgentID != nil && strings.TrimSpace(*in.AssigneeAgentID) != "" {
		var isMember bool
		if err := s.pg.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM team_members WHERE team_id = $1 AND agent_id = $2)`,
			teamID, strings.TrimSpace(*in.AssigneeAgentID)).Scan(&isMember); err != nil {
			return TeamCard{}, err
		}
		if !isMember {
			return TeamCard{}, errTeamMemberNotInProject
		}
	}
	var assignee any
	if in.AssigneeAgentID != nil {
		if v := strings.TrimSpace(*in.AssigneeAgentID); v != "" {
			assignee = v
		}
	}
	var c TeamCard
	var outAssignee *string
	err := s.pg.QueryRow(ctx, `
UPDATE team_cards SET
	status = COALESCE($3, status),
	title = COALESCE(NULLIF(TRIM($4::text), ''), title),
	body = COALESCE($5, body),
	assignee_agent_id = CASE WHEN $6 THEN $7::uuid ELSE assignee_agent_id END,
	updated_at = now()
WHERE id = $2 AND team_id = $1
RETURNING id::text, team_id::text, status, title, body, assignee_agent_id::text, position, created_at, updated_at`,
		teamID, cardID, in.Status, in.Title, in.Body, in.AssigneeAgentID != nil, assignee).
		Scan(&c.ID, &c.TeamID, &c.Status, &c.Title, &c.Body, &outAssignee, &c.Position, &c.CreatedAt, &c.UpdatedAt)
	if err != nil {
		return TeamCard{}, ErrTeamCardNotFound
	}
	if outAssignee != nil {
		c.AssigneeAgentID = *outAssignee
	}
	return c, nil
}

// DeleteTeamCard removes a card (any project member).
func (s *Store) DeleteTeamCard(ctx context.Context, userID, projectID, teamID, cardID string) error {
	_, team, err := s.teamProject(ctx, userID, projectID, teamID)
	if err != nil {
		return err
	}
	_, err = s.pg.Exec(ctx, `DELETE FROM team_cards WHERE id = $1 AND team_id = $2`, cardID, team.ID)
	return err
}

// TeamLeadership is one team an agent currently leads, resolved for the run
// path: the team identity plus the member roster (lead excluded) in the same
// shape the delegate roster uses, so members merge into spawn_subagent's
// teammate list.
type TeamLeadership struct {
	TeamID  string
	Name    string
	Slug    string
	Members []AgentDelegateTarget
}

// TeamsLedByAgentForRun returns the teams this agent leads with their live
// member rosters (internal trusted path, mirrors AgentDelegatesForRun). The
// joins enforce that members still exist and are enabled, so a deleted or
// disabled member drops off the roster fail-closed. The lead itself is
// excluded from the roster — self-delegation is spawn_subagent's built-in
// self-fork. An agent that leads no team returns an empty slice, which leaves
// its run path byte-for-byte unchanged.
func (s *Store) TeamsLedByAgentForRun(ctx context.Context, scopeID string) ([]TeamLeadership, error) {
	rows, err := s.pg.Query(ctx, `
SELECT t.id::text, t.name, t.slug
FROM teams t
WHERE t.lead_agent_id = $1
ORDER BY t.name ASC`, scopeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	teams := make([]TeamLeadership, 0)
	for rows.Next() {
		var t TeamLeadership
		if err := rows.Scan(&t.TeamID, &t.Name, &t.Slug); err != nil {
			return nil, err
		}
		teams = append(teams, t)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range teams {
		members, err := s.teamMembersForRun(ctx, teams[i].TeamID, scopeID)
		if err != nil {
			return nil, err
		}
		teams[i].Members = members
	}
	return teams, nil
}

// teamMembersForRun resolves one team's live members (excluding the lead) with
// the same persona hint the delegate roster carries.
func (s *Store) teamMembersForRun(ctx context.Context, teamID, leadAgentID string) ([]AgentDelegateTarget, error) {
	rows, err := s.pg.Query(ctx, `
SELECT m.agent_id::text, a.name, a.slug,
       COALESCE(split_part(def.soul_md, E'\n', 1), '')
FROM team_members m
JOIN agents a ON a.id = m.agent_id AND a.enabled
LEFT JOIN agent_definitions def ON def.scope_id = a.id
WHERE m.team_id = $1 AND m.agent_id <> $2
ORDER BY m.position ASC, a.name ASC`, teamID, leadAgentID)
	if err != nil {
		return nil, err
	}
	return scanDelegateTargets(rows)
}

// TeamCardsForRun returns a led team's board for the lead's team_board tool
// (internal trusted path — the runner only builds the tool over teams this
// agent leads).
func (s *Store) TeamCardsForRun(ctx context.Context, teamID string) ([]TeamCard, error) {
	return s.teamCards(ctx, teamID)
}

// CreateTeamCardForRun adds a backlog card from the lead's team_board tool.
func (s *Store) CreateTeamCardForRun(ctx context.Context, teamID, title, body string) (TeamCard, error) {
	return s.createTeamCard(ctx, teamID, title, body)
}

// UpdateTeamCardForRun moves/edits a card from the lead's team_board tool.
func (s *Store) UpdateTeamCardForRun(ctx context.Context, teamID, cardID string, in TeamCardUpdate) (TeamCard, error) {
	return s.updateTeamCard(ctx, teamID, cardID, in)
}
