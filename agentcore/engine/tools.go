package engine

import (
	"context"
	"encoding/json"
	"errors"
	"sync"

	"github.com/lohi-ai/agentray/ai"
	"github.com/lohi-ai/agentray/internal/jsonjs"
)

type preparedCall struct {
	call      *ai.ContentBlock
	tool      *Tool
	args      any
	immediate *ToolOutcome
}

// RunToolCall applies the same preparation, validation and hooks as a model
// call, without emitting lifecycle events or appending transcript messages.
// The input call, hook call and outcome call retain the same pointer.
// The selected tool object survives replacement of its entry in tools.
// Results and updates retain the tool's pointers unless a hook overrides or fails.
// An update callback's panic propagates synchronously to the executing tool;
// a returned error represents a rejected update promise and rejects the call
// after execution settles. Tools that launch goroutines own their panic boundary.
func RunToolCall(ctx context.Context, call *ai.ContentBlock, tools []*Tool, assistant *ai.Message, current *Context, hooks ToolHooks, update func(*ToolResult) error) (ToolOutcome, error) {
	prepared := prepareCall(ctx, call, tools, assistant, current, hooks)
	if prepared.immediate != nil {
		return *prepared.immediate, nil
	}
	return executeCall(ctx, prepared, assistant, current, hooks, update)
}

func toolCalls(message *ai.Message) []*ai.ContentBlock {
	calls := []*ai.ContentBlock{}
	for i := range message.Content.Blocks {
		block := message.Content.Blocks[i]
		if block.Type == "toolCall" {
			calls = append(calls, block)
		}
	}
	return calls
}

func errorResult(message string) *ToolResult {
	return &ToolResult{Content: []*ai.ContentBlock{{Type: "text", Text: message}}, Details: NewObject()}
}

func prepareCall(ctx context.Context, call *ai.ContentBlock, tools []*Tool, assistant *ai.Message, current *Context, hooks ToolHooks) (prepared preparedCall) {
	prepared.call = call
	fail := func(message string, terminate bool) {
		result := errorResult(message)
		if terminate {
			result.Terminate = &terminate
		}
		prepared.immediate = &ToolOutcome{ToolCall: call, Result: result, IsError: true}
	}
	defer func() {
		if value := recover(); value != nil {
			fail(failureError(value).Error(), false)
		}
	}()
	for i := range tools {
		if tools[i].Name == call.Name {
			prepared.tool = tools[i]
			break
		}
	}
	if prepared.tool == nil {
		fail("Tool "+call.Name+" not found", false)
		return
	}
	args := call.Arguments
	var err error
	if prepared.tool.PrepareArguments != nil {
		args, err = prepared.tool.PrepareArguments(args)
		if err != nil {
			fail(err.Error(), false)
			return
		}
	}
	validated, err := validateArguments(prepared.tool.Tool, args)
	if err != nil {
		fail(err.Error(), false)
		return
	}
	prepared.args = validated
	if hooks.Before != nil {
		before := BeforeToolCall{AssistantMessage: assistant, ToolCall: call, Args: prepared.args, Context: current}
		decision, err := hooks.Before(ctx, &before)
		if err != nil {
			fail(err.Error(), false)
			return
		}
		if ctx.Err() != nil {
			fail("Operation aborted", false)
			return
		}
		if decision != nil && decision.Block {
			reason := decision.Reason
			if reason == "" {
				reason = "Tool execution was blocked"
			}
			fail(reason, decision.Terminate)
			return
		}
	}
	if ctx.Err() != nil {
		fail("Operation aborted", false)
	}
	return
}

