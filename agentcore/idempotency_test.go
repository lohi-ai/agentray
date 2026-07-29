package agentcore

import (
	"context"
	"strings"
	"testing"
)

// runKeyCapture executes one agent run whose single tool call records the
// idempotency key it was handed, returning (key, ok).
func runKeyCapture(t *testing.T, sessionID string) (string, bool) {
	t.Helper()
	var key string
	var ok bool
	tool := funcTool{
		name: "effect",
		run: func(ctx context.Context, _ string) (string, error) {
			key, ok = IdempotencyKey(ctx)
			return "done", nil
		},
	}
	cfg := Config{
		Provider: NewFauxProvider(
			AssistantToolCall("c1", "effect", `{}`),
			AssistantText("ok"),
		),
		Model:  "test",
		Tools:  NewToolSet(tool),
		Policy: NewAllowList("effect"),
	}
	if sessionID != "" {
		cfg.Session = newMemSessionStore()
		cfg.SessionID = sessionID
	}
	agent, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := agent.Prompt(context.Background(), "start"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	return key, ok
}

// TestIdempotencyKeyStableAcrossResume pins the dedupe contract: the same
// logical call — same session, same tool-call ID, exactly what RecoverSession
// replays after a crash — must resolve to the same key in a fresh process, so
// an external system can drop the duplicated side effect.
func TestIdempotencyKeyStableAcrossResume(t *testing.T) {
	k1, ok1 := runKeyCapture(t, "sess-A")
	k2, ok2 := runKeyCapture(t, "sess-A") // fresh agent + store: simulates the resumed process
	if !ok1 || !ok2 {
		t.Fatalf("durable runs must hand tools a key (ok1=%v ok2=%v)", ok1, ok2)
	}
	if !strings.HasPrefix(k1, "ik_") {
		t.Fatalf("key %q missing ik_ prefix", k1)
	}
	if k1 != k2 {
		t.Fatalf("same (session, call) produced different keys: %q vs %q", k1, k2)
	}
	// A different session must never collide, or cross-run effects would dedupe
	// against each other.
	k3, _ := runKeyCapture(t, "sess-B")
	if k3 == k1 {
		t.Fatalf("different sessions produced the same key %q", k1)
	}
}

// TestIdempotencyKeyAbsentWithoutSession verifies a storeless run hands out no
// key: without a durable log there is no replay, and a key that changed every
// process restart would be worse than none.
func TestIdempotencyKeyAbsentWithoutSession(t *testing.T) {
	key, ok := runKeyCapture(t, "")
	if ok || key != "" {
		t.Fatalf("storeless run leaked a key: %q ok=%v", key, ok)
	}
}

// TestIdempotencyKeyOnTrace verifies the key is persisted on the tool trace so
// an external side effect can be correlated back to the logical call.
func TestIdempotencyKeyOnTrace(t *testing.T) {
	agent, err := New(Config{
		Provider: NewFauxProvider(
			AssistantToolCall("c1", "noop", `{}`),
			AssistantText("ok"),
		),
		Model:     "test",
		Tools:     NewToolSet(noopTool{}),
		Policy:    NewAllowList("noop"),
		Session:   newMemSessionStore(),
		SessionID: "sess-trace",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	res, err := agent.Prompt(context.Background(), "start")
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	var traced string
	for _, tr := range res.Tools {
		if tr.Tool == "noop" {
			traced = tr.IdempotencyKey
		}
	}
	if traced == "" {
		t.Fatal("executed tool trace missing idempotency key")
	}
	if traced != toolIdempotencyKey("sess-trace", "c1") {
		t.Fatalf("trace key %q does not match derivation", traced)
	}
}
