package agentcore

import (
	"context"
	"fmt"
	"time"
)

// Executing one tool call: lookup -> prepare -> validate -> gate -> run ->
// bound -> trace.
//
// This is the trust boundary of the whole runtime. Every rule that decides
// whether a call happens at all, what the model is allowed to see of its
// result, and what the durable trace records about it, is applied here and
// nowhere else — which is what makes the gate unskippable rather than
// merely usually-called.

// maxToolFailures is how many times in a row one tool may error within a single
// run before the loop disables it for the remainder of that run (a per-run
// circuit breaker). A successful execution resets the tool's counter.
const maxToolFailures = 3

// callTool executes a tool — streaming when an emit callback is supplied,
// otherwise plain Run — inside panic recovery. A panicking tool is converted to
// an error so one broken tool degrades to a normal error result instead of
// crashing the run (or, in the parallel dispatch path, the process).
func callTool(ctx context.Context, tool Tool, args string, emit func(partial string)) (out string, err error) {
	defer func() {
		if r := recover(); r != nil {
			out, err = "", fmt.Errorf("tool panicked: %v", r)
		}
	}()
	if st, ok := tool.(StreamingTool); ok && emit != nil {
		return st.RunStreaming(ctx, args, emit)
	}
	return tool.Run(ctx, args)
}

// ToolTrace is a persisted projection of one tool execution (§9
// agent_tool_calls): tool name, validated args, whether it was allowed, and
// result metadata.
type ToolTrace struct {
	// CallID is the provider's id for this specific invocation. It is what makes a
	// trace addressable: two concurrent calls to the same tool differ in nothing
	// else, so a consumer keying on name (or on array position, which shifts as
	// the list grows) reconciles the wrong one onto the other. Empty only for a
	// synthesized trace with no originating model call.
	CallID     string `json:"call_id,omitempty"`
	Tool       string `json:"tool"`
	Args       string `json:"args"`
	Allowed    bool   `json:"allowed"`
	Reason     string `json:"reason,omitempty"`
	Error      string `json:"error,omitempty"`
	ResultMeta string `json:"result_meta,omitempty"`
	LatencyMS  int64  `json:"latency_ms,omitempty"` // wall-clock of the tool execution (0 when not executed)
	// IdempotencyKey is the framework-derived dedupe key handed to the tool
	// (empty on runs without a durable session). Persisting it lets an external
	// side effect be correlated back to the exact logical call that caused it.
	IdempotencyKey string `json:"idempotency_key,omitempty"`
	// SpillLocator is set when the result was too large for the context and its
	// full text was saved to a spill artifact (plugins/spill). It makes the complete
	// output recoverable from the trace long after the run, not just by the model
	// mid-run — an oversized result is no longer lost to observability either.
	SpillLocator string `json:"spill_locator,omitempty"`
}

// ToolDenialReason is why a tool call was refused (ToolTrace.Reason).
// Distinct from RunResult.StopReason — they share the "aborted" spelling
// by coincidence (a cancelled run stops with StopReason "aborted" AND
// remaining tool calls are denied with this reason). The JSON/wire value
// is the existing literal so rows already written keep classifying.
type ToolDenialReason string

const (
	// ToolDenialAborted is the loop's denial reason when a remaining call
	// is short-circuited because the run was cancelled (agentcore/loop.go).
	// ClassifyTool buckets this as ToolAborted; the loop itself also
	// compares against it to decide whether a result is settled enough to
	// persist. A typed constant so those two sites cannot drift from a typo.
	ToolDenialAborted ToolDenialReason = "aborted"
)

// DeniedAborted reports whether this trace is the loop's cancel-denial.
// Historical rows carry the bare string "aborted"; the comparison is on
// that wire value.
func (t ToolTrace) DeniedAborted() bool {
	return t.Reason == string(ToolDenialAborted)
}

// toolOutcome is the result of executing one tool call: the persisted trace, the
// tool-result message fed back to the model, whether the run should terminate,
// and whether it counted against the tool-call budget (only real executions do).
type toolOutcome struct {
	trace     ToolTrace
	message   Message
	terminate bool
	executed  bool
	// extra are messages an extension asked to add because of this call. They
	// are buffered and appended by the caller AFTER every result in the batch,
	// so tool-call/result adjacency is never broken.
	extra []Message
}

