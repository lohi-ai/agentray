package agentcore

import (
	"context"
	"errors"
	"strings"
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

func TestRequestForCapabilitiesDegradesImagesCopyOnWrite(t *testing.T) {
	original := []Message{{
		Role: RoleTool, Content: "plot", ToolCallID: "c1",
		ContentParts: []ContentPart{
			{Type: ContentPartText, Text: "caption"},
			{Type: ContentPartImage, MIMEType: "image/png", Data: "aW1hZ2U="},
		},
	}}
	req := ChatRequest{Messages: original}
	limited := &capabilityProbeProvider{name: "text-only", caps: ModelCapabilities{ImageInput: CapabilityUnsupported}}
	got, err := requestForCapabilities(limited, "m", ModelCapabilities{}, req)
	if err != nil {
		t.Fatalf("requestForCapabilities: %v", err)
	}
	if len(got.Messages[0].ContentParts) != 1 || got.Messages[0].ContentParts[0].Type != ContentPartText {
		t.Fatalf("degraded parts = %+v", got.Messages[0].ContentParts)
	}
	if got.Messages[0].Content != "plot\n[1 image output(s) omitted: provider/model path does not support image input]" {
		t.Fatalf("degraded content = %q", got.Messages[0].Content)
	}
	if len(original[0].ContentParts) != 2 || original[0].Content != "plot" {
		t.Fatalf("capability filtering mutated escalation source: %+v", original[0])
	}

	unknown := &capabilityProbeProvider{name: "unknown"}
	kept, err := requestForCapabilities(unknown, "m", ModelCapabilities{}, req)
	if err != nil || len(kept.Messages[0].ContentParts) != 2 {
		t.Fatalf("unknown image capability should preserve request: %+v err=%v", kept, err)
	}
}

func TestRequestForCapabilitiesClampsOldestImagesToRungLimit(t *testing.T) {
	image := func(data string) ContentPart {
		return ContentPart{Type: ContentPartImage, MIMEType: "image/png", Data: data}
	}
	original := []Message{
		{Role: RoleTool, ToolCallID: "c1", Content: "first", ContentParts: []ContentPart{image("1"), {Type: ContentPartText, Text: "keep"}, image("2")}},
		{Role: RoleTool, ToolCallID: "c2", Content: "second", ContentParts: []ContentPart{image("3"), image("4")}},
		{Role: RoleTool, ToolCallID: "c3", Content: "third", ContentParts: []ContentPart{image("5"), image("6")}},
	}
	provider := &capabilityProbeProvider{name: "limited-vision", caps: ModelCapabilities{
		ImageInput: CapabilitySupported, MaxInputImages: 3,
	}}
	got, err := requestForCapabilities(provider, "m", ModelCapabilities{}, ChatRequest{Messages: original})
	if err != nil {
		t.Fatal(err)
	}
	count := func(messages []Message) int {
		n := 0
		for _, message := range messages {
			for _, part := range message.ContentParts {
				if part.Type == ContentPartImage {
					n++
				}
			}
		}
		return n
	}
	if count(got.Messages) != 3 || count(original) != 6 {
		t.Fatalf("image counts: clamped=%d original=%d", count(got.Messages), count(original))
	}
	if len(got.Messages[0].ContentParts) != 1 || got.Messages[0].ContentParts[0].Type != ContentPartText {
		t.Fatalf("non-image part was not preserved: %+v", got.Messages[0].ContentParts)
	}
	if !strings.Contains(got.Messages[0].Content, "2 older image attachment(s) omitted") ||
		!strings.Contains(got.Messages[1].Content, "1 older image attachment(s) omitted") {
		t.Fatalf("omissions were not visible: %+v", got.Messages)
	}
	if got.Messages[1].ContentParts[0].Data != "4" || got.Messages[2].ContentParts[0].Data != "5" {
		t.Fatalf("did not retain the newest images in order: %+v", got.Messages)
	}
}

func TestRequestForCapabilitiesUsesSafeImageFloorWhenLimitUnknown(t *testing.T) {
	parts := make([]ContentPart, 6)
	for i := range parts {
		parts[i] = ContentPart{Type: ContentPartImage, MIMEType: "image/png", Data: string(rune('a' + i))}
	}
	original := []Message{{Role: RoleUser, ContentParts: parts}}
	got, err := requestForCapabilities(&capabilityProbeProvider{name: "unknown"}, "m", ModelCapabilities{}, ChatRequest{Messages: original})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Messages[0].ContentParts) != unknownProviderImageLimit || got.Messages[0].ContentParts[0].Data != "b" {
		t.Fatalf("unknown provider floor did not drop the oldest image: %+v", got.Messages[0])
	}
	if len(original[0].ContentParts) != 6 {
		t.Fatal("image clamp mutated canonical history")
	}
}

func TestCapabilitiesOfBridgesLegacyProviderToolsFlag(t *testing.T) {
	legacy := NewFauxProvider(AssistantText("ok"))
	if got := CapabilitiesOf(legacy, "m").Tools; got != CapabilitySupported {
		t.Fatalf("legacy tools capability = %q, want supported", got)
	}
}
