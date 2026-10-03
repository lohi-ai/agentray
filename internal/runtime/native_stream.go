package agentruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/lohi-ai/agentray/agentcore"
	"github.com/lohi-ai/agentray/agentcore/engine"
	"github.com/lohi-ai/agentray/ai"
	"github.com/lohi-ai/agentray/telemetry"
)

// NativeCallbackStream adapts the existing lossless host callback contract to
// the Go engine directly. It preserves progressive events and the independent
// callback result, without a worker process or a legacy Message projection.
// The supplied telemetry context is the explicit parent for every request.
func NativeCallbackStream(callback agentcore.PiCallback, parent telemetry.Context) engine.StreamFn {
	return nativeCallbackStream(callback, func() telemetry.Context { return parent })
}

func nativeCallbackStream(callback agentcore.PiCallback, parent func() telemetry.Context) engine.StreamFn {
	var nextRequestID atomic.Uint64
	return nativeObservedCallbackStream(callback, func(ctx context.Context, model json.RawMessage, transcript ai.TranscriptContext, run func(*telemetry.Span) (*ai.Message, error)) (*ai.Message, error) {
		var identity struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(model, &identity); err != nil {
			return nil, err
		}
		return telemetry.StartSpan(parent(), telemetry.SpanOptions{Name: "agentray.ai.request", Attributes: telemetry.Attributes{"model.id": identity.ID, "agentray.request.id": nextRequestID.Add(1)}}, run)
	})
}

type nativeRequestObserver func(context.Context, json.RawMessage, ai.TranscriptContext, func(*telemetry.Span) (*ai.Message, error)) (*ai.Message, error)

func nativeObservedCallbackStream(callback agentcore.PiCallback, observe nativeRequestObserver) engine.StreamFn {
	return func(ctx context.Context, model json.RawMessage, transcript ai.TranscriptContext, options map[string]any) (*ai.AssistantMessageEventStream, error) {
		wireOptions := make(map[string]any, len(options))
		for key, value := range options {
			switch key {
			case "signal", "telemetryContext", "onPayload", "onResponse", "onProviderStreamEvent":
				continue
			}
			wireOptions[key] = value
		}
		params, err := json.Marshal(struct {
			Model   json.RawMessage      `json:"model"`
			Context ai.TranscriptContext `json:"context"`
			Options map[string]any       `json:"options"`
		}{model, transcript, wireOptions})
		if err != nil {
			return nil, err
		}
		completed := make(chan struct{})
		var result *ai.Message
		var failure error
		wait := func(waitCtx context.Context) (*ai.Message, error) {
			select {
			case <-completed:
				return result, failure
			default:
			}
			select {
			case <-completed:
				return result, failure
			case <-waitCtx.Done():
				return nil, waitCtx.Err()
			}
		}
		stream := ai.NewAssistantMessageEventStreamWithHooks(ai.AssistantStreamHooks{Result: wait, AfterEnd: func(waitCtx context.Context) error {
			// The iterator may finish at a terminal event before the callback
			// settles. Only result() unconditionally waits for that promise.
			select {
			case <-completed:
				return failure
			default:
				return nil
			}
		}})
		// State is shared only with in-flight progress callbacks. The same lock
		// fences settlement so a late emit cannot publish another frame.
		var progressMu sync.Mutex
		var terminal *ai.Message
		started, settled := false, false
		forward := func(event ai.AssistantMessageEvent) {
			if terminal != nil {
				return
			}
			started = true
			if event.Type == "done" {
				terminal = event.Message
			}
			if event.Type == "error" {
				terminal = event.Error
			}
			stream.Push(event)
		}
		emit := func(raw json.RawMessage) error {
			progressMu.Lock()
			defer progressMu.Unlock()
			if settled || ctx.Err() != nil {
				return context.Canceled
			}
			var event ai.AssistantMessageEvent
			if err := json.Unmarshal(raw, &event); err != nil {
				return err
			}
			forward(event)
			return nil
		}
		admitted := make(chan struct{})
		go func() {
			defer stream.End()
			result, failure = observe(ctx, model, transcript, func(span *telemetry.Span) (*ai.Message, error) {
				close(admitted)
				raw, err := invokeNativeCallback(ctx, callback, "stream", params, emit)
				progressMu.Lock()
				defer progressMu.Unlock()
				settled = true
				if err != nil {
					return nil, err
				}
				message := terminal
				if message == nil {
					var header struct {
						Role string `json:"role"`
					}
					if len(raw) == 0 || json.Unmarshal(raw, &header) != nil || header.Role != "assistant" {
						return nil, errors.New("stream callback must return an assistant message")
					}
					message = new(ai.Message)
					if err := json.Unmarshal(raw, message); err != nil {
						return nil, err
					}
				}
				if message.Role != "assistant" {
					return nil, errors.New("stream callback must return an assistant message")
				}
				if message.StopReason == "pending" {
					return nil, errors.New("stream callback returned an unfinished assistant message")
				}
				if terminal == nil {
					if !started {
						forward(ai.AssistantMessageEvent{Type: "start", Partial: message})
					}
					if message.StopReason == "error" || message.StopReason == "aborted" {
						forward(ai.AssistantMessageEvent{Type: "error", Reason: message.StopReason, Error: message})
					} else {
						forward(ai.AssistantMessageEvent{Type: "done", Reason: message.StopReason, Message: message})
					}
				}
				if message.StopReason == "error" || message.StopReason == "aborted" {
					span.SetStatus(telemetry.SpanStatus{Status: "error"})
				}
				return message, nil
			})
			close(completed)
		}()
		// Pi admits its telemetry callback synchronously, before returning the
		// stream. Preserve that ordering while the request itself runs in Go.
		select {
		case <-admitted:
		case <-completed:
		}
		return stream, nil
	}
}

// Cancellation ends the logical callback wait without claiming a host effect
// physically completed. The host receives the same cancelled context and may
// finish cleanup later. No result/progress after logical settlement is admitted.
func invokeNativeCallback(ctx context.Context, callback agentcore.PiCallback, method string, params json.RawMessage, emit func(json.RawMessage) error) (json.RawMessage, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if callback == nil {
		return nil, fmt.Errorf("no Pi callback handler for %s", method)
	}
	type outcome struct {
		value json.RawMessage
		err   error
	}
	completed := make(chan outcome, 1)
	var progressMu sync.Mutex
	settled := false
	progress := func(value json.RawMessage) error {
		progressMu.Lock()
		defer progressMu.Unlock()
		if settled || ctx.Err() != nil {
			return context.Canceled
		}
		if emit == nil {
			return nil
		}
		return emit(value)
	}
	go func() {
		value, err := func() (value json.RawMessage, err error) {
			defer func() {
				if recovered := recover(); recovered != nil {
					err = fmt.Errorf("Pi callback panic: %v", recovered)
				}
			}()
			return callback(ctx, method, params, progress)
		}()
		progressMu.Lock()
		settled = true
		progressMu.Unlock()
		if err == nil && len(value) > 0 && !json.Valid(value) {
			err = errors.New("Pi callback returned invalid JSON")
		}
		var wireError *agentcore.PiError
		if errors.As(err, &wireError) {
			err = &telemetry.ErrorDetails{Name: wireError.Name, Message: wireError.Message}
		}
		completed <- outcome{value, err}
	}()
	select {
	case result := <-completed:
		return result.value, result.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
