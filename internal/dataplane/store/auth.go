package storage

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"
)

type User struct {
	ID        string    `json:"id"`
	Email     string    `json:"email"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type Workspace struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Role string `json:"role"`
	// Plan is display-only (see workspace_plan.go): it drives the plan badge,
	// the usage meter's ceiling, and the upgrade moment. Nothing enforces it.
	Plan string `json:"plan"`
	// IsDemo marks the ONE shared demo workspace (demo.go). The caller's Role in
	// it is 'viewer'; together they let the UI say "this is a live demo of
	// someone else's site, you are reading it" instead of presenting it as the
	// user's own workspace.
	IsDemo    bool      `json:"is_demo,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type WorkspaceMember struct {
	WorkspaceID string    `json:"workspace_id"`
	UserID      string    `json:"user_id"`
	Email       string    `json:"email"`
	Name        string    `json:"name"`
	Role        string    `json:"role"`
	CreatedAt   time.Time `json:"created_at"`
}

type WorkspaceUsage struct {
	WorkspaceID   string    `json:"workspace_id"`
	ProjectCount  uint64    `json:"project_count"`
	EventCount    uint64    `json:"event_count"`
	DistinctUsers uint64    `json:"distinct_users"`
	GeneratedAt   time.Time `json:"generated_at"`
}

type WorkspaceAuditLog struct {
	ID          string    `json:"id"`
	WorkspaceID string    `json:"workspace_id"`
	ActorID     string    `json:"actor_id"`
	ActorEmail  string    `json:"actor_email"`
	Action      string    `json:"action"`
	TargetType  string    `json:"target_type"`
	TargetID    string    `json:"target_id"`
	TargetLabel string    `json:"target_label"`
	Metadata    string    `json:"metadata"`
	CreatedAt   time.Time `json:"created_at"`
}

type UserSession struct {
	ID        string    `json:"id"`
	UserID    string    `json:"user_id"`
	ExpiresAt time.Time `json:"expires_at"`
	CreatedAt time.Time `json:"created_at"`
}

type AccountBootstrap struct {
	User      User      `json:"user"`
	Workspace Workspace `json:"workspace"`
	Project   Project   `json:"project"`
	Projects  []Project `json:"projects,omitempty"`
}

