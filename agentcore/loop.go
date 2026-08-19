package agentcore

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
)

// budgetExhaustedSteer is injected as a final user turn when the budget gate
// trips, instructing the model to wrap up. Tools are stripped for this turn so it
// can only produce a text summary before the run stops.
const budgetExhaustedSteer = "Your run budget for this period has been exhausted. Do not call any more tools. Summarize the progress you have made so far and any recommended next steps in a few sentences, then stop."

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
	// agent_end observers see the finished RunResult — child usage folded in and,
	// on a failed run, the synthesized failure turn appended — so the deferred
	// close runs them just before the stream event that ends the run.
	defer func() {
		a.hooks.runAgentEnd(ctx, res)
		emit(StreamEvent{Type: StreamAgentEnd, Turn: res.Turns})
	}()

	// The loop itself is a seam: runLoop owns the run bracket (single-flight,
	// agent_start/agent_end, child-usage folding, failure synthesis) and hands
	// the turn loop to whichever Driver the composition installed. A custom
	// driver therefore inherits the bracket rather than reimplementing it.
	driver := a.driver
	if driver == nil {
		driver = DefaultDriver()
	}
	res, err := driver.Drive(ctx, a, messages, task, sink, emit)
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
		// The turn that failed was already closed inside drive (endTurn runs on
		// every path out of a turn), so this synthesized turn boundary is a stream
		// courtesy only — re-running the turn_end hooks here would double-count it.
	}
	return res, err
}

