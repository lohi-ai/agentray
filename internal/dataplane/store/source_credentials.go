package storage

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Source credentials (redesign slice 2, approved contract: write-only
// reusable credential IDs). A source secret is stored once through a
// session-only endpoint and referenced by ID everywhere else — operations,
// agents, and receipts never carry secret material. The ciphertext is
// readable back only by the run/probe path (ConnectorDSNFor*), never by a
// list or read API.

// SourceCredential is the metadata a client may see — never the secret.
type SourceCredential struct {
	ID        string     `json:"id"`
	ProjectID string     `json:"project_id"`
	Name      string     `json:"name"`
	CreatedBy string     `json:"created_by"`
	CreatedAt time.Time  `json:"created_at"`
	RevokedAt *time.Time `json:"revoked_at,omitempty"`
}

func (s *Store) migrateSourceCredentials(ctx context.Context) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS source_credentials (
	id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
	project_id UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
	name VARCHAR(128) NOT NULL,
	dsn_ciphertext TEXT NOT NULL,
	created_by UUID REFERENCES users(id) ON DELETE SET NULL,
	created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	revoked_at TIMESTAMPTZ
)`,
		`CREATE INDEX IF NOT EXISTS source_credentials_project_idx ON source_credentials (project_id, created_at DESC)`,
		// Connectors may reference a credential instead of carrying inline
		// ciphertext; revision gives update_source the same optimistic
		// contract dashboards and syncs have.
		`ALTER TABLE data_connectors ADD COLUMN IF NOT EXISTS credential_id UUID REFERENCES source_credentials(id) ON DELETE RESTRICT`,
		`ALTER TABLE data_connectors ADD COLUMN IF NOT EXISTS revision BIGINT NOT NULL DEFAULT 1`,
	}
	for _, stmt := range stmts {
		if _, err := s.pg.Exec(ctx, stmt); err != nil {
			return err
		}
	}
	return nil
}

// CreateSourceCredential stores a source secret (session owner/admin only —
// the route enforces; this method double-checks the workspace role). The DSN
// is encrypted before it touches the row and is never returned.
func (s *Store) CreateSourceCredential(ctx context.Context, userID, projectID, name, dsn string) (SourceCredential, error) {
	project, err := s.ProjectByIDForUser(ctx, userID, projectID)
	if err != nil {
		return SourceCredential{}, err
	}
	canManage, err := s.userCanManageWorkspace(ctx, userID, project.WorkspaceID)
	if err != nil {
		return SourceCredential{}, err
	}
	if !canManage {
		return SourceCredential{}, ErrAgentForbidden
	}
	if len(dsn) < 8 {
		return SourceCredential{}, fmt.Errorf("credential material looks too short to be a DSN")
	}
	ciphertext, err := encryptAgentKey(dsn)
	if err != nil {
		return SourceCredential{}, err
	}
	var c SourceCredential
	err = s.pg.QueryRow(ctx, `
INSERT INTO source_credentials (project_id, name, dsn_ciphertext, created_by)
VALUES ($1, $2, $3, $4)
RETURNING id::text, project_id::text, name, created_by::text, created_at, revoked_at`,
		projectID, name, ciphertext, userID).
		Scan(&c.ID, &c.ProjectID, &c.Name, &c.CreatedBy, &c.CreatedAt, &c.RevokedAt)
	if err != nil {
		return SourceCredential{}, err
	}
	_ = s.recordWorkspaceAudit(ctx, project.WorkspaceID, userID, "source_credential.create", "project", project.ID, project.Name, "{}")
	return c, nil
}

// CreateSourceConnectorIdempotent is the session-only secret-entry bridge for
// the web Connectors tab. It encrypts the DSN, inserts its write-only
// credential, and references it from a connector in the SAME idempotent
// transaction: a failed connector insert rolls the credential back, while a
// response lost after commit safely replays the original connector with the
// caller's idempotency key. The shared create_source operation remains the
// credential-ID contract for CLI/MCP/runtime callers; it never accepts DSNs.
func (s *Store) CreateSourceConnectorIdempotent(ctx context.Context, userID, projectID, name, kind, dsn, idemKey string) (DataConnector, error) {
	project, err := s.ProjectByIDForUser(ctx, userID, projectID)
	if err != nil {
		return DataConnector{}, err
	}
	canManage, err := s.userCanManageWorkspace(ctx, userID, project.WorkspaceID)
	if err != nil {
		return DataConnector{}, err
	}
	if !canManage {
		return DataConnector{}, ErrAgentForbidden
	}
	if len(dsn) < 8 {
		return DataConnector{}, fmt.Errorf("credential material looks too short to be a DSN")
	}
	if !connectorKindKnown(kind) {
		return DataConnector{}, fmt.Errorf("unknown connector kind %q", kind)
	}
	if name == "" {
		name = "Untitled source"
	}
	ciphertext, err := encryptAgentKey(dsn)
	if err != nil {
		return DataConnector{}, err
	}
	request, err := json.Marshal(struct {
		Name string `json:"name"`
		Kind string `json:"kind"`
		DSN  string `json:"dsn"`
	}{Name: name, Kind: kind, DSN: dsn})
	if err != nil {
		return DataConnector{}, err
	}
	requestHash := fmt.Sprintf("%x", sha256.Sum256(request))

	raw, err := s.runIdempotent(ctx, projectID, "create_source_connector", idemKey, requestHash,
		func(ctx context.Context, q pgQuerier) (json.RawMessage, error) {
			var credentialID string
			if err := q.QueryRow(ctx, `
INSERT INTO source_credentials (project_id, name, dsn_ciphertext, created_by)
VALUES ($1, $2, $3, $4)
RETURNING id::text`, projectID, name, ciphertext, userID).Scan(&credentialID); err != nil {
				return nil, err
			}
			var c DataConnector
			if err := q.QueryRow(ctx, `
INSERT INTO data_connectors (project_id, name, kind, credential_id)
VALUES ($1, $2, $3, $4)
RETURNING id::text, project_id::text, name, kind, true, revision, created_at, updated_at`,
				projectID, name, kind, credentialID).
				Scan(&c.ID, &c.ProjectID, &c.Name, &c.Kind, &c.HasDSN, &c.Revision, &c.CreatedAt, &c.UpdatedAt); err != nil {
				return nil, err
			}
			if _, err := q.Exec(ctx, `
INSERT INTO workspace_audit_logs (workspace_id, actor_id, action, target_type, target_id, target_label, metadata)
VALUES ($1, $2, $3, $4, $5::uuid, $6, '{}'::jsonb)`,
				project.WorkspaceID, userID, "source_connector.create", "connector", c.ID, c.Name); err != nil {
				return nil, err
			}
			return json.Marshal(c)
		})
	if err != nil {
		return DataConnector{}, err
	}
	var c DataConnector
	if err := json.Unmarshal(raw, &c); err != nil {
		return DataConnector{}, fmt.Errorf("stored source connector receipt unreadable: %w", err)
	}
	return c, nil
}

// RevokeSourceCredential marks a credential unusable. Connectors referencing
// it keep their row but fail at run/probe time — revocation is the point.
// No route calls this today; it is the only writer of revoked_at and the
// fixture the fails-closed tests exercise.
func (s *Store) RevokeSourceCredential(ctx context.Context, userID, projectID, credentialID string) error {
	project, err := s.ProjectByIDForUser(ctx, userID, projectID)
	if err != nil {
		return err
	}
	canManage, err := s.userCanManageWorkspace(ctx, userID, project.WorkspaceID)
	if err != nil {
		return err
	}
	if !canManage {
		return ErrAgentForbidden
	}
	tag, err := s.pg.Exec(ctx, `
UPDATE source_credentials SET revoked_at = now()
WHERE id = $1 AND project_id = $2 AND revoked_at IS NULL`, credentialID, projectID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	_ = s.recordWorkspaceAudit(ctx, project.WorkspaceID, userID, "source_credential.revoke", "project", project.ID, project.Name, "{}")
	return nil
}


// sourceCredentialDSN decrypts a credential's secret for the run/probe path.
// A revoked or foreign credential fails closed.
func (s *Store) sourceCredentialDSN(ctx context.Context, q pgQuerier, projectID, credentialID string) (string, error) {
	var ciphertext string
	err := q.QueryRow(ctx, `
SELECT dsn_ciphertext FROM source_credentials
WHERE id = $1 AND project_id = $2 AND revoked_at IS NULL`, credentialID, projectID).Scan(&ciphertext)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("credential not found or revoked")
	}
	if err != nil {
		return "", err
	}
	return decryptAgentKey(ciphertext)
}

// CreateDataConnectorForProject is the project-scoped create the operation
// layer uses: the caller supplies a credential ID, never secret material.
// The credential must belong to the same project and be live.
func (s *Store) CreateDataConnectorForProject(ctx context.Context, projectID, name, kind, credentialID string) (DataConnector, error) {
	if !connectorKindKnown(kind) {
		return DataConnector{}, fmt.Errorf("unknown connector kind %q", kind)
	}
	if name == "" {
		name = "Untitled source"
	}
	// Prove the credential exists, is this project's, is not revoked, and was
	// session-created (created_by set — only CreateSourceCredential writes
	// rows, always with the session user) — before the connector references it.
	var live bool
	if err := s.pg.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM source_credentials WHERE id = $1 AND project_id = $2 AND revoked_at IS NULL AND created_by IS NOT NULL)`,
		credentialID, projectID).Scan(&live); err != nil {
		return DataConnector{}, err
	}
	if !live {
		return DataConnector{}, fmt.Errorf("credential not found or revoked")
	}
	var c DataConnector
	err := s.pg.QueryRow(ctx, `
INSERT INTO data_connectors (project_id, name, kind, credential_id)
VALUES ($1, $2, $3, $4)
RETURNING `+dataConnectorColumns,
		projectID, name, kind, credentialID).
		Scan(dataConnectorScanDest(&c)...)
	return c, err
}

