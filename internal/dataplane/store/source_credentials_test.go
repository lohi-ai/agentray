package storage

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
)

// Live tests for write-only source credentials and credential-referenced
// connectors. Needs the compose Postgres; skips without one.

func TestSourceCredentialLifecycle(t *testing.T) {
	s := openConvTestStore(t)
	t.Setenv("AGENT_KEY_ENC_SECRET", "source-cred-test-secret")
	ctx := context.Background()
	userID, projectID := seedConvProject(t, s)

	// Create stores encrypted; the returned row carries no secret.
	cred, err := s.CreateSourceCredential(ctx, userID, projectID, "prod-pg", "postgres://u:p@h:5432/db")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if cred.ID == "" || cred.RevokedAt != nil {
		t.Fatalf("cred = %+v", cred)
	}
	// The credential row exists (metadata only — the ciphertext column is never
	// selected by any read path).
	var credCount int
	if err := s.pg.QueryRow(ctx, `SELECT count(*) FROM source_credentials WHERE project_id = $1`, projectID).Scan(&credCount); err != nil || credCount != 1 {
		t.Fatalf("credential count = %d %v", credCount, err)
	}

	// A connector referencing the credential resolves its DSN at run time.
	dc, err := s.CreateDataConnectorIdempotent(ctx, projectID, "warehouse", "postgres", cred.ID, "ck1", "h1")
	if err != nil {
		t.Fatalf("create connector: %v", err)
	}
	if !dc.HasDSN {
		t.Fatal("connector should report has_dsn")
	}
	kind, dsn, err := s.ConnectorDSNForProject(ctx, projectID, dc.ID)
	if err != nil || kind != "postgres" || dsn != "postgres://u:p@h:5432/db" {
		t.Fatalf("dsn resolve = %q %q %v", kind, dsn, err)
	}

	// Idempotent replay returns the same connector, no duplicate.
	replay, err := s.CreateDataConnectorIdempotent(ctx, projectID, "warehouse", "postgres", cred.ID, "ck1", "h1")
	if err != nil || replay.ID != dc.ID {
		t.Fatalf("replay = %+v %v", replay, err)
	}
	all, _ := s.ListDataConnectorsForProject(ctx, projectID)
	if len(all) != 1 {
		t.Fatalf("duplicate connector created: %d", len(all))
	}

	// Update with omitted credential preserves it; revision bumps once.
	newName := "wh2"
	updated, err := s.UpdateDataConnectorIdempotent(ctx, projectID, dc.ID, &newName, nil, 1, "uk1", "h2")
	if err != nil || updated.Name != "wh2" {
		t.Fatalf("update = %+v %v", updated, err)
	}
	if _, dsn2, err := s.ConnectorDSNForProject(ctx, projectID, dc.ID); err != nil || dsn2 != dsn {
		t.Fatalf("credential not preserved: %v", err)
	}
	// Stale revision conflicts.
	if _, err := s.UpdateDataConnectorIdempotent(ctx, projectID, dc.ID, &newName, nil, 1, "uk2", "h3"); !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("stale update = %v, want ErrRevisionConflict", err)
	}

	// Revoked credential: existing connector fails closed at resolve time.
	if err := s.RevokeSourceCredential(ctx, userID, projectID, cred.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if _, _, err := s.ConnectorDSNForProject(ctx, projectID, dc.ID); err == nil {
		t.Fatal("revoked credential still resolves a DSN")
	}
	// A revoked credential cannot back a new connector.
	if _, err := s.CreateDataConnectorIdempotent(ctx, projectID, "x", "postgres", cred.ID, "ck2", "h4"); err == nil {
		t.Fatal("revoked credential accepted for create")
	}
	// Cross-project credential reference fails closed.
	otherUser, otherProject := seedConvProject(t, s)
	cred2, err := s.CreateSourceCredential(ctx, otherUser, otherProject, "other", "postgres://o:p@h/db")
	if err != nil {
		t.Fatalf("other cred: %v", err)
	}
	if _, err := s.CreateDataConnectorIdempotent(ctx, projectID, "y", "postgres", cred2.ID, "ck3", "h5"); err == nil {
		t.Fatal("foreign credential accepted")
	}
	// Unknown connector id is not-found.
	if _, _, err := s.ConnectorDSNForProject(ctx, projectID, "00000000-0000-0000-0000-000000000000"); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("missing connector err = %v, want ErrNoRows", err)
	}
}

// The web's DSN entry is a single idempotent transaction: invalid input creates
// neither row, and retrying the same key after a lost response replays exactly
// one connector/credential pair instead of accumulating orphan credentials.
func TestCreateSourceConnectorIdempotent(t *testing.T) {
	s := openConvTestStore(t)
	t.Setenv("AGENT_KEY_ENC_SECRET", "atomic-source-connector-test-secret")
	ctx := context.Background()
	userID, projectID := seedConvProject(t, s)

	if _, err := s.CreateSourceConnectorIdempotent(ctx, userID, projectID, "bad", "unknown", "postgres://u:p@h/db", "bad-key"); err == nil {
		t.Fatal("unknown kind accepted")
	}
	var credCount int
	if err := s.pg.QueryRow(ctx, `SELECT count(*) FROM source_credentials WHERE project_id = $1`, projectID).Scan(&credCount); err != nil || credCount != 0 {
		t.Fatalf("invalid request left credential(s): %d %v", credCount, err)
	}

	first, err := s.CreateSourceConnectorIdempotent(ctx, userID, projectID, "warehouse", "postgres", "postgres://u:p@h/db", "retry-key")
	if err != nil {
		t.Fatalf("create atomic pair: %v", err)
	}
	replay, err := s.CreateSourceConnectorIdempotent(ctx, userID, projectID, "warehouse", "postgres", "postgres://u:p@h/db", "retry-key")
	if err != nil || replay.ID != first.ID {
		t.Fatalf("ambiguous-response replay = %+v %v", replay, err)
	}
	if err := s.pg.QueryRow(ctx, `SELECT count(*) FROM source_credentials WHERE project_id = $1`, projectID).Scan(&credCount); err != nil || credCount != 1 {
		t.Fatalf("replay created orphan credential(s): %d %v", credCount, err)
	}
	connectors, err := s.ListDataConnectorsForProject(ctx, projectID)
	if err != nil || len(connectors) != 1 || connectors[0].ID != first.ID {
		t.Fatalf("replay created connector(s): %+v %v", connectors, err)
	}
	if _, dsn, err := s.ConnectorDSNForProject(ctx, projectID, first.ID); err != nil || dsn != "postgres://u:p@h/db" {
		t.Fatalf("atomic connector DSN = %q %v", dsn, err)
	}
	if _, err := s.CreateSourceConnectorIdempotent(ctx, userID, projectID, "warehouse", "postgres", "postgres://changed@h/db", "retry-key"); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("key reused for changed DSN = %v, want idempotency conflict", err)
	}
}
