package agentruntime

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lohi-ai/agentray/agentcore"
	"github.com/lohi-ai/agentray/agentcore/engine"
	"github.com/lohi-ai/agentray/ai"
	"github.com/lohi-ai/agentray/telemetry"
)

func TestNativeCallbackStreamMigrationOracle(t *testing.T) {
	data, err := os.ReadFile("testdata/native-stream.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixtures struct {
		UpstreamCommit string
		Model          json.RawMessage
		Context        ai.Context
		Cases          []struct {
			Input struct {
				Name, Failure, FailureName string
				Result                     json.RawMessage
				Events                     []json.RawMessage
				Options                    map[string]any
				SettleAfterIteration       bool
			}
			Expected json.RawMessage
		}
	}
	if err := json.Unmarshal(data, &fixtures); err != nil {
		t.Fatal(err)
	}
	if fixtures.UpstreamCommit != "eeac84ca92498ac18b6832754d01aef1d3c5f654" || len(fixtures.Cases) != 17 {
		t.Fatal("unexpected migration oracle revision/coverage")
	}
	for _, tc := range fixtures.Cases {
		t.Run(tc.Input.Name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			recorder := telemetry.NewInMemory()
			requests := []json.RawMessage{}
			release := make(chan struct{})
			var once sync.Once
			finish := func() { once.Do(func() { close(release) }) }
			defer finish()
			callback := func(ctx context.Context, method string, params json.RawMessage, emit func(json.RawMessage) error) (json.RawMessage, error) {
				if method != "stream" {
					return nil, errors.New("unexpected callback method")
				}
				requests = append(requests, params)
				for _, event := range tc.Input.Events {
					if err := emit(event); err != nil {
						return nil, err
					}
				}
				if tc.Input.SettleAfterIteration {
					select {
					case <-release:
					case <-ctx.Done():
						return nil, ctx.Err()
					}
				}
				if tc.Input.Failure != "" {
					if tc.Input.FailureName != "" {
						return nil, &agentcore.PiError{Name: tc.Input.FailureName, Message: tc.Input.Failure}
					}
					return nil, errors.New(tc.Input.Failure)
				}
				return tc.Input.Result, nil
			}
			stream, err := NativeCallbackStream(callback, recorder.Context)(ctx, fixtures.Model, ai.NormalizeContext(fixtures.Context), tc.Input.Options)
			if err != nil {
				t.Fatal(err)
			}
			events := []ai.AssistantMessageEvent{}
			var iterationError, resultError *string
			for {
				event, ok, err := stream.Next(ctx)
				if err != nil {
					message := err.Error()
					iterationError = &message
					break
				}
				if !ok {
					break
				}
				events = append(events, event)
			}
			finish()
			result, err := stream.Result(ctx)
			if err != nil {
				message := err.Error()
				resultError = &message
			}
			if err := stream.WaitForEnd(ctx); err != nil {
				t.Fatal(err)
			}
			actual, err := json.Marshal(map[string]any{"events": events, "requests": requests, "result": result, "iterationError": iterationError, "resultError": resultError, "spans": recorder.GetSpans()})
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
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("Go: %s\nWorker: %s", actual, tc.Expected)
			}
		})
	}
}

const nativeStreamModel = `{"id":"test","api":"test","provider":"test"}`
const nativeStreamReply = `{"role":"assistant","content":[{"type":"text","text":"done"}],"stopReason":"stop","api":"test","provider":"test","model":"test","timestamp":100,"usage":{"input":0,"output":0,"cacheRead":0,"cacheWrite":0,"totalTokens":0,"cost":{"input":0,"output":0,"cacheRead":0,"cacheWrite":0,"total":0}}}`

func TestNativeCallbackStreamProgressBeforeSettlement(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	release := make(chan struct{})
	var once sync.Once
	finish := func() { once.Do(func() { close(release) }) }
	defer finish()
	callback := func(ctx context.Context, _ string, _ json.RawMessage, emit func(json.RawMessage) error) (json.RawMessage, error) {
		if err := emit(json.RawMessage(`{"type":"start","partial":` + nativeStreamReply + `}`)); err != nil {
			return nil, err
		}
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return json.RawMessage(nativeStreamReply), nil
	}
	stream, err := NativeCallbackStream(callback, telemetry.Context{})(ctx, json.RawMessage(nativeStreamModel), ai.NormalizeContext(ai.Context{}), nil)
	if err != nil {
		t.Fatal(err)
	}
	event, ok, err := stream.Next(ctx)
	if err != nil || !ok || event.Type != "start" {
		t.Fatalf("progress was buffered until callback settlement: %v %v %+v", err, ok, event)
	}
	cancelled, stop := context.WithCancel(ctx)
	stop()
	if _, err := stream.Result(cancelled); !errors.Is(err, context.Canceled) {
		t.Fatalf("callback result settled early: %v", err)
	}
	finish()
	result, err := stream.Result(ctx)
	if err != nil || result.StopReason != "stop" {
		t.Fatalf("result: %v %+v", err, result)
	}
	if err := stream.WaitForEnd(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestNativeCallbackStreamCancellationFencesLateProgress(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	wait, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	entered, release := make(chan struct{}), make(chan struct{})
	late := make(chan error, 1)
	var once sync.Once
	finish := func() { once.Do(func() { close(release) }) }
	defer finish()
	failure := errors.New("late physical completion")
	callback := func(_ context.Context, _ string, _ json.RawMessage, emit func(json.RawMessage) error) (json.RawMessage, error) {
		close(entered)
		<-release
		late <- emit(json.RawMessage(`{"type":"start","partial":` + nativeStreamReply + `}`))
		return nil, failure
	}
	stream, err := NativeCallbackStream(callback, telemetry.Context{})(ctx, json.RawMessage(nativeStreamModel), ai.NormalizeContext(ai.Context{}), nil)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-wait.Done():
		t.Fatal(wait.Err())
	}
	cancel()
	if _, err := stream.Result(wait); !errors.Is(err, context.Canceled) {
		t.Fatalf("logical cancellation blocked on host: %v", err)
	}
	if err := stream.WaitForEnd(wait); err != nil {
		t.Fatal(err)
	}
	finish()
	select {
	case err := <-late:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("late emit admitted: %v", err)
		}
	case <-wait.Done():
		t.Fatal(wait.Err())
	}
	if _, err := stream.Result(wait); !errors.Is(err, context.Canceled) {
		t.Fatal("late failure replaced logical outcome")
	}
}

