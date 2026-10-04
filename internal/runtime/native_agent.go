package agentruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/lohi-ai/agentray/agentcore"
	"github.com/lohi-ai/agentray/agentcore/engine"
	"github.com/lohi-ai/agentray/ai"
	"github.com/lohi-ai/agentray/internal/jsonjs"
	"github.com/lohi-ai/agentray/telemetry"
)

// NativeAgentConfig keeps the existing host JSON/callback vocabulary while
// executing the ported Agent directly in Go. StreamFn overrides the built-in Go
// provider dispatcher in streamMode=native; callback mode uses Callback instead.
type NativeAgentConfig struct {
	Options  json.RawMessage
	Callback agentcore.PiCallback
	OnEvent  func(context.Context, json.RawMessage) error
	// OnTrace enables request tracing. Delivery is passive and flushed at run end.
	OnTrace  func(context.Context, json.RawMessage)
	StreamFn engine.StreamFn
	Now      func() int64
	// Set only by the native session host after binding its model ladder.
	admitRequest engine.AdmitRequestFn
}

// NativeAgent is the host adapter around engine.Agent. Policy, durable writes,
// credentials and tool implementations remain in host callbacks.
type NativeAgent struct {
	agent         *engine.Agent
	config        NativeAgentConfig
	options       engine.AgentConfig
	ctx           context.Context
	cancel        context.CancelFunc
	recorder      *telemetry.InMemory
	mu            sync.Mutex
	parent        telemetry.Context
	running       chan struct{}
	runCancel     context.CancelFunc
	closed        bool
	codexSessions map[string]struct{}
	traceRequests bool
	nextRequestID atomic.Uint64
	requests      sync.WaitGroup
	traceMu       sync.Mutex
	traceTail     <-chan struct{}
}

const nativeAgentRevision = "eeac84ca92498ac18b6832754d01aef1d3c5f654"

func NewNativeAgent(ctx context.Context, config NativeAgentConfig) (*NativeAgent, error) {
	life, cancel := context.WithCancel(ctx)
	a := &NativeAgent{ctx: life, cancel: cancel, config: config, recorder: telemetry.NewInMemory()}
	options, err := a.bindOptions(config.Options)
	if err != nil {
		cancel()
		return nil, err
	}
	a.options = options.AgentConfig
	a.agent, err = engine.NewAgent(options)
	if err != nil {
		cancel()
		return nil, err
	}
	a.agent.Subscribe(&engine.Listener{Handle: func(_ context.Context, event engine.Event) error {
		if a.config.OnEvent == nil {
			return nil
		}
		raw, err := json.Marshal(event)
		if err != nil {
			return err
		}
		// Events, including the final aborted lifecycle, use the agent lifetime.
		// A prompt's abort signal must not discard its durable event callbacks.
		_, err = invokeNativeCallback(a.ctx, func(ctx context.Context, _ string, params json.RawMessage, _ func(json.RawMessage) error) (json.RawMessage, error) {
			return nil, a.config.OnEvent(ctx, params)
		}, "event", raw, nil)
		return err
	}})
	return a, nil
}

// UpstreamCommit identifies the reference contract, not source-byte identity
// between the Go implementation and the original TypeScript.
func (*NativeAgent) UpstreamCommit() string { return nativeAgentRevision }
func (a *NativeAgent) telemetryParent() telemetry.Context {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.parent
}
func (a *NativeAgent) callbackContext() context.Context {
	if a.agent != nil {
		if ctx := a.agent.Signal(); ctx != nil {
			return ctx
		}
	}
	return a.ctx
}

func (a *NativeAgent) State(ctx context.Context) (json.RawMessage, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := a.ctx.Err(); err != nil {
		return nil, err
	}
	state := a.agent.State()
	raw, err := json.Marshal(state)
	if err != nil {
		return nil, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, err
	}
	// The host protocol explicitly spreads Pi's Set into an array. The native
	// engine keeps the original Set identity and direct JSON shape instead.
	fields["pendingToolCalls"], err = json.Marshal(state.PendingToolCalls.Values())
	if err != nil {
		return nil, err
	}
	return json.Marshal(fields)
}

func (a *NativeAgent) Prompt(ctx context.Context, input json.RawMessage) error {
	return a.run(ctx, false, input, nil)
}
func (a *NativeAgent) Continue(ctx context.Context) error { return a.run(ctx, true, nil, nil) }

