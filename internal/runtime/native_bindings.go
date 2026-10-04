package agentruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/lohi-ai/agentray/agentcore/engine"
	"github.com/lohi-ai/agentray/agentcore/plugins/ask"
	"github.com/lohi-ai/agentray/ai"
	"github.com/lohi-ai/agentray/internal/jsonjs"
	"github.com/lohi-ai/agentray/telemetry"
)

func (a *NativeAgent) invoke(ctx context.Context, method string, params any) (json.RawMessage, error) {
	raw, err := json.Marshal(params)
	if err != nil {
		return nil, err
	}
	return invokeNativeCallback(ctx, a.config.Callback, method, raw, nil)
}

func (a *NativeAgent) bindOptions(raw json.RawMessage) (engine.AgentOptions, error) {
	options := engine.AgentOptions{}
	if len(raw) == 0 {
		raw = json.RawMessage(`{}`)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return options, errors.New("Pi options must be an object")
	}
	var wire struct {
		InitialState struct {
			SystemPrompt  json.RawMessage
			Model         json.RawMessage
			ThinkingLevel string
			Tools         json.RawMessage
			Messages      []*ai.Message
		}
		Callbacks                                            []string
		StreamMode                                           string
		StreamOptions                                        json.RawMessage
		TraceRequests                                        bool
		SteeringMode, FollowUpMode, Transport, ToolExecution string
		SessionID                                            *string
		ThinkingBudgets                                      json.RawMessage
		MaxRetryDelayMS                                      *int
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		return options, err
	}
	a.traceRequests = wire.TraceRequests || a.config.OnTrace != nil
	mode := wire.StreamMode
	if mode == "" {
		mode = "callback"
	}
	if mode != "callback" && mode != "native" {
		return options, fmt.Errorf("Unknown stream mode: %s", mode)
	}
	streamOptions := map[string]any{}
	if wire.StreamOptions != nil {
		if err := json.Unmarshal(wire.StreamOptions, &streamOptions); err != nil || streamOptions == nil {
			return options, errors.New("streamOptions must be an object")
		}
	}
	for _, name := range []string{"signal", "telemetryContext", "apiKey", "fetch", "onPayload", "onResponse", "onProviderStreamEvent"} {
		if _, ok := streamOptions[name]; ok {
			return options, fmt.Errorf("Stream option %s belongs to the native runtime or its callback", name)
		}
	}
	tools, err := a.bindTools(wire.InitialState.Tools)
	if err != nil {
		return options, err
	}
	var systemPrompt string
	if len(wire.InitialState.SystemPrompt) > 0 {
		systemPrompt, err = nativeString(wire.InitialState.SystemPrompt)
		if err != nil {
			return options, err
		}
	}
	options.InitialState = engine.InitialState{SystemPrompt: systemPrompt, Model: wire.InitialState.Model, ThinkingLevel: wire.InitialState.ThinkingLevel, Tools: tools, Messages: wire.InitialState.Messages}
	if nativeNull(options.InitialState.Model) {
		options.InitialState.Model = nil
	}
	options.AgentConfig = engine.AgentConfig{SteeringMode: wire.SteeringMode, FollowUpMode: wire.FollowUpMode, Transport: wire.Transport, SessionID: wire.SessionID, ThinkingBudgets: wire.ThinkingBudgets, MaxRetryDelayMS: wire.MaxRetryDelayMS}
	options.ToolExecution = wire.ToolExecution
	options.Now = a.config.Now
	stream := nativeObservedCallbackStream(a.config.Callback, a.observeRequest)
	if mode == "native" {
		provider := a.config.StreamFn
		if provider == nil {
			if options.InitialState.Model != nil {
				if err := validateNativeProviderModel(options.InitialState.Model); err != nil {
					return options, err
				}
			}
			provider = NativeProviderStream
		}
		stream = a.nativeProviderStream(provider)
	}
	mergeOptions := func(request map[string]any) map[string]any {
		merged := make(map[string]any, len(streamOptions)+len(request))
		for key, value := range streamOptions {
			merged[key] = value
		}
		for key, value := range request {
			merged[key] = value
		}
		return merged
	}
	options.StreamFn = func(ctx context.Context, model json.RawMessage, transcript ai.TranscriptContext, request map[string]any) (*ai.AssistantMessageEventStream, error) {
		return stream(ctx, model, transcript, mergeOptions(request))
	}
	if a.config.admitRequest != nil {
		if mode != "native" {
			return options, errors.New("native request admission requires native stream mode")
		}
		options.AdmitRequest = func(ctx context.Context, request engine.Request, options map[string]any) (*engine.RequestAdmission, error) {
			return a.config.admitRequest(ctx, request, mergeOptions(options))
		}
	}
	for _, name := range wire.Callbacks {
		switch name {
		case "convertToLlm":
			options.ConvertToLLM = func(messages *engine.MessageList) (*engine.MessageList, error) {
				raw, err := a.invoke(a.ctx, "convertToLlm", messages)
				if err != nil {
					return nil, err
				}
				var converted *engine.MessageList
				err = json.Unmarshal(raw, &converted)
				return converted, err
			}
		case "transformContext":
			options.TransformContext = func(ctx context.Context, messages *engine.MessageList) (*engine.MessageList, error) {
				raw, err := a.invoke(ctx, "transformContext", messages)
				if err != nil {
					return nil, err
				}
				var transformed *engine.MessageList
				err = json.Unmarshal(raw, &transformed)
				return transformed, err
			}
		case "getApiKey":
			options.GetAPIKey = func(provider string) (string, error) {
				raw, err := a.invoke(a.callbackContext(), "getApiKey", provider)
				if err != nil {
					return "", err
				}
				if nativeNull(raw) {
					return "", nil
				}
				var key string
				err = json.Unmarshal(raw, &key)
				return key, err
			}
		case "beforeToolCall":
			options.Before = func(ctx context.Context, call *engine.BeforeToolCall) (*engine.BeforeToolResult, error) {
				raw, err := a.invoke(ctx, "beforeToolCall", nativeToolCall(*call))
				if err != nil || nativeNull(raw) {
					return nil, err
				}
				var result engine.BeforeToolResult
				err = json.Unmarshal(raw, &result)
				return &result, err
			}
		case "afterToolCall":
			options.After = func(ctx context.Context, call engine.AfterToolCall) (*engine.AfterToolResult, error) {
				params := nativeToolCall(call.BeforeToolCall)
				params["result"], params["isError"] = call.Result, call.IsError
				raw, err := a.invoke(ctx, "afterToolCall", params)
				if err != nil || nativeNull(raw) {
					return nil, err
				}
				var result engine.AfterToolResult
				err = json.Unmarshal(raw, &result)
				return &result, err
			}
		case "finishTurn":
			options.FinishTurn = func(ctx context.Context, turn *engine.Turn) (string, error) {
				raw, err := a.invoke(ctx, "finishTurn", nativeTurn(turn))
				if err != nil || nativeNull(raw) {
					return "", err
				}
				var decision struct {
					Action string `json:"action"`
				}
				err = json.Unmarshal(raw, &decision)
				return decision.Action, err
			}
		case "prepareRequest":
			options.PrepareRequest = func(ctx context.Context, request engine.Request) (*engine.TurnUpdate, error) {
				raw, err := a.invoke(ctx, "prepareRequest", map[string]any{"context": nativeContext(request.Context), "model": request.Model, "thinkingLevel": request.ThinkingLevel})
				if err != nil {
					return nil, err
				}
				return a.bindUpdate(raw)
			}
		case "prepareNextTurn":
			options.AgentConfig.PrepareNextTurn = func(ctx context.Context) (*engine.TurnUpdate, error) {
				raw, err := a.invoke(ctx, "prepareNextTurn", nil)
				if err != nil {
					return nil, err
				}
				return a.bindUpdate(raw)
			}
		case "prepareNextTurnWithContext":
			options.PrepareNextTurnWithContext = func(ctx context.Context, turn *engine.Turn) (*engine.TurnUpdate, error) {
				raw, err := a.invoke(ctx, "prepareNextTurnWithContext", nativeTurn(turn))
				if err != nil {
					return nil, err
				}
				return a.bindUpdate(raw)
			}
		case "onPayload":
			if options.Options == nil {
				options.Options = map[string]any{}
			}
			options.Options["onPayload"] = func(ctx context.Context, payload, model json.RawMessage) (json.RawMessage, error) {
				return a.invoke(ctx, "onPayload", map[string]any{"payload": payload, "model": model})
			}
		default:
			return options, fmt.Errorf("Unsupported callback: %s", name)
		}
	}
	return options, nil
}

