package ai

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/lohi-ai/agentray/agentcore"
)

func TestWiredListModelsOverlaysAndRetainsLiveCapabilities(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{
			"id":"text-only",
			"supports_tools":false,
			"supports_reasoning":true,
			"max_input_images":7,
			"capabilities":{"prompt_caching":"supported"},
			"supported_parameters":["response_format"],
			"architecture":{"input_modalities":["text"]}
		}]}`))
	}))
	defer srv.Close()

	provider, err := New(Spec{ID: "row", Vendor: "openai", BaseURL: srv.URL, HTTP: srv.Client()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	models, err := provider.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if len(models) != 1 {
		t.Fatalf("models = %d, want 1", len(models))
	}
	caps := models[0].Capabilities
	if caps.Tools != agentcore.CapabilityUnsupported ||
		caps.ReasoningEffort != agentcore.CapabilitySupported ||
		caps.StructuredOutput != agentcore.CapabilitySupported ||
		caps.PromptCaching != agentcore.CapabilitySupported ||
		caps.ImageInput != agentcore.CapabilityUnsupported || caps.MaxInputImages != 7 {
		t.Fatalf("listed capabilities = %+v", caps)
	}

	capable, ok := provider.(agentcore.ModelCapabilityProvider)
	if !ok {
		t.Fatal("wired provider lost ModelCapabilityProvider")
	}
	retained := capable.ModelCapabilities("text-only")
	if retained.Tools != agentcore.CapabilityUnsupported || retained.ReasoningEffort != agentcore.CapabilitySupported {
		t.Fatalf("retained capabilities = %+v", retained)
	}
}

func TestDiscoveredCapabilitiesNeverTreatsMissingMetadataAsUnsupported(t *testing.T) {
	got := (discoveredCapabilities{}).modelCapabilities()
	if got != (agentcore.ModelCapabilities{}) {
		t.Fatalf("empty discovery = %+v, want entirely unknown", got)
	}
}

func TestDedicatedCapabilityBooleanWinsOverGenericHints(t *testing.T) {
	no := false
	got := (discoveredCapabilities{
		SupportsTools:       &no,
		SupportedParameters: []string{"tools"},
	}).modelCapabilities()
	if got.Tools != agentcore.CapabilityUnsupported {
		t.Fatalf("tools = %q, explicit supports_tools:false must win", got.Tools)
	}
}

func TestCapabilityOverlayUsesOnlyKnownNewValues(t *testing.T) {
	base := agentcore.ModelCapabilities{
		Tools:          agentcore.CapabilitySupported,
		PromptCaching:  agentcore.CapabilitySupported,
		MaxInputImages: 90,
	}
	got := base.Overlay(agentcore.ModelCapabilities{Tools: agentcore.CapabilityUnsupported})
	if got.Tools != agentcore.CapabilityUnsupported || got.PromptCaching != agentcore.CapabilitySupported || got.MaxInputImages != 90 {
		t.Fatalf("overlay = %+v", got)
	}
	got = got.Overlay(agentcore.ModelCapabilities{MaxInputImages: 12})
	if got.MaxInputImages != 12 {
		t.Fatalf("numeric capability did not overlay: %+v", got)
	}
}

func TestDiscoveredImageLimitAcceptsDedicatedAndCapabilityMapShapes(t *testing.T) {
	twelve := 12
	got := (discoveredCapabilities{
		MaxImages: &twelve,
		Capabilities: map[string]json.RawMessage{
			"max_input_images": json.RawMessage(`9`),
		},
	}).modelCapabilities()
	if got.MaxInputImages != 12 {
		t.Fatalf("dedicated max_images should win over generic map: %+v", got)
	}
}

func TestModelCapabilitiesRejectsNegativeImageLimit(t *testing.T) {
	if err := (agentcore.ModelCapabilities{MaxInputImages: -1}).Validate(); err == nil {
		t.Fatal("negative image limit must be rejected")
	}
}

func TestOpenAIResponsesAdvertisesStatefulWireWithoutChangingChatCompletions(t *testing.T) {
	responses := CapabilitiesFor(VendorOpenAIResponses, "gpt-test")
	if responses.StatefulResponses != agentcore.CapabilitySupported ||
		responses.StructuredOutput != agentcore.CapabilitySupported ||
		responses.Tools != agentcore.CapabilitySupported || responses.MaxInputImages != 200 {
		t.Fatalf("responses capabilities = %+v", responses)
	}
	if chat := CapabilitiesFor("openai", "gpt-test"); chat.StatefulResponses != agentcore.CapabilityUnsupported {
		t.Fatalf("chat-completions stateful capability = %q, want unsupported", chat.StatefulResponses)
	}
}

func TestProviderImageBudgetsArePortableAndAntigravityIsExplicitlyTextOnly(t *testing.T) {
	for vendor, want := range map[string]int{
		"openai": 200, VendorOpenAIResponses: 200, "anthropic": 90,
		VendorClaudeCode: 90, VendorOpenAICodex: 200, "google": 200, "openrouter": 90,
	} {
		caps := CapabilitiesFor(vendor, "m")
		if caps.ImageInput != agentcore.CapabilitySupported || caps.MaxInputImages != want {
			t.Errorf("%s image capabilities = %+v, want supported/%d", vendor, caps, want)
		}
	}
	if got := CapabilitiesFor(VendorGoogleAntigravity, "m"); got.ImageInput != agentcore.CapabilityUnsupported {
		t.Fatalf("antigravity image capability = %+v", got)
	}
}
