package agentruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/lohi-ai/agentray/agentcore"
)

// PiRunConfig executes a native run. Seed history belongs in the native
// initialState.messages option; Input is a native prompt, or nil to Continue.
type PiRunConfig struct {
	Compaction *PiContextCompaction
	Session    PiSessionConfig
	Input      json.RawMessage
	Sink       agentcore.StreamSink
	// Host connects composed prompt, step, batch, and stop policies. The caller
	// owns it and closes it after RunPi returns. Task is used for memory recall.
	Host *agentcore.PiToolHost
	Task string
	// PricingKnown distinguishes an intentionally free model from an unknown
	// model whose native cost metadata was supplied as zeros.
	PricingKnown bool
}

// PiRunResult keeps the authoritative native state beside a display/accounting
// projection. Projection.Messages must never be used to reconstruct Pi history:
// native signatures, extensions, and custom message fields remain in State.
type PiRunResult struct {
	Revision   string
	State      json.RawMessage
	Telemetry  json.RawMessage
	Projection agentcore.RunResult
}

// RunPi drives the original Pi loop with server session policy and supplies the
// server's existing streaming/result vocabulary. Native events and state remain
// unchanged in the durable log and in the caller's original OnEvent callback.
func RunPi(ctx context.Context, cfg PiRunConfig) (result PiRunResult, err error) {
	if cfg.Host != nil {
		defer func() { result.Projection = cfg.Host.CompletePiRun(context.WithoutCancel(ctx), result.Projection) }()
	}
	var projection piRunProjection
	projection.sink, projection.pricingKnown = cfg.Sink, cfg.PricingKnown
	projection.calls = map[string]agentcore.ToolTrace{}
	var lifecycle *piHostLifecycle
	if cfg.Host != nil {
		lifecycle, err = bindPiHostLifecycle(ctx, &cfg, &projection)
		if err != nil {
			return result, err
		}
	}
	prepareCompaction, err := bindPiRequestCompaction(&cfg, &projection)
	if err != nil {
		return result, err
	}
	original := cfg.Session.Pi.OnEvent
	cfg.Session.Pi.OnEvent = func(eventCtx context.Context, event json.RawMessage) error {
		if err := projection.event(event); err != nil {
			return err
		}
		if original != nil {
			return original(eventCtx, event)
		}
		return nil
	}
	// Startup follows caller cancellation. Once initialized, prompt cancellation
	// uses Pi's abort protocol, leaving the process and lease alive long enough
	// to settle native terminal events and take a final snapshot.
	life, cancel := context.WithCancel(context.WithoutCancel(ctx))
	defer cancel()
	stopStartup := context.AfterFunc(ctx, cancel)
	session, err := NewPiSession(life, cfg.Session)
	if err != nil {
		stopStartup()
		return PiRunResult{}, err
	}
	if err := prepareCompaction(session); err != nil {
		stopStartup()
		_ = session.Close()
		return result, err
	}
	stopStartup()
	defer func() { err = errors.Join(err, session.Close()) }()
	question, err := session.PendingQuestion(ctx)
	if err != nil {
		return result, err
	}
	if len(question) > 0 {
		projection.result.Parked, projection.result.Question, projection.result.StopReason = true, question, "parked"
		projection.emit(agentcore.StreamEvent{Type: agentcore.StreamQuestion, Question: question})
		if lifecycle != nil {
			lifecycle.parked, lifecycle.stopReason = true, "parked"
		}
	} else if lifecycle != nil {
		if err := lifecycle.prepareFirst(ctx, session); err != nil {
			return result, err
		}
	}
	if len(question) > 0 {
		// Reopening a parked run is not a model turn. Keep the current native
		// snapshot and surface the same question until the human answers.
	} else if len(cfg.Input) == 0 {
		err = session.Continue(ctx)
	} else {
		err = session.Prompt(ctx, cfg.Input)
	}
	finishCtx, finishCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer finishCancel()
	if _, idleErr := session.agent.Call(finishCtx, "waitForIdle", nil); idleErr != nil {
		err = errors.Join(err, idleErr)
	}
	state, stateErr := session.State(finishCtx)
	telemetry, telemetryErr := session.agent.Call(finishCtx, "telemetry", nil)
	err = errors.Join(err, stateErr, telemetryErr)
	projection.mu.Lock()
	defer projection.mu.Unlock()
	result = PiRunResult{Revision: session.agent.UpstreamCommit(), State: state, Telemetry: telemetry, Projection: projection.result}
	if lifecycle != nil {
		lifecycle.mu.Lock()
		result.Projection.Parked = lifecycle.parked
		if lifecycle.stopReason != "" && result.Projection.StopReason != "error" && result.Projection.StopReason != "aborted" {
			result.Projection.StopReason = lifecycle.stopReason
		}
		lifecycle.mu.Unlock()
	}
	if stateErr == nil {
		var native struct {
			Messages     []json.RawMessage
			ErrorMessage string
		}
		if decodeErr := json.Unmarshal(state, &native); decodeErr != nil {
			return result, errors.Join(err, decodeErr)
		}
		result.Projection.Messages = make([]agentcore.Message, 0, len(native.Messages))
		for _, raw := range native.Messages {
			message, decodeErr := projectPiMessage(raw)
			if decodeErr != nil {
				return result, errors.Join(err, decodeErr)
			}
			result.Projection.Messages = append(result.Projection.Messages, message)
		}
		if native.ErrorMessage != "" {
			err = errors.Join(err, fmt.Errorf("Pi run: %s", native.ErrorMessage))
		}
	}
	return result, err
}

