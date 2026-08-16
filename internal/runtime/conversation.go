package agentruntime

import (
	"context"
	"encoding/json"
	"sort"
	"strconv"
	"strings"

	"github.com/lohi-ai/agentray/agentcore"
	"github.com/lohi-ai/agentray/internal/dataplane/store"
)

// This file is the conversation context reducer (DESIGN-CONVERSATION-STORE.md §5):
// the server-side replacement for the client replaying user/assistant pairs. The
// durable, conversation-scoped entry log (storage.agent_conversation_entries) is
// the single source; BuildHistory folds the path-to-leaf into the agentcore
// History the model is fed. Steps, tool traces, and cards are skipped here (human-
// only projection) but still rendered by the UI from the same entries.

// Entry kinds in the conversation log. Storage treats kind as an opaque string;
// these are the values this layer reads and writes.
const (
	ConvKindMessage    = "message"      // a chat turn (role user|assistant|system)
	ConvKindCompaction = "compaction"   // a non-destructive history-compaction bracket
	ConvKindToolTrace  = "tool_trace"   // a completed tool call (human/debug view only)
	ConvKindStep       = "step"         // a progress/step marker (human view only)
	ConvKindModelChg   = "model_change" // model/settings change (folded, not shown)
	// ConvKindClear is a /clear seam: everything before it stops contributing to
	// the model's context, with no summary standing in for it. Nothing is
	// deleted — the transcript above the seam is still on disk and still on
	// screen — which is the whole difference between clearing the context and
	// starting a new thread.
	ConvKindClear = "clear"
	// ConvKindPlan is a snapshot of the agent's live todo list (the todo plugin's
	// update_plan). The plan lives out-of-band in the run's Store by design, so it
	// survives compaction; mirroring each revision here is what lets a person see
	// it — on a reload, on a second machine, after the run that wrote it is over.
	// Human-only: the model gets the plan pinned into its request by the plugin,
	// never replayed out of the conversation.
	ConvKindPlan = "plan"
	// ConvKindGoal records a /goal directive: the completion condition the run was
	// gated on. Human-only for the same reason — the gate itself is durable in the
	// run's own session log (agentcore EntryGoal); this is how the thread shows it.
	ConvKindGoal = "goal"
)

// Compaction policy defaults, named once (design §6). The trigger compares the
// running context-token estimate of the live (post-last-compaction) path against
// the model window less a reserve.
const (
	ConvReserveTokens    = 16384
	ConvKeepRecentTokens = 20000
)

// convMessagePayload is the body of a ConvKindMessage entry. Kept minimal: the
// rendered text. Richer human-view fields (cards, steps) live on their own entry
// kinds so the reducer can ignore them.
type convMessagePayload struct {
	Text string `json:"text"`
	// Command marks a control-plane turn: a handled slash command (/clear,
	// /compact, /help, /plan, /agents) and the server's canned reply to it. It
	// belongs in the transcript — the user needs to see that the thread was
	// cleared — but never in the model's context. Replayed as history it reads as
	// something the agent said about the conversation itself, and a model handed
	// "Cleared. I've forgotten everything above this line" as its own most recent
	// words will echo it back as an answer (observed, 2026-08-16).
	Command bool `json:"command,omitempty"`
}

// convCompactionPayload is the body of a ConvKindCompaction entry. FirstKeptEntryID
// is the id of the first entry that survives into the model window; everything
// before it is summarized into Summary and drops out of context (but stays on disk,
// recoverable). TokensBefore records the pre-compaction estimate for observability.
type convCompactionPayload struct {
	Summary          string `json:"summary"`
	FirstKeptEntryID string `json:"first_kept_entry_id"`
	TokensBefore     int    `json:"tokens_before"`
}

// BuildHistory derives the model context for a conversation by folding its path
// from root to leaf (design §5). If the path contains a compaction entry, the
// most recent one wins: its summary is emitted as a system message and only
// entries from its FirstKeptEntryID onward contribute, so the compacted prefix
// leaves the model window without being deleted. Only ConvKindMessage entries
// become History turns; every other kind is a human-only projection and skipped.
//
// The returned slice excludes the just-appended latest user turn only if the
// caller hasn't appended it yet — callers append the new user message as an entry
// before calling BuildHistory, so the model always sees the latest turn.
func BuildHistory(ctx context.Context, store *storage.Store, convID string) ([]agentcore.Message, error) {
	entries, err := store.PathToLeaf(ctx, convID)
	if err != nil {
		return nil, err
	}
	return foldHistory(entries), nil
}