func TestNativeCallbackStreamErrorsReachGoAgent(t *testing.T) {
	for _, failure := range []string{"callback", "panic", "json", "after-terminal"} {
		t.Run(failure, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			callback := func(_ context.Context, _ string, _ json.RawMessage, emit func(json.RawMessage) error) (json.RawMessage, error) {
				switch failure {
				case "panic":
					panic("provider panic")
				case "json":
					return json.RawMessage(`{invalid`), nil
				case "after-terminal":
					if err := emit(json.RawMessage(`{"type":"done","reason":"stop","message":` + nativeStreamReply + `}`)); err != nil {
						return nil, err
					}
				}
				return nil, errors.New("provider callback failed")
			}
			agent, err := engine.NewAgent(engine.AgentOptions{AgentConfig: engine.AgentConfig{StreamFn: NativeCallbackStream(callback, telemetry.Context{})}})
			if err != nil {
				t.Fatal(err)
			}
			events := []string{}
			agent.Subscribe(&engine.Listener{Handle: func(_ context.Context, event engine.Event) error { events = append(events, event.Type); return nil }})
			if err := agent.Prompt(ctx, "test"); err != nil {
				t.Fatal(err)
			}
			state := agent.State()
			last := state.Messages[len(state.Messages)-1]
			if state.IsStreaming || last.StopReason != "error" || state.ErrorMessage == nil {
				t.Fatalf("failure lifecycle missing: %+v", state)
			}
			if events[len(events)-1] != "agent_end" {
				t.Fatalf("missing recovery agent_end: %v", events)
			}
			if failure == "after-terminal" && !strings.Contains(*state.ErrorMessage, "provider callback failed") {
				t.Fatal("terminal event masked callback failure")
			}
		})
	}
}

func TestNativeCallbackStreamTelemetryAdmissionAndRequestIDs(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	recorder := telemetry.NewInMemory()
	release := make(chan struct{})
	var once sync.Once
	finish := func() { once.Do(func() { close(release) }) }
	defer finish()
	var first *ai.AssistantMessageEventStream
	err := recorder.StartSpan(telemetry.SpanOptions{Name: "run"}, func(parent *telemetry.Span) error {
		factory := NativeCallbackStream(func(ctx context.Context, _ string, _ json.RawMessage, _ func(json.RawMessage) error) (json.RawMessage, error) {
			select {
			case <-release:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			return json.RawMessage(nativeStreamReply), nil
		}, parent.Context())
		var err error
		first, err = factory(ctx, json.RawMessage(nativeStreamModel), ai.NormalizeContext(ai.Context{}), nil)
		if err != nil {
			return err
		}
		spans := recorder.GetSpans()
		if len(spans) != 2 || spans[1].ParentID == nil || *spans[1].ParentID != spans[0].ID || spans[1].Settled {
			t.Fatal("request span was not admitted synchronously")
		}
		finish()
		if err := first.WaitForEnd(ctx); err != nil {
			return err
		}
		second, err := factory(ctx, json.RawMessage(nativeStreamModel), ai.NormalizeContext(ai.Context{}), nil)
		if err != nil {
			return err
		}
		return second.WaitForEnd(ctx)
	})
	if err != nil {
		t.Fatal(err)
	}
	spans := recorder.GetSpans()
	if len(spans) != 3 || spans[1].Attributes["agentray.request.id"] != uint64(1) || spans[2].Attributes["agentray.request.id"] != uint64(2) {
		t.Fatalf("request IDs lost: %+v", spans)
	}
}

func TestNativeCallbackProgressScopeEndsAtReturn(t *testing.T) {
	var saved func(json.RawMessage) error
	calls := 0
	value, err := invokeNativeCallback(context.Background(), func(_ context.Context, _ string, _ json.RawMessage, emit func(json.RawMessage) error) (json.RawMessage, error) {
		saved = emit
		return json.RawMessage(`true`), emit(json.RawMessage(`1`))
	}, "tool", nil, func(json.RawMessage) error { calls++; return nil })
	if err != nil || string(value) != "true" || calls != 1 {
		t.Fatalf("callback outcome: %s %v calls=%d", value, err, calls)
	}
	if err := saved(json.RawMessage(`2`)); !errors.Is(err, context.Canceled) || calls != 1 {
		t.Fatalf("late callback progress admitted: %v calls=%d", err, calls)
	}
}