func (s *Store) CreateAccount(ctx context.Context, email string, name string, password string, workspaceName string, projectName string) (AccountBootstrap, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	name = strings.TrimSpace(name)
	workspaceName = strings.TrimSpace(workspaceName)
	projectName = strings.TrimSpace(projectName)
	if email == "" || !strings.Contains(email, "@") {
		return AccountBootstrap{}, fmt.Errorf("email is required")
	}
	if len(password) < 8 {
		return AccountBootstrap{}, fmt.Errorf("password must be at least 8 characters")
	}
	if name == "" {
		name = email
	}
	if workspaceName == "" {
		workspaceName = "My workspace"
	}
	if projectName == "" {
		projectName = "Default project"
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return AccountBootstrap{}, err
	}

	tx, err := s.pg.Begin(ctx)
	if err != nil {
		return AccountBootstrap{}, err
	}
	defer tx.Rollback(ctx)

	var out AccountBootstrap
	if err := tx.QueryRow(ctx, `
INSERT INTO users (email, name, password_hash)
VALUES ($1, $2, $3)
RETURNING id::text, email, name, created_at, updated_at`, email, name, string(hash)).
		Scan(&out.User.ID, &out.User.Email, &out.User.Name, &out.User.CreatedAt, &out.User.UpdatedAt); err != nil {
		return AccountBootstrap{}, err
	}
	if err := tx.QueryRow(ctx, `
INSERT INTO workspaces (name, created_by)
VALUES ($1, $2)
RETURNING id::text, name, 'owner', COALESCE(plan, 'free'), created_at, updated_at`, workspaceName, out.User.ID).
		Scan(&out.Workspace.ID, &out.Workspace.Name, &out.Workspace.Role, &out.Workspace.Plan, &out.Workspace.CreatedAt, &out.Workspace.UpdatedAt); err != nil {
		return AccountBootstrap{}, err
	}
	if _, err := tx.Exec(ctx, `
INSERT INTO workspace_members (workspace_id, user_id, role)
VALUES ($1, $2, 'owner')`, out.Workspace.ID, out.User.ID); err != nil {
		return AccountBootstrap{}, err
	}
	// Exactly one project, and it is theirs. This used to also insert a project
	// named "Demo" full of synthetic events — invented numbers sitting in the
	// owner's own workspace, indistinguishable from data they had collected. The
	// demo is now one real shared project they join as a viewer (see demo.go).
	own := Project{Role: "owner"}
	key := "agentray_" + uuid.NewString()
	if err := tx.QueryRow(ctx, `
INSERT INTO projects (workspace_id, owner_id, name, api_key)
VALUES ($1, $2, $3, $4)
RETURNING id::text, workspace_id::text, name, api_key, created_at`, out.Workspace.ID, out.User.ID, projectName, key).
		Scan(&own.ID, &own.WorkspaceID, &own.Name, &own.APIKey, &own.CreatedAt); err != nil {
		return AccountBootstrap{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return AccountBootstrap{}, err
	}
	if err := s.SeedProjectFromTemplate(ctx, own.ID); err != nil {
		return AccountBootstrap{}, err
	}
	// Read-only membership in the shared demo, if this instance has one. Same
	// best-effort contract the synthetic seed had: the account is already
	// committed, and an account without the demo still works — the next boot's
	// backfill grants it.
	if err := s.addDemoViewer(ctx, out.User.ID); err != nil {
		fmt.Printf("warn: addDemoViewer(%s): %v\n", out.User.ID, err)
	}
	out.Project = own
	out.Projects = []Project{own}
	_ = s.recordWorkspaceAudit(ctx, out.Workspace.ID, out.User.ID, "workspace.created", "workspace", out.Workspace.ID, out.Workspace.Name, fmt.Sprintf(`{"project_id":%q}`, own.ID))
	return out, nil
}

func (s *Store) AuthenticateUser(ctx context.Context, email string, password string) (User, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	var user User
	var passwordHash string
	err := s.pg.QueryRow(ctx, `
SELECT id::text, email, name, password_hash, created_at, updated_at
FROM users
WHERE email = $1`, email).
		Scan(&user.ID, &user.Email, &user.Name, &passwordHash, &user.CreatedAt, &user.UpdatedAt)
	if err != nil {
		return User{}, err
	}
	if err := bcrypt.CompareHashAndPassword([]byte(passwordHash), []byte(password)); err != nil {
		return User{}, fmt.Errorf("invalid credentials")
	}
	return user, nil
}

func (s *Store) CreateUserSession(ctx context.Context, userID string, ttl time.Duration) (UserSession, string, error) {
	token, err := randomToken()
	if err != nil {
		return UserSession{}, "", err
	}
	expiresAt := time.Now().UTC().Add(ttl)
	var session UserSession
	err = s.pg.QueryRow(ctx, `
INSERT INTO user_sessions (user_id, token_hash, expires_at)
VALUES ($1, $2, $3)
RETURNING id::text, user_id::text, expires_at, created_at`, userID, hashToken(token), expiresAt).
		Scan(&session.ID, &session.UserID, &session.ExpiresAt, &session.CreatedAt)
	return session, token, err
}

func (s *Store) UserBySessionToken(ctx context.Context, token string) (User, UserSession, error) {
	if token == "" {
		return User{}, UserSession{}, fmt.Errorf("missing session")
	}
	var user User
	var session UserSession
	err := s.pg.QueryRow(ctx, `
SELECT
	u.id::text, u.email, u.name, u.created_at, u.updated_at,
	s.id::text, s.user_id::text, s.expires_at, s.created_at
FROM user_sessions s
JOIN users u ON u.id = s.user_id
WHERE s.token_hash = $1 AND s.expires_at > now()`, hashToken(token)).
		Scan(
			&user.ID, &user.Email, &user.Name, &user.CreatedAt, &user.UpdatedAt,
			&session.ID, &session.UserID, &session.ExpiresAt, &session.CreatedAt,
		)
	return user, session, err
}

func (s *Store) DeleteUserSession(ctx context.Context, token string) error {
	if token == "" {
		return nil
	}
	_, err := s.pg.Exec(ctx, `DELETE FROM user_sessions WHERE token_hash = $1`, hashToken(token))
	return err
}

func (s *Store) UpdateUser(ctx context.Context, userID string, name string) (User, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return User{}, fmt.Errorf("name is required")
	}
	var user User
	err := s.pg.QueryRow(ctx, `
UPDATE users
SET name = $2, updated_at = now()
WHERE id = $1
RETURNING id::text, email, name, created_at, updated_at`, userID, name).
		Scan(&user.ID, &user.Email, &user.Name, &user.CreatedAt, &user.UpdatedAt)
	return user, err
}

