package engine

import (
	"context"
	"encoding/json"
	"errors"
	"slices"

	"github.com/lohi-ai/agentray/ai"
)

// Run starts a prompt invocation. Returned messages exclude pre-existing
// history. Hooks and sink failures propagate without inventing agent_end.
func Run(ctx context.Context, prompts []ai.Message, initial Context, config Config, emit EventSink, stream StreamFn) ([]ai.Message, error) {
	initialMessages := declareToolChanges(initial, prompts, config.now)
	newMessages := append([]ai.Message{}, initialMessages...)
	current := initial
	current.Messages = append(slices.Clone(initial.Messages), initialMessages...)
	if err := emit(Event{Type: "agent_start"}); err != nil {
		return nil, err
	}
	if err := emit(Event{Type: "turn_start"}); err != nil {
		return nil, err
	}
	for i := range initialMessages {
		if err := emitMessage(&initialMessages[i], emit); err != nil {
			return nil, err
		}
	}
	if stream == nil && config.AdmitRequest == nil {
		var err error
		stream, err = GetDefaultStreamFn()
		if err != nil {
			return nil, err
		}
	}
	return runLoop(ctx, &current, newMessages, config, emit, stream)
}

// Continue reuses the transcript without emitting the existing prompt again.
func Continue(ctx context.Context, initial Context, config Config, emit EventSink, stream StreamFn) ([]ai.Message, error) {
	if err := validateContinuation(initial); err != nil {
		return nil, err
	}
	if err := emit(Event{Type: "agent_start"}); err != nil {
		return nil, err
	}
	if err := emit(Event{Type: "turn_start"}); err != nil {
		return nil, err
	}
	if stream == nil && config.AdmitRequest == nil {
		var err error
		stream, err = GetDefaultStreamFn()
		if err != nil {
			return nil, err
		}
	}
	return runLoop(ctx, &initial, []ai.Message{}, config, emit, stream)
}

func runLoop(ctx context.Context, current *Context, messages []ai.Message, config Config, emit EventSink, stream StreamFn) ([]ai.Message, error) {
	var last *Turn
	pending, err := poll(config.GetSteeringMessages)
	if err != nil {
		return nil, err
	}
	explicit := false
	finish := func() ([]ai.Message, error) { return messages, emit(Event{Type: "agent_end", Messages: messages}) }
	for {
		moreTools := true
		for moreTools || len(pending) > 0 {
			var prepared []ai.Message
			if last != nil {
				if config.PrepareNextTurn != nil {
					update, err := config.PrepareNextTurn(*last)
					if err != nil {
						return nil, err
					}
					if update != nil {
						current = applyUpdate(current, &config, update)
						prepared = update.Messages
					}
				}
				if len(pending) == 0 {
					pending, err = poll(config.GetSteeringMessages)
					if err != nil {
						return nil, err
					}
				}
				if err := emit(Event{Type: "turn_start"}); err != nil {
					return nil, err
				}
			}
			for _, message := range declareToolChanges(*current, append(slices.Clone(prepared), pending...), config.now) {
				if err := emitMessage(&message, emit); err != nil {
					return nil, err
				}
				current.Messages = append(current.Messages, message)
				messages = append(messages, message)
			}
			pending = nil
			var admitted *RequestAdmission
			if config.AdmitRequest != nil {
				admitted, err = config.AdmitRequest(ctx, Request{Context: current, Model: config.Model, ThinkingLevel: thinkingLevel(config.Reasoning)}, requestOptions(config))
				if err != nil {
					return nil, err
				}
				if admitted == nil || admitted.Stream == nil || admitted.Request.Context == nil || len(admitted.Request.Model) == 0 {
					return nil, errors.New("engine: invalid prepared request admission")
				}
				current = applyUpdate(current, &config, &TurnUpdate{Context: admitted.Request.Context, Model: admitted.Request.Model, ThinkingLevel: &admitted.Request.ThinkingLevel})
			} else if config.PrepareRequest != nil {
				update, err := config.PrepareRequest(ctx, Request{Context: current, Model: config.Model, ThinkingLevel: thinkingLevel(config.Reasoning)})
				if err != nil {
					return nil, err
				}
				if update != nil {
					current = applyUpdate(current, &config, update)
				}
			}
			var message *ai.Message
			if admitted != nil {
				message, err = consumeAssistant(ctx, current, config, emit, admitted.Stream)
			} else {
				message, err = streamAssistant(ctx, current, config, emit, stream)
			}
			if err != nil {
				return nil, err
			}
			messages = append(messages, *message)
			toolResults := []ai.Message{}
			if message.StopReason == "error" || message.StopReason == "aborted" {
				last = &Turn{Message: message, ToolResults: toolResults, Context: current, NewMessages: messages}
				if config.FinishTurn != nil {
					if _, err := config.FinishTurn(ctx, *last); err != nil {
						return nil, err
					}
				}
				if err := emit(Event{Type: "turn_end", Message: message, ToolResults: toolResults}); err != nil {
					return nil, err
				}
				return finish()
			}
			moreTools = false
			calls := toolCalls(message)
			if len(calls) > 0 {
				batch, err := executeBatch(ctx, current, message, calls, config, emit)
				if err != nil {
					return nil, err
				}
				toolResults, moreTools = batch.messages, !batch.terminate
				current.Messages = append(current.Messages, toolResults...)
				messages = append(messages, toolResults...)
			}
			last = &Turn{Message: message, ToolResults: toolResults, Context: current, NewMessages: messages}
			decision := ""
			if config.FinishTurn != nil {
				decision, err = config.FinishTurn(ctx, *last)
				if err != nil {
					return nil, err
				}
			}
			if err := emit(Event{Type: "turn_end", Message: message, ToolResults: toolResults}); err != nil {
				return nil, err
			}
			if decision == "end" {
				return finish()
			}
			explicit = decision == "continue"
			pending, err = poll(config.GetSteeringMessages)
			if err != nil {
				return nil, err
			}
			if moreTools || len(pending) > 0 {
				explicit = false
			}
		}
		followUp, err := poll(config.GetFollowUpMessages)
		if err != nil {
			return nil, err
		}
		if len(followUp) > 0 {
			explicit = false
			pending = followUp
			continue
		}
		if explicit {
			explicit = false
			continue
		}
		return finish()
	}
}