// foldHistory is the pure reducer over an ordered (root→leaf) entry path, split
// out so it is unit-testable without Postgres.
func foldHistory(entries []storage.AgentConversationEntry) []agentcore.Message {
	// Find the last seam. A compaction represents everything before it by a
	// summary; a /clear represents it by nothing at all. Whichever came last wins,
	// so a clear after a compaction drops that compaction's summary too — the user
	// asked to start fresh, and handing the model a summary of what it was told to
	// forget is not starting fresh.
	lastComp := -1
	var comp convCompactionPayload
	for i, e := range entries {
		switch e.Kind {
		case ConvKindCompaction:
			var p convCompactionPayload
			if json.Unmarshal([]byte(e.PayloadJSON), &p) == nil {
				lastComp = i
				comp = p
			}
		case ConvKindClear:
			lastComp = i
			comp = convCompactionPayload{} // no summary, no first-kept: resume right here
		}
	}

	out := []agentcore.Message{}
	start := 0
	if lastComp >= 0 {
		if comp.Summary != "" {
			out = append(out, agentcore.Message{Role: agentcore.RoleSystem, Content: comp.Summary})
		}
		// Resume from the first kept entry (by id); fall back to just after the
		// compaction entry if the id isn't on the path.
		start = lastComp + 1
		if comp.FirstKeptEntryID != "" {
			for i, e := range entries {
				if e.ID == comp.FirstKeptEntryID {
					start = i
					break
				}
			}
		}
	}

	for _, e := range entries[start:] {
		if e.Kind != ConvKindMessage {
			continue
		}
		role := agentcore.Role(e.Role)
		if role != agentcore.RoleUser && role != agentcore.RoleAssistant && role != agentcore.RoleSystem {
			continue
		}
		var p convMessagePayload
		if json.Unmarshal([]byte(e.PayloadJSON), &p) != nil || p.Text == "" || p.Command {
			continue
		}
		out = append(out, agentcore.Message{Role: role, Content: p.Text})
	}
	return out
}

// AppendMessageEntry records one chat turn as a ConvKindMessage entry, advancing
// the conversation leaf (storage owns that, atomically). authorUserID is empty for
// the agent's own (assistant) turns. agentID stamps which agent handled the turn
// (the per-message override; empty for the project's default agent). token_estimate
// uses a chars/4 heuristic — good enough for the compaction trigger, which is the
// only consumer (design §10).
func AppendMessageEntry(ctx context.Context, store *storage.Store, convID, role, text, agentID, authorUserID, runID string, turn int) (storage.AgentConversationEntry, error) {
	return appendMessage(ctx, store, convID, role, text, agentID, authorUserID, runID, turn, false)
}

// AppendCommandEntry records a handled slash command, or the server's reply to
// one, as a transcript-only message: rendered like any other turn, skipped by
// BuildHistory, and worth zero tokens against the compaction trigger (it is never
// in the window it would be triggering on).
func AppendCommandEntry(ctx context.Context, store *storage.Store, convID, role, text, agentID, authorUserID string) (storage.AgentConversationEntry, error) {
	return appendMessage(ctx, store, convID, role, text, agentID, authorUserID, "", 0, true)
}

func appendMessage(ctx context.Context, store *storage.Store, convID, role, text, agentID, authorUserID, runID string, turn int, command bool) (storage.AgentConversationEntry, error) {
	payload, _ := json.Marshal(convMessagePayload{Text: text, Command: command})
	tokens := estimateTokens(text)
	if command {
		tokens = 0
	}
	return store.AppendConversationEntry(ctx, storage.AgentConversationEntry{
		ConversationID: convID,
		Kind:           ConvKindMessage,
		Role:           role,
		AgentID:        agentID,
		AuthorUserID:   authorUserID,
		RunID:          runID,
		Turn:           turn,
		PayloadJSON:    string(payload),
		TokenEstimate:  tokens,
	})
}

