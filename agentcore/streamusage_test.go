package agentcore

import (
	"context"
	"testing"
)

// Usage on a streamed turn, when the provider does not hand it over all at once.
//
// Both providers in this module happen to accumulate internally and report
// everything on the Done delta, which is why a turn that silently billed zero
// went unnoticed for so long: the only way to see it is a provider that reports
// the way the wire actually does. Anthropic states input tokens on
// message_start, before any output exists; OpenAI emits a usage-only chunk
// AFTER the chunk carrying finish_reason. A loop that reads usage off Done alone
// scores the first as zero-output and the second as zero-everything, and the
// only visible symptom is a budget gate that never trips.

// pieceProvider streams one fixed answer with usage split across deltas.
type pieceProvider struct{ deltas []ChatDelta }

func (*pieceProvider) Name() string        { return "pieces" }
func (*pieceProvider) SupportsTools() bool { return true }

func (*pieceProvider) Chat(context.Context, ChatRequest) (ChatResponse, error) {
	return ChatResponse{}, nil
}

func (p *pieceProvider) Stream(context.Context, ChatRequest) (<-chan ChatDelta, error) {
	ch := make(chan ChatDelta, len(p.deltas))
	for _, d := range p.deltas {
		ch <- d
	}
	close(ch)
	return ch, nil
}

func streamOnce(t *testing.T, deltas ...ChatDelta) ChatResponse {
	t.Helper()
	a := &Agent{}
	resp, err := a.streamTurn(context.Background(), &pieceProvider{deltas: deltas},
		ChatRequest{}, func(StreamEvent) {})
	if err != nil {
		t.Fatalf("streamTurn: %v", err)
	}
	return resp
}

// The Anthropic shape: input tokens arrive first, output tokens only at the end.
func TestStreamedUsageSurvivesAnEarlyReport(t *testing.T) {
	resp := streamOnce(t,
		ChatDelta{Usage: Usage{InputTokens: 1200}},
		ChatDelta{ContentDelta: "hello"},
		ChatDelta{Done: true, StopReason: "stop", Usage: Usage{OutputTokens: 40}},
	)
	if resp.Usage.InputTokens != 1200 {
		t.Fatalf("input tokens reported before the first output token were dropped: got %d, want 1200. "+
			"The turn is then billed as if its prompt were free, and the prompt is the expensive half",
			resp.Usage.InputTokens)
	}
	if resp.Usage.OutputTokens != 40 {
		t.Fatalf("output tokens = %d, want 40", resp.Usage.OutputTokens)
	}
	if resp.StopReason != "stop" {
		t.Fatalf("stop reason = %q, want %q", resp.StopReason, "stop")
	}
	if resp.Message.Content != "hello" {
		t.Fatalf("content = %q, want %q", resp.Message.Content, "hello")
	}
}

// The OpenAI shape: the terminal chunk carries no usage at all, and a usage-only
// chunk follows it.
func TestStreamedUsageSurvivesAReportAfterDone(t *testing.T) {
	resp := streamOnce(t,
		ChatDelta{ContentDelta: "hi"},
		ChatDelta{Done: true, StopReason: "stop"},
		ChatDelta{Usage: Usage{InputTokens: 900, OutputTokens: 12, CacheReadTokens: 800}},
	)
	want := Usage{InputTokens: 900, OutputTokens: 12, CacheReadTokens: 800}
	if resp.Usage != want {
		t.Fatalf("usage = %+v, want %+v: a usage-only chunk after the terminal one is how OpenAI "+
			"reports with stream_options.include_usage, so ignoring it bills the whole turn at zero",
			resp.Usage, want)
	}
	if resp.StopReason != "stop" {
		t.Fatalf("a later delta clobbered the terminal stop reason: got %q", resp.StopReason)
	}
}

// Reports within a turn are running totals, not increments, so restating a field
// must not double it. This is the reason the merge overwrites rather than adds.
func TestStreamedUsageIsNotDoubleCountedWhenRestated(t *testing.T) {
	resp := streamOnce(t,
		ChatDelta{Usage: Usage{InputTokens: 500}},
		ChatDelta{ContentDelta: "x"},
		ChatDelta{Usage: Usage{InputTokens: 500, OutputTokens: 7}},
		ChatDelta{Done: true, StopReason: "stop", Usage: Usage{InputTokens: 500, OutputTokens: 9}},
	)
	if resp.Usage.InputTokens != 500 {
		t.Fatalf("input tokens = %d, want 500: a restated running total was summed instead of "+
			"replaced, which over-bills every turn a provider reports more than once",
			resp.Usage.InputTokens)
	}
	if resp.Usage.OutputTokens != 9 {
		t.Fatalf("output tokens = %d, want 9 (the newest total)", resp.Usage.OutputTokens)
	}
}

// A provider that reports once, on Done — the case both of this module's
// providers produce — must be unaffected by any of the above.
func TestStreamedUsageOnDoneAloneStillWorks(t *testing.T) {
	want := Usage{InputTokens: 10, OutputTokens: 20, CacheWriteTokens: 5, CostUSD: 0.25}
	resp := streamOnce(t,
		ChatDelta{ContentDelta: "ok"},
		ChatDelta{Done: true, StopReason: "stop", Usage: want},
	)
	if resp.Usage != want {
		t.Fatalf("usage = %+v, want %+v", resp.Usage, want)
	}
}
