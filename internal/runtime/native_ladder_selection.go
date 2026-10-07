package agentruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"

	"github.com/2found/2ai/agentcore"
	"github.com/2found/2ai/agentcore/engine"
	nativehost "github.com/2found/2ai/agentcore/host"
	"github.com/2found/2ai/ai"
)

type nativeLadderSelection = ai.FallbackSelection

type nativeBoundRung struct {
	config        NativeAgentConfig
	model         json.RawMessage
	providerID    string
	pricingKnown  bool
	stream        engine.StreamFn
	toolsDisabled bool
}
type nativeModelLadder struct {
	mu         sync.Mutex
	rungs      []nativeBoundRung
	active     int
	generation uint64
	tools      json.RawMessage // original host catalogue; nil means host-composed
}

// fork copies immutable bindings into a new selection owner. A new child starts
// at its configured primary; a resumed child's journal restores its own choice.
// Credential refresh functions/pools are shared services, never selection state.
func (l *nativeModelLadder) fork() *nativeModelLadder {
	child := &nativeModelLadder{rungs: make([]nativeBoundRung, len(l.rungs)), tools: append(json.RawMessage(nil), l.tools...)}
	for i, rung := range l.rungs {
		rung.model = append(json.RawMessage(nil), rung.model...)
		rung.config.Options = append(json.RawMessage(nil), rung.config.Options...)
		child.rungs[i] = rung
	}
	return child
}

// Options are resolved per concrete tier, so two rows sharing a vendor cannot
// accidentally share a vendor-only refresh callback. Construction performs no IO.
func newNativeModelLadder(tier ModelTier, base NativeAgentConfig, options func(ModelTier) (PiModelOptions, error)) (*nativeModelLadder, error) {
	if options == nil {
		return nil, errors.New("native ladder requires rung-scoped binding options")
	}
	var original struct {
		InitialState struct{ Tools json.RawMessage }
	}
	if len(base.Options) > 0 {
		if err := json.Unmarshal(base.Options, &original); err != nil {
			return nil, err
		}
	}
	ladder := &nativeModelLadder{tools: append(json.RawMessage(nil), original.InitialState.Tools...)}
	for i, resolved := range tier.resolvedRungs() {
		opts, err := options(resolved.tier)
		if err != nil {
			return nil, fmt.Errorf("bind native rung %d: %w", i, err)
		}
		cfg, known, stream, err := resolved.tier.bindNativeTier(base, opts, nil)
		if err != nil {
			return nil, fmt.Errorf("bind native rung %d: %w", i, err)
		}
		var wire struct {
			InitialState struct{ Model json.RawMessage }
		}
		if err = json.Unmarshal(cfg.Options, &wire); err != nil {
			return nil, err
		}
		if stream == nil {
			stream = (ai.NativeProvider{}).Stream
		}
		disabled := resolved.tier.Capabilities.Tools == agentcore.CapabilityUnsupported || (opts.ToolChoice.Mode == agentcore.ToolChoiceNone && resolved.tier.Capabilities.ToolChoice == agentcore.CapabilityUnsupported)
		ladder.rungs = append(ladder.rungs, nativeBoundRung{config: cfg, model: append(json.RawMessage(nil), wire.InitialState.Model...), providerID: resolved.tier.ProviderID, pricingKnown: known, stream: stream, toolsDisabled: disabled})
	}
	return ladder, nil
}

// Admission keeps the host's tool catalogue independently of provider
// capabilities. Each attempt filters only its provider view; a tool-free
// primary must not erase tools which a later candidate can use. Explicit host
// restrictions (including an empty catalogue) still apply to every rung.
func (l *nativeModelLadder) admissionBinding() (NativeAgentConfig, bool, engine.StreamFn) {
	cfg, known, stream := l.sessionBinding()
	var options map[string]json.RawMessage
	_ = json.Unmarshal(cfg.Options, &options)
	var initial map[string]json.RawMessage
	_ = json.Unmarshal(options["initialState"], &initial)
	if len(l.tools) == 0 {
		delete(initial, "tools")
	} else {
		initial["tools"] = append(json.RawMessage(nil), l.tools...)
	}
	options["initialState"], _ = json.Marshal(initial)
	cfg.Options, _ = json.Marshal(options)
	return cfg, known, stream
}
func (l *nativeModelLadder) selectionLocked() nativeLadderSelection {
	rung := l.rungs[l.active]
	return nativeLadderSelection{Version: 1, Generation: l.generation, Rung: l.active, ProviderID: rung.providerID, Model: append(json.RawMessage(nil), rung.model...)}
}
func (l *nativeModelLadder) selection() nativeLadderSelection {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.selectionLocked()
}
func (l *nativeModelLadder) binding() (NativeAgentConfig, bool, engine.StreamFn) {
	l.mu.Lock()
	defer l.mu.Unlock()
	rung := l.rungs[l.active]
	cfg := rung.config
	cfg.Options = append(json.RawMessage(nil), cfg.Options...)
	return cfg, rung.pricingKnown, rung.stream
}