// convPlanPayload is the body of a ConvKindPlan entry: one snapshot of the run's
// todo list, in the order the agent wrote it.
type convPlanPayload struct {
	Items []PlanItem `json:"items"`
}

// PlanItem is one step of the agent's live plan, mirrored out of the todo
// plugin's Store into the conversation so a person can watch it. The shape is
// the plugin's (content + status), restated here rather than imported so the
// wire contract with the client is owned by this layer.
type PlanItem struct {
	Content string `json:"content"`
	// Status is pending | in_progress | completed — the todo plugin's vocabulary,
	// which the tool itself validates before the store ever sees it.
	Status string `json:"status"`
}

// convGoalPayload is the body of a ConvKindGoal entry: the completion condition
// a /goal turn gated its run on.
type convGoalPayload struct {
	Goal string `json:"goal"`
}

// AppendPlanEntry mirrors one revision of the run plan into the conversation.
// TokenEstimate is deliberately 0: the plan never enters the model's context
// through this log (the todo plugin pins it into the request itself), so counting
// it toward the compaction trigger would compact a thread on weight it isn't
// actually carrying.
func AppendPlanEntry(ctx context.Context, store *storage.Store, convID, agentID, runID string, items []PlanItem, turn int) (storage.AgentConversationEntry, error) {
	payload, _ := json.Marshal(convPlanPayload{Items: items})
	return store.AppendConversationEntry(ctx, storage.AgentConversationEntry{
		ConversationID: convID,
		Kind:           ConvKindPlan,
		AgentID:        agentID,
		RunID:          runID,
		Turn:           turn,
		PayloadJSON:    string(payload),
	})
}

// AppendGoalEntry records the completion condition of a /goal turn. Same
// zero-estimate reasoning as the plan: the gate reaches the model through the
// goal plugin's system-prompt contract, not through replayed history.
func AppendGoalEntry(ctx context.Context, store *storage.Store, convID, agentID, runID, goal string) (storage.AgentConversationEntry, error) {
	payload, _ := json.Marshal(convGoalPayload{Goal: goal})
	return store.AppendConversationEntry(ctx, storage.AgentConversationEntry{
		ConversationID: convID,
		Kind:           ConvKindGoal,
		AgentID:        agentID,
		RunID:          runID,
		PayloadJSON:    string(payload),
	})
}

// AppendClearEntry writes a /clear seam. The entry carries no payload — its
// position IS the fact — and no token estimate, since it drops the live window
// to nothing rather than adding to it.
func AppendClearEntry(ctx context.Context, store *storage.Store, convID, agentID string) (storage.AgentConversationEntry, error) {
	return store.AppendConversationEntry(ctx, storage.AgentConversationEntry{
		ConversationID: convID,
		Kind:           ConvKindClear,
		AgentID:        agentID,
		PayloadJSON:    "{}",
	})
}

// LatestPlan returns the most recent plan snapshot on the conversation's active
// path, and whether one was found. It reads the log rather than the run's Store
// because the Store is per-run and gone once the run ends — this is what /plan
// answers from between turns.
func LatestPlan(ctx context.Context, store *storage.Store, convID string) ([]PlanItem, bool, error) {
	entries, err := store.PathToLeaf(ctx, convID)
	if err != nil {
		return nil, false, err
	}
	for i := len(entries) - 1; i >= 0; i-- {
		if entries[i].Kind != ConvKindPlan {
			continue
		}
		var p convPlanPayload
		if json.Unmarshal([]byte(entries[i].PayloadJSON), &p) != nil {
			continue
		}
		return p.Items, true, nil
	}
	return nil, false, nil
}

