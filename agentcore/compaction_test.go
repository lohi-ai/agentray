package agentcore

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// bigText returns a string of n bytes so token estimates cross thresholds.
func bigText(n int) string { return strings.Repeat("x", n) }

// scriptedSummaryProvider returns a fixed summary for the summarization call.
type scriptedSummaryProvider struct {
	summary string
	err     error
	calls   int
}

func (p *scriptedSummaryProvider) Name() string        { return "summary" }
func (p *scriptedSummaryProvider) SupportsTools() bool { return false }
func (p *scriptedSummaryProvider) Stream(context.Context, ChatRequest) (<-chan ChatDelta, error) {
	return nil, errors.New("no stream")
}
func (p *scriptedSummaryProvider) Chat(_ context.Context, _ ChatRequest) (ChatResponse, error) {
	p.calls++
	if p.err != nil {
		return ChatResponse{}, p.err
	}
	return ChatResponse{Message: Message{Role: RoleAssistant, Content: p.summary}, StopReason: "stop"}, nil
}

// longTranscript builds [system, then many user/assistant/tool turns] whose
// older portion exceeds keepRecent so a cut point exists.
func longTranscript() []Message {
	msgs := []Message{{Role: RoleSystem, Content: "you are an agent"}}
	for i := 0; i < 8; i++ {
		msgs = append(msgs,
			Message{Role: RoleUser, Content: "question " + bigText(2000)},
			Message{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "c", Name: "q", Arguments: "{}"}}},
			Message{Role: RoleTool, Name: "q", Content: "result " + bigText(2000)},
			Message{Role: RoleAssistant, Content: "answer " + bigText(500)},
		)
	}
	return msgs
}

func TestCompactWithSummary_ReplacesOlderSpan(t *testing.T) {
	prov := &scriptedSummaryProvider{summary: "## Goal\nfinish the task\n## Next Steps\n1. continue"}
	msgs := longTranscript()
	out := compactWithSummary(context.Background(), prov, "m", msgs, CompactionSettings{KeepRecentTokens: 3000})

	if prov.calls != 1 {
		t.Fatalf("expected 1 summarization call, got %d", prov.calls)
	}
	if len(out) >= len(msgs) {
		t.Fatalf("compaction should shrink transcript: before=%d after=%d", len(msgs), len(out))
	}
	// Leading real system prompt preserved first.
	if out[0].Role != RoleSystem || out[0].Content != "you are an agent" {
		t.Fatalf("system header not preserved: %+v", out[0])
	}
	// A summary message with the marker is present.
	var sawSummary bool
	for _, m := range out {
		if m.Role == RoleSystem && strings.HasPrefix(m.Content, summaryMarker) {
			sawSummary = true
		}
	}
	if !sawSummary {
		t.Fatalf("expected a summary message, got %+v", out)
	}
	// Retained tail must not begin on a tool-result (would break the provider).
	for i, m := range out {
		if m.Role == RoleSystem && strings.HasPrefix(m.Content, summaryMarker) {
			if i+1 < len(out) && out[i+1].Role == RoleTool {
				t.Fatalf("tail begins on a tool-result message")
			}
		}
	}
}

// capturingSummaryProvider records the last summarization request so a test can
// assert how the prompt was built (fresh vs iterative update).
type capturingSummaryProvider struct {
	summary string
	lastReq ChatRequest
	calls   int
}

func (p *capturingSummaryProvider) Name() string        { return "capture" }
func (p *capturingSummaryProvider) SupportsTools() bool { return false }
func (p *capturingSummaryProvider) Stream(context.Context, ChatRequest) (<-chan ChatDelta, error) {
	return nil, errors.New("no stream")
}
func (p *capturingSummaryProvider) Chat(_ context.Context, req ChatRequest) (ChatResponse, error) {
	p.calls++
	p.lastReq = req
	return ChatResponse{Message: Message{Role: RoleAssistant, Content: p.summary}, StopReason: "stop"}, nil
}

