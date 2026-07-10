package agentcore

import (
	"context"
	"fmt"
	"sync"
	"time"
)

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
	Tool       string `json:"tool"`
	Args       string `json:"args"`
	Allowed    bool   `json:"allowed"`
	Reason     string `json:"reason,omitempty"`
	Error      string `json:"error,omitempty"`
	ResultMeta string `json:"result_meta,omitempty"`
	LatencyMS  int64  `json:"latency_ms,omitempty"` // wall-clock of the tool execution (0 when not executed)
}

// budgetExhaustedSteer is injected as a final user turn when the budget gate
// trips, instructing the model to wrap up. Tools are stripped for this turn so it
// can only produce a text summary before the run stops.
const budgetExhaustedSteer = "Your run budget for this period has been exhausted. Do not call any more tools. Summarize the progress you have made so far and any recommended next steps in a few sentences, then stop."

// RunResult is the outcome of a run: the final assistant text, the full message
// history (working memory), the tool trace, and summed usage.
type RunResult struct {
	Final      string      `json:"final"`
	Messages   []Message   `json:"messages"`
	Tools      []ToolTrace `json:"tool_calls"`
	Usage      Usage       `json:"usage"`
	Turns      int         `json:"turns"`
	StopReason string      `json:"stop_reason"`
}

// StreamEventType classifies an incremental event emitted during a streamed run.
type StreamEventType string

const (
	StreamToken    StreamEventType = "token"    // a text fragment of the assistant's answer
	StreamTool     StreamEventType = "tool"     // a completed tool-call trace
	StreamProgress StreamEventType = "progress" // a plain-language progress note (no tool identifier)
	StreamCard     StreamEventType = "card"     // a structured result card (stat | series)

	// Granular lifecycle events (pi's event vocabulary). They are additive: a
	// consumer that only reads token/tool/progress/card keeps working, while an
	// observability layer can reconstruct turn / message / tool-execution
	// boundaries. Emitted only on a streamed run (nil sink => none).
	StreamAgentStart     StreamEventType = "agent_start"           // run begins
	StreamTurnStart      StreamEventType = "turn_start"            // a reasoning turn begins
	StreamMessageStart   StreamEventType = "message_start"         // assistant message begins (before tokens)
	StreamMessageEnd     StreamEventType = "message_end"           // assistant message complete
	StreamToolExecStart  StreamEventType = "tool_execution_start"  // a tool call begins
	StreamToolExecUpdate StreamEventType = "tool_execution_update" // a streaming tool's partial output (P8)
	StreamToolExecEnd    StreamEventType = "tool_execution_end"    // a tool call finished (carries the trace)
	StreamTurnEnd        StreamEventType = "turn_end"              // the turn (reason + act) is complete
	StreamSavePoint      StreamEventType = "save_point"            // a turn's buffered durable writes were flushed atomically
	StreamAgentEnd       StreamEventType = "agent_end"             // run ends (any exit path)
)

// ResultCard is a compact, structured answer artifact a consumer may attach to a
// streamed turn so the UI can render a stat block or a small chart instead of
// prose alone. It is deliberately product-agnostic: a title, a kind, and either
// a few stat rows or a short series of points. agentcore never builds one — a
// consumer (e.g. the orchestrator) emits it via the sink; the type lives here so
// the stream vocabulary is shared.
type ResultCard struct {
	Title  string      `json:"title"`
	Kind   string      `json:"kind"`           // "stat" | "series"
	Unit   string      `json:"unit,omitempty"` // optional label for the values
	Stats  []CardStat  `json:"stats,omitempty"`
	Points []CardPoint `json:"points,omitempty"`
}

// CardStat is one labeled metric row in a "stat" card.
type CardStat struct {
	Label string `json:"label"`
	Value string `json:"value"`
}

// CardPoint is one point in a "series" card (e.g. a time bucket count).
type CardPoint struct {
	Label string  `json:"label"`
	Value float64 `json:"value"`
}

// StreamEvent is one increment surfaced to a live viewer (the SSE chat endpoint)
// while a run is in flight. Callbacks fire from the run goroutine, in order.
// Token/Tool are emitted by the core loop; Progress/Card are emitted by a
// consumer wrapping the loop (the core never sets them).
type StreamEvent struct {
	Type  StreamEventType
	Token string      // set when Type == StreamToken
	Tool  *ToolTrace  // set when Type == StreamTool
	Note  string      // set when Type == StreamProgress
	Card  *ResultCard // set when Type == StreamCard
	Turn  int
}

// StreamSink receives StreamEvents during a streamed run. A nil sink runs the
// loop in non-streaming mode (one Chat call per turn).
type StreamSink func(StreamEvent)

