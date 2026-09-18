package observe

import (
	"context"
	"testing"

	"github.com/lohi-ai/agentray/agentcore"
)

type capabilityInner struct{}

func (capabilityInner) Name() string        { return "inner" }
func (capabilityInner) SupportsTools() bool { return true }
func (capabilityInner) ModelCapabilities(string) agentcore.ModelCapabilities {
	return agentcore.ModelCapabilities{Tools: agentcore.CapabilityUnsupported}
}
func (capabilityInner) Chat(context.Context, agentcore.ChatRequest) (agentcore.ChatResponse, error) {
	return agentcore.AssistantText("ok"), nil
}
func (capabilityInner) Stream(context.Context, agentcore.ChatRequest) (<-chan agentcore.ChatDelta, error) {
	ch := make(chan agentcore.ChatDelta)
	close(ch)
	return ch, nil
}

func TestTracingProviderPreservesModelCapabilities(t *testing.T) {
	wrapped := newTracingProvider(capabilityInner{}, nil, nil)
	if got := wrapped.ModelCapabilities("m").Tools; got != agentcore.CapabilityUnsupported {
		t.Fatalf("wrapped tools capability = %q", got)
	}
}