// TestCompactWithSummary_IterativeUpdate verifies that when the body already
// begins with a prior summary (a second+ compaction), the older span is folded
// into that summary via the UPDATE prompt instead of being re-summarized raw.
func TestCompactWithSummary_IterativeUpdate(t *testing.T) {
	prov := &capturingSummaryProvider{summary: "## Goal\nupdated goal\n## Next Steps\n1. go"}

	// Transcript: real system header, a PRIOR summary, then many fresh turns.
	msgs := []Message{
		{Role: RoleSystem, Content: "you are an agent"},
		{Role: RoleSystem, Content: summaryMarker + "\nPRIOR-SUMMARY-SENTINEL: earlier facts"},
	}
	for i := 0; i < 8; i++ {
		msgs = append(msgs,
			Message{Role: RoleUser, Content: "fresh question " + bigText(2000)},
			Message{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "c", Name: "q", Arguments: "{}"}}},
			Message{Role: RoleTool, Name: "q", Content: "result " + bigText(2000)},
			Message{Role: RoleAssistant, Content: "answer " + bigText(500)},
		)
	}

	out := compactWithSummary(context.Background(), prov, "m", msgs, CompactionSettings{KeepRecentTokens: 3000})

	if prov.calls != 1 {
		t.Fatalf("expected 1 summarization call, got %d", prov.calls)
	}
	user := prov.lastReq.Messages[len(prov.lastReq.Messages)-1].Content
	// The prior summary must be handed back to the model as the base to update,
	// and the UPDATE prompt (not the from-scratch prompt) must be used.
	if !strings.Contains(user, "PRIOR-SUMMARY-SENTINEL") {
		t.Fatalf("update prompt must carry the previous summary; got:\n%s", user)
	}
	if !strings.Contains(user, "Previous summary") || !strings.Contains(user, "New messages since that summary") {
		t.Fatalf("expected iterative-update framing; got:\n%s", user)
	}
	// Exactly one summary marker survives (the new one replaces the prior).
	markers := 0
	for _, m := range out {
		if m.Role == RoleSystem && strings.HasPrefix(m.Content, summaryMarker) {
			markers++
		}
	}
	if markers != 1 {
		t.Fatalf("expected exactly one summary marker after compaction, got %d", markers)
	}
}

// TestSplitPriorSummary covers the head-detection helper directly.
func TestSplitPriorSummary(t *testing.T) {
	prev, rest := splitPriorSummary([]Message{
		{Role: RoleSystem, Content: summaryMarker + "\nkept facts"},
		{Role: RoleUser, Content: "new"},
	})
	if prev != "kept facts" || len(rest) != 1 || rest[0].Role != RoleUser {
		t.Fatalf("split failed: prev=%q rest=%+v", prev, rest)
	}
	// No prior summary -> empty prev, original slice returned.
	prev2, rest2 := splitPriorSummary([]Message{{Role: RoleUser, Content: "hi"}})
	if prev2 != "" || len(rest2) != 1 {
		t.Fatalf("expected passthrough, got prev=%q rest=%+v", prev2, rest2)
	}
}

func TestCompactWithSummary_FallsBackOnError(t *testing.T) {
	prov := &scriptedSummaryProvider{err: errors.New("boom")}
	msgs := longTranscript()
	out := compactWithSummary(context.Background(), prov, "m", msgs, CompactionSettings{KeepRecentTokens: 3000})
	// Falls back to deterministic elide: no summary marker, still shrinks/holds.
	for _, m := range out {
		if strings.HasPrefix(m.Content, summaryMarker) {
			t.Fatalf("error path must not produce a model summary")
		}
	}
	if len(out) == 0 {
		t.Fatalf("fallback produced empty transcript")
	}
}

func TestEstimateContextTokens_PrefersUsage(t *testing.T) {
	// No usage anywhere -> byte heuristic over the whole transcript.
	plain := []Message{
		{Role: RoleUser, Content: string(make([]byte, 400))},
	}
	if got := estimateContextTokens(plain); got != 100 {
		t.Fatalf("byte fallback: want 100, got %d", got)
	}

	// Assistant carries usage -> use input+output + trailing byte estimate.
	withUsage := []Message{
		{Role: RoleUser, Content: "hi"},
		{Role: RoleAssistant, Content: "ok", Usage: &Usage{InputTokens: 5000, OutputTokens: 200}},
		{Role: RoleTool, Content: string(make([]byte, 400))}, // trailing 400 bytes ≈ 100 tokens
	}
	if got := estimateContextTokens(withUsage); got != 5300 {
		t.Fatalf("usage-based: want 5300, got %d", got)
	}
}

func TestEstimateContextTokens_UsesLatestUsage(t *testing.T) {
	msgs := []Message{
		{Role: RoleAssistant, Content: "a", Usage: &Usage{InputTokens: 1000}},
		{Role: RoleUser, Content: "more"},
		{Role: RoleAssistant, Content: "b", Usage: &Usage{InputTokens: 8000, OutputTokens: 100}},
	}
	// Latest usage wins; nothing trails it.
	if got := estimateContextTokens(msgs); got != 8100 {
		t.Fatalf("want 8100, got %d", got)
	}
}
