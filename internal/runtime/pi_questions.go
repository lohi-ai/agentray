package agentruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"time"

	"github.com/lohi-ai/agentray/agentcore"
)

var ErrPiQuestionPending = errors.New("Pi session is waiting for a human answer")

// piAnswerMessages returns durable answers that Pi has not yet appended through
// its native message lifecycle. A crash after message_end cannot deliver twice;
// a crash before it leaves the exact recorded message available for retry.
func piAnswerMessages(entries []agentcore.SessionEntry, state json.RawMessage) ([]json.RawMessage, error) {
	var native struct{ Messages []json.RawMessage }
	if err := json.Unmarshal(state, &native); err != nil {
		return nil, err
	}
	delivered := map[string]any{}
	for _, raw := range native.Messages {
		var header struct{ Role, AgentrayAnswerID string }
		if json.Unmarshal(raw, &header) != nil || header.Role != "user" || header.AgentrayAnswerID == "" {
			continue
		}
		var message any
		_ = json.Unmarshal(raw, &message)
		if _, exists := delivered[header.AgentrayAnswerID]; exists {
			return nil, errors.New("native Pi history contains a repeated human answer")
		}
		delivered[header.AgentrayAnswerID] = message
	}
	questions, answered := map[string]json.RawMessage{}, map[string]bool{}
	var pending []json.RawMessage
	for _, entry := range entries {
		if id, question, valid := agentcore.PiQuestionFromEntry(entry); valid {
			questions[id] = question
		}
		if entry.Kind != agentcore.EntryPiAnswer {
			continue
		}
		if len(questions[entry.CallID]) == 0 || answered[entry.CallID] {
			return nil, errors.New("Pi answer has no unique preceding parked question")
		}
		answered[entry.CallID] = true
		var header struct {
			Role, AgentrayAnswerID, Content string
			Timestamp                       int64
		}
		var message any
		expected := "Human answer to question " + string(questions[entry.CallID]) + ":\n" + entry.Answer
		if json.Unmarshal([]byte(entry.Content), &header) != nil || header.Role != "user" || header.AgentrayAnswerID != entry.CallID || header.Content != expected || header.Timestamp <= 0 || json.Unmarshal([]byte(entry.Content), &message) != nil {
			return nil, errors.New("invalid native Pi human answer")
		}
		if seen, exists := delivered[entry.CallID]; exists {
			if !reflect.DeepEqual(message, seen) {
				return nil, errors.New("native Pi human answer differs from its durable record")
			}
		} else {
			pending = append(pending, json.RawMessage(entry.Content))
		}
	}
	// Seeded native history may include answers delivered in an earlier durable
	// session. Only this session's answer records schedule new deliveries.
	return pending, nil
}

// PendingQuestion exposes the host workflow without fabricating native events.
// Answer via RecordSessionAnswer while holding the same durable session lease.
func (s *PiSession) PendingQuestion(ctx context.Context) (json.RawMessage, error) {
	if s.config.Store == nil {
		return nil, nil
	}
	entries, err := s.config.Store.Log(ctx, s.config.SessionID)
	if err != nil {
		return nil, err
	}
	_, question, _ := agentcore.PendingQuestion(entries)
	return question, nil
}

func (s *PiSession) answerInput(ctx context.Context, input json.RawMessage) (json.RawMessage, error) {
	if s.config.Store == nil {
		return input, nil
	}
	entries, err := s.config.Store.Log(ctx, s.config.SessionID)
	if err != nil {
		return nil, err
	}
	if _, _, pending := agentcore.PendingQuestion(entries); pending {
		return nil, ErrPiQuestionPending
	}
	state, err := s.agent.State(ctx)
	if err != nil {
		return nil, err
	}
	answers, err := piAnswerMessages(entries, state)
	if err != nil || len(answers) == 0 {
		return input, err
	}
	if len(input) > 0 {
		var value any
		if err := json.Unmarshal(input, &value); err != nil {
			return nil, err
		}
		switch value := value.(type) {
		case string:
			message, _ := json.Marshal(map[string]any{"role": "user", "content": value, "timestamp": time.Now().UnixMilli()})
			answers = append(answers, message)
		case []any:
			var messages []json.RawMessage
			_ = json.Unmarshal(input, &messages)
			answers = append(answers, messages...)
		case map[string]any:
			answers = append(answers, input)
		default:
			return nil, fmt.Errorf("invalid Pi prompt input %T", value)
		}
	}
	return json.Marshal(answers)
}
