package storage

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Scoped project credentials (redesign slice 2, Option A): private, revocable
// management keys that carry an explicit scope set, beside the legacy
// projects.api_key. The legacy key is the capture credential — and, on
// projects that have not opted into the split, also the historical
// management/read bridge. projects.credential_split_at marks the opt-in: once
// set, the api_key is capture-only and management flows require a
// project_credentials row.
//
// Secrets are stored as SHA-256 hashes (they are random bearer tokens, not
// passwords — no password KDF needed) and shown exactly once at creation.
// Scope vocabulary lives in opcore (Access); the store persists strings and
// validates only shape, so a scope the app layer does not issue cannot be
// minted here either.

// ManagementScope is one scope string a management credential may carry.
// The set is closed: anything else is rejected at creation.
var ManagementScopes = map[string]bool{
	"analytics:read":   true,
	"dashboards:write": true,
	"sources:read":     true,
	"sources:manage":   true,
	"growth:write":     true,
}

// ProjectCredential is the non-secret view of a management credential: what
// list endpoints return. The secret itself is never read back.
type ProjectCredential struct {
	ID         string     `json:"id"`
	ProjectID  string     `json:"project_id"`
	Name       string     `json:"name"`
	Scopes     []string   `json:"scopes"`
	KeyHint    string     `json:"key_hint"` // last 4 chars, for identification only
	CreatedBy  string     `json:"created_by"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at"`
	RevokedAt  *time.Time `json:"revoked_at"`
}

// ResolvedCredential is what CredentialBySecret returns to the auth resolver:
// the project it binds and the scopes it carries.
type ResolvedCredential struct {
	CredentialID string
	ProjectID    string
	Scopes       []string
}

const managementKeyPrefix = "agm_"

