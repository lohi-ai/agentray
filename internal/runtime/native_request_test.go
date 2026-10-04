package agentruntime

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/lohi-ai/agentray/agentcore"
	"github.com/lohi-ai/agentray/agentcore/engine"
	"github.com/lohi-ai/agentray/ai"
)

func TestNativeRequestPreparationMatchesEngine(t *testing.T) {
	for _, custom := range []bool{false, true} {
		t.Run(map[bool]string{false: "default-conversion", true: "custom-conversion"}[custom], func(t *testing.T) {
			ctx := context.Background()
			names := []string{"prepareRequest", "transformContext"}
			if custom {
				names = append(names, "convertToLlm")
			}
			var sourceRaw json.RawMessage
			order := []string{}
			base := agentcore.PiConfig{Options: piRequestJSON(map[string]any{"callbacks": names, "initialState": map[string]any{"thinkingLevel": "high"}})}
			base.Callback = func(_ context.Context, method string, raw json.RawMessage, _ func(json.RawMessage) error) (json.RawMessage, error) {
				order = append(order, method)
				var messages []json.RawMessage
				if method == "prepareRequest" {
					sourceRaw = append(json.RawMessage(nil), raw...)
					var request struct {
						Context struct{ Messages []json.RawMessage }
					}
					if err := json.Unmarshal(raw, &request); err != nil {
						return nil, err
					}
					messages = append(request.Context.Messages, json.RawMessage(`{"role":"host","content":"opaque context","extension":9007199254740993}`), json.RawMessage(`{"role":"user","content":"prepared"}`))
					return piRequestJSON(map[string]any{"context": map[string]any{"messages": messages, "tools": []any{}}, "thinkingLevel": "off", "messages": []any{map[string]string{"role": "user", "content": "not appended by prepareRequest"}}}), nil
				}
				if err := json.Unmarshal(raw, &messages); err != nil {
					return nil, err
				}
				messages = append(messages, piRequestJSON(map[string]string{"role": "user", "content": method}))
				return piRequestJSON(messages), nil
			}
			tier := ModelTier{TierConfig: TierConfig{Provider: "openai", Model: "fixture", APIKey: "test"}}
			ladder, err := newNativeModelLadder(tier, base, func(ModelTier) (PiModelOptions, error) { return PiModelOptions{}, nil })
			if err != nil {
				t.Fatal(err)
			}
			var observed ai.TranscriptContext
			var observedOptions map[string]any
			ladder.rungs[0].stream = func(ctx context.Context, _ json.RawMessage, transcript ai.TranscriptContext, options map[string]any) (*ai.AssistantMessageEventStream, error) {
				observed, observedOptions = transcript, options
				return usageStream(ctx, usageFixture("stop", 1, 0)), nil
			}
			binding, _, stream := ladder.sessionBinding()
			session, err := NewPiSession(ctx, PiSessionConfig{NativeGo: true, Pi: binding, NativeStream: stream, nativeLadder: ladder})
			if err != nil {
				t.Fatal(err)
			}
			defer session.Close()
			if err = session.Prompt(ctx, json.RawMessage(`"original"`)); err != nil {
				t.Fatal(err)
			}
			wantContext, wantOptions, wantOrder := piRequestJSON(observed), piRequestJSON(observedOptions), append([]string(nil), order...)
			input, err := session.native.bindUpdate(sourceRaw)
			if err != nil {
				t.Fatal(err)
			}
			source := engine.Request{Context: input.Context, Model: input.Model, ThinkingLevel: *input.ThinkingLevel}
			before := piRequestJSON(nativeContext(source.Context))
			stateBefore, err := session.State(ctx)
			if err != nil {
				t.Fatal(err)
			}
			attemptCtx, err := ladder.attemptContext(ctx, 0, 0)
			if err != nil {
				t.Fatal(err)
			}
			order = nil
			request, err := session.prepareNativeAttempt(attemptCtx, ladder.rungs[0], source, observedOptions)
			if err != nil {
				t.Fatal(err)
			}
			response, err := session.openNativeAttempt(attemptCtx, ladder.rungs[0], request.transcript, request.options)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = response.SnapshotResult(ctx); err != nil {
				t.Fatal(err)
			}
			if !samePiJSON(piRequestJSON(observed), wantContext) || !samePiJSON(piRequestJSON(observedOptions), wantOptions) || !reflect.DeepEqual(order, wantOrder) {
				t.Fatalf("candidate pipeline differs from engine: order=%v want=%v\ncontext=%s want=%s", order, wantOrder, piRequestJSON(observed), wantContext)
			}
			if strings.Contains(string(piRequestJSON(nativeContext(request.request.Context))), "transformContext") || !strings.Contains(string(piRequestJSON(nativeContext(request.request.Context))), "9007199254740993") {
				t.Fatal("provider transform changed prepared native context")
			}
			request.request.Context.Messages.Get(0).Content = ai.TextContent("mutated")
			stateAfter, err := session.State(ctx)
			if err != nil || !samePiJSON(stateBefore, stateAfter) || !samePiJSON(before, piRequestJSON(nativeContext(source.Context))) {
				t.Fatal("candidate preparation mutated source or persistent agent state", err)
			}
		})
	}
}

