package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/lohi-ai/agentray/internal/dataplane/connector"
)

const snapshotGenerationColumns = `project_id::text,connector_id::text,table_name,sync_id::text,generation::text,generation_seq,
binding_digest,state,key_position,next_batch_index,row_count,capture_started_at,capture_finished_at,created_at,updated_at,terminal_at,
run_id::text,owner,lease_epoch`

func snapshotGenerationDest(g *connector.SnapshotGeneration) []any {
	return []any{&g.ProjectID, &g.ConnectorID, &g.Table, &g.SyncID, &g.Generation, &g.GenerationSeq,
		&g.BindingDigest, &g.State, &g.KeyPosition, &g.NextBatchIndex, &g.Rows, &g.CaptureStartedAt,
		&g.CaptureFinishedAt, &g.CreatedAt, &g.UpdatedAt, &g.TerminalAt, &g.RunID, &g.Owner, &g.LeaseEpoch}
}

func (s *Store) migrateConnectorSnapshots(ctx context.Context) error {
	for _, stmt := range []string{
		`CREATE TABLE IF NOT EXISTS connector_snapshot_generations (
	project_id UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
	connector_id UUID NOT NULL REFERENCES data_connectors(id) ON DELETE CASCADE,
	table_name VARCHAR(256) NOT NULL,
	sync_id UUID NOT NULL REFERENCES connector_syncs(id) ON DELETE CASCADE,
	generation UUID NOT NULL,
	generation_seq BIGINT NOT NULL,
	binding_digest VARCHAR(64) NOT NULL,
	state VARCHAR(16) NOT NULL,
	key_position TEXT NOT NULL DEFAULT '',
	next_batch_index BIGINT NOT NULL DEFAULT 0,
	row_count BIGINT NOT NULL DEFAULT 0,
	capture_started_at TIMESTAMPTZ NOT NULL,
	capture_finished_at TIMESTAMPTZ,
	created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	terminal_at TIMESTAMPTZ,
	run_id UUID NOT NULL,
	owner TEXT NOT NULL,
	lease_epoch BIGINT NOT NULL,
	CHECK (generation_seq > 0),
	CHECK (next_batch_index >= 0 AND row_count >= 0),
	CHECK (state IN ('capturing','yielded','sealed','failed','cancelled')),
	PRIMARY KEY (project_id, connector_id, table_name, generation),
	UNIQUE (project_id, connector_id, table_name, generation_seq)
)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS connector_snapshot_one_resumable
	ON connector_snapshot_generations(project_id,connector_id,table_name)
	WHERE state IN ('capturing','yielded')`,
		`CREATE INDEX IF NOT EXISTS connector_snapshot_sync_idx ON connector_snapshot_generations(sync_id,created_at DESC)`,
		`CREATE TABLE IF NOT EXISTS connector_snapshot_outbox (
	id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
	project_id UUID NOT NULL,
	connector_id UUID NOT NULL,
	table_name VARCHAR(256) NOT NULL,
	generation UUID NOT NULL,
	batch_id VARCHAR(128) NOT NULL,
	batch_index BIGINT NOT NULL,
	kind VARCHAR(16) NOT NULL,
	payload BYTEA NOT NULL,
	payload_sha256 VARCHAR(64) NOT NULL DEFAULT '',
	lower_key TEXT NOT NULL DEFAULT '',
	upper_key TEXT NOT NULL DEFAULT '',
	row_count INT NOT NULL DEFAULT 0,
	published BOOLEAN NOT NULL DEFAULT false,
	created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	published_at TIMESTAMPTZ,
	CHECK (batch_index >= 0 AND row_count >= 0),
	CHECK (kind IN ('batch','complete')),
	UNIQUE (project_id,connector_id,table_name,generation,kind,batch_id)
)`,
		`ALTER TABLE connector_snapshot_outbox ADD COLUMN IF NOT EXISTS lower_key TEXT NOT NULL DEFAULT ''`,
		`CREATE INDEX IF NOT EXISTS connector_snapshot_outbox_pending_idx ON connector_snapshot_outbox(generation,batch_index) WHERE NOT published`,
		`CREATE UNIQUE INDEX IF NOT EXISTS connector_snapshot_outbox_batch_index_idx ON connector_snapshot_outbox(project_id,connector_id,table_name,generation,kind,batch_index)`,
	} {
		if _, err := s.pg.Exec(ctx, stmt); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) ClaimSnapshotGeneration(ctx context.Context, job connector.SyncJob, runID, owner string, leaseEpoch int64) (connector.SnapshotGeneration, error) {
	if job.SourceBinding == nil || job.SyncMode != "snapshot" {
		return connector.SnapshotGeneration{}, fmt.Errorf("snapshot source binding is unavailable")
	}
	bindingDigest := job.SourceBinding.Digest("snapshot")
	tx, err := s.pg.Begin(ctx)
	if err != nil {
		return connector.SnapshotGeneration{}, err
	}
	defer tx.Rollback(ctx)
	if err := requireSnapshotRunFence(ctx, tx, runID, job.SyncID, owner, leaseEpoch); err != nil {
		return connector.SnapshotGeneration{}, err
	}
	var g connector.SnapshotGeneration
	err = tx.QueryRow(ctx, `SELECT `+snapshotGenerationColumns+` FROM connector_snapshot_generations g
WHERE g.project_id=$1 AND g.connector_id=$2 AND g.table_name=$3 AND
(g.state IN ('capturing','yielded') OR (g.state='sealed' AND EXISTS(SELECT 1 FROM connector_snapshot_outbox o WHERE o.generation=g.generation AND NOT o.published)))
ORDER BY g.generation_seq DESC LIMIT 1 FOR UPDATE`, job.ProjectID, job.ConnectorID, job.Table).Scan(snapshotGenerationDest(&g)...)
	if err == nil {
		if g.SyncID != job.SyncID || g.BindingDigest != bindingDigest {
			return connector.SnapshotGeneration{}, fmt.Errorf("live snapshot generation binding changed; cancel it before reconfiguration")
		}
		if _, err := tx.Exec(ctx, `UPDATE connector_snapshot_generations SET run_id=$2,owner=$3,lease_epoch=$4,
state=CASE WHEN state='yielded' THEN 'capturing' ELSE state END,updated_at=now() WHERE generation=$1`, g.Generation, runID, owner, leaseEpoch); err != nil {
			return connector.SnapshotGeneration{}, err
		}
		g.RunID, g.Owner, g.LeaseEpoch = runID, owner, leaseEpoch
		if g.State == "yielded" {
			g.State = "capturing"
		}
		return g, tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return connector.SnapshotGeneration{}, err
	}
	var next int64
	if err := tx.QueryRow(ctx, `SELECT COALESCE(max(generation_seq),0)+1 FROM connector_snapshot_generations
WHERE project_id=$1 AND connector_id=$2 AND table_name=$3`, job.ProjectID, job.ConnectorID, job.Table).Scan(&next); err != nil {
		return connector.SnapshotGeneration{}, err
	}
	generation := uuid.NewString()
	err = tx.QueryRow(ctx, `INSERT INTO connector_snapshot_generations
(project_id,connector_id,table_name,sync_id,generation,generation_seq,binding_digest,state,capture_started_at,run_id,owner,lease_epoch)
VALUES($1,$2,$3,$4,$5,$6,$7,'capturing',now(),$8,$9,$10) RETURNING `+snapshotGenerationColumns,
		job.ProjectID, job.ConnectorID, job.Table, job.SyncID, generation, next, bindingDigest, runID, owner, leaseEpoch).Scan(snapshotGenerationDest(&g)...)
	if err != nil {
		return connector.SnapshotGeneration{}, err
	}
	return g, tx.Commit(ctx)
}

func requireSnapshotRunFence(ctx context.Context, tx pgx.Tx, runID, syncID, owner string, leaseEpoch int64) error {
	var ok bool
	// Lock the run row for the lifetime of the generation write. In particular,
	// this makes seal and CancelConnectorRun serialize on the same row: either
	// cancellation is visible here and no marker is created, or sealing commits
	// first and CancelConnectorRun's sealed-generation predicate refuses it.
	err := tx.QueryRow(ctx, `SELECT status='running' AND owner=$3 AND lease_epoch=$4 AND NOT cancel_requested
AND heartbeat_at > now()-interval '2 minutes' FROM connector_runs WHERE id=$1 AND sync_id=$2 FOR UPDATE`,
		runID, syncID, owner, leaseEpoch).Scan(&ok)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("snapshot generation lease is stale or cancelled")
	}
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("snapshot generation lease is stale or cancelled")
	}
	return nil
}

