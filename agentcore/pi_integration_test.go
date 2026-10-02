//go:build pi

package agentcore_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lohi-ai/agentray/agentcore"
)

func piJSON(value any) json.RawMessage {
	b, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return b
}

func piAssistant(content any, stop string) json.RawMessage {
	return piJSON(map[string]any{
		"role": "assistant", "content": content, "api": "test", "provider": "test", "model": "test",
		"stopReason": stop, "timestamp": 100,
		"usage": map[string]any{"input": 1, "output": 1, "cacheRead": 0, "cacheWrite": 0, "totalTokens": 2,
			"cost": map[string]any{"input": 0, "output": 0, "cacheRead": 0, "cacheWrite": 0, "total": 0}},
	})
}

func piText(text string) json.RawMessage {
	return piAssistant([]any{map[string]any{"type": "text", "text": text}}, "stop")
}

func piToolResult(text string, terminate bool) json.RawMessage {
	return piJSON(map[string]any{"content": []any{map[string]any{"type": "text", "text": text}}, "details": nil, "terminate": terminate})
}

func piTool(name string) map[string]any {
	return map[string]any{"name": name, "label": name, "description": name,
		"parameters": map[string]any{"type": "object", "properties": map[string]any{}, "additionalProperties": false}}
}

func piStart(t *testing.T, cfg agentcore.PiConfig) (*agentcore.PiAgent, context.Context) {
	t.Helper()
	worker, err := filepath.Abs("../third_party/pi/dist/worker.mjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(worker); err != nil {
		t.Fatal("Build the real Pi worker first: cd third_party/pi && bun run build")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	cfg.Worker = worker
	cfg.Stderr = os.Stderr
	a, err := agentcore.NewPi(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	if a.UpstreamCommit() != "eeac84ca92498ac18b6832754d01aef1d3c5f654" {
		t.Fatalf("wrong runtime revision: %s", a.UpstreamCommit())
	}
	return a, ctx
}

func TestPiPromptStreamsAndSettlesAwaitedEvents(t *testing.T) {
	var a *agentcore.PiAgent
	var mu sync.Mutex
	var events []string
	var busyAtEnd bool
	var eventErr error
	var ctx context.Context
	a, ctx = piStart(t, agentcore.PiConfig{
		Callback: func(_ context.Context, method string, _ json.RawMessage, emit func(json.RawMessage) error) (json.RawMessage, error) {
			if method != "stream" {
				return nil, fmt.Errorf("unexpected callback %s", method)
			}
			for _, frame := range []any{
				map[string]any{"type": "start", "partial": piText("")},
				map[string]any{"type": "text_start", "contentIndex": 0, "partial": piText("")},
				map[string]any{"type": "text_delta", "contentIndex": 0, "delta": "hello", "partial": piText("hello")},
				map[string]any{"type": "text_end", "contentIndex": 0, "content": "hello", "partial": piText("hello")},
			} {
				if err := emit(piJSON(frame)); err != nil {
					return nil, err
				}
			}
			return piText("hello"), nil
		},
		OnEvent: func(eventCtx context.Context, raw json.RawMessage) error {
			var ev struct{ Type string }
			_ = json.Unmarshal(raw, &ev)
			mu.Lock()
			defer mu.Unlock()
			events = append(events, ev.Type)
			if ev.Type == "agent_end" {
				// A synchronous state request from a callback must not deadlock
				// the reader, and Pi is still busy until this callback settles.
				state, err := a.State(eventCtx)
				eventErr = err
				var s struct{ IsStreaming bool }
				_ = json.Unmarshal(state, &s)
				busyAtEnd = s.IsStreaming
			}
			return nil
		},
	})
	if err := a.Prompt(ctx, piJSON("hello")); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if eventErr != nil || !busyAtEnd || len(events) == 0 || events[0] != "agent_start" || events[len(events)-1] != "agent_end" || !slices.Contains(events, "message_update") {
		t.Fatalf("lifecycle: busy=%v err=%v events=%v", busyAtEnd, eventErr, events)
	}
	state, err := a.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var s struct {
		IsStreaming bool
		Messages    []json.RawMessage
	}
	if err := json.Unmarshal(state, &s); err != nil || s.IsStreaming || len(s.Messages) != 2 {
		t.Fatalf("unexpected state %s: %v", state, err)
	}
	spans, err := a.Call(ctx, "telemetry", nil)
	if err != nil {
		t.Fatal(err)
	}
	var records []struct {
		ID       int
		ParentID *int
		Name     string
		Settled  bool
	}
	if err := json.Unmarshal(spans, &records); err != nil || len(records) != 2 || records[1].ParentID == nil || *records[1].ParentID != records[0].ID || !records[0].Settled || !records[1].Settled {
		t.Fatalf("telemetry tree: %s, %v", spans, err)
	}
}

func TestPiParallelToolsCompleteOutOfOrderAndPersistInSourceOrder(t *testing.T) {
	var streams atomic.Int32
	secondDone := make(chan struct{})
	var mu sync.Mutex
	var ended []string
	options := piJSON(map[string]any{"initialState": map[string]any{"tools": []any{piTool("first"), piTool("second")}}})
	a, ctx := piStart(t, agentcore.PiConfig{Options: options,
		Callback: func(ctx context.Context, method string, params json.RawMessage, emit func(json.RawMessage) error) (json.RawMessage, error) {
			if method == "stream" {
				streams.Add(1)
				return piAssistant([]any{
					map[string]any{"type": "toolCall", "id": "a", "name": "first", "arguments": map[string]any{}},
					map[string]any{"type": "toolCall", "id": "b", "name": "second", "arguments": map[string]any{}},
				}, "toolUse"), nil
			}
			if method != "tool" {
				return nil, fmt.Errorf("unexpected callback %s", method)
			}
			var call struct{ ToolName string }
			_ = json.Unmarshal(params, &call)
			if call.ToolName == "first" {
				select {
				case <-secondDone:
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			}
			if err := emit(piToolResult("progress", false)); err != nil {
				return nil, err
			}
			return piToolResult(call.ToolName, true), nil
		},
		OnEvent: func(_ context.Context, raw json.RawMessage) error {
			var event struct{ Type, ToolName string }
			_ = json.Unmarshal(raw, &event)
			if event.Type == "tool_execution_end" {
				mu.Lock()
				ended = append(ended, event.ToolName)
				mu.Unlock()
				if event.ToolName == "second" {
					close(secondDone)
				}
			}
			return nil
		},
	})
	if err := a.Prompt(ctx, piJSON("tools")); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(ended, []string{"second", "first"}) || streams.Load() != 1 {
		t.Fatalf("completion order=%v provider calls=%d", ended, streams.Load())
	}
	state, _ := a.State(ctx)
	var s struct {
		Messages []struct{ Role, ToolCallID string }
	}
	_ = json.Unmarshal(state, &s)
	var ids []string
	for _, m := range s.Messages {
		if m.Role == "toolResult" {
			ids = append(ids, m.ToolCallID)
		}
	}
	if !slices.Equal(ids, []string{"a", "b"}) {
		t.Fatalf("transcript order %v: %s", ids, state)
	}
}

func TestPiCancellationBusyGuardAndReuse(t *testing.T) {
	started := make(chan struct{})
	cancelled := make(chan struct{})
	var count atomic.Int32
	a, ctx := piStart(t, agentcore.PiConfig{Callback: func(ctx context.Context, _ string, _ json.RawMessage, _ func(json.RawMessage) error) (json.RawMessage, error) {
		if count.Add(1) == 1 {
			close(started)
			<-ctx.Done()
			close(cancelled)
			return nil, ctx.Err()
		}
		return piText("next run"), nil
	}})
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- a.Prompt(runCtx, piJSON("wait")) }()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if err := a.Prompt(ctx, piJSON("busy")); err == nil {
		t.Fatal("accepted a concurrent prompt")
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel result: %v", err)
	}
	select {
	case <-cancelled:
	case <-ctx.Done():
		t.Fatal("host provider was not cancelled")
	}
	if _, err := a.Call(ctx, "waitForIdle", nil); err != nil {
		t.Fatal(err)
	}
	if err := a.Prompt(ctx, piJSON("again")); err != nil {
		t.Fatal(err)
	}
	if count.Load() != 2 {
		t.Fatalf("provider count %d", count.Load())
	}
	_ = a.Close()
	_ = a.Close()
	if _, err := a.State(ctx); err == nil {
		t.Fatal("closed worker accepted a call")
	}
}

func TestPiHookReplacementAndErrorIsolation(t *testing.T) {
	var tools atomic.Int32
	options := piJSON(map[string]any{"callbacks": []string{"beforeToolCall"}, "initialState": map[string]any{"tools": []any{piTool("blocked")}}})
	a, ctx := piStart(t, agentcore.PiConfig{Options: options, Callback: func(_ context.Context, method string, _ json.RawMessage, _ func(json.RawMessage) error) (json.RawMessage, error) {
		switch method {
		case "stream":
			return piAssistant([]any{map[string]any{"type": "toolCall", "id": "blocked", "name": "blocked", "arguments": map[string]any{}}}, "toolUse"), nil
		case "beforeToolCall":
			return piJSON(map[string]any{"block": true, "reason": "policy denied", "terminate": true}), nil
		case "tool":
			tools.Add(1)
		}
		return nil, errors.New("unexpected callback")
	}})
	if err := a.Prompt(ctx, piJSON("try")); err != nil {
		t.Fatal(err)
	}
	if tools.Load() != 0 {
		t.Fatal("blocked tool executed")
	}
	if _, err := a.Call(ctx, "setState", piJSON(map[string]any{"isStreaming": false})); err == nil {
		t.Fatal("accepted mutation of runtime-owned state")
	}
	if _, err := a.Call(ctx, "unknown", nil); err == nil {
		t.Fatal("accepted unknown method")
	}
	if _, err := a.State(ctx); err != nil {
		t.Fatalf("bad command killed the worker: %v", err)
	}
}

func TestPiCallbackFailuresSettleThroughUpstream(t *testing.T) {
	for _, kind := range []string{"error", "panic", "invalid JSON"} {
		t.Run(kind, func(t *testing.T) {
			a, ctx := piStart(t, agentcore.PiConfig{Callback: func(context.Context, string, json.RawMessage, func(json.RawMessage) error) (json.RawMessage, error) {
				switch kind {
				case "panic":
					panic("provider panic")
				case "invalid JSON":
					return json.RawMessage(`{"unfinished":`), nil
				default:
					return nil, &agentcore.PiError{Name: "ProviderError", Message: "provider failed"}
				}
			}})
			// Pi represents an admitted run's provider failure in state rather
			// than rejecting prompt(). The bridge must preserve that contract.
			if err := a.Prompt(ctx, piJSON("fail")); err != nil {
				t.Fatal(err)
			}
			state, err := a.State(ctx)
			if err != nil {
				t.Fatal(err)
			}
			var s struct {
				IsStreaming  bool
				ErrorMessage string
				Messages     []struct{ StopReason string }
			}
			if err := json.Unmarshal(state, &s); err != nil || s.IsStreaming || s.ErrorMessage == "" || len(s.Messages) != 2 || s.Messages[1].StopReason != "error" {
				t.Fatalf("unsettled failure: %s, %v", state, err)
			}
		})
	}
}

func TestPiCloseCancelsOutstandingCallback(t *testing.T) {
	started, stopped := make(chan struct{}), make(chan struct{})
	a, ctx := piStart(t, agentcore.PiConfig{Callback: func(ctx context.Context, _ string, _ json.RawMessage, _ func(json.RawMessage) error) (json.RawMessage, error) {
		close(started)
		<-ctx.Done()
		close(stopped)
		return nil, ctx.Err()
	}})
	done := make(chan error, 1)
	go func() { done <- a.Prompt(ctx, piJSON("wait")) }()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-stopped:
	case <-ctx.Done():
		t.Fatal("Close left a live callback")
	}
	if err := <-done; err == nil {
		t.Fatal("closing the worker reported a successful run")
	}
}
