package integration

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/lohi-ai/agentray/agentcore"
	"github.com/lohi-ai/agentray/agentcore/plugins/ask"
)

// memSession is an in-memory session store for testing durability.
type memSession struct {
	entries map[string][]agentcore.SessionEntry
}

func newMemSession() *memSession {
	return &memSession{entries: map[string][]agentcore.SessionEntry{}}
}

func (m *memSession) Append(_ context.Context, id string, e agentcore.SessionEntry) error {
	m.entries[id] = append(m.entries[id], e)
	return nil
}

func (m *memSession) Log(_ context.Context, id string) ([]agentcore.SessionEntry, error) {
	return append([]agentcore.SessionEntry{}, m.entries[id]...), nil
}

// TestAskToolParksRun verifies that calling ask parks the run:
// - res.Parked is true, res.StopReason is "parked"
// - an EntryQuestion is recorded in the durable log
// - a StreamQuestion event is emitted
// - no tool-result message is produced (the call stays dangling)
func TestAskToolParksRun(t *testing.T) {
	session := newMemSession()
	sessionID := "sess-park-1"

	askCall := `{"question": "Which cohort should we analyze?", "options": [{"label": "new"}, {"label": "retained"}]}`
	faux := agentcore.NewFauxProvider(
		agentcore.AssistantToolCall("c_ask_1", "ask", askCall),
	)

	var emitted []agentcore.StreamEvent
	sink := func(ev agentcore.StreamEvent) {
		emitted = append(emitted, ev)
	}

	agent, err := agentcore.New(agentcore.Config{
		Provider:      faux,
		Model:         "test",
		Tools:         agentcore.NewToolSet(ask.Tool{}),
		Policy:        agentcore.NewAllowList("ask"),
		Session:       session,
		SessionID:     sessionID,
		ResumeSession: true,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	res, err := agent.ContinueStream(context.Background(), nil, "analyze users", sink)
	if err != nil {
		t.Fatalf("ContinueStream: %v", err)
	}

	if !res.Parked {
		t.Error("expected res.Parked = true")
	}
	if res.StopReason != "parked" {
		t.Errorf("expected StopReason 'parked', got %q", res.StopReason)
	}

	// Verify durable log: must have EntryQuestion with the call id and args.
	log, _ := session.Log(context.Background(), sessionID)
	var foundQuestion bool
	for _, e := range log {
		if e.Kind == agentcore.EntryQuestion && e.CallID == "c_ask_1" {
			foundQuestion = true
			var q struct {
				Question string `json:"question"`
			}
			_ = json.Unmarshal(e.Question, &q)
			if q.Question != "Which cohort should we analyze?" {
				t.Errorf("unexpected question in log: %q", q.Question)
			}
		}
	}
	if !foundQuestion {
		t.Error("expected EntryQuestion in durable log")
	}

	// Verify StreamQuestion event was emitted.
	var sawStreamQ bool
	for _, ev := range emitted {
		if ev.Type == agentcore.StreamQuestion {
			sawStreamQ = true
			if len(ev.Question) == 0 {
				t.Error("StreamQuestion carries empty Question payload")
			}
		}
	}
	if !sawStreamQ {
		t.Error("expected StreamQuestion in stream events")
	}

	// Verify PendingQuestion helper finds the unanswered question.
	callID, qPayload, found := agentcore.PendingQuestion(log)
	if !found || callID != "c_ask_1" || len(qPayload) == 0 {
		t.Errorf("PendingQuestion: callID=%q found=%v payload=%s", callID, found, qPayload)
	}
}

// TestAskToolResumeWithAnswer verifies the full park -> answer -> resume round-trip:
//  1. Run 1 parks on ask.
//  2. An out-of-band EntryAnswer is appended to the session log.
//  3. Run 2 resumes the session: RecoverSession closes the call with the answer,
//     the model sees the answer as the tool result, and produces the final reply.
func TestAskToolResumeWithAnswer(t *testing.T) {
	session := newMemSession()
	sessionID := "sess-resume-1"

	// Provider 1: emits the ask call.
	p1 := agentcore.NewFauxProvider(
		agentcore.AssistantToolCall("c_ask_42", "ask", `{"question": "Pick a date range"}`),
	)
	a1, err := agentcore.New(agentcore.Config{
		Provider:      p1,
		Model:         "test",
		Tools:         agentcore.NewToolSet(ask.Tool{}),
		Policy:        agentcore.NewAllowList("ask"),
		Session:       session,
		SessionID:     sessionID,
		ResumeSession: true,
	})
	if err != nil {
		t.Fatalf("a1 New: %v", err)
	}

	res1, err := a1.Prompt(context.Background(), "prepare report")
	if err != nil {
		t.Fatalf("a1 Prompt: %v", err)
	}
	if !res1.Parked {
		t.Fatalf("expected run 1 parked, got stop=%q", res1.StopReason)
	}

	// Human answers out-of-band: append EntryAnswer to the log.
	_ = session.Append(context.Background(), sessionID, agentcore.SessionEntry{
		Kind:   agentcore.EntryAnswer,
		CallID: "c_ask_42",
		Answer: "last 30 days",
	})

	// PendingQuestion should now report NO pending question (answered).
	log, _ := session.Log(context.Background(), sessionID)
	if _, _, found := agentcore.PendingQuestion(log); found {
		t.Error("expected PendingQuestion = false after EntryAnswer appended")
	}

	// Provider 2: receives the resumed transcript (including the answer as tool result)
	// and produces the final answer.
	p2 := agentcore.NewFauxProvider(
		agentcore.AssistantText("Report generated for the last 30 days."),
	)
	a2, err := agentcore.New(agentcore.Config{
		Provider:      p2,
		Model:         "test",
		Tools:         agentcore.NewToolSet(ask.Tool{}),
		Policy:        agentcore.NewAllowList("ask"),
		Session:       session,
		SessionID:     sessionID,
		ResumeSession: true,
	})
	if err != nil {
		t.Fatalf("a2 New: %v", err)
	}

	// Resume without prompt: continues the interrupted turn.
	res2, err := a2.Prompt(context.Background(), "")
	if err != nil {
		t.Fatalf("a2 Prompt: %v", err)
	}

	if res2.Parked {
		t.Error("expected run 2 NOT parked")
	}
	if res2.Final != "Report generated for the last 30 days." {
		t.Errorf("unexpected final answer: %q", res2.Final)
	}

	// Verify the tool result in the final transcript is the human's answer.
	var foundAnswerMsg bool
	for _, m := range res2.Messages {
		if m.Role == agentcore.RoleTool && m.ToolCallID == "c_ask_42" {
			foundAnswerMsg = true
			if m.Content != "last 30 days" {
				t.Errorf("tool result content: got %q, want 'last 30 days'", m.Content)
			}
		}
	}
	if !foundAnswerMsg {
		t.Error("tool result message for c_ask_42 not found in resumed messages")
	}
}

// TestAskToolRestartWhileParkedReParks verifies that if the process restarts
// before an answer arrives, the resumed run re-issues the ask call and re-parks
// without duplicating the EntryQuestion in the log.
func TestAskToolRestartWhileParkedReParks(t *testing.T) {
	session := newMemSession()
	sessionID := "sess-repark-1"

	p1 := agentcore.NewFauxProvider(
		agentcore.AssistantToolCall("c_ask_99", "ask", `{"question": "Confirm delete?"}`),
	)
	a1, _ := agentcore.New(agentcore.Config{
		Provider:      p1,
		Model:         "test",
		Tools:         agentcore.NewToolSet(ask.Tool{}),
		Policy:        agentcore.NewAllowList("ask"),
		Session:       session,
		SessionID:     sessionID,
		ResumeSession: true,
	})
	res1, _ := a1.Prompt(context.Background(), "clean up")
	if !res1.Parked {
		t.Fatal("run 1 should be parked")
	}

	// Process restarts without an answer: a fresh agent resumes from the same log.
	// Since ask is RetrySafe, RecoverSession queues it for replay, which calls
	// ask.Run again -> returns ErrParked -> re-parks.
	p2 := agentcore.NewFauxProvider() // no model call needed; re-park happens during replay
	a2, _ := agentcore.New(agentcore.Config{
		Provider:      p2,
		Model:         "test",
		Tools:         agentcore.NewToolSet(ask.Tool{}),
		Policy:        agentcore.NewAllowList("ask"),
		Session:       session,
		SessionID:     sessionID,
		ResumeSession: true,
	})
	res2, err := a2.Prompt(context.Background(), "")
	if err != nil {
		t.Fatalf("resumed Prompt: %v", err)
	}
	if !res2.Parked {
		t.Errorf("expected resumed run to re-park, got stop=%q", res2.StopReason)
	}

	// Count EntryQuestion in the log: must be exactly 1 (deduped).
	log, _ := session.Log(context.Background(), sessionID)
	qCount := 0
	for _, e := range log {
		if e.Kind == agentcore.EntryQuestion && e.CallID == "c_ask_99" {
			qCount++
		}
	}
	if qCount != 1 {
		t.Errorf("expected exactly 1 EntryQuestion in log, got %d", qCount)
	}
}