// CreateDataConnectorForUser is the session-facing create: owner/admin in the
// project's workspace, then the same credential-referenced insert.
func (s *Store) CreateDataConnectorForUser(ctx context.Context, userID, projectID, name, kind, credentialID string) (DataConnector, error) {
	project, err := s.ProjectByIDForUser(ctx, userID, projectID)
	if err != nil {
		return DataConnector{}, err
	}
	canManage, err := s.userCanManageWorkspace(ctx, userID, project.WorkspaceID)
	if err != nil {
		return DataConnector{}, err
	}
	if !canManage {
		return DataConnector{}, ErrAgentForbidden
	}
	c, err := s.CreateDataConnectorForProject(ctx, projectID, name, kind, credentialID)
	if err != nil {
		return DataConnector{}, err
	}
	_ = s.recordWorkspaceAudit(ctx, project.WorkspaceID, userID, "connector.create", "project", project.ID, project.Name, "{}")
	return c, nil
}

// CreateDataConnectorIdempotent is CreateDataConnectorForProject under an
// idempotency claim — a retried create replays the first connector instead
// of inserting a duplicate.
func (s *Store) CreateDataConnectorIdempotent(ctx context.Context, projectID, name, kind, credentialID, idemKey, requestHash string) (DataConnector, error) {
	raw, err := s.runIdempotent(ctx, projectID, "create_source", idemKey, requestHash,
		func(ctx context.Context, q pgQuerier) (json.RawMessage, error) {
			if !connectorKindKnown(kind) {
				return nil, fmt.Errorf("unknown connector kind %q", kind)
			}
			if name == "" {
				name = "Untitled source"
			}
			var live bool
			if err := q.QueryRow(ctx,
				`SELECT EXISTS(SELECT 1 FROM source_credentials WHERE id = $1 AND project_id = $2 AND revoked_at IS NULL AND created_by IS NOT NULL)`,
				credentialID, projectID).Scan(&live); err != nil {
				return nil, err
			}
			if !live {
				return nil, fmt.Errorf("credential not found or revoked")
			}
			var c DataConnector
			err := q.QueryRow(ctx, `
INSERT INTO data_connectors (project_id, name, kind, credential_id)
VALUES ($1, $2, $3, $4)
RETURNING `+dataConnectorColumns,
				projectID, name, kind, credentialID).
				Scan(dataConnectorScanDest(&c)...)
			if err != nil {
				return nil, err
			}
			return json.Marshal(c)
		})
	if err != nil {
		return DataConnector{}, err
	}
	var c DataConnector
	if err := json.Unmarshal(raw, &c); err != nil {
		return DataConnector{}, fmt.Errorf("stored source receipt unreadable: %w", err)
	}
	return c, nil
}

