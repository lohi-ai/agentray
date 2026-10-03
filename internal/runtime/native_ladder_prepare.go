package agentruntime

import (
	"context"
	"encoding/json"
	"errors"
)

// Pi keeps its model in a loop-local config. Publishing Agent state alone does
// not change the next request in that loop. Translate only a known old binding
// through host prepareRequest; never mutate the pinned engine's contract.
func (s *PiSession) callback(ctx context.Context, method string, params json.RawMessage, emit func(json.RawMessage) error) (json.RawMessage, error) {
	ladder := s.config.nativeLadder
	if method != "prepareRequest" || ladder == nil {
		return s.hostCallback(ctx, method, params, emit)
	}
	if err := s.failure(); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var request map[string]json.RawMessage
	if json.Unmarshal(params, &request) != nil || request == nil {
		return nil, errors.New("invalid native ladder request")
	}
	known := false
	for _, rung := range ladder.rungs {
		if nativeModelIdentityEqual(request["model"], rung.model) {
			known = true
			break
		}
	}
	if !known {
		return nil, errors.New("native request model is outside bound ladder")
	}
	selected, err := ladder.requestBinding(ctx)
	if err != nil {
		return nil, err
	}
	changed := !nativeModelIdentityEqual(request["model"], selected.model)
	if changed {
		request["model"] = selected.model
		params, err = json.Marshal(request)
		if err != nil {
			return nil, err
		}
	}
	value, err := s.hostCallback(ctx, method, params, emit)
	if err != nil {
		return nil, err
	}
	current, err := ladder.requestBinding(ctx)
	if err != nil {
		return nil, err
	}
	if current.providerID != selected.providerID || !nativeModelIdentityEqual(current.model, selected.model) {
		return nil, errors.New("native selection changed during request preparation")
	}
	var update map[string]json.RawMessage
	if !nativeNull(value) {
		if json.Unmarshal(value, &update) != nil || update == nil {
			return nil, errors.New("invalid native prepareRequest update")
		}
		if proposed := update["model"]; len(proposed) > 0 && !nativeModelIdentityEqual(proposed, selected.model) {
			return nil, errors.New("native prepareRequest changed the bound model")
		}
	}
	if !changed {
		return value, nil
	}
	if update == nil {
		update = map[string]json.RawMessage{}
	}
	update["model"] = selected.model
	return json.Marshal(update)
}
