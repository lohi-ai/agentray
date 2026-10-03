package agentruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/lohi-ai/agentray/ai"
	"github.com/lohi-ai/agentray/telemetry"
	"os"
	"reflect"
	"sync"
	"testing"
	"time"
)

func TestNativeAgentMigrationOracle(t *testing.T) {
	data, err := os.ReadFile("testdata/native-agent.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		UpstreamCommit string
		Now            int64
		Cases          []struct {
			Input struct {
				Name    string
				Options json.RawMessage
				Actions []struct {
					Method string
					Params json.RawMessage
				}
				Responses                  []json.RawMessage
				StreamFailure, ToolFailure string
				ToolResult                 json.RawMessage
				ToolUpdates                []json.RawMessage
				HookResults                map[string][]json.RawMessage
				HookFailures               map[string]string
			}
			Expected json.RawMessage
		}
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	if fixture.UpstreamCommit != nativeAgentRevision || len(fixture.Cases) != 34 {
		t.Fatal("unexpected native agent oracle revision/coverage")
	}
	for _, tc := range fixture.Cases {
		t.Run(tc.Input.Name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			callbacks := []map[string]any{}
			events := []json.RawMessage{}
			traces := []json.RawMessage{}
			actions := []map[string]any{}
			indices := map[string]int{}
			var mu sync.Mutex
			config := NativeAgentConfig{Options: tc.Input.Options, Now: func() int64 { return fixture.Now }, Callback: func(_ context.Context, method string, params json.RawMessage, emit func(json.RawMessage) error) (json.RawMessage, error) {
				mu.Lock()
				if method == "trace" {
					traces = append(traces, append(json.RawMessage(nil), params...))
					mu.Unlock()
					return nil, nil
				}
				callbacks = append(callbacks, map[string]any{"method": method, "params": params})
				index := indices[method]
				indices[method] = index + 1
				mu.Unlock()
				switch method {
				case "stream":
					if tc.Input.StreamFailure != "" {
						return nil, errors.New(tc.Input.StreamFailure)
					}
					if index >= len(tc.Input.Responses) {
						return nil, errors.New("script exhausted")
					}
					return tc.Input.Responses[index], nil
				case "tool":
					for _, update := range tc.Input.ToolUpdates {
						if err := emit(update); err != nil {
							return nil, err
						}
					}
					if tc.Input.ToolFailure != "" {
						return nil, errors.New(tc.Input.ToolFailure)
					}
					return tc.Input.ToolResult, nil
				default:
					if failure := tc.Input.HookFailures[method]; failure != "" {
						return nil, errors.New(failure)
					}
					values := tc.Input.HookResults[method]
					if index < len(values) {
						return values[index], nil
					}
					return nil, nil
				}
			}, OnEvent: func(_ context.Context, event json.RawMessage) error {
				mu.Lock()
				defer mu.Unlock()
				events = append(events, event)
				return nil
			}}
			config.StreamFn = func(ctx context.Context, model json.RawMessage, transcript ai.TranscriptContext, options map[string]any) (*ai.AssistantMessageEventStream, error) {
				parent := options["telemetryContext"].(telemetry.Context)
				serializable := map[string]any{}
				for key, value := range options {
					if key != "telemetryContext" {
						serializable[key] = value
					}
				}
				params, err := json.Marshal(map[string]any{"model": model, "context": transcript, "options": serializable})
				if err != nil {
					return nil, err
				}
				mu.Lock()
				callbacks = append(callbacks, map[string]any{"method": "nativeStream", "params": json.RawMessage(params)})
				index := indices["nativeStream"]
				indices["nativeStream"] = index + 1
				mu.Unlock()
				if tc.Input.StreamFailure != "" {
					return nil, errors.New(tc.Input.StreamFailure)
				}
				_ = parent.StartSpan(telemetry.SpanOptions{Name: "provider.request", Attributes: telemetry.Attributes{"transport": "test"}}, func(*telemetry.Span) error { return nil })
				var message ai.Message
				if err := json.Unmarshal(tc.Input.Responses[index], &message); err != nil {
					return nil, err
				}
				stream := ai.NewAssistantMessageEventStream()
				stream.Push(ai.AssistantMessageEvent{Type: "start", Partial: &message})
				if message.StopReason == "error" || message.StopReason == "aborted" {
					stream.Push(ai.AssistantMessageEvent{Type: "error", Reason: message.StopReason, Error: &message})
				} else {
					stream.Push(ai.AssistantMessageEvent{Type: "done", Reason: message.StopReason, Message: &message})
				}
				stream.End()
				return stream, nil
			}
			agent, err := NewNativeAgent(ctx, config)
			var initError *string
			var state json.RawMessage
			spans := json.RawMessage(`[]`)
			if err != nil {
				message := err.Error()
				initError = &message
			} else {
				defer agent.Close()
				for _, action := range tc.Input.Actions {
					value, err := agent.Call(ctx, action.Method, action.Params)
					var failure *string
					if err != nil {
						message := err.Error()
						failure = &message
					}
					snapshot, err := agent.State(ctx)
					if err != nil {
						t.Fatal(err)
					}
					actions = append(actions, map[string]any{"method": action.Method, "value": value, "error": failure, "state": snapshot})
				}
				state, err = agent.State(ctx)
				if err != nil {
					t.Fatal(err)
				}
				spans, err = agent.Call(ctx, "telemetry", nil)
				if err != nil {
					t.Fatal(err)
				}
			}
			actual, err := json.Marshal(map[string]any{"initError": initError, "callbacks": callbacks, "events": events, "actions": actions, "state": state, "spans": spans, "traces": traces})
			if err != nil {
				t.Fatal(err)
			}
			var got, want any
			if err := json.Unmarshal(actual, &got); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(tc.Expected, &want); err != nil {
				t.Fatal(err)
			}
			if difference := nativeJSONDifference(got, want, ""); difference != "" {
				t.Fatal(difference)
			}
		})
	}
}

