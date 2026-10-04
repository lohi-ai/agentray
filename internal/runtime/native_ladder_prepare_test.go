package agentruntime

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/lohi-ai/agentray/ai"
)

func TestNativeLadderPrepareUpdatesNextEngineTurn(t *testing.T) {
	ctx := context.Background()
	turns := 0
	base := NativeAgentConfig{Options: json.RawMessage(`{"callbacks":["finishTurn"]}`), Callback: func(_ context.Context, method string, _ json.RawMessage, _ func(json.RawMessage) error) (json.RawMessage, error) {
		if method != "finishTurn" {
			return nil, errors.New("unexpected callback")
		}
		turns++
		if turns == 1 {
			return json.RawMessage(`{"action":"continue"}`), nil
		}
		return json.RawMessage(`{"action":"end"}`), nil
	}}
	tier := ModelTier{TierConfig: TierConfig{Provider: "openai", ProviderID: "a", Model: "primary", APIKey: "key-a", Fallback: &TierConfig{Provider: "openai", ProviderID: "b", Model: "fallback", APIKey: "key-b"}}}
	ladder, err := newNativeModelLadder(tier, base, func(ModelTier) (PiModelOptions, error) { return PiModelOptions{}, nil })
	if err != nil {
		t.Fatal(err)
	}
	var session *PiSession
	calls := []int{}
	for i := range ladder.rungs {
		index := i
		ladder.rungs[i].stream = func(ctx context.Context, model json.RawMessage, _ ai.TranscriptContext, options map[string]any) (*ai.AssistantMessageEventStream, error) {
			calls = append(calls, index)
			want := "key-a"
			if index == 1 {
				want = "key-b"
			}
			if options["apiKey"] != want || !nativeModelIdentityEqual(model, ladder.rungs[index].model) {
				t.Error("loop model/key not synchronized")
			}
			if index == 0 {
				if err := session.selectNativeRung(ctx, 0, 1); err != nil {
					return nil, err
				}
			}
			return nativeChildResponse(ai.ContentBlock{Type: "text", Text: want}), nil
		}
	}
	binding, _, stream := ladder.sessionBinding()
	original := binding.Callback
	seen := []json.RawMessage{}
	binding.Callback = func(ctx context.Context, method string, params json.RawMessage, emit func(json.RawMessage) error) (json.RawMessage, error) {
		value, err := original(ctx, method, params, emit)
		if err == nil && method == "prepareRequest" {
			var request struct{ Model json.RawMessage }
			_ = json.Unmarshal(params, &request)
			seen = append(seen, request.Model)
			return json.RawMessage(`{"thinkingLevel":"off"}`), nil
		}
		return value, err
	}
	session, err = NewPiSession(ctx, PiSessionConfig{Pi: binding, NativeStream: stream, nativeLadder: ladder})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	if err = session.Prompt(ctx, json.RawMessage(`"continue twice"`)); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 2 || calls[0] != 0 || calls[1] != 1 || len(seen) != 2 || !nativeModelIdentityEqual(seen[1], ladder.rungs[1].model) {
		t.Fatalf("engine loop retained old model: calls=%v prepared=%d", calls, len(seen))
	}
	state, err := session.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var final struct{ Model json.RawMessage }
	if json.Unmarshal(state, &final) != nil || !nativeModelIdentityEqual(final.Model, ladder.rungs[1].model) {
		t.Fatal("persistent model lost", string(state))
	}
}

func TestNativeLadderPrepareRejectsHostModelOverride(t *testing.T) {
	for _, mode := range []string{"foreign-request", "host-override", "concurrent-selection"} {
		t.Run(mode, func(t *testing.T) {
			ladder := testNativeLadder(t)
			binding, _, stream := ladder.sessionBinding()
			original := binding.Callback
			binding.Callback = func(ctx context.Context, method string, params json.RawMessage, emit func(json.RawMessage) error) (json.RawMessage, error) {
				value, err := original(ctx, method, params, emit)
				if method == "prepareRequest" && err == nil {
					if mode == "host-override" {
						return json.RawMessage(`{"model":{"id":"foreign"}}`), nil
					}
					if mode == "concurrent-selection" {
						err = ladder.selectRung(ctx, 0, 1, func(context.Context, nativeLadderSelection) error { return nil })
					}
				}
				return value, err
			}
			session, err := NewPiSession(context.Background(), PiSessionConfig{Pi: binding, NativeStream: stream, nativeLadder: ladder})
			if err != nil {
				t.Fatal(err)
			}
			defer session.Close()
			model := ladder.rungs[0].model
			if mode == "foreign-request" {
				model = json.RawMessage(`{"id":"foreign"}`)
			}
			request, _ := json.Marshal(map[string]any{"model": model, "context": map[string]any{"messages": []any{}}})
			if _, err = session.callback(session.ctx, "prepareRequest", request, nil); err == nil {
				t.Fatal("unsafe preparation accepted")
			}
		})
	}
}

func TestNativeLadderPrepareCandidateDoesNotPublishAgentModel(t *testing.T) {
	ladder := testNativeLadder(t)
	binding, _, stream := ladder.sessionBinding()
	session, err := NewPiSession(context.Background(), PiSessionConfig{Pi: binding, NativeStream: stream, nativeLadder: ladder})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	ctx, err := ladder.attemptContext(session.ctx, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	request, _ := json.Marshal(map[string]any{"model": ladder.rungs[0].model, "context": map[string]any{"messages": []any{}}})
	result, err := session.callback(ctx, "prepareRequest", request, nil)
	if err != nil {
		t.Fatal(err)
	}
	var update struct{ Model json.RawMessage }
	if json.Unmarshal(result, &update) != nil || !nativeModelIdentityEqual(update.Model, ladder.rungs[1].model) {
		t.Fatal("candidate model not returned to request", string(result))
	}
	state, err := session.State(session.ctx)
	if err != nil {
		t.Fatal(err)
	}
	var current struct{ Model json.RawMessage }
	if json.Unmarshal(state, &current) != nil || !nativeModelIdentityEqual(current.Model, ladder.rungs[0].model) || ladder.selection().Generation != 0 {
		t.Fatal("prepare published candidate before response")
	}
}
