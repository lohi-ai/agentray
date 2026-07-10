package agentcore

import (
	"context"
	"fmt"
	"strings"
)

// defaultContextTokenBudget is the soft ceiling at which the loop compacts old
// turns so long autonomous runs stay inside the model window (§5.2, §7). It is
// intentionally conservative; consumers can override via Limits.MaxContextTokens.
const defaultContextTokenBudget = 200_000

// defaultKeepRecentTokens is the approximate recent-context budget preserved
// verbatim after a compaction; everything older is summarized (pi's
// keepRecentTokens).
const defaultKeepRecentTokens = 20_000

// summaryMarker prefixes a compaction summary message so later compactions can
// recognize a prior summary and fold it into the next one instead of
// re-summarizing it as raw history.
const summaryMarker = "[context summary of earlier conversation]"

// elideMarker prefixes the breadcrumb the deterministic elide path leaves when
// summarization is unavailable or fails. Surfacing it (alongside summaryMarker)
// lets observers see that a *degraded* compaction happened rather than none.
const elideMarker = "[context compaction]"

// goalMarker prefixes the pinned-goal system message. The first time the loop
// compacts the original task out of the live window, that task is lifted into a
// goal-marked system message kept verbatim by every later compaction (it sorts
// into the leading-system head, which is never summarized). This stops the run's
// objective from drifting as successive lossy summaries fold into one another —
// the literal goal is always in front of the model. (pi keeps the first user
// task pinned for the same reason.)
const goalMarker = "[pinned goal — the original task for this run; keep working toward it]"

// CompactionSettings tunes how the loop compacts a long transcript. KeepRecent
// is the approximate token budget of recent messages kept verbatim; the older
// span is summarized by the model into a structured checkpoint.
type CompactionSettings struct {
	KeepRecentTokens int
}

// DefaultCompactionSettings returns conservative defaults.
func DefaultCompactionSettings() CompactionSettings {
	return CompactionSettings{KeepRecentTokens: defaultKeepRecentTokens}
}

// estimateContextTokens estimates how full the context window is, used only to
// decide when to compact — never for billing. It prefers the provider's real
// token count: it finds the most recent assistant message carrying Usage (the
// input+output of that turn ≈ the context size at that point) and adds a cheap
// byte estimate for the messages appended after it (the untracked trailing tool
// results / steering). When no message carries usage it falls back to a pure
// ~4-bytes/token heuristic over the whole transcript (pi's
// estimateContextTokens shape).
func estimateContextTokens(messages []Message) int {
	lastUsageIdx := -1
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == RoleAssistant && messages[i].Usage != nil {
			lastUsageIdx = i
			break
		}
	}
	if lastUsageIdx < 0 {
		return estimateBytesTokens(messages)
	}
	u := messages[lastUsageIdx].Usage
	return u.InputTokens + u.OutputTokens + estimateBytesTokens(messages[lastUsageIdx+1:])
}

// estimateBytesTokens is the cheap ~4-bytes/token fallback over a message slice.
func estimateBytesTokens(messages []Message) int {
	bytes := 0
	for _, m := range messages {
		bytes += len(m.Content)
		for _, tc := range m.ToolCalls {
			bytes += len(tc.Name) + len(tc.Arguments)
		}
	}
	return bytes / 4
}

// shouldCompact reports whether the estimated context exceeds the budget.
func shouldCompact(messages []Message, budget int) bool {
	if budget <= 0 {
		budget = defaultContextTokenBudget
	}
	return estimateContextTokens(messages) > budget
}

// leadingSystemCount counts the original leading system messages — those that
// are NOT a prior compaction summary. A prior summary (system role, summaryMarker
// prefix) is deliberately excluded so the next compaction folds it into the new
// summary rather than freezing it as permanent header.
func leadingSystemCount(messages []Message) int {
	n := 0
	for n < len(messages) && messages[n].Role == RoleSystem && !strings.HasPrefix(messages[n].Content, summaryMarker) {
		n++
	}
	return n
}

// findCutPoint returns the index in body at which the retained recent tail
// begins: walk back from the end accumulating an estimate until keepRecentTokens
// is reached, then snap earlier so the tail never starts on a tool-result
// message (which must stay attached to the assistant turn that issued it). A
// return of 0 means nothing is old enough to compact.
func findCutPoint(body []Message, keepRecentTokens int) int {
	if keepRecentTokens <= 0 {
		keepRecentTokens = defaultKeepRecentTokens
	}
	acc := 0
	cut := 0
	reached := false
	for i := len(body) - 1; i >= 0; i-- {
		acc += estimateBytesTokens(body[i : i+1])
		if acc >= keepRecentTokens {
			cut = i
			reached = true
			break
		}
	}
	if !reached {
		return 0 // whole transcript fits in the recent budget
	}
	for cut > 0 && body[cut].Role == RoleTool {
		cut-- // keep tool results with their owning assistant turn
	}
	return cut
}

