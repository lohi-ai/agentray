package agentruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/lohi-ai/agentray/agentcore"
	nativehost "github.com/lohi-ai/agentray/agentcore/host"
	"github.com/lohi-ai/agentray/ai"
)

// PiRunConfig executes a native run. Seed history belongs in the native
// initialState.messages option; Input is a native prompt, or nil to Continue.
type PiRunConfig struct {
	Compaction *nativehost.CompactionPolicy
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

// RunPi drives the selected Pi-contract agent (Go unless a worker is explicit)
// with server session policy and the server's streaming/result vocabulary.
// Lossless events and state remain authoritative in the durable log and the
// caller's original OnEvent callback.
func RunPi(ctx context.Context, cfg PiRunConfig) (result PiRunResult, err error) {
	if cfg.Host != nil {
		defer func() { result.Projection = cfg.Host.CompletePiRun(context.WithoutCancel(ctx), result.Projection) }()
	}
	var projection piRunProjection
	projection.sink, projection.pricingKnown = cfg.Sink, cfg.PricingKnown
	projection.calls = map[string]agentcore.ToolTrace{}
	cfg.Session.nativeAttemptObserved = projection.accountNativeAttempt
	cfg.Session.nativeTerminalPublished = projection.expectNativeTerminal
	var lifecycle *piHostLifecycle
	if cfg.Host != nil {
		lifecycle, err = bindPiHostLifecycle(ctx, &cfg, &projection)
		if err != nil {
			return result, err
		}
		originalRequest := cfg.Session.Pi.OnRequest
		cfg.Session.Pi.OnRequest = func(ctx context.Context, transcript ai.TranscriptContext) error {
			if originalRequest != nil {
				if err := originalRequest(ctx, transcript); err != nil {
					return err
				}
			}
			raw, err := json.Marshal(transcript.Messages())
			if err != nil {
				return err
			}
			projection.mu.Lock()
			turn := projection.result.Turns
			projection.mu.Unlock()
			return cfg.Host.ObservePiMessages(ctx, agentcore.PhaseRequest, turn, raw)
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
		if cfg.Host != nil {
			var appended struct {
				Type    string
				Message json.RawMessage
			}
			if err := json.Unmarshal(event, &appended); err != nil {
				return err
			}
			if appended.Type == "message_end" {
				projection.mu.Lock()
				turn := projection.result.Turns
				projection.mu.Unlock()
				raw, err := json.Marshal([]json.RawMessage{appended.Message})
				if err != nil {
					return err
				}
				if err := cfg.Host.ObservePiMessages(eventCtx, agentcore.PhaseAppend, turn, raw); err != nil {
					return err
				}
			}
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
	if err := session.resumeDelegations(ctx, cfg.Host, &projection); err != nil {
		projection.mu.Lock()
		result.Projection = projection.result
		projection.mu.Unlock()
		return result, err
	}
	question, err := session.PendingQuestion(ctx)
	if err != nil {
		return result, err
	}
	terminated := false
	if len(question) == 0 {
		terminated, err = session.finishTerminalDelegations(ctx, cfg.Input)
		if err != nil {
			return result, err
		}
	}
	if len(question) > 0 {
		projection.result.Parked, projection.result.Question, projection.result.StopReason = true, question, "parked"
		projection.emit(agentcore.StreamEvent{Type: agentcore.StreamQuestion, Question: question})
		if lifecycle != nil {
			lifecycle.parked, lifecycle.stopReason = true, "parked"
		}
	} else if terminated {
		projection.result.StopReason = "toolUse"
		if lifecycle != nil {
			lifecycle.stopReason = "toolUse"
		}
	} else if lifecycle != nil {
		if err := lifecycle.prepareFirst(ctx, session); err != nil {
			return result, err
		}
	}
	if len(question) > 0 || terminated {
		// Reopening a parked run is not a model turn. Keep the current native
		// snapshot and surface the same question until the human answers.
	} else if len(cfg.Input) == 0 {
		err = session.Continue(ctx)
	} else {
		err = session.Prompt(ctx, cfg.Input)
	}
	finishCtx, finishCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer finishCancel()
	if idleErr := session.agent.wait(finishCtx); idleErr != nil {
		err = errors.Join(err, idleErr)
	}
	state, stateErr := session.State(finishCtx)
	telemetry, telemetryErr := session.agent.spans(finishCtx)
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
			message, decodeErr := nativehost.ProjectMessage(raw)
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
	mu             sync.Mutex
	sink           agentcore.StreamSink
	pricingKnown   bool
	result         agentcore.RunResult
	calls          map[string]agentcore.ToolTrace
	nativeTerminal json.RawMessage
}

func (p *piRunProjection) emit(event agentcore.StreamEvent) {
	event.Turn = p.result.Turns
	if p.sink != nil {
		p.sink(event)
	}
}

// Continuations emit host stream frames for real tool work. These are display
// projections, not fabricated Pi events, and never enter native history.
func (p *piRunProjection) delegationStart(original agentcore.ToolTrace) agentcore.ToolTrace {
	trace := agentcore.ToolTrace{CallID: original.CallID, Tool: original.Tool, Args: original.Args, IdempotencyKey: original.IdempotencyKey}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.emit(agentcore.StreamEvent{Type: agentcore.StreamToolExecStart, Tool: &trace})
	return trace
}

func (p *piRunProjection) delegationUpdate(trace agentcore.ToolTrace, raw json.RawMessage) error {
	var update struct{ Content json.RawMessage }
	if err := json.Unmarshal(raw, &update); err != nil {
		return err
	}
	text, _, _, err := nativehost.ProjectContent(update.Content)
	if err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.emit(agentcore.StreamEvent{Type: agentcore.StreamToolExecUpdate, Tool: &trace, Note: text})
	return nil
}

func (p *piRunProjection) delegationEnd(trace agentcore.ToolTrace, audit agentcore.PiToolOutcome, failure error) {
	if audit.Trace.CallID == trace.CallID && audit.Trace.Tool == trace.Tool {
		trace = audit.Trace
	}
	if failure != nil {
		trace.Error = failure.Error()
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.result.Tools = append(p.result.Tools, trace)
	for _, nested := range audit.Invocations {
		p.result.Tools = append(p.result.Tools, nested.Trace)
	}
	p.emit(agentcore.StreamEvent{Type: agentcore.StreamToolExecEnd, Tool: &trace})
	p.emit(agentcore.StreamEvent{Type: agentcore.StreamTool, Tool: &trace})
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
		message, err := nativehost.ProjectMessage(event.Message)
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
		accounted := len(p.nativeTerminal) > 0
		if accounted {
			terminal, err := nativeUsageTerminal(event.Message)
			if err != nil {
				return err
			}
			if !nativehost.SameJSON(p.nativeTerminal, terminal) {
				return errors.New("native terminal differs from accounted attempt")
			}
			p.nativeTerminal = nil
		}
		p.result.Final = message.Content
		p.result.StopReason = native.StopReason
		if message.Usage != nil && !accounted {
			p.addUsage(*message.Usage, p.pricingKnown)
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
		text, _, _, err := nativehost.ProjectContent(partial.Content)
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
				if audit.ChildQuestion != nil {
					p.result.Question = audit.ChildQuestion.Question
				}
				p.emit(agentcore.StreamEvent{Type: agentcore.StreamQuestion, Question: p.result.Question})
			}
		} else {
			trace.Allowed = !event.IsError
			if event.IsError {
				trace.Error, _, _, _ = nativehost.ProjectContent(result.Content)
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
