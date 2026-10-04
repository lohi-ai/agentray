package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"

	"github.com/lohi-ai/agentray/ai"
)

type InitialState struct {
	SystemPrompt  string
	Model         json.RawMessage
	ThinkingLevel string
	Tools         []*Tool
	Messages      []*ai.Message
}

// AgentConfig configures the stateful wrapper. Config supplies the shared loop
// hooks and stream options; state and owned queues override its model/queue
// fields. Configure replaces these options atomically for future runs. An
// already-installed preparation hook reads the latest wrapper callbacks.
type AgentConfig struct {
	Config
	StreamFn                   StreamFn
	PrepareNextTurn            func(context.Context) (*TurnUpdate, error)
	PrepareNextTurnWithContext func(context.Context, Turn) (*TurnUpdate, error)
	SteeringMode               string
	FollowUpMode               string
	SessionID                  *string
	ThinkingBudgets            json.RawMessage
	Transport                  string
	MaxRetryDelayMS            *int
}

type AgentOptions struct {
	InitialState InitialState
	AgentConfig
}

// State snapshots scalar fields while retaining live message/tool collections
// and message objects. List operations are synchronized; callers synchronize
// concurrent item edits. Use SetMessages/SetTools to replace a collection.
type State struct {
	Model            json.RawMessage `json:"model"`
	ThinkingLevel    string          `json:"thinkingLevel"`
	Tools            *ToolList       `json:"tools"`
	Messages         *MessageList    `json:"messages"`
	IsStreaming      bool            `json:"isStreaming"`
	StreamingMessage *ai.Message     `json:"streamingMessage,omitempty"`
	PendingToolCalls *ToolCallSet    `json:"pendingToolCalls"`
	ErrorMessage     *string         `json:"errorMessage,omitempty"`
}

// SystemPrompt replays the current collection lazily, like Pi's state getter.
// Holding State retains that collection even when the agent later replaces it.
func (s State) SystemPrompt() string {
	return ai.GetCurrentSystemPrompt(MessageValues(s.Messages.Values()))
}

func (s State) MarshalJSON() ([]byte, error) {
	type plain State
	return json.Marshal(struct {
		SystemPrompt string `json:"systemPrompt"`
		plain
	}{SystemPrompt: s.SystemPrompt(), plain: plain(s)})
}

// Listener has identity independently of its Go callback. Subscribing the same
// pointer twice is idempotent, as with Pi's Set of listener functions.
type Listener struct {
	Handle func(context.Context, Event) error
}

type activeRun struct {
	ctx        context.Context
	cancel     context.CancelFunc
	done       chan struct{}
	stopParent func() bool
}

type messageQueue struct {
	mode     string
	messages []*ai.Message
}

func (q *messageQueue) peek() []*ai.Message {
	count := len(q.messages)
	if q.mode != "all" && count > 1 {
		count = 1
	}
	return append([]*ai.Message{}, q.messages[:count]...)
}

func (q *messageQueue) drain() []*ai.Message {
	selected := q.peek()
	q.messages = slices.Clone(q.messages[len(selected):])
	return selected
}

// Agent owns transcript state, queues and awaited subscriptions. It invokes the
// native Go loop directly; no worker process or legacy Agent is involved.
type Agent struct {
	mu             sync.Mutex
	state          State
	messages       *MessageList
	options        AgentConfig
	steering       messageQueue
	followUp       messageQueue
	listeners      map[*Listener]uint64
	nextListenerID uint64
	active         *activeRun
}

const defaultModel = `{"id":"unknown","name":"unknown","api":"unknown","provider":"unknown","baseUrl":"","reasoning":false,"input":[],"cost":{"input":0,"output":0,"cacheRead":0,"cacheWrite":0},"contextWindow":0,"maxTokens":0}`