// UpdateDataConnectorIdempotent is UpdateDataConnectorForProject under an
// idempotency claim.
func (s *Store) UpdateDataConnectorIdempotent(ctx context.Context, projectID, connectorID string, name *string, credentialID *string, expectedRevision int64, idemKey, requestHash string) (DataConnector, error) {
	raw, err := s.runIdempotent(ctx, projectID, "update_source", idemKey, requestHash,
		func(ctx context.Context, q pgQuerier) (json.RawMessage, error) {
			if credentialID != nil {
				var live bool
				if err := q.QueryRow(ctx,
					`SELECT EXISTS(SELECT 1 FROM source_credentials WHERE id = $1 AND project_id = $2 AND revoked_at IS NULL AND created_by IS NOT NULL)`,
					*credentialID, projectID).Scan(&live); err != nil {
					return nil, err
				}
				if !live {
					return nil, fmt.Errorf("credential not found or revoked")
				}
			}
			var c DataConnector
			err := q.QueryRow(ctx, `
UPDATE data_connectors
SET name = CASE WHEN $3::text IS NULL THEN name WHEN $3 = '' THEN name ELSE $3 END,
    credential_id = COALESCE($4::uuid, credential_id),
    revision = revision + 1, updated_at = now()
WHERE id = $1 AND project_id = $2 AND revision = $5
RETURNING `+dataConnectorColumns,
				connectorID, projectID, name, credentialID, expectedRevision).
				Scan(dataConnectorScanDest(&c)...)
			if errors.Is(err, pgx.ErrNoRows) {
				var exists bool
				if qerr := q.QueryRow(ctx,
					`SELECT EXISTS(SELECT 1 FROM data_connectors WHERE id = $1 AND project_id = $2)`,
					connectorID, projectID).Scan(&exists); qerr == nil && exists {
					return nil, ErrRevisionConflict
				}
				return nil, pgx.ErrNoRows
			}
			if err != nil {
				return nil, err
			}
			return json.Marshal(c)
		})
	if err != nil {
		return DataConnector{}, err
	}
	var c DataConnector
	if err := json.Unmarshal(raw, &c); err != nil {
		return DataConnector{}, fmt.Errorf("stored source receipt unreadable: %w", err)
	}
	return c, nil
}

