package agentruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"time"

	"github.com/google/uuid"
	"github.com/2found/2ai/agentcore"
	nativehost "github.com/2found/2ai/agentcore/host"
)

var ErrPiQuestionPending = errors.New("Pi session is waiting for a human answer")

// A routed answer must resume its original child before the parent's next
// model request. Until that completion is attached, delivering it as an
// ordinary parent message would leave the delegation unfinished.
var ErrPiChildResumeRequired = errors.New("Pi session must resume its delegated child before continuing")

// piAnswerMessages returns durable answers that Pi has not yet appended through
// its native message lifecycle. A crash after message_end cannot deliver twice;
// a crash before it leaves the exact recorded message available for retry.
func piAnswerMessages(entries []agentcore.SessionEntry, state json.RawMessage) ([]json.RawMessage, error) {
	delegations, err := piDelegations(entries)
	if err != nil {
		return nil, err
	}
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
	routed := map[string]bool{}
	var pending []json.RawMessage
	for _, entry := range entries {
		if id, question, valid := agentcore.PiQuestionFromEntry(entry); valid {
			questions[id] = question
		}
		if id, _, valid := agentcore.PiChildQuestionFromEntry(entry); valid {
			routed[id] = true
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
		} else if !routed[entry.CallID] {
			pending = append(pending, json.RawMessage(entry.Content))
		}
	}
	// Batch receipts own the complete delivery order and may suppress every
	// sibling's additional context when any settled outcome is terminal.
	batched := map[string]bool{}
	var deliveries []json.RawMessage
	for _, entry := range entries {
		if entry.Kind != agentcore.EntryPiDelegationBatch {
			continue
		}
		batches, err := piDelegationBatches(entries, state)
		if err != nil {
			return nil, err
		}
		for _, b := range batches {
			if b.Receipt == nil {
				continue
			}
			for _, origin := range b.Origins {
				batched[origin] = true
			}
			deliveries = append(deliveries, b.Receipt.Messages...)
			if b.Receipt.End {
				for _, d := range b.Delegations {
					if d == nil {
						continue
					}
					for _, raw := range native.Messages {
						var header struct{ AgentrayDelegationContextID string }
						_ = json.Unmarshal(raw, &header)
						for _, forbidden := range d.Delivery.Contexts {
							var identity struct{ AgentrayDelegationContextID string }
							_ = json.Unmarshal(forbidden, &identity)
							if header.AgentrayDelegationContextID != "" && header == identity {
								return nil, errors.New("terminal delegation batch delivered suppressed context")
							}
						}
					}
				}
			}
		}
		break
	}
	for _, delegation := range delegations {
		if !delegation.Complete {
			continue
		}
		if !batched[delegation.OriginID] {
			deliveries = append(deliveries, delegation.Delivery.Message)
			deliveries = append(deliveries, delegation.Delivery.Contexts...)
		}
	}
	for _, expected := range deliveries {
		var identity struct{ AgentrayDelegationID, AgentrayDelegationContextID, AgentrayDelegationBatchContextID string }
		_ = json.Unmarshal(expected, &identity)
		seen := false
		for _, message := range native.Messages {
			var header struct{ AgentrayDelegationID, AgentrayDelegationContextID, AgentrayDelegationBatchContextID string }
			if json.Unmarshal(message, &header) != nil || header != identity {
				continue
			}
			if seen || !nativehost.SameJSON(message, expected) {
				return nil, errors.New("native delegation delivery is repeated or differs from its receipt")
			}
			seen = true
		}
		if !seen {
			pending = append(pending, expected)
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
	delegations, err := piDelegations(entries)
	if err != nil {
		return nil, err
	}
	for _, delegation := range delegations {
		if !delegation.Complete {
			return nil, ErrPiChildResumeRequired
		}
	}
	state, err := s.agent.State(ctx)
	if err != nil {
		return nil, err
	}
	batches, err := piDelegationBatches(entries, state)
	if err != nil {
		return nil, err
	}
	for _, b := range batches {
		if !b.Ready || (b.Receipt == nil && !b.Delivered) {
			return nil, ErrPiChildResumeRequired
		}
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

type piDelegationReceipt struct {
	ID       string            `json:"effectId"`
	OriginID string            `json:"originEffectId"`
	AnswerID string            `json:"answerId"`
	Result   json.RawMessage   `json:"result"`
	Message  json.RawMessage   `json:"message,omitempty"`
	Contexts []json.RawMessage `json:"contexts,omitempty"`
}

type piDelegation struct {
	OriginID, QuestionID string
	Original             agentcore.PiToolOutcome
	Answered, Complete   bool
	Delivery             piDelegationReceipt
}

// Fold the continuation chain before trusting it for replay or delivery. Each
// continuation consumes exactly one previously answered parent question, and
// must retain the original tool call. Native tool messages remain immutable.
func piDelegations(entries []agentcore.SessionEntry) ([]*piDelegation, error) {
	var ordered []*piDelegation
	questions := map[string]*piDelegation{}
	ids := map[string]bool{}
	for _, entry := range entries {
		if entry.Kind == agentcore.EntryPiEffectDone {
			id, _, valid := agentcore.PiChildQuestionFromEntry(entry)
			if !valid {
				continue
			}
			if ids[id] {
				return nil, errors.New("repeated native delegation identity")
			}
			var receipt struct {
				Result struct{ Details agentcore.PiToolOutcome }
			}
			_ = json.Unmarshal([]byte(entry.Content), &receipt)
			delegation := &piDelegation{OriginID: id, QuestionID: id, Original: receipt.Result.Details}
			ordered = append(ordered, delegation)
			questions[id], ids[id] = delegation, true
			continue
		}
		if entry.Kind == agentcore.EntryPiAnswer {
			if delegation := questions[entry.CallID]; delegation != nil {
				if delegation.QuestionID != entry.CallID || delegation.Answered || delegation.Complete {
					return nil, errors.New("repeated or stale delegated answer")
				}
				delegation.Answered = true
			}
			continue
		}
		if entry.Kind != agentcore.EntryPiDelegation {
			continue
		}
		var receipt piDelegationReceipt
		if json.Unmarshal([]byte(entry.Content), &receipt) != nil || receipt.ID == "" || ids[receipt.ID] {
			return nil, errors.New("invalid native delegation receipt")
		}
		delegation := questions[receipt.AnswerID]
		if delegation == nil || !delegation.Answered || delegation.Complete || delegation.QuestionID != receipt.AnswerID || delegation.OriginID != receipt.OriginID {
			return nil, errors.New("native delegation continuation has no unique answered predecessor")
		}
		var result struct {
			Details agentcore.PiToolOutcome
			Content []json.RawMessage
			IsError *bool
		}
		if json.Unmarshal(receipt.Result, &result) != nil || result.Details.Trace.CallID != delegation.Original.Trace.CallID || result.Details.Trace.Tool != delegation.Original.Trace.Tool || !nativehost.SameJSON(json.RawMessage(result.Details.Trace.Args), json.RawMessage(delegation.Original.Trace.Args)) || entry.CallID != delegation.Original.Trace.CallID {
			return nil, errors.New("native delegation continuation changed its original call")
		}
		if result.Content == nil || result.IsError == nil || *result.IsError != (result.Details.Trace.Error != "" || !result.Details.Trace.Allowed) || (result.Details.Executed && result.Details.Trace.IdempotencyKey != delegation.Original.Trace.IdempotencyKey) {
			return nil, errors.New("native delegation result has inconsistent execution metadata")
		}
		if result.Details.Parked {
			id, _, valid := agentcore.PiChildQuestionFromEntry(entry)
			if !valid || id != receipt.ID || len(receipt.Message) != 0 || len(receipt.Contexts) != 0 {
				return nil, errors.New("invalid continued child question")
			}
			delegation.QuestionID, delegation.Answered = id, false
			questions[id] = delegation
		} else {
			if result.Details.ChildQuestion != nil {
				return nil, errors.New("completed delegation still carries a child question")
			}
			var message struct{ Timestamp int64 }
			if json.Unmarshal(receipt.Message, &message) != nil || message.Timestamp <= 0 {
				return nil, errors.New("invalid delegation completion message")
			}
			expected, err := piDelegationMessage(receipt.ID, receipt.Result, message.Timestamp)
			if err != nil || !nativehost.SameJSON(receipt.Message, expected) {
				return nil, errors.New("delegation completion differs from its tool result")
			}
			contexts, err := piDelegationContexts(receipt.ID, result.Details, message.Timestamp)
			if err != nil || len(contexts) != len(receipt.Contexts) {
				return nil, errors.New("delegation context differs from its tool result")
			}
			for i := range contexts {
				if !nativehost.SameJSON(contexts[i], receipt.Contexts[i]) {
					return nil, errors.New("delegation context differs from its tool result")
				}
			}
			delegation.Complete, delegation.Delivery = true, receipt
		}
		ids[receipt.ID] = true
	}
	return ordered, nil
}

func piDelegationMessage(id string, result json.RawMessage, timestamp int64) (json.RawMessage, error) {
	var value struct {
		Content []json.RawMessage
		Details agentcore.PiToolOutcome
		IsError bool
	}
	if err := json.Unmarshal(result, &value); err != nil {
		return nil, err
	}
	status := "completed"
	if value.IsError {
		status = "failed"
	}
	header, _ := json.Marshal(map[string]string{"type": "text", "text": fmt.Sprintf("Delegation %s (call %s) %s:", value.Details.Trace.Tool, value.Details.Trace.CallID, status)})
	content := append([]json.RawMessage{header}, value.Content...)
	return json.Marshal(map[string]any{"role": "user", "content": content, "timestamp": timestamp, "agentrayDelegationId": id})
}

func piDelegationContexts(id string, outcome agentcore.PiToolOutcome, timestamp int64) ([]json.RawMessage, error) {
	// FinishPiTurn also discards additional contexts for terminal outcomes.
	if outcome.Terminate {
		return nil, nil
	}
	messages, err := nativehost.InputMessages(outcome.AdditionalContexts)
	if err != nil {
		return nil, err
	}
	for i, raw := range messages {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &fields); err != nil {
			return nil, err
		}
		fields["timestamp"], _ = json.Marshal(timestamp)
		fields["agentrayDelegationContextId"], _ = json.Marshal(fmt.Sprintf("%s/context/%d", id, i))
		messages[i], err = json.Marshal(fields)
		if err != nil {
			return nil, err
		}
	}
	return messages, nil
}

// A terminal tool continuation ends this run before another model request.
// Append its recorded deliveries through setState and checkpoint the resulting
// native state; do not fabricate a model/tool event sequence for work that ran
// outside the model loop. A failed checkpoint leaves the delivery replayable.
func (s *PiSession) finishTerminalDelegations(ctx context.Context, input json.RawMessage) (bool, error) {
	if s.config.Store == nil {
		return false, nil
	}
	entries, err := s.config.Store.Log(s.ctx, s.config.SessionID)
	if err != nil {
		return false, err
	}
	if _, _, pending := agentcore.PendingQuestion(entries); pending {
		return false, nil
	}
	state, err := s.agent.State(ctx)
	if err != nil {
		return false, err
	}
	var native struct{ Messages []json.RawMessage }
	if err := json.Unmarshal(state, &native); err != nil {
		return false, err
	}
	terminal := false
	batches, err := piDelegationBatches(entries, state)
	if err != nil {
		return false, err
	}
	for _, b := range batches {
		if !b.Ready {
			return false, nil
		}
		if b.Receipt != nil && b.Receipt.End && !b.Delivered {
			terminal = true
		}
	}
	if !terminal {
		return false, nil
	}
	input, err = s.answerInput(ctx, input)
	if err != nil {
		return false, err
	}
	var messages []json.RawMessage
	if err := json.Unmarshal(input, &messages); err != nil {
		return false, err
	}
	update, err := json.Marshal(map[string]any{"messages": append(native.Messages, messages...)})
	if err != nil {
		return false, err
	}
	if err := s.agent.setState(ctx, update); err != nil {
		return false, err
	}
	if err := s.checkpoint(s.ctx); err != nil {
		return false, err
	}
	return true, nil
}

func (s *PiSession) resumeDelegations(ctx context.Context, host *agentcore.PiToolHost, projection *piRunProjection) error {
	if s.config.Store == nil {
		return nil
	}
	entries, err := s.config.Store.Log(s.ctx, s.config.SessionID)
	if err != nil {
		return err
	}
	delegations, err := piDelegations(entries)
	if err != nil {
		return err
	}
	resumeCtx, cancel := context.WithCancel(s.ctx)
	stop := context.AfterFunc(ctx, cancel)
	defer func() { stop(); cancel() }()
	if ctx.Err() != nil {
		cancel()
	}
	for _, delegation := range delegations {
		if delegation.Complete || !delegation.Answered {
			continue
		}
		if host == nil {
			return ErrPiChildResumeRequired
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		err := func() (err error) {
			trace := projection.delegationStart(delegation.Original.Trace)
			var audit agentcore.PiToolOutcome
			defer func() { projection.delegationEnd(trace, audit, err) }()
			run := func(ctx context.Context) (json.RawMessage, error) {
				var raw json.RawMessage
				var err error
				raw, audit, err = host.ResumeDelegation(ctx, delegation.OriginID, delegation.Original, func(raw json.RawMessage) error {
					return projection.delegationUpdate(trace, raw)
				})
				return raw, err
			}
			result, err := s.agent.observeDelegation(resumeCtx, delegation.Original.Trace.Tool, run)
			if err != nil {
				return err
			}
			if err := resumeCtx.Err(); err != nil {
				return err
			}
			receipt := piDelegationReceipt{ID: uuid.NewString(), OriginID: delegation.OriginID, AnswerID: delegation.QuestionID}
			if audit.Parked {
				audit.QuestionID = receipt.ID
				var fields map[string]json.RawMessage
				if err := json.Unmarshal(result, &fields); err != nil {
					return err
				}
				fields["details"], _ = json.Marshal(audit)
				result, err = json.Marshal(fields)
			} else {
				timestamp := time.Now().UnixMilli()
				receipt.Message, err = piDelegationMessage(receipt.ID, result, timestamp)
				if err == nil {
					receipt.Contexts, err = piDelegationContexts(receipt.ID, audit, timestamp)
				}
			}
			if err != nil {
				return err
			}
			receipt.Result = result
			raw, err := json.Marshal(receipt)
			if err != nil {
				return err
			}
			entry := agentcore.SessionEntry{Kind: agentcore.EntryPiDelegation, CallID: audit.Trace.CallID, Content: string(raw)}
			entries = append(entries, entry)
			if _, err := piDelegations(entries); err != nil {
				return err
			}
			if err := s.record(resumeCtx, entry.Kind, entry.CallID, raw); err != nil {
				return err
			}
			return nil
		}()
		if err != nil {
			return err
		}
	}
	return s.finishDelegationBatches(resumeCtx, host)
}