func (s *Store) PendingSnapshotOutbox(ctx context.Context, g connector.SnapshotGeneration) ([]connector.SnapshotOutbox, error) {
	rows, err := s.pg.Query(ctx, `SELECT id::text,generation::text,batch_id,batch_index,kind,payload,lower_key,upper_key,row_count,published
FROM connector_snapshot_outbox WHERE generation=$1 AND NOT published ORDER BY batch_index,kind`, g.Generation)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []connector.SnapshotOutbox{}
	for rows.Next() {
		var item connector.SnapshotOutbox
		if err := rows.Scan(&item.ID, &item.Generation, &item.BatchID, &item.BatchIndex, &item.Kind, &item.Payload, &item.LowerKey, &item.UpperKey, &item.Rows, &item.Published); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func (s *Store) PrepareSnapshotEnvelope(ctx context.Context, g connector.SnapshotGeneration, env connector.SnapshotEnvelope, lowerKey, upperKey string) (connector.SnapshotOutbox, error) {
	payload, err := connector.MarshalSnapshotEnvelope(env)
	if err != nil {
		return connector.SnapshotOutbox{}, err
	}
	if env.Generation != g.Generation || env.GenerationSeq != g.GenerationSeq || env.BindingDigest != g.BindingDigest {
		return connector.SnapshotOutbox{}, fmt.Errorf("snapshot envelope identity does not match claimed generation")
	}
	tx, err := s.pg.Begin(ctx)
	if err != nil {
		return connector.SnapshotOutbox{}, err
	}
	defer tx.Rollback(ctx)
	if err := requireSnapshotRunFence(ctx, tx, g.RunID, g.SyncID, g.Owner, g.LeaseEpoch); err != nil {
		return connector.SnapshotOutbox{}, err
	}
	if g.State != "capturing" {
		return connector.SnapshotOutbox{}, fmt.Errorf("snapshot generation is not capturing")
	}
	batchID := env.BatchID
	if env.Kind == connector.SnapshotKindComplete {
		batchID = "complete"
	}
	var out connector.SnapshotOutbox
	err = tx.QueryRow(ctx, `INSERT INTO connector_snapshot_outbox
(project_id,connector_id,table_name,generation,batch_id,batch_index,kind,payload,payload_sha256,lower_key,upper_key,row_count)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
ON CONFLICT(project_id,connector_id,table_name,generation,kind,batch_id) DO UPDATE SET batch_id=EXCLUDED.batch_id
RETURNING id::text,generation::text,batch_id,batch_index,kind,payload,lower_key,upper_key,row_count,published`,
		g.ProjectID, g.ConnectorID, g.Table, g.Generation, batchID, env.BatchIndex, env.Kind, payload, env.PayloadSHA256, lowerKey, upperKey, len(env.Rows)).
		Scan(&out.ID, &out.Generation, &out.BatchID, &out.BatchIndex, &out.Kind, &out.Payload, &out.LowerKey, &out.UpperKey, &out.Rows, &out.Published)
	if err != nil {
		return connector.SnapshotOutbox{}, err
	}
	if string(out.Payload) != string(payload) || out.LowerKey != lowerKey || out.UpperKey != upperKey {
		return connector.SnapshotOutbox{}, fmt.Errorf("snapshot outbox identity conflicts with immutable payload")
	}
	return out, tx.Commit(ctx)
}

func (s *Store) SealSnapshotGeneration(ctx context.Context, g connector.SnapshotGeneration, env connector.SnapshotEnvelope, runRows int) (connector.SnapshotOutbox, error) {
	if env.Kind != connector.SnapshotKindComplete {
		return connector.SnapshotOutbox{}, fmt.Errorf("snapshot seal requires completion envelope")
	}
	payload, err := connector.MarshalSnapshotEnvelope(env)
	if err != nil {
		return connector.SnapshotOutbox{}, err
	}
	tx, err := s.pg.Begin(ctx)
	if err != nil {
		return connector.SnapshotOutbox{}, err
	}
	defer tx.Rollback(ctx)
	if err := requireSnapshotRunFence(ctx, tx, g.RunID, g.SyncID, g.Owner, g.LeaseEpoch); err != nil {
		return connector.SnapshotOutbox{}, err
	}
	var out connector.SnapshotOutbox
	err = tx.QueryRow(ctx, `INSERT INTO connector_snapshot_outbox
(project_id,connector_id,table_name,generation,batch_id,batch_index,kind,payload,row_count)
VALUES($1,$2,$3,$4,'complete',$5,'complete',$6,0)
ON CONFLICT(project_id,connector_id,table_name,generation,kind,batch_id) DO UPDATE SET batch_id=EXCLUDED.batch_id
RETURNING id::text,generation::text,batch_id,batch_index,kind,payload,lower_key,upper_key,row_count,published`,
		g.ProjectID, g.ConnectorID, g.Table, g.Generation, env.ExpectedBatches, payload).
		Scan(&out.ID, &out.Generation, &out.BatchID, &out.BatchIndex, &out.Kind, &out.Payload, &out.LowerKey, &out.UpperKey, &out.Rows, &out.Published)
	if err != nil {
		return connector.SnapshotOutbox{}, err
	}
	if string(out.Payload) != string(payload) {
		return connector.SnapshotOutbox{}, fmt.Errorf("snapshot completion conflicts with immutable payload")
	}
	tag, err := tx.Exec(ctx, `UPDATE connector_snapshot_generations SET state='sealed',capture_finished_at=$2,terminal_at=now(),updated_at=now()
WHERE generation=$1 AND state='capturing' AND run_id=$3 AND owner=$4 AND lease_epoch=$5`, g.Generation, *env.CaptureFinishedAt, g.RunID, g.Owner, g.LeaseEpoch)
	if err != nil {
		return connector.SnapshotOutbox{}, err
	}
	if tag.RowsAffected() != 1 {
		return connector.SnapshotOutbox{}, fmt.Errorf("snapshot generation could not be sealed")
	}
	_ = runRows // folded by FinishConnectorRun after the completion is published.
	return out, tx.Commit(ctx)
}

func (s *Store) MarkSnapshotOutboxPublished(ctx context.Context, g connector.SnapshotGeneration, item connector.SnapshotOutbox) error {
	tx, err := s.pg.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var published bool
	var kind, upper string
	var rowCount int
	if err := tx.QueryRow(ctx, `SELECT published,kind,upper_key,row_count FROM connector_snapshot_outbox WHERE id=$1 FOR UPDATE`, item.ID).Scan(&published, &kind, &upper, &rowCount); err != nil {
		return err
	}
	if !published {
		if _, err := tx.Exec(ctx, `UPDATE connector_snapshot_outbox SET published=true,published_at=now() WHERE id=$1`, item.ID); err != nil {
			return err
		}
		if kind == connector.SnapshotKindBatch {
			if _, err := tx.Exec(ctx, `UPDATE connector_snapshot_generations SET key_position=$2,next_batch_index=GREATEST(next_batch_index,$3),row_count=row_count+$4,updated_at=now()
WHERE generation=$1 AND next_batch_index <= $3`, g.Generation, upper, item.BatchIndex+1, rowCount); err != nil {
				return err
			}
		}
	}
	return tx.Commit(ctx)
}

func (s *Store) YieldSnapshotGeneration(ctx context.Context, g connector.SnapshotGeneration) error {
	tx, err := s.pg.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := requireSnapshotRunFence(ctx, tx, g.RunID, g.SyncID, g.Owner, g.LeaseEpoch); err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `UPDATE connector_snapshot_generations SET state='yielded',updated_at=now() WHERE generation=$1 AND state='capturing' AND run_id=$2 AND owner=$3 AND lease_epoch=$4`, g.Generation, g.RunID, g.Owner, g.LeaseEpoch)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("snapshot generation could not yield")
	}
	return tx.Commit(ctx)
}

