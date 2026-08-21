package storage

import (
	"context"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lohi-ai/agentray/internal/shared/config"
)

// The signup + shared-demo behaviours that only a real Postgres can prove.
// Skipped unless AGENTRAY_LIVE_PG is set, matching the other *_live_test.go
// files, so CI and the default `go test` never touch a real cluster.
//
// Run with the docker-compose stack up:
//
//	AGENTRAY_LIVE_PG=postgres://lohi:lohi@localhost:5434/lohi_analytics?sslmode=disable \
//	go test ./internal/dataplane/store/ -run Demo -v

func demoTestStore(t *testing.T) (*Store, context.Context) {
	t.Helper()
	dsn := os.Getenv("AGENTRAY_LIVE_PG")
	if dsn == "" {
		t.Skip("set AGENTRAY_LIVE_PG to run the live demo-membership tests")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	return &Store{pg: pool}, ctx
}

// dropAccount removes a test account and its workspaces. projects.workspace_id
// is ON DELETE SET NULL, not CASCADE, so a project outlives its workspace as an
// orphan row — delete projects first or every run leaves litter behind.
func dropAccount(t *testing.T, s *Store, ctx context.Context, userID string, workspaceIDs ...string) {
	t.Helper()
	t.Cleanup(func() {
		for _, id := range workspaceIDs {
			if id == "" {
				continue
			}
			dropWorkspace(t, s, ctx, id)
		}
		if _, err := s.pg.Exec(ctx, `DELETE FROM users WHERE id = $1::uuid`, userID); err != nil {
			t.Errorf("cleanup user %s: %v", userID, err)
		}
	})
}

func dropWorkspace(t *testing.T, s *Store, ctx context.Context, workspaceID string) {
	t.Helper()
	if _, err := s.pg.Exec(ctx, `DELETE FROM projects WHERE workspace_id = $1::uuid`, workspaceID); err != nil {
		t.Errorf("cleanup projects of %s: %v", workspaceID, err)
	}
	if _, err := s.pg.Exec(ctx, `DELETE FROM workspaces WHERE id = $1::uuid`, workspaceID); err != nil {
		t.Errorf("cleanup workspace %s: %v", workspaceID, err)
	}
}

// Signup used to insert a project literally named "Demo" into the new owner's
// OWN workspace and fill it with invented events, so their first session showed
// synthetic numbers sitting next to their real project, indistinguishable from
// data they had collected. The account must now get exactly the one project it
// asked for.
func TestDemoSignupCreatesOnlyTheCallersOwnProject(t *testing.T) {
	s, ctx := demoTestStore(t)
	s.demoProjectID, s.demoWorkspaceID = "", ""

	email := "demo-signup-" + uuid.NewString() + "@example.test"
	boot, err := s.CreateAccount(ctx, email, "Solo", "correct-horse", "Acme", "Production")
	if err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}
	dropAccount(t, s, ctx, boot.User.ID, boot.Workspace.ID)

	projects, err := s.ListWorkspaceProjects(ctx, boot.User.ID, boot.Workspace.ID)
	if err != nil {
		t.Fatalf("ListWorkspaceProjects: %v", err)
	}
	if len(projects) != 1 {
		names := make([]string, len(projects))
		for i, p := range projects {
			names[i] = p.Name
		}
		t.Fatalf("new account got %d projects %v, want exactly 1", len(projects), names)
	}
	if projects[0].Name != "Production" {
		t.Errorf("project name = %q, want the one the caller asked for (%q)", projects[0].Name, "Production")
	}
	// The regression this guards is specifically a project named "Demo" in the
	// user's own workspace.
	var demoNamed int
	if err := s.pg.QueryRow(ctx,
		`SELECT count(*) FROM projects WHERE workspace_id = $1::uuid AND lower(name) = 'demo'`,
		boot.Workspace.ID).Scan(&demoNamed); err != nil {
		t.Fatalf("count demo-named projects: %v", err)
	}
	if demoNamed != 0 {
		t.Errorf("%d project(s) named Demo landed in the owner's own workspace", demoNamed)
	}
	// The project the app opens on is theirs, and it says so.
	def, err := s.DefaultProjectForUser(ctx, boot.User.ID)
	if err != nil {
		t.Fatalf("DefaultProjectForUser: %v", err)
	}
	if def.ID != projects[0].ID {
		t.Errorf("default project %q is not the account's own project %q", def.ID, projects[0].ID)
	}
	if def.Role != "owner" || def.IsDemo {
		t.Errorf("own project reported role=%q is_demo=%v, want owner/false", def.Role, def.IsDemo)
	}
}