func executeCall(ctx context.Context, prepared preparedCall, assistant *ai.Message, current *Context, hooks ToolHooks, update func(*ToolResult) error) (ToolOutcome, error) {
	// Updates belong to one Execute invocation. Closing admission before waiting
	// ensures retained callbacks cannot create late events in a later tool call.
	var mu sync.Mutex
	pending, nextUpdate := 0, 0
	accepting := true
	var updateErr error
	failureIndex := -1
	failed, settled := make(chan struct{}), make(chan struct{})
	onUpdate := func(partial *ToolResult) {
		mu.Lock()
		if !accepting {
			mu.Unlock()
			return
		}
		index := nextUpdate
		nextUpdate++
		pending++
		mu.Unlock()
		var err error
		defer func() {
			mu.Lock()
			defer mu.Unlock()
			pending--
			if err != nil && (updateErr == nil || index < failureIndex) {
				if updateErr == nil {
					close(failed)
				}
				updateErr, failureIndex = err, index
			}
			if !accepting && pending == 0 {
				close(settled)
			}
		}()
		if update != nil {
			// Like Pi's Promise.resolve(onUpdate(...)), a synchronous throw
			// happens before the update promise is admitted. Let the executing
			// tool catch it, or invoke turn it into an ordinary tool failure.
			err = update(partial)
		}
	}
	result, err := invoke(prepared.tool, ctx, prepared.call.ID, prepared.args, onUpdate)
	mu.Lock()
	accepting = false
	if pending == 0 {
		close(settled)
	}
	mu.Unlock()
	// Promise.all rejects without waiting for unfinished siblings. When
	// multiple updates already failed, registration order determines the
	// failure; Pi registers the update promises in admission order. A rejected
	// update also takes precedence over an execution failure.
	select {
	case <-failed:
	case <-settled:
	}
	mu.Lock()
	pendingError := updateErr
	mu.Unlock()
	if pendingError != nil {
		return ToolOutcome{}, pendingError
	}
	if err == nil && result == nil {
		// A Go nil result corresponds to a JS null result. Pi catches the
		// property-read failure after all admitted updates have settled.
		err = errors.New("null is not an object (evaluating 'result.isError')")
	}
	isError := false
	if err != nil {
		result, isError = errorResult(err.Error()), true
	} else {
		isError = result.IsError != nil && *result.IsError
	}
	if hooks.After != nil {
		override, err := afterCall(hooks.After, ctx, AfterToolCall{
			BeforeToolCall: BeforeToolCall{AssistantMessage: assistant, ToolCall: prepared.call, Args: prepared.args, Context: current},
			Result:         result, IsError: isError,
		})
		if err != nil {
			result, isError = errorResult(err.Error()), true
		} else if override != nil {
			// Pi spreads the executed result whenever an override is returned,
			// including when the hook returns the very same result object.
			copy := *result
			result = &copy
			if !jsonjs.IsNullish(override.StructuredContent) {
				result.StructuredContent = override.StructuredContent
			} else if override.Content != nil {
				result.StructuredContent = nil
			}
			if override.Content != nil {
				result.Content = override.Content
			}
			if !jsonjs.IsNullish(override.Details) {
				result.Details = override.Details
			}
			if override.Usage != nil {
				result.Usage = override.Usage
			}
			if override.Terminate != nil {
				result.Terminate = override.Terminate
			}
			if override.IsError != nil {
				isError = *override.IsError
			}
		}
	}
	return ToolOutcome{ToolCall: prepared.call, Result: result, IsError: isError}, nil
}

func invoke(tool *Tool, ctx context.Context, id string, args any, update func(*ToolResult)) (result *ToolResult, err error) {
	defer func() {
		if value := recover(); value != nil {
			err = failureError(value)
		}
	}()
	return tool.Execute(ctx, id, args, update)
}

func afterCall(hook func(context.Context, AfterToolCall) (*AfterToolResult, error), ctx context.Context, call AfterToolCall) (result *AfterToolResult, err error) {
	defer func() {
		if value := recover(); value != nil {
			err = failureError(value)
		}
	}()
	return hook(ctx, call)
}

type toolBatch struct {
	messages  []*ai.Message
	terminate bool
}

