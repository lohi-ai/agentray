package agentcore

import (
	"context"
	"slices"
	"time"
)

// SessionEntryKind classifies one append-only entry in a durable session log
// (pi's semi-durable harness). The log is the source of truth: run state is
// rebuilt by reducing it, never by mutating a row in place.
type SessionEntryKind string

const (
	// EntryMessage records one conversation message reaching its final form
	// (message_end): a user prompt, an assistant turn, or a tool result.
	EntryMessage SessionEntryKind = "message"
	// EntryLeaf marks the run reaching a final answer — a leaf of the session
	// tree. Its presence means the run completed normally.
	EntryLeaf SessionEntryKind = "leaf"
	// EntryCompaction brackets a compaction. Final=false is the start record
	// (compaction in flight); Final=true is the completion record carrying the
	// summary. A start with no matching completion means compaction was
	// interrupted and must be re-run on resume.
	EntryCompaction SessionEntryKind = "compaction"
	// EntryBranchSummary records a fork/branch checkpoint summary.
	EntryBranchSummary SessionEntryKind = "branch_summary"
	// EntryModelChange records the active model switching (escalation or a
	// save-point bump), so a resumed run reconstructs the right model.
	EntryModelChange SessionEntryKind = "model_change"
	// EntryActiveToolsChange records the active tool set changing mid-run.
	EntryActiveToolsChange SessionEntryKind = "active_tools_change"
	// EntryTurnInterrupted is written by recovery to mark a turn that never
	// completed (a crash between a tool result and the next assistant turn).
	EntryTurnInterrupted SessionEntryKind = "turn_interrupted"
	// EntryToolDisabled records the circuit breaker disabling a tool for the rest
	// of the run after repeated failures, so the disable is reconstructed on
	// resume (and the broken tool isn't retried from scratch).
	EntryToolDisabled SessionEntryKind = "tool_disabled"
	// EntryLeafMove is a control entry (not a tree node): it moves the session's
	// active leaf to Target, so the next appended entry chains from there. This
	// is how a consumer rewinds a session to an earlier entry, or switches to
	// another branch, without rewriting history (pi's branch()/leaf pointer).
	EntryLeafMove SessionEntryKind = "leaf_move"
	// EntryGoal records the run's goal-gate condition (Config.Goal), written at
	// the start of a goal-gated run so a resume re-arms the gate — without it a
	// crashed /goal run would resume ungated while its replayed transcript still
	// carries the gate's nudge messages. A leaf closes the goal along with the
	// run, so later runs chained onto the same log are not gated by it.
	EntryGoal SessionEntryKind = "goal"
)

