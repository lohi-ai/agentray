package agentruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"slices"

	"github.com/2found/2ai/agentcore"
	nativehost "github.com/2found/2ai/agentcore/host"
)

func bindPiRequestCompaction(cfg *PiRunConfig, projection *piRunProjection) (func(*PiSession) error, error) {
	if cfg.Compaction == nil {
		return func(*PiSession) error { return nil }, nil
	}
	policy := *cfg.Compaction
	var options map[string]json.RawMessage
	if json.Unmarshal(cfg.Session.Pi.Options, &options) != nil || options == nil {
		return nil, errors.New("invalid Pi compaction options")
	}
	var initial struct{ Model struct{ ContextWindow int } }
	_ = json.Unmarshal(options["initialState"], &initial)
	if policy.Budget <= 0 && cfg.Host != nil {
		policy.Budget, policy.KeepRecent = cfg.Host.PiCompactionPolicy()
	}
	if policy.KeepRecent <= 0 {
		policy.KeepRecent = nativehost.DefaultKeepRecentTokens
	}
	var names []string
	if raw := options["callbacks"]; len(raw) > 0 {
		if err := json.Unmarshal(raw, &names); err != nil {
			return nil, err
		}
	}
	hadTransform := slices.Contains(names, "transformContext")
	if !hadTransform {
		names = append(names, "transformContext")
	}
	options["callbacks"], _ = json.Marshal(names)
	cfg.Session.Pi.Options, _ = json.Marshal(options)
	var compactor *nativehost.Compactor
	accountUsage := func(u agentcore.Usage) {
		projection.mu.Lock()
		defer projection.mu.Unlock()
		v := &projection.result.Usage
		v.InputTokens += u.InputTokens
		v.OutputTokens += u.OutputTokens
		v.CacheReadTokens += u.CacheReadTokens
		v.CacheWriteTokens += u.CacheWriteTokens
		v.CostUSD += u.CostUSD
		v.CostUnpriced = v.CostUnpriced || u.CostUnpriced
	}
	original := cfg.Session.Pi.Callback
	ladder := cfg.Session.nativeLadder
	cfg.Session.Pi.Callback = func(ctx context.Context, method string, params json.RawMessage, emit func(json.RawMessage) error) (json.RawMessage, error) {
		if method != "transformContext" {
			if original == nil {
				return nil, errors.New("missing native callback")
			}
			return original(ctx, method, params, emit)
		}
		window := initial.Model.ContextWindow
		if ladder != nil {
			rung, err := ladder.requestBinding(ctx)
			if err != nil {
				return nil, err
			}
			var model struct{ ContextWindow int }
			if err := json.Unmarshal(rung.model, &model); err != nil {
				return nil, err
			}
			window = model.ContextWindow
		}
		view := params
		if hadTransform && original != nil {
			value, err := original(ctx, method, params, emit)
			if err != nil {
				return params, nil
			}
			if len(value) > 0 {
				view = value
			}
		}
		previous := compactor.Checkpoint()
		compacted := compactor.TransformWithPolicy(ctx, view, policy.ForWindow(window))
		if cfg.Host != nil && !bytes.Equal(previous, compactor.Checkpoint()) {
			projection.mu.Lock()
			turn := projection.result.Turns
			projection.mu.Unlock()
			if err := cfg.Host.ObservePiMessages(ctx, agentcore.PhaseRebase, turn, compacted); err != nil {
				return nil, err
			}
		}
		return compacted, nil
	}
	return func(session *PiSession) error {
		compactor = nativehost.NewCompactor(nativehost.CompactorOptions{
			Revision: session.agent.UpstreamCommit(), Policy: policy, Usage: accountUsage,
			Record: func(ctx context.Context, raw json.RawMessage) error {
				return session.record(ctx, agentcore.EntryPiContextSummary, "", raw)
			},
		})
		if cfg.Session.Store == nil {
			return nil
		}
		entries, err := cfg.Session.Store.Log(session.ctx, cfg.Session.SessionID)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if entry.Kind != agentcore.EntryPiContextSummary {
				continue
			}
			if err := compactor.Restore(json.RawMessage(entry.Content)); err != nil {
				return err
			}
		}
		return nil
	}, nil
}
