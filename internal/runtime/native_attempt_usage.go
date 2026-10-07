package agentruntime

import (
	"encoding/json"
	"errors"
	nativehost "github.com/2found/2ai/agentcore/host"

	"github.com/2found/2ai/agentcore"
	"github.com/2found/2ai/ai"
)

func nativeUsageTerminal(raw json.RawMessage) (json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || fields == nil {
		return nil, errors.New("invalid native usage terminal")
	}
	// engine.streamAssistant sets this on its final snapshot, after provider
	// settlement. Everything else must still match the accounted response.
	delete(fields, "thinkingLevel")
	return json.Marshal(fields)
}

func (p *piRunProjection) addUsage(u agentcore.Usage, known bool) {
	p.result.Usage.InputTokens += u.InputTokens
	p.result.Usage.OutputTokens += u.OutputTokens
	p.result.Usage.CacheReadTokens += u.CacheReadTokens
	p.result.Usage.CacheWriteTokens += u.CacheWriteTokens
	p.result.Usage.CostUSD += u.CostUSD
	p.result.Usage.CostUnpriced = p.result.Usage.CostUnpriced || u.CostUnpriced || (!known && u.InputTokens+u.OutputTokens+u.CacheReadTokens+u.CacheWriteTokens > 0)
}

// Settled attempts are charged exactly once, including responses discarded for
// retry/escalation. No failed message is inserted into native conversation state.
func (p *piRunProjection) accountNativeAttempt(rung nativeBoundRung, attempt ai.FallbackAttempt) error {
	message := attempt.Outcome.Message()
	if message == nil {
		return nil
	}
	raw, err := json.Marshal(message)
	if err != nil {
		return err
	}
	projected, err := nativehost.ProjectMessage(raw)
	if err != nil {
		return err
	}
	if projected.Usage != nil {
		p.mu.Lock()
		defer p.mu.Unlock()
		p.addUsage(*projected.Usage, rung.pricingKnown)
	}
	return nil
}

// Register before terminal publication, so the engine cannot emit message_end
// before accounting owns it. This is a per-request handoff, not content-based
// global deduplication: two identical later responses are charged independently.
func (p *piRunProjection) expectNativeTerminal(outcome ai.AttemptOutcome) error {
	message := outcome.Message()
	if message == nil {
		return errors.New("native attempt has no terminal message")
	}
	raw, err := json.Marshal(message)
	if err != nil {
		return err
	}
	raw, err = nativeUsageTerminal(raw)
	if err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.nativeTerminal) > 0 {
		return errors.New("native terminal accounting overlaps another request")
	}
	p.nativeTerminal = raw
	return nil
}
