package storage

import (
	"context"
	"database/sql"
	"strings"
	"time"
)

// The plan a workspace is on. There is deliberately NO enforcement behind these
// values: the plan informs the UI (badge, usage meter, upgrade moment) and
// nothing in the ingest or agent path reads it. Introducing enforcement is a
// separate decision with its own failure modes — a workspace that silently
// stops accepting events is worse than one that is over its quota.
const (
	PlanFree = "free"
	PlanSolo = "solo"
	PlanTeam = "team"
)

// PlanIDs is the ordered ladder. Order is the display order on the pricing page
// and the comparison order for "is this an upgrade".
var PlanIDs = []string{PlanFree, PlanSolo, PlanTeam}

// NormalizePlan maps free text onto a known plan id, defaulting to free. Any
// unknown value read back from the database reads as free rather than blank, so
// no surface has to handle an empty plan.
func NormalizePlan(plan string) string {
	switch strings.ToLower(strings.TrimSpace(plan)) {
	case PlanSolo:
		return PlanSolo
	case PlanTeam:
		return PlanTeam
	default:
		return PlanFree
	}
}

// UpgradeRequest is one recorded "I would pay for this" — the honest CTA while
// there is no payment processor. Stored so the button is never dead and so the
// demand signal survives past a page view.
type UpgradeRequest struct {
	ID          string    `json:"id"`
	WorkspaceID string    `json:"workspace_id"`
	UserID      string    `json:"user_id"`
	Plan        string    `json:"plan"`
	Email       string    `json:"email"`
	Volume      string    `json:"volume"`
	Note        string    `json:"note"`
	CreatedAt   time.Time `json:"created_at"`
}

// migrateWorkspacePlan adds the plan column and the upgrade-request table.
// Additive and idempotent, matching the repo's inline-migrate convention: the
// column is NOT NULL DEFAULT 'free' so every existing workspace reads back a
// real plan without a backfill pass.
func (s *Store) migrateWorkspacePlan(ctx context.Context) error {
	stmts := []string{
		`ALTER TABLE workspaces ADD COLUMN IF NOT EXISTS plan VARCHAR(32) NOT NULL DEFAULT 'free'`,
		`CREATE TABLE IF NOT EXISTS upgrade_requests (
	id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
	workspace_id UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
	user_id UUID REFERENCES users(id) ON DELETE SET NULL,
	plan VARCHAR(32) NOT NULL DEFAULT 'solo',
	email VARCHAR(255) NOT NULL DEFAULT '',
	volume VARCHAR(64) NOT NULL DEFAULT '',
	note TEXT NOT NULL DEFAULT '',
	created_at TIMESTAMPTZ NOT NULL DEFAULT now()
)`,
		`CREATE INDEX IF NOT EXISTS upgrade_requests_workspace_created_idx
ON upgrade_requests (workspace_id, created_at DESC)`,
	}
	for _, stmt := range stmts {
		if _, err := s.pg.Exec(ctx, stmt); err != nil {
			return err
		}
	}
	return nil
}

// CreateUpgradeRequest records interest in a paid plan. Any workspace member may
// file one — this is a demand signal, not a privileged mutation, and gating it on
// owner/admin would silently drop the exact person who wants to buy.
func (s *Store) CreateUpgradeRequest(ctx context.Context, userID string, workspaceID string, plan string, email string, volume string, note string) (UpgradeRequest, error) {
	if ok, err := s.userCanAccessWorkspace(ctx, userID, workspaceID); err != nil || !ok {
		if err != nil {
			return UpgradeRequest{}, err
		}
		return UpgradeRequest{}, sql.ErrNoRows
	}
	req := UpgradeRequest{}
	err := s.pg.QueryRow(ctx, `
INSERT INTO upgrade_requests (workspace_id, user_id, plan, email, volume, note)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING id::text, workspace_id::text, COALESCE(user_id::text, ''), plan, email, volume, note, created_at`,
		workspaceID, userID, NormalizePlan(plan), strings.TrimSpace(email), strings.TrimSpace(volume), strings.TrimSpace(note)).
		Scan(&req.ID, &req.WorkspaceID, &req.UserID, &req.Plan, &req.Email, &req.Volume, &req.Note, &req.CreatedAt)
	if err == nil {
		_ = s.recordWorkspaceAudit(ctx, workspaceID, userID, "workspace.upgrade_requested", "workspace", workspaceID, req.Plan, "{}")
	}
	return req, err
}

// LatestUpgradeRequest is the workspace's most recent request, so the upgrade
// sheet can show "you already asked" instead of inviting a duplicate.
func (s *Store) LatestUpgradeRequest(ctx context.Context, userID string, workspaceID string) (UpgradeRequest, error) {
	if ok, err := s.userCanAccessWorkspace(ctx, userID, workspaceID); err != nil || !ok {
		if err != nil {
			return UpgradeRequest{}, err
		}
		return UpgradeRequest{}, sql.ErrNoRows
	}
	req := UpgradeRequest{}
	err := s.pg.QueryRow(ctx, `
SELECT id::text, workspace_id::text, COALESCE(user_id::text, ''), plan, email, volume, note, created_at
FROM upgrade_requests
WHERE workspace_id = $1
ORDER BY created_at DESC
LIMIT 1`, workspaceID).
		Scan(&req.ID, &req.WorkspaceID, &req.UserID, &req.Plan, &req.Email, &req.Volume, &req.Note, &req.CreatedAt)
	return req, err
}