// compactWithSummary replaces the older span of a long transcript with a single
// model-generated structured checkpoint, keeping the leading system prompt and
// the recent tail verbatim. The summary call uses the supplied provider/model
// (the active rung — typically the cheapest). On any failure (provider error,
// empty summary, nothing old enough) it falls back to the deterministic elide so
// a run is never broken by compaction.
func compactWithSummary(ctx context.Context, provider LLMProvider, model string, messages []Message, settings CompactionSettings) []Message {
	sysN := leadingSystemCount(messages)
	head := messages[:sysN]
	body := messages[sysN:] // may begin with a prior summary (folded in below)

	cut := findCutPoint(body, settings.KeepRecentTokens)
	if cut <= 0 {
		return compact(messages, 6) // nothing old enough for a clean cut; elide
	}
	older := body[:cut]
	tail := body[cut:]

	// Iterative update-summary: if the older span begins with a prior summary
	// (folded in by a previous compaction), lift it out and fold only the
	// genuinely-new older messages into it, rather than re-summarizing the prior
	// summary as raw transcript. This keeps long runs lossless on facts the prior
	// summary already captured and saves the model from re-derived drift. (pi's
	// UPDATE_SUMMARIZATION_PROMPT / generateSummary(previousSummary).)
	prevSummary, newOlder := splitPriorSummary(older)
	if prevSummary != "" && len(newOlder) == 0 {
		return messages // only the prior summary was old; nothing new to fold
	}

	summary, err := summarizeSpan(ctx, provider, model, newOlder, prevSummary)
	if err != nil || strings.TrimSpace(summary) == "" {
		return compact(messages, 6) // summarization failed — degrade, don't break
	}

	summaryMsg := Message{Role: RoleSystem, Content: summaryMarker + "\n" + strings.TrimSpace(summary)}
	out := make([]Message, 0, len(head)+2+len(tail))
	out = append(out, head...)
	// Pin the original goal the first time it would be summarized away. Once a
	// goal pin exists it lives in head (leading system, non-summary) and every
	// later compaction preserves it verbatim, so the objective never drifts.
	if !hasGoalPin(messages) {
		if goal, ok := firstUserText(older); ok {
			out = append(out, Message{Role: RoleSystem, Content: goalMarker + "\n" + strings.TrimSpace(goal)})
		}
	}
	out = append(out, summaryMsg)
	out = append(out, tail...)
	return out
}

// firstUserText returns the content of the first non-empty user message in span.
func firstUserText(span []Message) (string, bool) {
	for _, m := range span {
		if m.Role == RoleUser && strings.TrimSpace(m.Content) != "" {
			return m.Content, true
		}
	}
	return "", false
}

// hasGoalPin reports whether a pinned-goal system message already exists, so the
// goal is lifted out exactly once and then preserved by head retention.
func hasGoalPin(msgs []Message) bool {
	for _, m := range msgs {
		if m.Role == RoleSystem && strings.HasPrefix(m.Content, goalMarker) {
			return true
		}
	}
	return false
}

// splitPriorSummary detects a prior compaction summary at the head of an older
// span and separates it from the genuinely-new messages that follow. The prior
// summary always sorts to the front of the body (it is a leading non-original
// system message), so it can only be older[0]. Returns the summary text (without
// its marker) and the remaining new messages; ("", older) when none is present.
func splitPriorSummary(older []Message) (string, []Message) {
	if len(older) > 0 && older[0].Role == RoleSystem && strings.HasPrefix(older[0].Content, summaryMarker) {
		prev := strings.TrimSpace(strings.TrimPrefix(older[0].Content, summaryMarker))
		return prev, older[1:]
	}
	return "", older
}

// summarizeSpan asks the model to distill a span of conversation into the
// structured checkpoint format. The span is serialized to a single transcript
// (roles preserved) and handed to a one-shot, non-streaming Chat call with no
// tools. When previousSummary is non-empty the model is asked to UPDATE it in
// place — preserving everything already captured and folding in only the new
// messages — instead of summarizing from scratch.
func summarizeSpan(ctx context.Context, provider LLMProvider, model string, span []Message, previousSummary string) (string, error) {
	if len(span) == 0 {
		return "", fmt.Errorf("empty span")
	}
	var userContent string
	if strings.TrimSpace(previousSummary) != "" {
		userContent = "## Previous summary\n" + strings.TrimSpace(previousSummary) +
			"\n\n## New messages since that summary\n" + serializeConversation(span) +
			"\n\n" + updateSummarizationPrompt
	} else {
		userContent = serializeConversation(span) + "\n\n" + summarizationPrompt
	}
	req := ChatRequest{
		Model: model,
		Messages: []Message{
			{Role: RoleSystem, Content: summarizationSystemPrompt},
			{Role: RoleUser, Content: userContent},
		},
		MaxTokens: 2048,
	}
	resp, err := provider.Chat(ctx, req)
	if err != nil {
		return "", err
	}
	return resp.Message.Content, nil
}

