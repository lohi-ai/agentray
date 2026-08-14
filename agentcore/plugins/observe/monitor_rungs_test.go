package observe_test

import (
	"context"
	"errors"
	"math"
	"sync"
	"testing"

	"github.com/lohi-ai/agentray/agentcore"
	"github.com/lohi-ai/agentray/agentcore/plugins/observe"
	"github.com/lohi-ai/agentray/agentcore/plugins/preset"
)

// TestPluginTracesEveryRung is the composition-level test: registered as a
// plugin, Monitor must decorate the whole ladder, not just the primary rung. A
// run that escalates past a failing rung has to keep producing records, or the
// spend on the rung that actually answered goes unaccounted.
//
// It lives in package observe_test because it composes through preset, and
// preset imports observe.
func TestPluginTracesEveryRung(t *testing.T) {
	primary := &failingProvider{err: errors.New("rate limited: 429")}
	backup := agentcore.NewFauxProvider(agentcore.ChatResponse{
		Message:    agentcore.Message{Role: agentcore.RoleAssistant, Content: "answered on the backup rung"},
		StopReason: "stop",
		Usage:      agentcore.Usage{InputTokens: 100, OutputTokens: 50},
	})
	sink := &rungSink{}

	retry := agentcore.RetryPolicy{MaxAttempts: 1}
	agent, err := agentcore.Build(append(
		preset.Plugins(agentcore.Config{
			Provider:   primary,
			Model:      "primary-model",
			Escalation: []agentcore.ModelRung{{Provider: backup, Model: "backup-model"}},
			Retry:      &retry,
			Policy:     agentcore.DenyAll{},
		}),
		observe.Monitor{
			Pricing: observe.Pricing{"backup-model": {InputPerM: 10, OutputPerM: 20}},
			Sink:    sink,
		},
	)...)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	res, err := agent.Prompt(context.Background(), "go")
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if res.Final != "answered on the backup rung" {
		t.Fatalf("run did not escalate: final=%q", res.Final)
	}

	// Both rungs are decorated: the failed primary call and the successful
	// backup call each produced a record.
	var failed, ok int
	for _, r := range sink.records {
		if r.Err != "" {
			failed++
			continue
		}
		ok++
		if r.Model != "backup-model" {
			t.Fatalf("successful record on unexpected model %q", r.Model)
		}
		// 100/1e6*10 + 50/1e6*20 = 0.001 + 0.001 = 0.002
		if math.Abs(r.Usage.CostUSD-0.002) > 1e-9 {
			t.Fatalf("backup rung cost = %v, want 0.002", r.Usage.CostUSD)
		}
	}
	if failed == 0 {
		t.Fatal("the failing primary rung produced no record — a decorator that only sees successes cannot account for spend")
	}
	if ok != 1 {
		t.Fatalf("expected exactly one successful record, got %d", ok)
	}
}

// rungSink captures records in memory for assertions.
type rungSink struct {
	mu      sync.Mutex
	records []observe.TraceRecord
}

func (c *rungSink) Record(r observe.TraceRecord) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.records = append(c.records, r)
}

// failingProvider always errors, so the run escalates past it.
type failingProvider struct{ err error }

func (p *failingProvider) Name() string        { return "failing" }
func (p *failingProvider) SupportsTools() bool { return true }
func (p *failingProvider) Chat(context.Context, agentcore.ChatRequest) (agentcore.ChatResponse, error) {
	return agentcore.ChatResponse{}, p.err
}
func (p *failingProvider) Stream(context.Context, agentcore.ChatRequest) (<-chan agentcore.ChatDelta, error) {
	return nil, p.err
}
