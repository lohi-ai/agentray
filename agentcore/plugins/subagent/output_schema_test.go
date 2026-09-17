package subagent_test

import (
	"context"
	"strings"
	"testing"

	"github.com/lohi-ai/agentray/agentcore"
	"github.com/lohi-ai/agentray/agentcore/plugins/subagent"
)

const fruitSchema = `{"type":"object","required":["fruit"],"properties":{"fruit":{"type":"string"}},"additionalProperties":false}`

// spawnResult extracts the spawn_subagent tool result from a parent transcript.
func spawnResult(t *testing.T, msgs []agentcore.Message) string {
	t.Helper()
	for _, m := range msgs {
		if m.Role == agentcore.RoleTool && m.Name == subagent.ToolSpawnSubagent {
			return m.Content
		}
	}
	t.Fatal("no spawn_subagent tool result in parent transcript")
	return ""
}

// TestSubagentOutputSchemaAccept: a child whose final answer satisfies
// output_schema returns it to the parent verbatim, and the child's task
// carried the schema instruction.
func TestSubagentOutputSchemaAccept(t *testing.T) {
	agent, provider := subagentAgent(t, &subagent.Plugin{},
		AssistantToolCall("c1", subagent.ToolSpawnSubagent,
			`{"task":"name a fruit","output_schema":`+fruitSchema+`}`),
		agentcore.AssistantText(`{"fruit":"banana"}`),
		agentcore.AssistantText("child said banana"),
	)
	res, err := agent.Prompt(context.Background(), "delegate")
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if got := spawnResult(t, res.Messages); got != `{"fruit":"banana"}` {
		t.Fatalf("spawn result = %q", got)
	}
	// The child's seeded task carried the schema and the bare-JSON contract.
	childReq := provider.Recorded[1]
	var sawSchema bool
	for _, m := range childReq.Messages {
		if strings.Contains(m.Content, `"required":["fruit"]`) && strings.Contains(m.Content, "JSON Schema") {
			sawSchema = true
		}
	}
	if !sawSchema {
		t.Fatalf("child task missing output_schema instruction: %+v", childReq.Messages)
	}
}

// TestSubagentOutputSchemaRejectRetry: a schema-violating answer re-opens the
// child once with the validation error; the corrected answer is what the
// parent receives.
func TestSubagentOutputSchemaRejectRetry(t *testing.T) {
	agent, provider := subagentAgent(t, &subagent.Plugin{},
		AssistantToolCall("c1", subagent.ToolSpawnSubagent,
			`{"task":"name a fruit","output_schema":`+fruitSchema+`}`),
		// First child answer: prose, not the required JSON object.
		agentcore.AssistantText("the fruit is banana"),
		// Re-opened child sees its transcript + the error and corrects.
		agentcore.AssistantText(`{"fruit":"banana"}`),
		agentcore.AssistantText("child said banana"),
	)
	res, err := agent.Prompt(context.Background(), "delegate")
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if got := spawnResult(t, res.Messages); got != `{"fruit":"banana"}` {
		t.Fatalf("spawn result = %q, want the corrected JSON", got)
	}
	// The retry request carried the rejected answer and the validation error.
	retryReq := provider.Recorded[2]
	var sawRejected, sawError bool
	for _, m := range retryReq.Messages {
		if strings.Contains(m.Content, "the fruit is banana") {
			sawRejected = true
		}
		if strings.Contains(m.Content, "failed output_schema validation") {
			sawError = true
		}
	}
	if !sawRejected || !sawError {
		t.Fatalf("retry child missing transcript/error (rejected=%v error=%v): %+v", sawRejected, sawError, retryReq.Messages)
	}
}

// TestSubagentOutputSchemaRetryAlsoFails: when the retry also violates the
// schema the parent gets the raw answer marked validation-failed — not an
// error, not silent acceptance.
func TestSubagentOutputSchemaRetryAlsoFails(t *testing.T) {
	agent, _ := subagentAgent(t, &subagent.Plugin{},
		AssistantToolCall("c1", subagent.ToolSpawnSubagent,
			`{"task":"name a fruit","output_schema":`+fruitSchema+`}`),
		agentcore.AssistantText("the fruit is banana"),
		agentcore.AssistantText(`{"fruit":42}`),
		agentcore.AssistantText("done"),
	)
	res, err := agent.Prompt(context.Background(), "delegate")
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	got := spawnResult(t, res.Messages)
	if !strings.Contains(got, `{"fruit":42}`) || !strings.Contains(got, "[validation failed:") {
		t.Fatalf("spawn result = %q, want raw answer + validation-failed note", got)
	}
}

