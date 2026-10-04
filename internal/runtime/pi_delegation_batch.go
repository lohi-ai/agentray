package agentruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/lohi-ai/agentray/agentcore"
	nativehost "github.com/lohi-ai/agentray/agentcore/host"
)

// A batch decision is host metadata, not a second native turn. Its identity is
// the first delegated physical effect in assistant source order, or the first
// local question when no child is delegated. Completions bind it to delegated
// continuations or local human answers.
type piDelegationBatchReceipt struct {
	ID          string              `json:"batchId"`
	Origins     []string            `json:"origins"`
	Completions []string            `json:"completions"`
	CallsDigest string              `json:"callsDigest"`
	Timestamp   int64               `json:"timestamp"`
	End         bool                `json:"end"`
	Extra       []agentcore.Message `json:"extra,omitempty"`
	Messages    []json.RawMessage   `json:"messages"`
}

type piDelegationBatch struct {
	Calls       []agentcore.ToolCall
	Outcomes    []agentcore.PiToolOutcome
	Delegations []*piDelegation // One slot per call; nil for ordinary siblings.
	Origins     []string
	Completions []string
	Digest      string
	Ready       bool
	ReadyAt     int
	Delivered   bool // All primary completions are already in native history.
	Receipt     *piDelegationBatchReceipt
}