func (s *Store) SnapshotManifest(ctx context.Context, generation string) ([]connector.SnapshotManifestEntry, int64, error) {
	rows, err := s.pg.Query(ctx, `SELECT batch_index,batch_id,payload_sha256,row_count FROM connector_snapshot_outbox
	WHERE generation=$1 AND kind='batch' AND published ORDER BY batch_index`, generation)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	entries := []connector.SnapshotManifestEntry{}
	var total int64
	for rows.Next() {
		var e connector.SnapshotManifestEntry
		if err := rows.Scan(&e.Index, &e.BatchID, &e.PayloadSHA256, &e.RowCount); err != nil {
			return nil, 0, err
		}
		entries = append(entries, e)
		total += int64(e.RowCount)
	}
	return entries, total, rows.Err()
}

func (s *Store) SnapshotGeneration(ctx context.Context, generation string) (connector.SnapshotGeneration, error) {
	var g connector.SnapshotGeneration
	err := s.pg.QueryRow(ctx, `SELECT `+snapshotGenerationColumns+` FROM connector_snapshot_generations WHERE generation=$1`, generation).Scan(snapshotGenerationDest(&g)...)
	return g, err
}

func (s *Store) FailSnapshotGeneration(ctx context.Context, g connector.SnapshotGeneration) error {
	tag, err := s.pg.Exec(ctx, `UPDATE connector_snapshot_generations SET state='failed',terminal_at=now(),updated_at=now()
WHERE generation=$1 AND state IN ('capturing','yielded') AND run_id=$2 AND owner=$3 AND lease_epoch=$4`, g.Generation, g.RunID, g.Owner, g.LeaseEpoch)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("snapshot generation failure was fenced")
	}
	return nil
}

