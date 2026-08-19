package observe

import (
	"bufio"
	"context"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/lohi-ai/agentray/agentcore"
)

// approx reports whether a and b are within floating-point rounding tolerance.
func approx(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

// scripted builds a agentcore.ChatResponse with explicit usage so cost is deterministic.
func scriptedText(content string, in, out int) agentcore.ChatResponse {
	return agentcore.ChatResponse{
		Message:    agentcore.Message{Role: agentcore.RoleAssistant, Content: content},
		StopReason: "stop",
		Usage:      agentcore.Usage{InputTokens: in, OutputTokens: out},
	}
}

func scriptedToolCall(id, name, args string, in, out int) agentcore.ChatResponse {
	return agentcore.ChatResponse{
		Message:    agentcore.Message{Role: agentcore.RoleAssistant, ToolCalls: []agentcore.ToolCall{{ID: id, Name: name, Arguments: args}}},
		StopReason: "tool_calls",
		Usage:      agentcore.Usage{InputTokens: in, OutputTokens: out},
	}
}

func TestPricingCost(t *testing.T) {
	p := Pricing{"gpt-4o": {InputPerM: 2.50, OutputPerM: 10.00}}

	// 1M input + 1M output at the listed price.
	got, priced := p.Cost("gpt-4o", agentcore.Usage{InputTokens: 1_000_000, OutputTokens: 1_000_000})
	if want := 12.50; got != want || !priced {
		t.Fatalf("exact: got %v priced=%v want %v true", got, priced, want)
	}
	// Prefix match: a dated variant resolves to the family price.
	if got, priced := p.Cost("gpt-4o-2024-08-06", agentcore.Usage{InputTokens: 1_000_000}); got != 2.50 || !priced {
		t.Fatalf("prefix: got %v priced=%v want 2.50 true", got, priced)
	}
	// Unknown model prices at zero AND reports itself unpriced — a caller must
	// not mistake this for a genuinely free call.
	if got, priced := p.Cost("mystery-model", agentcore.Usage{InputTokens: 1_000_000}); got != 0 || priced {
		t.Fatalf("unknown: got %v priced=%v want 0 false", got, priced)
	}
}

// collectSink captures records in memory for assertions.
type collectSink struct {
	mu      sync.Mutex
	records []TraceRecord
}

func (c *collectSink) Record(r TraceRecord) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.records = append(c.records, r)
}

// TestSinkFuncAdaptsAFunction covers the closure form of a Sink, which is what
// a caller that only wants "give me the records" uses instead of declaring a
// type. It must reach the same one-method contract MultiSink and FileSink do.
func TestSinkFuncAdaptsAFunction(t *testing.T) {
	var got []TraceRecord
	var s Sink = SinkFunc(func(r TraceRecord) { got = append(got, r) })
	s.Record(TraceRecord{Model: "m-1"})
	if len(got) != 1 || got[0].Model != "m-1" {
		t.Fatalf("SinkFunc did not forward the record: %+v", got)
	}
}