// RenderPlan formats a plan as the markdown checklist the chat surface shows for
// /plan. Empty for an empty plan, so a caller can tell "no plan" from "a plan
// with nothing in it" without inspecting the slice twice.
func RenderPlan(items []PlanItem) string {
	if len(items) == 0 {
		return ""
	}
	var b strings.Builder
	for _, it := range items {
		switch it.Status {
		case "completed":
			b.WriteString("- [x] ~~" + strings.TrimSpace(it.Content) + "~~\n")
		case "in_progress":
			b.WriteString("- [ ] **" + strings.TrimSpace(it.Content) + "** ← doing this now\n")
		default:
			b.WriteString("- [ ] " + strings.TrimSpace(it.Content) + "\n")
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// MessageEntryText extracts the rendered text of a ConvKindMessage entry (the
// fork/regenerate route reads it to resend a prior user turn verbatim). Returns
// "" for non-message or unparsable entries.
func MessageEntryText(e storage.AgentConversationEntry) string {
	if e.Kind != ConvKindMessage {
		return ""
	}
	var p convMessagePayload
	if json.Unmarshal([]byte(e.PayloadJSON), &p) != nil {
		return ""
	}
	return p.Text
}

// estimateTokens is the chars/4 heuristic pi uses as the cheap floor. Real
// provider usage on agent_runs trues this up; the estimate only gates compaction.
func estimateTokens(s string) int {
	return (len(s) + 3) / 4
}

// convToolTracePayload is the body of a ConvKindToolTrace entry — the completed
// tool call mirrored into the conversation log so a second machine/user sees the
// same work timeline the originating client streamed (design §7.3). Skipped by the
// context reducer (human-only projection).
type convToolTracePayload struct {
	// CallID is the provider's per-invocation id, mirrored so a reloaded client
	// keys its rows exactly the way the streaming one did — two concurrent calls
	// to the same tool stay two rows across a reload instead of collapsing.
	CallID     string `json:"call_id,omitempty"`
	Tool       string `json:"tool"`
	Target     string `json:"target,omitempty"`
	Allowed    bool   `json:"allowed"`
	Reason     string `json:"reason,omitempty"`
	Error      string `json:"error,omitempty"`
	ResultMeta string `json:"result_meta,omitempty"`
}

// ToolTarget renders a tool call's arguments as the short human label the work
// log shows beside the tool name ("signup_completed, 30"). Without it two
// concurrent calls to the same tool are indistinguishable on screen — which is
// exactly the case call ids exist to keep separate.
//
// It reads only scalar values and caps the result, so a large argument blob
// never rides the stream or the conversation log: the args are already the
// gated, validated form (credentials are still {{cred:NAME}} placeholders), but
// a full dump would be noise on the wire and unreadable in the row.
func ToolTarget(args string) string {
	var parsed map[string]any
	if json.Unmarshal([]byte(args), &parsed) != nil {
		return ""
	}
	// Sort the keys so the same call always renders the same label — Go's map
	// iteration order would otherwise reshuffle it between the tool_start frame
	// and the mirrored trace entry.
	keys := make([]string, 0, len(parsed))
	for k := range parsed {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		switch v := parsed[k].(type) {
		case string:
			if v != "" {
				parts = append(parts, v)
			}
		case float64:
			parts = append(parts, strconv.FormatFloat(v, 'f', -1, 64))
		case bool:
			if v {
				parts = append(parts, k)
			}
		}
	}
	out := strings.Join(parts, ", ")
	// Cut on a rune boundary, not a byte one: tool arguments carry Vietnamese
	// titles and search terms, and slicing mid-rune leaves a broken tail that
	// JSON-encodes as U+FFFD in both the SSE frame and the persisted trace.
	if len(out) > toolTargetMax {
		r := []rune(out)
		if len(r) > toolTargetMax {
			r = r[:toolTargetMax]
		}
		return string(r) + "…"
	}
	return out
}

// toolTargetMax caps the rendered argument label. The row truncates visually at
// far less than this on a narrow viewport; the cap is about what crosses the
// wire, not what fits.
const toolTargetMax = 80

// AppendToolTraceEntry mirrors one completed tool call into the conversation log.
// Best-effort, bounded to the number of tool calls in a turn (not per token), so a
// joining client renders the tool timeline without the originating SSE stream.
func AppendToolTraceEntry(ctx context.Context, store *storage.Store, convID, agentID, runID string, t convToolTracePayload, turn int) (storage.AgentConversationEntry, error) {
	payload, _ := json.Marshal(t)
	return store.AppendConversationEntry(ctx, storage.AgentConversationEntry{
		ConversationID: convID,
		Kind:           ConvKindToolTrace,
		AgentID:        agentID,
		RunID:          runID,
		Turn:           turn,
		PayloadJSON:    string(payload),
	})
}

// ConvContextWindow is the FALLBACK model context window for the
// conversation-level compaction trigger, used only when the caller could not
// determine the real one. Callers pass the answering tier's actual window
// (agentruntime.EffectiveContextWindow), which is the same fact the run-level
// compaction budget is capped against — the two layers compact the same thread
// for the same model, so they must not disagree about how much it holds.
//
// Conservative on purpose: a window guessed too small compacts early (wasteful),
// while one guessed too large replays a history the model cannot accept.
const ConvContextWindow = 128000

// MaybeCompactConversation appends a compaction entry when the live (post-last-
// compaction) context estimate exceeds the window less the reserve (design §6). It
// finds a cut snapped to a turn boundary (a user message), summarizes everything
// before it via the supplied summarize callback, and appends a non-destructive
// compaction entry — the prefix stays on disk but leaves the model window. No-op
// (returns false) when below threshold or when no clean cut keeps recent context.
//
// summarize receives the transcript to compress plus the previous compaction's
// summary (empty on the first compaction). When non-empty it must produce an
// UPDATED running summary that preserves the prior one and folds in the new
// transcript — because only the last compaction's summary survives into
// BuildHistory, a from-scratch summary of just the new slice would silently drop
// everything the earlier summary captured. (pi's iterative update-summary.)
func MaybeCompactConversation(ctx context.Context, store *storage.Store, convID string, window int, summarize func(ctx context.Context, transcript, previousSummary string) (string, error)) (bool, error) {
	return compactConversation(ctx, store, convID, window, false, summarize)
}

// CompactConversationNow is the user-initiated /compact: the same non-destructive
// fold, run regardless of how full the window is. A person asks for it when they
// know the thread has drifted, or before handing the agent a large new task —
// long before the automatic trigger would fire. It returns false with no error
// when there is nothing to fold (a thread with one turn in it), so the caller can
// say so plainly instead of reporting a failure.
func CompactConversationNow(ctx context.Context, store *storage.Store, convID string, window int, summarize func(ctx context.Context, transcript, previousSummary string) (string, error)) (bool, error) {
	return compactConversation(ctx, store, convID, window, true, summarize)
}

func compactConversation(ctx context.Context, store *storage.Store, convID string, window int, force bool, summarize func(ctx context.Context, transcript, previousSummary string) (string, error)) (bool, error) {
	entries, err := store.PathToLeaf(ctx, convID)
	if err != nil {
		return false, err
	}

	plan := planCompaction(entries, window, force)
	if !plan.ok {
		return false, nil // below threshold, or no clean cut that keeps a recent window
	}

	transcript := renderTranscript(entries[plan.liveStart:plan.firstKept])
	if transcript == "" {
		return false, nil
	}
	summary, err := summarize(ctx, transcript, plan.prevSummary)
	if err != nil || summary == "" {
		return false, err
	}

	payload, _ := json.Marshal(convCompactionPayload{
		Summary:          summary,
		FirstKeptEntryID: entries[plan.firstKept].ID,
		TokensBefore:     plan.total,
	})
	_, err = store.AppendConversationEntry(ctx, storage.AgentConversationEntry{
		ConversationID: convID,
		Kind:           ConvKindCompaction,
		PayloadJSON:    string(payload),
		TokenEstimate:  estimateTokens(summary),
	})
	if err != nil {
		return false, err
	}
	return true, nil
}

// compactionPlan is the pure decision behind MaybeCompactConversation: whether to
// compact, where to cut the live path, and which prior summary to carry forward.
// Split out so the threshold + turn-boundary logic is unit-testable without
// Postgres, mirroring foldHistory. Indices are absolute into the path passed to
// planCompaction: the transcript to summarize is entries[liveStart:firstKept] and
// the first surviving entry is entries[firstKept]. ok is false when nothing should
// be compacted (below threshold, or no clean cut that preserves a recent window).
type compactionPlan struct {
	liveStart   int    // first entry after the last compaction (start of the live window)
	firstKept   int    // first entry that survives into the model window (a user turn boundary)
	prevSummary string // last compaction's summary, carried forward into the new one
	total       int    // live-window token estimate (observability)
	ok          bool
}

// planCompaction decides the cut. Only entries after the last compaction count
// toward the live window; that compaction's summary is carried forward so the new
// summary extends it rather than replacing it (its prefix is no longer on the live
// path). When the live estimate exceeds the window less the reserve, it walks
// backward accumulating recent tokens and cuts at the next-older turn boundary (a
// user message) once past keepRecent — so a tool-call/result or user/assistant
// pair is never split across the boundary, and at least one recent turn is kept.
// A forced (/compact) pass skips the threshold and keeps only the most recent
// turn, because the person asking for it is asking for room, not for a trim.
func planCompaction(entries []storage.AgentConversationEntry, window int, force bool) compactionPlan {
	if window <= 0 {
		window = ConvContextWindow
	}
	liveStart := 0
	prevSummary := ""
	for i, e := range entries {
		switch e.Kind {
		case ConvKindCompaction:
			liveStart = i + 1
			var p convCompactionPayload
			if json.Unmarshal([]byte(e.PayloadJSON), &p) == nil {
				prevSummary = p.Summary
			}
		case ConvKindClear:
			// A /clear already dropped everything above it out of context, and left
			// no summary to carry forward. Compacting across it would summarize
			// what the user asked to forget straight back into the window.
			liveStart = i + 1
			prevSummary = ""
		}
	}
	live := entries[liveStart:]

	total := 0
	for _, e := range live {
		total += e.TokenEstimate
	}
	plan := compactionPlan{liveStart: liveStart, prevSummary: prevSummary, total: total}
	if !force && total <= window-ConvReserveTokens {
		return plan
	}

	// keepRecent is how much recent context survives verbatim. Forced, it is zero:
	// the cut lands at the last user turn, so the thread keeps its most recent
	// exchange and everything older becomes summary.
	keepRecent := ConvKeepRecentTokens
	if force {
		keepRecent = 0
	}
	recent := 0
	for i := len(live) - 1; i >= 0; i-- {
		recent += live[i].TokenEstimate
		// !isCommandEntry, for the same reason hasUserMessage below applies it:
		// `/compact` is itself persisted as a user message, and it is the NEWEST
		// one at the moment this runs. Cutting there would keep the invisible
		// command line live and fold the whole real conversation — the most recent
		// exchange included — into the summary, which is the one thing /compact
		// promises not to do.
		if recent >= keepRecent && live[i].Kind == ConvKindMessage &&
			live[i].Role == string(agentcore.RoleUser) && !isCommandEntry(live[i]) {
			if i <= 0 {
				break // cutting here would drop everything — keep the whole live window
			}
			// Forced by /compact, the cut lands at the newest user turn, so the
			// region above it can be a single leftover reply — the tail of the
			// previous compaction, or the acknowledgement /clear just wrote. There
			// is no exchange there to fold, and summarizing it anyway would spend a
			// model call to report "Compacted" for nothing. Require that the region
			// contains at least one thing the user said.
			if force && !hasUserMessage(live[:i]) {
				break
			}
			plan.firstKept = liveStart + i
			plan.ok = true
			return plan
		}
	}
	return plan
}

// hasUserMessage reports whether the span holds anything the user said. A slash
// command does not count: it never entered the model's context, so folding it
// into a summary would summarize nothing.
func hasUserMessage(entries []storage.AgentConversationEntry) bool {
	for _, e := range entries {
		if e.Kind == ConvKindMessage && e.Role == string(agentcore.RoleUser) && !isCommandEntry(e) {
			return true
		}
	}
	return false
}

// isCommandEntry reports whether an entry is a control-plane command turn.
func isCommandEntry(e storage.AgentConversationEntry) bool {
	if e.Kind != ConvKindMessage {
		return false
	}
	var p convMessagePayload
	return json.Unmarshal([]byte(e.PayloadJSON), &p) == nil && p.Command
}

// renderTranscript flattens message entries into a plain "role: text" transcript
// for the summarizer. Non-message entries are skipped (their work is reflected in
// the surrounding messages).
func renderTranscript(entries []storage.AgentConversationEntry) string {
	var b strings.Builder
	for _, e := range entries {
		if e.Kind != ConvKindMessage {
			continue
		}
		var p convMessagePayload
		if json.Unmarshal([]byte(e.PayloadJSON), &p) != nil || p.Text == "" || p.Command {
			continue
		}
		b.WriteString(e.Role)
		b.WriteString(": ")
		b.WriteString(p.Text)
		b.WriteString("\n")
	}
	return b.String()
}