// SessionEntry is one immutable record in the append-only session log. The log
// is a tree, not just a line: ID/ParentID give each entry a stable address and
// a tree parent, so a consumer can branch from any earlier entry in place (pi's
// id/parentId session format). Both are optional — an entry without them chains
// implicitly to the entry appended before it, so a flat log written by an older
// writer is a single-branch tree and reduces exactly as before.
type SessionEntry struct {
	Seq      int              `json:"seq"` // append order, assigned by the store
	Kind     SessionEntryKind `json:"kind"`
	ID       string           `json:"id,omitempty"`        // stable entry id (writer-assigned)
	ParentID string           `json:"parent_id,omitempty"` // tree parent; "" chains to the previous entry
	Target   string           `json:"target,omitempty"`    // EntryLeafMove: the new active leaf
	Turn     int              `json:"turn,omitempty"`
	Message  *Message         `json:"message,omitempty"` // EntryMessage
	Model    string           `json:"model,omitempty"`   // EntryModelChange
	Tools    []string         `json:"tools,omitempty"`   // EntryActiveToolsChange
	Tool     string           `json:"tool,omitempty"`    // EntryToolDisabled
	Goal     string           `json:"goal,omitempty"`    // EntryGoal: the run's completion condition
	Summary  string           `json:"summary,omitempty"` // EntryCompaction (completion) / EntryBranchSummary
	Final    bool             `json:"final,omitempty"`   // EntryCompaction completion marker
	// Usage records what the summarization call itself cost (EntryCompaction
	// completion / EntryBranchSummary). Compaction and branch summaries are real
	// billable provider calls; without this they are invisible spend (pi #6671).
	Usage     *Usage    `json:"usage,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// SessionStore is the append-only durability seam (extends the working-memory
// MemoryStore conceptually; kept separate so a consumer can adopt durability
// incrementally). The store assigns Seq and never mutates a written entry.
type SessionStore interface {
	// Append records one entry; the store assigns its Seq.
	Append(ctx context.Context, sessionID string, entry SessionEntry) error
	// Log returns the full ordered entry log for a session.
	Log(ctx context.Context, sessionID string) ([]SessionEntry, error)
}

// ReducedState is the run state rebuilt by folding a session log: the message
// history to resume from, the active model and tools, and flags for whether the
// run completed and whether a compaction was left unfinished.
type ReducedState struct {
	Messages          []Message
	Model             string
	ActiveTools       []string
	DisabledTools     []string // tools the circuit breaker disabled (EntryToolDisabled)
	Completed         bool     // an EntryLeaf was seen
	PendingCompaction bool     // a compaction start with no completion
	Goal              string   // goal-gate condition of the unfinished tail run (EntryGoal)
	LastTurn          int
}

// ReduceSession folds an append-only log into the current run state. It is a
// pure function of the log — the heart of durable resume. The fold walks only
// the ACTIVE branch (leaf → root, reversed): a log that was rewound or branched
// reduces to the history the next turn should actually see, while abandoned
// branches stay in the log for inspection. A flat log's active branch is the
// whole log, so pre-tree logs reduce exactly as before.
func ReduceSession(log []SessionEntry) ReducedState {
	var rs ReducedState
	for _, e := range ActivePath(log) {
		if e.Turn > rs.LastTurn {
			rs.LastTurn = e.Turn
		}
		switch e.Kind {
		case EntryMessage:
			if e.Message != nil {
				rs.Messages = append(rs.Messages, *e.Message)
			}
		case EntryBranchSummary:
			// A branch switch distilled the abandoned branch into this summary;
			// surface it to the resumed run as context, marker-prefixed like a
			// compaction checkpoint so the model reads it as background.
			if s := e.Summary; s != "" {
				rs.Messages = append(rs.Messages, Message{Role: RoleSystem, Content: branchSummaryMarker + "\n" + s})
			}
		case EntryModelChange:
			rs.Model = e.Model
		case EntryActiveToolsChange:
			rs.ActiveTools = e.Tools
		case EntryToolDisabled:
			// Append-once: a tool is disabled at most once per run, but guard against
			// a duplicated log entry so the reduced set stays a clean set.
			if e.Tool != "" && !slices.Contains(rs.DisabledTools, e.Tool) {
				rs.DisabledTools = append(rs.DisabledTools, e.Tool)
			}
		case EntryCompaction:
			// A start (Final=false) opens a pending compaction; the completion
			// (Final=true) closes it and folds its summary into history.
			if e.Final {
				rs.PendingCompaction = false
			} else {
				rs.PendingCompaction = true
			}
		case EntryGoal:
			rs.Goal = e.Goal
		case EntryLeaf:
			rs.Completed = true
			// The goal belongs to the run that just finished; a later run chained
			// onto this log (a chat continuation) must not inherit its gate.
			rs.Goal = ""
		}
	}
	return rs
}

// RecoveryPolicy governs how an interrupted run is resumed.
type RecoveryPolicy string

const (
	// RecoveryMarkInterrupted is the conservative default: rebuild state, mark an
	// unfinished turn interrupted, re-run an unfinished compaction, and re-issue
	// only the tool calls whose tools declare themselves retry-safe.
	RecoveryMarkInterrupted RecoveryPolicy = "mark_interrupted"
)

// RetrySafeTool is an optional Tool capability: a tool that is safe to re-run
// after a crash (idempotent / read-only) declares RetrySafe() true. Recovery
// auto-retries only these; a tool that does not implement it is treated as
// non-idempotent and is never auto-retried (its dangling call is left for the
// model to decide).
type RetrySafeTool interface {
	RetrySafe() bool
}

// CallRetrySafeTool refines RetrySafeTool per invocation: a tool whose
// retry-safety depends on the specific call's arguments (spawn_subagent is
// crash-safe only when it self-forks, not when it routes to a delegate agent)
// implements this and receives the original dangling call. When both interfaces
// are implemented, this one wins.
type CallRetrySafeTool interface {
	RetrySafeCall(call ToolCall) bool
}

// isRetrySafe reports whether a registered tool opts into post-crash retry for
// this specific dangling call.
func isRetrySafe(tools *ToolSet, call ToolCall) bool {
	if tools == nil {
		return false
	}
	t, ok := tools.Get(call.Name)
	if !ok {
		return false
	}
	if cs, ok := t.(CallRetrySafeTool); ok {
		return cs.RetrySafeCall(call)
	}
	rs, ok := t.(RetrySafeTool)
	return ok && rs.RetrySafe()
}

// ResumePlan is the recovery output: the history to resume from, the active
// model/tools, and the conservative decisions about an interrupted turn.
type ResumePlan struct {
	Messages        []Message
	Model           string
	ActiveTools     []string
	DisabledTools   []string   // tools the circuit breaker disabled; re-applied on resume
	Completed       bool       // run already reached a leaf; nothing to resume
	Goal            string     // goal-gate condition of the interrupted run, re-armed on resume
	Interrupted     bool       // the last turn did not complete
	RerunCompaction bool       // an unfinished compaction must be re-run first
	RetryCalls      []ToolCall // dangling calls whose tools are retry-safe
	DroppedCalls    []ToolCall // dangling calls left for the model (not retry-safe)
}

// RecoverSession turns a durable log into a conservative resume plan. It reduces
// the log, then — under the (default) mark_interrupted policy — detects a turn
// that crashed mid-flight: an assistant message whose tool calls have no matching
// tool result. Retry-safe calls are queued for re-run; the rest are dropped so
// non-idempotent side effects are never silently repeated.
func RecoverSession(log []SessionEntry, tools *ToolSet, policy RecoveryPolicy) ResumePlan {
	rs := ReduceSession(log)
	plan := ResumePlan{
		Messages:        rs.Messages,
		Model:           rs.Model,
		ActiveTools:     rs.ActiveTools,
		DisabledTools:   rs.DisabledTools,
		Completed:       rs.Completed,
		Goal:            rs.Goal,
		RerunCompaction: rs.PendingCompaction,
	}
	if rs.Completed {
		return plan // a leaf exists: the run finished, nothing to recover
	}

	// Find dangling tool calls: those issued by an assistant message that never
	// received a tool result.
	satisfied := map[string]bool{}
	for _, m := range rs.Messages {
		if m.Role == RoleTool && m.ToolCallID != "" {
			satisfied[m.ToolCallID] = true
		}
	}
	for _, m := range rs.Messages {
		if m.Role != RoleAssistant {
			continue
		}
		for _, c := range m.ToolCalls {
			if satisfied[c.ID] {
				continue
			}
			plan.Interrupted = true
			if isRetrySafe(tools, c) {
				plan.RetryCalls = append(plan.RetryCalls, c)
			} else {
				plan.DroppedCalls = append(plan.DroppedCalls, c)
			}
		}
	}
	// A run that stopped without a leaf but with no dangling call is still
	// considered interrupted (it never reached a final answer).
	if !plan.Interrupted && len(rs.Messages) > 0 {
		plan.Interrupted = true
	}
	return plan
}

// interruptedCallNote is the synthesized tool result that closes a dangling
// call in a recovered transcript, shared by CloseDanglingCalls and the drive-
// level resume path so the model always sees the same wording.
const interruptedCallNote = "[interrupted: this tool call did not complete before the run was suspended and was not re-run automatically]"

// CloseDanglingCalls returns a transcript in which every assistant tool call
// that never received a result is satisfied by a synthesized interrupted-note
// tool message. Providers reject a history with an unanswered tool call, so
// this is what makes a recovered transcript replayable. Already-satisfied calls
// and non-assistant messages pass through untouched, in order.
func CloseDanglingCalls(messages []Message) []Message {
	satisfied := map[string]bool{}
	for _, m := range messages {
		if m.Role == RoleTool && m.ToolCallID != "" {
			satisfied[m.ToolCallID] = true
		}
	}
	out := make([]Message, 0, len(messages))
	for _, m := range messages {
		out = append(out, m)
		if m.Role != RoleAssistant {
			continue
		}
		for _, c := range m.ToolCalls {
			if satisfied[c.ID] {
				continue
			}
			out = append(out, Message{
				Role:       RoleTool,
				ToolCallID: c.ID,
				Name:       c.Name,
				Content:    interruptedCallNote,
			})
			satisfied[c.ID] = true
		}
	}
	return out
}