// A self-hosted `docker compose up` operator will never set
// AGENTRAY_DEMO_PROJECT_ID. Signup has to work end to end with no demo, and must
// not leave the account a member of anything but its own workspace.
func TestDemoAbsentSignupGrantsNoExtraMembership(t *testing.T) {
	s, ctx := demoTestStore(t)
	// Explicitly the unconfigured instance.
	if err := s.migrateDemoWorkspace(ctx, config.Config{}); err != nil {
		t.Fatalf("migrateDemoWorkspace with no demo: %v", err)
	}
	if s.DemoWorkspaceID() != "" || s.DemoProjectID() != "" {
		t.Fatalf("no demo configured but resolved to project %q / workspace %q", s.DemoProjectID(), s.DemoWorkspaceID())
	}

	email := "demo-absent-" + uuid.NewString() + "@example.test"
	boot, err := s.CreateAccount(ctx, email, "Solo", "correct-horse", "Acme", "Production")
	if err != nil {
		t.Fatalf("CreateAccount with no demo configured: %v", err)
	}
	dropAccount(t, s, ctx, boot.User.ID, boot.Workspace.ID)

	workspaces, err := s.ListUserWorkspaces(ctx, boot.User.ID)
	if err != nil {
		t.Fatalf("ListUserWorkspaces: %v", err)
	}
	if len(workspaces) != 1 || workspaces[0].ID != boot.Workspace.ID {
		t.Fatalf("account belongs to %d workspaces, want only its own", len(workspaces))
	}
	if workspaces[0].Role != "owner" || workspaces[0].IsDemo {
		t.Errorf("own workspace reported role=%q is_demo=%v, want owner/false", workspaces[0].Role, workspaces[0].IsDemo)
	}
}

