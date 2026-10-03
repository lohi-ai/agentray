package agentruntime

import (
	"context"
	"encoding/json"
	"time"

	"github.com/lohi-ai/agentray/agentcore"
	"github.com/lohi-ai/agentray/agentcore/plugins/observe"
)

// bindPiTrace observes the native stream boundary. Display fields serve existing
// inspectors; NativeTrace retains the unprojected request/response and Pi spans.
// It must not supply history, edit a response, or affect completion/accounting.
func bindPiTrace(worker agentcore.PiConfig, sink observe.Sink, pricingKnown bool, fallbackRunID string) agentcore.PiConfig {
	if sink == nil {
		return worker
	}
	previous := worker.OnTrace
	worker.OnTrace = func(ctx context.Context, raw json.RawMessage) {
		if previous != nil {
			func() { defer func() { _ = recover() }(); previous(ctx, append(json.RawMessage{}, raw...)) }()
		}
		var trace struct {
			Attempt                 *struct{ PricingKnown *bool }
			StartedAtMs, DurationMs int64
			Model                   struct{ ID, Provider string }
			Context                 struct {
				SystemPrompt string
				Messages     []json.RawMessage
				Tools        []struct{ Name string }
			}
			Response json.RawMessage
			Error    *struct{ Name, Message string }
		}
		if json.Unmarshal(raw, &trace) != nil {
			return
		}
		known := pricingKnown
		if trace.Attempt != nil && trace.Attempt.PricingKnown != nil {
			known = *trace.Attempt.PricingKnown
		}
		runID := observe.TraceIDFrom(ctx)
		if runID == "" {
			runID = fallbackRunID
		}
		session := agentcore.RunSessionFrom(ctx)
		if session == "" {
			session = runID
		}
		record := observe.TraceRecord{TraceID: runID, SessionKey: session, Depth: agentcore.DelegationDepth(ctx), Timestamp: time.UnixMilli(trace.StartedAtMs), Provider: trace.Model.Provider, Model: trace.Model.ID, LatencyMS: trace.DurationMs, Streamed: true, NativeTrace: append(json.RawMessage{}, raw...)}
		if trace.Context.SystemPrompt != "" {
			record.Messages = append(record.Messages, agentcore.Message{Role: agentcore.RoleSystem, Content: trace.Context.SystemPrompt})
		}
		for _, raw := range trace.Context.Messages {
			message, err := projectPiMessage(raw)
			if err != nil {
				return
			}
			record.Messages = append(record.Messages, message)
		}
		for _, tool := range trace.Context.Tools {
			record.Tools = append(record.Tools, tool.Name)
		}
		if len(trace.Response) > 0 && string(trace.Response) != "null" {
			message, err := projectPiMessage(trace.Response)
			if err != nil {
				return
			}
			record.Response, record.ToolCalls = message.Content, message.ToolCalls
			if message.Usage != nil {
				record.Usage = *message.Usage
				u := record.Usage
				record.Usage.CostUnpriced = u.CostUnpriced || (!known && u.InputTokens+u.OutputTokens+u.CacheReadTokens+u.CacheWriteTokens > 0)
			}
			var terminal struct{ StopReason, ErrorMessage string }
			_ = json.Unmarshal(trace.Response, &terminal)
			record.StopReason, record.Err = terminal.StopReason, terminal.ErrorMessage
		}
		if trace.Error != nil {
			record.Err = trace.Error.Message
		}
		sink.Record(record)
	}
	return worker
}