func (s *Store) CancelSnapshotGeneration(ctx context.Context, runID, owner string, leaseEpoch int64) error {
	_, err := s.pg.Exec(ctx, `UPDATE connector_snapshot_generations SET state='cancelled',terminal_at=now(),updated_at=now()
WHERE run_id=$1 AND owner=$2 AND lease_epoch=$3 AND state IN ('capturing','yielded')`, runID, owner, leaseEpoch)
	return err
}

func (s *Store) ApplySnapshotEnvelope(ctx context.Context, env connector.SnapshotEnvelope, mark AppliedMark) (*connector.SnapshotPromotion, error) {
	if s.duck == nil {
		return nil, errors.New("storage: duckdb not open")
	}
	return s.duck.ApplySnapshotEnvelope(ctx, env, mark)
}

func (s *Store) SnapshotPromotion(ctx context.Context, projectID, connectorID, table string) (connector.SnapshotPromotion, error) {
	if s.duck == nil {
		return connector.SnapshotPromotion{}, errors.New("storage: duckdb not open")
	}
	return s.duck.SnapshotPromotion(ctx, projectID, connectorID, table)
}

// SnapshotGenerationDescriptors is the C2 handoff. Resumability and pending
// outbox are producer facts; active is resolved against this Store's DuckDB
// promotion record and is therefore never copied into the producer wire.
func (s *Store) SnapshotGenerationDescriptors(ctx context.Context, projectID, connectorID, table string) ([]connector.SnapshotGenerationDescriptor, error) {
	rows, err := s.pg.Query(ctx, `SELECT `+snapshotGenerationColumns+`,EXISTS(SELECT 1 FROM connector_snapshot_outbox o WHERE o.generation=g.generation AND NOT o.published)
FROM connector_snapshot_generations g WHERE project_id=$1 AND connector_id=$2 AND table_name=$3 ORDER BY generation_seq DESC`, projectID, connectorID, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	activeGeneration := ""
	if p, perr := s.SnapshotPromotion(ctx, projectID, connectorID, table); perr == nil {
		activeGeneration = p.Generation
	}
	out := []connector.SnapshotGenerationDescriptor{}
	for rows.Next() {
		var d connector.SnapshotGenerationDescriptor
		if err := rows.Scan(append(snapshotGenerationDest(&d.SnapshotGeneration), &d.HasUnpublishedOutbox)...); err != nil {
			return nil, err
		}
		d.Resumable = d.State == "capturing" || d.State == "yielded"
		d.IsActiveOnThisStore = d.Generation == activeGeneration
		out = append(out, d)
	}
	return out, rows.Err()
}

// LatestSnapshotGenerationsForSyncs batches source_status enrichment so a
// connector page does not issue one Postgres query per sync. Active is joined
// in memory from one DuckDB promotion scan for this project.
func (s *Store) LatestSnapshotGenerationsForSyncs(ctx context.Context, projectID string, syncIDs []string) (map[string]connector.SnapshotGenerationDescriptor, error) {
	out := map[string]connector.SnapshotGenerationDescriptor{}
	if len(syncIDs) == 0 {
		return out, nil
	}
	rows, err := s.pg.Query(ctx, `SELECT DISTINCT ON (g.sync_id) `+snapshotGenerationColumns+`,
EXISTS(SELECT 1 FROM connector_snapshot_outbox o WHERE o.generation=g.generation AND NOT o.published)
FROM connector_snapshot_generations g WHERE g.project_id=$1 AND g.sync_id=ANY($2::uuid[]) ORDER BY g.sync_id,g.generation_seq DESC`, projectID, syncIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var d connector.SnapshotGenerationDescriptor
		if err := rows.Scan(append(snapshotGenerationDest(&d.SnapshotGeneration), &d.HasUnpublishedOutbox)...); err != nil {
			return nil, err
		}
		d.Resumable = d.State == "capturing" || d.State == "yielded"
		out[d.SyncID] = d
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if s.duck == nil {
		return out, nil
	}
	active := map[string]struct{}{}
	err = s.duck.Read(ctx, func(conn *sql.Conn) error {
		rows, err := conn.QueryContext(ctx, `SELECT generation::VARCHAR FROM connector_snapshot_promotions WHERE project_id=?`, projectID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var generation string
			if err := rows.Scan(&generation); err != nil {
				return err
			}
			active[generation] = struct{}{}
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	for id, d := range out {
		_, d.IsActiveOnThisStore = active[d.Generation]
		out[id] = d
	}
	return out, nil
}