func (a *NativeAgent) run(ctx context.Context, continuation bool, input json.RawMessage, images []ai.ContentBlock) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	a.mu.Lock()
	if a.closed || a.ctx.Err() != nil {
		a.mu.Unlock()
		return errors.New("native agent closed")
	}
	if a.running != nil {
		a.mu.Unlock()
		if continuation {
			return errors.New("Agent is already processing. Wait for completion before continuing.")
		}
		return errors.New("Agent is already processing a prompt. Use steer() or followUp() to queue messages, or wait for completion.")
	}
	var prompt any
	if !continuation {
		var err error
		prompt, err = decodeNativePrompt(input)
		if err != nil {
			a.mu.Unlock()
			return err
		}
	}
	runCtx, cancel := context.WithCancel(ctx)
	stopLife := context.AfterFunc(a.ctx, cancel)
	if a.ctx.Err() != nil {
		cancel()
	}
	done := make(chan struct{})
	a.running = done
	a.runCancel = cancel
	a.mu.Unlock()
	result := make(chan error, 1)
	go func() {
		err := a.recorder.StartSpan(telemetry.SpanOptions{Name: "agentray.agent.run"}, func(span *telemetry.Span) error {
			a.mu.Lock()
			a.parent = span.Context()
			a.mu.Unlock()
			defer func() { a.mu.Lock(); a.parent = telemetry.Context{}; a.mu.Unlock() }()
			defer a.flushTraces()
			var err error
			if continuation {
				err = a.agent.Continue(runCtx)
			} else {
				err = a.agent.Prompt(runCtx, prompt, ai.BlockContent(images...).Blocks.Values()...)
			}
			if state := a.agent.State(); state.ErrorMessage != nil && *state.ErrorMessage != "" {
				span.SetStatus(telemetry.SpanStatus{Status: "error"})
			}
			return err
		})
		stopLife()
		cancel()
		a.mu.Lock()
		close(done)
		a.running = nil
		a.runCancel = nil
		a.mu.Unlock()
		result <- err
	}()
	select {
	case err := <-result:
		return err
	case <-ctx.Done():
		return ctx.Err()
	case <-a.ctx.Done():
		return a.ctx.Err()
	}
}

func decodeNativePrompt(raw json.RawMessage) (any, error) {
	raw = bytes.TrimSpace(raw)
	if text, err := nativeString(raw); err == nil {
		return text, nil
	}
	var messages []ai.Message
	if len(raw) > 0 && raw[0] == '[' {
		if err := json.Unmarshal(raw, &messages); err != nil {
			return nil, err
		}
		return messages, nil
	}
	var message ai.Message
	if err := json.Unmarshal(raw, &message); err != nil {
		return nil, err
	}
	return message, nil
}

// Keep the typed boundary (including its null handling and diagnostics), then
// recover lone UTF-16 units that encoding/json replaces in ordinary strings.
func nativeString(raw json.RawMessage) (string, error) {
	var text string
	if err := json.Unmarshal(raw, &text); err != nil {
		return "", err
	}
	value, err := jsonjs.DecodeJSON(raw)
	if err != nil {
		return "", err
	}
	if value != nil {
		text = value.(string)
	}
	return text, nil
}

func (a *NativeAgent) wait(ctx context.Context) error {
	a.mu.Lock()
	done := a.running
	a.mu.Unlock()
	if done == nil {
		return nil
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (a *NativeAgent) Close() error {
	a.mu.Lock()
	a.closed = true
	a.cancel()
	a.mu.Unlock()
	a.agent.Abort()
	err := a.wait(context.Background())
	a.mu.Lock()
	sessions := a.codexSessions
	a.codexSessions = nil
	a.mu.Unlock()
	for session := range sessions {
		ai.CloseCodexResponsesSessions(session)
	}
	return err
}

// Call keeps the session adapter's JSON operations local. It never serializes
// a command to another process. Mutating settings does not replace callbacks.
func (a *NativeAgent) Call(ctx context.Context, method string, params json.RawMessage) (json.RawMessage, error) {
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
		a.mu.Lock()
		if a.runCancel != nil {
			a.runCancel()
		}
		a.mu.Unlock()
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
		return nil, a.setState(params)
	case "configure":
		return nil, a.configure(params)
	default:
		return nil, fmt.Errorf("Unknown method: %s", method)
	}
}

func (a *NativeAgent) setState(raw json.RawMessage) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return err
	}
	for key := range fields {
		switch key {
		case "model", "thinkingLevel", "messages", "tools":
		default:
			return fmt.Errorf("State field is read-only: %s", key)
		}
	}
	// Decode every supplied value before mutating state.
	var level string
	var messages []ai.Message
	var tools []*engine.Tool
	var err error
	if value, ok := fields["thinkingLevel"]; ok {
		if err = json.Unmarshal(value, &level); err != nil {
			return err
		}
	}
	if value, ok := fields["messages"]; ok {
		if err = json.Unmarshal(value, &messages); err != nil {
			return err
		}
	}
	if value, ok := fields["tools"]; ok {
		tools, err = a.bindTools(value)
		if err != nil {
			return err
		}
	}
	if value, ok := fields["model"]; ok {
		a.agent.SetModel(value)
	}
	if _, ok := fields["thinkingLevel"]; ok {
		a.agent.SetThinkingLevel(level)
	}
	if _, ok := fields["messages"]; ok {
		a.agent.SetMessages(engine.MessagePointers(messages))
	}
	if _, ok := fields["tools"]; ok {
		a.agent.SetTools(tools)
	}
	return nil
}

func (a *NativeAgent) configure(raw json.RawMessage) error {
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
