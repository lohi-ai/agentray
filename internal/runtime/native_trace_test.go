package agentruntime

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lohi-ai/agentray/agentcore"
	"github.com/lohi-ai/agentray/telemetry/llm"
	"github.com/lohi-ai/agentray/ai"
	"github.com/lohi-ai/agentray/telemetry"
)

func TestNativeDelegationTelemetrySettlesWithToolOutcome(t *testing.T) {
	for _, mode := range []string{"success", "tool error", "callback error"} {
		t.Run(mode, func(t *testing.T) {
			agent, err := NewNativeAgent(context.Background(), NativeAgentConfig{Options: json.RawMessage(`{"initialState":{}}`)})
			if err != nil {
				t.Fatal(err)
			}
			defer agent.Close()
			failure := errors.New("callback failed")
			calls := 0
			_, err = agent.observeDelegation(context.Background(), "delegate", func(context.Context) (json.RawMessage, error) {
				calls++
				switch mode {
				case "tool error":
					return json.RawMessage(`{"isError":true}`), nil
				case "callback error":
					return nil, failure
				default:
					return json.RawMessage(`{"isError":false}`), nil
				}
			})
			if calls != 1 || (mode == "callback error" && !errors.Is(err, failure)) || (mode != "callback error" && err != nil) {
				t.Fatalf("telemetry changed callback outcome: calls=%d err=%v", calls, err)
			}
			spans := agent.recorder.GetSpans()
			if len(spans) != 2 || spans[0].Name != "agentray.delegation.resume" || spans[0].ParentID != nil || spans[1].Name != "agentray.tool.execute" || spans[1].ParentID == nil || *spans[1].ParentID != spans[0].ID {
				t.Fatalf("incorrect continuation ancestry: %+v", spans)
			}
			want := "ok"
			if mode != "success" {
				want = "error"
			}
			for _, span := range spans {
				if !span.Settled || span.Status.Status != want {
					t.Fatalf("unsettled/wrong status: %+v", span)
				}
			}
		})
	}
}

func TestNativeDelegationCloseCancelsAndWaitsForCleanup(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	agent, err := NewNativeAgent(ctx, NativeAgentConfig{Options: json.RawMessage(`{"initialState":{}}`)})
	if err != nil {
		t.Fatal(err)
	}
	entered, cancelled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer func() { unblock(); _ = agent.Close() }()
	result := make(chan error, 1)
	go func() {
		_, err := agent.observeDelegation(ctx, "delegate", func(ctx context.Context) (json.RawMessage, error) {
			close(entered)
			<-ctx.Done()
			close(cancelled)
			<-release
			return nil, ctx.Err()
		})
		result <- err
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("continuation did not start")
	}
	if err := agent.Prompt(ctx, json.RawMessage(`"overlap"`)); err == nil {
		t.Fatal("model prompt overlapped an active continuation")
	}
	if _, err := agent.observeDelegation(ctx, "overlap", func(context.Context) (json.RawMessage, error) {
		t.Error("overlapping callback executed")
		return nil, nil
	}); err == nil {
		t.Fatal("second continuation was admitted")
	}
	closed := make(chan error, 1)
	go func() { closed <- agent.Close() }()
	select {
	case <-cancelled:
	case <-ctx.Done():
		t.Fatal("Close did not cancel the continuation")
	}
	select {
	case <-closed:
		t.Fatal("Close returned before callback cleanup")
	default:
	}
	unblock()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("wrong cancellation outcome: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("continuation did not settle")
	}
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("Close did not settle")
	}
	for _, span := range agent.recorder.GetSpans() {
		if !span.Settled || span.Status.Status != "error" {
			t.Fatalf("Close left an unfinished continuation span: %+v", span)
		}
	}
}

