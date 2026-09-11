package storage

import (
	"context"
	"testing"
)

// Live tests for the credential split (Option A): management credentials are
// hashed, scoped, revocable; the split flag flips a project key from legacy to
// capture-only; new projects are born split. Needs the compose Postgres
// (AGENTRAY_TEST_DATABASE_URL or localhost:5434); skips without one.

func TestCredentialLifecycle(t *testing.T) {
	s := openConvTestStore(t)
	ctx := context.Background()
	userID, projectID := seedConvProject(t, s)

	// seedConvProject inserts the row directly (no credential_split_at) — an
	// unsplit legacy project.
	split, err := s.ProjectCredentialSplit(ctx, projectID)
	if err != nil || split {
		t.Fatalf("fresh seeded project split = %v, %v — want unsplit", split, err)
	}

	// Create: secret shown once, hash stored, scopes validated.
	cred, secret, err := s.CreateProjectCredential(ctx, userID, projectID, "ci-agent", []string{"analytics:read", "sources:manage"})
	if err != nil {
		t.Fatalf("create credential: %v", err)
	}
	if secret == "" || len(secret) < 20 || secret[:4] != "agm_" {
		t.Fatalf("secret shape wrong: %q", secret)
	}
	if cred.KeyHint == "" || len(cred.KeyHint) != 4 {
		t.Fatalf("key hint = %q", cred.KeyHint)
	}

	// Resolve by secret: project + scopes, sources:manage implies read at the
	// app layer (store returns the raw set).
	res, err := s.CredentialBySecret(ctx, secret)
	if err != nil {
		t.Fatalf("resolve credential: %v", err)
	}
	if res.ProjectID != projectID || res.CredentialID != cred.ID {
		t.Fatalf("resolved to wrong project/credential: %+v", res)
	}
	if len(res.Scopes) != 2 {
		t.Fatalf("scopes = %v", res.Scopes)
	}

	// Unknown scope rejected at creation.
	if _, _, err := s.CreateProjectCredential(ctx, userID, projectID, "bad", []string{"root:all"}); err == nil {
		t.Fatal("unknown scope accepted")
	}

	// List shows metadata, never the secret.
	creds, err := s.ListProjectCredentials(ctx, userID, projectID)
	if err != nil || len(creds) != 1 {
		t.Fatalf("list = %v, %v", creds, err)
	}
	if creds[0].Name != "ci-agent" || creds[0].RevokedAt != nil {
		t.Fatalf("listed credential = %+v", creds[0])
	}

	// Split requires a live credential — covered by having one now.
	if err := s.MarkProjectCredentialSplit(ctx, userID, projectID); err != nil {
		t.Fatalf("split: %v", err)
	}
	split, err = s.ProjectCredentialSplit(ctx, projectID)
	if err != nil || !split {
		t.Fatalf("after split = %v, %v", split, err)
	}
	// Idempotent refusal: second split is a no-op error, not a state change.
	if err := s.MarkProjectCredentialSplit(ctx, userID, projectID); err == nil {
		t.Fatal("second split accepted")
	}

	// Revoke: immediate — the secret stops resolving.
	if err := s.RevokeProjectCredential(ctx, userID, projectID, cred.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if _, err := s.CredentialBySecret(ctx, secret); err == nil {
		t.Fatal("revoked credential still resolves")
	}
}

func TestSplitRequiresLiveCredential(t *testing.T) {
	s := openConvTestStore(t)
	ctx := context.Background()
	userID, projectID := seedConvProject(t, s)

	// No credential yet: split must refuse (it would brick management access).
	if err := s.MarkProjectCredentialSplit(ctx, userID, projectID); err == nil {
		t.Fatal("split without a live credential accepted")
	}
}

func TestNewProjectBornSplit(t *testing.T) {
	s := openConvTestStore(t)
	ctx := context.Background()
	userID, _ := seedConvProject(t, s)
	var wsID string
	if err := s.pg.QueryRow(ctx, `SELECT workspace_id::text FROM workspace_members WHERE user_id = $1 LIMIT 1`, userID).Scan(&wsID); err != nil {
		t.Fatalf("workspace: %v", err)
	}
	project, err := s.CreateWorkspaceProject(ctx, userID, wsID, "born-split")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	split, err := s.ProjectCredentialSplit(ctx, project.ID)
	if err != nil || !split {
		t.Fatalf("new project split = %v, %v — want capture-only from birth", split, err)
	}
}