// runLoop is the single-flight, observable entry point shared by every run
// method. It claims the busy guard, brackets the run with agent_start/agent_end,
// and — when the inner drive aborts on a provider or hook error — synthesizes a
// failure assistant message so a subscriber always sees a clean
// message/turn/agent lifecycle (pi's createFailureMessage) rather than a stream
// that just stops mid-turn.
func (a *Agent) runLoop(ctx context.Context, messages []Message, task string, sink StreamSink) (RunResult, error) {
	if !a.tryAcquire() {
		return RunResult{}, ErrBusy
	}
	defer a.release()

	emit := func(ev StreamEvent) {
		if sink != nil {
			sink(ev)
		}
	}

	// agent_start / agent_end bracket the whole run; agent_end fires on every exit
	// path (final answer, budget guard, abort, max_turns, error) via the deferred
	// close, which reads the final res.Turns by closure.
	emit(StreamEvent{Type: StreamAgentStart})
	var res RunResult
	defer func() { emit(StreamEvent{Type: StreamAgentEnd, Turn: res.Turns}) }()

	res, err := a.drive(ctx, messages, task, sink, emit)
	// Fold in what spawned sub-agents spent (spawn_subagent accumulates child
	// usage out-of-band), so a parent run's accounting includes its children on
	// every exit path.
	if cu := a.takeChildUsage(); cu != (Usage{}) {
		res.Usage = addUsage(res.Usage, cu)
	}
	if err != nil {
		// The run aborted before producing a final answer. Append a synthesized
		// failure turn and emit its lifecycle so an observer that drives off the
		// event stream still settles cleanly; the error is still returned to the
		// caller unchanged.
		stop := "error"
		if ctx.Err() != nil {
			stop = "aborted"
		}
		fail := Message{Role: RoleAssistant, Error: err.Error()}
		res.Messages = append(res.Messages, fail)
		res.StopReason = stop
		emit(StreamEvent{Type: StreamMessageStart, Turn: res.Turns})
		_ = a.hooks.runMessageEnd(ctx, fail)
		emit(StreamEvent{Type: StreamMessageEnd, Turn: res.Turns})
		emit(StreamEvent{Type: StreamTurnEnd, Turn: res.Turns})
	}
	return res, err
}

