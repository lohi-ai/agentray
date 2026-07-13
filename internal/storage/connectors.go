package storage

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/lohi-ai/agentray/internal/connector"
)

// Data connectors (parent plan bs-eano39vq §1): operator-configured external
// data sources whose rows are pulled into the ClickHouse external_rows landing
// table on a schedule, where run_sql can query them next to events. The DSN is
// AES-encrypted with the same agentEncKey path as agent secrets and is
// write-only over the API — list/read surfaces return only its presence.

// DataConnector is one configured source connection (DSN never included).
type DataConnector struct {
	ID        string    `json:"id"`
	ProjectID string    `json:"project_id"`
	Name      string    `json:"name"`
	Kind      string    `json:"kind"`
	HasDSN    bool      `json:"has_dsn"`
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
	LastRunAt    *time.Time `json:"last_run_at"`
	LastStatus   string     `json:"last_status"`
	LastError    string     `json:"last_error"`
	LastRows     int        `json:"last_rows"`
	TotalRows    int64      `json:"total_rows"`
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
}

const connectorSyncColumns = `id::text, connector_id::text, project_id::text, source_table, key_column,
	cursor_column, schedule_cron, enabled, cursor, last_run_at, last_status, last_error, last_rows, total_rows,
	created_at, updated_at`

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
RETURNING id::text, project_id::text, name, kind, dsn_ciphertext != '', created_at, updated_at`,
		projectID, name, kind, ciphertext).
		Scan(&out.ID, &out.ProjectID, &out.Name, &out.Kind, &out.HasDSN, &out.CreatedAt, &out.UpdatedAt)
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
SELECT id::text, project_id::text, name, kind, dsn_ciphertext != '', created_at, updated_at
FROM data_connectors WHERE project_id = $1 ORDER BY created_at DESC`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]DataConnector, 0)
	for rows.Next() {
		var c DataConnector
		if err := rows.Scan(&c.ID, &c.ProjectID, &c.Name, &c.Kind, &c.HasDSN, &c.CreatedAt, &c.UpdatedAt); err != nil {
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
	var ciphertext string
	err = s.pg.QueryRow(ctx, `
SELECT kind, dsn_ciphertext FROM data_connectors WHERE project_id = $1 AND id = $2`,
		projectID, connectorID).Scan(&kind, &ciphertext)
	if err != nil {
		return "", "", err
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
INSERT INTO connector_syncs (connector_id, project_id, source_table, key_column, cursor_column, schedule_cron, enabled)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING `+connectorSyncColumns,
		connectorID, projectID, in.SourceTable, in.KeyColumn, in.CursorColumn, in.ScheduleCron, in.Enabled).
		Scan(syncScanDest(&out)...)
	if err != nil {
		return ConnectorSync{}, err
	}
	_ = s.recordWorkspaceAudit(ctx, project.WorkspaceID, userID, "connector.sync.create", "project", project.ID, project.Name, "{}")
	return out, nil
}

// UpdateConnectorSync overwrites a sync's editable fields (owner/admin only).
// Changing the source table or cursor column resets the cursor — the old
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
	source_table = $3, key_column = $4, cursor_column = $5, schedule_cron = $6, enabled = $7,
	cursor = CASE WHEN source_table = $3 AND cursor_column = $5 THEN cursor ELSE '' END,
	updated_at = now()
WHERE project_id = $1 AND id = $2
RETURNING `+connectorSyncColumns,
		projectID, syncID, in.SourceTable, in.KeyColumn, in.CursorColumn, in.ScheduleCron, in.Enabled).
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
	return nil
}

func connectorKindKnown(kind string) bool {
	for _, k := range connector.Kinds() {
		if k == kind {
			return true
		}
	}
	return false
}

func syncScanDest(cs *ConnectorSync) []any {
	return []any{&cs.ID, &cs.ConnectorID, &cs.ProjectID, &cs.SourceTable, &cs.KeyColumn,
		&cs.CursorColumn, &cs.ScheduleCron, &cs.Enabled, &cs.Cursor, &cs.LastRunAt, &cs.LastStatus,
		&cs.LastError, &cs.LastRows, &cs.TotalRows, &cs.CreatedAt, &cs.UpdatedAt}
}

// --- engine surface (connector.Store) ---

// ListEnabledConnectorSyncs returns every enabled sync with a schedule, for
// the engine's minute tick.
func (s *Store) ListEnabledConnectorSyncs(ctx context.Context) ([]connector.ScheduledSync, error) {
	rows, err := s.pg.Query(ctx, `
SELECT id::text, schedule_cron FROM connector_syncs WHERE enabled AND schedule_cron != ''`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]connector.ScheduledSync, 0)
	for rows.Next() {
		var ss connector.ScheduledSync
		if err := rows.Scan(&ss.ID, &ss.Cron); err != nil {
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
	var ciphertext string
	err := s.pg.QueryRow(ctx, `
SELECT cs.id::text, cs.project_id::text, cs.connector_id::text, dc.kind, dc.dsn_ciphertext,
	cs.source_table, cs.key_column, cs.cursor_column, cs.cursor
FROM connector_syncs cs
JOIN data_connectors dc ON dc.id = cs.connector_id
WHERE cs.id = $1`, syncID).
		Scan(&job.SyncID, &job.ProjectID, &job.ConnectorID, &job.Kind, &ciphertext,
			&job.Table, &job.KeyColumn, &job.CursorColumn, &job.Cursor)
	if err != nil {
		return connector.SyncJob{}, err
	}
	job.DSN, err = decryptAgentKey(ciphertext)
	if err != nil {
		return connector.SyncJob{}, err
	}
	return job, nil
}

// FinishConnectorSync persists one run's outcome. The error text is truncated
// defensively; sources are responsible for never leaking credentials into it.
func (s *Store) FinishConnectorSync(ctx context.Context, syncID string, result connector.SyncResult) error {
	status := "ok"
	errText := result.Err
	if errText != "" {
		status = "error"
		if len(errText) > 500 {
			errText = errText[:500]
		}
	}
	_, err := s.pg.Exec(ctx, `
UPDATE connector_syncs SET
	cursor = CASE WHEN $2 != '' THEN $2 ELSE cursor END,
	last_run_at = now(), last_status = $3, last_error = $4, last_rows = $5,
	total_rows = total_rows + $5, updated_at = now()
WHERE id = $1`, syncID, result.Cursor, status, errText, result.Rows)
	return err
}

// InsertExternalRows lands one batch in the ClickHouse external_rows table.
// The table is a ReplacingMergeTree keyed by (project, connector, table, row),
// so re-landing the same rows (snapshot mode, retried batches) deduplicates on
// merge instead of accumulating.
func (s *Store) InsertExternalRows(ctx context.Context, projectID, connectorID, table string, rows []connector.LandedRow) error {
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
	batch, err := s.ch.PrepareBatch(ctx, `INSERT INTO external_rows (project_id, connector_id, table_name, row_key, cursor, data, synced_at)`)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	for _, r := range rows {
		if err := batch.Append(pid, cid, table, r.Key, r.Cursor, r.DataJSON, now); err != nil {
			return err
		}
	}
	return batch.Send()
}
