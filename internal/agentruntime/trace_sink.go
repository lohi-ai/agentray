package agentruntime

import (
	"context"
	"encoding/json"

	"github.com/lohi-ai/agentray/agentcore"
	"github.com/lohi-ai/agentray/internal/storage"
)

// storeTraceSink persists every per-LLM-call TraceRecord to agent_llm_calls,
// keyed by the run id the TracingProvider stamped on the record (the trace id
// the Runner set via agentcore.WithTraceID). It is the DB-backed half of the
// trace fan-out — the queryable source for the monitoring console — and lives
// here, in the consumer, because it is the one place that may import both
// agentcore and storage (storage never imports agentcore).
//
// Records with no trace id (e.g. classifier calls made outside any run) have no
// run to attach to and are dropped — fail-safe, never an orphan row. All writes
// are best-effort: tracing must never break a run, so the storage error is
// swallowed, mirroring FileTraceSink.
type storeTraceSink struct {
	store *storage.Store
}

// NewStoreTraceSink returns a TraceSink that writes LLM-call traces to Postgres.
func NewStoreTraceSink(store *storage.Store) agentcore.TraceSink {
	return &storeTraceSink{store: store}
}

func (s *storeTraceSink) Record(r agentcore.TraceRecord) {
	if r.TraceID == "" {
		return // no run correlation → nothing to attach the trace to
	}
	msgs, _ := json.Marshal(r.Messages)
	calls, _ := json.Marshal(r.ToolCalls)
	_ = s.store.RecordAgentLLMCall(context.Background(), storage.AgentLLMCall{
		RunID:         r.TraceID,
		Provider:      r.Provider,
		Model:         r.Model,
		MessagesJSON:  string(msgs),
		Tools:         r.Tools,
		Response:      r.Response,
		ToolCallsJSON: string(calls),
		StopReason:    r.StopReason,
		TokenInput:    r.Usage.InputTokens,
		TokenOutput:   r.Usage.OutputTokens,
		CostUSD:       r.Usage.CostUSD,
		LatencyMS:     int(r.LatencyMS),
		Streamed:      r.Streamed,
		Error:         r.Err,
	})
}