// drive runs the turn-based, hook-gated loop until the model stops requesting
// tools, a terminal tool fires, or a stop guard trips. When sink is non-nil each
// turn streams its tokens to sink as they arrive; tool execution is identical to
// the non-streaming path. runLoop owns the busy guard and the agent_start/end
// brackets; drive owns everything between.
func (a *Agent) drive(ctx context.Context, messages []Message, task string, sink StreamSink, emit func(StreamEvent)) (RunResult, error) {
	limits := a.limits
	res := RunResult{Messages: messages}

	// Durable writes are buffered per turn and flushed atomically at the turn
	// boundary (pi's pendingSessionWrites + save_point), so a crash mid-turn loses
	// the whole in-flight turn rather than leaving a half-written one — which is
	// exactly the shape RecoverSession treats as cleanly interrupted. A nil store
	// makes both buffer and flush no-ops.
	var pending []SessionEntry
	bufferEntry := func(e SessionEntry) {
		if a.session == nil || a.sessionID == "" {
			return
		}
		e.CreatedAt = time.Now()
		pending = append(pending, e)
	}
	flush := func() {
		if a.session == nil || a.sessionID == "" || len(pending) == 0 {
			return
		}
		for _, e := range pending {
			_ = a.session.Append(ctx, a.sessionID, e)
		}
		pending = pending[:0]
		emit(StreamEvent{Type: StreamSavePoint, Turn: res.Turns})
	}
	// A trailing flush guarantees the last turn's buffered entries (leaf included)
	// are committed on every return path.
	defer flush()
	appendEntry := bufferEntry
	// Persist the seed messages (the user prompt / prior thread) so the log is a
	// complete, reducible record from the first turn.
	for i := range messages {
		m := messages[i]
		appendEntry(SessionEntry{Kind: EntryMessage, Message: &m})
	}

	// recordTool appends a trace and, when streaming, forwards it to the sink as a
	// tool_execution_end event (and the back-compat StreamTool) so the UI sees
	// tool activity live (parity with the persisted tool_calls).
	recordTool := func(t ToolTrace) {
		res.Tools = append(res.Tools, t)
		if sink != nil {
			tc := t
			sink(StreamEvent{Type: StreamTool, Tool: &tc, Turn: res.Turns})
			end := t
			sink(StreamEvent{Type: StreamToolExecEnd, Tool: &end, Turn: res.Turns})
		}
	}

	// emitUpdate forwards a streaming tool's partial output as a
	// tool_execution_update event (P8). It is guarded by a mutex because partials
	// arrive from tool goroutines that may run concurrently (parallel-eligible
	// tools), and the StreamSink is not assumed to be concurrency-safe.
	var sinkMu sync.Mutex
	emitUpdate := func(call ToolCall, partial string) {
		if sink == nil {
			return
		}
		sinkMu.Lock()
		defer sinkMu.Unlock()
		tr := ToolTrace{Tool: call.Name, Args: call.Arguments, Allowed: true, ResultMeta: "partial"}
		sink(StreamEvent{Type: StreamToolExecUpdate, Tool: &tr, Note: partial, Turn: res.Turns})
	}

	// Perceive: assemble the system prompt once from the definition + recalled
	// memory + the available-skill headers. Skill bodies are NOT inlined; the
	// model pulls one on demand via the read_skill tool (progressive disclosure),
	// so only the skills the task actually needs ever enter context.
	var recalled []MemoryEntry
	if a.memory != nil {
		if got, err := a.memory.Recall(ctx, a.def.ScopeID, task, 8); err == nil {
			recalled = got
		}
	}
	skills := a.def.enabledSkills()
	system := buildSystemPrompt(a.def, recalled, skills)
	if system != "" {
		res.Messages = append([]Message{{Role: RoleSystem, Content: system}}, res.Messages...)
	}

	// Effective tool registry for this run = the host tools, plus the built-in
	// read_skill tool when the definition carries skills, plus the built-in
	// spawn_subagent tool when delegation is enabled and this run is still
	// above the nesting cap (the depth rides the ctx so it survives crossing
	// into another agent's run; a run at MaxDepth is not offered the tool, so
	// delegation bottoms out structurally). Both additions clone the set, so
	// the shared base ToolSet is never mutated per run.
	tools := a.tools
	if len(skills) > 0 {
		tools = withReadSkill(tools, a.def)
	}
	if a.subagents != nil && DelegationDepth(ctx) < a.subagents.normalized().MaxDepth {
		tools = withSubagent(tools, a)
	}

	// Circuit-breaker state (per run): consecutive failures per tool, and the set
	// of tools disabled after crossing maxToolFailures. A disabled tool is dropped
	// from the advertised schemas (below) and refused if the model calls it anyway
	// (during dispatch), so a persistently broken tool can't stall the run. A resume
	// re-applies the tools disabled in the crashed run (recovered from its durable
	// log) so the breaker's verdict survives the restart.
	toolFailures := map[string]int{}
	disabledTools := map[string]bool{}
	for _, name := range a.seedDisabledTools {
		disabledTools[name] = true
	}

	// buildSchemas advertises a tool set to the model: policy-permitted tools plus
	// the always-allowed built-in read_skill (deduped), minus any tool the circuit
	// breaker disabled this run. Recomputed per turn so a PrepareNextTurn hook that
	// swaps the tool set — or a newly disabled tool — takes effect next turn.
	buildSchemas := func(ts *ToolSet) []ToolSchema {
		permitted := a.policy.PermittedTools(ctx, ts.Names())
		schemas := filterSchemas(ts.Schemas(), permitted)
		if rs, ok := ts.Get(readSkillToolName); ok {
			already := false
			for _, s := range schemas {
				if s.Name == readSkillToolName {
					already = true
					break
				}
			}
			if !already {
				schemas = append(schemas, rs.Schema())
			}
		}
		if len(disabledTools) > 0 {
			kept := schemas[:0]
			for _, s := range schemas {
				if !disabledTools[s.Name] {
					kept = append(kept, s)
				}
			}
			schemas = kept
		}
		return schemas
	}

	// Build the model ladder: the primary provider/model first, then the
	// configured escalation rungs. rung points at the rung currently in use; once
	// a higher rung succeeds the loop stays there for subsequent turns.
	ladder := append([]ModelRung{{Provider: a.provider, Model: a.model}}, a.escalation...)
	rung := 0

	// state is the per-turn save-point. It is applied at the top of each turn and
	// refreshed by PrepareNextTurn after each turn (P7), so model / tools / system
	// changes apply to the next request without touching the in-flight one.
	state := TurnState{Model: a.model, Tools: tools, System: system}

	toolCallCount := 0
	// budgetFinalizing latches once the budget gate trips: the loop injects one
	// tool-free wrap-up turn and then stops with StopReason "budget_exhausted".
	budgetFinalizing := false
	// freeTurns refunds turns spent only on plan bookkeeping (update_plan) so the
	// built-in todo list can't starve the MaxTurns budget on a long task. The
	// MaxToolCalls budget still backstops a runaway planning loop.
	freeTurns := 0
	for res.Turns-freeTurns < limits.MaxTurns {
		// Honor cancellation between turns so an aborted viewer (SSE client gone)
		// stops the run before spending another provider call.
		if err := ctx.Err(); err != nil {
			res.StopReason = "aborted"
			res.Final = lastAssistantText(res.Messages)
			return res, nil
		}
		res.Turns++
		emit(StreamEvent{Type: StreamTurnStart, Turn: res.Turns})

		// Explain-mode pause point (Lab): block before this turn does any work until
		// the consumer permits it. Everything below — compaction, steering, the
		// permission gate, secret resolution, budgets, escalation — still runs after
		// the gate releases, so a stepped run stays fail-closed and accounted exactly
		// like a continuous one. nil gate (production) never pauses.
		if a.stepGate != nil {
			if err := a.stepGate(ctx, res.Turns); err != nil {
				res.StopReason = "halted"
				res.Final = lastAssistantText(res.Messages)
				return res, nil
			}
		}

		// Budget gate (#4): when the run has reached its ceiling, do one final
		// tool-free turn that summarizes progress, then stop. Checked with the usage
		// accumulated so far PLUS any sub-agent spend not yet folded into res.Usage
		// (takeChildUsage only folds on run exit, so peek it here — otherwise a run
		// that delegates heavily blows past the cap before stopping). Latched so the
		// wrap-up turn itself isn't re-gated. The steer message is appended like any
		// mid-run correction; stripping tools below forces a text-only wrap-up.
		if a.budgetGate != nil && !budgetFinalizing && a.budgetGate(ctx, addUsage(res.Usage, a.peekChildUsage())) {
			budgetFinalizing = true
			res.Messages = append(res.Messages, Message{Role: RoleUser, Content: budgetExhaustedSteer})
			if sink != nil {
				sink(StreamEvent{Type: StreamProgress, Note: "Budget reached — summarizing and stopping.", Turn: res.Turns})
			}
		}

		// Apply the current turn snapshot (P7): the base-rung model, the tool set
		// (and its advertised schemas), and the system prompt. A PrepareNextTurn
		// hook may have changed any of these after the previous turn.
		if rung == 0 {
			ladder[0].Model = state.Model
		}
		if state.Tools != nil {
			tools = state.Tools
		}
		schemas := buildSchemas(tools)
		// A finalizing turn advertises no tools, so the model can only write its
		// wrap-up and then stop via the natural "no tool calls" completion below.
		if budgetFinalizing {
			schemas = nil
		}
		if state.System != system && len(res.Messages) > 0 && res.Messages[0].Role == RoleSystem {
			res.Messages[0].Content = state.System
			system = state.System
		}

		// Stop guard: compact old turns when the estimated context approaches
		// the model window so long autonomous runs stay bounded (§5.2). The older
		// span is summarized by the active rung's model into a structured
		// checkpoint; on any failure it degrades to a deterministic elide.
		if shouldCompact(res.Messages, limits.MaxContextTokens) {
			// Compaction runs on its own tier when the consumer pinned one
			// (compactionProvider/Model); otherwise it borrows the active rung.
			compactProvider, compactModel := ladder[rung].Provider, ladder[rung].Model
			if a.compactionProvider != nil && a.compactionModel != "" {
				compactProvider, compactModel = a.compactionProvider, a.compactionModel
			}
			// Bracket the compaction in the durable log: a start entry, then the
			// completion. A start with no completion (crash mid-compaction) tells
			// recovery to re-run it.
			appendEntry(SessionEntry{Kind: EntryCompaction, Turn: res.Turns})
			res.Messages = compactWithSummary(ctx, compactProvider, compactModel, res.Messages, a.compaction)
			appendEntry(SessionEntry{Kind: EntryCompaction, Turn: res.Turns, Final: true})
		}

		// Steering: drain any user-injected corrections queued since the last turn
		// and thread them in before the model reasons, so a mid-run correction is
		// honored on the very next turn (pi's steering queue).
		if a.getSteering != nil {
			for _, m := range a.getSteering(ctx) {
				res.Messages = append(res.Messages, m)
				if sink != nil {
					sink(StreamEvent{Type: StreamProgress, Note: m.Content, Turn: res.Turns})
				}
			}
		}

		// Reason. Stream the turn when a sink is attached so tokens reach the
		// viewer live; otherwise issue one non-streaming Chat call. reason walks
		// the escalation ladder on a retryable error.
		//
		// context hooks (P10) transform the outgoing message view — redaction,
		// trimming, reminders — without mutating the persisted history; the result
		// drives only this request. before_provider_request hooks then inspect or
		// rewrite the assembled request. Both honor the hook error policy.
		reqMessages, herr := a.hooks.runContext(ctx, res.Messages)
		if herr != nil {
			return res, herr
		}
		req := ChatRequest{Messages: reqMessages, Tools: schemas, CacheKey: a.cacheKey, CacheRetention: a.cacheRetention, MaxTokens: a.maxTokens, ReasoningEffort: a.reasoningEffort}
		if req, herr = a.hooks.runBeforeProviderRequest(ctx, req); herr != nil {
			return res, herr
		}
		emit(StreamEvent{Type: StreamMessageStart, Turn: res.Turns})
		resp, err := a.reason(ctx, req, sink, ladder, &rung)
		if err != nil {
			return res, fmt.Errorf("provider chat (turn %d): %w", res.Turns, err)
		}
		emit(StreamEvent{Type: StreamMessageEnd, Turn: res.Turns})
		res.Usage.InputTokens += resp.Usage.InputTokens
		res.Usage.OutputTokens += resp.Usage.OutputTokens
		res.Usage.CacheReadTokens += resp.Usage.CacheReadTokens
		res.Usage.CacheWriteTokens += resp.Usage.CacheWriteTokens
		res.Usage.CostUSD += resp.Usage.CostUSD
		res.StopReason = resp.StopReason
		// Stamp the turn's usage onto the assistant message so compaction can use
		// the provider's real token count (not a byte heuristic) to find when the
		// context window is filling.
		turnMsg := resp.Message
		if resp.Usage.InputTokens > 0 || resp.Usage.OutputTokens > 0 {
			u := resp.Usage
			turnMsg.Usage = &u
		}
		res.Messages = append(res.Messages, turnMsg)
		appendEntry(SessionEntry{Kind: EntryMessage, Turn: res.Turns, Message: &turnMsg})
		// message_end observers (P10): the assistant message is now final. Read-only;
		// a failure is attributed and (under HookThrow) aborts the run.
		if err := a.hooks.runMessageEnd(ctx, turnMsg); err != nil {
			return res, err
		}
		// Reflect any escalation into the snapshot so the next turn's prompt/budget
		// (and a PrepareNextTurn hook) see the model actually in use. Record a
		// model change in the durable log so resume reconstructs the active rung.
		if ladder[rung].Model != state.Model {
			appendEntry(SessionEntry{Kind: EntryModelChange, Turn: res.Turns, Model: ladder[rung].Model})
		}
		state.Model = ladder[rung].Model

		// No tool calls -> the model produced its final answer.
		if len(resp.Message.ToolCalls) == 0 {
			res.Final = resp.Message.Content
			// A budget-finalizing wrap-up turn ends the run here regardless of
			// queued follow-ups: the ceiling is hit, so we do not restart the loop.
			if budgetFinalizing {
				res.StopReason = "budget_exhausted"
				emit(StreamEvent{Type: StreamTurnEnd, Turn: res.Turns})
				appendEntry(SessionEntry{Kind: EntryLeaf, Turn: res.Turns})
				flush()
				return res, nil
			}
			// Follow-up: after the agent would stop, drain any queued follow-up
			// messages and restart the loop instead of returning, so a conversation
			// continues inside the same bounded run (pi's follow-up queue). MaxTurns
			// still bounds the extended loop.
			if a.getFollowUp != nil {
				if follow := a.getFollowUp(ctx); len(follow) > 0 {
					for _, m := range follow {
						res.Messages = append(res.Messages, m)
						emit(StreamEvent{Type: StreamProgress, Note: m.Content, Turn: res.Turns})
					}
					emit(StreamEvent{Type: StreamTurnEnd, Turn: res.Turns})
					flush()
					continue
				}
			}
			emit(StreamEvent{Type: StreamTurnEnd, Turn: res.Turns})
			appendEntry(SessionEntry{Kind: EntryLeaf, Turn: res.Turns})
			flush()
			return res, nil
		}

		// Act: dispatch the batch of requested tool calls. Calls run sequentially
		// by default; when every call in the turn targets a parallel-eligible
		// (read-only) tool, they run concurrently and results are applied in the
		// model's original order so traces and tool messages stay deterministic.
		calls := resp.Message.ToolCalls

		// Budget guard (§7): once the run has spent its tool-call budget, block
		// the whole batch and stop cleanly. (Checked per batch, not per call, so
		// a single turn may run its full batch before the cap takes effect.)
		if toolCallCount >= limits.MaxToolCalls {
			for _, call := range calls {
				recordTool(ToolTrace{Tool: call.Name, Args: call.Arguments, Allowed: false, Reason: "tool-call budget exhausted"})
				res.Messages = append(res.Messages, toolResult(call, "stopped: tool-call budget exhausted"))
			}
			res.StopReason = "max_tool_calls"
			res.Final = lastAssistantText(res.Messages)
			emit(StreamEvent{Type: StreamTurnEnd, Turn: res.Turns})
			flush()
			return res, nil
		}

		// tool_execution_start for each requested call, in the model's order, before
		// dispatch (parallel or sequential).
		for i := range calls {
			start := ToolTrace{Tool: calls[i].Name, Args: calls[i].Arguments}
			emit(StreamEvent{Type: StreamToolExecStart, Tool: &start, Turn: res.Turns})
		}

		// A tool the circuit breaker disabled this run is refused without executing.
		// It is also dropped from the advertised schemas, so a well-behaved model
		// won't call it — this catches a model retrying it from memory.
		disabledOutcome := func(call ToolCall) toolOutcome {
			return toolOutcome{
				trace:   ToolTrace{Tool: call.Name, Args: call.Arguments, Allowed: false, Reason: "disabled after repeated failures"},
				message: toolResult(call, "blocked: "+call.Name+" was disabled for this run after repeated failures — do not call it again; finish another way"),
			}
		}

		outcomes := make([]toolOutcome, len(calls))
		if len(calls) > 1 && a.allParallel(tools, calls) {
			var wg sync.WaitGroup
			for i := range calls {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					// disabledTools is only written in the (single-threaded) accounting
					// loop after wg.Wait, so reading it here during dispatch is safe.
					if disabledTools[calls[i].Name] {
						outcomes[i] = disabledOutcome(calls[i])
						return
					}
					outcomes[i] = a.runToolCall(ctx, tools, calls[i], limits, emitUpdate)
				}(i)
			}
			wg.Wait()
		} else {
			for i := range calls {
				if disabledTools[calls[i].Name] {
					outcomes[i] = disabledOutcome(calls[i])
					continue
				}
				// Honor cancellation between tool calls; remaining calls short-
				// circuit to an aborted result rather than draining the batch.
				if err := ctx.Err(); err != nil {
					outcomes[i] = toolOutcome{
						trace:   ToolTrace{Tool: calls[i].Name, Args: calls[i].Arguments, Allowed: false, Reason: "aborted"},
						message: toolResult(calls[i], "stopped: run aborted"),
					}
					continue
				}
				outcomes[i] = a.runToolCall(ctx, tools, calls[i], limits, emitUpdate)
			}
		}

		// Apply outcomes in the model's original order: record (and stream) each
		// trace, append its tool-result message, count real executions against the
		// budget, and propagate terminate.
		terminate := false
		for i := range outcomes {
			msg := outcomes[i].message
			// Circuit-breaker accounting: count consecutive failures per tool within
			// this run. A real execution that errored increments the tool's counter; a
			// success resets it. Once a tool crosses maxToolFailures it is disabled for
			// the rest of the run — dropped from the advertised schemas next turn — and
			// a note is appended to its result so the model stops retrying it and routes
			// around it instead of stalling.
			if outcomes[i].executed {
				name := outcomes[i].trace.Tool
				if outcomes[i].trace.Error != "" {
					toolFailures[name]++
					if toolFailures[name] >= maxToolFailures && !disabledTools[name] {
						disabledTools[name] = true
						// Log the disable so it is reconstructed on resume (RecoverSession),
						// keeping a broken tool disabled across a crash rather than retried.
						appendEntry(SessionEntry{Kind: EntryToolDisabled, Turn: res.Turns, Tool: name})
						msg.Content += fmt.Sprintf("\n\n[%s has failed %d times in a row and is now disabled for the rest of this run. Do not call it again — complete the task another way.]", name, toolFailures[name])
						emit(StreamEvent{Type: StreamProgress, Note: fmt.Sprintf("Disabled %q after %d consecutive failures; continuing without it.", name, toolFailures[name]), Turn: res.Turns})
					}
				} else {
					delete(toolFailures, name)
				}
			}
			recordTool(outcomes[i].trace)
			res.Messages = append(res.Messages, msg)
			appendEntry(SessionEntry{Kind: EntryMessage, Turn: res.Turns, Message: &msg})
			if outcomes[i].executed {
				toolCallCount++
			}
			terminate = terminate || outcomes[i].terminate
		}

		if terminate {
			res.Final = lastAssistantText(res.Messages)
			emit(StreamEvent{Type: StreamTurnEnd, Turn: res.Turns})
			flush()
			return res, nil
		}

		// Refund a turn that did nothing but update the run plan: plan bookkeeping
		// is not productive progress, so it must not consume the MaxTurns budget on
		// a long multi-step task (the MaxToolCalls budget still backstops a runaway
		// planner).
		if allBookkeeping(calls) {
			freeTurns++
		}

		// Per-turn save-point refresh (P7): hand the just-completed state to the
		// hook so the next turn can use a new model / tools / system. Empty returned
		// fields keep the current value, so a careless hook can't blank the run.
		if a.prepareNextTurn != nil {
			next := a.prepareNextTurn(ctx, TurnState{Model: state.Model, Tools: tools, System: system, Messages: res.Messages})
			if next.Model != "" {
				state.Model = next.Model
			}
			if next.System != "" {
				state.System = next.System
			}
			if next.Tools != nil {
				state.Tools = next.Tools
			}
		}

		// Turn complete (reason + act); flush the turn's buffered durable writes as
		// one save-point, then continue to the next turn.
		emit(StreamEvent{Type: StreamTurnEnd, Turn: res.Turns})
		flush()
	}

	res.StopReason = "max_turns"
	res.Final = lastAssistantText(res.Messages)
	return res, nil
}

