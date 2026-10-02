package agentruntime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/lohi-ai/agentray/agentcore"
)

// PiContextCompaction configures the consumer's request-only transform. The
// original Agent retains its full transcript, including every tool result.
type PiContextCompaction struct {
	// Approximate token counts. Zero uses the composed host policy; the native
	// model's window caps Budget, and KeepRecent is capped at half that budget.
	Budget, KeepRecent int
	Summarize          func(context.Context, json.RawMessage, string) (string, agentcore.Usage, error)
}

type piContextSummary struct {
	Revision     string          `json:"revision"`
	PrefixCount  int             `json:"prefix_count"`
	PrefixDigest string          `json:"prefix_digest"`
	Message      json.RawMessage `json:"message"`
}

func parsePiContextSummary(raw string) (piContextSummary, error) {
	var summary piContextSummary
	var message struct {
		Role, Content, AgentrayContextSummary string
		Timestamp                             int64
	}
	if json.Unmarshal([]byte(raw), &summary) != nil || summary.Revision == "" || summary.PrefixCount <= 0 || len(summary.PrefixDigest) != 64 || json.Unmarshal(summary.Message, &message) != nil || message.Role != "user" || strings.TrimSpace(message.Content) == "" || message.AgentrayContextSummary == "" || message.Timestamp <= 0 {
		return summary, errors.New("invalid native context summary")
	}
	if _, err := hex.DecodeString(summary.PrefixDigest); err != nil {
		return summary, errors.New("invalid native context summary digest")
	}
	return summary, nil
}

func piPrefixDigest(messages []json.RawMessage) string {
	raw, _ := json.Marshal(messages)
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if decoder.Decode(&value) != nil {
		return ""
	}
	canonical, _ := json.Marshal(value)
	digest := sha256.Sum256(canonical)
	return hex.EncodeToString(digest[:])
}

type piRequestCompactor struct {
	config   PiContextCompaction
	revision string
	saved    *piContextSummary
	record   func(context.Context, json.RawMessage) error
	usage    func(agentcore.Usage)
}

// Request transforms are called serially by the original loop. A saved summary
// refers to a prefix of this exact view (after native host hooks), never to a Go
// text projection. On mismatch the full native context is the safe fallback.
func (c *piRequestCompactor) transform(ctx context.Context, raw json.RawMessage) (out json.RawMessage) {
	out = raw
	defer func() {
		if recover() != nil {
			out = raw
		}
	}()
	if ctx.Err() != nil || validatePiConversationMessages(raw) != nil {
		return
	}
	var original []json.RawMessage
	if json.Unmarshal(raw, &original) != nil {
		return
	}
	view := original
	baseCount, headCount := 0, 0
	if saved := c.saved; saved != nil && saved.Revision == c.revision && saved.PrefixCount < len(original) && piPrefixDigest(original[:saved.PrefixCount]) == saved.PrefixDigest {
		var boundary struct{ Role string }
		_ = json.Unmarshal(original[saved.PrefixCount], &boundary)
		if boundary.Role == "assistant" || boundary.Role == "user" {
			view = piSummaryView(original, saved.PrefixCount, saved.Message)
			baseCount = saved.PrefixCount
			headCount = len(view) - (len(original) - baseCount)
		}
	}
	out, _ = json.Marshal(view)
	cut := piRequestCompactionCut(view, c.config.Budget, c.config.KeepRecent)
	if cut <= headCount || c.config.Summarize == nil {
		return
	}
	prefix, _ := json.Marshal(view[:cut])
	text, usage, err := c.config.Summarize(ctx, prefix, c.revision)
	if c.usage != nil {
		c.usage(usage)
	}
	if err != nil || ctx.Err() != nil || strings.TrimSpace(text) == "" {
		return
	}
	count := baseCount + cut - headCount
	message, _ := json.Marshal(map[string]any{"role": "user", "content": "[Earlier work summary]\n" + strings.TrimSpace(text), "agentrayContextSummary": uuid.NewString(), "timestamp": time.Now().UnixMilli()})
	next := piSummaryView(original, count, message)
	nextRaw, _ := json.Marshal(next)
	// A summary which grows the request is not compaction. Keep the last useful
	// view while still accounting for the auxiliary model work already performed.
	if len(nextRaw) >= len(out) {
		return
	}
	saved := piContextSummary{Revision: c.revision, PrefixCount: count, PrefixDigest: piPrefixDigest(original[:count]), Message: message}
	payload, _ := json.Marshal(saved)
	if c.record != nil && c.record(ctx, payload) != nil {
		return
	}
	c.saved = &saved
	return nextRaw
}

