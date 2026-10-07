package agentruntime

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/2found/2ai/agentcore"
	"github.com/2found/2ai/ai"
)

func TestNativeRequestAdmissionFallbackToolContextUsageAndTraces(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ladder := testNativeLadder(t)
	calls := []int{}
	for i := range ladder.rungs {
		index := i
		ladder.rungs[i].stream = func(ctx context.Context, model json.RawMessage, transcript ai.TranscriptContext, options map[string]any) (*ai.AssistantMessageEventStream, error) {
			calls = append(calls, index)
			wantLevel, wantKey := "high", "secret-a-fresh"
			if index == 1 {
				wantLevel, wantKey = "low", "secret-b-fresh"
			}
			if options["reasoning"] != wantLevel || options["apiKey"] != wantKey || !nativeModelIdentityEqual(model, ladder.rungs[index].model) {
				t.Error("attempt used another candidate's options/model")
			}
			if !strings.Contains(string(piRequestJSON(transcript)), "provider-view") {
				t.Error("provider missed transform")
			}
			message := usageFixture("error", 2, 0.2)
			message.Model = "primary"
			if index == 1 {
				message = usageFixture("toolUse", 3, 0.3)
				message.Content = ai.BlockContent(ai.ContentBlock{Type: "toolCall", ID: "read-1", Name: "read", Arguments: json.RawMessage(`{}`)})
				if len(calls) == 3 {
					message = usageFixture("stop", 5, 0.5)
				}
				message.Model = "fallback"
			}
			return usageStream(ctx, message), nil
		}
	}
	binding, _, stream := ladder.sessionBinding()
	var options map[string]json.RawMessage
	_ = json.Unmarshal(binding.Options, &options)
	options["callbacks"] = piRequestJSON([]string{"prepareRequest", "transformContext", "getApiKey", "beforeToolCall"})
	binding.Options = piRequestJSON(options)
	original := binding.Callback
	prepares, transforms, tools := 0, 0, 0
	binding.Callback = func(ctx context.Context, method string, raw json.RawMessage, emit func(json.RawMessage) error) (json.RawMessage, error) {
		switch method {
		case "prepareRequest":
			prepares++
			var request struct {
				Context struct{ Messages []json.RawMessage }
				Model   struct{ ID string }
			}
			if err := json.Unmarshal(raw, &request); err != nil {
				return nil, err
			}
			level := "high"
			if request.Model.ID == "fallback" {
				level = "low"
			}
			messages := append(request.Context.Messages, piRequestJSON(map[string]string{"role": "user", "content": "prepared-" + request.Model.ID}))
			return piRequestJSON(map[string]any{"thinkingLevel": level, "context": map[string]any{"messages": messages, "tools": []any{map[string]any{"name": "read", "description": "candidate tool", "parameters": map[string]string{"type": "object"}}}}}), nil
		case "transformContext":
			transforms++
			var messages []json.RawMessage
			if err := json.Unmarshal(raw, &messages); err != nil {
				return nil, err
			}
			return piRequestJSON(append(messages, json.RawMessage(`{"role":"user","content":"provider-view"}`))), nil
		case "beforeToolCall":
			if !strings.Contains(string(raw), "prepared-fallback") || strings.Contains(string(raw), "provider-view") || strings.Contains(string(raw), "prepared-primary") {
				t.Error("tool callback received discarded or provider-only context", string(raw))
			}
			if ladder.selection().Rung != 1 {
				t.Error("tool admitted before model selection")
			}
		case "tool":
			tools++
			return json.RawMessage(`{"content":[{"type":"text","text":"read complete"}]}`), nil
		}
		return original(ctx, method, raw, emit)
	}
	var traceMu sync.Mutex
	var traces []json.RawMessage
	binding.OnTrace = func(_ context.Context, raw json.RawMessage) {
		traceMu.Lock()
		defer traceMu.Unlock()
		traces = append(traces, append(json.RawMessage(nil), raw...))
	}
	store := agentcore.NewMemorySessionStore()
	result, err := RunPi(ctx, PiRunConfig{Input: json.RawMessage(`"read and finish"`), Session: PiSessionConfig{Pi: binding, NativeStream: stream, nativeLadder: ladder, nativeAttempts: &agentcore.RetryPolicy{MaxAttempts: 1}, Policy: agentcore.NewAllowList("read"), Store: store, SessionID: "admission"}})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(calls, []int{0, 1, 1}) || prepares != 3 || transforms != 3 || tools != 1 {
		t.Fatalf("request pipeline repeated or bypassed: calls=%v prepare=%d transform=%d tool=%d", calls, prepares, transforms, tools)
	}
	if result.Projection.Usage.InputTokens != 10 || result.Projection.Usage.OutputTokens != 3 || result.Projection.Final != "same response" {
		t.Fatal("attempt usage or final result lost/doubled", result.Projection)
	}
	var state struct {
		Model    json.RawMessage
		Messages []ai.Message
	}
	if err = json.Unmarshal(result.State, &state); err != nil {
		t.Fatal(err)
	}
	if !nativeModelIdentityEqual(state.Model, ladder.rungs[1].model) {
		t.Fatal("selected model not checkpointed")
	}
	assistants := 0
	for _, message := range state.Messages {
		if message.Role == "assistant" {
			assistants++
			if message.Model != "fallback" || message.ThinkingLevel == nil || *message.ThinkingLevel != "low" || message.StopReason == "error" {
				t.Fatal("discarded candidate or stale reasoning entered history", message)
			}
		}
	}
	if assistants != 2 {
		t.Fatal("unexpected native assistant history", assistants)
	}
	traceMu.Lock()
	defer traceMu.Unlock()
	if len(traces) != 3 {
		t.Fatal("missing attempt trace or duplicate outer trace", len(traces))
	}
	entries, err := store.Log(ctx, "admission")
	if err != nil {
		t.Fatal(err)
	}
	selections, effects := 0, 0
	for _, entry := range entries {
		if entry.Kind == agentcore.EntryPiModelSelection {
			selections++
		}
		if entry.Kind == agentcore.EntryPiEffectStart {
			effects++
			if selections != 1 {
				t.Fatal("tool effect preceded durable model selection")
			}
		}
	}
	if selections != 1 || effects != 1 {
		t.Fatal("missing/duplicated selection or effect receipt", selections, effects)
	}
}

