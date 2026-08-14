package agentcore

import "context"

// Delegation depth is core, not a plugin concern, even though nothing in the
// core loop spawns anything.
//
// The reason is the cap: a delegation plugin bounds recursion by refusing to
// advertise its spawn tool past a maximum depth, and that only works if the
// depth an A -> B -> A cycle has reached survives crossing an agent boundary
// where the two agents may be composed with DIFFERENT plugins. So the counter
// rides the context, the loop reads it into RunInfo.Depth, and every extension
// sees the same number.

// delegationDepthKey carries how many delegation hops deep the current run is.
// Depth travels on ctx — not on the Agent — because a cross-agent delegate is a
// freshly built Agent whose struct fields know nothing of the caller; ctx is
// the only thread that survives the hop, so it is what stops A→B→A recursion.
type delegationDepthKey struct{}

// DelegationDepth returns the current delegation depth (0 for a top-level run).
func DelegationDepth(ctx context.Context) int {
	if v, ok := ctx.Value(delegationDepthKey{}).(int); ok {
		return v
	}
	return 0
}

// WithDelegationDepth returns ctx marked as depth hops deep. Consumers normally
// never call this — the spawn tool wraps the child ctx itself — but a consumer
// embedding a run inside another delegation system may seed a floor.
func WithDelegationDepth(ctx context.Context, depth int) context.Context {
	return context.WithValue(ctx, delegationDepthKey{}, depth)
}