func nativeJSONDifference(got, want any, path string) string {
	if reflect.DeepEqual(got, want) {
		return ""
	}
	switch got := got.(type) {
	case map[string]any:
		if want, ok := want.(map[string]any); ok {
			for key, value := range got {
				other, exists := want[key]
				if !exists {
					return path + "/" + key + ": unexpected field"
				}
				if diff := nativeJSONDifference(value, other, path+"/"+key); diff != "" {
					return diff
				}
			}
			for key := range want {
				if _, exists := got[key]; !exists {
					return path + "/" + key + ": missing field"
				}
			}
		}
	case []any:
		if want, ok := want.([]any); ok {
			if len(got) != len(want) {
				return fmt.Sprintf("%s: length Go=%d worker=%d", path, len(got), len(want))
			}
			for i, value := range got {
				if diff := nativeJSONDifference(value, want[i], fmt.Sprintf("%s/%d", path, i)); diff != "" {
					return diff
				}
			}
		}
	}
	return fmt.Sprintf("%s: Go=%v worker=%v", path, got, want)
}

func TestNativeAgentRemainsBusyThroughEndSubscriber(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	finish := func() { once.Do(func() { close(release) }) }
	defer finish()
	agent, err := NewNativeAgent(ctx, NativeAgentConfig{Callback: func(context.Context, string, json.RawMessage, func(json.RawMessage) error) (json.RawMessage, error) {
		return json.RawMessage(nativeStreamReply), nil
	}, OnEvent: func(_ context.Context, raw json.RawMessage) error {
		var event struct{ Type string }
		_ = json.Unmarshal(raw, &event)
		if event.Type == "agent_end" {
			close(entered)
			select {
			case <-release:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer agent.Close()
	completed := make(chan error, 1)
	go func() { completed <- agent.Prompt(ctx, json.RawMessage(`"test"`)) }()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	for _, method := range []string{"prompt", "continue", "reset"} {
		if _, err := agent.Call(ctx, method, json.RawMessage(`{"input":"busy"}`)); err == nil {
			t.Errorf("%s admitted during end listener", method)
		}
	}
	snapshot, err := agent.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var state struct{ IsStreaming bool }
	_ = json.Unmarshal(snapshot, &state)
	if !state.IsStreaming {
		t.Fatal("became idle before subscriber settled")
	}
	finish()
	select {
	case err := <-completed:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}

func TestNativeAgentAbortKeepsFinalEventCallbacksLive(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	entered := make(chan struct{})
	var mu sync.Mutex
	ended := false
	agent, err := NewNativeAgent(ctx, NativeAgentConfig{Callback: func(ctx context.Context, _ string, _ json.RawMessage, _ func(json.RawMessage) error) (json.RawMessage, error) {
		close(entered)
		<-ctx.Done()
		return nil, ctx.Err()
	}, OnEvent: func(eventCtx context.Context, raw json.RawMessage) error {
		if eventCtx.Err() != nil {
			return errors.New("run abort cancelled event callback")
		}
		var event struct{ Type string }
		_ = json.Unmarshal(raw, &event)
		if event.Type == "agent_end" {
			mu.Lock()
			ended = true
			mu.Unlock()
		}
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer agent.Close()
	completed := make(chan error, 1)
	go func() { completed <- agent.Prompt(ctx, json.RawMessage(`"test"`)) }()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if _, err := agent.Call(ctx, "abort", nil); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-completed:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	raw, err := agent.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var state struct{ Messages []struct{ StopReason string } }
	_ = json.Unmarshal(raw, &state)
	if state.Messages[len(state.Messages)-1].StopReason != "aborted" {
		t.Fatalf("abort lifecycle missing: %s", raw)
	}
	mu.Lock()
	defer mu.Unlock()
	if !ended {
		t.Fatal("abort dropped final event")
	}
}