// TestSubagentOutputSchemaAbsentUnchanged: without output_schema the child is
// not given the JSON contract and its answer passes through untouched.
func TestSubagentOutputSchemaAbsentUnchanged(t *testing.T) {
	agent, provider := subagentAgent(t, &subagent.Plugin{},
		AssistantToolCall("c1", subagent.ToolSpawnSubagent, `{"task":"name a fruit"}`),
		agentcore.AssistantText("banana, obviously"),
		agentcore.AssistantText("done"),
	)
	res, err := agent.Prompt(context.Background(), "delegate")
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if got := spawnResult(t, res.Messages); got != "banana, obviously" {
		t.Fatalf("spawn result = %q", got)
	}
	for _, m := range provider.Recorded[1].Messages {
		if strings.Contains(m.Content, "JSON Schema") {
			t.Fatalf("schema instruction leaked into schema-less spawn: %q", m.Content)
		}
	}
}

// TestSubagentOutputSchemaInvalidArg: a malformed schema fails the call before
// any child runs — no provider call is spent on the child.
func TestSubagentOutputSchemaInvalidArg(t *testing.T) {
	agent, provider := subagentAgent(t, &subagent.Plugin{},
		AssistantToolCall("c1", subagent.ToolSpawnSubagent,
			`{"task":"name a fruit","output_schema":{"type":"bogus-type"}}`),
		agentcore.AssistantText("spawn failed as expected"),
	)
	res, err := agent.Prompt(context.Background(), "delegate")
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if got := spawnResult(t, res.Messages); !strings.Contains(got, "output_schema") {
		t.Fatalf("spawn result = %q, want an output_schema error", got)
	}
	// Only the parent's two turns hit the provider — no child call.
	if len(provider.Recorded) != 2 {
		t.Fatalf("provider calls = %d, want 2 (no child run)", len(provider.Recorded))
	}
}

// TestSubagentOutputSchemaReplayReattaches: a durable spawn that needed the
// retry still replays cleanly — the re-issued spawn reattaches to the recorded
// child logs (the invalid first answer, then the completed retry) and returns
// the validated JSON with no new provider calls for the children.
func TestSubagentOutputSchemaReplayReattaches(t *testing.T) {
	store := agentcore.NewMemorySessionStore()
	first := durableSubagentAgent(t, store, "p1",
		AssistantToolCall("c1", subagent.ToolSpawnSubagent,
			`{"task":"name a fruit","output_schema":`+fruitSchema+`}`),
		agentcore.AssistantText("the fruit is banana"),
		agentcore.AssistantText(`{"fruit":"banana"}`),
		agentcore.AssistantText("child said banana"),
	)
	if _, err := first.Prompt(context.Background(), "delegate"); err != nil {
		t.Fatalf("first Prompt: %v", err)
	}
	// Both child logs exist: the rejected run and the retry run.
	if rs := agentcore.ReduceSession(mustLog(t, store, "p1/c1")); !rs.Completed {
		t.Fatal("first child log must reduce Completed")
	}
	if rs := agentcore.ReduceSession(mustLog(t, store, "p1/c1/retry")); !rs.Completed {
		t.Fatal("retry child log must reduce Completed")
	}

	// The replayed parent: same store, same session, same call ID. Its script
	// has NO child responses — any child re-run would derail it.
	second := durableSubagentAgent(t, store, "p1",
		AssistantToolCall("c1", subagent.ToolSpawnSubagent,
			`{"task":"name a fruit","output_schema":`+fruitSchema+`}`),
		agentcore.AssistantText("child reported again: banana"),
	)
	res, err := second.Prompt(context.Background(), "delegate")
	if err != nil {
		t.Fatalf("second Prompt: %v", err)
	}
	if res.Final != "child reported again: banana" {
		t.Fatalf("final = %q", res.Final)
	}
	if got := spawnResult(t, res.Messages); got != `{"fruit":"banana"}` {
		t.Fatalf("replayed spawn result = %q, want the validated JSON", got)
	}
}

func mustLog(t *testing.T, store agentcore.SessionStore, id string) []agentcore.SessionEntry {
	t.Helper()
	log, err := store.Log(context.Background(), id)
	if err != nil {
		t.Fatalf("log %s: %v", id, err)
	}
	if len(log) == 0 {
		t.Fatalf("log %s is empty", id)
	}
	return log
}
