package observe

import (
	"context"
	"encoding/json"
	"os"
	"sync"
	"time"

	"github.com/lohi-ai/agentray/agentcore"
)

// traceCtxKey carries an opaque correlation id down the context into the
// provider. agentcore stays generic — it never learns what the id *means* (the
// consumer maps it run → agent); it only stamps it onto every emitted record so
// a trace stream can be attributed after the fact.
type traceCtxKey struct{}

// WithTraceID tags ctx with a correlation id the tracingProvider stamps onto
// every TraceRecord produced under it. The consumer (e.g. the Runner) sets it to
// its run id just before driving the loop; an empty id is a no-op.
func WithTraceID(ctx context.Context, id string) context.Context {
	if id == "" {
		return ctx
	}
	return context.WithValue(ctx, traceCtxKey{}, id)
}

// traceIDFrom reads the correlation id set by WithTraceID ("" when absent, e.g.
// a classifier call made outside any run).
func traceIDFrom(ctx context.Context) string {
	if v, ok := ctx.Value(traceCtxKey{}).(string); ok {
		return v
	}
	return ""
}

// TraceRecord is one observed LLM call: what was sent, what came back, what it
// cost, and how long it took. It is the durable, debuggable unit behind an agent
// run — the "message sent to the LLM + est. fee" trace. Emitted once per Chat or
// streamed turn.
type TraceRecord struct {
	TraceID string `json:"trace_id,omitempty"` // correlation id (the run id), set via WithTraceID
	// SessionKey identifies WHICH agent made the call — the run's own session,
	// or a spawned child's derived session. A parent and its children share one
	// provider and one ctx chain, so without this their calls are one
	// undifferentiated stream. Empty outside a durable run.
	SessionKey string `json:"session_key,omitempty"`
	// Depth is the delegation depth of the caller: 0 for the top-level run.
	Depth      int                  `json:"depth,omitempty"`
	Timestamp  time.Time            `json:"timestamp"`
	Provider   string               `json:"provider"`
	Model      string               `json:"model"`
	Messages   []agentcore.Message  `json:"messages"`             // the request sent to the model
	Tools      []string             `json:"tools,omitempty"`      // tool names advertised this turn
	Response   string               `json:"response,omitempty"`   // assistant text returned
	ToolCalls  []agentcore.ToolCall `json:"tool_calls,omitempty"` // tool calls the model requested
	StopReason string               `json:"stop_reason,omitempty"`
	Usage      agentcore.Usage      `json:"usage"` // tokens + computed CostUSD
	LatencyMS  int64                `json:"latency_ms"`
	Streamed   bool                 `json:"streamed"`
	Err        string               `json:"error,omitempty"`
}

// Sink receives a TraceRecord per LLM call. Implementations fan out to a
// file, stdout, or an analytics store. A nil sink disables emission — cost is
// still computed (so res.Usage.CostUSD is always honest), only the trace is
// dropped.
type Sink interface {
	Record(TraceRecord)
}

// SinkFunc adapts a function to a Sink.
type SinkFunc func(TraceRecord)

func (f SinkFunc) Record(r TraceRecord) { f(r) }

// FileSink appends one JSON object per line (JSONL) to an open file. Safe
// for concurrent runs. Close it when the process shuts down.
type FileSink struct {
	mu  sync.Mutex
	w   *os.File
	enc *json.Encoder
}

// NewFileSink opens (creating/appending) a JSONL trace file at path.
func NewFileSink(path string) (*FileSink, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	return &FileSink{w: f, enc: json.NewEncoder(f)}, nil
}

// Record writes one JSONL line. Encoding/IO errors are dropped — tracing must
// never break a run.
func (s *FileSink) Record(r TraceRecord) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_ = s.enc.Encode(r)
}

// Close releases the underlying file.
func (s *FileSink) Close() error { return s.w.Close() }

// MultiSink fans one record out to several sinks (e.g. file + stdout).
type MultiSink []Sink

func (m MultiSink) Record(r TraceRecord) {
	for _, s := range m {
		if s != nil {
			s.Record(r)
		}
	}
}

// tracingProvider decorates any agentcore.LLMProvider with two cross-cutting concerns the
// real providers shouldn't each reimplement: pricing (fill agentcore.Usage.CostUSD from a
// price table) and tracing (emit a TraceRecord per call). It is transparent —
// the loop, hooks, and escalation are unchanged — so wrapping is the single
// place cost is computed for every model, real or faux.
type tracingProvider struct {
	inner   agentcore.LLMProvider
	pricing Pricing
	sink    Sink
}