func piSummaryView(messages []json.RawMessage, cut int, summary json.RawMessage) []json.RawMessage {
	kept := []json.RawMessage{}
	for _, raw := range messages[:cut] {
		var header struct{ Role string }
		_ = json.Unmarshal(raw, &header)
		if header.Role == "system" {
			kept = append(kept, raw)
		}
	}
	kept = append(kept, summary)
	return append(kept, messages[cut:]...)
}

// A long autonomous run may contain only one user prompt. Cut at a completed
// assistant/tool batch boundary as well as a user turn, never inside the batch.
func piRequestCompactionCut(messages []json.RawMessage, budget, keep int) int {
	if budget <= 0 {
		return 0
	}
	raw, _ := json.Marshal(messages)
	if estimateTokens(string(raw)) <= budget || validatePiConversationMessages(raw) != nil {
		return 0
	}
	keep = min(max(1, keep), max(1, budget/2))
	roles := make([]string, len(messages))
	priorAssistant := make([]bool, len(messages))
	seen := false
	for i, raw := range messages {
		var m struct{ Role string }
		_ = json.Unmarshal(raw, &m)
		roles[i] = m.Role
		priorAssistant[i] = seen
		seen = seen || m.Role == "assistant"
	}
	recent := 0
	for i := len(messages) - 1; i > 0; i-- {
		recent += estimateTokens(string(messages[i]))
		if recent >= keep && priorAssistant[i] && (roles[i] == "assistant" || roles[i] == "user") {
			return i
		}
	}
	return 0
}

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
	if window := initial.Model.ContextWindow; window > 0 {
		limit := window - min(ConvReserveTokens, max(1, window/4))
		if policy.Budget <= 0 || policy.Budget > limit {
			policy.Budget = limit
		}
	}
	if policy.KeepRecent <= 0 {
		policy.KeepRecent = ConvKeepRecentTokens
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
	compactor := &piRequestCompactor{config: policy, usage: func(u agentcore.Usage) {
		projection.mu.Lock()
		defer projection.mu.Unlock()
		v := &projection.result.Usage
		v.InputTokens += u.InputTokens
		v.OutputTokens += u.OutputTokens
		v.CacheReadTokens += u.CacheReadTokens
		v.CacheWriteTokens += u.CacheWriteTokens
		v.CostUSD += u.CostUSD
		v.CostUnpriced = v.CostUnpriced || u.CostUnpriced
	}}
	original := cfg.Session.Pi.Callback
	cfg.Session.Pi.Callback = func(ctx context.Context, method string, params json.RawMessage, emit func(json.RawMessage) error) (json.RawMessage, error) {
		if method != "transformContext" {
			if original == nil {
				return nil, errors.New("missing native callback")
			}
			return original(ctx, method, params, emit)
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
		return compactor.transform(ctx, view), nil
	}
	return func(session *PiSession) error {
		compactor.revision = session.agent.UpstreamCommit()
		compactor.record = func(ctx context.Context, raw json.RawMessage) error {
			return session.record(ctx, agentcore.EntryPiContextSummary, "", raw)
		}
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
			saved, err := parsePiContextSummary(entry.Content)
			if err != nil {
				return err
			}
			if saved.Revision != compactor.revision {
				return errors.New("native context summary revision mismatch")
			}
			compactor.saved = &saved
		}
		return nil
	}, nil
}