// serializeConversation renders a span of messages into a plain-text transcript
// the summarizer can read (roles labeled; tool calls and results inlined).
func serializeConversation(span []Message) string {
	var b strings.Builder
	for _, m := range span {
		switch m.Role {
		case RoleSystem:
			fmt.Fprintf(&b, "SYSTEM: %s\n", strings.TrimSpace(m.Content))
		case RoleUser:
			fmt.Fprintf(&b, "USER: %s\n", strings.TrimSpace(m.Content))
		case RoleAssistant:
			if c := strings.TrimSpace(m.Content); c != "" {
				fmt.Fprintf(&b, "ASSISTANT: %s\n", c)
			}
			for _, tc := range m.ToolCalls {
				fmt.Fprintf(&b, "ASSISTANT called tool %s(%s)\n", tc.Name, tc.Arguments)
			}
		case RoleTool:
			fmt.Fprintf(&b, "TOOL %s -> %s\n", m.Name, strings.TrimSpace(m.Content))
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

const summarizationSystemPrompt = `You are a context summarization assistant. Read a conversation between a user and an AI assistant, then produce a structured summary following the exact format specified.

Do NOT continue the conversation. Do NOT respond to any questions in the conversation. ONLY output the structured summary.`

const summarizationPrompt = `The transcript above is a conversation to summarize. Create a structured context checkpoint that another assistant will use to continue the work.

Use this EXACT format:

## Goal
[What is the user trying to accomplish?]

## Constraints & Preferences
- [Constraints, preferences, or requirements mentioned] (or "(none)")

## Progress
### Done
- [x] [Completed tasks/changes]
### In Progress
- [ ] [Current work]
### Blocked
- [Issues preventing progress, if any]

## Key Decisions
- **[Decision]**: [Brief rationale]

## Next Steps
1. [Ordered list of what should happen next]

## Critical Context
- [Data, examples, identifiers, or references needed to continue] (or "(none)")

Keep each section concise. Preserve exact identifiers, names, and error messages.`

const updateSummarizationPrompt = `Above is a "Previous summary" (a structured context checkpoint from earlier in this same conversation) followed by the "New messages since that summary".

Produce an UPDATED checkpoint in the EXACT same format as the previous summary:

## Goal
## Constraints & Preferences
## Progress
### Done
### In Progress
### Blocked
## Key Decisions
## Next Steps
## Critical Context

Rules for the update:
- PRESERVE everything in the previous summary that is still true — do not drop facts, decisions, identifiers, or critical context just because they are not mentioned again in the new messages.
- FOLD IN what the new messages add: append new completed work to Done, move finished items out of In Progress into Done, add new decisions and next steps, and update or clear blockers that were resolved.
- Do NOT invent anything and do NOT re-derive or contradict the previous summary; only correct it when the new messages explicitly supersede it.
- Output only the updated checkpoint, no preamble.`

// compact collapses old tool-result messages into a short placeholder while
// preserving the system message, the original user task, and the most recent
// keepRecent messages verbatim. This is the deterministic, no-LLM fallback used
// when model summarization is unavailable or fails.
func compact(messages []Message, keepRecent int) []Message {
	if keepRecent < 2 {
		keepRecent = 2
	}
	if len(messages) <= keepRecent+2 {
		return messages
	}

	out := make([]Message, 0, len(messages))
	cutoff := len(messages) - keepRecent
	collapsed := 0
	for i, m := range messages {
		// Always keep the leading system prompt and the recent tail verbatim.
		if m.Role == RoleSystem || i >= cutoff {
			out = append(out, m)
			continue
		}
		// Collapse bulky tool results in the older region; keep their linkage.
		if m.Role == RoleTool && len(m.Content) > 256 {
			collapsed++
			out = append(out, Message{
				Role:       RoleTool,
				ToolCallID: m.ToolCallID,
				Name:       m.Name,
				Content:    "[older tool result elided to fit context]",
			})
			continue
		}
		out = append(out, m)
	}
	if collapsed > 0 {
		// Leave a breadcrumb so the model knows history was trimmed, inserted
		// right after any leading system prompt.
		note := Message{Role: RoleSystem, Content: fmt.Sprintf(
			"%s %d older tool results were elided to stay within the model window. Re-run a tool if you need the detail.", elideMarker, collapsed)}
		at := 0
		if len(out) > 0 && out[0].Role == RoleSystem {
			at = 1
		}
		out = append(out[:at:at], append([]Message{note}, out[at:]...)...)
	}
	return out
}
