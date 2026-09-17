package agentruntime

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/lohi-ai/agentray/agentcore"
	"github.com/lohi-ai/agentray/agentcore/plugins/ask"
)

func TestAskToolTriggerGating(t *testing.T) {
	// Gating rule in runner.execute:
	// trigger == "chat" && opts.SessionID != "" && !opts.ReadOnly
	cases := []struct {
		name      string
		trigger   string
		sessionID string
		readOnly  bool
		wantAsk   bool
	}{
		{"chat with session", "chat", "sess-1", false, true},
		{"chat without session", "chat", "", false, false},
		{"chat read-only", "chat", "sess-1", true, false},
		{"scheduled trigger", "scheduled", "sess-1", false, false},
		{"webhook trigger", "webhook", "sess-1", false, false},
		{"delegate trigger", "delegate", "sess-1", false, false},
		{"manual trigger", "manual", "sess-1", false, false},
		{"default empty trigger", "", "sess-1", false, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			trigger := tc.trigger
			if trigger == "" {
				trigger = "manual"
			}
			var tools []agentcore.Tool
			if trigger == "chat" && tc.sessionID != "" && !tc.readOnly {
				tools = append(tools, ask.Tool{})
			}
			hasAsk := false
			for _, tool := range tools {
				if tool.Name() == "ask" {
					hasAsk = true
				}
			}
			if hasAsk != tc.wantAsk {
				t.Errorf("trigger=%q sessionID=%q readOnly=%v: got hasAsk=%v, want %v",
					tc.trigger, tc.sessionID, tc.readOnly, hasAsk, tc.wantAsk)
			}
		})
	}
}

func TestChatResultWaitingFields(t *testing.T) {
	qJSON := json.RawMessage(`{"question":"Which tier?","options":[{"label":"pro"}]}`)
	res := ChatResult{
		RunID:    "run-123",
		Waiting:  true,
		Question: qJSON,
	}

	b, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}

	var parsed struct {
		RunID    string          `json:"run_id"`
		Waiting  bool            `json:"waiting"`
		Question json.RawMessage `json:"question"`
	}
	if err := json.Unmarshal(b, &parsed); err != nil {
		t.Fatal(err)
	}

	if !parsed.Waiting {
		t.Error("expected waiting=true in serialized JSON")
	}
	if string(parsed.Question) != string(qJSON) {
		t.Errorf("question JSON mismatch: got %s, want %s", parsed.Question, qJSON)
	}
}

func TestAnswerQuestionMissingStore(t *testing.T) {
	svc := &ChatService{}
	_, err := svc.AnswerQuestion(context.Background(), AnswerOptions{}, nil)
	if err == nil {
		t.Fatal("expected error with nil runner/store")
	}
}
