package agentcore

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
)

type capabilityProbeProvider struct {
	name     string
	caps     ModelCapabilities
	response ChatResponse
	calls    int32
	recorded []ChatRequest
}

func (p *capabilityProbeProvider) Name() string        { return p.name }
func (p *capabilityProbeProvider) SupportsTools() bool { return true }
func (p *capabilityProbeProvider) ModelCapabilities(string) ModelCapabilities {
	return p.caps
}
func (p *capabilityProbeProvider) Chat(_ context.Context, req ChatRequest) (ChatResponse, error) {
	atomic.AddInt32(&p.calls, 1)
	p.recorded = append(p.recorded, req)
	return p.response, nil
}
func (p *capabilityProbeProvider) Stream(context.Context, ChatRequest) (<-chan ChatDelta, error) {
	return nil, errors.New("unused")
}

type capabilityProbeTool struct{}

func (capabilityProbeTool) Name() string { return "probe" }
func (capabilityProbeTool) Schema() ToolSchema {
	return ToolSchema{Name: "probe", Parameters: map[string]any{"type": "object"}}
}
func (capabilityProbeTool) Run(context.Context, string) (string, error) { return "ok", nil }

func TestUnsupportedToolsSkipPrimaryAndEscalateWithoutCallingIt(t *testing.T) {
	primary := &capabilityProbeProvider{
		name: "local-text-only",
		// The adapter is optimistic, but live discovery for this selected model
		// explicitly said no tools. The rung snapshot must win.
		caps: ModelCapabilities{Tools: CapabilitySupported},
	}
	fallback := &capabilityProbeProvider{
		name:     "cloud-tools",
		caps:     ModelCapabilities{Tools: CapabilitySupported},
		response: AssistantText("fallback answered"),
	}
	agent, err := New(Config{
		Provider: primary, Model: "text-model",
		ModelCapabilities: ModelCapabilities{Tools: CapabilityUnsupported},
		Escalation:        []ModelRung{{Provider: fallback, Model: "tool-model"}},
		Tools:             NewToolSet(capabilityProbeTool{}), Policy: NewAllowList("probe"),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	result, err := agent.Prompt(context.Background(), "use the available tool if needed")
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if result.Final != "fallback answered" {
		t.Fatalf("final = %q", result.Final)
	}
	if got := atomic.LoadInt32(&primary.calls); got != 0 {
		t.Fatalf("incapable primary was called %d times; want 0", got)
	}
	if got := atomic.LoadInt32(&fallback.calls); got != 1 {
		t.Fatalf("capable fallback calls = %d; want 1", got)
	}
	if len(fallback.recorded) != 1 || len(fallback.recorded[0].Tools) != 1 {
		t.Fatalf("fallback request lost tool schemas: %+v", fallback.recorded)
	}
}

func TestRequestForCapabilitiesStripsOnlyExplicitlyUnsupportedHints(t *testing.T) {
	schema := &OutputSchema{Name: "answer", Schema: map[string]any{"type": "object"}}
	req := ChatRequest{
		ReasoningEffort: "high", OutputSchema: schema,
		CacheKey: "session-1", CacheRetention: "long",
	}
	unsupported := &capabilityProbeProvider{name: "limited", caps: ModelCapabilities{
		ReasoningEffort:  CapabilityUnsupported,
		StructuredOutput: CapabilityUnsupported,
		PromptCaching:    CapabilityUnsupported,
	}}
	got, err := requestForCapabilities(unsupported, "m", ModelCapabilities{}, req)
	if err != nil {
		t.Fatalf("requestForCapabilities: %v", err)
	}
	if got.ReasoningEffort != "" || got.OutputSchema != nil || got.CacheKey != "" || got.CacheRetention != "" {
		t.Fatalf("unsupported hints were not stripped: %+v", got)
	}

	unknown := &capabilityProbeProvider{name: "unknown"}
	got, err = requestForCapabilities(unknown, "m", ModelCapabilities{}, req)
	if err != nil {
		t.Fatalf("unknown requestForCapabilities: %v", err)
	}
	if got.ReasoningEffort != req.ReasoningEffort || got.OutputSchema != schema ||
		got.CacheKey != req.CacheKey || got.CacheRetention != req.CacheRetention {
		t.Fatalf("unknown capabilities changed a backward-compatible request: %+v", got)
	}
}

func TestCapabilitiesOfBridgesLegacyProviderToolsFlag(t *testing.T) {
	legacy := NewFauxProvider(AssistantText("ok"))
	if got := CapabilitiesOf(legacy, "m").Tools; got != CapabilitySupported {
		t.Fatalf("legacy tools capability = %q, want supported", got)
	}
}
