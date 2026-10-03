package agentruntime

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/lohi-ai/agentray/agentcore"
)

func piQuestionLog(t *testing.T) []agentcore.SessionEntry {
	t.Helper()
	store := agentcore.NewMemorySessionStore()
	ctx, release, err := agentcore.AcquireSessionLease(context.Background(), store, "question")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	outcome := agentcore.PiToolOutcome{Executed: true, Parked: true, QuestionID: "physical-id", Trace: agentcore.ToolTrace{CallID: "provider-id", Allowed: true, Tool: "ask", Args: `{"question":"Which?"}`}}
	receipt := piModelJSON(map[string]any{"effectId": "physical-id", "result": map[string]any{"details": outcome}})
	if err := store.Append(ctx, "question", agentcore.SessionEntry{Kind: agentcore.EntryPiEffectDone, CallID: "provider-id", Content: string(receipt)}); err != nil {
		t.Fatal(err)
	}
	if _, err := agentcore.RecordSessionAnswer(ctx, store, "question", "physical-id", "Human choice"); err != nil {
		t.Fatal(err)
	}
	entries, err := store.Log(ctx, "question")
	if err != nil {
		t.Fatal(err)
	}
	return entries
}

func TestPiDelegationReceiptChainAndDeliveryValidation(t *testing.T) {
	entries := piQuestionLog(t)
	var question struct {
		ID     string                                    `json:"effectId"`
		Result struct{ Details agentcore.PiToolOutcome } `json:"result"`
	}
	if err := json.Unmarshal([]byte(entries[0].Content), &question); err != nil {
		t.Fatal(err)
	}
	audit := &question.Result.Details
	audit.ChildQuestion = &agentcore.ChildQuestionError{SessionID: "parent/child", QuestionID: "child-id", Question: json.RawMessage(audit.Trace.Args)}
	audit.Trace.Tool, audit.Trace.Args = "spawn_subagent", `{"task":"work"}`
	entries[0].Content = string(piModelJSON(question))
	completed := *audit
	completed.ChildQuestion, completed.Parked, completed.QuestionID = nil, false, ""
	completed.AdditionalContexts = []agentcore.Message{{Role: agentcore.RoleUser, Content: "user context"}, {Role: agentcore.RoleSystem, Content: "system context"}}
	result := piModelJSON(map[string]any{"content": []any{map[string]any{"type": "text", "text": "child final"}}, "details": completed, "isError": false})
	receipt := piDelegationReceipt{ID: "delivery-id", OriginID: "physical-id", AnswerID: "physical-id", Result: result}
	var err error
	receipt.Message, err = piDelegationMessage(receipt.ID, receipt.Result, 100)
	if err != nil {
		t.Fatal(err)
	}
	receipt.Contexts, err = piDelegationContexts(receipt.ID, completed, 100)
	if err != nil {
		t.Fatal(err)
	}
	entries = append(entries, agentcore.SessionEntry{Kind: agentcore.EntryPiDelegation, CallID: "provider-id", Content: string(piModelJSON(receipt))})
	deliveries := append([]json.RawMessage{receipt.Message}, receipt.Contexts...)
	for delivered := 0; delivered <= len(deliveries); delivered++ {
		messages := deliveries[:delivered]
		pending, err := piAnswerMessages(entries, piModelJSON(map[string]any{"messages": messages}))
		want := len(deliveries) - delivered
		if err != nil || len(pending) != want || (want > 0 && !samePiJSON(pending[0], deliveries[delivered])) {
			t.Fatalf("delegation delivery was lost/duplicated: delivered=%v pending=%s err=%v", delivered, pending, err)
		}
	}
	for _, mutate := range []func(*piDelegationReceipt){
		func(r *piDelegationReceipt) { r.OriginID = "other" },
		func(r *piDelegationReceipt) { r.AnswerID = "other" },
		func(r *piDelegationReceipt) { r.ID = "physical-id" },
		func(r *piDelegationReceipt) { r.Contexts = nil },
		func(r *piDelegationReceipt) {
			r.Message = json.RawMessage(strings.Replace(string(r.Message), "child final", "altered", 1))
		},
		func(r *piDelegationReceipt) {
			r.Result = json.RawMessage(strings.Replace(string(r.Result), "spawn_subagent", "another_tool", 1))
		},
	} {
		bad := receipt
		mutate(&bad)
		log := append([]agentcore.SessionEntry(nil), entries...)
		log[2].Content = string(piModelJSON(bad))
		if _, err := piAnswerMessages(log, json.RawMessage(`{"messages":[]}`)); err == nil {
			t.Fatalf("corrupt delegation receipt accepted: %+v", bad)
		}
	}
	for _, log := range [][]agentcore.SessionEntry{
		{entries[0], entries[2], entries[1]},
		append(append([]agentcore.SessionEntry{}, entries...), entries[2]),
	} {
		if _, err := piAnswerMessages(log, json.RawMessage(`{"messages":[]}`)); err == nil {
			t.Fatal("orphan/repeated delegation continuation accepted")
		}
	}
	if _, err := piAnswerMessages(entries, piModelJSON(map[string]any{"messages": []json.RawMessage{receipt.Message, receipt.Message}})); err == nil {
		t.Fatal("repeated native delegation delivery accepted")
	}
	if _, err := piAnswerMessages(entries, piModelJSON(map[string]any{"messages": []json.RawMessage{receipt.Message, receipt.Contexts[0], receipt.Contexts[0]}})); err == nil {
		t.Fatal("repeated native delegation context accepted")
	}
}

