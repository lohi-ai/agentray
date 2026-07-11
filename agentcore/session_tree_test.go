package agentcore

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// msgEntry builds an EntryMessage node with an explicit id/parent.
func msgEntry(id, parent string, role Role, content string) SessionEntry {
	return SessionEntry{Kind: EntryMessage, ID: id, ParentID: parent, Message: &Message{Role: role, Content: content}}
}

// TestFlatLogActivePathIsWholeLog pins backward compatibility: a legacy id-less
// log is a single-branch tree, so ActivePath and ReduceSession see every entry
// in order — exactly the pre-tree behavior.
func TestFlatLogActivePathIsWholeLog(t *testing.T) {
	log := []SessionEntry{
		{Kind: EntryMessage, Message: &Message{Role: RoleUser, Content: "hi"}},
		{Kind: EntryMessage, Message: &Message{Role: RoleAssistant, Content: "hello"}},
		{Kind: EntryLeaf},
	}
	path := ActivePath(log)
	if len(path) != 3 {
		t.Fatalf("flat log path length = %d, want 3", len(path))
	}
	rs := ReduceSession(log)
	if len(rs.Messages) != 2 || rs.Messages[0].Content != "hi" || rs.Messages[1].Content != "hello" {
		t.Fatalf("flat log reduce changed: %+v", rs.Messages)
	}
	if !rs.Completed {
		t.Fatal("flat log with leaf must reduce Completed")
	}
}

// TestBranchByParentIDForksThePath verifies pi's append-is-branch model: an
// entry whose ParentID names an earlier entry forks the tree, and the active
// path follows the new branch while the abandoned one stays in the log.
func TestBranchByParentIDForksThePath(t *testing.T) {
	log := []SessionEntry{
		msgEntry("a", "", RoleUser, "task"),
		msgEntry("b", "", RoleAssistant, "first try"),
		msgEntry("c", "", RoleAssistant, "dead end"),
		// Fork: continue from "a" instead of "c".
		msgEntry("d", "a", RoleAssistant, "second try"),
	}
	if leaf := ActiveLeaf(log); leaf != "d" {
		t.Fatalf("leaf = %q, want d", leaf)
	}
	rs := ReduceSession(log)
	want := []string{"task", "second try"}
	if len(rs.Messages) != len(want) {
		t.Fatalf("reduced %d messages, want %d: %+v", len(rs.Messages), len(want), rs.Messages)
	}
	for i, w := range want {
		if rs.Messages[i].Content != w {
			t.Fatalf("message[%d] = %q, want %q", i, rs.Messages[i].Content, w)
		}
	}
}

// TestLeafMoveRewindsImplicitChain verifies an EntryLeafMove rewinds the leaf so
// the next id-less append chains from the move target, not the previous tail.
func TestLeafMoveRewindsImplicitChain(t *testing.T) {
	log := []SessionEntry{
		msgEntry("a", "", RoleUser, "task"),
		msgEntry("b", "", RoleAssistant, "wrong direction"),
		{Kind: EntryLeafMove, Target: "a"},
		// Legacy-style writer appends without ids after the rewind.
		{Kind: EntryMessage, Message: &Message{Role: RoleAssistant, Content: "fresh start"}},
	}
	rs := ReduceSession(log)
	want := []string{"task", "fresh start"}
	if len(rs.Messages) != len(want) {
		t.Fatalf("reduced %d messages, want %d: %+v", len(rs.Messages), len(want), rs.Messages)
	}
	for i, w := range want {
		if rs.Messages[i].Content != w {
			t.Fatalf("message[%d] = %q, want %q", i, rs.Messages[i].Content, w)
		}
	}
}

// TestSessionTreeAddressesLegacyEntries verifies id-less entries get stable
// synthetic ids so they remain valid Rewind targets.
func TestSessionTreeAddressesLegacyEntries(t *testing.T) {
	log := []SessionEntry{
		{Kind: EntryMessage, Message: &Message{Role: RoleUser, Content: "hi"}},
		{Kind: EntryMessage, Message: &Message{Role: RoleAssistant, Content: "hello"}},
	}
	nodes := SessionTree(log)
	if len(nodes) != 2 {
		t.Fatalf("tree has %d nodes, want 2", len(nodes))
	}
	if nodes[0].ID != "#0" || nodes[1].ID != "#1" || nodes[1].ParentID != "#0" {
		t.Fatalf("synthetic ids wrong: %+v", nodes)
	}
}