func (s *Store) ListUserWorkspaces(ctx context.Context, userID string) ([]Workspace, error) {
	// The demo workspace sorts LAST regardless of age. Everyone is a member of
	// it, it predates every account, and workspaces[0] is what the app opens on —
	// so plain created_at ASC would land every new signup inside someone else's
	// site instead of their own workspace.
	rows, err := s.pg.Query(ctx, `
SELECT w.id::text, w.name, wm.role, COALESCE(w.plan, 'free'), w.created_at, w.updated_at
FROM workspaces w
JOIN workspace_members wm ON wm.workspace_id = w.id
WHERE wm.user_id = $1
ORDER BY (w.id = NULLIF($2, '')::uuid) ASC, w.created_at ASC`, userID, s.demoWorkspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	workspaces := []Workspace{}
	for rows.Next() {
		var workspace Workspace
		if err := rows.Scan(&workspace.ID, &workspace.Name, &workspace.Role, &workspace.Plan, &workspace.CreatedAt, &workspace.UpdatedAt); err != nil {
			return nil, err
		}
		workspace.Plan = NormalizePlan(workspace.Plan)
		workspace.IsDemo = s.isDemoWorkspace(workspace.ID)
		workspaces = append(workspaces, workspace)
	}
	return workspaces, rows.Err()
}

func (s *Store) ListWorkspaceMembers(ctx context.Context, userID string, workspaceID string) ([]WorkspaceMember, error) {
	if ok, err := s.userCanAccessWorkspace(ctx, userID, workspaceID); err != nil || !ok {
		if err != nil {
			return nil, err
		}
		return nil, sql.ErrNoRows
	}
	rows, err := s.pg.Query(ctx, `
SELECT wm.workspace_id::text, u.id::text, u.email, u.name, wm.role, wm.created_at
FROM workspace_members wm
JOIN users u ON u.id = wm.user_id
WHERE wm.workspace_id = $1
ORDER BY
	`+workspaceRoleOrderSQL("wm.role")+`,
	wm.created_at ASC`, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	members := []WorkspaceMember{}
	for rows.Next() {
		var member WorkspaceMember
		if err := rows.Scan(&member.WorkspaceID, &member.UserID, &member.Email, &member.Name, &member.Role, &member.CreatedAt); err != nil {
			return nil, err
		}
		members = append(members, member)
	}
	return members, rows.Err()
}

func (s *Store) CreateWorkspace(ctx context.Context, userID string, name string) (Workspace, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		name = "Untitled workspace"
	}
	tx, err := s.pg.Begin(ctx)
	if err != nil {
		return Workspace{}, err
	}
	defer tx.Rollback(ctx)
	var workspace Workspace
	if err := tx.QueryRow(ctx, `
INSERT INTO workspaces (name, created_by)
VALUES ($1, $2)
RETURNING id::text, name, 'owner', COALESCE(plan, 'free'), created_at, updated_at`, name, userID).
		Scan(&workspace.ID, &workspace.Name, &workspace.Role, &workspace.Plan, &workspace.CreatedAt, &workspace.UpdatedAt); err != nil {
		return Workspace{}, err
	}
	if _, err := tx.Exec(ctx, `
INSERT INTO workspace_members (workspace_id, user_id, role)
VALUES ($1, $2, 'owner')`, workspace.ID, userID); err != nil {
		return Workspace{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Workspace{}, err
	}
	_ = s.recordWorkspaceAudit(ctx, workspace.ID, userID, "workspace.created", "workspace", workspace.ID, workspace.Name, "{}")
	return workspace, nil
}

func (s *Store) AddWorkspaceMemberByEmail(ctx context.Context, actorID string, workspaceID string, email string, role string) (WorkspaceMember, error) {
	if ok, err := s.userCanManageWorkspace(ctx, actorID, workspaceID); err != nil || !ok {
		if err != nil {
			return WorkspaceMember{}, err
		}
		return WorkspaceMember{}, sql.ErrNoRows
	}
	email = strings.ToLower(strings.TrimSpace(email))
	role = normalizeWorkspaceRole(role)
	if email == "" || !strings.Contains(email, "@") {
		return WorkspaceMember{}, fmt.Errorf("email is required")
	}
	var member WorkspaceMember
	err := s.pg.QueryRow(ctx, `
WITH target_user AS (
	SELECT id, email, name FROM users WHERE email = $2
), upserted AS (
	INSERT INTO workspace_members (workspace_id, user_id, role)
	SELECT $1, id, $3 FROM target_user
	ON CONFLICT (workspace_id, user_id) DO UPDATE
	SET role = EXCLUDED.role
	RETURNING workspace_id, user_id, role, created_at
)
SELECT upserted.workspace_id::text, target_user.id::text, target_user.email, target_user.name, upserted.role, upserted.created_at
FROM upserted
JOIN target_user ON target_user.id = upserted.user_id`, workspaceID, email, role).
		Scan(&member.WorkspaceID, &member.UserID, &member.Email, &member.Name, &member.Role, &member.CreatedAt)
	if err == nil {
		_ = s.recordWorkspaceAudit(ctx, workspaceID, actorID, "member.upserted", "user", member.UserID, member.Email, fmt.Sprintf(`{"role":%q}`, member.Role))
	}
	return member, err
}

func (s *Store) UpdateWorkspace(ctx context.Context, userID string, workspaceID string, name string) (Workspace, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		name = "Untitled workspace"
	}
	var workspace Workspace
	err := s.pg.QueryRow(ctx, `
UPDATE workspaces w
SET name = $3, updated_at = now()
FROM workspace_members wm
WHERE w.id = $2 AND wm.workspace_id = w.id AND wm.user_id = $1
	AND wm.role IN ('owner', 'admin')
RETURNING w.id::text, w.name, wm.role, COALESCE(w.plan, 'free'), w.created_at, w.updated_at`, userID, workspaceID, name).
		Scan(&workspace.ID, &workspace.Name, &workspace.Role, &workspace.Plan, &workspace.CreatedAt, &workspace.UpdatedAt)
	workspace.Plan = NormalizePlan(workspace.Plan)
	workspace.IsDemo = s.isDemoWorkspace(workspace.ID)
	if err == nil {
		_ = s.recordWorkspaceAudit(ctx, workspaceID, userID, "workspace.renamed", "workspace", workspaceID, workspace.Name, "{}")
	}
	return workspace, err
}

func (s *Store) UpdateWorkspaceMemberRole(ctx context.Context, actorID string, workspaceID string, memberUserID string, role string) (WorkspaceMember, error) {
	if ok, err := s.userCanManageWorkspace(ctx, actorID, workspaceID); err != nil || !ok {
		if err != nil {
			return WorkspaceMember{}, err
		}
		return WorkspaceMember{}, sql.ErrNoRows
	}
	role = normalizeWorkspaceRole(role)
	if role != "owner" {
		if err := s.ensureNotLastOwner(ctx, workspaceID, memberUserID); err != nil {
			return WorkspaceMember{}, err
		}
	}
	var member WorkspaceMember
	err := s.pg.QueryRow(ctx, `
UPDATE workspace_members wm
SET role = $3
FROM users u
WHERE wm.workspace_id = $1 AND wm.user_id = $2 AND u.id = wm.user_id
RETURNING wm.workspace_id::text, u.id::text, u.email, u.name, wm.role, wm.created_at`, workspaceID, memberUserID, role).
		Scan(&member.WorkspaceID, &member.UserID, &member.Email, &member.Name, &member.Role, &member.CreatedAt)
	if err == nil {
		_ = s.recordWorkspaceAudit(ctx, workspaceID, actorID, "member.role_updated", "user", member.UserID, member.Email, fmt.Sprintf(`{"role":%q}`, member.Role))
	}
	return member, err
}

func (s *Store) RemoveWorkspaceMember(ctx context.Context, actorID string, workspaceID string, memberUserID string) error {
	if ok, err := s.userCanManageWorkspace(ctx, actorID, workspaceID); err != nil || !ok {
		if err != nil {
			return err
		}
		return sql.ErrNoRows
	}
	if err := s.ensureNotLastOwner(ctx, workspaceID, memberUserID); err != nil {
		return err
	}
	var targetEmail string
	_ = s.pg.QueryRow(ctx, `SELECT email FROM users WHERE id = $1`, memberUserID).Scan(&targetEmail)
	tag, err := s.pg.Exec(ctx, `DELETE FROM workspace_members WHERE workspace_id = $1 AND user_id = $2`, workspaceID, memberUserID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return sql.ErrNoRows
	}
	_ = s.recordWorkspaceAudit(ctx, workspaceID, actorID, "member.removed", "user", memberUserID, targetEmail, "{}")
	return nil
}

func (s *Store) ListWorkspaceProjects(ctx context.Context, userID string, workspaceID string) ([]Project, error) {
	// projects[0] is the project /api/auth/me hands the app as the active one, so
	// the order has to be stable: p.id breaks the created_at tie rather than
	// leaving it to the planner.
	rows, err := s.pg.Query(ctx, `
SELECT p.id::text, p.workspace_id::text, p.name, p.api_key, p.created_at, wm.role
FROM projects p
JOIN workspace_members wm ON wm.workspace_id = p.workspace_id
WHERE wm.user_id = $1 AND p.workspace_id = $2
ORDER BY p.created_at ASC, p.id ASC`, userID, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	projects := []Project{}
	for rows.Next() {
		var project Project
		if err := rows.Scan(&project.ID, &project.WorkspaceID, &project.Name, &project.APIKey, &project.CreatedAt, &project.Role); err != nil {
			return nil, err
		}
		project.IsDemo = s.isDemoWorkspace(project.WorkspaceID)
		project.redactAPIKeyForRole()
		projects = append(projects, project)
	}
	return projects, rows.Err()
}

func (s *Store) CreateWorkspaceProject(ctx context.Context, userID string, workspaceID string, name string) (Project, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		name = "Untitled project"
	}
	if ok, err := s.userCanManageWorkspace(ctx, userID, workspaceID); err != nil || !ok {
		if err != nil {
			return Project{}, err
		}
		return Project{}, sql.ErrNoRows
	}
	apiKey := "agentray_" + uuid.NewString()
	var project Project
	err := s.pg.QueryRow(ctx, `
INSERT INTO projects (workspace_id, owner_id, name, api_key)
VALUES ($1, $2, $3, $4)
RETURNING id::text, workspace_id::text, name, api_key, created_at`, workspaceID, userID, name, apiKey).
		Scan(&project.ID, &project.WorkspaceID, &project.Name, &project.APIKey, &project.CreatedAt)
	if err != nil {
		return project, err
	}
	if err := s.SeedProjectFromTemplate(ctx, project.ID); err != nil {
		return project, err
	}
	// Only an owner/admin gets here (userCanManageWorkspace above), and the demo
	// mark follows the workspace it was created in.
	project.Role = "owner"
	project.IsDemo = s.isDemoWorkspace(project.WorkspaceID)
	_ = s.recordWorkspaceAudit(ctx, workspaceID, userID, "project.created", "project", project.ID, project.Name, "{}")
	return project, nil
}

func (s *Store) ProjectByIDForUser(ctx context.Context, userID string, projectID string) (Project, error) {
	var project Project
	err := s.pg.QueryRow(ctx, `
SELECT p.id::text, p.workspace_id::text, p.name, p.api_key, p.created_at, wm.role
FROM projects p
JOIN workspace_members wm ON wm.workspace_id = p.workspace_id
WHERE wm.user_id = $1 AND p.id = $2`, userID, projectID).
		Scan(&project.ID, &project.WorkspaceID, &project.Name, &project.APIKey, &project.CreatedAt, &project.Role)
	project.IsDemo = s.isDemoWorkspace(project.WorkspaceID)
	project.redactAPIKeyForRole()
	return project, err
}

func (s *Store) DefaultProjectForUser(ctx context.Context, userID string) (Project, error) {
	var project Project
	// This spans every workspace the user belongs to, and the shared demo is one
	// of them — older than every account, so created_at ASC alone would make
	// someone else's site the default project of every new signup. The demo is
	// pushed last; p.id keeps the order stable after that.
	err := s.pg.QueryRow(ctx, `
SELECT p.id::text, p.workspace_id::text, p.name, p.api_key, p.created_at, wm.role
FROM projects p
JOIN workspace_members wm ON wm.workspace_id = p.workspace_id
WHERE wm.user_id = $1
ORDER BY (p.workspace_id = NULLIF($2, '')::uuid) ASC, p.created_at ASC, p.id ASC
LIMIT 1`, userID, s.demoWorkspaceID).
		Scan(&project.ID, &project.WorkspaceID, &project.Name, &project.APIKey, &project.CreatedAt, &project.Role)
	project.IsDemo = s.isDemoWorkspace(project.WorkspaceID)
	project.redactAPIKeyForRole()
	return project, err
}

func (s *Store) UpdateProjectForUser(ctx context.Context, userID string, projectID string, name string) (Project, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		name = "Untitled project"
	}
	var project Project
	err := s.pg.QueryRow(ctx, `
UPDATE projects p
SET name = $3
FROM workspace_members wm
WHERE p.id = $2 AND wm.workspace_id = p.workspace_id AND wm.user_id = $1
	AND wm.role IN ('owner', 'admin')
RETURNING p.id::text, p.workspace_id::text, p.name, p.api_key, p.created_at, wm.role`, userID, projectID, name).
		Scan(&project.ID, &project.WorkspaceID, &project.Name, &project.APIKey, &project.CreatedAt, &project.Role)
	project.IsDemo = s.isDemoWorkspace(project.WorkspaceID)
	if err == nil {
		_ = s.recordWorkspaceAudit(ctx, project.WorkspaceID, userID, "project.renamed", "project", project.ID, project.Name, "{}")
	}
	return project, err
}

