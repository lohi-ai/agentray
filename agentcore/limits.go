package agentcore

// The bounds of one run.
//
// Limits are read by the loop every turn and handed to extensions through
// RunInfo, so a capability can size its own behavior to the run it is in
// without asking the Agent. They are core because there is no composition in
// which a run is unbounded.

// Limits bound a run so long autonomous loops stay safe and cheap (§7).
type Limits struct {
	MaxTurns         int // hard cap on LLM calls
	MaxToolCalls     int // hard cap on tool executions across the run
	MaxToolResultLen int // byte cap per tool result before it reaches the LLM
	MaxContextTokens int // soft budget; old turns are compacted above it (§5.2)
}

// DefaultLimits are conservative caps suitable for v1.
func DefaultLimits() Limits {
	return Limits{MaxTurns: 12, MaxToolCalls: 24, MaxToolResultLen: defaultMaxToolResultBytes, MaxContextTokens: defaultContextTokenBudget}
}
