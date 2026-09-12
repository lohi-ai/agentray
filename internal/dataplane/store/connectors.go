package storage

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/lohi-ai/agentray/internal/dataplane/connector"
)

// Data connectors (parent plan bs-eano39vq §1): operator-configured external
// data sources whose rows are pulled into the ClickHouse external_rows landing
// table on a schedule, where run_sql can query them next to events. The DSN is
// AES-encrypted with the same agentEncKey path as agent secrets and is
// write-only over the API — list/read surfaces return only its presence.

// DataConnector is one configured source connection (DSN never included).
type DataConnector struct {
	ID        string `json:"id"`
	ProjectID string `json:"project_id"`
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	HasDSN    bool   `json:"has_dsn"`
	// Revision is the optimistic-concurrency counter update_source carries —
	// same contract as dashboards and syncs.
	Revision  int64     `json:"revision"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// ConnectorSync is one table-sync config plus its run status.
type ConnectorSync struct {
	ID           string     `json:"id"`
	ConnectorID  string     `json:"connector_id"`
	ProjectID    string     `json:"project_id"`
	SourceTable  string     `json:"source_table"`
	KeyColumn    string     `json:"key_column"`
	CursorColumn string     `json:"cursor_column"`
	ScheduleCron string     `json:"schedule_cron"`
	Enabled      bool       `json:"enabled"`
	Cursor       string     `json:"cursor"`
	CursorKey    string     `json:"cursor_key"`
	LastRunAt    *time.Time `json:"last_run_at"`
	LastStatus   string     `json:"last_status"`
	LastError    string     `json:"last_error"`
	LastRows     int        `json:"last_rows"`
	TotalRows    int64      `json:"total_rows"`
	// LastSuccessAt is the last run whose final status was 'succeeded' —
	// distinct from LastRunAt, which records the last attempt of any outcome.
	LastSuccessAt *time.Time `json:"last_success_at"`
	// JoinKey names the source column holding a person-identity value
	// (resolved to distinct_id via aliases). Fact-level keys are rejected.
	JoinKey string `json:"join_key"`
	// JoinValidated is 'validated' | 'unvalidated' | '' — a bounded
	// duplicate-key check over the deduped set; fail-closed above the cap.
	JoinValidated string `json:"join_validated"`
	// DeletionMode: 'none' (hard deletes unsupported — honest default) or
	// 'soft_column' (source marks deletions).
	DeletionMode string `json:"deletion_mode"`
	// SoftDeleteColumn names the source column carrying the deletion mark.
	SoftDeleteColumn string `json:"soft_delete_column"`
	// SoftDeleteSemantics: 'bool_true' (boolean, true = deleted) or
	// 'non_null' (e.g. deleted_at timestamp, non-NULL = deleted).
	SoftDeleteSemantics string `json:"soft_delete_semantics"`
	// LandingSeq is the per-sync monotonic run sequence feeding the
	// external_rows version column (landing_seq*1e6 + batch_index).
	LandingSeq int64 `json:"landing_seq"`
	// Revision is the optimistic-concurrency counter pause/update carry —
	// same contract as dashboards.
	Revision  int64     `json:"revision"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

const connectorSyncColumns = `id::text, connector_id::text, project_id::text, source_table, key_column,
	cursor_column, schedule_cron, enabled, cursor, cursor_key, last_run_at, last_status, last_error, last_rows, total_rows,
	last_success_at, join_key, join_validated, deletion_mode, soft_delete_column, soft_delete_semantics, landing_seq,
	revision, created_at, updated_at`

func (s *Store) migrateConnectors(ctx context.Context) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS data_connectors (
	id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
	project_id UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
	name VARCHAR(128) NOT NULL,
	kind VARCHAR(32) NOT NULL,
	dsn_ciphertext TEXT NOT NULL DEFAULT '',
	created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
)`,
		`CREATE INDEX IF NOT EXISTS data_connectors_project_idx ON data_connectors (project_id, created_at DESC)`,
		`CREATE TABLE IF NOT EXISTS connector_syncs (
	id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
	connector_id UUID NOT NULL REFERENCES data_connectors(id) ON DELETE CASCADE,
	project_id UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
	source_table VARCHAR(256) NOT NULL,
	key_column VARCHAR(128) NOT NULL,
	cursor_column VARCHAR(128) NOT NULL DEFAULT '',
	schedule_cron VARCHAR(64) NOT NULL DEFAULT '',
	enabled BOOLEAN NOT NULL DEFAULT true,
	cursor TEXT NOT NULL DEFAULT '',
	last_run_at TIMESTAMPTZ,
	last_status VARCHAR(16) NOT NULL DEFAULT '',
	last_error TEXT NOT NULL DEFAULT '',
	last_rows INT NOT NULL DEFAULT 0,
	total_rows BIGINT NOT NULL DEFAULT 0,
	created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	UNIQUE (connector_id, source_table)
)`,
		`CREATE INDEX IF NOT EXISTS connector_syncs_project_idx ON connector_syncs (project_id, created_at DESC)`,
		// cursor_key is the tie-breaking half of the keyset cursor: the key of
		// the last synced row, so rows sharing one cursor value are never
		`ALTER TABLE connector_syncs ADD COLUMN IF NOT EXISTS cursor_key TEXT NOT NULL DEFAULT ''`,
		// Slice-4 dataset contract: join/deletion config and the monotonic
		// landing sequence feeding the external_rows version column.
		`ALTER TABLE connector_syncs ADD COLUMN IF NOT EXISTS last_success_at TIMESTAMPTZ`,
		`ALTER TABLE connector_syncs ADD COLUMN IF NOT EXISTS join_key VARCHAR(128) NOT NULL DEFAULT ''`,
		`ALTER TABLE connector_syncs ADD COLUMN IF NOT EXISTS join_validated VARCHAR(16) NOT NULL DEFAULT ''`,
		`ALTER TABLE connector_syncs ADD COLUMN IF NOT EXISTS deletion_mode VARCHAR(16) NOT NULL DEFAULT 'none'`,
		`ALTER TABLE connector_syncs ADD COLUMN IF NOT EXISTS soft_delete_column VARCHAR(128) NOT NULL DEFAULT ''`,
		`ALTER TABLE connector_syncs ADD COLUMN IF NOT EXISTS soft_delete_semantics VARCHAR(16) NOT NULL DEFAULT ''`,
		`ALTER TABLE connector_syncs ADD COLUMN IF NOT EXISTS landing_seq BIGINT NOT NULL DEFAULT 0`,
	}
	for _, stmt := range stmts {
		if _, err := s.pg.Exec(ctx, stmt); err != nil {
			return err
		}
	}
	return nil
}

// --- connector CRUD ---

// CreateDataConnector stores a connection (owner/admin only). The DSN is
// encrypted before it touches the row and is never returned.
func (s *Store) CreateDataConnector(ctx context.Context, userID, projectID, name, kind, dsn string) (DataConnector, error) {
	project, err := s.ProjectByIDForUser(ctx, userID, projectID)
	if err != nil {
		return DataConnector{}, err
	}
	canManage, err := s.userCanManageWorkspace(ctx, userID, project.WorkspaceID)
	if err != nil {
		return DataConnector{}, err
	}
	if !canManage {
		return DataConnector{}, errAgentForbidden
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return DataConnector{}, fmt.Errorf("connector name is required")
	}
	if !connectorKindKnown(kind) {
		return DataConnector{}, fmt.Errorf("unknown connector kind %q (available: %v)", kind, connector.Kinds())
	}
	if strings.TrimSpace(dsn) == "" {
		return DataConnector{}, fmt.Errorf("connection string is required")
	}
	ciphertext, err := encryptAgentKey(dsn)
	if err != nil {
		return DataConnector{}, err
	}
	var out DataConnector
	err = s.pg.QueryRow(ctx, `
INSERT INTO data_connectors (project_id, name, kind, dsn_ciphertext)
VALUES ($1, $2, $3, $4)
RETURNING id::text, project_id::text, name, kind, dsn_ciphertext != '', revision, created_at, updated_at`,
		projectID, name, kind, ciphertext).
		Scan(&out.ID, &out.ProjectID, &out.Name, &out.Kind, &out.HasDSN, &out.Revision, &out.CreatedAt, &out.UpdatedAt)
	if err != nil {
		return DataConnector{}, err
	}
	_ = s.recordWorkspaceAudit(ctx, project.WorkspaceID, userID, "connector.create", "project", project.ID, project.Name, "{}")
	return out, nil
}

// ListDataConnectors returns a project's connectors (member-readable, no DSN).
func (s *Store) ListDataConnectors(ctx context.Context, userID, projectID string) ([]DataConnector, error) {
	if _, err := s.ProjectByIDForUser(ctx, userID, projectID); err != nil {
		return nil, err
	}
	rows, err := s.pg.Query(ctx, `
SELECT id::text, project_id::text, name, kind, dsn_ciphertext != '' OR credential_id IS NOT NULL, revision, created_at, updated_at
FROM data_connectors WHERE project_id = $1 ORDER BY created_at DESC`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]DataConnector, 0)
	for rows.Next() {
		var c DataConnector
		if err := rows.Scan(&c.ID, &c.ProjectID, &c.Name, &c.Kind, &c.HasDSN, &c.Revision, &c.CreatedAt, &c.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// DeleteDataConnector removes a connector and its syncs (owner/admin only).
// Landed rows in ClickHouse are kept — they are the analytical record.
func (s *Store) DeleteDataConnector(ctx context.Context, userID, projectID, connectorID string) error {
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
	_, err = s.pg.Exec(ctx, `DELETE FROM data_connectors WHERE project_id = $1 AND id = $2`, projectID, connectorID)
	if err != nil {
		return err
	}
	_ = s.recordWorkspaceAudit(ctx, project.WorkspaceID, userID, "connector.delete", "project", project.ID, project.Name, "{}")
	return nil
}

// ConnectorDSNForRun decrypts a connector's DSN for in-process use (engine,
// test-connection, schema discovery). Caller must have authorized the user —
// this is the internal trust-boundary read, mirroring AgentSecretsForRun.
func (s *Store) ConnectorDSNForRun(ctx context.Context, projectID, connectorID string) (kind, dsn string, err error) {
	var ciphertext, credentialID string
	err = s.pg.QueryRow(ctx, `
SELECT kind, dsn_ciphertext, COALESCE(credential_id::text, '') FROM data_connectors WHERE project_id = $1 AND id = $2`,
		projectID, connectorID).Scan(&kind, &ciphertext, &credentialID)
	if err != nil {
		return "", "", err
	}
	if credentialID != "" {
		dsn, err = s.sourceCredentialDSN(ctx, s.pg, projectID, credentialID)
		if err != nil {
			return "", "", err
		}
		return kind, dsn, nil
	}
	dsn, err = decryptAgentKey(ciphertext)
	if err != nil {
		return "", "", err
	}
	return kind, dsn, nil
}

// ConnectorDSNForUser is the authorized edge for test-connection / schema
// discovery: owner/admin only, and the decrypted DSN stays in-process — the
// route uses it to dial the source and returns only the outcome.
func (s *Store) ConnectorDSNForUser(ctx context.Context, userID, projectID, connectorID string) (kind, dsn string, err error) {
	project, err := s.ProjectByIDForUser(ctx, userID, projectID)
	if err != nil {
		return "", "", err
	}
	canManage, err := s.userCanManageWorkspace(ctx, userID, project.WorkspaceID)
	if err != nil {
		return "", "", err
	}
	if !canManage {
		return "", "", errAgentForbidden
	}
	return s.ConnectorDSNForRun(ctx, projectID, connectorID)
}

// --- sync CRUD ---
// ConnectorSyncInput is the operator-editable subset of a sync config.
type ConnectorSyncInput struct {
	SourceTable  string `json:"source_table"`
	KeyColumn    string `json:"key_column"`
	CursorColumn string `json:"cursor_column"`
	ScheduleCron string `json:"schedule_cron"`
	Enabled      bool   `json:"enabled"`
	// JoinKey names the source column holding a person-identity value
	// (resolved to distinct_id via aliases). Fact-level keys are rejected by
	// the usecase layer's join validation.
	JoinKey string `json:"join_key"`
	// DeletionMode: 'none' (hard deletes unsupported — honest default) or
	// 'soft_column' (source marks deletions).
	DeletionMode string `json:"deletion_mode"`
	// SoftDeleteColumn names the source column carrying the deletion mark.
	SoftDeleteColumn string `json:"soft_delete_column"`
	// SoftDeleteSemantics: 'bool_true' (boolean, true = deleted) or
	// 'non_null' (e.g. deleted_at timestamp, non-NULL = deleted).
	SoftDeleteSemantics string `json:"soft_delete_semantics"`
}

// CreateConnectorSync adds a table sync to a connector (owner/admin only).
func (s *Store) CreateConnectorSync(ctx context.Context, userID, projectID, connectorID string, in ConnectorSyncInput) (ConnectorSync, error) {
	project, err := s.ProjectByIDForUser(ctx, userID, projectID)
	if err != nil {
		return ConnectorSync{}, err
	}
	canManage, err := s.userCanManageWorkspace(ctx, userID, project.WorkspaceID)
	if err != nil {
		return ConnectorSync{}, err
	}
	if !canManage {
		return ConnectorSync{}, errAgentForbidden
	}
	if err := validateSyncInput(in); err != nil {
		return ConnectorSync{}, err
	}
	// The connector must belong to the same project (an id from another
	// tenant must not be attachable).
	var one int
	if err := s.pg.QueryRow(ctx, `SELECT 1 FROM data_connectors WHERE project_id = $1 AND id = $2`, projectID, connectorID).Scan(&one); err != nil {
		return ConnectorSync{}, fmt.Errorf("connector not found")
	}
	var out ConnectorSync
	err = s.pg.QueryRow(ctx, `
INSERT INTO connector_syncs (connector_id, project_id, source_table, key_column, cursor_column, schedule_cron, enabled,
	join_key, deletion_mode, soft_delete_column, soft_delete_semantics)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
RETURNING `+connectorSyncColumns,
		connectorID, projectID, in.SourceTable, in.KeyColumn, in.CursorColumn, in.ScheduleCron, in.Enabled,
		in.JoinKey, in.DeletionMode, in.SoftDeleteColumn, in.SoftDeleteSemantics).
		Scan(syncScanDest(&out)...)
	if err != nil {
		return ConnectorSync{}, err
	}
	_ = s.recordWorkspaceAudit(ctx, project.WorkspaceID, userID, "connector.sync.create", "project", project.ID, project.Name, "{}")
	return out, nil
}

// UpdateConnectorSync overwrites a sync's editable fields (owner/admin only).
// Changing the source table or cursor column resets the cursor pair — the old
// position is meaningless against a new shape.
func (s *Store) UpdateConnectorSync(ctx context.Context, userID, projectID, syncID string, in ConnectorSyncInput) (ConnectorSync, error) {
	project, err := s.ProjectByIDForUser(ctx, userID, projectID)
	if err != nil {
		return ConnectorSync{}, err
	}
	canManage, err := s.userCanManageWorkspace(ctx, userID, project.WorkspaceID)
	if err != nil {
		return ConnectorSync{}, err
	}
	if !canManage {
		return ConnectorSync{}, errAgentForbidden
	}
	if err := validateSyncInput(in); err != nil {
		return ConnectorSync{}, err
	}
	var out ConnectorSync
	err = s.pg.QueryRow(ctx, `
UPDATE connector_syncs SET
	source_table = $3::text, key_column = $4::text, cursor_column = $5::text, schedule_cron = $6, enabled = $7,
	join_key = $8::text, deletion_mode = $9::text, soft_delete_column = $10::text, soft_delete_semantics = $11::text,
	join_validated = CASE WHEN join_key IS DISTINCT FROM $8::text THEN '' ELSE join_validated END,
	cursor = CASE WHEN source_table = $3::text AND cursor_column = $5::text THEN cursor ELSE '' END,
	cursor_key = CASE WHEN source_table = $3::text AND cursor_column = $5::text AND key_column = $4::text THEN cursor_key ELSE '' END,
	revision = revision + 1, updated_at = now()
WHERE project_id = $1 AND id = $2
RETURNING `+connectorSyncColumns,
		projectID, syncID, in.SourceTable, in.KeyColumn, in.CursorColumn, in.ScheduleCron, in.Enabled,
		in.JoinKey, in.DeletionMode, in.SoftDeleteColumn, in.SoftDeleteSemantics).
		Scan(syncScanDest(&out)...)
	if err != nil {
		return ConnectorSync{}, err
	}
	return out, nil
}

// ListConnectorSyncs returns a connector's syncs with status (member-readable).
func (s *Store) ListConnectorSyncs(ctx context.Context, userID, projectID, connectorID string) ([]ConnectorSync, error) {
	if _, err := s.ProjectByIDForUser(ctx, userID, projectID); err != nil {
		return nil, err
	}
	rows, err := s.pg.Query(ctx, `
SELECT `+connectorSyncColumns+`
FROM connector_syncs WHERE project_id = $1 AND connector_id = $2 ORDER BY created_at ASC`, projectID, connectorID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]ConnectorSync, 0)
	for rows.Next() {
		var cs ConnectorSync
		if err := rows.Scan(syncScanDest(&cs)...); err != nil {
			return nil, err
		}
		out = append(out, cs)
	}
	return out, rows.Err()
}

// DeleteConnectorSync removes one sync config (owner/admin only).
func (s *Store) DeleteConnectorSync(ctx context.Context, userID, projectID, syncID string) error {
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
	_, err = s.pg.Exec(ctx, `DELETE FROM connector_syncs WHERE project_id = $1 AND id = $2`, projectID, syncID)
	return err
}

// SyncBelongsToProject reports whether a sync id is in the project — the
// authorization step for the manual run-now endpoint.
func (s *Store) SyncBelongsToProject(ctx context.Context, projectID, syncID string) (bool, error) {
	var one int
	err := s.pg.QueryRow(ctx, `SELECT 1 FROM connector_syncs WHERE project_id = $1 AND id = $2`, projectID, syncID).Scan(&one)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func validateSyncInput(in ConnectorSyncInput) error {
	if strings.TrimSpace(in.SourceTable) == "" {
		return fmt.Errorf("source table is required")
	}
	if strings.TrimSpace(in.KeyColumn) == "" {
		return fmt.Errorf("key column is required")
	}
	if in.ScheduleCron != "" && len(strings.Fields(in.ScheduleCron)) != 5 {
		return fmt.Errorf("schedule must be a 5-field cron expression")
	}
	switch in.DeletionMode {
	case "", "none":
		if in.SoftDeleteColumn != "" || in.SoftDeleteSemantics != "" {
			return fmt.Errorf("soft_delete_column requires deletion_mode 'soft_column'")
		}
	case "soft_column":
		if strings.TrimSpace(in.SoftDeleteColumn) == "" {
			return fmt.Errorf("soft_delete_column is required when deletion_mode is 'soft_column'")
		}
		if in.SoftDeleteSemantics != "bool_true" && in.SoftDeleteSemantics != "non_null" {
			return fmt.Errorf("soft_delete_semantics must be 'bool_true' or 'non_null'")
		}
	default:
		return fmt.Errorf("deletion_mode must be 'none' or 'soft_column'")
	}
	return nil
}

func connectorKindKnown(kind string) bool {
	return slices.Contains(connector.Kinds(), kind)
}

func syncScanDest(cs *ConnectorSync) []any {
	return []any{&cs.ID, &cs.ConnectorID, &cs.ProjectID, &cs.SourceTable, &cs.KeyColumn,
		&cs.CursorColumn, &cs.ScheduleCron, &cs.Enabled, &cs.Cursor, &cs.CursorKey, &cs.LastRunAt, &cs.LastStatus,
		&cs.LastError, &cs.LastRows, &cs.TotalRows, &cs.LastSuccessAt, &cs.JoinKey, &cs.JoinValidated,
		&cs.DeletionMode, &cs.SoftDeleteColumn, &cs.SoftDeleteSemantics, &cs.LandingSeq,
		&cs.Revision, &cs.CreatedAt, &cs.UpdatedAt}
}

// --- engine surface (connector.Store) ---

// ListEnabledConnectorSyncs returns every enabled sync with a schedule, for
// the engine's minute tick.
func (s *Store) ListEnabledConnectorSyncs(ctx context.Context) ([]connector.ScheduledSync, error) {
	rows, err := s.pg.Query(ctx, `
SELECT id::text, project_id::text, schedule_cron FROM connector_syncs WHERE enabled AND schedule_cron != ''`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]connector.ScheduledSync, 0)
	for rows.Next() {
		var ss connector.ScheduledSync
		if err := rows.Scan(&ss.ID, &ss.ProjectID, &ss.Cron); err != nil {
			return nil, err
		}
		out = append(out, ss)
	}
	return out, rows.Err()
}

// ConnectorSyncJob resolves one sync into a runnable job, decrypting the DSN.
// Internal run path — authorization happens at the API edge.
func (s *Store) ConnectorSyncJob(ctx context.Context, syncID string) (connector.SyncJob, error) {
	var job connector.SyncJob
	var ciphertext, credentialID string
	err := s.pg.QueryRow(ctx, `
SELECT cs.id::text, cs.project_id::text, cs.connector_id::text, dc.kind, dc.dsn_ciphertext,
	COALESCE(dc.credential_id::text, ''),
	cs.source_table, cs.key_column, cs.cursor_column, cs.cursor, cs.cursor_key
FROM connector_syncs cs
JOIN data_connectors dc ON dc.id = cs.connector_id
WHERE cs.id = $1`, syncID).
		Scan(&job.SyncID, &job.ProjectID, &job.ConnectorID, &job.Kind, &ciphertext, &credentialID,
			&job.Table, &job.KeyColumn, &job.CursorColumn, &job.Cursor, &job.CursorKey)
	if err != nil {
		return connector.SyncJob{}, err
	}
	if credentialID != "" {
		job.DSN, err = s.sourceCredentialDSN(ctx, s.pg, job.ProjectID, credentialID)
		if err != nil {
			return connector.SyncJob{}, err
		}
		return job, nil
	}
	job.DSN, err = decryptAgentKey(ciphertext)
	if err != nil {
		return connector.SyncJob{}, err
	}
	return job, nil
}

// InsertExternalRows lands one batch in the ClickHouse external_rows table.
// The table is a ReplacingMergeTree keyed by (project, connector, table, row),
// so re-landing the same rows (snapshot mode, retried batches) deduplicates on
// merge instead of accumulating.
func (s *Store) InsertExternalRows(ctx context.Context, projectID, connectorID, table string, rows []connector.LandedRow, version uint64) error {
	if len(rows) == 0 {
		return nil
	}
	pid, err := uuid.Parse(projectID)
	if err != nil {
		return err
	}
	cid, err := uuid.Parse(connectorID)
	if err != nil {
		return err
	}
	batch, err := s.ch.PrepareBatch(ctx, `INSERT INTO external_rows (project_id, connector_id, table_name, row_key, cursor, data, synced_at, version)`)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	for _, r := range rows {
		if err := batch.Append(pid, cid, table, r.Key, r.Cursor, r.DataJSON, now, version); err != nil {
			return err
		}
	}
	return batch.Send()
}
