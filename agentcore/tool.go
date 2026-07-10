package agentcore

import (
	"context"
	"fmt"
	"unicode/utf8"
)

// Tool is a single capability the agent can invoke. Implementations are
// provided by the consumer (host-injected): the user's Agent Definition may
// reference and permit tools but can never conjure new ones, keeping the
// capability surface under code control.
type Tool interface {
	// Name is the stable identifier the model calls.
	Name() string
	// Schema advertises the tool to the model (JSON Schema parameters).
	Schema() ToolSchema
	// Run executes with validated JSON arguments and returns a result string
	// (already truncated by the loop before reaching the model).
	Run(ctx context.Context, args string) (string, error)
}

// ArgPreparer is an optional Tool capability (pi's prepareArguments): it
// normalizes the raw JSON argument string before validation and execution —
// defaulting fields, coercing shapes the model commonly gets wrong. A tool that
// does not implement it runs with the model's arguments verbatim.
type ArgPreparer interface {
	PrepareArguments(raw string) string
}

// StreamingTool is an optional Tool capability (pi's tool_execution_update): a
// long-running tool may emit partial output as it works via the supplied emit
// callback, which the loop forwards to the stream sink so a viewer sees progress
// before the tool finishes. The returned string is still the authoritative final
// result (identical to Run's), and a tool that does not implement this runs
// through Run unchanged. emit is a no-op on a non-streaming run.
type StreamingTool interface {
	RunStreaming(ctx context.Context, args string, emit func(partial string)) (string, error)
}

// ParallelTool is an optional Tool capability: a tool whose Parallel() returns
// true may be executed concurrently with the other parallel-eligible tool calls
// in the same assistant turn (pi's executionMode). The safe default is
// sequential — tools that mutate state or depend on call ordering must NOT
// implement this, so concurrency is opt-in per read-only tool.
type ParallelTool interface {
	Parallel() bool
}

// ToolSet is an ordered registry of tools keyed by name.
type ToolSet struct {
	order []string
	byKey map[string]Tool
}

// NewToolSet builds a registry from the given tools, preserving order.
func NewToolSet(tools ...Tool) *ToolSet {
	ts := &ToolSet{byKey: make(map[string]Tool, len(tools))}
	for _, t := range tools {
		ts.Add(t)
	}
	return ts
}

// Add registers a tool, overwriting any existing tool of the same name.
func (ts *ToolSet) Add(t Tool) {
	if _, exists := ts.byKey[t.Name()]; !exists {
		ts.order = append(ts.order, t.Name())
	}
	ts.byKey[t.Name()] = t
}

// Get returns the tool of the given name, if registered.
func (ts *ToolSet) Get(name string) (Tool, bool) {
	t, ok := ts.byKey[name]
	return t, ok
}

// Schemas returns the advertised schemas in registration order.
func (ts *ToolSet) Schemas() []ToolSchema {
	out := make([]ToolSchema, 0, len(ts.order))
	for _, name := range ts.order {
		out = append(out, ts.byKey[name].Schema())
	}
	return out
}

// Names returns tool names in registration order.
func (ts *ToolSet) Names() []string {
	out := make([]string, len(ts.order))
	copy(out, ts.order)
	return out
}

// defaultMaxToolResultBytes bounds a tool result before it reaches the LLM —
// token + safety guard (pi harness truncate.ts, rebuilt UTF-8-safe).
const defaultMaxToolResultBytes = 24 * 1024

// truncateBytes trims s to at most maxBytes without splitting a UTF-8 rune,
// appending a marker when it cuts. A maxBytes <= 0 disables truncation.
func truncateBytes(s string, maxBytes int) string {
	if maxBytes <= 0 || len(s) <= maxBytes {
		return s
	}
	const marker = "\n…[truncated]"
	budget := maxBytes - len(marker)
	if budget <= 0 {
		budget = maxBytes
	}
	// Back off to a rune boundary.
	for budget > 0 && !utf8.RuneStart(s[budget]) {
		budget--
	}
	return s[:budget] + marker
}

// truncateMiddle trims an oversized string to at most maxBytes by cutting its
// MIDDLE out, keeping the head and the tail verbatim with an omission marker
// between them. Tool results get this shape (not head-only truncateBytes)
// because the end of a long result usually carries the signal — the error after
// pages of build output, a query's final rows, a stack trace's root cause. The
// head gets roughly two thirds of the budget, the tail one third; cuts never
// split a UTF-8 rune. A maxBytes <= 0 disables truncation.
func truncateMiddle(s string, maxBytes int) string {
	if maxBytes <= 0 || len(s) <= maxBytes {
		return s
	}
	// Reserve room for the marker (its digits vary; 64 covers any length).
	const reserve = 64
	budget := maxBytes - reserve
	if budget < 2 {
		return truncateBytes(s, maxBytes)
	}
	head := budget * 2 / 3
	for head > 0 && !utf8.RuneStart(s[head]) {
		head--
	}
	start := len(s) - (budget - head)
	for start < len(s) && !utf8.RuneStart(s[start]) {
		start++
	}
	return s[:head] + fmt.Sprintf("\n…[%d bytes truncated]…\n", start-head) + s[start:]
}