func applyUpdate(current *Context, config *Config, update *TurnUpdate) *Context {
	if update.Context != nil {
		current = update.Context
	}
	if update.Model != nil {
		config.Model = update.Model
	}
	if update.ThinkingLevel != nil {
		config.Reasoning = *update.ThinkingLevel
		if config.Reasoning == "off" {
			config.Reasoning = ""
		}
	}
	return current
}

func poll(fn func() ([]ai.Message, error)) ([]ai.Message, error) {
	if fn == nil {
		return nil, nil
	}
	return fn()
}

func thinkingLevel(reasoning string) string {
	if reasoning == "" {
		return "off"
	}
	return reasoning
}

func emitMessage(message *ai.Message, emit EventSink) error {
	if err := emit(Event{Type: "message_start", Message: message}); err != nil {
		return err
	}
	return emit(Event{Type: "message_end", Message: message})
}

func declareToolChanges(current Context, pending []ai.Message, now func() int64) []ai.Message {
	systemIndex := -1
	for i := len(pending) - 1; i >= 0; i-- {
		if pending[i].Role == "system" {
			systemIndex = i
			break
		}
	}
	baseline := pending
	if systemIndex >= 0 {
		baseline = slices.Clone(pending)
		baseline[systemIndex] = ai.WithToolChanges(pending[systemIndex], ai.ToolStateChanges{})
	}
	tools := make([]ai.Tool, len(current.Tools))
	for i, tool := range current.Tools {
		tools[i] = ai.ToToolDeclaration(tool.Tool)
	}
	changes := ai.GetToolStateChanges(ai.GetCurrentTools(append(slices.Clone(current.Messages), baseline...)), tools)
	unchanged := len(changes.ToolsAdded) == 0 && len(changes.ToolsRemoved) == 0
	if systemIndex >= 0 {
		if unchanged && len(pending[systemIndex].ToolsAdded) == 0 && len(pending[systemIndex].ToolsRemoved) == 0 {
			return pending
		}
		baseline[systemIndex] = ai.WithToolChanges(pending[systemIndex], changes)
		return baseline
	}
	if unchanged {
		return pending
	}
	update := ai.WithToolChanges(ai.Message{Role: "system", Content: ai.TextContent(""), Timestamp: now()}, changes)
	index := len(pending)
	for i, message := range pending {
		if message.Role != "system" {
			index = i
			break
		}
	}
	return slices.Insert(slices.Clone(pending), index, update)
}

