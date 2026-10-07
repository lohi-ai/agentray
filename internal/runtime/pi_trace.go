package agentruntime

import (
	"context"
	"encoding/json"
	"github.com/2found/2ai/agentcore"
	"github.com/2found/2ai/telemetry/llm"
)

// bindPiTrace adapts native telemetry to the host's storage/file sinks.
func bindPiTrace(worker NativeAgentConfig, sink llm.Sink, pricingKnown bool, fallbackRunID string) NativeAgentConfig {
	if sink == nil {
		return worker
	}
	previous := worker.OnTrace
	worker.OnTrace = func(ctx context.Context, raw json.RawMessage) {
		if previous != nil {
			func() { defer func() { _ = recover() }(); previous(ctx, append(json.RawMessage(nil), raw...)) }()
		}
		runID := llm.TraceIDFrom(ctx)
		if runID == "" {
			runID = fallbackRunID
		}
		record, err := llm.ParseNative(raw, llm.Metadata{TraceID: runID, SessionKey: agentcore.RunSessionFrom(ctx), Depth: agentcore.DelegationDepth(ctx), PricingKnown: pricingKnown})
		if err != nil {
			return
		}
		sink.Record(record)
	}
	return worker
}