// newTracingProvider wraps inner. pricing fills CostUSD when the provider didn't
// already report it (a nil/empty table leaves cost at 0). sink may be nil (cost
// still computed, no trace emitted).
func newTracingProvider(inner agentcore.LLMProvider, pricing Pricing, sink Sink) *tracingProvider {
	return &tracingProvider{inner: inner, pricing: pricing, sink: sink}
}

func (t *tracingProvider) Name() string        { return t.inner.Name() }
func (t *tracingProvider) SupportsTools() bool { return t.inner.SupportsTools() }

// UpdateAPIKey forwards key rotation to the inner provider when it supports it,
// so wrapping doesn't break long-run BYO-key refresh (the loop type-asserts the
// provider for agentcore.KeyUpdater).
func (t *tracingProvider) UpdateAPIKey(key string) {
	if u, ok := t.inner.(agentcore.KeyUpdater); ok {
		u.UpdateAPIKey(key)
	}
}

// Chat runs the call, prices it, and emits one trace record.
func (t *tracingProvider) Chat(ctx context.Context, req agentcore.ChatRequest) (agentcore.ChatResponse, error) {
	start := time.Now()
	resp, err := t.inner.Chat(ctx, req)
	t.price(req.Model, &resp.Usage)
	t.emit(ctx, req, resp, time.Since(start), false, err)
	return resp, err
}

// Stream wraps the delta channel: it prices the terminal Done delta and emits a
// trace record assembled from the full streamed turn.
func (t *tracingProvider) Stream(ctx context.Context, req agentcore.ChatRequest) (<-chan agentcore.ChatDelta, error) {
	in, err := t.inner.Stream(ctx, req)
	if err != nil {
		return nil, err
	}
	out := make(chan agentcore.ChatDelta)
	go func() {
		defer close(out)
		start := time.Now()
		var assembled agentcore.ChatResponse
		msg := agentcore.Message{Role: agentcore.RoleAssistant}
		var streamErr error
		for d := range in {
			if d.Err != nil {
				streamErr = d.Err
			}
			msg.Content += d.ContentDelta
			if d.ToolCall != nil {
				msg.ToolCalls = append(msg.ToolCalls, *d.ToolCall)
			}
			if d.Done {
				t.price(req.Model, &d.Usage)
				assembled.Usage = d.Usage
				assembled.StopReason = d.StopReason
			}
			out <- d
		}
		assembled.Message = msg
		t.emit(ctx, req, assembled, time.Since(start), true, streamErr)
	}()
	return out, nil
}

// price fills CostUSD from the table only when the provider didn't already
// report a cost, so a vendor that returns native pricing wins.
func (t *tracingProvider) price(model string, u *agentcore.Usage) {
	if u.CostUSD == 0 {
		u.CostUSD = t.pricing.Cost(model, *u)
	}
}

func (t *tracingProvider) emit(ctx context.Context, req agentcore.ChatRequest, resp agentcore.ChatResponse, dur time.Duration, streamed bool, err error) {
	if t.sink == nil {
		return
	}
	rec := TraceRecord{
		TraceID:    traceIDFrom(ctx),
		SessionKey: agentcore.RunSessionFrom(ctx),
		Depth:      agentcore.DelegationDepth(ctx),
		Timestamp:  start(dur),
		Provider:   t.inner.Name(),
		Model:      req.Model,
		Messages:   req.Messages,
		Tools:      toolNames(req.Tools),
		Response:   resp.Message.Content,
		ToolCalls:  resp.Message.ToolCalls,
		StopReason: resp.StopReason,
		Usage:      resp.Usage,
		LatencyMS:  dur.Milliseconds(),
		Streamed:   streamed,
	}
	if err != nil {
		rec.Err = err.Error()
	}
	t.sink.Record(rec)
}

// start back-dates the record timestamp to call start (now - duration), so the
// record reflects when the call began.
func start(dur time.Duration) time.Time { return time.Now().Add(-dur) }

func toolNames(tools []agentcore.ToolSchema) []string {
	if len(tools) == 0 {
		return nil
	}
	out := make([]string, len(tools))
	for i, t := range tools {
		out[i] = t.Name
	}
	return out
}