func streamAssistant(ctx context.Context, current *Context, config Config, emit EventSink, stream StreamFn) (*ai.Message, error) {
	messages := current.Messages
	var err error
	if config.TransformContext != nil {
		messages, err = config.TransformContext(ctx, messages)
		if err != nil {
			return nil, err
		}
	}
	if config.ConvertToLLM == nil {
		return nil, errors.New("engine: convertToLLM is required")
	}
	messages, err = config.ConvertToLLM(messages)
	if err != nil {
		return nil, err
	}
	options := requestOptions(config)
	if config.GetAPIKey != nil {
		var model struct {
			Provider string `json:"provider"`
		}
		if err := json.Unmarshal(config.Model, &model); err != nil {
			return nil, err
		}
		key, err := config.GetAPIKey(model.Provider)
		if err != nil {
			return nil, err
		}
		if key != "" {
			options["apiKey"] = key
		}
	}
	response, err := stream(ctx, config.Model, ai.NormalizeContext(ai.Context{Messages: messages}), options)
	if err != nil {
		return nil, err
	}
	if response == nil {
		return nil, errors.New("engine: streamFn returned a nil stream")
	}
	return consumeAssistant(ctx, current, config, emit, response)
}

func requestOptions(config Config) map[string]any {
	options := make(map[string]any, len(config.Options)+2)
	for key, value := range config.Options {
		options[key] = value
	}
	// Pi spreads the loop config into provider options. Keep its serializable
	// model/execution fields too, even though model is also a positional arg.
	options["model"] = config.Model
	if config.ToolExecution != "" {
		options["toolExecution"] = config.ToolExecution
	} else {
		delete(options, "toolExecution")
	}
	if config.Reasoning == "" {
		delete(options, "reasoning")
	} else {
		options["reasoning"] = config.Reasoning
	}
	return options
}

func consumeAssistant(ctx context.Context, current *Context, config Config, emit EventSink, response *ai.AssistantMessageEventStream) (*ai.Message, error) {
	addedPartial, hasPartial := false, false
	settle := func() (*ai.Message, error) {
		message, err := response.SnapshotResult(context.WithoutCancel(ctx))
		if err != nil {
			return nil, err
		}
		if message == nil {
			return nil, errors.New("engine: stream has no final assistant message")
		}
		level := thinkingLevel(config.Reasoning)
		message.ThinkingLevel = &level
		if addedPartial {
			current.Messages[len(current.Messages)-1] = *message
		} else {
			current.Messages = append(current.Messages, *message)
			copy := *message
			if err := emit(Event{Type: "message_start", Message: &copy}); err != nil {
				return nil, err
			}
		}
		if err := emit(Event{Type: "message_end", Message: message}); err != nil {
			return nil, err
		}
		return message, nil
	}
	for {
		// Abort belongs to the provider. Drain its final aborted/error message;
		// cancelling this reader would truncate Pi's lifecycle.
		event, ok, err := response.Next(context.WithoutCancel(ctx))
		if err != nil {
			return nil, err
		}
		if !ok {
			break
		}
		event, err = response.SnapshotEvent(event)
		if err != nil {
			return nil, err
		}
		switch event.Type {
		case "start":
			if event.Partial == nil {
				return nil, errors.New("engine: start event has no partial message")
			}
			current.Messages = append(current.Messages, *event.Partial)
			addedPartial, hasPartial = true, true
			copy := *event.Partial
			if err := emit(Event{Type: "message_start", Message: &copy}); err != nil {
				return nil, err
			}
		case "text_start", "text_delta", "text_end", "thinking_start", "thinking_delta", "thinking_end", "toolcall_start", "toolcall_delta", "toolcall_end":
			if hasPartial {
				if event.Partial == nil {
					return nil, errors.New("engine: update event has no partial message")
				}
				current.Messages[len(current.Messages)-1] = *event.Partial
				copy := *event.Partial
				if err := emit(Event{Type: "message_update", Message: &copy, AssistantMessageEvent: &event}); err != nil {
					return nil, err
				}
			}
		case "done", "error":
			return settle()
		}
	}
	return settle()
}

func validateContinuation(initial Context) error {
	if len(initial.Messages) == 0 {
		return errors.New("Cannot continue: no messages in context")
	}
	if initial.Messages[len(initial.Messages)-1].Role == "assistant" {
		return errors.New("Cannot continue from message role: assistant")
	}
	return nil
}
