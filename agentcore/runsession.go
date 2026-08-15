package agentcore

import "context"

// Which agent is making this call?
//
// A spawned sub-agent shares its parent's provider (Fork copies it) and its
// parent's context, so anything decorating the provider — pricing, tracing,
// metering — sees the parent's calls and every descendant's calls arrive
// through the same object, in interleaved order, with nothing to tell them
// apart. A run that delegated three hundred tasks therefore reads back as one
// agent that inexplicably did all of it itself, and the six hundred child calls
// inflate the parent's own turn count.
//
// The run's session id is the natural discriminator: the loop already derives a
// distinct one per child ("<parent session>/<tool call id>"), and it is the same
// key the durable log is written under, so a trace tagged with it lines up with
// the log row for row. Depth (delegation.go) says how deep; this says which.
//
// It rides the context for the same reason depth does: a child is a different
// Agent value, and ctx is the only thread that survives the hop.

// runSessionKey carries the session id of the run currently making a call.
type runSessionKey struct{}

// WithRunSession tags ctx with the session id of the run about to execute. The
// loop sets it once per run, before the first turn; an empty id is a no-op, so
// an in-memory run (which has no session) leaves the tag absent rather than
// stamping a meaningless empty string over an outer one.
func WithRunSession(ctx context.Context, id string) context.Context {
	if id == "" {
		return ctx
	}
	return context.WithValue(ctx, runSessionKey{}, id)
}

// RunSessionFrom returns the session id of the run making the current call, or
// "" outside any run (a connectivity probe, a classifier call) and on a run with
// no durable session. Consumers must treat "" as "attribute it to the run", not
// as an error.
func RunSessionFrom(ctx context.Context) string {
	if v, ok := ctx.Value(runSessionKey{}).(string); ok {
		return v
	}
	return ""
}
