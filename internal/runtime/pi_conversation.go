package agentruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/2found/2ai/agentcore"
	nativehost "github.com/2found/2ai/agentcore/host"
	"github.com/lohi-ai/agentray/internal/dataplane/store"
)

const ConvKindPiHistory = "pi_history"

// PiConversationHistory is a native transcript at one immutable conversation
// branch point. LeafID anchors the next delta; display messages are never replay.
type PiConversationHistory struct {
	Messages json.RawMessage
	Revision string
	LeafID   string
}

type piConversationDelta struct {
	BaseEntryID string            `json:"base_entry_id"`
	Revision    string            `json:"revision"`
	Messages    []json.RawMessage `json:"messages"`
}

// PiConversationInputID identifies the last durable user input actually consumed
// by this native turn. Regeneration must not pick a later, still-pending input
// merely because its display entry happens to precede the answer.
func PiConversationInputID(entry storage.AgentConversationEntry) string {
	if entry.Kind != ConvKindPiHistory {
		return ""
	}
	var delta piConversationDelta
	if json.Unmarshal([]byte(entry.PayloadJSON), &delta) != nil {
		return ""
	}
	for i := len(delta.Messages) - 1; i >= 0; i-- {
		var message struct {
			Role string
			ID   string `json:"agentrayInputId"`
		}
		if json.Unmarshal(delta.Messages[i], &message) == nil && message.Role == "user" && message.ID != "" {
			return message.ID
		}
	}
	return ""
}

func BuildPiHistory(ctx context.Context, store *storage.Store, conversationID string) (PiConversationHistory, error) {
	entries, err := store.PathToLeaf(ctx, conversationID)
	if err != nil {
		return PiConversationHistory{}, err
	}
	return foldPiHistory(entries)
}

func buildPiResumeHistory(ctx context.Context, store *storage.Store, conversationID string) (PiConversationHistory, error) {
	entries, err := store.PathToLeaf(ctx, conversationID)
	if err != nil {
		return PiConversationHistory{}, err
	}
	for i := len(entries) - 1; i >= 0; i-- {
		if entries[i].Kind == ConvKindClear || entries[i].Kind == ConvKindPiCompaction {
			break
		}
		if entries[i].Kind == ConvKindPiHistory {
			return foldPiHistory(entries[:i+1])
		}
	}
	return PiConversationHistory{}, errors.New("parked native conversation has no history checkpoint on this branch")
}

func foldPiHistory(entries []storage.AgentConversationEntry) (PiConversationHistory, error) {
	type marker struct {
		count    int
		revision string
	}
	markers := map[string]marker{"": {}}
	messages := []json.RawMessage{}
	revision, leaf := "", ""
	start := 0
	for i, entry := range entries {
		if entry.Kind == ConvKindClear {
			start = i
		}
	}
	for i, entry := range entries {
		if i < start {
			continue
		}
		switch entry.Kind {
		case ConvKindClear:
			messages, revision = []json.RawMessage{}, ""
			markers = map[string]marker{}
		case ConvKindCompaction:
			return PiConversationHistory{}, errors.New("legacy conversation compaction requires explicit native migration or /clear")
		case ConvKindPiCompaction:
			var err error
			messages, err = foldPiCompaction(entry, leaf, revision, messages)
			if err != nil {
				return PiConversationHistory{}, err
			}
			markers = map[string]marker{}
		case ConvKindPiHistory:
			var delta piConversationDelta
			if json.Unmarshal([]byte(entry.PayloadJSON), &delta) != nil || delta.Revision == "" || delta.Messages == nil {
				return PiConversationHistory{}, errors.New("invalid native conversation history record")
			}
			base, ok := markers[delta.BaseEntryID]
			if !ok || base.count > len(messages) {
				return PiConversationHistory{}, errors.New("native history anchor is missing or was superseded")
			}
			if base.revision != "" && base.revision != delta.Revision {
				return PiConversationHistory{}, errors.New("native conversation mixes Pi revisions")
			}
			// Only inputs actually present in Pi state are covered. A correction
			// queued after the final drain remains available to the next run.
			consumed := map[string]bool{}
			for _, raw := range delta.Messages {
				var m struct {
					Role    string
					InputID string `json:"agentrayInputId"`
				}
				if json.Unmarshal(raw, &m) == nil && m.Role == "user" && m.InputID != "" {
					consumed[m.InputID] = true
				}
			}
			pending := []json.RawMessage{}
			for _, raw := range messages[base.count:] {
				var m struct {
					InputID string `json:"agentrayInputId"`
				}
				_ = json.Unmarshal(raw, &m)
				if !consumed[m.InputID] {
					pending = append(pending, raw)
				}
			}
			messages = append(messages[:base.count:base.count], delta.Messages...)
			messages = append(messages, pending...)
			revision = delta.Revision
			// Covered user/trace entries were projections of this native turn.
			// They cannot anchor a later delta on a different transcript prefix.
			markers = map[string]marker{}
		case ConvKindMessage:
			var payload convMessagePayload
			if json.Unmarshal([]byte(entry.PayloadJSON), &payload) != nil {
				return PiConversationHistory{}, errors.New("invalid conversation message")
			}
			if payload.Command || payload.PiDisplay || payload.Text == "" {
				break
			}
			if entry.Role != "user" {
				return PiConversationHistory{}, errors.New("legacy assistant history requires explicit native migration or /clear")
			}
			// User-authored text has no provider signature or hidden model fields.
			message, _ := json.Marshal(map[string]any{"role": "user", "content": payload.Text, "timestamp": entry.CreatedAt.UnixMilli(), "agentrayInputId": entry.ID})
			messages = append(messages, message)
		}
		leaf = entry.ID
		markers[entry.ID] = marker{len(messages), revision}
	}
	raw, err := json.Marshal(messages)
	if err != nil {
		return PiConversationHistory{}, err
	}
	if err := nativehost.ValidateMessages(raw); err != nil {
		return PiConversationHistory{}, err
	}
	return PiConversationHistory{Messages: raw, Revision: revision, LeafID: leaf}, nil
}