// TestTracingProviderChat verifies a non-streamed call is priced and traced.
func TestTracingProviderChat(t *testing.T) {
	faux := agentcore.NewFauxProvider(scriptedText("hello there", 1000, 500))
	sink := &collectSink{}
	tp := newTracingProvider(faux, Pricing{"gpt-4o": {InputPerM: 2.50, OutputPerM: 10.00}}, sink)

	resp, err := tp.Chat(context.Background(), agentcore.ChatRequest{
		Model:    "gpt-4o",
		Messages: []agentcore.Message{{Role: agentcore.RoleUser, Content: "hi"}},
		Tools:    []agentcore.ToolSchema{{Name: "run_sql"}},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	// 1000/1e6*2.50 + 500/1e6*10.00 = 0.0025 + 0.005 = 0.0075
	if !approx(resp.Usage.CostUSD, 0.0075) {
		t.Fatalf("cost: got %v want 0.0075", resp.Usage.CostUSD)
	}
	if len(sink.records) != 1 {
		t.Fatalf("expected 1 record, got %d", len(sink.records))
	}
	r := sink.records[0]
	if r.Model != "gpt-4o" || r.Response != "hello there" || !approx(r.Usage.CostUSD, 0.0075) {
		t.Fatalf("record mismatch: %+v", r)
	}
	if len(r.Messages) != 1 || r.Messages[0].Content != "hi" {
		t.Fatalf("request messages not captured: %+v", r.Messages)
	}
	if len(r.Tools) != 1 || r.Tools[0] != "run_sql" {
		t.Fatalf("advertised tools not captured: %+v", r.Tools)
	}
}

// TestTracingProviderEndToEnd wraps the faux provider, drives the real agent
// loop (tool turn + final turn), and asserts the trace captured every LLM call,
// the per-call cost, and that the loop summed cost into the run result.
func TestTracingProviderEndToEnd(t *testing.T) {
	tool := &echoTool{name: "run_query"}
	faux := agentcore.NewFauxProvider(
		scriptedToolCall("c1", "run_query", `{"sql":"select 1"}`, 1000, 200),
		scriptedText("the query returned 1", 1500, 300),
	)
	dir := t.TempDir()
	path := filepath.Join(dir, "trace.jsonl")
	file, err := NewFileSink(path)
	if err != nil {
		t.Fatalf("NewFileSink: %v", err)
	}
	mem := &collectSink{}
	sink := MultiSink{file, mem}

	pricing := Pricing{"test-model": {InputPerM: 1.00, OutputPerM: 2.00}}
	agent, err := agentcore.New(agentcore.Config{
		Provider: newTracingProvider(faux, pricing, sink),
		Model:    "test-model",
		Tools:    agentcore.NewToolSet(tool),
		Policy:   agentcore.NewAllowList("run_query"),
	})
	if err != nil {
		t.Fatalf("agentcore.New: %v", err)
	}

	res, err := agent.Prompt(context.Background(), "run a query")
	if err != nil {
		t.Fatalf("agentcore.Prompt: %v", err)
	}
	_ = file.Close()

	// Two LLM calls → two trace records.
	if len(mem.records) != 2 {
		t.Fatalf("expected 2 trace records, got %d", len(mem.records))
	}
	// Call 1: 1000/1e6*1 + 200/1e6*2 = 0.001 + 0.0004 = 0.0014
	// Call 2: 1500/1e6*1 + 300/1e6*2 = 0.0015 + 0.0006 = 0.0021
	if c := mem.records[0].Usage.CostUSD; !approx(c, 0.0014) {
		t.Fatalf("call 1 cost: got %v want 0.0014", c)
	}
	if len(mem.records[0].ToolCalls) != 1 || mem.records[0].ToolCalls[0].Name != "run_query" {
		t.Fatalf("call 1 should record the tool call: %+v", mem.records[0])
	}
	// The loop summed per-call cost into the run result: 0.0014 + 0.0021 = 0.0035.
	if !approx(res.Usage.CostUSD, 0.0035) {
		t.Fatalf("summed cost: got %v want 0.0035", res.Usage.CostUSD)
	}

	// The JSONL file is a durable, parseable trace.
	lines := readJSONL(t, path)
	if len(lines) != 2 {
		t.Fatalf("expected 2 JSONL lines, got %d", len(lines))
	}
	if lines[1].Response != "the query returned 1" {
		t.Fatalf("final record response: %q", lines[1].Response)
	}
}

// TestTracingProviderStream verifies a streamed turn is priced on the Done delta
// and traced once with the assembled response.
func TestTracingProviderStream(t *testing.T) {
	faux := agentcore.NewFauxProvider(scriptedText("streamed answer", 800, 400))
	sink := &collectSink{}
	tp := newTracingProvider(faux, Pricing{"m": {InputPerM: 1.00, OutputPerM: 1.00}}, sink)

	ch, err := tp.Stream(context.Background(), agentcore.ChatRequest{Model: "m", Messages: []agentcore.Message{{Role: agentcore.RoleUser, Content: "go"}}})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	var text string
	var doneCost float64
	for d := range ch {
		text += d.ContentDelta
		if d.Done {
			doneCost = d.Usage.CostUSD
		}
	}
	if text != "streamed answer" {
		t.Fatalf("streamed text: %q", text)
	}
	// (800+400)/1e6 = 0.0012
	if !approx(doneCost, 0.0012) {
		t.Fatalf("done delta cost: got %v want 0.0012", doneCost)
	}
	if len(sink.records) != 1 || !sink.records[0].Streamed || sink.records[0].Response != "streamed answer" {
		t.Fatalf("stream record mismatch: %+v", sink.records)
	}
}

func readJSONL(t *testing.T, path string) []TraceRecord {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open trace: %v", err)
	}
	defer f.Close()
	var out []TraceRecord
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var r TraceRecord
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			t.Fatalf("unmarshal line: %v", err)
		}
		out = append(out, r)
	}
	return out
}

// The composition-level test — Monitor registered as a plugin must decorate
// every rung of the escalation ladder — lives in monitor_rungs_test.go, in
// package observe_test, because it composes through preset and preset imports
// this package.