func (s *Store) RotateProjectAPIKeyForUser(ctx context.Context, userID string, projectID string) (Project, error) {
	apiKey := "agentray_" + uuid.NewString()
	var project Project
	err := s.pg.QueryRow(ctx, `
UPDATE projects p
SET api_key = $3
FROM workspace_members wm
WHERE p.id = $2 AND wm.workspace_id = p.workspace_id AND wm.user_id = $1
	AND wm.role IN ('owner', 'admin')
RETURNING p.id::text, p.workspace_id::text, p.name, p.api_key, p.created_at, wm.role`, userID, projectID, apiKey).
		Scan(&project.ID, &project.WorkspaceID, &project.Name, &project.APIKey, &project.CreatedAt, &project.Role)
	project.IsDemo = s.isDemoWorkspace(project.WorkspaceID)
	if err == nil {
		_ = s.recordWorkspaceAudit(ctx, project.WorkspaceID, userID, "project.key_rotated", "project", project.ID, project.Name, "{}")
	}
	return project, err
}

func (s *Store) ListWorkspaceAuditLogs(ctx context.Context, userID string, workspaceID string, limit int) ([]WorkspaceAuditLog, error) {
	if ok, err := s.userCanAccessWorkspace(ctx, userID, workspaceID); err != nil || !ok {
		if err != nil {
			return nil, err
		}
		return nil, sql.ErrNoRows
	}
	if limit <= 0 || limit > 50 {
		limit = 20
	}
	rows, err := s.pg.Query(ctx, `
SELECT
	wal.id::text,
	wal.workspace_id::text,
	COALESCE(wal.actor_id::text, ''),
	COALESCE(u.email, ''),
	wal.action,
	wal.target_type,
	COALESCE(wal.target_id::text, ''),
	wal.target_label,
	wal.metadata::text,
	wal.created_at
FROM workspace_audit_logs wal
LEFT JOIN users u ON u.id = wal.actor_id
WHERE wal.workspace_id = $1
ORDER BY wal.created_at DESC
LIMIT $2`, workspaceID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	logs := []WorkspaceAuditLog{}
	for rows.Next() {
		var log WorkspaceAuditLog
		if err := rows.Scan(&log.ID, &log.WorkspaceID, &log.ActorID, &log.ActorEmail, &log.Action, &log.TargetType, &log.TargetID, &log.TargetLabel, &log.Metadata, &log.CreatedAt); err != nil {
			return nil, err
		}
		logs = append(logs, log)
	}
	return logs, rows.Err()
}

func (s *Store) WorkspaceUsage(ctx context.Context, userID string, workspaceID string, filter EventFilter) (WorkspaceUsage, error) {
	usage := WorkspaceUsage{WorkspaceID: workspaceID, GeneratedAt: time.Now().UTC()}
	projects, err := s.ListWorkspaceProjects(ctx, userID, workspaceID)
	if err != nil {
		return usage, err
	}
	usage.ProjectCount = uint64(len(projects))
	if len(projects) == 0 {
		if ok, err := s.userCanAccessWorkspace(ctx, userID, workspaceID); err != nil || !ok {
			if err != nil {
				return usage, err
			}
			return usage, sql.ErrNoRows
		}
		return usage, nil
	}
	projectIDs := make([]string, len(projects))
	for i, project := range projects {
		projectIDs[i] = project.ID
	}
	where, args := workspaceFilteredWhere(projectIDs, filter, true)
	// The two numbers on this row answer different questions and must not share
	// a definition.
	//
	// EventCount is what the plan meters — every event that was ingested and
	// stored, crawlers included, because those were ingested and stored. Filtering
	// them here would show the customer a smaller number than the one their
	// ceiling is measured against.
	//
	// DistinctUsers renders as "People". It was uniqExact(distinct_id): no
	// identity stitching, so one human who browsed anonymously and then logged in
	// counted twice, and no bot filter, so every crawler counted as a person — on
	// the screen someone reads right before deciding to upgrade. It now matches
	// what "People" means everywhere else in the product (Persons, the funnel,
	// the activity summary): stitched, and human.
	//
	// The count is over (project_id, canonical) pairs because identity namespaces
	// are per-project — the alias dictionary is keyed that way, and nothing in the
	// product resolves one person across two projects. Collapsing on the bare id
	// would silently merge two projects' `user-42` into one person.
	canonical := s.workspaceCanonicalExpr("distinct_id")
	err = s.ch.QueryRow(ctx, `
SELECT
	count(),
	uniqExactIf((project_id, `+canonical+`), ifNull(visitor_class, 'human') = 'human')
FROM events
WHERE `+where, args...).Scan(&usage.EventCount, &usage.DistinctUsers)
	return usage, err
}

func (s *Store) userCanAccessWorkspace(ctx context.Context, userID string, workspaceID string) (bool, error) {
	var exists bool
	err := s.pg.QueryRow(ctx, `
SELECT EXISTS (
	SELECT 1 FROM workspace_members
	WHERE user_id = $1 AND workspace_id = $2
)`, userID, workspaceID).Scan(&exists)
	return exists, err
}

func (s *Store) recordWorkspaceAudit(ctx context.Context, workspaceID string, actorID string, action string, targetType string, targetID string, targetLabel string, metadata string) error {
	metadata = strings.TrimSpace(metadata)
	if metadata == "" {
		metadata = "{}"
	}
	_, err := s.pg.Exec(ctx, `
INSERT INTO workspace_audit_logs (workspace_id, actor_id, action, target_type, target_id, target_label, metadata)
VALUES ($1, $2, $3, $4, NULLIF($5, '')::uuid, $6, $7::jsonb)`, workspaceID, actorID, action, targetType, targetID, targetLabel, metadata)
	return err
}

func (s *Store) userCanManageWorkspace(ctx context.Context, userID string, workspaceID string) (bool, error) {
	var exists bool
	err := s.pg.QueryRow(ctx, `
SELECT EXISTS (
	SELECT 1 FROM workspace_members
	WHERE user_id = $1 AND workspace_id = $2 AND role IN ('owner', 'admin')
)`, userID, workspaceID).Scan(&exists)
	return exists, err
}

// UserCanManageWorkspace reports whether the user is an owner/admin of the
// workspace. Exported for HTTP handlers that need the same permission gate as the
// storage mutators without duplicating the SQL.
func (s *Store) UserCanManageWorkspace(ctx context.Context, userID string, workspaceID string) (bool, error) {
	return s.userCanManageWorkspace(ctx, userID, workspaceID)
}

func (s *Store) ensureNotLastOwner(ctx context.Context, workspaceID string, memberUserID string) error {
	var role string
	var owners int
	err := s.pg.QueryRow(ctx, `
SELECT
	COALESCE((SELECT role FROM workspace_members WHERE workspace_id = $1 AND user_id = $2), ''),
	(SELECT count(*) FROM workspace_members WHERE workspace_id = $1 AND role = 'owner')`, workspaceID, memberUserID).
		Scan(&role, &owners)
	if err != nil {
		return err
	}
	if role == "owner" && owners <= 1 {
		return fmt.Errorf("workspace must keep at least one owner")
	}
	return nil
}

// workspaceRoles is the role scale, most privileged first. It is the single
// source for both normalization and display order, so a new role is placed by
// editing this line alone. 'viewer' — the read-only role the shared demo grants
// (demo.go) — sits last because it is the least privileged, not because the
// members list happened to sort it there.
var workspaceRoles = []string{"owner", "admin", "member", DemoViewerRole}

// writeRoles is the other half of the role scale: which memberships may CHANGE
// a workspace, as opposed to reading it. It is a map rather than a condition
// inside one query because the same answer is needed by the HTTP write guard
// (internal/app/demo_guard.go), which stands in front of routes whose store
// call does not join workspace_members at all.
//
// Everything absent from it — 'viewer', an empty role, a role a future release
// adds and forgets to classify — cannot write. That direction is deliberate: a
// new role appearing in workspaceRoles without an entry here is read-only until
// someone decides otherwise, and TestEveryWorkspaceRoleIsClassified fails until
// they do. The opposite default would hand write access to a role nobody has
// thought about yet.
var writeRoles = map[string]bool{
	"owner":  true,
	"admin":  true,
	"member": true,
}

// RoleMayWrite reports whether a workspace membership may mutate the workspace.
// Exported for the HTTP write guard; unknown and empty roles answer false.
func RoleMayWrite(role string) bool {
	return writeRoles[strings.ToLower(strings.TrimSpace(role))]
}

// WorkspaceRoleForUser returns the caller's role in a workspace, or "" when they
// are not a member of it. Exported because the write guard has to make the
// read-only decision for ANY workspace-scoped route up front, including the
// ones whose handler resolves a workspace id straight from the path.
//
// A workspace id that is not a UUID answers "" rather than a cast error: the
// caller is asking "may this person write here", and a nonexistent workspace
// must answer no, not 500.
func (s *Store) WorkspaceRoleForUser(ctx context.Context, userID string, workspaceID string) (string, error) {
	if _, err := uuid.Parse(userID); err != nil {
		return "", nil
	}
	if _, err := uuid.Parse(workspaceID); err != nil {
		return "", nil
	}
	var role string
	err := s.pg.QueryRow(ctx, `
SELECT role FROM workspace_members WHERE user_id = $1::uuid AND workspace_id = $2::uuid`, userID, workspaceID).Scan(&role)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return role, err
}

// redactAPIKeyForRole blanks the project's API key for a membership that may not
// hold it.
//
// The key is a WRITE credential: anyone holding it can ingest events into the
// project (that is what the customer's own site does with it). Handing it to a
// read-only member would give back with one hand exactly what the viewer role
// takes with the other — a demo viewer could read the demo project's key off
// /api/auth/me and start writing events into someone else's live site.
//
// The api-key path leaves Role empty by design (there is no user to have one),
// so this is applied only where a membership was actually resolved.
func (p *Project) redactAPIKeyForRole() {
	if !RoleMayWrite(p.Role) {
		p.APIKey = ""
	}
}

// workspaceRoleRank is the display sort key. An unrecognised role sorts after
// every known one rather than silently landing among them.
func workspaceRoleRank(role string) int {
	for i, known := range workspaceRoles {
		if role == known {
			return i
		}
	}
	return len(workspaceRoles)
}

// workspaceRoleOrderSQL renders the same ranking as an ORDER BY expression, so
// the members list cannot drift from workspaceRoleRank. Roles are literals from
// the constant above — never user input — so inlining them is safe.
func workspaceRoleOrderSQL(column string) string {
	var b strings.Builder
	b.WriteString("CASE " + column)
	for i, role := range workspaceRoles {
		fmt.Fprintf(&b, " WHEN '%s' THEN %d", role, i)
	}
	fmt.Fprintf(&b, " ELSE %d END", len(workspaceRoles))
	return b.String()
}

// normalizeWorkspaceRole maps free-form input onto the enumerated scale.
// 'viewer' is enumerated here so it survives a round-trip through the members
// API instead of being silently promoted to 'member' by the default arm.
func normalizeWorkspaceRole(role string) string {
	switch strings.ToLower(strings.TrimSpace(role)) {
	case "owner":
		return "owner"
	case "admin":
		return "admin"
	case "viewer":
		return DemoViewerRole
	default:
		return "member"
	}
}

func randomToken() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}
