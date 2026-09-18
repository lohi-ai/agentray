package ai

import (
	"context"
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
		caps.ImageInput != agentcore.CapabilityUnsupported {
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
		Tools:         agentcore.CapabilitySupported,
		PromptCaching: agentcore.CapabilitySupported,
	}
	got := base.Overlay(agentcore.ModelCapabilities{Tools: agentcore.CapabilityUnsupported})
	if got.Tools != agentcore.CapabilityUnsupported || got.PromptCaching != agentcore.CapabilitySupported {
		t.Fatalf("overlay = %+v", got)
	}
}

func TestOpenAIResponsesAdvertisesStatefulWireWithoutChangingChatCompletions(t *testing.T) {
	responses := CapabilitiesFor(VendorOpenAIResponses, "gpt-test")
	if responses.StatefulResponses != agentcore.CapabilitySupported ||
		responses.StructuredOutput != agentcore.CapabilitySupported ||
		responses.Tools != agentcore.CapabilitySupported {
		t.Fatalf("responses capabilities = %+v", responses)
	}
	if chat := CapabilitiesFor("openai", "gpt-test"); chat.StatefulResponses != agentcore.CapabilityUnsupported {
		t.Fatalf("chat-completions stateful capability = %q, want unsupported", chat.StatefulResponses)
	}
}