func TestNativeRequestPreparationUsesFreshCandidateContext(t *testing.T) {
	ladder := testNativeLadder(t)
	for i := range ladder.rungs {
		index, original := i, ladder.rungs[i].config.Callback
		var options map[string]json.RawMessage
		_ = json.Unmarshal(ladder.rungs[i].config.Options, &options)
		var names []string
		_ = json.Unmarshal(options["callbacks"], &names)
		options["callbacks"] = piRequestJSON(append(names, "transformContext", "convertToLlm"))
		ladder.rungs[i].config.Options = piRequestJSON(options)
		ladder.rungs[i].config.Callback = func(ctx context.Context, method string, params json.RawMessage, emit func(json.RawMessage) error) (json.RawMessage, error) {
			if method == "transformContext" || method == "convertToLlm" {
				var messages []json.RawMessage
				if err := json.Unmarshal(params, &messages); err != nil {
					return nil, err
				}
				return piRequestJSON(append(messages, piRequestJSON(map[string]any{"role": "user", "content": method, "rung": index}))), nil
			}
			return original(ctx, method, params, emit)
		}
	}
	binding, _, stream := ladder.sessionBinding()
	session, err := NewPiSession(context.Background(), PiSessionConfig{NativeGo: true, Pi: binding, NativeStream: stream, nativeLadder: ladder})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	source := engine.Request{Context: &engine.Context{Messages: engine.NewList([]*ai.Message{{Role: "user", Content: ai.TextContent("original")}}...)}, Model: ladder.rungs[0].model, ThinkingLevel: "high"}
	for _, index := range []int{0, 1, 1} {
		ctx, err := ladder.attemptContext(session.ctx, index, 0)
		if err != nil {
			t.Fatal(err)
		}
		prepared, err := session.prepareNativeAttempt(ctx, ladder.rungs[index], source, map[string]any{"reasoning": "off"})
		if err != nil {
			t.Fatal(err)
		}
		var messages []struct {
			Content string
			Rung    int
		}
		if err = json.Unmarshal(piRequestJSON(prepared.transcript.Messages()), &messages); err != nil {
			t.Fatal(err)
		}
		if len(messages) != 3 || messages[0].Content != "original" || messages[1].Content != "transformContext" || messages[2].Content != "convertToLlm" || messages[1].Rung != index || messages[2].Rung != index || prepared.options["reasoning"] != "high" {
			t.Fatal("candidate inherited another attempt's transformed context or callbacks", messages)
		}
	}
	if source.Context.Messages.Len() != 1 || ladder.selection().Generation != 0 {
		t.Fatal("request-only preparation committed state")
	}
}

func TestNativeRequestPreparationFailureNeverRetriesOrEscalates(t *testing.T) {
	for _, stage := range []string{"prepareRequest", "transformContext", "convertToLlm", "getApiKey"} {
		t.Run(stage, func(t *testing.T) {
			ladder := testNativeLadder(t)
			binding, _, stream := ladder.sessionBinding()
			var options map[string]json.RawMessage
			_ = json.Unmarshal(binding.Options, &options)
			options["callbacks"] = piRequestJSON([]string{"prepareRequest", "transformContext", "convertToLlm", "getApiKey"})
			binding.Options = piRequestJSON(options)
			failure := &agentcore.ProviderError{Status: 503, Message: "host service unavailable", RetryAfter: time.Millisecond}
			callbacks, attempts, providers := 0, 0, 0
			original := binding.Callback
			binding.Callback = func(ctx context.Context, method string, raw json.RawMessage, emit func(json.RawMessage) error) (json.RawMessage, error) {
				if method == stage {
					callbacks++
					return nil, failure
				}
				if method == "transformContext" || method == "convertToLlm" {
					return raw, nil
				}
				return original(ctx, method, raw, emit)
			}
			for i := range ladder.rungs {
				ladder.rungs[i].stream = func(context.Context, json.RawMessage, ai.TranscriptContext, map[string]any) (*ai.AssistantMessageEventStream, error) {
					providers++
					return nil, errors.New("unexpected provider")
				}
			}
			session, err := NewPiSession(context.Background(), PiSessionConfig{NativeGo: true, Pi: binding, NativeStream: stream, nativeLadder: ladder})
			if err != nil {
				t.Fatal(err)
			}
			defer session.Close()
			out := ai.NewAssistantMessageEventStream()
			_, err = session.runNativeLadder(session.ctx, out, nativeLadderRun{policy: agentcore.RetryPolicy{MaxAttempts: 3}, open: func(ctx context.Context, rung nativeBoundRung, _, _ int) (*ai.AssistantMessageEventStream, error) {
				attempts++
				prepared, err := session.prepareNativeAttempt(ctx, rung, engine.Request{Context: &engine.Context{}, Model: ladder.rungs[0].model}, nil)
				if err != nil {
					return nil, err
				}
				return session.openNativeAttempt(ctx, rung, prepared.transcript, prepared.options)
			}})
			if !errors.Is(err, failure) || callbacks != 1 || attempts != 1 || providers != 0 || ladder.selection().Generation != 0 {
				t.Fatalf("host failure retried/escalated: err=%v callbacks=%d attempts=%d providers=%d", err, callbacks, attempts, providers)
			}
			assertAttemptEmpty(t, out)
		})
	}
}