// allBookkeeping reports whether every call in a turn's batch is a non-productive
// run-plan update (update_plan). Such turns are refunded against MaxTurns so the
// built-in todo list can't starve a long, multi-step task.
func allBookkeeping(calls []ToolCall) bool {
	if len(calls) == 0 {
		return false
	}
	for _, c := range calls {
		if c.Name != ToolUpdatePlan {
			return false
		}
	}
	return true
}

// toolOutcome is the result of executing one tool call: the persisted trace, the
// tool-result message fed back to the model, whether the run should terminate,
// and whether it counted against the tool-call budget (only real executions do).
type toolOutcome struct {
	trace     ToolTrace
	message   Message
	terminate bool
	executed  bool
}

// runToolCall takes a single model tool call through lookup -> prepareArguments
// -> validate -> beforeToolCall (permission gate + hooks) -> execute ->
// afterToolCall. It touches no shared run state, so it is safe to call
// concurrently for parallel-eligible tools; budget counting and trace recording
// happen in the caller, in order.
func (a *Agent) runToolCall(ctx context.Context, tools *ToolSet, call ToolCall, limits Limits, emitUpdate func(ToolCall, string)) toolOutcome {
	trace := ToolTrace{Tool: call.Name, Args: call.Arguments}

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

	// beforeToolCall preflight (permission gate + consumer hooks). The built-in
	// read_skill tool bypasses it: it only returns definition-authored skill
	// bodies, so it is always allowed regardless of the (default-deny) policy.
	if call.Name != readSkillToolName {
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
	execStart := time.Now()
	out, runErr := callTool(ctx, tool, runArgs, emit)
	trace.LatencyMS = time.Since(execStart).Milliseconds()
	// Head+tail truncation: an oversized result keeps its beginning AND end,
	// because the end often carries the signal (a shell error after pages of
	// build output, the final rows of a query, a stack trace's cause).
	out = truncateMiddle(out, limits.MaxToolResultLen)
	out, term := a.hooks.runAfter(ctx, gated, out, runErr)

	trace.Allowed = true
	if runErr != nil {
		trace.Error = runErr.Error()
		out = "error: " + runErr.Error()
	}
	trace.ResultMeta = fmt.Sprintf("%d bytes in %dms", len(out), trace.LatencyMS)
	return toolOutcome{trace: trace, message: toolResult(call, out), terminate: term, executed: true}
}

// allParallel reports whether every call in the batch targets a registered tool
// that opts into concurrent execution (ParallelTool). A single non-eligible call
// forces the whole batch to run sequentially — the safe default.
func (a *Agent) allParallel(tools *ToolSet, calls []ToolCall) bool {
	for _, call := range calls {
		tool, ok := tools.Get(call.Name)
		if !ok {
			return false
		}
		p, ok := tool.(ParallelTool)
		if !ok || !p.Parallel() {
			return false
		}
	}
	return true
}

// reason issues one turn against the rung currently in use. Two distinct
// recoveries are layered, in order, so a transient blip is no longer conflated
// with a capability shortfall:
//
//  1. Same-rung retry (callRung): a retryable failure (429/5xx/network) on the
//     *same* model is retried with exponential backoff, honoring any Retry-After,
//     before the rung is given up — a brief outage rides out in place, cheaply.
//  2. Escalation: only once a rung is exhausted (its retries spent, or a
//     non-retryable error) does the loop fall down the ladder to the next rung and
//     try the turn there, sticking with the first rung that works (*rung advances
//     in place). Cancellation is never retried or escalated.
func (a *Agent) reason(ctx context.Context, req ChatRequest, sink StreamSink, ladder []ModelRung, rung *int) (ChatResponse, error) {
	for {
		p := ladder[*rung]
		req.Model = p.Model

		// Re-resolve this rung's API key before the call so an expiring BYO token
		// doesn't kill a long run; applied only when the provider is a KeyUpdater.
		if a.refreshKey != nil {
			if key, kerr := a.refreshKey(ctx, p.Provider.Name()); kerr == nil {
				if u, ok := p.Provider.(KeyUpdater); ok {
					u.UpdateAPIKey(key)
				}
			}
		}

		resp, err := a.callRung(ctx, p, req, sink)
		if err == nil {
			// after_provider_response observers see the raw response before its usage
			// is folded into the run total. Under HookThrow a failure aborts the turn.
			if herr := a.hooks.runAfterProviderResponse(ctx, resp); herr != nil {
				return ChatResponse{}, herr
			}
			return resp, nil
		}
		// Don't escalate on cancellation, and stop when the ladder is exhausted.
		if ctx.Err() != nil || *rung+1 >= len(ladder) {
			return ChatResponse{}, err
		}
		*rung++ // escalate to the next rung and retry this turn
	}
}

// callRung issues the turn against one rung, retrying the same model on a
// transient failure per the run's RetryPolicy before surfacing the error to the
// escalation logic. The first attempt is immediate; each retry waits a backoff
// (Retry-After when the server supplied one, else exponential with jitter) that
// is cancellation-aware. A non-retryable error or a cancellation returns at once,
// spending no further attempts.
func (a *Agent) callRung(ctx context.Context, p ModelRung, req ChatRequest, sink StreamSink) (ChatResponse, error) {
	var lastErr error
	for attempt := 0; attempt < a.retry.MaxAttempts; attempt++ {
		if attempt > 0 {
			if err := sleepBackoff(ctx, a.retry.delay(attempt-1, retryAfterOf(lastErr))); err != nil {
				return ChatResponse{}, lastErr // cancelled mid-backoff; report the provider failure
			}
		}
		var resp ChatResponse
		var err error
		if sink != nil {
			resp, err = a.streamTurn(ctx, p.Provider, req, sink)
		} else {
			resp, err = p.Provider.Chat(ctx, req)
		}
		if err == nil {
			return resp, nil
		}
		lastErr = err
		// A cancellation or a non-retryable error won't improve with another attempt:
		// hand it to the escalation logic immediately.
		if ctx.Err() != nil || !isRetryable(err) {
			return ChatResponse{}, err
		}
	}
	return ChatResponse{}, lastErr
}

// streamTurn consumes the provider's delta channel for one turn, forwarding
// content fragments to the sink as they arrive and accumulating the full
// assistant message (text + tool calls + usage) so the Act path is identical to
// the non-streaming turn.
func (a *Agent) streamTurn(ctx context.Context, provider LLMProvider, req ChatRequest, sink StreamSink) (ChatResponse, error) {
	ch, err := provider.Stream(ctx, req)
	if err != nil {
		return ChatResponse{}, err
	}
	msg := Message{Role: RoleAssistant}
	var resp ChatResponse
	for d := range ch {
		if d.Err != nil {
			return ChatResponse{}, d.Err
		}
		if d.ContentDelta != "" {
			msg.Content += d.ContentDelta
			sink(StreamEvent{Type: StreamToken, Token: d.ContentDelta})
		}
		if d.ToolCall != nil {
			msg.ToolCalls = append(msg.ToolCalls, *d.ToolCall)
		}
		if d.Done {
			resp.StopReason = d.StopReason
			resp.Usage = d.Usage
		}
	}
	resp.Message = msg
	return resp, nil
}

// toolResult builds a tool-role message linked to its call.
func toolResult(call ToolCall, content string) Message {
	return Message{Role: RoleTool, ToolCallID: call.ID, Name: call.Name, Content: content}
}

// filterSchemas keeps only the schemas whose name is in permitted.
func filterSchemas(all []ToolSchema, permitted []string) []ToolSchema {
	keep := make(map[string]bool, len(permitted))
	for _, n := range permitted {
		keep[n] = true
	}
	out := make([]ToolSchema, 0, len(all))
	for _, s := range all {
		if keep[s.Name] {
			out = append(out, s)
		}
	}
	return out
}

// lastAssistantText returns the content of the most recent assistant message.
func lastAssistantText(messages []Message) string {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == RoleAssistant && messages[i].Content != "" {
			return messages[i].Content
		}
	}
	return ""
}