// UpdateDataConnectorForProject is the revision-checked project-scoped
// update. A nil credentialID preserves the current credential; a non-nil one
// must be a live credential of the same project.
func (s *Store) UpdateDataConnectorForProject(ctx context.Context, projectID, connectorID, name string, credentialID *string, expectedRevision int64) (DataConnector, error) {
	if credentialID != nil {
		var live bool
		if err := s.pg.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM source_credentials WHERE id = $1 AND project_id = $2 AND revoked_at IS NULL AND created_by IS NOT NULL)`,
			*credentialID, projectID).Scan(&live); err != nil {
			return DataConnector{}, err
		}
		if !live {
			return DataConnector{}, fmt.Errorf("credential not found or revoked")
		}
	}
	var c DataConnector
	err := s.pg.QueryRow(ctx, `
UPDATE data_connectors
SET name = CASE WHEN $3 = '' THEN name ELSE $3 END,
    credential_id = COALESCE($4::uuid, credential_id),
    revision = revision + 1, updated_at = now()
WHERE id = $1 AND project_id = $2 AND revision = $5
RETURNING `+dataConnectorColumns,
		connectorID, projectID, name, credentialID, expectedRevision).
		Scan(dataConnectorScanDest(&c)...)
	if errors.Is(err, pgx.ErrNoRows) {
		var exists bool
		if qerr := s.pg.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM data_connectors WHERE id = $1 AND project_id = $2)`,
			connectorID, projectID).Scan(&exists); qerr == nil && exists {
			return DataConnector{}, ErrRevisionConflict
		}
		return DataConnector{}, pgx.ErrNoRows
	}
	return c, err
}