func nativeContext(c *engine.Context) map[string]any {
	if c == nil {
		return nil
	}
	messages := c.Messages
	if messages == nil {
		messages = engine.NewList[*ai.Message]()
	}
	tools := c.Tools
	if tools == nil {
		tools = engine.NewList[*engine.Tool]()
	}
	return map[string]any{"messages": messages, "tools": tools}
}
func nativeTurn(turn *engine.Turn) map[string]any {
	return map[string]any{"message": turn.Message, "toolResults": turn.ToolResults, "context": nativeContext(turn.Context), "newMessages": turn.NewMessages}
}
func nativeToolCall(call engine.BeforeToolCall) map[string]any {
	return map[string]any{"assistantMessage": call.AssistantMessage, "toolCall": call.ToolCall, "args": call.Args, "context": nativeContext(call.Context)}
}
func nativeNull(raw json.RawMessage) bool {
	return len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

func (a *NativeAgent) bindUpdate(raw json.RawMessage) (*engine.TurnUpdate, error) {
	if nativeNull(raw) {
		return nil, nil
	}
	var update struct {
		Context *struct {
			Messages []*ai.Message
			Tools    json.RawMessage
		}
		Messages      []*ai.Message
		Model         json.RawMessage
		ThinkingLevel *string
	}
	if err := json.Unmarshal(raw, &update); err != nil {
		return nil, err
	}
	result := &engine.TurnUpdate{Messages: engine.NewList(update.Messages...), Model: update.Model, ThinkingLevel: update.ThinkingLevel}
	if update.Context != nil {
		tools, err := a.bindTools(update.Context.Tools)
		if err != nil {
			return nil, err
		}
		result.Context = &engine.Context{Messages: engine.NewList(update.Context.Messages...), Tools: engine.NewList(tools...)}
	}
	return result, nil
}

func (a *NativeAgent) bindTools(raw json.RawMessage) ([]*engine.Tool, error) {
	tools := []*engine.Tool{}
	if nativeNull(raw) {
		return tools, nil
	}
	var definitions []json.RawMessage
	if err := json.Unmarshal(raw, &definitions); err != nil {
		return nil, err
	}
	for _, definition := range definitions {
		var declaration ai.Tool
		if err := json.Unmarshal(definition, &declaration); err != nil {
			return nil, err
		}
		var fields struct {
			Label, Replay, ExecutionMode string
			OutputSchema                 json.RawMessage
			PrepareArguments             json.RawMessage
			AgentrayPrepareArguments     json.RawMessage
		}
		if err := json.Unmarshal(definition, &fields); err != nil {
			return nil, err
		}
		if len(fields.PrepareArguments) > 0 && !bytes.Equal(fields.PrepareArguments, []byte("false")) && !nativeNull(fields.PrepareArguments) {
			return nil, errors.New("prepareArguments requires a Go tool binding")
		}
		tool := &engine.Tool{Tool: declaration, Label: fields.Label, Replay: fields.Replay, ExecutionMode: fields.ExecutionMode, OutputSchema: fields.OutputSchema}
		for _, key := range []string{"label", "replay", "executionMode", "outputSchema", "execute", "agentrayPrepareArguments"} {
			delete(tool.Extra, key)
		}
		if fields.AgentrayPrepareArguments != nil {
			var preparer string
			if json.Unmarshal(fields.AgentrayPrepareArguments, &preparer) != nil || preparer != "ask-v1" {
				return nil, fmt.Errorf("Unknown native argument preparer: %s", fields.AgentrayPrepareArguments)
			}
			tool.PrepareArguments = func(args json.RawMessage) (json.RawMessage, error) {
				return json.RawMessage((ask.Tool{}).PrepareArguments(string(args))), nil
			}
		}
		tool.Execute = func(ctx context.Context, id string, args any, onUpdate func(*engine.ToolResult)) (*engine.ToolResult, error) {
			return telemetry.StartSpan(a.telemetryParent(), telemetry.SpanOptions{Name: "agentray.tool.execute", Attributes: telemetry.NewAttributes(telemetry.Property{Name: "tool.name", Value: declaration.Name})}, func(span *telemetry.Span) (*engine.ToolResult, error) {
				encodedArgs, err := jsonjs.MarshalValue(args)
				if err != nil {
					return nil, err
				}
				params, err := json.Marshal(map[string]any{"toolCallId": id, "toolName": declaration.Name, "args": json.RawMessage(encodedArgs)})
				if err != nil {
					return nil, err
				}
				raw, err := invokeNativeCallback(ctx, a.config.Callback, "tool", params, func(raw json.RawMessage) error {
					var update *engine.ToolResult
					if err := json.Unmarshal(raw, &update); err != nil {
						return err
					}
					onUpdate(update)
					return nil
				})
				if err != nil {
					return nil, err
				}
				var result *engine.ToolResult
				if err := json.Unmarshal(raw, &result); err != nil {
					return nil, err
				}
				if result != nil && result.IsError != nil && *result.IsError {
					span.SetStatus(telemetry.SpanStatus{Status: "error"})
				}
				return result, nil
			})
		}
		tools = append(tools, tool)
	}
	return tools, nil
}
