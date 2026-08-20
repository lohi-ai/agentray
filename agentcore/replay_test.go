package agentcore

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestReplayProviderEmptyRecording(t *testing.T) {
	p := NewReplayProvider()
	_, err := p.Chat(context.Background(), ChatRequest{})
	if err == nil || !strings.Contains(err.Error(), "empty recording") {
		t.Fatalf("empty recording: %v", err)
	}
}

func TestReplayProviderExtraCall(t *testing.T) {
	p := NewReplayProvider(TurnRecord{
		Messages:   []Message{{Role: RoleUser, Content: "hi"}},
		Response:   "hello",
		StopReason: "stop",
	})
	if _, err := p.Chat(context.Background(), ChatRequest{Messages: []Message{{Role: RoleUser, Content: "hi"}}}); err != nil {
		t.Fatalf("first call: %v", err)
	}
	_, err := p.Chat(context.Background(), ChatRequest{Messages: []Message{{Role: RoleUser, Content: "hi"}}})
	if err == nil || !strings.Contains(err.Error(), "extra call") {
		t.Fatalf("extra call: %v", err)
	}
}

func TestReplayProviderReturnsRecordedError(t *testing.T) {
	p := NewReplayProvider(TurnRecord{
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
		Response: "partial",
		Error:    "provider exploded",
	})
	resp, err := p.Chat(context.Background(), ChatRequest{Messages: []Message{{Role: RoleUser, Content: "hi"}}})
	if err == nil || err.Error() != "provider exploded" {
		t.Fatalf("error: %v", err)
	}
	if resp.Message.Content != "partial" {
		t.Fatalf("recorded response dropped: %q", resp.Message.Content)
	}
}

func TestReplayProviderRejectsDrift(t *testing.T) {
	p := NewReplayProvider(TurnRecord{
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
		Response: "hello",
	})
	_, err := p.Chat(context.Background(), ChatRequest{Messages: []Message{{Role: RoleUser, Content: "hello"}}})
	if err == nil || !strings.Contains(err.Error(), "drifted") {
		t.Fatalf("drift: %v", err)
	}
}

// TestReplayProviderDrivesLoopAgainstTranscript is the intended use: capture a
// real loop's requests via FauxProvider, persist the TurnRecords through JSON
// (the same lossy round-trip agent_llm_calls applies), then drive the real
// loop against ReplayProvider and get the same run.
func TestReplayProviderDrivesLoopAgainstTranscript(t *testing.T) {
	tool := &echoTool{name: "run_query"}
	faux := NewFauxProvider(
		AssistantToolCall("c1", "run_query", `{"sql":"select 1"}`),
		AssistantText("done: the query returned 1"),
	)
	cfg := Config{
		Provider: faux,
		Model:    "test",
		Tools:    NewToolSet(tool),
		Policy:   NewAllowList("run_query"),
		Retry:    &RetryPolicy{MaxAttempts: 1},
	}
	agent, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	res, err := agent.Prompt(context.Background(), "run a query")
	if err != nil {
		t.Fatalf("record run: %v", err)
	}

	records := turnRecordsFromFaux(t, faux)
	raw, err := json.Marshal(records)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := json.Unmarshal(raw, &records); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	tool2 := &echoTool{name: "run_query"}
	cfg.Provider = NewReplayProvider(records...)
	cfg.Tools = NewToolSet(tool2)
	agent2, err := New(cfg)
	if err != nil {
		t.Fatalf("New replay: %v", err)
	}
	res2, err := agent2.Prompt(context.Background(), "run a query")
	if err != nil {
		t.Fatalf("replay run: %v", err)
	}
	if res2.Final != res.Final {
		t.Fatalf("final got %q want %q", res2.Final, res.Final)
	}
	if tool2.called != 1 {
		t.Fatalf("tool called %d, want 1", tool2.called)
	}
	if len(res2.Tools) != len(res.Tools) {
		t.Fatalf("tool traces %d vs %d", len(res2.Tools), len(res.Tools))
	}
}

func turnRecordsFromFaux(t *testing.T, f *FauxProvider) []TurnRecord {
	t.Helper()
	n := len(f.Recorded)
	if n > len(f.Responses) {
		n = len(f.Responses)
	}
	if n == 0 {
		t.Fatal("faux provider recorded no calls")
	}
	out := make([]TurnRecord, 0, n)
	for i := range n {
		req := f.Recorded[i]
		resp := f.Responses[i]
		out = append(out, TurnRecord{
			Messages:   req.Messages,
			Response:   resp.Message.Content,
			ToolCalls:  resp.Message.ToolCalls,
			Tools:      advertisedToolNames(req.Tools),
			StopReason: resp.StopReason,
			TokensIn:   resp.Usage.InputTokens,
			TokensOut:  resp.Usage.OutputTokens,
			CostUSD:    resp.Usage.CostUSD,
		})
	}
	return out
}
