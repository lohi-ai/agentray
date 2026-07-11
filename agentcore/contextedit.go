package agentcore

import (
	"encoding/json"
	"fmt"
	"strings"
)

// contextedit.go — cheap, deterministic context editing that runs before full
// compaction. Agent transcripts are mostly tool results, and most of those are
// dead weight a few turns later: the model already acted on them, a newer call
// superseded them, or a later edit made them wrong. Past a soft threshold
// (half the compaction budget) this pass batch-replaces such results with
// one-line placeholders — no summarization call, no information the model
// still needs. Batching matters: every cleared block invalidates the prompt-
// cache prefix from that point, so edits fire together at the threshold rather
// than dribbling one per turn.

// Conventional sandbox file-tool names. The staleness rule (a read made wrong
// by a later write) keys on these; consumers whose tools use other names simply
// don't trigger that rule — the age and superseded rules are name-agnostic.
const (
	editToolNameRead  = "read_file"
	editToolNameWrite = "write_file"
	editToolNameEdit  = "edit_file"
)

// editClearMinBytes is the floor below which an old tool result is not worth
// clearing on age alone (the placeholder would barely be smaller).
const editClearMinBytes = 1024

// editSupersededMinBytes is the (lower) floor for results that are superseded
// or stale — those are cleared more aggressively because a newer result already
// carries the truth.
const editSupersededMinBytes = 128

// editContext runs the batched clearing pass over messages when the estimated
// context exceeds half of budget (the compaction budget; <=0 uses the
// default). Only messages older than the keep-recent window are touched — the
// tail the model is actively working from stays verbatim. Within the old span
// it clears, in order of confidence:
//
//  1. results superseded by a newer call with identical tool name + arguments
//     (the newest occurrence, wherever it is, keeps the truth);
//  2. read_file results made stale by a later edit_file/write_file to the same
//     path (the content is not just bulky, it is wrong);
//  3. any bulky tool result on age alone (re-runnable if needed).
//
// It never mutates the input slice; callers get either the original slice
// (edited=false) or a copy. The pass is idempotent: placeholders are small, so
// a second run past the threshold finds nothing new and reports edited=false,
// letting full compaction proceed.
func editContext(messages []Message, budget int, settings CompactionSettings) ([]Message, bool) {
	if budget <= 0 {
		budget = defaultContextTokenBudget
	}
	if estimateContextTokens(messages) <= budget/2 {
		return messages, false
	}

	sysN := leadingSystemCount(messages)
	body := messages[sysN:]
	cut := findCutPoint(body, settings.KeepRecentTokens)
	if cut <= 0 {
		return messages, false
	}

	// Index the whole transcript once: call arguments by id, the newest
	// occurrence of each identical call, and the latest write per file path.
	callArgs := map[string]ToolCall{}   // ToolCallID -> originating call
	newestByCall := map[string]int{}    // name\x00args -> newest body index of its result
	lastWriteByPath := map[string]int{} // path -> newest body index of an edit/write call
	for i, m := range body {
		if m.Role == RoleAssistant {
			for _, tc := range m.ToolCalls {
				callArgs[tc.ID] = tc
				if (tc.Name == editToolNameWrite || tc.Name == editToolNameEdit) && toolArgPath(tc.Arguments) != "" {
					lastWriteByPath[toolArgPath(tc.Arguments)] = i
				}
			}
		}
		if m.Role == RoleTool && m.ToolCallID != "" {
			if tc, ok := callArgs[m.ToolCallID]; ok {
				newestByCall[tc.Name+"\x00"+tc.Arguments] = i
			}
		}
	}

	out := make([]Message, len(messages))
	copy(out, messages)
	edited := false
	clearAt := func(i int, placeholder string) {
		m := body[i]
		out[sysN+i] = Message{
			Role:       RoleTool,
			ToolCallID: m.ToolCallID,
			Name:       m.Name,
			Content:    placeholder,
		}
		edited = true
	}

	for i := 0; i < cut; i++ {
		m := body[i]
		if m.Role != RoleTool || m.ToolCallID == "" {
			continue
		}
		tc, haveCall := callArgs[m.ToolCallID]

		// Rule 1: a newer identical call exists — its result is the current truth.
		if haveCall && len(m.Content) > editSupersededMinBytes {
			if newest := newestByCall[tc.Name+"\x00"+tc.Arguments]; newest > i {
				clearAt(i, fmt.Sprintf("[superseded by a newer identical %s call — re-run it if you need the content]", tc.Name))
				continue
			}
		}
		// Rule 2: a read whose file was modified afterwards is wrong, not just old.
		if haveCall && tc.Name == editToolNameRead && len(m.Content) > editSupersededMinBytes {
			if p := toolArgPath(tc.Arguments); p != "" && lastWriteByPath[p] > i {
				clearAt(i, fmt.Sprintf("[stale read_file result — %s was modified later; re-read it if needed]", p))
				continue
			}
		}
		// Rule 3: bulky and old — re-runnable, so the bytes are not worth keeping.
		if len(m.Content) > editClearMinBytes {
			name := m.Name
			if name == "" && haveCall {
				name = tc.Name
			}
			if name == "" {
				name = "the tool"
			}
			clearAt(i, fmt.Sprintf("[older tool result cleared to save context — re-run %s if you need the detail]", name))
		}
	}

	if !edited {
		return messages, false
	}
	return out, true
}

// toolArgPath extracts the conventional "path" argument from a tool call's raw
// JSON arguments; "" when absent or unparsable.
func toolArgPath(args string) string {
	var in struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal([]byte(args), &in); err != nil {
		return ""
	}
	return strings.TrimSpace(in.Path)
}