func piDelegationBatches(entries []agentcore.SessionEntry, state json.RawMessage) ([]*piDelegationBatch, error) {
	delegations, err := piDelegations(entries)
	if err != nil {
		return nil, err
	}
	byOrigin := map[string]*piDelegation{}
	for _, d := range delegations {
		byOrigin[d.OriginID] = d
	}
	localQuestions := map[string]agentcore.PiToolOutcome{}
	for _, entry := range entries {
		if id, _, valid := agentcore.PiQuestionFromEntry(entry); valid {
			var receipt struct {
				Result struct{ Details agentcore.PiToolOutcome }
			}
			_ = json.Unmarshal([]byte(entry.Content), &receipt)
			if receipt.Result.Details.ChildQuestion == nil {
				localQuestions[id] = receipt.Result.Details
			}
		}
	}
	answeredAt, completedAt := map[string]int{}, map[string]int{}
	for i, entry := range entries {
		if entry.Kind == agentcore.EntryPiAnswer {
			answeredAt[entry.CallID] = i + 1
		}
		if entry.Kind == agentcore.EntryPiDelegation {
			var receipt piDelegationReceipt
			_ = json.Unmarshal([]byte(entry.Content), &receipt)
			completedAt[receipt.ID] = i + 1
		}
	}
	var native struct{ Messages []json.RawMessage }
	if err := json.Unmarshal(state, &native); err != nil {
		return nil, err
	}
	delivered := map[string]bool{}
	answersDelivered := map[string]bool{}
	for _, raw := range native.Messages {
		var header struct{ AgentrayDelegationID, AgentrayAnswerID string }
		_ = json.Unmarshal(raw, &header)
		if header.AgentrayAnswerID != "" {
			answersDelivered[header.AgentrayAnswerID] = true
		}
		if header.AgentrayDelegationID != "" {
			delivered[header.AgentrayDelegationID] = true
		}
	}
	seen := map[string]bool{}
	localSeen := map[string]bool{}
	var batches []*piDelegationBatch
	for i, raw := range native.Messages {
		var header struct{ Role string }
		_ = json.Unmarshal(raw, &header)
		if header.Role != "assistant" {
			continue
		}
		message, err := nativehost.ProjectMessage(raw)
		if err != nil {
			return nil, err
		}
		if len(message.ToolCalls) == 0 {
			continue
		}
		type result struct {
			Role, ToolCallID, ToolName string
			Details                    json.RawMessage
		}
		results := map[string]result{}
		duplicate := false
		for _, next := range native.Messages[i+1:] {
			var r result
			if json.Unmarshal(next, &r) != nil || r.Role != "toolResult" {
				break
			}
			_, exists := results[r.ToolCallID]
			duplicate = duplicate || exists
			results[r.ToolCallID] = r
		}
		b := &piDelegationBatch{Calls: message.ToolCalls, Ready: true, Delivered: true}
		var locals []string
		callIDs := map[string]bool{}
		for _, call := range b.Calls {
			duplicate = duplicate || callIDs[call.ID]
			callIDs[call.ID] = true
			r, exists := results[call.ID]
			if !exists {
				b.Ready = false
			}
			var audit agentcore.PiToolOutcome
			governed := json.Unmarshal(r.Details, &audit) == nil && audit.Trace.CallID == call.ID && audit.Trace.Tool == call.Name && r.ToolName == call.Name
			var d *piDelegation
			if governed {
				d = byOrigin[audit.QuestionID]
				if d != nil {
					actual, _ := json.Marshal(audit)
					original, _ := json.Marshal(d.Original)
					if seen[d.OriginID] || !nativehost.SameJSON(actual, original) {
						return nil, errors.New("delegation batch has repeated or changed original outcome")
					}
					seen[d.OriginID] = true
					b.Origins = append(b.Origins, d.OriginID)
					b.Completions = append(b.Completions, d.Delivery.ID)
					b.Ready = b.Ready && d.Complete
					b.Delivered = b.Delivered && delivered[d.Delivery.ID]
					b.ReadyAt = max(b.ReadyAt, completedAt[d.Delivery.ID])
					if d.Complete {
						var final struct{ Details agentcore.PiToolOutcome }
						_ = json.Unmarshal(d.Delivery.Result, &final)
						audit = final.Details
					}
				} else if original, valid := localQuestions[audit.QuestionID]; audit.Parked && valid {
					if localSeen[audit.QuestionID] || !nativehost.SameJSON(passiveNativeJSON(audit), passiveNativeJSON(original)) {
						return nil, errors.New("parked batch has repeated or changed original outcome")
					}
					localSeen[audit.QuestionID] = true
					locals = append(locals, audit.QuestionID)
					// A local ask sibling settles by answer, without reexecuting
					// its tool. Parking forced Terminate in the original result.
					at := answeredAt[audit.QuestionID]
					b.Ready = b.Ready && at > 0
					b.ReadyAt = max(b.ReadyAt, at)
					if at > 0 {
						audit.Parked, audit.Terminate = false, false
					}
				}
			} else {
				audit = agentcore.PiToolOutcome{}
			}
			b.Outcomes = append(b.Outcomes, audit)
			b.Delegations = append(b.Delegations, d)
		}
		if len(b.Origins) == 0 && len(locals) > 0 {
			// Local asks complete with their durable human answers. Keep the
			// existing delegated identity for mixed batches and older receipts.
			b.Origins, b.Completions = locals, slices.Clone(locals)
			for _, id := range locals {
				b.Delivered = b.Delivered && answersDelivered[id]
			}
		}
		if len(b.Origins) == 0 {
			continue // Unrelated or seeded historical batch.
		}
		if duplicate || len(results) != len(b.Calls) {
			return nil, errors.New("delegation batch has ambiguous or missing native results")
		}
		var digestCalls []json.RawMessage
		for _, call := range b.Calls {
			// Arguments are JSON, not a string: JSONB key ordering must not
			// change the binding when the log is read from SQL storage.
			encoded, err := json.Marshal(struct {
				ID, Name  string
				Arguments json.RawMessage
			}{call.ID, call.Name, json.RawMessage(call.Arguments)})
			if err != nil {
				return nil, err
			}
			digestCalls = append(digestCalls, encoded)
		}
		b.Digest = nativehost.PrefixDigest(digestCalls)
		batches = append(batches, b)
	}
	if len(seen) != len(byOrigin) {
		return nil, errors.New("delegation has no original native batch")
	}
	byID := map[string]*piDelegationBatch{}
	for _, b := range batches {
		byID[b.Origins[0]] = b
	}
	for i, entry := range entries {
		if entry.Kind != agentcore.EntryPiDelegationBatch {
			continue
		}
		var r piDelegationBatchReceipt
		if json.Unmarshal([]byte(entry.Content), &r) != nil {
			return nil, errors.New("invalid delegation batch receipt")
		}
		b := byID[r.ID]
		if b == nil || b.Receipt != nil || !b.Ready || b.ReadyAt > i || entry.CallID != r.ID || r.Timestamp <= 0 || r.CallsDigest != b.Digest || !slices.Equal(r.Origins, b.Origins) || !slices.Equal(r.Completions, b.Completions) {
			return nil, errors.New("delegation batch receipt has no unique settled predecessor")
		}
		terminal := false
		for _, outcome := range b.Outcomes {
			terminal = terminal || outcome.Terminate
		}
		if r.End != terminal {
			return nil, errors.New("delegation batch changed its terminal decision")
		}
		if r.End && len(r.Extra) != 0 {
			return nil, errors.New("terminal delegation batch retains extra context")
		}
		expected, err := b.messages(r)
		if err != nil || !nativehost.SameJSON(passiveNativeJSON(expected), passiveNativeJSON(r.Messages)) {
			return nil, errors.New("delegation batch deliveries differ from their receipt")
		}
		b.Receipt = &r
	}
	for _, b := range batches {
		if b.Receipt == nil && !b.Delivered {
			for _, id := range b.Origins {
				if answersDelivered[id] {
					return nil, errors.New("partially delivered parked batch has no decision receipt")
				}
			}
			for _, d := range b.Delegations {
				if d != nil && d.Complete && delivered[d.Delivery.ID] {
					return nil, errors.New("partially delivered delegation batch has no decision receipt")
				}
			}
		}
	}
	return batches, nil
}

