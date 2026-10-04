package agentruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/lohi-ai/agentray/agentcore/engine"
	"github.com/lohi-ai/agentray/ai"
)

type nativeAgentFixture struct {
	*NativeAgent
	options engine.AgentConfig
}

// Call decodes recorded worker actions for the migration fixture only.
func (a *nativeAgentFixture) Call(ctx context.Context, method string, params json.RawMessage) (json.RawMessage, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := a.ctx.Err(); err != nil {
		return nil, err
	}
	switch method {
	case "state":
		return a.State(ctx)
	case "prompt":
		var value struct {
			Input  json.RawMessage
			Images []ai.ContentBlock
		}
		if err := json.Unmarshal(params, &value); err != nil {
			return nil, err
		}
		return nil, a.run(ctx, false, value.Input, value.Images)
	case "continue":
		return nil, a.Continue(ctx)
	case "waitForIdle":
		return nil, a.wait(ctx)
	case "telemetry":
		return json.Marshal(a.recorder.GetSpans())
	case "abort":
		a.agent.Abort()
		return nil, nil
	case "reset":
		a.mu.Lock()
		defer a.mu.Unlock()
		if a.running != nil {
			return nil, errors.New("Agent is already processing. Wait for completion before resetting.")
		}
		return nil, a.agent.Reset()
	case "clearSteeringQueue":
		a.agent.ClearSteeringQueue()
		return nil, nil
	case "clearFollowUpQueue":
		a.agent.ClearFollowUpQueue()
		return nil, nil
	case "clearAllQueues":
		a.agent.ClearAllQueues()
		return nil, nil
	case "hasQueuedMessages":
		return json.Marshal(a.agent.HasQueuedMessages())
	case "peekQueuedMessages":
		return json.Marshal(a.agent.PeekQueuedMessages())
	case "steer", "followUp":
		var message ai.Message
		if err := json.Unmarshal(params, &message); err != nil {
			return nil, err
		}
		if method == "steer" {
			a.agent.Steer(&message)
		} else {
			a.agent.FollowUp(&message)
		}
		return nil, nil
	case "setState":
		return nil, a.setState(ctx, params)
	case "configure":
		return nil, a.configure(params)
	default:
		return nil, fmt.Errorf("Unknown method: %s", method)
	}
}

func (a *nativeAgentFixture) configure(raw json.RawMessage) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	next := a.options
	for key, value := range fields {
		var target any
		switch key {
		case "steeringMode":
			target = &next.SteeringMode
		case "followUpMode":
			target = &next.FollowUpMode
		case "sessionId":
			target = &next.SessionID
		case "thinkingBudgets":
			target = &next.ThinkingBudgets
		case "transport":
			target = &next.Transport
		case "maxRetryDelayMs":
			target = &next.MaxRetryDelayMS
		case "toolExecution":
			target = &next.ToolExecution
		default:
			return fmt.Errorf("Unknown setting: %s", key)
		}
		if err := json.Unmarshal(value, target); err != nil {
			return err
		}
	}
	if err := a.agent.Configure(next); err != nil {
		return err
	}
	a.options = next
	return nil
}