type piRunProjection struct {
	mu           sync.Mutex
	sink         agentcore.StreamSink
	pricingKnown bool
	result       agentcore.RunResult
	calls        map[string]agentcore.ToolTrace
}

func (p *piRunProjection) emit(event agentcore.StreamEvent) {
	event.Turn = p.result.Turns
	if p.sink != nil {
		p.sink(event)
	}
}

func (p *piRunProjection) event(raw json.RawMessage) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	var event struct {
		Type                  string
		Message               json.RawMessage
		ToolCallID            string `json:"toolCallId"`
		ToolName              string `json:"toolName"`
		Args                  json.RawMessage
		Result                json.RawMessage
		PartialResult         json.RawMessage
		IsError               bool
		AssistantMessageEvent struct{ Type, Delta string }
	}
	if err := json.Unmarshal(raw, &event); err != nil {
		return err
	}
	switch event.Type {
	case "agent_start":
		p.emit(agentcore.StreamEvent{Type: agentcore.StreamAgentStart})
	case "agent_end":
		p.emit(agentcore.StreamEvent{Type: agentcore.StreamAgentEnd})
	case "turn_start":
		p.result.Turns++
		p.emit(agentcore.StreamEvent{Type: agentcore.StreamTurnStart})
	case "turn_end":
		p.emit(agentcore.StreamEvent{Type: agentcore.StreamTurnEnd})
	case "message_start", "message_end":
		message, err := projectPiMessage(event.Message)
		if err != nil {
			return err
		}
		if message.Role != agentcore.RoleAssistant {
			return nil
		}
		if event.Type == "message_start" {
			p.emit(agentcore.StreamEvent{Type: agentcore.StreamMessageStart})
			return nil
		}
		var native struct{ StopReason string }
		_ = json.Unmarshal(event.Message, &native)
		p.result.Final = message.Content
		p.result.StopReason = native.StopReason
		if message.Usage != nil {
			u := message.Usage
			p.result.Usage.InputTokens += u.InputTokens
			p.result.Usage.OutputTokens += u.OutputTokens
			p.result.Usage.CacheReadTokens += u.CacheReadTokens
			p.result.Usage.CacheWriteTokens += u.CacheWriteTokens
			p.result.Usage.CostUSD += u.CostUSD
			p.result.Usage.CostUnpriced = p.result.Usage.CostUnpriced || u.CostUnpriced || (!p.pricingKnown && u.InputTokens+u.OutputTokens+u.CacheReadTokens+u.CacheWriteTokens > 0)
		}
		p.emit(agentcore.StreamEvent{Type: agentcore.StreamMessageEnd})
	case "message_update":
		if event.AssistantMessageEvent.Type == "text_delta" {
			p.emit(agentcore.StreamEvent{Type: agentcore.StreamToken, Token: event.AssistantMessageEvent.Delta})
		}
	case "tool_execution_start":
		trace := agentcore.ToolTrace{CallID: event.ToolCallID, Tool: event.ToolName, Args: string(event.Args)}
		p.calls[event.ToolCallID] = trace
		p.emit(agentcore.StreamEvent{Type: agentcore.StreamToolExecStart, Tool: &trace})
	case "tool_execution_update":
		trace := p.calls[event.ToolCallID]
		var partial struct{ Content json.RawMessage }
		if err := json.Unmarshal(event.PartialResult, &partial); err != nil {
			return err
		}
		text, _, _, err := projectPiContent(partial.Content)
		if err != nil {
			return err
		}
		p.emit(agentcore.StreamEvent{Type: agentcore.StreamToolExecUpdate, Tool: &trace, Note: text})
	case "tool_execution_end":
		trace := p.calls[event.ToolCallID]
		var result struct {
			Content json.RawMessage
			Details json.RawMessage
		}
		if err := json.Unmarshal(event.Result, &result); err != nil {
			return err
		}
		var audit agentcore.PiToolOutcome
		governed := json.Unmarshal(result.Details, &audit) == nil && audit.Trace.CallID == event.ToolCallID && audit.Trace.Tool == event.ToolName
		if governed {
			trace = audit.Trace
			if audit.Parked {
				p.result.Parked = true
				p.result.Question = json.RawMessage(audit.Trace.Args)
				p.emit(agentcore.StreamEvent{Type: agentcore.StreamQuestion, Question: p.result.Question})
			}
		} else {
			trace.Allowed = !event.IsError
			if event.IsError {
				trace.Error, _, _, _ = projectPiContent(result.Content)
			}
		}
		p.result.Tools = append(p.result.Tools, trace)
		if governed {
			for _, nested := range audit.Invocations {
				p.result.Tools = append(p.result.Tools, nested.Trace)
			}
		}
		p.emit(agentcore.StreamEvent{Type: agentcore.StreamToolExecEnd, Tool: &trace})
		p.emit(agentcore.StreamEvent{Type: agentcore.StreamTool, Tool: &trace})
		delete(p.calls, event.ToolCallID)
	}
	return nil
}

