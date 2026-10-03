package ingestion

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/lohi-ai/agentray/internal/dataplane/connector"
	storage "github.com/lohi-ai/agentray/internal/dataplane/store"
)

func TestSnapshotEndToEndTwoStoresMutableNextGeneration(t *testing.T) {
	ctx := context.Background()
	raw, err := os.ReadFile(filepath.Join("testdata", "c1-wire-v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Legacy   ExternalRowsBatch            `json:"legacy_batch"`
		Batches  []connector.SnapshotEnvelope `json:"batches"`
		Complete connector.SnapshotEnvelope   `json:"complete"`
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}

	stores := make([]*storage.DuckDB, 0, 2)
	for _, name := range []string{"blue", "green"} {
		db, err := storage.OpenDuckDB(ctx, filepath.Join(t.TempDir(), name+".duckdb"))
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		stores = append(stores, db)
		if err := db.InsertExternalRows(ctx, fixture.Complete.ProjectID, fixture.Complete.ConnectorID, fixture.Complete.Table,
			[]connector.LandedRow{{Key: "old", DataJSON: `{"amount_vnd":1,"id":"old"}`}}, storage.AppliedMark{}); err != nil {
			t.Fatal(err)
		}
	}

	// Both delivery orders stay private until every immutable batch and the
	// completion marker are present.
	applySnapshotWire(t, stores[0], fixture.Batches[0])
	applySnapshotWire(t, stores[0], fixture.Complete)
	if keys := snapshotKeys(t, stores[0], fixture.Complete); len(keys) != 1 || keys[0] != "old" {
		t.Fatalf("blue exposed partial generation: %v", keys)
	}
	applySnapshotWire(t, stores[0], fixture.Batches[1])
	applySnapshotWire(t, stores[1], fixture.Batches[1])
	applySnapshotWire(t, stores[1], fixture.Batches[0])
	applySnapshotWire(t, stores[1], fixture.Complete)
	for i, db := range stores {
		if keys := snapshotKeys(t, db, fixture.Complete); len(keys) != 2 || keys[0] != "1" || keys[1] != "2" {
			t.Fatalf("store %d generation one keys=%v", i, keys)
		}
	}

	started := time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)
	finished := started.Add(time.Minute)
	rows := []connector.SnapshotRow{
		{Key: "1", Data: json.RawMessage(`{"amount_vnd":999,"id":1,"payment_status":"refunded"}`)},
		{Key: "3", Data: json.RawMessage(`{"amount_vnd":300000,"id":3,"payment_status":"completed"}`)},
	}
	payloadDigest, err := connector.SnapshotPayloadDigest(rows)
	if err != nil {
		t.Fatal(err)
	}
	batch := connector.SnapshotEnvelope{Protocol: connector.SnapshotProtocolV1, ProjectID: fixture.Complete.ProjectID,
		ConnectorID: fixture.Complete.ConnectorID, Table: fixture.Complete.Table, SyncID: fixture.Complete.SyncID,
		Generation: "00000000-0000-4000-8000-000000000008", GenerationSeq: 2, BindingDigest: fixture.Complete.BindingDigest,
		CaptureStartedAt: started, Kind: connector.SnapshotKindBatch, RunID: fixture.Complete.RunID,
		BatchID: "batch-000000", BatchIndex: 0, PayloadSHA256: payloadDigest, Rows: rows}
	manifestDigest, err := connector.SnapshotManifestDigest([]connector.SnapshotManifestEntry{{Index: 0, BatchID: batch.BatchID, PayloadSHA256: payloadDigest, RowCount: len(rows)}})
	if err != nil {
		t.Fatal(err)
	}
	complete := connector.SnapshotEnvelope{Protocol: connector.SnapshotProtocolV1, ProjectID: batch.ProjectID,
		ConnectorID: batch.ConnectorID, Table: batch.Table, SyncID: batch.SyncID, Generation: batch.Generation,
		GenerationSeq: batch.GenerationSeq, BindingDigest: batch.BindingDigest, CaptureStartedAt: started,
		CaptureFinishedAt: &finished, Kind: connector.SnapshotKindComplete, RunID: batch.RunID,
		ExpectedBatches: 1, ExpectedRows: int64(len(rows)), BatchManifestSHA256: manifestDigest}
	for i, db := range stores {
		applySnapshotWire(t, db, batch)
		applySnapshotWire(t, db, complete)
		keys := snapshotKeys(t, db, complete)
		if len(keys) != 2 || keys[0] != "1" || keys[1] != "3" {
			t.Fatalf("store %d generation two keys=%v", i, keys)
		}
		if amount := snapshotAmount(t, db, complete, "1"); amount != 999 {
			t.Fatalf("store %d updated amount=%d", i, amount)
		}
	}
}

func applySnapshotWire(t *testing.T, db *storage.DuckDB, env connector.SnapshotEnvelope) {
	t.Helper()
	raw, err := connector.MarshalSnapshotEnvelope(env)
	if err != nil {
		t.Fatal(err)
	}
	legacy, decoded, err := decodeConnectorEnvelope(raw)
	if err != nil || legacy != nil || decoded == nil {
		t.Fatalf("decode legacy=%v snapshot=%v err=%v", legacy, decoded, err)
	}
	if _, err := db.ApplySnapshotEnvelope(context.Background(), *decoded, storage.AppliedMark{}); err != nil {
		t.Fatal(err)
	}
}

func snapshotKeys(t *testing.T, db *storage.DuckDB, env connector.SnapshotEnvelope) []string {
	t.Helper()
	var keys []string
	err := db.Read(context.Background(), func(conn *sql.Conn) error {
		rows, err := conn.QueryContext(context.Background(), `SELECT row_key FROM external_rows WHERE project_id=? AND connector_id=? AND table_name=? ORDER BY row_key`, env.ProjectID, env.ConnectorID, env.Table)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var key string
			if err := rows.Scan(&key); err != nil {
				return err
			}
			keys = append(keys, key)
		}
		return rows.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	return keys
}

func snapshotAmount(t *testing.T, db *storage.DuckDB, env connector.SnapshotEnvelope, key string) int64 {
	t.Helper()
	var amount int64
	err := db.Read(context.Background(), func(conn *sql.Conn) error {
		return conn.QueryRowContext(context.Background(), `SELECT CAST(json_extract(data,'$.amount_vnd') AS BIGINT) FROM external_rows WHERE project_id=? AND connector_id=? AND table_name=? AND row_key=?`,
			env.ProjectID, env.ConnectorID, env.Table, key).Scan(&amount)
	})
	if err != nil {
		t.Fatal(err)
	}
	return amount
}
