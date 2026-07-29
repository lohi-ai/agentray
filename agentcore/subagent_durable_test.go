package agentcore

import (
	"context"
	"strings"
	"testing"
)

// durableSubagentAgent wires a delegation-enabled agent whose run is durable,
// so spawned children get deterministic durable sessions of their own.
func durableSubagentAgent(t *testing.T, store SessionStore, sessionID string, script ...ChatResponse) *Agent {
	t.Helper()
	agent, err := New(Config{
		Provider:  NewFauxProvider(script...),
		Model:     "faux-1",
		Tools:     NewToolSet(&echoTool{name: "echo"}),
		Policy:    NewAllowList("echo", ToolSpawnSubagent),
		Subagents: &SubagentSettings{},
		Session:   store,
		SessionID: sessionID,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return agent
}

// TestSubagentChildSessionDurable verifies a durable parent gives its child a
// durable session at the deterministic ID parentSessionID+"/"+toolCallID, whose
// log reduces to the child's completed run — the record Lab needs to inspect
// parent/child run trees.
func TestSubagentChildSessionDurable(t *testing.T) {
	store := newMemSessionStore()
	agent := durableSubagentAgent(t, store, "p1",
		AssistantToolCall("c1", ToolSpawnSubagent, `{"task":"echo banana and report"}`),
		AssistantToolCall("c2", "echo", `{"text":"banana"}`),
		AssistantText("the echo returned: banana"),
		AssistantText("child reported: banana"),
	)
	if _, err := agent.Prompt(context.Background(), "delegate"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	childLog, err := store.Log(context.Background(), "p1/c1")
	if err != nil {
		t.Fatalf("child log: %v", err)
	}
	if len(childLog) == 0 {
		t.Fatal("child ran without a durable session at p1/c1")
	}
	rs := ReduceSession(childLog)
	if !rs.Completed {
		t.Fatalf("child log must reduce Completed; entries=%d", len(childLog))
	}
	if got := lastAssistantText(rs.Messages); got != "the echo returned: banana" {
		t.Fatalf("child log final = %q", got)
	}
}

// TestSubagentReattachSkipsRerun pins the replay contract: a spawn call
// re-issued with the same (parent session, call ID) — what RecoverSession does
// after a parent crash — must return the completed child's recorded answer
// without re-running it (no duplicate spend or side effects). The second run's
// script contains NO child responses, so a re-run would derail the script.
func TestSubagentReattachSkipsRerun(t *testing.T) {
	store := newMemSessionStore()
	first := durableSubagentAgent(t, store, "p1",
		AssistantToolCall("c1", ToolSpawnSubagent, `{"task":"echo banana and report"}`),
		AssistantToolCall("c2", "echo", `{"text":"banana"}`),
		AssistantText("the echo returned: banana"),
		AssistantText("child reported: banana"),
	)
	if _, err := first.Prompt(context.Background(), "delegate"); err != nil {
		t.Fatalf("first Prompt: %v", err)
	}

	// The replayed parent process: same store, same session, same call ID.
	second := durableSubagentAgent(t, store, "p1",
		AssistantToolCall("c1", ToolSpawnSubagent, `{"task":"echo banana and report"}`),
		AssistantText("child reported again: banana"),
	)
	res, err := second.Prompt(context.Background(), "delegate")
	if err != nil {
		t.Fatalf("second Prompt: %v", err)
	}
	if res.Final != "child reported again: banana" {
		t.Fatalf("final = %q", res.Final)
	}
	var spawnResult string
	for _, m := range res.Messages {
		if m.Role == RoleTool && m.Name == ToolSpawnSubagent {
			spawnResult = m.Content
		}
	}
	if spawnResult != "the echo returned: banana" {
		t.Fatalf("reattach must return the recorded child answer, got %q", spawnResult)
	}
}

// TestSubagentResumesInterruptedChild verifies a spawn call replayed against a
// child log that crashed mid-run RESUMES that child from its own history — the
// recovered transcript (dangling call closed with an interrupted note) reaches
// the model, and the seeds are not persisted twice.
func TestSubagentResumesInterruptedChild(t *testing.T) {
	ctx := context.Background()
	store := newMemSessionStore()
	// Hand-build the crashed child log: the seed task and an assistant turn whose
	// echo call never got a result (the process died mid-tool).
	for _, e := range []SessionEntry{
		{Kind: EntryMessage, Message: &Message{Role: RoleUser, Content: "echo banana and report"}},
		{Kind: EntryMessage, Message: &Message{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "x1", Name: "echo", Arguments: `{"text":"banana"}`}}}},
	} {
		if err := store.Append(ctx, "p1/c1", e); err != nil {
			t.Fatal(err)
		}
	}

	provider := NewFauxProvider(
		AssistantToolCall("c1", ToolSpawnSubagent, `{"task":"echo banana and report"}`),
		// Child resume turn: sees the recovered transcript, answers directly.
		AssistantText("recovered: banana was already echoed"),
		AssistantText("child recovered fine"),
	)
	agent, err := New(Config{
		Provider:  provider,
		Model:     "faux-1",
		Tools:     NewToolSet(&echoTool{name: "echo"}),
		Policy:    NewAllowList("echo", ToolSpawnSubagent),
		Subagents: &SubagentSettings{},
		Session:   store,
		SessionID: "p1",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	res, err := agent.Prompt(ctx, "delegate")
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if res.Final != "child recovered fine" {
		t.Fatalf("final = %q", res.Final)
	}

	// The child's provider request carried the recovered history: the original
	// task plus the synthesized interrupted note closing the dangling call.
	childReq := provider.Recorded[1]
	var sawTask, sawInterrupted bool
	for _, m := range childReq.Messages {
		if strings.Contains(m.Content, "echo banana and report") {
			sawTask = true
		}
		if m.Role == RoleTool && strings.Contains(m.Content, "[interrupted:") {
			sawInterrupted = true
		}
	}
	if !sawTask || !sawInterrupted {
		t.Fatalf("resumed child missing recovered history (task=%v interrupted=%v): %+v", sawTask, sawInterrupted, childReq.Messages)
	}

	// The child log now completes, and the seed task appears exactly once —
	// resume must not double-persist the history it recovered.
	childLog, _ := store.Log(ctx, "p1/c1")
	rs := ReduceSession(childLog)
	if !rs.Completed {
		t.Fatal("resumed child log must reduce Completed")
	}
	seeds := 0
	for _, e := range childLog {
		if e.Kind == EntryMessage && e.Message != nil && e.Message.Content == "echo banana and report" {
			seeds++
		}
	}
	if seeds != 1 {
		t.Fatalf("seed task persisted %d times, want 1", seeds)
	}
}

// TestSubagentStorelessParentKeepsEphemeralChild pins the old behavior for
// non-durable runs: no session store, no child log, everything still works.
func TestSubagentStorelessParentKeepsEphemeralChild(t *testing.T) {
	agent, _ := subagentAgent(t, &SubagentSettings{},
		AssistantToolCall("c1", ToolSpawnSubagent, `{"task":"echo banana and report"}`),
		AssistantToolCall("c2", "echo", `{"text":"banana"}`),
		AssistantText("the echo returned: banana"),
		AssistantText("child reported: banana"),
	)
	res, err := agent.Prompt(context.Background(), "delegate")
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if res.Final != "child reported: banana" {
		t.Fatalf("final = %q", res.Final)
	}
}
