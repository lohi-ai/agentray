package agentruntime

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/2found/2ai/agentcore/engine"
	"github.com/2found/2ai/ai"
)

type nativePreparedAttempt struct {
	request    engine.Request
	transcript ai.TranscriptContext
	options    map[string]any
}

// prepareNativeAttempt starts from the untransformed logical request for every
// candidate. Its context is private until the caller admits that candidate;
// transforms/conversion affect only the provider view, never native history.
// Credential and payload hooks remain in openNativeAttempt so every transport
// retry acquires the current row credential after context preparation.
func (s *PiSession) prepareNativeAttempt(ctx context.Context, rung nativeBoundRung, source engine.Request, options map[string]any) (prepared nativePreparedAttempt, err error) {
	defer func() {
		if err != nil {
			err = &ai.PreparationError{Cause: err}
		}
	}()
	if s.agent == nil || s.config.nativeLadder == nil || source.Context == nil {
		return prepared, errors.New("native request preparation requires an agent, ladder and context")
	}
	check := func() error {
		if err := s.failure(); err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		bound, err := s.config.nativeLadder.requestBinding(ctx)
		if err != nil {
			return err
		}
		if bound.providerID != rung.providerID || !nativeModelIdentityEqual(bound.model, rung.model) {
			return errors.New("native request preparation differs from bound attempt")
		}
		return nil
	}
	if err = check(); err != nil {
		return prepared, err
	}
	// The JSON callback boundary already clones the request. Decode that same
	// snapshot for the no-update path, retaining opaque native message fields
	// and rebinding concrete tools without executing them.
	raw, err := json.Marshal(map[string]any{"context": nativeContext(source.Context), "model": source.Model, "thinkingLevel": source.ThinkingLevel})
	if err != nil {
		return prepared, err
	}
	cloned, err := s.agent.bindUpdate(raw)
	if err != nil {
		return prepared, err
	}
	// Native tool callbacks are not JSON metadata. The no-update path must keep
	// the exact installed argument preparer/executor (not reconstruct them from
	// a declaration); ask-v1 in particular normalizes shorthand question args.
	for i, tool := range source.Context.Tools.Values() {
		cloned.Context.Tools.Get(i).PrepareArguments = tool.PrepareArguments
		cloned.Context.Tools.Get(i).Execute = tool.Execute
	}
	prepared.request = engine.Request{Context: cloned.Context, Model: append(json.RawMessage(nil), rung.model...), ThinkingLevel: source.ThinkingLevel}
	updateRaw, err := s.callback(ctx, "prepareRequest", raw, nil)
	if err != nil {
		return prepared, err
	}
	update, err := s.agent.bindUpdate(updateRaw)
	if err != nil {
		return prepared, err
	}
	if update != nil {
		if update.Context != nil {
			prepared.request.Context = update.Context
		}
		if update.ThinkingLevel != nil {
			prepared.request.ThinkingLevel = *update.ThinkingLevel
		}
		// Like Pi's prepareRequest/applyUpdate, update.Messages is not appended.
	}
	if err = check(); err != nil {
		return prepared, err
	}
	messages := engine.MessageValues(prepared.request.Context.Messages.Values())
	for _, method := range []string{"transformContext", "convertToLlm"} {
		if !s.callbacks[method] {
			if method == "convertToLlm" {
				// Pi Agent's default conversion drops custom host message roles.
				converted := []ai.Message{}
				for _, message := range messages {
					switch message.Role {
					case "system", "user", "assistant", "toolResult":
						converted = append(converted, message)
					}
				}
				messages = converted
			}
			continue
		}
		raw, err = json.Marshal(messages)
		if err != nil {
			return prepared, err
		}
		raw, err = s.callback(ctx, method, raw, nil)
		if err != nil {
			return prepared, err
		}
		// Decode into a fresh slice: a shrinking transform must not overwrite
		// the prepared context which will be used by later tool callbacks.
		var view []ai.Message
		if err = json.Unmarshal(raw, &view); err != nil {
			return prepared, err
		}
		messages = view
		if err = check(); err != nil {
			return prepared, err
		}
	}
	if rung.toolsDisabled {
		// Strip the entire declaration history from this provider-only view:
		// providers with native mid-conversation tool support may serialize the
		// initial catalogue even after a later toolsRemoved update. Keep native
		// tool calls/results and the engine's governed execution catalogue intact.
		view := append([]ai.Message(nil), messages...)
		for i := range view {
			if view[i].Role == "system" {
				view[i] = ai.WithToolChanges(view[i], ai.ToolStateChanges{})
			}
		}
		messages = view
	}
	prepared.transcript = ai.NormalizeContext(ai.Context{Messages: messages})
	prepared.options = make(map[string]any, len(options)+1)
	for key, value := range options {
		prepared.options[key] = value
	}
	if level := prepared.request.ThinkingLevel; level != "" && level != "off" {
		prepared.options["reasoning"] = level
	} else {
		delete(prepared.options, "reasoning")
	}
	return prepared, check()
}