func TestNativeRequestAdmissionStreamsBeforeProviderSettlement(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ladder := testNativeLadder(t)
	producing := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	finish := func() { once.Do(func() { close(release) }) }
	defer finish()
	ladder.rungs[0].stream = func(ctx context.Context, _ json.RawMessage, _ ai.TranscriptContext, _ map[string]any) (*ai.AssistantMessageEventStream, error) {
		return usageStream(ctx, usageFixture("error", 1, 0)), nil
	}
	ladder.rungs[1].stream = func(ctx context.Context, _ json.RawMessage, _ ai.TranscriptContext, _ map[string]any) (*ai.AssistantMessageEventStream, error) {
		stream := ai.NewAssistantMessageEventStreamFor(ctx)
		go func() {
			defer stream.End()
			message := usageFixture("stop", 1, 0)
			stream.Push(ai.AssistantMessageEvent{Type: "start", Partial: message})
			stream.Push(ai.AssistantMessageEvent{Type: "text_delta", Partial: message, Delta: "live"})
			close(producing)
			select {
			case <-release:
			case <-ctx.Done():
			}
			stream.Push(ai.AssistantMessageEvent{Type: "done", Message: message})
		}()
		return stream, nil
	}
	binding, _, stream := ladder.sessionBinding()
	seen := make(chan struct{}, 1)
	binding.OnEvent = func(_ context.Context, raw json.RawMessage) error {
		var event struct{ Type string }
		_ = json.Unmarshal(raw, &event)
		if event.Type == "message_update" {
			select {
			case seen <- struct{}{}:
			default:
			}
		}
		return nil
	}
	session, err := NewPiSession(ctx, PiSessionConfig{Pi: binding, NativeStream: stream, nativeLadder: ladder, nativeAttempts: &agentcore.RetryPolicy{MaxAttempts: 1}})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	done := make(chan error, 1)
	go func() { done <- session.Prompt(ctx, json.RawMessage(`"stream"`)) }()
	select {
	case <-producing:
	case <-ctx.Done():
		t.Fatal("provider did not start")
	}
	select {
	case <-seen:
	case <-ctx.Done():
		t.Fatal("admission buffered a live response until settlement")
	}
	if ladder.selection().Rung != 1 {
		t.Fatal("event escaped before commit")
	}
	select {
	case err = <-done:
		t.Fatal("prompt settled before producer", err)
	default:
	}
	finish()
	if err = <-done; err != nil {
		t.Fatal(err)
	}
}