// drive runs the turn-based, hook-gated loop until the model stops requesting
// tools, a terminal tool fires, or a stop guard trips. When sink is non-nil each
// turn streams its tokens to sink as they arrive; tool execution is identical to
// the non-streaming path. runLoop owns the busy guard and the agent_start/end
// brackets; drive owns everything between.
func (a *Agent) drive(ctx context.Context, messages []Message, task string, sink StreamSink, emit func(StreamEvent)) (res RunResult, err error) {
	limits := a.limits
	res = RunResult{Messages: messages}

	// The resume log is read ONCE, up front, because the run's durable goal has
	// to be known before the extensions begin: the goal gate is an extension,
	// and a crash-resumed run must come back gated even when the resuming caller
	// could not re-supply the condition. Reading it here rather than inside the
	// resume block below is what lets RunInfo carry the recovered goal.
	var resumeLog []SessionEntry
	if a.resumeSession && a.session != nil && a.sessionID != "" {
		var lerr error
		// Windowed when the store can serve one: a resume needs the newest
		// checkpoint and the work after it, not the history the checkpoint already
		// stands for. LoadResumeLog falls back to the whole log whenever a suffix
		// would not fold to the same state.
		if resumeLog, lerr = LoadResumeLog(ctx, a.session, a.sessionID); lerr != nil {
			// An unreadable log fails the resume rather than silently degrading to
			// a from-scratch run: a fresh run would splice new seed messages onto
			// the crashed run's existing log and could redo non-idempotent work
			// that log already records.
			return res, fmt.Errorf("resume: reading durable session log: %w", lerr)
		}
	}
	// goal is the run's effective goal-gate condition: the configured one, or —
	// on a resume that didn't re-supply it — the one recovered from the log.
	// The loop owns the goal as durable STATE (it writes and recovers the
	// entry); what to DO about an unmet goal is the goal plugin's business.
	goal := a.goal
	if goal == "" {
		goal = goalFromLog(resumeLog)
	}

	// Extensions: instantiate every installed capability provider for THIS run.
	// The loop knows the interfaces in extension.go and nothing else — no plugin
	// is named here, so a composition without spill, jobs, or retrieval simply
	// has fewer extensions, not a different loop.
	//
	// The owner token fences run-scoped resources: the durable session when
	// there is one, else a token unique to this run.
	owner := a.sessionID
	if owner == "" {
		owner = "run_" + newEntryID()
	}
	// runTools is bound once the run's effective tool registry is assembled
	// below. The bookkeeping predicate closes over it because extensions are
	// created BEFORE tools are resolved (an extension may itself contribute
	// one), so the question can only be answered later — but must be askable
	// from the moment an extension exists.
	var runTools *ToolSet
	exts, xerr := beginExtensions(ctx, a.extensions, RunInfo{
		SessionID: a.sessionID,
		Owner:     owner,
		Limits:    limits,
		Depth:     DelegationDepth(ctx),
		Durable:   a.session != nil && a.sessionID != "",
		Session:   a.session,
		Agent:     a,
		Goal:      goal,
		Bookkeeping: func(name string) bool {
			return isBookkeeping(runTools, name)
		},
	})
	if xerr != nil {
		return res, fmt.Errorf("extension setup: %w", xerr)
	}
	// Contributions to the run context (a job launcher, a workspace handle) are
	// folded before the first turn; CloseRun fires on every exit path, so
	// nothing an extension started outlives the run that owns it.
	ctx = exts.runContext(ctx)
	// Tag every call this run makes with the session it belongs to, so a
	// provider decorator can tell a sub-agent's calls from its parent's — they
	// share one provider and one ctx chain, and without this they are
	// indistinguishable downstream.
	ctx = WithRunSession(ctx, a.sessionID)
	defer exts.closeRun()

	// The compaction strategy, resolved once. Nil-safe like the driver so a
	// hand-built Agent still bounds its context.
	compactor := a.compactor
	if compactor == nil {
		compactor = DefaultCompactor()
	}

	// Durable writes are buffered per turn and flushed atomically at the turn
	// boundary (pi's pendingSessionWrites + save_point), so a crash mid-turn loses
	// the whole in-flight turn rather than leaving a half-written one — which is
	// exactly the shape RecoverSession treats as cleanly interrupted. A nil store
	// makes both buffer and flush no-ops.
	var pending []SessionEntry
	// lastEntryID chains this run's entries into the session tree: each buffered
	// entry gets a stable id and points at the one before it, so any entry is a
	// valid Rewind target later. An id-less entry (rand failure) degrades to the
	// implicit chain — same shape, just not directly addressable.
	var lastEntryID string
	bufferEntry := func(e SessionEntry) {
		if a.session == nil || a.sessionID == "" {
			return
		}
		e.CreatedAt = time.Now()
		if e.ID == "" {
			e.ID = newEntryID()
		}
		if e.ParentID == "" {
			e.ParentID = lastEntryID
		}
		if e.ID != "" {
			lastEntryID = e.ID
		}
		// Report every durable write to the log observers. Doing it HERE — at the
		// single choke point every persisted entry goes through — is what lets an
		// observer hold a property over the recorded conversation without the
		// loop knowing what property it is checking.
		exts.observeLogged(e)
		pending = append(pending, e)
	}
	// flushCtx survives run cancellation: an aborted run must still commit its
	// buffered entries, or the durable log loses exactly the turns recovery
	// needs to see. WithoutCancel keeps the ctx values (tracing) minus the kill.
	flushCtx := context.WithoutCancel(ctx)
	flush := func() {
		if a.session == nil || a.sessionID == "" || len(pending) == 0 {
			return
		}
		// Append in order and stop at the first failure, so the durable log is
		// always a valid *prefix* of the run — a child entry never lands without
		// its parent (holes would break tree chaining and recovery). The
		// unflushed suffix stays pending and is retried at the next save point,
		// so a transient store blip self-heals; entries still pending when the
		// run returns are surfaced as res.UnpersistedEntries.
		n := 0
		for _, e := range pending {
			if aerr := a.session.Append(flushCtx, a.sessionID, e); aerr != nil {
				emit(StreamEvent{Type: StreamProgress, Note: "durable session write failed; retrying at next save point", Turn: res.Turns})
				break
			}
			n++
		}
		if n == 0 {
			return
		}
		pending = pending[n:]
		emit(StreamEvent{Type: StreamSavePoint, Turn: res.Turns})
	}
	// A trailing flush guarantees the last turn's buffered entries (leaf included)
	// are committed on every return path; whatever still couldn't be written is
	// reported on the result instead of being dropped silently.
	defer func() {
		flush()
		res.UnpersistedEntries = len(pending)
	}()
	appendEntry := bufferEntry

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
		tr := ToolTrace{CallID: call.ID, Tool: call.Name, Args: call.Arguments, Allowed: true, ResultMeta: "partial"}
		sink(StreamEvent{Type: StreamToolExecUpdate, Tool: &tr, Note: partial, Turn: res.Turns})
	}

	skills := a.def.enabledSkills()

	// Effective tool registry for this run = the host tools, plus the built-in
	// read_skill tool when the definition carries skills, plus whatever the
	// extensions contributed. Every addition clones the set, so the shared base
	// ToolSet is never mutated per run.
	tools := a.tools
	if len(skills) > 0 {
		tools = withReadSkill(tools, a.def)
	}
	// The loop does not know which extension contributed what, and does not
	// need to: an extension that has nothing to offer this run declined it at
	// BeginRun (delegation already at max depth, no store to spill into), so
	// "should this tool be advertised?" was answered before we got here.
	extTools, extExempt := exts.contributedTools()
	if len(extTools) > 0 {
		tools = tools.With(extTools...)
	}
	// Bind the run's final registry so RunInfo.Bookkeeping answers about the
	// tools that actually exist this run, extension-contributed ones included.
	runTools = tools

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

	// A tool the circuit breaker disabled this run is refused without executing.
	// It is also dropped from the advertised schemas, so a well-behaved model
	// won't call it — this catches a model retrying it from memory.
	disabledOutcome := func(call ToolCall) toolOutcome {
		return toolOutcome{
			trace:   ToolTrace{CallID: call.ID, Tool: call.Name, Args: call.Arguments, Allowed: false, Reason: "disabled after repeated failures"},
			message: toolResult(call, "blocked: "+call.Name+" was disabled for this run after repeated failures — do not call it again; finish another way"),
		}
	}

	// toolCallCount counts real executions against MaxToolCalls across the whole
	// run — resume replays included, so a resumed run can't spend more than a
	// live one.
	toolCallCount := 0
	// checkpoint mirrors the non-transcript state a fold of this log would
	// produce, so a compaction can stamp it onto its completion entry and make
	// that entry a place a resume may start reading from.
	//
	// It is maintained rather than derived: every field below is updated at the
	// exact line that appends the entry the fold would have read, so the two can
	// only disagree if a state entry is written without updating it — which is
	// what the mirrored-state test pins. Deriving it instead would mean reducing
	// the log on every compaction, i.e. paying the cost the checkpoint exists to
	// avoid.
	var checkpoint CheckpointState
	// applyBreaker is the circuit-breaker accounting shared by live dispatch and
	// resume replay: a real execution that errored increments its tool's
	// consecutive-failure counter — crossing maxToolFailures disables the tool
	// for the rest of the run (durably, via EntryToolDisabled) and appends a
	// routing note to the result message — while a success resets the counter.
	applyBreaker := func(o toolOutcome, msg *Message) {
		if !o.executed {
			return
		}
		// A cancelled run fails every call still in flight, and none of those
		// failures says anything about the tool. Counting them trips the breaker on
		// a tool that works — and the breaker writes EntryToolDisabled, so the
		// verdict outlives the process: the resume comes back with the tool off and
		// answers the interrupted calls with "disabled for this run", permanently
		// unable to redo the work the cancellation interrupted. A wide parallel
		// batch reaches maxToolFailures in one turn, which is exactly when this
		// matters most.
		if ctx.Err() != nil {
			return
		}
		name := o.trace.Tool
		if o.trace.Error == "" {
			delete(toolFailures, name)
			return
		}
		toolFailures[name]++
		if toolFailures[name] >= maxToolFailures && !disabledTools[name] {
			disabledTools[name] = true
			// Log the disable so it is reconstructed on resume (RecoverSession),
			// keeping a broken tool disabled across a crash rather than retried.
			appendEntry(SessionEntry{Kind: EntryToolDisabled, Turn: res.Turns, Tool: name})
			checkpoint.DisabledTools = append(checkpoint.DisabledTools, name)
			msg.Content += fmt.Sprintf("\n\n[%s has failed %d times in a row and is now disabled for the rest of this run. Do not call it again — complete the task another way.]", name, toolFailures[name])
			emit(StreamEvent{Type: StreamProgress, Note: fmt.Sprintf("Disabled %q after %d consecutive failures; continuing without it.", name, toolFailures[name]), Turn: res.Turns})
		}
	}

	// Durable resume (P9, pi's harness resume): when this run continues an
	// existing session log (Config.ResumeSession), history is rebuilt from the
	// log instead of the seeds. Recovery is conservative but not inert: a
	// dangling call whose tool declares RetrySafe is re-issued NOW with its
	// ORIGINAL call id — reproducing its idempotency key and, for
	// spawn_subagent, its deterministic child-session id, so the replay
	// reattaches to completed work instead of redoing it — while every other
	// dangling call is closed with an interrupted note for the model to act on.
	// A log that already reached its leaf short-circuits: the recorded answer
	// is returned with no provider call (reattach). An unfinished compaction
	// needs no special step — the reduced history still carries the full span,
	// so the in-loop compaction guard simply fires again.
	resumed := false
	{
		if len(resumeLog) > 0 {
			plan := RecoverSession(resumeLog, tools, RecoveryMarkInterrupted)
			// Seed the mirror from the log this run inherits, so a checkpoint
			// written later carries the state accumulated ACROSS the crash and not
			// just what this process happened to observe. A run that resumes,
			// compacts, and crashes again would otherwise write a checkpoint that
			// silently forgets the first run's disabled tools and goal.
			checkpoint = CheckpointState{
				Model:         plan.Model,
				ActiveTools:   plan.ActiveTools,
				DisabledTools: plan.DisabledTools,
				Goal:          plan.Goal,
				Completed:     plan.Completed,
			}
			switch {
			case plan.Completed:
				if final := strings.TrimSpace(lastAssistantText(plan.Messages)); final != "" {
					res.Messages = plan.Messages
					res.Final = final
					res.StopReason = "reattached"
					emit(StreamEvent{Type: StreamProgress, Note: "reattached to completed session; returning its recorded answer"})
					return res, nil
				}
				// Completed but answerless (degenerate log): fall through to a
				// fresh run; its entries chain onto the existing log.
			case len(plan.Messages) > 0:
				resumed = true
				// A recovered history came OUT of the log, so it satisfies the
				// invariant by definition; seed the tracker with it or every
				// resumed run would report its whole history as unlogged.
				exts.observe(ctx, PhaseAppend, res.Turns, plan.Messages)
				for _, name := range plan.DisabledTools {
					disabledTools[name] = true
				}
				emit(StreamEvent{Type: StreamProgress, Note: "resuming interrupted session from its durable log"})
				// Replays are real executions, so they run under the same rails as
				// live dispatch: the step and budget gates are consulted first (a
				// gated run replays nothing — its dangling calls close with
				// interrupted notes below and the loop's own gates take over from
				// turn 1), and each call honors cancellation, the tool-call
				// budget, and circuit-breaker accounting.
				replay := plan.RetryCalls
				if a.stepGate != nil && len(replay) > 0 {
					if gerr := a.stepGate(ctx, 0); gerr != nil {
						replay = nil
					}
				}
				if a.budgetGate != nil && len(replay) > 0 && a.budgetGate(ctx, addUsage(res.Usage, a.peekChildUsage())) {
					replay = nil
				}
				terminated := false
				retried := map[string]Message{}
				for _, call := range replay {
					if ctx.Err() != nil || toolCallCount >= limits.MaxToolCalls {
						break // the rest close with interrupted notes below
					}
					var out toolOutcome
					if disabledTools[call.Name] {
						out = disabledOutcome(call)
					} else {
						out = a.runToolCall(ctx, exts, extExempt, tools, call, limits, emitUpdate)
					}
					applyBreaker(out, &out.message)
					if out.executed {
						toolCallCount++
					}
					recordTool(out.trace)
					retried[call.ID] = out.message
					terminated = terminated || out.terminate
				}
				// Stitch each dangling call's closure — the replayed result or an
				// interrupted note — directly after the assistant message that
				// issued it, and persist only these NEW messages (the recovered
				// history is already in the log).
				satisfied := map[string]bool{}
				for _, m := range plan.Messages {
					if m.Role == RoleTool && m.ToolCallID != "" {
						satisfied[m.ToolCallID] = true
					}
				}
				stitched := make([]Message, 0, len(plan.Messages)+len(plan.RetryCalls)+len(plan.DroppedCalls))
				for _, m := range plan.Messages {
					stitched = append(stitched, m)
					if m.Role != RoleAssistant {
						continue
					}
					for _, c := range m.ToolCalls {
						if satisfied[c.ID] {
							continue
						}
						closing, ok := retried[c.ID]
						if !ok {
							closing = Message{Role: RoleTool, ToolCallID: c.ID, Name: c.Name, Content: interruptedCallNote}
						}
						stitched = append(stitched, closing)
						appendEntry(SessionEntry{Kind: EntryMessage, Message: &closing})
						satisfied[c.ID] = true
					}
				}
				messages = stitched
				res.Messages = stitched
				if terminated {
					// A replayed terminal tool ends the run exactly as a live one
					// would: record the leaf so the log reduces to Completed=true
					// (a later resume reattaches instead of replaying the child's
					// terminal operation). The stitched transcript is already
					// valid; the new closures and the leaf flush via the deferred
					// trailing flush.
					res.Final = lastAssistantText(res.Messages)
					appendEntry(SessionEntry{Kind: EntryLeaf})
					return res, nil
				}
			}
			// len(plan.Messages) == 0 falls through: nothing recoverable, run fresh.
		}
	}

	// A resume can carry a NEW instruction — the operator restarted the run and
	// told it something ("also include contractor payroll", "skip region EU").
	// The recovered history replaced the caller's seed messages wholesale above,
	// so without this the instruction never reaches the model at all: the run
	// comes back on its old objective, does not do the thing it was asked, and
	// nothing anywhere records that a user was ignored. Silently dropping input
	// is the worst available outcome — worse than refusing the resume.
	//
	// It is stamped Directive for the same reason a fresh Prompt's seed is: it is
	// human-authored, so the pin has to carry it past the compaction that
	// summarizes it away, exactly like a steered correction. And it is persisted
	// here because the resume path skips the seed-persist below, and an
	// instruction the model can see must be in the log.
	if resumed {
		if t := strings.TrimSpace(task); t != "" && !endsWithUserText(messages, t) {
			m := Message{Role: RoleUser, Content: t, Directive: true}
			messages = append(messages, m)
			res.Messages = messages
			appendEntry(SessionEntry{Kind: EntryMessage, Message: &m})
		}
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
	// Extensions that hold the model to a protocol state it here, up front,
	// where it costs one fixed prefix instead of a per-turn reminder. Appended
	// in composition order and on every run alike — the system message is
	// rebuilt each run, so a resumed run re-states the same contract.
	//
	// A closure rather than a one-off, because a contract can change mid-run: a
	// revised goal has to reach the model as the SAME standing instruction, not
	// as a correction appended after the one it contradicts. Re-running it is
	// safe by the interface's own terms — SystemPrompt() returns the extension's
	// current instruction and is called once per assembly, never accumulating.
	buildSystem := func() string {
		s := buildSystemPrompt(a.def, recalled, skills)
		for _, section := range exts.systemPrompt() {
			s += "\n\n" + section
		}
		return s
	}
	system := buildSystem()

	// before_agent_start (P10): the last seam that can shape the FIRST request.
	// It runs on the assembled prompt and the seed messages, before either is
	// persisted, so an injected message becomes part of the recorded conversation
	// on a fresh run. On a resumed run the durable log is authoritative and the
	// history below is already persisted, so a hook's message edits apply to this
	// attempt only — persist a mid-run injection through the steering queue.
	start, herr := a.hooks.runBeforeAgentStart(ctx, RunStart{System: system, Messages: messages, Task: task})
	if herr != nil {
		return res, herr
	}
	system = start.System
	messages = start.Messages
	res.Messages = messages

	// Continue() supplies the ask inside a prior thread rather than as a fresh
	// prompt, so its directive is whichever seed message IS the task. Prompt()
	// stamps its own message directly; this covers the thread case, and is a
	// no-op on a resumed log that already carries the stamp. Marking before the
	// seed is persisted is what puts it in the log, so a later resume rebuilds
	// the same pin instead of falling back to the first thing ever said.
	if t := strings.TrimSpace(task); t != "" {
		for i := range messages {
			if messages[i].Role == RoleUser && strings.TrimSpace(messages[i].Content) == t {
				messages[i].Directive = true
			}
		}
	}

	// Persist the seed messages (the user prompt / prior thread) so the log is a
	// complete, reducible record from the first turn — unless this run resumed an
	// existing log above, in which case the history is already durable and new
	// entries chain onto it instead.
	if !resumed {
		// Record the goal first so RecoverSession re-arms the gate on a resumed
		// run even when the caller cannot re-supply Config.Goal.
		if goal != "" {
			appendEntry(SessionEntry{Kind: EntryGoal, Goal: goal})
			checkpoint.Goal = goal
		}
		for i := range messages {
			m := messages[i]
			appendEntry(SessionEntry{Kind: EntryMessage, Message: &m})
		}
	}

	if system != "" {
		res.Messages = append([]Message{{Role: RoleSystem, Content: system}}, res.Messages...)
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
	ladder := append([]ModelRung{{Provider: a.provider, Model: a.model, ContextWindow: a.contextWindow}}, a.escalation...)
	rung := 0

	// state is the per-turn save-point. It is applied at the top of each turn and
	// refreshed by PrepareNextTurn after each turn (P7), so model / tools / system
	// changes apply to the next request without touching the in-flight one.
	state := TurnState{Model: a.model, Tools: tools, System: system}

	// budgetFinalizing latches once the budget gate trips: the loop injects one
	// tool-free wrap-up turn and then stops with StopReason "budget_exhausted".
	budgetFinalizing := false
	// freeTurns refunds turns spent only on bookkeeping tools (see
	// BookkeepingTool) so self-management can't starve the MaxTurns budget on a
	// long task. The MaxToolCalls budget still backstops a runaway loop.
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
		// turn_start / turn_end hooks (P10) fire on every run, streamed or not, so
		// metering and audit do not depend on a viewer being attached. endTurn is
		// idempotent per turn and closes the turn on every path out of it — normal
		// completion, a guard stop, or an aborted turn — so the stream event and the
		// hook always agree on how many turns happened.
		turnClosed := false
		// endTurn closes the turn exactly once. emitStream is false only on the
		// error path, where runLoop emits the turn_end event itself to bracket the
		// synthesized failure message — the hooks still fire here so a turn_end
		// observer sees exactly one close per started turn.
		endTurn := func(emitStream bool) {
			if turnClosed {
				return
			}
			turnClosed = true
			if emitStream {
				emit(StreamEvent{Type: StreamTurnEnd, Turn: res.Turns})
			}
			// A turn-end observer cannot abort: the turn's work is already done and
			// several call sites are on a return path. Failures are still attributed
			// through Hooks.OnError.
			_ = a.hooks.runTurnHooks(ctx, a.hooks.TurnEnd, "turn_end",
				TurnInfo{Turn: res.Turns, Model: state.Model, Usage: res.Usage, StopReason: res.StopReason})
		}
		// failTurn closes the turn before surfacing an error that aborts the run, so
		// a turn_end observer never sees a turn that started and never ended.
		failTurn := func(e error) (RunResult, error) {
			endTurn(false)
			return res, e
		}
		if herr := a.hooks.runTurnHooks(ctx, a.hooks.TurnStart, "turn_start",
			TurnInfo{Turn: res.Turns, Model: state.Model, Usage: res.Usage}); herr != nil {
			return failTurn(herr)
		}

		// Explain-mode pause point (Lab): block before this turn does any work until
		// the consumer permits it. Everything below — compaction, steering, the
		// permission gate, secret resolution, budgets, escalation — still runs after
		// the gate releases, so a stepped run stays fail-closed and accounted exactly
		// like a continuous one. nil gate (production) never pauses.
		if a.stepGate != nil {
			if err := a.stepGate(ctx, res.Turns); err != nil {
				res.StopReason = "halted"
				res.Final = lastAssistantText(res.Messages)
				endTurn(true)
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
			wrap := Message{Role: RoleUser, Content: budgetExhaustedSteer}
			res.Messages = append(res.Messages, wrap)
			appendEntry(SessionEntry{Kind: EntryMessage, Turn: res.Turns, Message: &wrap})
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

		// A revised objective, drained before the model reasons so the turn that
		// reads the contract is the first turn to run under it.
		//
		// Both effects belong to the loop and neither can be left to the reviser.
		// The EntryGoal is what a resume reads (goalFromLog takes the last one),
		// so a condition changed without one recovers the ORIGINAL gate after a
		// crash and quietly re-arms an objective the run has moved past. And the
		// system prompt is where the contract lives; rewriting it in place is
		// what keeps the model from holding two conditions at once, and costs
		// nothing per turn because it replaces text rather than appending.
		for _, g := range exts.goalRevisions() {
			goal = g
			checkpoint.Goal = g
			appendEntry(SessionEntry{Kind: EntryGoal, Turn: res.Turns, Goal: g})
			if next := buildSystem(); next != system && len(res.Messages) > 0 && res.Messages[0].Role == RoleSystem {
				res.Messages[0].Content = next
				system, state.System = next, next
			}
			// Surfaced, because a run that redefines what it is for is the single
			// thing a watching human most needs to see happen.
			emit(StreamEvent{Type: StreamProgress, Note: "goal updated: " + g, Turn: res.Turns})
		}

		// The budget is re-derived every turn rather than once per run, because
		// the rung can change under us: an escalation moves the run to a model
		// with its own window, and a ladder built from a 1M-token model and a
		// 128k one has no single correct ceiling.
		budget := effectiveBudget(limits.MaxContextTokens, ladder[rung].ContextWindow)

		// The keep-recent window is clamped to the actual budget so a small
		// MaxContextTokens cannot leave compaction with "nothing old enough".
		compaction := effectiveCompaction(a.compaction, budget)

		// Stop guard: compact old turns when the estimated context approaches
		// the model window so long autonomous runs stay bounded (§5.2). The older
		// span is summarized by the active rung's model into a structured
		// checkpoint; on any failure it degrades to a deterministic elide.
		if compactor.ShouldCompact(res.Messages, budget) {
			// before_compact (P10): the consumer may defer this compaction or supply
			// its own — a domain summarizer, or a cut that pins content the default
			// would drop. Asked before any durable bracket is written, so a skipped
			// compaction leaves no trace to recover.
			decision, herr := a.hooks.runBeforeCompact(ctx, CompactRequest{
				Turn:     res.Turns,
				Messages: res.Messages,
				Budget:   budget,
				Settings: compaction,
			})
			if herr != nil {
				return failTurn(herr)
			}
			switch {
			case decision.Skip:
				// Nothing to bracket: the transcript is untouched this turn.
			case decision.Messages != nil:
				// A consumer-supplied compaction still gets the durable bracket (the
				// transcript really did change shape) but costs no summarization call.
				appendEntry(SessionEntry{Kind: EntryCompaction, Turn: res.Turns})
				res.Messages = decision.Messages
				appendEntry(SessionEntry{
					Kind: EntryCompaction, Turn: res.Turns, Final: true,
					Retained: retainedTranscript(res.Messages, system),
					State:    checkpoint.clone(),
				})
			default:
				// Compaction runs on its own tier when the consumer pinned one
				// (compactionProvider/Model); otherwise it borrows the active rung.
				compactProvider, compactModel := ladder[rung].Provider, ladder[rung].Model
				if a.compactionProvider != nil && a.compactionModel != "" {
					compactProvider, compactModel = a.compactionProvider, a.compactionModel
				}
				// Bracket the compaction in the durable log: a start entry, then the
				// completion. A start with no completion (crash mid-compaction) tells
				// recovery to re-run it. The bracket is the LOOP's, not the
				// compactor's — a strategy that forgot to record what it did would
				// leave a resumed run unable to tell a compacted history from a
				// truncated one.
				appendEntry(SessionEntry{Kind: EntryCompaction, Turn: res.Turns})
				var cu Usage
				out, cerr := compactor.Compact(ctx, CompactionRequest{
					Messages: res.Messages,
					Budget:   budget,
					Turn:     res.Turns,
					Settings: compaction,
					Provider: compactProvider,
					Model:    compactModel,
				})
				// A compactor that fails leaves the transcript alone: an oversized
				// context still answers, while a killed run loses the whole thing.
				// The bracket still closes, so recovery sees a completed attempt
				// rather than a dangling one it would re-run forever.
				if cerr == nil && len(out.Messages) > 0 {
					res.Messages, cu = out.Messages, out.Usage
				}
				// The summarization call is real billable spend: fold it into the
				// run's accounting and stamp it on the completion entry so the audit
				// trail shows what compaction itself cost (pi #6671).
				res.Usage = addUsage(res.Usage, cu)
				// Record what the compaction LEFT, not just that it happened: the
				// completion entry carries the retained transcript, so a resumed
				// run restarts from this checkpoint instead of replaying the span
				// the summary already stands for and re-summarizing it.
				fin := SessionEntry{
					Kind: EntryCompaction, Turn: res.Turns, Final: true,
					Retained: retainedTranscript(res.Messages, system),
					State:    checkpoint.clone(),
				}
				if cu != (Usage{}) {
					fin.Usage = &cu
				}
				appendEntry(fin)
			}
			// Compaction replaces the transcript wholesale, but the log carries
			// the bracket that says so and recovery re-derives the same shape, so
			// the rewritten history is the new baseline for the invariant.
			exts.observe(ctx, PhaseRebase, res.Turns, res.Messages)
		}

		// Per-turn extension injections: anything an extension needs the model to
		// know for THIS turn (a background job finished, external state moved).
		// Drained before steering so the model reads results it was waiting on
		// first, and persisted here so an injection can never be model-visible
		// but absent from the log.
		for _, m := range exts.beforeStep(ctx, StepInfo{Turn: res.Turns, Model: state.Model, Usage: res.Usage}) {
			m := m
			res.Messages = append(res.Messages, m)
			appendEntry(SessionEntry{Kind: EntryMessage, Turn: res.Turns, Message: &m})
		}

		// Steering: drain any user-injected corrections queued since the last turn
		// and thread them in before the model reasons, so a mid-run correction is
		// honored on the very next turn (pi's steering queue).
		if a.getSteering != nil {
			for _, m := range a.getSteering(ctx) {
				m := m
				// A steer is human input by definition — that is what the queue is
				// for — so the loop stamps it rather than trusting every consumer
				// to remember. The stamp is what lets compaction keep the pinned
				// requirement current instead of pinning the one being corrected.
				if m.Role == RoleUser {
					m.Directive = true
				}
				// New human input is reported to the extensions: one tracking the
				// model's own behavior must treat this as a break, because the
				// model now has information it did not have.
				exts.observe(ctx, PhaseExternalInput, res.Turns, []Message{m})
				res.Messages = append(res.Messages, m)
				// Persist the drained steer: the model acts on it this turn, so a
				// resume that rebuilt history without it would replay a different
				// conversation than the one that ran.
				appendEntry(SessionEntry{Kind: EntryMessage, Turn: res.Turns, Message: &m})
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
		// "Model-visible means logged": check the history the loop maintains,
		// NOT the post-hook request view. Context hooks are documented to shape
		// the outgoing request without mutating persisted history, so checking
		// after them would flag every redaction as a violation.
		exts.observe(ctx, PhaseRequest, res.Turns, res.Messages)

		reqMessages, herr := a.hooks.runContext(ctx, res.Messages)
		if herr != nil {
			return failTurn(herr)
		}
		// Cache-anchor placement is a loop decision, not a provider one: mark the
		// stable prefix on the request view and let each provider translate the
		// marks into its native caching (or ignore them).
		reqMessages = markCacheAnchors(reqMessages, res.Messages, a.cacheKey)
		req := ChatRequest{Messages: reqMessages, Tools: schemas, CacheKey: a.cacheKey, CacheRetention: a.cacheRetention, MaxTokens: a.maxTokens, ReasoningEffort: a.reasoningEffort, OutputSchema: a.outputSchema}
		if req, herr = a.hooks.runBeforeProviderRequest(ctx, req); herr != nil {
			return failTurn(herr)
		}
		emit(StreamEvent{Type: StreamMessageStart, Turn: res.Turns})
		resp, err := a.reason(ctx, req, sink, ladder, &rung)
		if err != nil {
			return failTurn(fmt.Errorf("provider chat (turn %d): %w", res.Turns, err))
		}
		emit(StreamEvent{Type: StreamMessageEnd, Turn: res.Turns})
		res.Usage.InputTokens += resp.Usage.InputTokens
		res.Usage.OutputTokens += resp.Usage.OutputTokens
		res.Usage.CacheReadTokens += resp.Usage.CacheReadTokens
		res.Usage.CacheWriteTokens += resp.Usage.CacheWriteTokens
		res.Usage.CostUSD += resp.Usage.CostUSD
		res.Usage.CostUnpriced = res.Usage.CostUnpriced || resp.Usage.CostUnpriced
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
			return failTurn(err)
		}
		// Reflect any escalation into the snapshot so the next turn's prompt/budget
		// (and a PrepareNextTurn hook) see the model actually in use. Record a
		// model change in the durable log so resume reconstructs the active rung.
		if ladder[rung].Model != state.Model {
			appendEntry(SessionEntry{Kind: EntryModelChange, Turn: res.Turns, Model: ladder[rung].Model})
			checkpoint.Model = ladder[rung].Model
		}
		state.Model = ladder[rung].Model

		// No tool calls -> the model produced its final answer.
		if len(resp.Message.ToolCalls) == 0 {
			res.Final = resp.Message.Content
			// A budget-finalizing wrap-up turn ends the run here regardless of
			// queued follow-ups: the ceiling is hit, so we do not restart the loop.
			if budgetFinalizing {
				res.StopReason = "budget_exhausted"
				endTurn(true)
				appendEntry(SessionEntry{Kind: EntryLeaf, Turn: res.Turns})
				flush()
				return res, nil
			}
			// Stop interception: before accepting the finish, let the extensions
			// hold the run to whatever contract they own (a completion sentinel,
			// a verification pass, an evidence requirement). The injected message
			// is persisted like a steer so a resume replays the same conversation.
			//
			// Runs before the follow-up drain — a stop interceptor is about the
			// answer just produced, while follow-ups are new input for after an
			// accepted finish.
			if stop, _ := exts.turnStopping(ctx, StopInfo{Final: res.Final, Turns: res.Turns, Tools: res.Tools}); stop.Continue {
				// The injection is new material in the conversation, so it is
				// reported like a steer: an extension tracking what the model has
				// been told (a repeat detector, a transcript mirror) must not
				// treat the turn after a nudge as a continuation of the one
				// before it.
				exts.observe(ctx, PhaseExternalInput, res.Turns, stop.Inject)
				for _, m := range stop.Inject {
					m := m
					res.Messages = append(res.Messages, m)
					appendEntry(SessionEntry{Kind: EntryMessage, Turn: res.Turns, Message: &m})
				}
				if stop.Note != "" {
					// Curated note, not the raw injection: progress notes reach the
					// end user's chat feed, while the injection is internal rail
					// scaffolding.
					emit(StreamEvent{Type: StreamProgress, Note: stop.Note, Turn: res.Turns})
				}
				endTurn(true)
				flush()
				continue
			} else if stop.StopReason != "" {
				res.StopReason = stop.StopReason
			}
			// Follow-up: after the agent would stop, drain any queued follow-up
			// messages and restart the loop instead of returning, so a conversation
			// continues inside the same bounded run (pi's follow-up queue). MaxTurns
			// still bounds the extended loop.
			if a.getFollowUp != nil {
				if follow := a.getFollowUp(ctx); len(follow) > 0 {
					// A follow-up is new human input; report it for the same
					// reason a steer is reported.
					exts.observe(ctx, PhaseExternalInput, res.Turns, follow)
					for _, m := range follow {
						m := m
						// Stamped for the same reason a steer is: a follow-up is
						// the user asking for something, and a run that keeps
						// pinning the FIRST thing they asked for is not following
						// the conversation it is in.
						if m.Role == RoleUser {
							m.Directive = true
						}
						res.Messages = append(res.Messages, m)
						// Persist the drained follow-up for the same reason as a
						// steer: it is part of the conversation the model saw.
						appendEntry(SessionEntry{Kind: EntryMessage, Turn: res.Turns, Message: &m})
						emit(StreamEvent{Type: StreamProgress, Note: m.Content, Turn: res.Turns})
					}
					endTurn(true)
					flush()
					continue
				}
			}
			endTurn(true)
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
				recordTool(ToolTrace{CallID: call.ID, Tool: call.Name, Args: call.Arguments, Allowed: false, Reason: "tool-call budget exhausted"})
				res.Messages = append(res.Messages, toolResult(call, "stopped: tool-call budget exhausted"))
			}
			res.StopReason = "max_tool_calls"
			res.Final = lastAssistantText(res.Messages)
			endTurn(true)
			flush()
			return res, nil
		}

		// tool_execution_start for each requested call, in the model's order, before
		// dispatch (parallel or sequential).
		for i := range calls {
			start := ToolTrace{CallID: calls[i].ID, Tool: calls[i].Name, Args: calls[i].Arguments}
			emit(StreamEvent{Type: StreamToolExecStart, Tool: &start, Turn: res.Turns})
		}

		// Chunked dispatch (Claude Code / pi executionMode parity): consecutive
		// parallel-eligible calls run concurrently as one group; a non-eligible
		// call is a barrier that runs alone at its position. A mixed batch like
		// [read, read, write, read] therefore runs the two reads concurrently,
		// then the write, then the last read — instead of the old all-or-nothing
		// rule where one sequential tool serialized the whole batch. Results are
		// always applied in the model's order (outcomes is index-addressed), and
		// a sequential tool still observes every earlier call completed.
		outcomes := make([]toolOutcome, len(calls))
		for lo := 0; lo < len(calls); {
			hi := lo + 1
			if isParallelTool(tools, calls[lo]) {
				for hi < len(calls) && isParallelTool(tools, calls[hi]) {
					hi++
				}
			}
			// Honor cancellation between groups; remaining calls short-circuit to
			// an aborted result rather than draining the batch.
			if err := ctx.Err(); err != nil {
				for i := lo; i < len(calls); i++ {
					outcomes[i] = toolOutcome{
						trace:   ToolTrace{CallID: calls[i].ID, Tool: calls[i].Name, Args: calls[i].Arguments, Allowed: false, Reason: "aborted"},
						message: toolResult(calls[i], "stopped: run aborted"),
					}
				}
				break
			}
			if hi-lo == 1 {
				if disabledTools[calls[lo].Name] {
					outcomes[lo] = disabledOutcome(calls[lo])
				} else {
					outcomes[lo] = a.runToolCall(ctx, exts, extExempt, tools, calls[lo], limits, emitUpdate)
				}
			} else {
				var wg sync.WaitGroup
				for i := lo; i < hi; i++ {
					wg.Add(1)
					go func(i int) {
						defer wg.Done()
						// disabledTools is only written in the (single-threaded)
						// accounting loop after all groups finish, so reading it
						// here during dispatch is safe.
						if disabledTools[calls[i].Name] {
							outcomes[i] = disabledOutcome(calls[i])
							return
						}
						outcomes[i] = a.runToolCall(ctx, exts, extExempt, tools, calls[i], limits, emitUpdate)
					}(i)
				}
				wg.Wait()
			}
			lo = hi
		}

		// Apply outcomes in the model's original order: record (and stream) each
		// trace, append its tool-result message, count real executions against the
		// budget, and propagate terminate.
		terminate := false
		// extra buffers whatever the extensions asked to add on account of an
		// individual call. It is NOT appended here: a tool result must follow its
		// call with nothing in between, so the buffer drains after every result
		// in the batch has landed.
		var extra []Message
		// A result that exists only because the run was cancelled is not a settled
		// fact, and persisting it makes it one. The log is what a resume rebuilds
		// from: a call with a recorded result is answered forever, so a "stopped:
		// run aborted" row — or a tool error that is really the cancellation
		// arriving mid-call — permanently closes work that was never done. Nothing
		// retries it, because from the log's point of view there is nothing to
		// retry, and the resumed run reports success on a batch it only half ran.
		//
		// Left unpersisted, the call is dangling, which recovery already knows how
		// to handle: re-issue it when its tool declares RetrySafe (a self-forked
		// spawn reattaches to the child's own log and finishes it), close it with
		// an interrupted note otherwise. The in-memory transcript still carries the
		// message so this dying process stays provider-valid.
		//
		// Without this, whether a cancelled fan-out is recoverable comes down to
		// timing: a child still in flight leaves a dangling call and is replayed,
		// while a child the cancellation reached first is recorded as permanently
		// failed. Same crash, same batch, opposite outcomes.
		aborting := ctx.Err() != nil
		for i := range outcomes {
			msg := outcomes[i].message
			applyBreaker(outcomes[i], &msg)
			recordTool(outcomes[i].trace)
			res.Messages = append(res.Messages, msg)
			settled := !aborting ||
				(outcomes[i].trace.Error == "" && outcomes[i].trace.Reason != "aborted")
			if settled {
				appendEntry(SessionEntry{Kind: EntryMessage, Turn: res.Turns, Message: &msg})
			}
			if outcomes[i].executed {
				toolCallCount++
			}
			extra = append(extra, outcomes[i].extra...)
			terminate = terminate || outcomes[i].terminate
		}

		if terminate {
			res.Final = lastAssistantText(res.Messages)
			endTurn(true)
			// A terminal tool completes the run: record the leaf so the durable
			// log reduces to Completed=true and a resume reattaches to the
			// recorded outcome instead of replaying the terminal call.
			appendEntry(SessionEntry{Kind: EntryLeaf, Turn: res.Turns})
			flush()
			return res, nil
		}

		// Additional contexts: messages the extensions asked to add because of
		// this batch — per-call (buffered above) and per-batch. They land AFTER
		// every tool result, never interleaved, so tool-call/result adjacency
		// holds. The core owns the append and the persist, so anything the model
		// can see is in the durable log by construction ("model-visible means
		// logged", see assertLogged below) and no extension touches the log.
		// Never fires on a terminating turn: that run is already over.
		for _, m := range append(extra, exts.interceptBatch(ctx, calls)...) {
			m := m
			res.Messages = append(res.Messages, m)
			appendEntry(SessionEntry{Kind: EntryMessage, Turn: res.Turns, Message: &m})
			// Deliberately not emitted as a StreamProgress note: progress notes
			// reach the end user, and batch injections are internal scaffolding.
		}

		// Refund a turn that did nothing but update the run plan: plan bookkeeping
		// is not productive progress, so it must not consume the MaxTurns budget on
		// a long multi-step task (the MaxToolCalls budget still backstops a runaway
		// planner).
		if allBookkeeping(tools, calls) {
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
		endTurn(true)
		flush()
	}

	res.StopReason = "max_turns"
	res.Final = lastAssistantText(res.Messages)
	return res, nil
}

// allBookkeeping reports whether every call in a turn's batch is a non-productive
// turn: every call went to a tool that declares itself BookkeepingTool. Such
// turns are refunded against MaxTurns so self-management (keeping a plan
// current) can't starve a long, multi-step task.
//
// The question is asked of the TOOL, never of a name the loop knows: a planning
// capability lives in its own package, and the loop must keep working when that
// package is not installed.
func allBookkeeping(tools *ToolSet, calls []ToolCall) bool {
	if len(calls) == 0 {
		return false
	}
	for _, c := range calls {
		if !isBookkeeping(tools, c.Name) {
			return false
		}
	}
	return true
}

// isBookkeeping resolves a tool name against the run's registry and asks the
// tool itself. An unknown tool is not bookkeeping: a call the run cannot even
// dispatch must never buy back a turn.
func isBookkeeping(tools *ToolSet, name string) bool {
	if tools == nil {
		return false
	}
	t, ok := tools.Get(name)
	if !ok {
		return false
	}
	bk, ok := t.(BookkeepingTool)
	return ok && bk.Bookkeeping()
}