func executeBatch(ctx context.Context, current *Context, assistant *ai.Message, calls []*ai.ContentBlock, config Config, emit EventSink) (toolBatch, error) {
	batch := toolBatch{messages: []*ai.Message{}}
	sequential := config.ToolExecution == "sequential"
	for _, call := range calls {
		for _, tool := range current.Tools {
			if tool.Name == call.Name {
				sequential = sequential || tool.ExecutionMode == "sequential"
				break
			}
		}
	}
	finalized := []ToolOutcome{}
	prepared := []preparedCall{}
	appendResult := func(outcome ToolOutcome) error {
		message := ai.Message{Role: "toolResult", ToolCallID: outcome.ToolCall.ID, ToolName: outcome.ToolCall.Name,
			Content: ai.BlockReferences(nonnil(outcome.Result.Content)...), Details: outcome.Result.Details, Usage: outcome.Result.Usage, IsError: outcome.IsError, Timestamp: config.now()}
		if outcome.Result.Usage == nil && outcome.Result.preserved["usage"] != nil {
			message.Extra = map[string]json.RawMessage{"usage": outcome.Result.preserved["usage"]}
		}
		if err := emitMessage(&message, emit); err != nil {
			return err
		}
		batch.messages = append(batch.messages, &message)
		return nil
	}
	end := func(outcome ToolOutcome) error {
		// Replacing event.Result or event.IsError is local to the event;
		// mutating the pointed-to result survives into later batch callbacks.
		return emit(Event{Type: "tool_execution_end", ToolCallID: outcome.ToolCall.ID, ToolName: outcome.ToolCall.Name, Result: outcome.Result, IsError: outcome.IsError})
	}
	run := func(call preparedCall) (ToolOutcome, error) {
		return executeCall(ctx, call, assistant, current, config.ToolHooks, func(result *ToolResult) error {
			return emit(Event{Type: "tool_execution_update", ToolCallID: call.call.ID, ToolName: call.call.Name, Args: call.call.Arguments, PartialResult: result})
		})
	}
	for _, call := range calls {
		if err := emit(Event{Type: "tool_execution_start", ToolCallID: call.ID, ToolName: call.Name, Args: call.Arguments}); err != nil {
			return batch, err
		}
		if assistant.StopReason == "length" {
			outcome := ToolOutcome{ToolCall: call, IsError: true, Result: errorResult("Tool call \"" + call.Name + "\" was not executed: the response hit the output token limit, so its arguments may be truncated. Re-issue the tool call with complete arguments.")}
			if err := end(outcome); err != nil {
				return batch, err
			}
			if err := appendResult(outcome); err != nil {
				return batch, err
			}
			continue
		}
		preparation := prepareCall(ctx, call, current.Tools, assistant, current, config.ToolHooks)
		if preparation.immediate != nil {
			outcome := *preparation.immediate
			if err := end(outcome); err != nil {
				return batch, err
			}
			finalized = append(finalized, outcome)
			prepared = append(prepared, preparation)
			if sequential {
				if err := appendResult(outcome); err != nil {
					return batch, err
				}
			}
		} else if sequential {
			outcome, err := run(preparation)
			if err != nil {
				return batch, err
			}
			if err := end(outcome); err != nil {
				return batch, err
			}
			if err := appendResult(outcome); err != nil {
				return batch, err
			}
			finalized = append(finalized, outcome)
		} else {
			prepared = append(prepared, preparation)
			finalized = append(finalized, ToolOutcome{})
		}
		if ctx.Err() != nil {
			break
		}
	}
	if assistant.StopReason == "length" {
		return batch, nil
	}
	if !sequential {
		var group sync.WaitGroup
		failures := make(chan error, 1)
		for i, call := range prepared {
			if call.immediate != nil {
				continue
			}
			group.Add(1)
			go func(i int, call preparedCall) {
				defer group.Done()
				defer func() {
					if value := recover(); value != nil {
						select {
						case failures <- failureError(value):
						default:
						}
					}
				}()
				var outcome ToolOutcome
				var err error
				if ctx.Err() != nil {
					outcome = ToolOutcome{ToolCall: call.call, Result: errorResult("Operation aborted"), IsError: true}
				} else {
					outcome, err = run(call)
				}
				if err == nil {
					err = end(outcome)
				}
				finalized[i] = outcome
				if err != nil {
					select {
					case failures <- err:
					default:
					}
				}
			}(i, call)
		}
		settled := make(chan struct{})
		go func() { group.Wait(); close(settled) }()
		// Promise.all rejects on the first failure. It neither publishes a
		// prefix of result messages nor cancels already-running sibling tools.
		select {
		case err := <-failures:
			return batch, err
		case <-settled:
			select {
			case err := <-failures:
				return batch, err
			default:
			}
		}
		for _, outcome := range finalized {
			if err := appendResult(outcome); err != nil {
				return batch, err
			}
		}
	}
	batch.terminate = len(finalized) > 0
	for _, outcome := range finalized {
		batch.terminate = batch.terminate && outcome.Result.Terminate != nil && *outcome.Result.Terminate
	}
	return batch, nil
}