func (s *ChatService) persistPiTurn(ctx context.Context, req chatWork, runID string, result agentcore.RunResult) error {
	if req.ConversationID == "" || s.runner == nil || s.runner.Store == nil {
		return nil
	}
	if req.PiHistory == nil {
		return errors.New("native conversation run has no branch anchor")
	}
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	return appendPiConversationTurn(wctx, s.runner.Store, req.ConversationID, req.AgentID, runID, *req.PiHistory, result)
}

// piConversationSuffix only accepts an unchanged initial native transcript.
// This never derives provider messages from RunResult.Messages or UI text.
func piConversationSuffix(history json.RawMessage, state json.RawMessage) ([]json.RawMessage, error) {
	var before []json.RawMessage
	var after struct{ Messages []json.RawMessage }
	if len(history) > 0 {
		if err := json.Unmarshal(history, &before); err != nil {
			return nil, err
		}
	}
	if err := json.Unmarshal(state, &after); err != nil || after.Messages == nil {
		return nil, errors.New("native run has no message array")
	}
	if len(after.Messages) < len(before) {
		return nil, errors.New("native run replaced its seeded history")
	}
	for i := range before {
		if !nativehost.SameJSON(before[i], after.Messages[i]) {
			return nil, fmt.Errorf("native run changed seeded message %d", i)
		}
	}
	full, _ := json.Marshal(after.Messages)
	if err := nativehost.ValidateMessages(full); err != nil {
		return nil, err
	}
	return append([]json.RawMessage{}, after.Messages[len(before):]...), nil
}

func appendPiConversationTurn(ctx context.Context, store *storage.Store, conversationID, agentID, runID string, base PiConversationHistory, result agentcore.RunResult) error {
	if len(result.NativeState) == 0 {
		return nil
	}
	if result.NativeRevision == "" {
		return errors.New("native run did not report its Pi revision")
	}
	if base.Revision != "" && base.Revision != result.NativeRevision {
		return errors.New("native run changed Pi revision")
	}
	messages, err := piConversationSuffix(base.Messages, result.NativeState)
	if err != nil {
		return err
	}
	entries, err := store.PathToLeaf(ctx, conversationID)
	if err != nil {
		return err
	}
	// Native input IDs name immutable conversation entries. If a fork removed
	// the triggering turn while the model was running, even a shared root anchor
	// cannot authorize publishing that abandoned turn on the new branch.
	onPath := map[string]bool{}
	for _, entry := range entries {
		onPath[entry.ID] = true
	}
	for _, raw := range messages {
		var input struct {
			Role string
			ID   string `json:"agentrayInputId"`
		}
		if json.Unmarshal(raw, &input) == nil && input.Role == "user" && input.ID != "" && !onPath[input.ID] {
			return storage.ErrConversationLeafChanged
		}
	}
	baseIndex := -1
	if base.LeafID != "" {
		for i, e := range entries {
			if e.ID == base.LeafID {
				baseIndex = i
				break
			}
		}
		if baseIndex < 0 {
			return storage.ErrConversationLeafChanged
		}
	}
	current, err := foldPiHistory(entries[:baseIndex+1])
	if err != nil {
		return err
	}
	seed := base.Messages
	if len(seed) == 0 {
		seed = json.RawMessage(`[]`)
	}
	if !nativehost.SameJSON(current.Messages, seed) || current.Revision != base.Revision {
		return errors.New("native conversation seed differs from its branch anchor")
	}
	for _, entry := range entries[baseIndex+1:] {
		if entry.Kind == ConvKindPiHistory || entry.Kind == ConvKindClear || entry.Kind == ConvKindCompaction || entry.Kind == ConvKindPiCompaction {
			return storage.ErrConversationLeafChanged
		}
	}
	leaf := ""
	if len(entries) > 0 {
		leaf = entries[len(entries)-1].ID
	}
	payload, err := json.Marshal(piConversationDelta{BaseEntryID: base.LeafID, Revision: result.NativeRevision, Messages: messages})
	if err != nil {
		return err
	}
	_, err = store.AppendConversationEntryAtLeaf(ctx, storage.AgentConversationEntry{ConversationID: conversationID, Kind: ConvKindPiHistory, AgentID: agentID, RunID: runID, Turn: result.Turns, PayloadJSON: string(payload)}, leaf)
	return err
}