// The demo is someone else's live site. A signup joins it to READ it — if the
// membership were ever owner or admin, every visitor could rename its projects
// and rotate its API keys.
func TestDemoSignupJoinsTheSharedDemoAsViewer(t *testing.T) {
	s, ctx := demoTestStore(t)
	demoWorkspaceID, demoProjectID, _ := seedDemoSite(t, s, ctx)
	if err := s.migrateDemoWorkspace(ctx, config.Config{DemoProjectID: demoProjectID}); err != nil {
		t.Fatalf("migrateDemoWorkspace: %v", err)
	}
	if s.DemoWorkspaceID() != demoWorkspaceID {
		t.Fatalf("demo resolved to workspace %q, want the project's owning workspace %q", s.DemoWorkspaceID(), demoWorkspaceID)
	}

	email := "demo-viewer-" + uuid.NewString() + "@example.test"
	boot, err := s.CreateAccount(ctx, email, "Visitor", "correct-horse", "Acme", "Production")
	if err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}
	dropAccount(t, s, ctx, boot.User.ID, boot.Workspace.ID)

	var role string
	if err := s.pg.QueryRow(ctx,
		`SELECT role FROM workspace_members WHERE workspace_id = $1::uuid AND user_id = $2::uuid`,
		demoWorkspaceID, boot.User.ID).Scan(&role); err != nil {
		t.Fatalf("signup did not join the demo workspace: %v", err)
	}
	if role != DemoViewerRole {
		t.Fatalf("demo membership role = %q, want %q", role, DemoViewerRole)
	}
	// A viewer must not pass the write gate every mutator checks.
	if ok, err := s.UserCanManageWorkspace(ctx, boot.User.ID, demoWorkspaceID); err != nil || ok {
		t.Errorf("viewer can manage the demo workspace (ok=%v err=%v)", ok, err)
	}

	// The API has to say what it is, or the UI cannot tell a live demo of
	// someone else's site from the user's own workspace.
	workspaces, err := s.ListUserWorkspaces(ctx, boot.User.ID)
	if err != nil {
		t.Fatalf("ListUserWorkspaces: %v", err)
	}
	if len(workspaces) != 2 {
		t.Fatalf("account belongs to %d workspaces, want its own + the demo", len(workspaces))
	}
	// The demo sorts last: workspaces[0] is what the app opens on, and the demo
	// predates every account, so plain created_at ASC would land every new
	// signup inside someone else's site.
	if workspaces[0].ID != boot.Workspace.ID {
		t.Errorf("first workspace is %q, want the account's own %q", workspaces[0].ID, boot.Workspace.ID)
	}
	demo := workspaces[1]
	if demo.ID != demoWorkspaceID || !demo.IsDemo || demo.Role != DemoViewerRole {
		t.Errorf("demo workspace reported id=%q is_demo=%v role=%q, want %q/true/%q",
			demo.ID, demo.IsDemo, demo.Role, demoWorkspaceID, DemoViewerRole)
	}
	// The default project must still be theirs, not the (much older) demo's.
	def, err := s.DefaultProjectForUser(ctx, boot.User.ID)
	if err != nil {
		t.Fatalf("DefaultProjectForUser: %v", err)
	}
	if def.WorkspaceID != boot.Workspace.ID {
		t.Errorf("a new signup opens on project %q in workspace %q — that is the demo, not their own",
			def.Name, def.WorkspaceID)
	}
	// And the demo's own projects carry the mark and the caller's role.
	demoProjects, err := s.ListWorkspaceProjects(ctx, boot.User.ID, demoWorkspaceID)
	if err != nil {
		t.Fatalf("ListWorkspaceProjects(demo): %v", err)
	}
	if len(demoProjects) != 1 || !demoProjects[0].IsDemo || demoProjects[0].Role != DemoViewerRole {
		t.Errorf("demo project list = %+v, want one project marked is_demo with role %q", demoProjects, DemoViewerRole)
	}
}

// The backfill runs on EVERY boot. Re-running it must be a no-op, and it must
// never overwrite a role someone already has — the demo site's real operator is
// an owner of that workspace, and a boot that demoted them to viewer would lock
// them out of their own data.
func TestDemoBackfillIsIdempotentAndNeverDowngrades(t *testing.T) {
	s, ctx := demoTestStore(t)
	demoWorkspaceID, demoProjectID, demoOwnerID := seedDemoSite(t, s, ctx)

	// A pre-existing account that signed up before the demo existed.
	oldEmail := "demo-backfill-" + uuid.NewString() + "@example.test"
	s.demoProjectID, s.demoWorkspaceID = "", ""
	old, err := s.CreateAccount(ctx, oldEmail, "Early", "correct-horse", "Acme", "Production")
	if err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}
	dropAccount(t, s, ctx, old.User.ID, old.Workspace.ID)

	for i := 0; i < 3; i++ {
		if err := s.migrateDemoWorkspace(ctx, config.Config{DemoProjectID: demoProjectID}); err != nil {
			t.Fatalf("migrateDemoWorkspace pass %d: %v", i, err)
		}
	}

	var backfilled string
	if err := s.pg.QueryRow(ctx,
		`SELECT role FROM workspace_members WHERE workspace_id = $1::uuid AND user_id = $2::uuid`,
		demoWorkspaceID, old.User.ID).Scan(&backfilled); err != nil {
		t.Fatalf("pre-existing user was not backfilled into the demo: %v", err)
	}
	if backfilled != DemoViewerRole {
		t.Errorf("backfilled role = %q, want %q", backfilled, DemoViewerRole)
	}
	// Idempotent: the primary key makes a duplicate impossible, so a count above
	// one would mean the schema lost that key.
	var rows int
	if err := s.pg.QueryRow(ctx,
		`SELECT count(*) FROM workspace_members WHERE workspace_id = $1::uuid AND user_id = $2::uuid`,
		demoWorkspaceID, old.User.ID).Scan(&rows); err != nil {
		t.Fatalf("count memberships: %v", err)
	}
	if rows != 1 {
		t.Errorf("three backfill passes left %d membership rows, want 1", rows)
	}

	// The one that matters: the demo site's owner is still an owner.
	var ownerRole string
	if err := s.pg.QueryRow(ctx,
		`SELECT role FROM workspace_members WHERE workspace_id = $1::uuid AND user_id = $2::uuid`,
		demoWorkspaceID, demoOwnerID).Scan(&ownerRole); err != nil {
		t.Fatalf("demo owner membership: %v", err)
	}
	if ownerRole != "owner" {
		t.Fatalf("the backfill downgraded the demo site's owner to %q", ownerRole)
	}
}

