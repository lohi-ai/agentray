package storage

import (
	"context"
	"fmt"

	"github.com/lohi-ai/agentray/internal/shared/config"
)

// The shared demo, in one place.
//
// It used to be synthetic: every signup got a project literally named "Demo"
// inside its OWN workspace, filled with invented events. That wowed and then
// lied — the numbers sat next to the account's real project, in the account's
// own workspace, indistinguishable from data the owner had collected.
//
// It is now one REAL project, on a site the instance operator actually runs,
// that every account joins as a read-only viewer. Nothing is generated; what a
// new user sees on their first session is traffic that happened.
//
// AGENTRAY_DEMO_PROJECT_ID names that project. Empty — the default, and every
// `docker compose up` — means this instance has no demo, and every path here
// degrades to a no-op rather than to a half-configured one.

// DemoViewerRole is the membership a signed-up visitor gets in the demo
// workspace. It is never owner or admin: the demo is someone else's site, and a
// viewer is there to read it. (What a viewer may not DO is enforced separately;
// this file only establishes the fact.)
const DemoViewerRole = "viewer"

// migrateDemoWorkspace resolves the configured demo project to the workspace
// that owns it, then backfills the viewer membership for everyone who signed up
// before the demo existed. Runs on every boot and must be safe to; see
// backfillDemoViewers for why re-running cannot demote anyone.
//
// A misconfigured or deleted demo project is NOT a boot failure. The instance
// is fully usable without a demo, so a bad id logs and leaves the feature off
// rather than taking the API down.
func (s *Store) migrateDemoWorkspace(ctx context.Context, cfg config.Config) error {
	s.demoProjectID = ""
	s.demoWorkspaceID = ""
	if cfg.DemoProjectID == "" {
		return nil
	}
	var projectID, workspaceID string
	// The demo workspace is not a second concept to configure — it is whatever
	// workspace owns the demo project. NULL workspace_id (the bootstrap default
	// project has none) means the id points at a project no one can be a member
	// of, so there is nothing to grant.
	err := s.pg.QueryRow(ctx, `
SELECT id::text, COALESCE(workspace_id::text, '')
FROM projects
WHERE id = NULLIF($1, '')::uuid`, cfg.DemoProjectID).Scan(&projectID, &workspaceID)
	if err != nil {
		fmt.Printf("warn: demo project %q not found, demo disabled: %v\n", cfg.DemoProjectID, err)
		return nil
	}
	if workspaceID == "" {
		fmt.Printf("warn: demo project %q has no workspace, demo disabled\n", cfg.DemoProjectID)
		return nil
	}
	s.demoProjectID = projectID
	s.demoWorkspaceID = workspaceID
	if err := s.backfillDemoViewers(ctx); err != nil {
		// Same reasoning as above: an account that misses the demo has a working
		// account. The next boot retries, and signup grants it directly.
		fmt.Printf("warn: backfillDemoViewers(%s): %v\n", workspaceID, err)
	}
	return nil
}

// backfillDemoViewers gives every existing user the viewer membership a new
// signup gets. ON CONFLICT DO NOTHING is the whole idempotency story AND the
// whole safety story: the demo site's real operator is presumably an owner of
// that workspace, and a boot that quietly demoted them to viewer would lock
// them out of their own data.
func (s *Store) backfillDemoViewers(ctx context.Context) error {
	if s.demoWorkspaceID == "" {
		return nil
	}
	_, err := s.pg.Exec(ctx, `
INSERT INTO workspace_members (workspace_id, user_id, role)
SELECT $1::uuid, u.id, $2 FROM users u
ON CONFLICT (workspace_id, user_id) DO NOTHING`, s.demoWorkspaceID, DemoViewerRole)
	return err
}

// addDemoViewer grants one user the demo membership. Best-effort by contract:
// CreateAccount calls it after its transaction has committed, so a failure here
// must leave the account intact — the next boot's backfill picks it up.
func (s *Store) addDemoViewer(ctx context.Context, userID string) error {
	if s.demoWorkspaceID == "" {
		return nil
	}
	_, err := s.pg.Exec(ctx, `
INSERT INTO workspace_members (workspace_id, user_id, role)
VALUES ($1::uuid, $2::uuid, $3)
ON CONFLICT (workspace_id, user_id) DO NOTHING`, s.demoWorkspaceID, userID, DemoViewerRole)
	return err
}

// DemoWorkspaceID is the workspace that owns the configured demo project, or ""
// when this instance has no demo. Exported so HTTP handlers can answer "is this
// the demo?" without re-reading config and re-resolving the project.
func (s *Store) DemoWorkspaceID() string { return s.demoWorkspaceID }

// DemoProjectID is the configured demo project, or "" when there is none.
func (s *Store) DemoProjectID() string { return s.demoProjectID }

// isDemoWorkspace marks what the API hands back, so the UI can say "this is a
// shared live demo of someone else's site, you are reading it" instead of
// guessing from a project name.
//
// Everything inside the demo workspace is demo, not just the one configured
// project: membership is granted at the workspace level, so every project that
// workspace holds is something the viewer got in through the demo.
func (s *Store) isDemoWorkspace(workspaceID string) bool {
	return workspaceID != "" && workspaceID == s.demoWorkspaceID
}