func (b *piDelegationBatch) messages(r piDelegationBatchReceipt) ([]json.RawMessage, error) {
	var messages []json.RawMessage
	for _, d := range b.Delegations {
		if d != nil {
			messages = append(messages, d.Delivery.Message)
		}
	}
	if r.End {
		return messages, nil
	}
	for i, outcome := range b.Outcomes {
		if d := b.Delegations[i]; d != nil {
			messages = append(messages, d.Delivery.Contexts...)
		} else {
			contexts, err := piBatchContexts(fmt.Sprintf("%s/tool/%d", r.ID, i), outcome.AdditionalContexts, r.Timestamp)
			if err != nil {
				return nil, err
			}
			messages = append(messages, contexts...)
		}
	}
	extra, err := piBatchContexts(r.ID+"/hook", r.Extra, r.Timestamp)
	return append(messages, extra...), err
}

func piBatchContexts(id string, extra []agentcore.Message, timestamp int64) ([]json.RawMessage, error) {
	messages, err := nativehost.InputMessages(extra)
	if err != nil {
		return nil, err
	}
	for i, raw := range messages {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &fields); err != nil {
			return nil, err
		}
		fields["timestamp"], _ = json.Marshal(timestamp)
		fields["agentrayDelegationBatchContextId"], _ = json.Marshal(fmt.Sprintf("%s/%d", id, i))
		messages[i], err = json.Marshal(fields)
		if err != nil {
			return nil, err
		}
	}
	return messages, nil
}

func (s *PiSession) finishDelegationBatches(ctx context.Context, host *agentcore.PiToolHost) error {
	entries, err := s.config.Store.Log(ctx, s.config.SessionID)
	if err != nil {
		return err
	}
	state, err := s.agent.State(ctx)
	if err != nil {
		return err
	}
	batches, err := piDelegationBatches(entries, state)
	if err != nil {
		return err
	}
	for _, b := range batches {
		if !b.Ready || b.Receipt != nil || b.Delivered {
			continue
		}
		if host == nil {
			return ErrPiChildResumeRequired
		}
		// Tool contexts already belong to their original outcomes. Obtain only
		// hook additions here, then compose all deliveries in source order.
		outcomes := slices.Clone(b.Outcomes)
		for i := range outcomes {
			outcomes[i].AdditionalContexts = nil
		}
		decision, err := host.FinishPiDelegationBatch(ctx, b.Calls, outcomes)
		if err != nil {
			return err
		}
		if decision.Parked {
			return errors.New("settled delegation batch remains parked")
		}
		r := piDelegationBatchReceipt{ID: b.Origins[0], Origins: b.Origins, Completions: b.Completions, CallsDigest: b.Digest, Timestamp: time.Now().UnixMilli(), End: decision.End, Extra: decision.Inject}
		r.Messages, err = b.messages(r)
		if err != nil {
			return err
		}
		raw, err := json.Marshal(r)
		if err != nil {
			return err
		}
		entry := agentcore.SessionEntry{Kind: agentcore.EntryPiDelegationBatch, CallID: r.ID, Content: string(raw)}
		entries = append(entries, entry)
		if _, err := piDelegationBatches(entries, state); err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := s.record(ctx, entry.Kind, entry.CallID, raw); err != nil {
			return err
		}
	}
	return nil
}