// A demo id pointing at a project that no longer exists must disable the demo,
// not take the API down — the instance is fully usable without one.
func TestDemoMisconfiguredIDDisablesTheDemoInsteadOfFailingBoot(t *testing.T) {
	s, ctx := demoTestStore(t)
	for _, id := range []string{"not-a-uuid", "00000000-0000-0000-0000-000000000000"} {
		if err := s.migrateDemoWorkspace(ctx, config.Config{DemoProjectID: id}); err != nil {
			t.Fatalf("migrateDemoWorkspace(%q) failed boot: %v", id, err)
		}
		if s.DemoWorkspaceID() != "" {
			t.Errorf("demo id %q resolved to workspace %q, want the demo left off", id, s.DemoWorkspaceID())
		}
	}
}

// seedDemoSite stands up a throwaway "real demo site": an operator, the
// workspace they own, and one project in it. Returns (workspaceID, projectID,
// ownerID); everything is dropped on the way out.
func seedDemoSite(t *testing.T, s *Store, ctx context.Context) (string, string, string) {
	t.Helper()
	var ownerID, workspaceID, projectID string
	if err := s.pg.QueryRow(ctx, `
INSERT INTO users (email, name, password_hash)
VALUES ('demo-site-' || gen_random_uuid()::text || '@example.test', 'Demo site owner', 'x')
RETURNING id::text`).Scan(&ownerID); err != nil {
		t.Fatalf("seed demo owner: %v", err)
	}
	if err := s.pg.QueryRow(ctx, `
INSERT INTO workspaces (name, created_by) VALUES ('Demo site', $1::uuid)
RETURNING id::text`, ownerID).Scan(&workspaceID); err != nil {
		t.Fatalf("seed demo workspace: %v", err)
	}
	if _, err := s.pg.Exec(ctx, `
INSERT INTO workspace_members (workspace_id, user_id, role) VALUES ($1::uuid, $2::uuid, 'owner')`,
		workspaceID, ownerID); err != nil {
		t.Fatalf("seed demo owner membership: %v", err)
	}
	if err := s.pg.QueryRow(ctx, `
INSERT INTO projects (workspace_id, owner_id, name, api_key)
VALUES ($1::uuid, $2::uuid, 'Live site', 'demo-live-test-' || gen_random_uuid()::text)
RETURNING id::text`, workspaceID, ownerID).Scan(&projectID); err != nil {
		t.Fatalf("seed demo project: %v", err)
	}
	t.Cleanup(func() {
		dropWorkspace(t, s, ctx, workspaceID)
		if _, err := s.pg.Exec(ctx, `DELETE FROM users WHERE id = $1::uuid`, ownerID); err != nil {
			t.Errorf("cleanup demo owner: %v", err)
		}
	})
	return workspaceID, projectID, ownerID
}
