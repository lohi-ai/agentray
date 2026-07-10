package agentruntime

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/lohi-ai/agentray/agentcore"
	"github.com/lohi-ai/agentray/internal/storage"
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
	ConvKindMessage    = "message"     // a chat turn (role user|assistant|system)
	ConvKindCompaction = "compaction"  // a non-destructive history-compaction bracket
	ConvKindToolTrace  = "tool_trace"  // a completed tool call (human/debug view only)
	ConvKindStep       = "step"        // a progress/step marker (human view only)
	ConvKindModelChg   = "model_change" // model/settings change (folded, not shown)
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
	// Find the last compaction; entries before its cut are represented by its
	// summary, not replayed verbatim.
	lastComp := -1
	var comp convCompactionPayload
	for i, e := range entries {
		if e.Kind == ConvKindCompaction {
			var p convCompactionPayload
			if json.Unmarshal([]byte(e.PayloadJSON), &p) == nil {
				lastComp = i
				comp = p
			}
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
		if json.Unmarshal([]byte(e.PayloadJSON), &p) != nil || p.Text == "" {
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
	payload, _ := json.Marshal(convMessagePayload{Text: text})
	return store.AppendConversationEntry(ctx, storage.AgentConversationEntry{
		ConversationID: convID,
		Kind:           ConvKindMessage,
		Role:           role,
		AgentID:        agentID,
		AuthorUserID:   authorUserID,
		RunID:          runID,
		Turn:           turn,
		PayloadJSON:    string(payload),
		TokenEstimate:  estimateTokens(text),
	})
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
	Tool       string `json:"tool"`
	Allowed    bool   `json:"allowed"`
	Reason     string `json:"reason,omitempty"`
	Error      string `json:"error,omitempty"`
	ResultMeta string `json:"result_meta,omitempty"`
}

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

// ConvContextWindow is the assumed model context window for the conversation-level
// compaction trigger. Conservative default; the trigger only fires for very long
// threads, so over-estimating is safe (no premature summarization). Tunable later
// per-model (design §10).
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
func MaybeCompactConversation(ctx context.Context, store *storage.Store, convID string, summarize func(ctx context.Context, transcript, previousSummary string) (string, error)) (bool, error) {
	entries, err := store.PathToLeaf(ctx, convID)
	if err != nil {
		return false, err
	}

	plan := planCompaction(entries)
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
func planCompaction(entries []storage.AgentConversationEntry) compactionPlan {
	liveStart := 0
	prevSummary := ""
	for i, e := range entries {
		if e.Kind == ConvKindCompaction {
			liveStart = i + 1
			var p convCompactionPayload
			if json.Unmarshal([]byte(e.PayloadJSON), &p) == nil {
				prevSummary = p.Summary
			}
		}
	}
	live := entries[liveStart:]

	total := 0
	for _, e := range live {
		total += e.TokenEstimate
	}
	plan := compactionPlan{liveStart: liveStart, prevSummary: prevSummary, total: total}
	if total <= ConvContextWindow-ConvReserveTokens {
		return plan
	}

	recent := 0
	for i := len(live) - 1; i >= 0; i-- {
		recent += live[i].TokenEstimate
		if recent >= ConvKeepRecentTokens && live[i].Kind == ConvKindMessage && live[i].Role == string(agentcore.RoleUser) {
			if i <= 0 {
				break // cutting here would drop everything — keep the whole live window
			}
			plan.firstKept = liveStart + i
			plan.ok = true
			return plan
		}
	}
	return plan
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
		if json.Unmarshal([]byte(e.PayloadJSON), &p) != nil || p.Text == "" {
			continue
		}
		b.WriteString(e.Role)
		b.WriteString(": ")
		b.WriteString(p.Text)
		b.WriteString("\n")
	}
	return b.String()
}