func TestNativeTraceDeliveryIsSerialAndDoesNotBlockRequests(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	first := make(chan struct{})
	release := make(chan struct{})
	ended := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	var delivered []int
	var requests atomic.Int32
	agent, err := NewNativeAgent(ctx, NativeAgentConfig{
		Options: json.RawMessage(`{"callbacks":["finishTurn"]}`),
		Callback: func(_ context.Context, method string, _ json.RawMessage, _ func(json.RawMessage) error) (json.RawMessage, error) {
			if method == "stream" {
				requests.Add(1)
				return json.RawMessage(nativeStreamReply), nil
			}
			if method == "finishTurn" && requests.Load() == 1 {
				return json.RawMessage(`{"action":"continue"}`), nil
			}
			return nil, nil
		},
		OnTrace: func(_ context.Context, raw json.RawMessage) {
			var packet struct{ RequestID int }
			_ = json.Unmarshal(raw, &packet)
			if packet.RequestID == 1 {
				close(first)
				<-release
			}
			delivered = append(delivered, packet.RequestID)
		},
		OnEvent: func(_ context.Context, raw json.RawMessage) error {
			var event struct{ Type string }
			_ = json.Unmarshal(raw, &event)
			if event.Type == "agent_end" {
				close(ended)
			}
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer agent.Close()
	result := make(chan error, 1)
	go func() { result <- agent.Prompt(ctx, json.RawMessage(`"test"`)) }()
	for _, ready := range []<-chan struct{}{first, ended} {
		select {
		case <-ready:
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	if requests.Load() != 2 {
		t.Fatal("trace delivery blocked the second request")
	}
	select {
	case <-result:
		t.Fatal("prompt returned before trace flush")
	default:
	}
	if spans := agent.recorder.GetSpans(); spans[0].Settled {
		t.Fatal("run span settled before trace delivery")
	}
	unblock()
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(delivered, []int{1, 2}) {
		t.Fatalf("delivery order: %v", delivered)
	}
}

func TestNativeTraceSinkPanicAndTimeoutArePassive(t *testing.T) {
	for _, mode := range []string{"panic", "timeout"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()
			release := make(chan struct{})
			returned := make(chan struct{})
			var started atomic.Bool
			agent, err := NewNativeAgent(ctx, NativeAgentConfig{
				Callback: func(context.Context, string, json.RawMessage, func(json.RawMessage) error) (json.RawMessage, error) {
					return json.RawMessage(nativeStreamReply), nil
				},
				OnTrace: func(ctx context.Context, _ json.RawMessage) {
					started.Store(true)
					defer close(returned)
					if mode == "panic" {
						panic("sink failed")
					}
					// Ignore cancellation to exercise the bounded logical wait.
					<-release
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			defer agent.Close()
			before := time.Now()
			err = agent.Prompt(ctx, json.RawMessage(`"test"`))
			elapsed := time.Since(before)
			close(release)
			<-returned
			if err != nil {
				t.Fatal(err)
			}
			if !started.Load() {
				t.Fatal("missing trace delivery")
			}
			if mode == "timeout" && (elapsed < nativeTraceTimeout || elapsed > 7*time.Second) {
				t.Fatalf("unbounded/early timeout: %v", elapsed)
			}
			if state := agent.agent.State(); state.ErrorMessage != nil {
				t.Fatalf("trace changed agent outcome: %s", *state.ErrorMessage)
			}
			for _, span := range agent.recorder.GetSpans() {
				if span.Status.Status != "ok" || !span.Settled {
					t.Fatalf("trace changed span: %+v", span)
				}
			}
		})
	}
}

func TestNativeTraceProviderStreamIdentityAndSnapshot(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var trace json.RawMessage
	agent, err := NewNativeAgent(ctx, NativeAgentConfig{OnTrace: func(_ context.Context, raw json.RawMessage) { trace = raw }, Now: func() int64 { return 100 }})
	if err != nil {
		t.Fatal(err)
	}
	defer agent.Close()
	agent.parent = agent.recorder.Context
	original := ai.NewAssistantMessageEventStream()
	var message ai.Message
	if err := json.Unmarshal([]byte(nativeStreamReply), &message); err != nil {
		t.Fatal(err)
	}
	transcript := ai.NormalizeContext(ai.Context{Messages: []ai.Message{{Role: "user", Content: ai.TextContent("before"), Timestamp: 100}}})
	childStarted := make(chan struct{})
	childRelease := make(chan struct{})
	childEnded := make(chan struct{})
	var once sync.Once
	finish := func() { once.Do(func() { close(childRelease) }); <-childEnded }
	defer finish()
	stream, err := agent.nativeProviderStream(func(_ context.Context, _ json.RawMessage, request ai.TranscriptContext, options map[string]any) (*ai.AssistantMessageEventStream, error) {
		request.Messages()[0].Content = ai.TextContent("changed by provider")
		parent := options["telemetryContext"].(telemetry.Context)
		go func() {
			defer close(childEnded)
			_ = parent.StartSpan(telemetry.SpanOptions{Name: "provider.active", Attributes: telemetry.NewAttributes(
				telemetry.Property{Name: "nonfinite", Value: math.NaN()}, telemetry.Property{Name: "bytes", Value: []byte{0, 255}}, telemetry.Property{Name: "numbers", Value: []any{int8(1), float64(2.5)}},
			)}, func(*telemetry.Span) error { close(childStarted); <-childRelease; return nil })
		}()
		<-childStarted
		original.Push(ai.AssistantMessageEvent{Type: "done", Reason: "stop", Message: &message})
		original.End()
		return original, nil
	})(ctx, json.RawMessage(`{"id":"test"}`), transcript, map[string]any{"apiKey": "private-key"})
	if err != nil {
		t.Fatal(err)
	}
	if stream != original {
		t.Fatal("provider stream was replaced")
	}
	agent.flushTraces()
	var packet struct {
		Context  ai.Context
		Spans    []telemetry.RecordedSpan
		Response ai.Message
	}
	if err := json.Unmarshal(trace, &packet); err != nil {
		t.Fatal(err)
	}
	if string(passiveNativeJSON(packet.Context.Messages[0].Content)) != `"before"` {
		t.Fatalf("context was not captured before provider: %s", trace)
	}
	if strings.Contains(string(trace), "private-key") {
		t.Fatal("options leaked into trace")
	}
	if len(packet.Spans) != 2 || !packet.Spans[0].Settled || packet.Spans[1].Settled || packet.Spans[1].Name != "provider.active" {
		t.Fatalf("active descendant snapshot lost: %s", trace)
	}
	attrs := packet.Spans[1].Attributes
	if value, exists := attrs.Lookup("nonfinite"); !exists || value != nil || !reflect.DeepEqual(attrs.Get("bytes").(*telemetry.Array).Values(), []any{float64(0), float64(255)}) || !reflect.DeepEqual(attrs.Get("numbers").(*telemetry.Array).Values(), []any{float64(1), float64(2.5)}) {
		t.Fatalf("numeric attributes lost or changed in delivered trace: %s", trace)
	}
	saved := append([]byte(nil), trace...)
	original.Synchronize(func() { message.Model = "later mutation" })
	finish()
	if !reflect.DeepEqual([]byte(trace), saved) {
		t.Fatal("delivered trace changed after settlement")
	}
}

func TestNativeSessionTraceUsesExistingSinkAndAttribution(t *testing.T) {
	ctx := agentcore.WithDelegationDepth(agentcore.WithRunSession(llm.WithTraceID(context.Background(), "trace-run"), "parent/child"), 2)
	var records []llm.TraceRecord
	cfg := bindPiTrace(NativeAgentConfig{
		Callback: func(context.Context, string, json.RawMessage, func(json.RawMessage) error) (json.RawMessage, error) {
			return json.RawMessage(nativeStreamReply), nil
		},
	}, llm.SinkFunc(func(record llm.TraceRecord) { records = append(records, record) }), true, "fallback")
	session, err := NewPiSession(ctx, PiSessionConfig{Pi: cfg, Store: agentcore.NewMemorySessionStore(), SessionID: "parent/child"})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	if err := session.Prompt(ctx, json.RawMessage(`"test"`)); err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 {
		t.Fatalf("trace count: %d", len(records))
	}
	record := records[0]
	if record.TraceID != "trace-run" || record.SessionKey != "parent/child" || record.Depth != 2 || len(record.NativeTrace) == 0 || record.Err != "" {
		t.Fatalf("invalid trace: %+v", record)
	}
}

func TestNativeTraceProviderFailureAndPanicDoNotHangAdmission(t *testing.T) {
	for _, mode := range []string{"error", "panic", "invalid model"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			agent, err := NewNativeAgent(ctx, NativeAgentConfig{})
			if err != nil {
				t.Fatal(err)
			}
			defer agent.Close()
			provider := agent.nativeProviderStream(func(context.Context, json.RawMessage, ai.TranscriptContext, map[string]any) (*ai.AssistantMessageEventStream, error) {
				if mode == "panic" {
					panic("provider panic")
				}
				return nil, errors.New("provider failed")
			})
			model := json.RawMessage(`{"id":"test"}`)
			if mode == "invalid model" {
				model = json.RawMessage(`[]`)
			}
			done := make(chan error, 1)
			go func() { _, err := provider(ctx, model, ai.TranscriptContext{}, nil); done <- err }()
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("lost provider failure")
				}
			case <-ctx.Done():
				t.Fatal("provider admission hung")
			}
			agent.flushTraces()
		})
	}
}
