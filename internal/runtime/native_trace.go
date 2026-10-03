package agentruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/lohi-ai/agentray/agentcore/engine"
	"github.com/lohi-ai/agentray/ai"
	"github.com/lohi-ai/agentray/telemetry"
)

const nativeTraceTimeout = 5 * time.Second
const nativeTraceMaxBytes = 60 * 1024 * 1024

// A continuation is real tool work, but is not a new model run. Give it its
// own telemetry root and retain the same busy/cancellation lifetime as Prompt
// so Close cannot release the session lease while the tool is still running.
func (a *NativeAgent) observeDelegation(ctx context.Context, tool string, run func(context.Context) (json.RawMessage, error)) (raw json.RawMessage, err error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	a.mu.Lock()
	if a.closed || a.ctx.Err() != nil {
		a.mu.Unlock()
		return nil, errors.New("native agent closed")
	}
	if a.running != nil {
		a.mu.Unlock()
		return nil, errors.New("native agent is already processing")
	}
	runCtx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(a.ctx, cancel)
	done := make(chan struct{})
	a.running, a.runCancel = done, cancel
	a.mu.Unlock()
	defer func() {
		stop()
		cancel()
		a.mu.Lock()
		close(done)
		a.running, a.runCancel = nil, nil
		a.mu.Unlock()
	}()
	err = a.recorder.StartSpan(telemetry.SpanOptions{Name: "agentray.delegation.resume"}, func(root *telemetry.Span) error {
		var failure error
		raw, failure = telemetry.StartSpan(root.Context(), telemetry.SpanOptions{Name: "agentray.tool.execute", Attributes: telemetry.Attributes{"tool.name": tool}}, func(span *telemetry.Span) (json.RawMessage, error) {
			result, err := run(runCtx)
			var value struct{ IsError bool }
			if json.Unmarshal(result, &value) == nil && value.IsError {
				span.SetStatus(telemetry.SpanStatus{Status: "error"})
				root.SetStatus(telemetry.SpanStatus{Status: "error"})
			}
			return result, err
		})
		return failure
	})
	return raw, err
}

func (a *NativeAgent) traceNow() int64 {
	if a.config.Now != nil {
		return a.config.Now()
	}
	return time.Now().UnixMilli()
}

// The request result never waits for the trace sink. Run completion flushes the
// serial delivery queue while the run span is still open, as in the worker.
func (a *NativeAgent) observeRequest(ctx context.Context, model json.RawMessage, transcript ai.TranscriptContext, run func(*telemetry.Span) (*ai.Message, error)) (*ai.Message, error) {
	a.requests.Add(1)
	defer a.requests.Done()
	requestID := a.nextRequestID.Add(1)
	started := a.traceNow()
	var identity struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(model, &identity); err != nil {
		return nil, err
	}
	var requestContext json.RawMessage
	if a.traceRequests {
		requestContext = passiveNativeJSON(transcript)
	}
	response, err := telemetry.StartSpan(a.telemetryParent(), telemetry.SpanOptions{Name: "agentray.ai.request", Attributes: telemetry.Attributes{"model.id": identity.ID, "agentray.request.id": requestID}}, run)
	if a.traceRequests {
		a.recordRequest(ctx, requestID, started, model, requestContext, response, err)
	}
	return response, err
}

func passiveNativeJSON(value any) (raw json.RawMessage) {
	defer func() {
		if recover() != nil {
			raw = nil
		}
	}()
	encoded, err := json.Marshal(value)
	if err == nil {
		raw = encoded
	}
	return
}