func TestNativeRequestAdmissionHostFailureDoesNotTraceOrEscalate(t *testing.T) {
	ladder := testNativeLadder(t)
	binding, _, stream := ladder.sessionBinding()
	failure := &agentcore.ProviderError{Status: 503, Message: "host preparation failed"}
	providers, traces := 0, 0
	for i := range ladder.rungs {
		ladder.rungs[i].stream = func(context.Context, json.RawMessage, ai.TranscriptContext, map[string]any) (*ai.AssistantMessageEventStream, error) {
			providers++
			return nil, errors.New("unexpected provider")
		}
	}
	binding.Callback = func(context.Context, string, json.RawMessage, func(json.RawMessage) error) (json.RawMessage, error) {
		return nil, failure
	}
	binding.OnTrace = func(context.Context, json.RawMessage) { traces++ }
	result, err := RunPi(context.Background(), PiRunConfig{Input: json.RawMessage(`"fail before dispatch"`), Session: PiSessionConfig{Pi: binding, NativeStream: stream, nativeLadder: ladder, nativeAttempts: &agentcore.RetryPolicy{MaxAttempts: 3}}})
	if err == nil || !strings.Contains(err.Error(), "host preparation failed") {
		t.Fatal("host preparation failure not surfaced", err)
	}
	if providers != 0 || traces != 0 || ladder.selection().Generation != 0 || result.Projection.StopReason != "error" || !strings.Contains(string(result.State), "host preparation failed") {
		t.Fatal("preparation failure lost, retried, or traced as provider work", result.Projection, providers, traces)
	}
}

func TestNativeRequestAdmissionAbortRetainsNativeTerminalAndUsage(t *testing.T) {
	for _, visible := range []bool{false, true} {
		t.Run(map[bool]string{false: "before-commit", true: "after-commit"}[visible], func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			ladder := testNativeLadder(t)
			started, observed := make(chan struct{}), make(chan struct{}, 1)
			ladder.rungs[0].stream = func(ctx context.Context, _ json.RawMessage, _ ai.TranscriptContext, _ map[string]any) (*ai.AssistantMessageEventStream, error) {
				return usageStream(ctx, usageFixture("error", 2, 0)), nil
			}
			ladder.rungs[1].stream = func(ctx context.Context, _ json.RawMessage, _ ai.TranscriptContext, _ map[string]any) (*ai.AssistantMessageEventStream, error) {
				stream := ai.NewAssistantMessageEventStreamFor(ctx)
				go func() {
					defer stream.End()
					message := usageFixture("stop", 7, 0)
					message.Model = "fallback"
					stream.Push(ai.AssistantMessageEvent{Type: "start", Partial: message})
					if visible {
						stream.Push(ai.AssistantMessageEvent{Type: "text_delta", Partial: message, Delta: "live"})
					}
					close(started)
					<-ctx.Done()
					stream.Synchronize(func() {
						message.StopReason = "aborted"
						reason := "native aborted terminal"
						message.ErrorMessage = &reason
						stream.Push(ai.AssistantMessageEvent{Type: "error", Reason: "aborted", Error: message})
					})
				}()
				return stream, nil
			}
			binding, _, stream := ladder.sessionBinding()
			binding.OnEvent = func(_ context.Context, raw json.RawMessage) error {
				var event struct{ Type string }
				_ = json.Unmarshal(raw, &event)
				if event.Type == "message_update" {
					select {
					case observed <- struct{}{}:
					default:
					}
				}
				return nil
			}
			type outcome struct {
				result PiRunResult
				err    error
			}
			done := make(chan outcome, 1)
			go func() {
				result, err := RunPi(ctx, PiRunConfig{Input: json.RawMessage(`"abort"`), Session: PiSessionConfig{Pi: binding, NativeStream: stream, nativeLadder: ladder, nativeAttempts: &agentcore.RetryPolicy{MaxAttempts: 1}}})
				done <- outcome{result, err}
			}()
			select {
			case <-started:
			case <-ctx.Done():
				t.Fatal("candidate did not start")
			}
			if visible {
				select {
				case <-observed:
				case <-ctx.Done():
					t.Fatal("candidate did not stream")
				}
			}
			cancel()
			var got outcome
			select {
			case got = <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("abort did not settle")
			}
			if !errors.Is(got.err, context.Canceled) || got.result.Projection.Usage.InputTokens != 9 || got.result.Projection.StopReason != "aborted" || !strings.Contains(string(got.result.State), "native aborted terminal") {
				t.Fatal("abort replaced native terminal or lost paid usage", got.err, got.result.Projection, string(got.result.State))
			}
			want := 0
			if visible {
				want = 1
			}
			if ladder.selection().Rung != want {
				t.Fatal("abort committed a speculative candidate or rolled back visible content")
			}
		})
	}
}