// sessionBinding installs routing before host wrappers are composed, so resume
// can restore the provider row without discarding policy, tools or tracing.
// A ladder belongs to one session; selections occur between provider attempts.
func (l *nativeModelLadder) sessionBinding() (NativeAgentConfig, bool, engine.StreamFn) {
	cfg, known, _ := l.binding()
	// Register hooks needed by any rung. An inactive rung's optional hook is
	// a no-op in bindPi; omitting it here would bypass fallback capabilities.
	var options map[string]json.RawMessage
	_ = json.Unmarshal(cfg.Options, &options)
	var names []string
	for _, rung := range l.rungs {
		var wire struct{ Callbacks []string }
		_ = json.Unmarshal(rung.config.Options, &wire)
		for _, name := range wire.Callbacks {
			if !slices.Contains(names, name) {
				names = append(names, name)
			}
		}
	}
	options["callbacks"], _ = json.Marshal(names)
	cfg.Options, _ = json.Marshal(options)
	cfg.Callback = func(ctx context.Context, method string, params json.RawMessage, emit func(json.RawMessage) error) (json.RawMessage, error) {
		binding, _, _ := l.binding()
		if method == "prepareRequest" || method == "transformContext" || method == "convertToLlm" || method == "getApiKey" || method == "onPayload" {
			rung, err := l.requestBinding(ctx)
			if err != nil {
				return nil, err
			}
			binding = rung.config
		}
		if binding.Callback == nil {
			return nil, fmt.Errorf("missing native ladder callback for %s", method)
		}
		return binding.Callback(ctx, method, params, emit)
	}
	stream := func(ctx context.Context, model json.RawMessage, transcript ai.TranscriptContext, options map[string]any) (*ai.AssistantMessageEventStream, error) {
		rung, err := l.requestBinding(ctx)
		if err != nil {
			return nil, err
		}
		if !nativeModelIdentityEqual(model, rung.model) {
			return nil, errors.New("native stream model differs from selected ladder binding")
		}
		return rung.stream(ctx, model, transcript, options)
	}
	return cfg, known, stream
}
func nativeModelIdentityEqual(a, b json.RawMessage) bool {
	return nativehost.SameJSON(a, b)
}
func (l *nativeModelLadder) validateLocked(selection nativeLadderSelection) error {
	if selection.Version != 1 || selection.Rung < 0 || selection.Rung >= len(l.rungs) {
		return errors.New("invalid native ladder selection")
	}
	rung := l.rungs[selection.Rung]
	if selection.ProviderID != rung.providerID || !nativeModelIdentityEqual(selection.Model, rung.model) {
		return errors.New("native ladder model or provider row changed; explicit migration required")
	}
	return nil
}

// restore runs before attempts are admitted. It never falls back to rung zero
// when the durable identity is no longer among the current host bindings.
func (l *nativeModelLadder) restore(selection nativeLadderSelection) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.validateLocked(selection); err != nil {
		return err
	}
	l.active, l.generation = selection.Rung, selection.Generation
	return nil
}

// selectRung publishes only after persistence succeeds. persist must not reenter
// this ladder; it is called under the selection lock to fence competing commits.
func (l *nativeModelLadder) selectRung(ctx context.Context, expected uint64, next int, persist func(context.Context, nativeLadderSelection) error) error {
	if persist == nil {
		return errors.New("native ladder selection requires persistence")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if expected != l.generation {
		return errors.New("stale native ladder selection")
	}
	if next < 0 || next >= len(l.rungs) {
		return errors.New("native ladder rung out of range")
	}
	if next == l.active {
		return nil
	}
	if l.generation == ^uint64(0) {
		return errors.New("native ladder generation exhausted")
	}
	rung := l.rungs[next]
	record := nativeLadderSelection{Version: 1, Generation: l.generation + 1, Rung: next, ProviderID: rung.providerID, Model: append(json.RawMessage(nil), rung.model...)}
	if err := persist(ctx, record); err != nil {
		return err
	}
	// The durable write is authoritative even if cancellation races its success.
	l.active, l.generation = next, record.Generation
	return nil
}
