package storage

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lohi-ai/agentray/internal/config"
)

// These are integration tests for the conversation store — the durable source the
// chat UI (raw entries) and the model context (folded path-to-leaf) both derive
// from. They need a real Postgres because the leaf-advance, the parent tree, and
// the path-to-leaf walk are all SQL. Point AGENTRAY_TEST_DATABASE_URL at one (the
// docker-compose db is host port 5434); the suite skips when none is reachable.

// openConvTestStore connects to the test Postgres and runs the full (idempotent)
// migration so the conversation tables and their FK targets exist. It skips —
// never fails — when no database is reachable, so the unit suite stays hermetic.
func openConvTestStore(t *testing.T) *Store {
	t.Helper()
	url := os.Getenv("AGENTRAY_TEST_DATABASE_URL")
	if url == "" {
		url = "postgres://lohi:lohi@localhost:5434/lohi_analytics?sslmode=disable"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Skipf("no test database (%v) — set AGENTRAY_TEST_DATABASE_URL", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Skipf("test database unreachable at %s (%v)", url, err)
	}
	s := &Store{pg: pool}
	if err := s.migratePostgres(ctx, config.Config{
		PostgresURL:          url,
		DefaultProjectName:   "conv-test",
		DefaultProjectAPIKey: "conv_test_default_key",
	}); err != nil {
		pool.Close()
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(pool.Close)
	return s
}

// seedConvProject creates a fresh user + workspace + membership + project and
// returns (userID, projectID). Each call uses gen_random_uuid() for the unique
// columns so repeated runs against a persistent db never collide.
func seedConvProject(t *testing.T, s *Store) (userID, projectID string) {
	t.Helper()
	ctx := context.Background()
	if err := s.pg.QueryRow(ctx, `
INSERT INTO users (email, name, password_hash)
VALUES ('conv-' || gen_random_uuid() || '@test.local', 'Conv Test', 'x')
RETURNING id::text`).Scan(&userID); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	var wsID string
	if err := s.pg.QueryRow(ctx, `
INSERT INTO workspaces (name, created_by) VALUES ('conv-ws', $1) RETURNING id::text`, userID).Scan(&wsID); err != nil {
		t.Fatalf("seed workspace: %v", err)
	}
	if _, err := s.pg.Exec(ctx, `
INSERT INTO workspace_members (workspace_id, user_id, role) VALUES ($1, $2, 'owner')`, wsID, userID); err != nil {
		t.Fatalf("seed membership: %v", err)
	}
	if err := s.pg.QueryRow(ctx, `
INSERT INTO projects (workspace_id, name, api_key, owner_id)
VALUES ($1, 'conv-proj', 'k_' || gen_random_uuid(), $2)
RETURNING id::text`, wsID, userID).Scan(&projectID); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	return userID, projectID
}

func msg(text string) string { return `{"text":` + `"` + text + `"}` }

// A linear thread: appends chain by parent, the leaf advances to the last entry,
// and both read paths (path-to-leaf for the model, the since-cursor for the FE)
// return the entries in seq order.
func TestConversationLinearAppendAndLeafAdvance(t *testing.T) {
	s := openConvTestStore(t)
	userID, projectID := seedConvProject(t, s)
	ctx := context.Background()

	conv, err := s.CreateConversation(ctx, userID, projectID, "", "linear thread")
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	if conv.LeafEntryID != "" {
		t.Fatalf("new conversation should have no leaf, got %q", conv.LeafEntryID)
	}

	e1, err := s.AppendConversationEntry(ctx, AgentConversationEntry{ConversationID: conv.ID, Kind: "message", Role: "user", PayloadJSON: msg("hello")})
	if err != nil {
		t.Fatalf("append e1: %v", err)
	}
	e2, err := s.AppendConversationEntry(ctx, AgentConversationEntry{ConversationID: conv.ID, Kind: "message", Role: "assistant", PayloadJSON: msg("hi there")})
	if err != nil {
		t.Fatalf("append e2: %v", err)
	}

	// Leaf followed the last append, and seq is monotonic.
	if got, _ := s.GetConversation(ctx, userID, projectID, conv.ID); got.LeafEntryID != e2.ID {
		t.Fatalf("leaf = %q, want last entry %q", got.LeafEntryID, e2.ID)
	}
	if e2.Seq <= e1.Seq {
		t.Fatalf("seq not monotonic: e1=%d e2=%d", e1.Seq, e2.Seq)
	}
	// e2 is parented to e1 (linear thread needs no explicit parent from the caller).
	if e2.ParentID != e1.ID {
		t.Fatalf("e2 parent = %q, want e1 %q", e2.ParentID, e1.ID)
	}

	path, err := s.PathToLeaf(ctx, conv.ID)
	if err != nil {
		t.Fatalf("PathToLeaf: %v", err)
	}
	if len(path) != 2 || path[0].ID != e1.ID || path[1].ID != e2.ID {
		t.Fatalf("path = %+v, want [e1 e2] in order", path)
	}
}

// The two-projection invariant at the database: a regenerate moves the leaf back
// to a branch point and appends a new reply, forking the line. PathToLeaf (the
// model context) follows only the WINNING line and excludes the abandoned reply;
// ConversationEntries (the FE chat view) returns EVERY entry of every branch. Same
// source, two deliberately different reads.
func TestConversationForkModelFollowsLeafFEKeepsAllBranches(t *testing.T) {
	s := openConvTestStore(t)
	userID, projectID := seedConvProject(t, s)
	ctx := context.Background()

	conv, err := s.CreateConversation(ctx, userID, projectID, "", "fork thread")
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	q1, _ := s.AppendConversationEntry(ctx, AgentConversationEntry{ConversationID: conv.ID, Kind: "message", Role: "user", PayloadJSON: msg("q1")})
	a1, _ := s.AppendConversationEntry(ctx, AgentConversationEntry{ConversationID: conv.ID, Kind: "message", Role: "assistant", PayloadJSON: msg("a1-original")})

	// Regenerate: repoint the leaf to the user turn, then append a fresh assistant
	// reply — it parents off q1 and forks a new line, abandoning a1-original.
	if err := s.SetConversationLeaf(ctx, conv.ID, q1.ID); err != nil {
		t.Fatalf("SetConversationLeaf: %v", err)
	}
	a2, err := s.AppendConversationEntry(ctx, AgentConversationEntry{ConversationID: conv.ID, Kind: "message", Role: "assistant", PayloadJSON: msg("a1-regenerated")})
	if err != nil {
		t.Fatalf("append a2: %v", err)
	}
	if a2.ParentID != q1.ID {
		t.Fatalf("regenerated reply should parent off q1 (%q), got %q", q1.ID, a2.ParentID)
	}

	// Model context: winning line only — q1 → a2, the abandoned a1 excluded.
	path, err := s.PathToLeaf(ctx, conv.ID)
	if err != nil {
		t.Fatalf("PathToLeaf: %v", err)
	}
	if len(path) != 2 || path[0].ID != q1.ID || path[1].ID != a2.ID {
		t.Fatalf("model path = %+v, want [q1 a2] (winning line)", path)
	}
	for _, e := range path {
		if e.ID == a1.ID {
			t.Fatalf("abandoned branch a1 leaked into the model context")
		}
	}

	// FE view: every entry of every branch, including the abandoned a1.
	all, err := s.ConversationEntries(ctx, userID, projectID, conv.ID, 0)
	if err != nil {
		t.Fatalf("ConversationEntries: %v", err)
	}
	ids := map[string]bool{}
	for _, e := range all {
		ids[e.ID] = true
	}
	if !ids[q1.ID] || !ids[a1.ID] || !ids[a2.ID] {
		t.Fatalf("FE view missing a branch: got %d entries %v", len(all), ids)
	}
}

// Per-message agent override: each entry is stamped with the acting agent, a
// switch is persisted on the conversation for future (override-less) turns, and
// past entries keep the agent that handled them — switching only affects new
// entries.
func TestConversationPerEntryAgentStampAndSwitch(t *testing.T) {
	s := openConvTestStore(t)
	userID, projectID := seedConvProject(t, s)
	ctx := context.Background()

	// Two distinct agent ids (no FK on entry/conversation agent_id, so any UUID is
	// fine for the storage contract).
	var agentA, agentB string
	if err := s.pg.QueryRow(ctx, `SELECT gen_random_uuid()::text, gen_random_uuid()::text`).Scan(&agentA, &agentB); err != nil {
		t.Fatalf("gen uuids: %v", err)
	}

	conv, err := s.CreateConversation(ctx, userID, projectID, agentA, "switch thread")
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	if conv.AgentID != agentA {
		t.Fatalf("conv agent = %q, want %q", conv.AgentID, agentA)
	}

	// Turn 1 handled by agent A.
	e1, err := s.AppendConversationEntry(ctx, AgentConversationEntry{
		ConversationID: conv.ID, Kind: "message", Role: "user", AgentID: agentA, PayloadJSON: msg("first"),
	})
	if err != nil {
		t.Fatalf("append e1: %v", err)
	}
	if e1.AgentID != agentA {
		t.Fatalf("e1 agent = %q, want %q", e1.AgentID, agentA)
	}

	// Switch the conversation to agent B (the per-message override persisting).
	if err := s.SetConversationAgent(ctx, userID, projectID, conv.ID, agentB); err != nil {
		t.Fatalf("SetConversationAgent: %v", err)
	}
	got, _ := s.GetConversation(ctx, userID, projectID, conv.ID)
	if got.AgentID != agentB {
		t.Fatalf("after switch conv agent = %q, want %q", got.AgentID, agentB)
	}

	// Turn 2 handled by agent B.
	e2, err := s.AppendConversationEntry(ctx, AgentConversationEntry{
		ConversationID: conv.ID, Kind: "message", Role: "user", AgentID: agentB, PayloadJSON: msg("second"),
	})
	if err != nil {
		t.Fatalf("append e2: %v", err)
	}

	// The switch only affected new entries: e1 still belongs to agent A, e2 to B.
	path, err := s.PathToLeaf(ctx, conv.ID)
	if err != nil {
		t.Fatalf("PathToLeaf: %v", err)
	}
	byID := map[string]string{}
	for _, e := range path {
		byID[e.ID] = e.AgentID
	}
	if byID[e1.ID] != agentA {
		t.Fatalf("e1 agent after switch = %q, want unchanged %q", byID[e1.ID], agentA)
	}
	if byID[e2.ID] != agentB {
		t.Fatalf("e2 agent = %q, want %q", byID[e2.ID], agentB)
	}
}

// The since-cursor read path the FE uses to sync: entries with seq > since only,
// in order — so a reconnecting client fetches just what it hasn't seen.
func TestConversationEntriesSinceCursor(t *testing.T) {
	s := openConvTestStore(t)
	userID, projectID := seedConvProject(t, s)
	ctx := context.Background()

	conv, _ := s.CreateConversation(ctx, userID, projectID, "", "sync thread")
	e1, _ := s.AppendConversationEntry(ctx, AgentConversationEntry{ConversationID: conv.ID, Kind: "message", Role: "user", PayloadJSON: msg("one")})
	e2, _ := s.AppendConversationEntry(ctx, AgentConversationEntry{ConversationID: conv.ID, Kind: "message", Role: "assistant", PayloadJSON: msg("two")})

	after, err := s.ConversationEntries(ctx, userID, projectID, conv.ID, e1.Seq)
	if err != nil {
		t.Fatalf("ConversationEntries since e1: %v", err)
	}
	if len(after) != 1 || after[0].ID != e2.ID {
		t.Fatalf("since e1.seq should return only e2, got %+v", after)
	}
}

// Cross-project isolation: a member of one project cannot read another project's
// conversation entries (the RBAC GetConversation guard on the FE read path).
func TestConversationEntriesRejectsForeignProject(t *testing.T) {
	s := openConvTestStore(t)
	ownerID, projectID := seedConvProject(t, s)
	otherUserID, _ := seedConvProject(t, s)
	ctx := context.Background()

	conv, _ := s.CreateConversation(ctx, ownerID, projectID, "", "private thread")
	_, _ = s.AppendConversationEntry(ctx, AgentConversationEntry{ConversationID: conv.ID, Kind: "message", Role: "user", PayloadJSON: msg("secret")})

	if _, err := s.ConversationEntries(ctx, otherUserID, projectID, conv.ID, 0); err == nil {
		t.Fatal("a non-member read another project's conversation entries")
	}
}
