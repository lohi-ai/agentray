package agentruntime

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/lohi-ai/agentray/ai"
)

// openNativeAttempt binds provider-owned controls afresh for each attempt. The
// transcript is the host's prepared request view; this function does not replace
// Agent history or run context transforms a second time.
func (s *PiSession) openNativeAttempt(ctx context.Context, rung nativeBoundRung, transcript ai.TranscriptContext, options map[string]any) (*ai.AssistantMessageEventStream, error) {
	request, err := s.nativeAttemptOptions(ctx, rung, options)
	if err != nil {
		return nil, &ai.PreparationError{Cause: err}
	}
	if s.agent == nil {
		return nil, &ai.PreparationError{Cause: errors.New("native attempt requires an in-process agent")}
	}
	return s.agent.observedProviderStream(rung.stream, true)(ctx, rung.model, transcript, request)
}

func (s *PiSession) nativeAttemptOptions(ctx context.Context, rung nativeBoundRung, options map[string]any) (map[string]any, error) {
	if err := s.failure(); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s.config.nativeLadder == nil {
		return nil, errors.New("native attempt requires session ladder")
	}
	bound, err := s.config.nativeLadder.requestBinding(ctx)
	if err != nil {
		return nil, err
	}
	if rung.providerID != bound.providerID || !nativeModelIdentityEqual(rung.model, bound.model) {
		return nil, errors.New("native attempt options do not match request scope")
	}
	var wire struct{ StreamOptions map[string]any }
	if err = json.Unmarshal(rung.config.Options, &wire); err != nil {
		return nil, err
	}
	request := make(map[string]any, len(options)+len(wire.StreamOptions))
	for key, value := range options {
		request[key] = value
	}
	// These controls include this rung's output-token cap. Never inherit a
	// different rung's cap or credential from the loop's original stream options.
	for key, value := range wire.StreamOptions {
		if _, supplied := request[key]; key == "maxTokens" || !supplied {
			request[key] = value
		}
	}
	request["model"] = append(json.RawMessage(nil), rung.model...)
	delete(request, "apiKey")
	var model struct{ Provider string }
	if err = json.Unmarshal(rung.model, &model); err != nil {
		return nil, err
	}
	provider, _ := json.Marshal(model.Provider)
	raw, err := s.callback(ctx, "getApiKey", provider, nil)
	if err != nil {
		return nil, err
	}
	if !nativeNull(raw) {
		var key string
		if err = json.Unmarshal(raw, &key); err != nil {
			return nil, err
		}
		if key != "" {
			request["apiKey"] = key
		}
	}
	if s.callbacks["onPayload"] {
		request["onPayload"] = func(ctx context.Context, payload, model json.RawMessage) (json.RawMessage, error) {
			params, err := json.Marshal(map[string]json.RawMessage{"payload": payload, "model": model})
			if err != nil {
				return nil, err
			}
			return s.callback(ctx, "onPayload", params, nil)
		}
	}
	current, err := s.config.nativeLadder.requestBinding(ctx)
	if err != nil {
		return nil, err
	}
	if current.providerID != bound.providerID || !nativeModelIdentityEqual(current.model, bound.model) {
		return nil, errors.New("native binding changed during credential preparation")
	}
	return request, nil
}