func hashManagementKey(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

func (s *Store) migrateCredentials(ctx context.Context) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS project_credentials (
	id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
	project_id UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
	name VARCHAR(128) NOT NULL,
	key_hash CHAR(64) UNIQUE NOT NULL,
	key_hint CHAR(4) NOT NULL,
	scopes TEXT[] NOT NULL DEFAULT '{}',
	created_by UUID REFERENCES users(id) ON DELETE SET NULL,
	created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	last_used_at TIMESTAMPTZ,
	revoked_at TIMESTAMPTZ
)`,
		`CREATE INDEX IF NOT EXISTS project_credentials_project_idx ON project_credentials (project_id, created_at DESC)`,
		// credential_split_at is the Option A opt-in marker: NULL means the
		// project key still bridges management (legacy), set means capture-only.
		`ALTER TABLE projects ADD COLUMN IF NOT EXISTS credential_split_at TIMESTAMPTZ`,
	}
	for _, stmt := range stmts {
		if _, err := s.pg.Exec(ctx, stmt); err != nil {
			return err
		}
	}
	return nil
}

// CreateProjectCredential mints a scoped management credential (owner/admin
// only). The plaintext secret is returned once and never stored — only its
// hash and a 4-char hint persist.
func (s *Store) CreateProjectCredential(ctx context.Context, userID, projectID, name string, scopes []string) (ProjectCredential, string, error) {
	project, err := s.ProjectByIDForUser(ctx, userID, projectID)
	if err != nil {
		return ProjectCredential{}, "", err
	}
	canManage, err := s.userCanManageWorkspace(ctx, userID, project.WorkspaceID)
	if err != nil {
		return ProjectCredential{}, "", err
	}
	if !canManage {
		return ProjectCredential{}, "", errAgentForbidden
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return ProjectCredential{}, "", fmt.Errorf("credential name is required")
	}
	if len(scopes) == 0 {
		return ProjectCredential{}, "", fmt.Errorf("at least one scope is required")
	}
	normalized := make([]string, 0, len(scopes))
	for _, sc := range scopes {
		sc = strings.ToLower(strings.TrimSpace(sc))
		if !ManagementScopes[sc] {
			return ProjectCredential{}, "", fmt.Errorf("unknown scope %q", sc)
		}
		normalized = append(normalized, sc)
	}
	secret := managementKeyPrefix + uuid.NewString() + uuid.NewString()
	secret = strings.ReplaceAll(secret, "-", "")
	hint := secret[len(secret)-4:]
	var cred ProjectCredential
	err = s.pg.QueryRow(ctx, `
INSERT INTO project_credentials (project_id, name, key_hash, key_hint, scopes, created_by)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING id::text, project_id::text, name, key_hint, scopes, created_by::text, created_at`,
		projectID, name, hashManagementKey(secret), hint, normalized, userID).
		Scan(&cred.ID, &cred.ProjectID, &cred.Name, &cred.KeyHint, &cred.Scopes, &cred.CreatedBy, &cred.CreatedAt)
	if err != nil {
		return ProjectCredential{}, "", err
	}
	_ = s.recordWorkspaceAudit(ctx, project.WorkspaceID, userID, "credential.created", "project", project.ID, project.Name,
		fmt.Sprintf(`{"credential_id":%q,"name":%q,"scopes":%q}`, cred.ID, cred.Name, strings.Join(normalized, ",")))
	return cred, secret, nil
}

// ListProjectCredentials returns the project's credentials without secrets
// (member-readable; revocation state included so the UI can show it).
func (s *Store) ListProjectCredentials(ctx context.Context, userID, projectID string) ([]ProjectCredential, error) {
	if _, err := s.ProjectByIDForUser(ctx, userID, projectID); err != nil {
		return nil, err
	}
	rows, err := s.pg.Query(ctx, `
SELECT id::text, project_id::text, name, key_hint, scopes, COALESCE(created_by::text,''), created_at, last_used_at, revoked_at
FROM project_credentials WHERE project_id = $1 ORDER BY created_at ASC`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]ProjectCredential, 0)
	for rows.Next() {
		var c ProjectCredential
		if err := rows.Scan(&c.ID, &c.ProjectID, &c.Name, &c.KeyHint, &c.Scopes, &c.CreatedBy, &c.CreatedAt, &c.LastUsedAt, &c.RevokedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// RevokeProjectCredential disables a credential immediately (owner/admin
// only). Revocation is checked on every authenticated request, so the next
// call with the revoked secret fails.
func (s *Store) RevokeProjectCredential(ctx context.Context, userID, projectID, credentialID string) error {
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
	res, err := s.pg.Exec(ctx, `
UPDATE project_credentials SET revoked_at = now()
WHERE project_id = $1 AND id = $2 AND revoked_at IS NULL`, projectID, credentialID)
	if err != nil {
		return err
	}
	if res.RowsAffected() == 0 {
		return fmt.Errorf("credential not found or already revoked")
	}
	_ = s.recordWorkspaceAudit(ctx, project.WorkspaceID, userID, "credential.revoked", "project", project.ID, project.Name,
		fmt.Sprintf(`{"credential_id":%q}`, credentialID))
	return nil
}

// CredentialBySecret resolves a management credential by its plaintext secret.
// Internal auth path — no user check; the secret IS the credential. A revoked
// credential resolves to ErrNoRows so the caller answers "invalid" uniformly.
// last_used_at is stamped best-effort (async would leak a goroutine past
// request end; a synchronous UPDATE on the hot auth path is one indexed write).
func (s *Store) CredentialBySecret(ctx context.Context, secret string) (ResolvedCredential, error) {
	if !strings.HasPrefix(secret, managementKeyPrefix) {
		return ResolvedCredential{}, pgx.ErrNoRows
	}
	var out ResolvedCredential
	err := s.pg.QueryRow(ctx, `
UPDATE project_credentials SET last_used_at = now()
WHERE key_hash = $1 AND revoked_at IS NULL
RETURNING id::text, project_id::text, scopes`, hashManagementKey(secret)).
		Scan(&out.CredentialID, &out.ProjectID, &out.Scopes)
	if err != nil {
		return ResolvedCredential{}, err
	}
	return out, nil
}

// ProjectCredentialSplit reports whether the project has opted into the
// capture/management split (credential_split_at set). NULL = legacy bridge.
func (s *Store) ProjectCredentialSplit(ctx context.Context, projectID string) (bool, error) {
	var splitAt *time.Time
	err := s.pg.QueryRow(ctx, `SELECT credential_split_at FROM projects WHERE id = $1`, projectID).Scan(&splitAt)
	if err != nil {
		return false, err
	}
	return splitAt != nil, nil
}

// MarkProjectCredentialSplit opts the project into the credential split
// (owner/admin only, irreversible in this slice). Requires the project to hold
// at least one live management credential first — splitting without one would
// brick every key-authenticated management client with no path back.
func (s *Store) MarkProjectCredentialSplit(ctx context.Context, userID, projectID string) error {
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
	var live int
	if err := s.pg.QueryRow(ctx, `
SELECT count(*) FROM project_credentials WHERE project_id = $1 AND revoked_at IS NULL`, projectID).Scan(&live); err != nil {
		return err
	}
	if live == 0 {
		return fmt.Errorf("create a management credential before splitting: the project key becomes capture-only")
	}
	res, err := s.pg.Exec(ctx, `
UPDATE projects SET credential_split_at = now() WHERE id = $1 AND credential_split_at IS NULL`, projectID)
	if err != nil {
		return err
	}
	if res.RowsAffected() == 0 {
		return fmt.Errorf("project is already split")
	}
	_ = s.recordWorkspaceAudit(ctx, project.WorkspaceID, userID, "project.credential_split", "project", project.ID, project.Name, "{}")
	return nil
}

// constantTimeKeyEqual compares two secrets without a timing oracle. Used by
// tests and any future non-indexed comparison path.
func constantTimeKeyEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

var errCredentialNotFound = errors.New("credential not found")
