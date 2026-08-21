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

// DefaultLimits are the caps a run gets when nobody says otherwise.
//
// The turn/tool numbers are measured, not guessed: at MaxTurns 12 an analytics
// agent answering a real question ("which feature drives retention?") ran out
// of budget on 2 of 3 first-run attempts — schema discovery, a couple of
// exploratory queries and one correction already spend a dozen turns before the
// answer is written. 24 turns / 40 tool calls clears that with room for a wrong
// turn.
//
// The graceful wrap-up turn a ceiling now triggers is a floor, not a substitute
// for enough budget: it buys an honest partial answer, never the answer. Size
// the budget so the wrap-up stays the exception.
func DefaultLimits() Limits {
	return Limits{MaxTurns: 24, MaxToolCalls: 40, MaxToolResultLen: defaultMaxToolResultBytes, MaxContextTokens: defaultContextTokenBudget}
}