// TestRewindWritesBranchSummaryAndMovesLeaf drives the full rewind flow against
// a store: the abandoned span (and only it) reaches the summarizer, the summary
// node chains from the target, the leaf moves, and the reduced history is the
// new branch with the summary folded in as marked system context.
func TestRewindWritesBranchSummaryAndMovesLeaf(t *testing.T) {
	ctx := context.Background()
	store := newMemSessionStore()
	const sid = "s1"
	for _, e := range []SessionEntry{
		msgEntry("a", "", RoleUser, "analyze signups"),
		msgEntry("b", "", RoleAssistant, "querying the wrong table"),
		msgEntry("c", "", RoleAssistant, "still wrong"),
	} {
		if err := store.Append(ctx, sid, e); err != nil {
			t.Fatal(err)
		}
	}

	var summarized []Message
	newLeaf, err := Rewind(ctx, store, sid, "a", BranchOptions{
		Summarize: func(_ context.Context, abandoned []Message) (string, error) {
			summarized = abandoned
			return "tried events_raw; it lacks signup rows", nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if newLeaf == "" || newLeaf == "a" {
		t.Fatalf("new leaf should be the summary node, got %q", newLeaf)
	}
	// Only the abandoned span (b, c) is summarized — the shared prefix (a) is not.
	if len(summarized) != 2 || summarized[0].Content != "querying the wrong table" || summarized[1].Content != "still wrong" {
		t.Fatalf("summarizer saw wrong span: %+v", summarized)
	}

	// Continue down the new branch and reduce.
	if err := store.Append(ctx, sid, msgEntry("d", "", RoleAssistant, "using signup_events now")); err != nil {
		t.Fatal(err)
	}
	log, err := store.Log(ctx, sid)
	if err != nil {
		t.Fatal(err)
	}
	rs := ReduceSession(log)
	if len(rs.Messages) != 3 {
		t.Fatalf("reduced %d messages, want 3 (task, branch summary, new work): %+v", len(rs.Messages), rs.Messages)
	}
	if rs.Messages[0].Content != "analyze signups" {
		t.Fatalf("message[0] = %q", rs.Messages[0].Content)
	}
	if rs.Messages[1].Role != RoleSystem || !strings.HasPrefix(rs.Messages[1].Content, branchSummaryMarker) || !strings.Contains(rs.Messages[1].Content, "events_raw") {
		t.Fatalf("branch summary not folded as marked system context: %+v", rs.Messages[1])
	}
	if rs.Messages[2].Content != "using signup_events now" {
		t.Fatalf("message[2] = %q", rs.Messages[2].Content)
	}
	// The abandoned branch is still in the log for inspection.
	if len(log) != 6 { // a, b, c, branch_summary, leaf_move, d
		t.Fatalf("log length = %d, want 6", len(log))
	}
}

// TestRewindDegradesToBareLeafMove verifies a failing summarizer never fails
// the rewind: the leaf still moves, with no summary node.
func TestRewindDegradesToBareLeafMove(t *testing.T) {
	ctx := context.Background()
	store := newMemSessionStore()
	const sid = "s1"
	for _, e := range []SessionEntry{
		msgEntry("a", "", RoleUser, "task"),
		msgEntry("b", "", RoleAssistant, "abandoned"),
	} {
		if err := store.Append(ctx, sid, e); err != nil {
			t.Fatal(err)
		}
	}
	newLeaf, err := Rewind(ctx, store, sid, "a", BranchOptions{
		Summarize: func(context.Context, []Message) (string, error) { return "", errors.New("summarizer down") },
	})
	if err != nil {
		t.Fatal(err)
	}
	if newLeaf != "a" {
		t.Fatalf("new leaf = %q, want a", newLeaf)
	}
	log, _ := store.Log(ctx, sid)
	rs := ReduceSession(log)
	if len(rs.Messages) != 1 || rs.Messages[0].Content != "task" {
		t.Fatalf("reduce after degraded rewind: %+v", rs.Messages)
	}
}

// TestRewindRejectsUnknownTarget pins the error path.
func TestRewindRejectsUnknownTarget(t *testing.T) {
	ctx := context.Background()
	store := newMemSessionStore()
	if err := store.Append(ctx, "s1", msgEntry("a", "", RoleUser, "task")); err != nil {
		t.Fatal(err)
	}
	if _, err := Rewind(ctx, store, "s1", "nope", BranchOptions{}); err == nil {
		t.Fatal("rewind to an unknown target must error")
	}
}

// TestRecoverSessionFollowsActiveBranch verifies durable recovery resumes down
// the rewound branch, not the abandoned one.
func TestRecoverSessionFollowsActiveBranch(t *testing.T) {
	log := []SessionEntry{
		msgEntry("a", "", RoleUser, "task"),
		msgEntry("b", "", RoleAssistant, "abandoned work"),
		{Kind: EntryLeafMove, Target: "a"},
		msgEntry("c", "", RoleAssistant, "new branch work"),
		// no leaf: the run crashed on the new branch
	}
	plan := RecoverSession(log, NewToolSet(), RecoveryMarkInterrupted)
	if plan.Completed {
		t.Fatal("crashed branch must not be Completed")
	}
	if !plan.Interrupted {
		t.Fatal("crashed branch must be Interrupted")
	}
	for _, m := range plan.Messages {
		if m.Content == "abandoned work" {
			t.Fatal("recovery leaked the abandoned branch into the resume history")
		}
	}
}

// TestLoopStampsEntryIDs verifies a durable run writes tree-addressable
// entries: every node has a unique id and chains to the entry before it.
func TestLoopStampsEntryIDs(t *testing.T) {
	store := newMemSessionStore()
	faux := NewFauxProvider(AssistantText("done"))
	agent, err := New(Config{
		Provider:  faux,
		Model:     "faux-1",
		Tools:     NewToolSet(),
		Policy:    NewAllowList(),
		Session:   store,
		SessionID: "run-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := agent.Prompt(context.Background(), "hi"); err != nil {
		t.Fatal(err)
	}
	log, err := store.Log(context.Background(), "run-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(log) < 3 { // user seed, assistant, leaf
		t.Fatalf("log too short: %d entries", len(log))
	}
	seen := map[string]bool{}
	for i, e := range log {
		if e.ID == "" {
			t.Fatalf("entry %d has no id: %+v", i, e)
		}
		if seen[e.ID] {
			t.Fatalf("duplicate entry id %q", e.ID)
		}
		seen[e.ID] = true
		if i == 0 {
			if e.ParentID != "" {
				t.Fatalf("root entry has parent %q", e.ParentID)
			}
			continue
		}
		if e.ParentID != log[i-1].ID {
			t.Fatalf("entry %d parent = %q, want %q (the previous entry)", i, e.ParentID, log[i-1].ID)
		}
	}
}