func NewAgent(options AgentOptions) (*Agent, error) {
	configured, err := normalizeAgentConfig(options.AgentConfig)
	if err != nil {
		return nil, err
	}
	initial := options.InitialState
	state := State{Model: slices.Clone(initial.Model), ThinkingLevel: initial.ThinkingLevel,
		Tools: NewList(initial.Tools...), PendingToolCalls: &ToolCallSet{}}
	messages := append([]*ai.Message{}, initial.Messages...)
	if state.Model == nil {
		state.Model = json.RawMessage(defaultModel)
	}
	if state.ThinkingLevel == "" {
		state.ThinkingLevel = "off"
	}
	declarations := make([]ai.Tool, len(initial.Tools))
	for i, tool := range initial.Tools {
		declarations[i] = ai.ToToolDeclaration(tool.Tool)
	}
	baseline := ai.CreateInitialSystemMessage(initial.SystemPrompt, declarations)
	if baseline != nil && (len(messages) == 0 || messages[0].Role != "system") {
		messages = append([]*ai.Message{baseline}, messages...)
	}
	return &Agent{state: state, messages: NewList(messages...), options: configured, steering: messageQueue{mode: configured.SteeringMode},
		followUp: messageQueue{mode: configured.FollowUpMode}, listeners: map[*Listener]uint64{}}, nil
}

func normalizeAgentConfig(options AgentConfig) (AgentConfig, error) {
	if options.StreamFn == nil && options.AdmitRequest == nil {
		var err error
		options.StreamFn, err = GetDefaultStreamFn()
		if err != nil {
			return options, err
		}
	}
	if options.ConvertToLLM == nil {
		options.ConvertToLLM = defaultConvertToLLM
	}
	if options.SteeringMode == "" {
		options.SteeringMode = "one-at-a-time"
	}
	if options.FollowUpMode == "" {
		options.FollowUpMode = "one-at-a-time"
	}
	if options.Transport == "" {
		options.Transport = "auto"
	}
	if options.ToolExecution == "" {
		options.ToolExecution = "parallel"
	}
	return options, nil
}

func defaultConvertToLLM(messages []*ai.Message) ([]*ai.Message, error) {
	result := []*ai.Message{}
	for _, message := range messages {
		switch message.Role {
		case "system", "user", "assistant", "toolResult":
			result = append(result, message)
		}
	}
	return result, nil
}

func (a *Agent) Configure(options AgentConfig) error {
	configured, err := normalizeAgentConfig(options)
	if err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.options = configured
	a.steering.mode, a.followUp.mode = configured.SteeringMode, configured.FollowUpMode
	return nil
}

// Caller holds mu. Replay helpers read a shallow projection of live entries.
func (a *Agent) messageSnapshot() []ai.Message {
	return MessageValues(a.messages.Values())
}

func (a *Agent) State() State {
	a.mu.Lock()
	defer a.mu.Unlock()
	state := a.state
	state.Messages = a.messages
	return state
}

func (a *Agent) SetMessages(messages []*ai.Message) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.messages = NewList(messages...)
}
func (a *Agent) SetTools(tools []*Tool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.state.Tools = NewList(tools...)
}

// SetMessageList mirrors assigning a state array, including its sparse slots.
// Assignment detaches the outer collection and retains message objects.
func (a *Agent) SetMessageList(messages *MessageList) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.messages = messages.Clone()
}

func (a *Agent) SetToolList(tools *ToolList) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.state.Tools = tools.Clone()
}