func TestPiAnswerDeliveryRecoversExactMessageAcrossCrashBoundaries(t *testing.T) {
	entries := piQuestionLog(t)
	answer := json.RawMessage(entries[1].Content)
	// Native history copied into a new durable run retains its already delivered
	// answer messages, without copying the previous run's workflow ledger.
	if pending, err := piAnswerMessages(nil, piModelJSON(map[string]any{"messages": []json.RawMessage{answer}})); err != nil || len(pending) != 0 {
		t.Fatalf("historical answer scheduled again: pending=%d err=%v", len(pending), err)
	}
	if entries[1].Kind != agentcore.EntryPiAnswer {
		t.Fatal("native answer used legacy recovery format")
	}
	for _, tc := range []struct {
		name     string
		messages []json.RawMessage
		want     int
	}{
		{"before append", nil, 1},
		{"after message end", []json.RawMessage{answer}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pending, err := piAnswerMessages(entries, piModelJSON(map[string]any{"messages": tc.messages}))
			if err != nil || len(pending) != tc.want {
				t.Fatalf("pending=%d err=%v", len(pending), err)
			}
			if tc.want == 1 && string(pending[0]) != string(answer) {
				t.Fatal("recovery reconstructed the answer instead of using the recorded message")
			}
		})
	}
	for _, tc := range []struct {
		name     string
		entries  []agentcore.SessionEntry
		messages []json.RawMessage
	}{
		{"orphan answer", entries[1:], nil},
		{"repeated metadata", append(append([]agentcore.SessionEntry{}, entries...), entries[1]), nil},
		{"repeated native delivery", entries, []json.RawMessage{answer, answer}},
		{"changed native message", entries, []json.RawMessage{json.RawMessage(strings.Replace(string(answer), "Human choice", "different", 1))}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := piAnswerMessages(tc.entries, piModelJSON(map[string]any{"messages": tc.messages})); err == nil {
				t.Fatal("ambiguous answer recovery accepted")
			}
		})
	}
	// Exercise the actual native log reducer at both sides of message_end.
	// A user message_start alone is not evidence of accepted delivery.
	for _, ended := range []bool{false, true} {
		log := append([]agentcore.SessionEntry{{Kind: agentcore.EntryPiState, Content: `{"messages":[]}`}}, entries...)
		log = append(log, agentcore.SessionEntry{Kind: agentcore.EntryPiEvent, Content: string(piModelJSON(map[string]any{"type": "message_start", "message": answer}))})
		if ended {
			log = append(log, agentcore.SessionEntry{Kind: agentcore.EntryPiEvent, Content: string(piModelJSON(map[string]any{"type": "message_end", "message": answer}))})
		}
		state, err := recoverPiState(log)
		if err != nil {
			t.Fatal(err)
		}
		pending, err := piAnswerMessages(log, state)
		want := 1
		if ended {
			want = 0
		}
		if err != nil || len(pending) != want {
			t.Fatalf("message_end=%v pending=%d err=%v", ended, len(pending), err)
		}
	}
}

func TestPiNativeGoalRecoveryRejectsMalformedAndChangedContracts(t *testing.T) {
	for _, raw := range []string{"null", " null ", "{}", "42"} {
		if _, _, err := piStoredGoal([]agentcore.SessionEntry{{Kind: agentcore.EntryPiGoal, Content: raw}}); err == nil {
			t.Fatalf("invalid goal %q accepted", raw)
		}
	}
	if _, _, err := piStoredGoal([]agentcore.SessionEntry{{Kind: agentcore.EntryPiGoal, Content: `"original"`}, {Kind: agentcore.EntryPiGoal, Content: `"weakened"`}}); err == nil {
		t.Fatal("silent goal revision accepted")
	}
}