func (a *NativeAgent) recordRequest(ctx context.Context, requestID uint64, started int64, model, requestContext json.RawMessage, response *ai.Message, failure error) {
	// Serialization and diagnostic inspection are passive too, including custom
	// error implementations supplied by a provider.
	defer func() { _ = recover() }()
	spans := a.recorder.GetSpans()
	selected := []telemetry.RecordedSpan{}
	included := map[int]bool{}
	for _, span := range spans {
		root := span.Name == "agentray.ai.request" && span.Attributes["agentray.request.id"] == requestID
		if root || (span.ParentID != nil && included[*span.ParentID]) {
			included[span.ID] = true
			selected = append(selected, span)
		}
	}
	packet := map[string]any{"requestID": requestID, "startedAtMs": started, "durationMs": a.traceNow() - started, "upstreamCommit": nativeAgentRevision, "model": model, "spans": selected}
	if attempt, ok := ctx.Value(nativeLadderAttemptKey{}).(nativeLadderAttemptScope); ok && attempt.owner != nil && attempt.rung >= 0 && attempt.rung < len(attempt.owner.rungs) {
		rung := attempt.owner.rungs[attempt.rung]
		packet["attempt"] = map[string]any{"rung": attempt.rung, "providerId": rung.providerID, "generation": attempt.generation, "pricingKnown": rung.pricingKnown}
	}
	if requestContext != nil {
		packet["context"] = requestContext
	}
	if response != nil {
		packet["response"] = response
	}
	if failure != nil {
		details := &telemetry.ErrorDetails{Name: "Error", Message: failure.Error()}
		var named *telemetry.ErrorDetails
		if errors.As(failure, &named) {
			details = named
		}
		packet["error"] = details
	}
	raw := passiveNativeJSON(packet)
	if len(raw) == 0 || len(raw) > nativeTraceMaxBytes {
		return
	}
	a.traceMu.Lock()
	previous := a.traceTail
	done := make(chan struct{})
	a.traceTail = done
	a.traceMu.Unlock()
	go func() {
		defer close(done)
		if previous != nil {
			<-previous
		}
		// Preserve trace/session attribution even when the request was aborted.
		delivery, cancel := context.WithTimeout(context.WithoutCancel(ctx), nativeTraceTimeout)
		stop := context.AfterFunc(a.ctx, cancel)
		defer stop()
		defer cancel()
		if a.ctx.Err() != nil {
			cancel()
		}
		if a.config.OnTrace != nil {
			_, _ = invokeNativeCallback(delivery, func(ctx context.Context, _ string, raw json.RawMessage, _ func(json.RawMessage) error) (json.RawMessage, error) {
				a.config.OnTrace(ctx, raw)
				return nil, nil
			}, "trace", raw, nil)
		} else {
			_, _ = invokeNativeCallback(delivery, a.config.Callback, "trace", raw, nil)
		}
	}()
}

func (a *NativeAgent) flushTraces() {
	a.requests.Wait()
	a.traceMu.Lock()
	tail := a.traceTail
	a.traceMu.Unlock()
	if tail != nil {
		<-tail
	}
}

// Return the provider's actual stream. A second event queue would change live
// partial-message identity and could lose mutations after the terminal event.
func (a *NativeAgent) nativeProviderStream(provider engine.StreamFn) engine.StreamFn {
	return a.observedProviderStream(provider, false)
}

// Host retries need the original admission error. The public Pi adapter keeps
// its pinned admission-error wording; only the internal attempt boundary opts in.
func (a *NativeAgent) observedProviderStream(provider engine.StreamFn, preserveAdmission bool) engine.StreamFn {
	return func(ctx context.Context, model json.RawMessage, transcript ai.TranscriptContext, options map[string]any) (*ai.AssistantMessageEventStream, error) {
		type admission struct {
			stream *ai.AssistantMessageEventStream
			err    error
		}
		admitted := make(chan admission, 1)
		var once sync.Once
		admit := func(stream *ai.AssistantMessageEventStream, err error) {
			once.Do(func() { admitted <- admission{stream, err} })
		}
		admitFailure := func(err error) {
			if !preserveAdmission {
				err = errors.New("Telemetry did not synchronously admit native stream")
			}
			admit(nil, err)
		}
		go func() {
			_, failure := a.observeRequest(ctx, model, transcript, func(span *telemetry.Span) (response *ai.Message, err error) {
				defer func() {
					if value := recover(); value != nil {
						response = nil
						err = fmt.Errorf("%v", value)
					}
				}()
				request := make(map[string]any, len(options)+1)
				for key, value := range options {
					request[key] = value
				}
				request["telemetryContext"] = span.Context()
				var selected struct{ API string }
				_ = json.Unmarshal(model, &selected)
				retention, _ := request["cacheRetention"].(string)
				if selected.API == "openai-codex-responses" && retention != "none" {
					if session, ok := request["sessionId"].(string); ok && session != "" {
						a.mu.Lock()
						if a.codexSessions == nil {
							a.codexSessions = map[string]struct{}{}
						}
						a.codexSessions[session] = struct{}{}
						a.mu.Unlock()
					}
				}
				stream, err := provider(ctx, model, transcript, request)
				if err == nil && stream == nil {
					err = errors.New("native Go provider returned no stream")
				}
				if err != nil {
					admitFailure(err)
					return nil, err
				}
				admit(stream, nil)
				response, err = stream.SnapshotResult(context.WithoutCancel(ctx))
				if response != nil && (response.StopReason == "error" || response.StopReason == "aborted") {
					span.SetStatus(telemetry.SpanStatus{Status: "error"})
				}
				return response, err
			})
			if failure != nil {
				admitFailure(failure)
			}
		}()
		result := <-admitted
		return result.stream, result.err
	}
}