func (a *Agent) SetModel(model json.RawMessage) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.state.Model = slices.Clone(model)
}
func (a *Agent) SetThinkingLevel(level string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.state.ThinkingLevel = level
}
func (a *Agent) SetSteeringMode(mode string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.steering.mode = mode
}
func (a *Agent) SetFollowUpMode(mode string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.followUp.mode = mode
}
func (a *Agent) SteeringMode() string { a.mu.Lock(); defer a.mu.Unlock(); return a.steering.mode }
func (a *Agent) FollowUpMode() string { a.mu.Lock(); defer a.mu.Unlock(); return a.followUp.mode }
func (a *Agent) Steer(message *ai.Message) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.steering.messages = append(a.steering.messages, message)
}
func (a *Agent) FollowUp(message *ai.Message) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.followUp.messages = append(a.followUp.messages, message)
}
func (a *Agent) ClearSteeringQueue() { a.mu.Lock(); defer a.mu.Unlock(); a.steering.messages = nil }
func (a *Agent) ClearFollowUpQueue() { a.mu.Lock(); defer a.mu.Unlock(); a.followUp.messages = nil }
func (a *Agent) ClearAllQueues() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.steering.messages, a.followUp.messages = nil, nil
}
func (a *Agent) HasQueuedMessages() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.steering.messages)+len(a.followUp.messages) > 0
}
func (a *Agent) PeekQueuedMessages() []*ai.Message {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.steering.messages) > 0 {
		return a.steering.peek()
	}
	return a.followUp.peek()
}

func (a *Agent) Subscribe(listener *Listener) func() {
	a.mu.Lock()
	if _, exists := a.listeners[listener]; !exists {
		a.nextListenerID++
		a.listeners[listener] = a.nextListenerID
	}
	a.mu.Unlock()
	return func() { a.mu.Lock(); delete(a.listeners, listener); a.mu.Unlock() }
}

func (a *Agent) Signal() context.Context {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.active == nil {
		return nil
	}
	return a.active.ctx
}

func (a *Agent) Abort() {
	a.mu.Lock()
	active := a.active
	a.mu.Unlock()
	if active != nil {
		active.cancel()
	}
}

