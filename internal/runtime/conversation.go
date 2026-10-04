package agentruntime

import (
	"context"
	"encoding/json"
	"sort"
	"strconv"
	"strings"

	nativehost "github.com/lohi-ai/agentray/agentcore/host"
	"github.com/lohi-ai/agentray/internal/dataplane/store"
)

// Host-facing conversation entries share an append-only log with native history.
// BuildPiHistory (pi_conversation.go) reconstructs the opaque provider transcript;
// the display messages, plans, tool traces and cards defined here serve the UI.

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
	// ConvKindQuestion records a parked ask tool call awaiting a human answer.
	// Human-only: the model receives the answer as the tool result on resume;
	// this entry is how the thread shows the question card on reload.
	ConvKindQuestion = "question"
	// ConvKindKeyword records the magic keywords a turn fired ("ultrathink").
	// Human-only: the words are stripped from the message the model sees, so this
	// entry is the durable record that they fired — and why the turn ran at a
	// higher effort.
	ConvKindKeyword = "keyword"
)

// Compaction policy defaults, named once (design §6). The trigger compares the
// running context-token estimate of the live (post-last-compaction) path against
// the model window less a reserve.
const (
	ConvReserveTokens    = nativehost.ReserveTokens
	ConvKeepRecentTokens = nativehost.DefaultKeepRecentTokens
)

// convMessagePayload is the body of a ConvKindMessage entry. Kept minimal: the
// rendered text. Richer human-view fields (cards, steps) live on their own entry
// kinds so the reducer can ignore them.
type convMessagePayload struct {
	Text      string `json:"text"`
	PiDisplay bool   `json:"pi_display,omitempty"` // native history lives in its own immutable entry
	// Command marks a control-plane turn: a handled slash command (/clear,
	// /compact, /help, /plan, /agents) and the server's canned reply to it. It
	// belongs in the transcript — the user needs to see that the thread was
	// cleared — but never in the model's context. Replayed as history it reads as
	// something the agent said about the conversation itself, and a model handed
	// "Cleared. I've forgotten everything above this line" as its own most recent
	// words will echo it back as an answer (observed, 2026-08-16).
	Command bool `json:"command,omitempty"`
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
// BuildPiHistory, and worth zero tokens against the compaction trigger (it is never
// in the window it would be triggering on).
func AppendCommandEntry(ctx context.Context, store *storage.Store, convID, role, text, agentID, authorUserID string) (storage.AgentConversationEntry, error) {
	return appendMessage(ctx, store, convID, role, text, agentID, authorUserID, "", 0, true)
}

func appendMessage(ctx context.Context, store *storage.Store, convID, role, text, agentID, authorUserID, runID string, turn int, command bool) (storage.AgentConversationEntry, error) {
	return appendMessageAtLeaf(ctx, store, convID, role, text, agentID, authorUserID, runID, turn, command, nil, false)
}

func AppendMessageEntryAtLeaf(ctx context.Context, store *storage.Store, convID, role, text, agentID, authorUserID, expectedLeaf string) (storage.AgentConversationEntry, error) {
	return appendMessageAtLeaf(ctx, store, convID, role, text, agentID, authorUserID, "", 0, false, &expectedLeaf, false)
}

func appendMessageAtLeaf(ctx context.Context, store *storage.Store, convID, role, text, agentID, authorUserID, runID string, turn int, command bool, expectedLeaf *string, piDisplay bool) (storage.AgentConversationEntry, error) {
	payload, _ := json.Marshal(convMessagePayload{Text: text, Command: command, PiDisplay: piDisplay})
	tokens := estimateTokens(text)
	if command || piDisplay {
		tokens = 0
	}
	entry := storage.AgentConversationEntry{
		ConversationID: convID,
		Kind:           ConvKindMessage,
		Role:           role,
		AgentID:        agentID,
		AuthorUserID:   authorUserID,
		RunID:          runID,
		Turn:           turn,
		PayloadJSON:    string(payload),
		TokenEstimate:  tokens,
	}
	if expectedLeaf != nil {
		return store.AppendConversationEntryAtLeaf(ctx, entry, *expectedLeaf)
	}
	return store.AppendConversationEntry(ctx, entry)
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

// AppendQuestionEntry mirrors a parked ask tool call into the conversation.
// TokenEstimate is 0: the question is a human-only projection that renders the
// question card on reload.
func AppendQuestionEntry(ctx context.Context, store *storage.Store, convID, agentID, runID string, q json.RawMessage, turn int) (storage.AgentConversationEntry, error) {
	payload := string(q)
	if payload == "" {
		payload = "{}"
	}
	return store.AppendConversationEntry(ctx, storage.AgentConversationEntry{
		ConversationID: convID,
		Kind:           ConvKindQuestion,
		AgentID:        agentID,
		RunID:          runID,
		Turn:           turn,
		PayloadJSON:    payload,
	})
}

// convKeywordPayload is the body of a ConvKindKeyword entry: the magic keywords
// a turn fired, in the order the catalog first matched them.
type convKeywordPayload struct {
	Keywords []string `json:"keywords"`
}

// AppendKeywordEntry records which magic keywords a turn fired. Same
// zero-estimate reasoning as the goal entry: the words never enter the model's
// context through this log — they are stripped from the message itself — so
// counting them toward the compaction trigger would be weight it isn't
// carrying.
func AppendKeywordEntry(ctx context.Context, store *storage.Store, convID, agentID, runID string, keywords []string) (storage.AgentConversationEntry, error) {
	payload, _ := json.Marshal(convKeywordPayload{Keywords: keywords})
	return store.AppendConversationEntry(ctx, storage.AgentConversationEntry{
		ConversationID: convID,
		Kind:           ConvKindKeyword,
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
