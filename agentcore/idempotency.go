package agentcore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
)

// Idempotency keys give tools with external side effects (payments, emails,
// webhooks, row inserts) a dedupe handle that survives a crash-resume. The
// framework derives the key from (sessionID, toolCallID): both are already in
// the durable log, and RecoverSession replays a dangling retry-safe call with
// its original ToolCall.ID, so the SAME logical call always resolves to the
// SAME key — a re-run after a crash presents the identical key to the external
// system, which can then drop the duplicate. Runs without a durable session get
// no key: there is no replay without a log, and a key that isn't stable across
// process restarts would only invite tools to trust it.

// idempotencyKeyCtx is the context key carrying the current invocation's key.
type idempotencyKeyCtx struct{}

// toolIdempotencyKey derives the stable key for one tool invocation. Hashing
// keeps the key fixed-length and opaque (session IDs may embed user-visible
// naming), sized for external APIs' idempotency-key fields.
func toolIdempotencyKey(sessionID, callID string) string {
	if sessionID == "" || callID == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(sessionID + "\x00" + callID))
	return "ik_" + hex.EncodeToString(sum[:16])
}

// withIdempotencyKey stamps the invocation key onto the context handed to the
// tool. A key of "" leaves the context unchanged.
func withIdempotencyKey(ctx context.Context, key string) context.Context {
	if key == "" {
		return ctx
	}
	return context.WithValue(ctx, idempotencyKeyCtx{}, key)
}

// IdempotencyKey returns the framework-generated idempotency key for the
// current tool invocation, for use inside Tool.Run. ok is false on a run
// without a durable session — a tool performing a non-idempotent external
// effect should treat that as "no crash-retry protection", not an error.
func IdempotencyKey(ctx context.Context) (key string, ok bool) {
	key, ok = ctx.Value(idempotencyKeyCtx{}).(string)
	return key, ok && key != ""
}

// toolCallIDCtx carries the provider-assigned ID of the current tool call. The
// spawn tool derives the child's deterministic session ID from it, so a
// replayed spawn call reattaches to the same child session (pi's deterministic
// child-session IDs from (parentSessionId, toolCallId)).
type toolCallIDCtx struct{}

// withToolCallID stamps the current tool call's ID onto the context handed to
// the tool. An empty id leaves the context unchanged.
func withToolCallID(ctx context.Context, id string) context.Context {
	if id == "" {
		return ctx
	}
	return context.WithValue(ctx, toolCallIDCtx{}, id)
}

// ToolCallID returns the provider-assigned ID of the current tool call, for
// use inside Tool.Run. ok is false when the tool was invoked outside the loop.
func ToolCallID(ctx context.Context) (id string, ok bool) {
	id, ok = ctx.Value(toolCallIDCtx{}).(string)
	return id, ok && id != ""
}