// WaitForIdle observes the current run, including awaited agent_end listeners.
// Cancelling the wait does not cancel the agent itself.
func (a *Agent) WaitForIdle(ctx context.Context) error {
	a.mu.Lock()
	active := a.active
	a.mu.Unlock()
	if active == nil {
		return nil
	}
	select {
	case <-active.done:
		return nil
	default:
	}
	select {
	case <-active.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (a *Agent) Reset() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.active != nil {
		return errors.New("Agent is already processing. Wait for completion before resetting.")
	}
	baseline := ai.GetCurrentSystemMessage(a.messageSnapshot())
	a.messages = NewList[*ai.Message]()
	if baseline != nil {
		a.messages.Append(baseline)
	}
	a.state.IsStreaming, a.state.StreamingMessage, a.state.ErrorMessage = false, nil, nil
	a.state.PendingToolCalls = &ToolCallSet{}
	a.steering.messages, a.followUp.messages = nil, nil
	return nil
}

const busyPrompt = "Agent is already processing a prompt. Use steer() or followUp() to queue messages, or wait for completion."

// Prompt accepts text, message values or message references (single or slices).
// Reference inputs keep their identity through callbacks and history.
// Images accompany text input.
// It blocks until the run and its listeners settle. Admission errors are
// returned directly; run failures become Pi's assistant failure lifecycle.
func (a *Agent) Prompt(ctx context.Context, input any, images ...ai.ContentBlock) error {
	a.mu.Lock()
	if a.active != nil {
		a.mu.Unlock()
		return errors.New(busyPrompt)
	}
	now := a.options.Config.now
	a.mu.Unlock()
	var messages []*ai.Message
	switch input := input.(type) {
	case string:
		content := append([]ai.ContentBlock{{Type: "text", Text: input}}, images...)
		messages = []*ai.Message{{Role: "user", Content: ai.BlockContent(content...), Timestamp: now()}}
	case ai.Message:
		messages = []*ai.Message{&input}
	case *ai.Message:
		messages = []*ai.Message{input}
	case []ai.Message:
		messages = MessagePointers(input)
	case []*ai.Message:
		messages = input
	default:
		return fmt.Errorf("engine: unsupported prompt input %T", input)
	}
	a.mu.Lock()
	if a.active != nil {
		a.mu.Unlock()
		return errors.New(busyPrompt)
	}
	active, current, config, stream := a.beginRun(ctx, false)
	a.mu.Unlock()
	return a.run(active, current, config, stream, messages, false)
}

func (a *Agent) Continue(ctx context.Context) error {
	a.mu.Lock()
	if a.active != nil {
		a.mu.Unlock()
		return errors.New("Agent is already processing. Wait for completion before continuing.")
	}
	messages := a.messages.Values()
	hasNonSystem := false
	for _, message := range messages {
		if message.Role != "system" {
			hasNonSystem = true
			break
		}
	}
	if !hasNonSystem {
		a.mu.Unlock()
		return errors.New("No messages to continue from")
	}
	var prompts []*ai.Message
	skipSteering, continuation := false, true
	if messages[len(messages)-1].Role == "assistant" {
		prompts = a.steering.drain()
		if len(prompts) > 0 {
			skipSteering = true
		} else {
			prompts = a.followUp.drain()
		}
		if len(prompts) == 0 {
			a.mu.Unlock()
			return errors.New("Cannot continue from message role: assistant")
		}
		continuation = false
	}
	active, current, config, stream := a.beginRun(ctx, skipSteering)
	a.mu.Unlock()
	return a.run(active, current, config, stream, prompts, continuation)
}

// beginRun is called with mu held. No user callbacks run during admission.
func (a *Agent) beginRun(parent context.Context, skipSteering bool) (*activeRun, Context, Config, StreamFn) {
	ctx, cancel := context.WithCancel(context.WithoutCancel(parent))
	stopParent := context.AfterFunc(parent, cancel)
	if parent.Err() != nil {
		cancel()
	}
	active := &activeRun{ctx: ctx, cancel: cancel, done: make(chan struct{}), stopParent: stopParent}
	a.active = active
	a.state.IsStreaming, a.state.StreamingMessage, a.state.ErrorMessage = true, nil, nil
	current := Context{Messages: a.messages.Values(), Tools: a.state.Tools.Values()}
	config := a.options.Config
	config.Model = a.state.Model
	config.Reasoning = a.state.ThinkingLevel
	if config.Reasoning == "off" {
		config.Reasoning = ""
	}
	config.Options = make(map[string]any, len(a.options.Options)+4)
	for key, value := range a.options.Options {
		config.Options[key] = value
	}
	config.Options["transport"] = a.options.Transport
	if a.options.SessionID != nil {
		config.Options["sessionId"] = *a.options.SessionID
	}
	if a.options.ThinkingBudgets != nil {
		config.Options["thinkingBudgets"] = a.options.ThinkingBudgets
	}
	if a.options.MaxRetryDelayMS != nil {
		config.Options["maxRetryDelayMs"] = *a.options.MaxRetryDelayMS
	}
	config.PrepareNextTurn = nil
	if a.options.PrepareNextTurnWithContext != nil || a.options.PrepareNextTurn != nil {
		config.PrepareNextTurn = func(turn Turn) (*TurnUpdate, error) {
			a.mu.Lock()
			withContext, legacy := a.options.PrepareNextTurnWithContext, a.options.PrepareNextTurn
			a.mu.Unlock()
			if withContext != nil {
				return withContext(active.ctx, turn)
			}
			if legacy != nil {
				return legacy(active.ctx)
			}
			return nil, nil
		}
	}
	config.GetSteeringMessages = func() ([]*ai.Message, error) {
		a.mu.Lock()
		defer a.mu.Unlock()
		if skipSteering {
			skipSteering = false
			return []*ai.Message{}, nil
		}
		return a.steering.drain(), nil
	}
	config.GetFollowUpMessages = func() ([]*ai.Message, error) { a.mu.Lock(); defer a.mu.Unlock(); return a.followUp.drain(), nil }
	return active, current, config, a.options.StreamFn
}

func (a *Agent) run(active *activeRun, current Context, config Config, stream StreamFn, prompts []*ai.Message, continuation bool) (err error) {
	defer func() {
		active.stopParent()
		a.mu.Lock()
		a.state.IsStreaming, a.state.StreamingMessage = false, nil
		a.state.PendingToolCalls = &ToolCallSet{}
		close(active.done)
		a.active = nil
		a.mu.Unlock()
	}()
	// Pi does not abort a successfully settled signal. Detach the parent
	// registration at settlement without cancelling a signal retained by a
	// subscriber. This also avoids retaining every run in a long-lived parent.
	err = executeAgentRun(func() error {
		if continuation {
			_, err := Continue(active.ctx, current, config, a.processEvent, stream)
			return err
		}
		_, err := Run(active.ctx, prompts, current, config, a.processEvent, stream)
		return err
	})
	if err == nil {
		return nil
	}
	return a.handleFailure(active, err, config.now())
}

func executeAgentRun(fn func() error) (err error) {
	defer func() {
		if value := recover(); value != nil {
			err = failureError(value)
		}
	}()
	return fn()
}

func (a *Agent) handleFailure(active *activeRun, failure error, timestamp int64) error {
	a.mu.Lock()
	modelRaw := a.state.Model
	a.mu.Unlock()
	var model struct {
		API      string `json:"api"`
		Provider string `json:"provider"`
		ID       string `json:"id"`
	}
	if err := json.Unmarshal(modelRaw, &model); err != nil {
		return err
	}
	message := &ai.Message{Role: "assistant", Content: ai.BlockContent(ai.ContentBlock{Type: "text", Text: ""}),
		API: model.API, Provider: model.Provider, Model: model.ID, Usage: &ai.Usage{}, StopReason: "error", Timestamp: timestamp}
	if active.ctx.Err() != nil {
		message.StopReason = "aborted"
	}
	text := failure.Error()
	message.ErrorMessage = &text
	for _, event := range []Event{{Type: "message_start", Message: message}, {Type: "message_end", Message: message},
		{Type: "turn_end", Message: message, ToolResults: []*ai.Message{}}} {
		if err := executeAgentRun(func() error { return a.processEvent(event) }); err != nil {
			return err
		}
	}
	return executeAgentRun(func() error {
		return a.processEvent(Event{Type: "agent_end", Messages: []*ai.Message{message}})
	})
}

func (a *Agent) processEvent(event Event) error {
	a.mu.Lock()
	switch event.Type {
	case "message_start", "message_update":
		a.state.StreamingMessage = event.Message
	case "message_end":
		a.state.StreamingMessage = nil
		a.messages.Append(event.Message)
	case "tool_execution_start":
		a.state.PendingToolCalls = a.state.PendingToolCalls.with(event.ToolCallID)
	case "tool_execution_end":
		a.state.PendingToolCalls = a.state.PendingToolCalls.without(event.ToolCallID)
	case "turn_end":
		if event.Message.Role == "assistant" && event.Message.ErrorMessage != nil && *event.Message.ErrorMessage != "" {
			text := *event.Message.ErrorMessage
			a.state.ErrorMessage = &text
		}
	case "agent_end":
		a.state.StreamingMessage = nil
	}
	active := a.active
	a.mu.Unlock()
	if active == nil {
		return errors.New("Agent listener invoked outside active run")
	}
	// Iterate the live set in insertion order. A listener removed before its
	// turn is skipped; one added by an earlier listener sees the same event.
	var last uint64
	for {
		a.mu.Lock()
		var next *Listener
		var id uint64
		for listener, candidate := range a.listeners {
			if candidate > last && (next == nil || candidate < id) {
				next, id = listener, candidate
			}
		}
		a.mu.Unlock()
		if next == nil {
			return nil
		}
		if err := next.Handle(active.ctx, event); err != nil {
			return err
		}
		last = id
	}
}