func projectPiMessage(raw json.RawMessage) (agentcore.Message, error) {
	var native struct {
		Role, ToolCallID, ToolName string
		Content                    json.RawMessage
		Usage                      *struct {
			Input, Output, CacheRead, CacheWrite int
			Cost                                 *struct{ Total float64 }
		}
	}
	if err := json.Unmarshal(raw, &native); err != nil {
		return agentcore.Message{}, err
	}
	m := agentcore.Message{Role: agentcore.Role(native.Role), ToolCallID: native.ToolCallID, Name: native.ToolName}
	if native.Role == "toolResult" {
		m.Role = agentcore.RoleTool
	}
	// Custom messages are retained in native state without imposing a legacy
	// content schema or making their extension payload model-visible text.
	if native.Role == "system" || native.Role == "user" || native.Role == "assistant" || native.Role == "toolResult" {
		var err error
		m.Content, m.ContentParts, m.ToolCalls, err = projectPiContent(native.Content)
		if err != nil {
			return m, err
		}
	}
	if native.Usage != nil {
		u := native.Usage
		m.Usage = &agentcore.Usage{InputTokens: u.Input, OutputTokens: u.Output, CacheReadTokens: u.CacheRead, CacheWriteTokens: u.CacheWrite, CostUnpriced: u.Cost == nil}
		if u.Cost != nil {
			m.Usage.CostUSD = u.Cost.Total
		}
	}
	return m, nil
}

func projectPiContent(raw json.RawMessage) (string, []agentcore.ContentPart, []agentcore.ToolCall, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil, nil, nil
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text, nil, nil, nil
	}
	var blocks []struct {
		Type, Text, Data, MimeType, ID, Name string
		Arguments                            json.RawMessage
	}
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return "", nil, nil, err
	}
	var parts []agentcore.ContentPart
	var calls []agentcore.ToolCall
	for _, block := range blocks {
		switch block.Type {
		case "text":
			text += block.Text
		case "image":
			parts = append(parts, agentcore.ContentPart{Type: agentcore.ContentPartImage, MIMEType: block.MimeType, Data: block.Data})
		case "toolCall":
			calls = append(calls, agentcore.ToolCall{ID: block.ID, Name: block.Name, Arguments: string(block.Arguments)})
		}
	}
	return text, parts, calls, nil
}