// runToolCall takes a single model tool call through lookup -> prepareArguments
// -> validate -> beforeToolCall (permission gate + hooks) -> execute ->
// afterToolCall. It touches no shared run state, so it is safe to call
// concurrently for parallel-eligible tools; budget counting and trace recording
// happen in the caller, in order.
func (a *Agent) runToolCall(ctx context.Context, exts *extensionSet, exempt map[string]bool, tools *ToolSet, call ToolCall, limits Limits, emitUpdate func(ToolCall, string)) toolOutcome {
	trace := ToolTrace{CallID: call.ID, Tool: call.Name, Args: call.Arguments}

	tool, ok := tools.Get(call.Name)
	if !ok {
		trace.Allowed = true
		trace.Error = "unknown tool"
		return toolOutcome{trace: trace, message: toolResult(call, "error: unknown tool "+call.Name)}
	}

	// Normalize arguments before validation (optional per-tool prepareArguments).
	args := call.Arguments
	if p, ok := tool.(ArgPreparer); ok {
		args = p.PrepareArguments(args)
	}
	trace.Args = args
	gated := call
	gated.Arguments = args

	// Validate arguments against the tool's schema before any hook runs.
	if err := validateArgs(args, tool.Schema().Parameters); err != nil {
		trace.Allowed = false
		trace.Error = err.Error()
		return toolOutcome{trace: trace, message: toolResult(call, "invalid arguments: "+err.Error())}
	}

	// beforeToolCall preflight (permission gate + consumer hooks). Two built-ins
	// bypass it because neither can reach anything the agent does not already
	// have: read_skill only returns definition-authored skill bodies, and
	// read_spill only returns output THIS session produced and had truncated
	// away (the store is fenced to the run's session id). Requiring every
	// marketplace preset to enumerate them would be friction with no security
	// value.
	if !gateExemptTools[call.Name] && !exempt[call.Name] {
		if d := a.hooks.runBefore(ctx, gated); !d.Allow {
			trace.Allowed = false
			trace.Reason = d.Reason
			return toolOutcome{trace: trace, message: toolResult(call, "blocked: "+d.Reason)}
		}
	}

	// Resolve credential placeholders at the trust boundary: the call was traced
	// (trace.Args) and gated above in {{cred:NAME}} placeholder form, so the
	// model and the persisted trace never see the literal. Only runArgs — handed
	// straight to the tool — carries the resolved secret. A resolver error fails
	// closed: the call is blocked and the reason fed back to the model.
	runArgs := args
	if a.env.Credentials != nil {
		resolved, err := a.env.Credentials.Resolve(ctx, args)
		if err != nil {
			trace.Allowed = false
			trace.Reason = err.Error()
			return toolOutcome{trace: trace, message: toolResult(call, "blocked: "+err.Error())}
		}
		runArgs = resolved
	}

	// A StreamingTool emits partials as it works (forwarded via emitUpdate); a
	// plain tool runs through Run. Either way out is the authoritative result.
	// callTool wraps execution in panic recovery so a misbehaving tool degrades
	// to an ordinary error result instead of crashing the run (or, on the parallel
	// dispatch path, the whole process).
	var emit func(partial string)
	if emitUpdate != nil {
		emit = func(partial string) { emitUpdate(gated, partial) }
	}
	// Hand the tool its crash-stable idempotency key: (sessionID, call ID) is
	// replayed verbatim by RecoverSession, so a re-run after a crash presents
	// the same key and the external system can dedupe the effect.
	ikey := toolIdempotencyKey(a.sessionID, call.ID)
	trace.IdempotencyKey = ikey
	execStart := time.Now()
	out, runErr := callTool(withToolCallID(withIdempotencyKey(ctx, ikey), call.ID), tool, runArgs, emit)
	trace.LatencyMS = time.Since(execStart).Milliseconds()
	// Bound the result for the model. Interceptors see the RAW output first,
	// because a lossless bounding strategy (persist the whole thing, hand back a
	// preview plus a locator) cannot be built on top of an already-truncated
	// string. Whichever one replaces the result owns the bound; if none does,
	// the loop falls back to head+tail truncation — keeping the beginning AND
	// end, because the end often carries the signal (a shell error after pages
	// of build output, the final rows of a query, a stack trace's cause) — and
	// the middle is gone for good. The loop does not know which extension, if
	// any, took the job.
	out, meta, extra, replaced, extTerm := exts.interceptToolResult(ctx, gated, out, runErr)
	if !replaced {
		out = truncateMiddle(out, limits.MaxToolResultLen)
	}
	trace.SpillLocator = meta
	out, term := a.hooks.runAfter(ctx, gated, out, runErr)
	term = term || extTerm

	trace.Allowed = true
	if runErr != nil {
		trace.Error = runErr.Error()
		out = "error: " + runErr.Error()
	}
	trace.ResultMeta = fmt.Sprintf("%d bytes in %dms", len(out), trace.LatencyMS)
	return toolOutcome{trace: trace, message: toolResult(call, out), terminate: term, executed: true, extra: extra}
}

// isParallelTool reports whether one call targets a registered tool that opts
// into concurrent execution (ParallelTool). Unknown tools and tools that don't
// opt in run sequentially — the safe default.
func isParallelTool(tools *ToolSet, call ToolCall) bool {
	tool, ok := tools.Get(call.Name)
	if !ok {
		return false
	}
	p, ok := tool.(ParallelTool)
	return ok && p.Parallel()
}

// toolResult builds a tool-role message linked to its call.
func toolResult(call ToolCall, content string) Message {
	return Message{Role: RoleTool, ToolCallID: call.ID, Name: call.Name, Content: content}
}
