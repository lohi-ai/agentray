package agentruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lohi-ai/agentray/agentcore"
	"github.com/lohi-ai/agentray/ai"
)

func writeNativePiMessagesEvents(w http.ResponseWriter, events ...any) {
	fmt.Fprint(w, "data: {\"type\":\"start\"}\n\n")
	for _, event := range events {
		fmt.Fprintf(w, "data: %s\n\n", passiveNativeJSON(event))
	}
}
func nativePiMessagesTerminal(reason string, input, output int) any {
	return map[string]any{"type": "done", "reason": reason, "usage": map[string]any{"input": input, "output": output, "cacheRead": 0, "cacheWrite": 0, "totalTokens": input + output, "cost": map[string]int{"input": 0, "output": 0, "cacheRead": 0, "cacheWrite": 0, "total": 0}}}
}
func writeNativePiMessagesAnswer(w http.ResponseWriter, answer string, input, output int) {
	writeNativePiMessagesEvents(w, map[string]any{"type": "text_start", "contentIndex": 0}, map[string]any{"type": "text_delta", "contentIndex": 0, "delta": answer}, map[string]any{"type": "text_end", "contentIndex": 0, "content": answer}, nativePiMessagesTerminal("stop", input, output))
}
func writeNativePiMessagesCall(w http.ResponseWriter, name, args string, input, output int) {
	var arguments any
	_ = json.Unmarshal([]byte(args), &arguments)
	writeNativePiMessagesEvents(w, map[string]any{"type": "toolcall_start", "contentIndex": 0, "id": "call", "toolName": name}, map[string]any{"type": "toolcall_delta", "contentIndex": 0, "delta": args}, map[string]any{"type": "toolcall_end", "contentIndex": 0, "toolCall": map[string]any{"type": "toolCall", "id": "call", "name": name, "arguments": arguments}}, nativePiMessagesTerminal("toolUse", input, output))
}

func TestNativePiMessagesModelTierHTTPControlsAndDurability(t *testing.T) {
	for _, vendor := range []string{"radius", ai.VendorPiMessages} {
		t.Run(vendor, func(t *testing.T) { testNativeRunnerHTTPProviderControlsAndDurability(t, false, vendor, false) })
	}
}
func TestNativePiMessagesInheritedByChildrenAndSummary(t *testing.T) {
	testNativeHTTPProviderInheritedByChildrenAndSummary(t, false, false, false, false, true)
}

func TestNativePiMessagesDispatcherCallbacks(t *testing.T) {
	sentinel := errors.New("observer failed")
	for _, mode := range []string{"mutate", "null-payload", "observer-error", "primitive-event", "null-event"} {
		t.Run(mode, func(t *testing.T) {
			var count atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				count.Add(1)
				if r.URL.Path != "/v1/messages" || r.URL.Query().Get("debug") != "1" || r.Header.Get("Authorization") != "Bearer key" {
					t.Errorf("wrong request: %s", r.URL)
				}
				body, _ := io.ReadAll(r.Body)
				if mode == "null-payload" {
					if string(body) != "null" {
						t.Errorf("null payload lost: %s", body)
					}
				} else if !strings.Contains(string(body), `"custom":true`) {
					t.Errorf("payload callback ignored: %s", body)
				}
				w.Header().Set("Content-Type", "text/event-stream")
				w.Header().Set("X-Trace", "response")
				writeNativePiMessagesAnswer(w, "original", 2, 3)
			}))
			defer server.Close()
			model := piModelJSON(map[string]any{"id": "test", "api": "pi-messages", "provider": "custom-gateway", "baseUrl": server.URL + "/v1"})
			ctx, capture := ai.WithNativeProviderFailure(context.Background())
			sequence := []string{}
			options := map[string]any{"apiKey": "key", "debug": true, "fetch": server.Client(), "onPayload": func(_ context.Context, raw, received json.RawMessage) (json.RawMessage, error) {
				sequence = append(sequence, "payload")
				if !strings.Contains(string(received), `"provider":"custom-gateway"`) {
					return nil, errors.New("wrong callback model")
				}
				if mode == "null-payload" {
					return json.RawMessage(`null`), nil
				}
				var payload map[string]json.RawMessage
				_ = json.Unmarshal(raw, &payload)
				payload["custom"] = json.RawMessage(`true`)
				return json.Marshal(payload)
			}, "onResponse": func(response ai.CompletionsResponse, _ json.RawMessage) error {
				sequence = append(sequence, "response")
				if response.Status != 200 || response.Headers["x-trace"] != "response" {
					return errors.New("wrong response metadata")
				}
				return nil
			}, "onProviderStreamEvent": func(raw *json.RawMessage, _ json.RawMessage) error {
				sequence = append(sequence, "event")
				if mode == "observer-error" {
					return sentinel
				}
				var event map[string]json.RawMessage
				_ = json.Unmarshal(*raw, &event)
				if string(event["type"]) == `"text_delta"` && mode == "mutate" {
					event["delta"] = json.RawMessage(`"changed"`)
					*raw, _ = json.Marshal(event)
				}
				if string(event["type"]) == `"text_end"` {
					if mode == "mutate" {
						event["content"] = json.RawMessage(`"changed"`)
						*raw, _ = json.Marshal(event)
					}
					if mode == "primitive-event" {
						*raw = json.RawMessage(`true`)
					}
					if mode == "null-event" {
						*raw = json.RawMessage(`null`)
					}
				}
				return nil
			}}
			stream, err := (ai.NativeProvider{}).Stream(ctx, model, ai.NormalizeContext(ai.Context{}), options)
			if err != nil {
				t.Fatal(err)
			}
			wait, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			result, err := stream.Result(wait)
			if err != nil {
				t.Fatal(err)
			}
			if err = stream.WaitForEnd(wait); err != nil {
				t.Fatal(err)
			}
			if count.Load() != 1 || len(sequence) < 3 || sequence[0] != "payload" || sequence[1] != "response" {
				t.Fatalf("callback order %v", sequence)
			}
			if mode == "observer-error" || mode == "null-event" {
				if result.StopReason != "error" || result.Content.Blocks.Len() != 0 {
					t.Fatalf("wrong failure %#v", result)
				}
				if mode == "observer-error" && (!capture.HostFailure() || result.ErrorMessage == nil || *result.ErrorMessage != sentinel.Error()) {
					t.Fatal("observer failure provenance lost")
				}
			} else {
				want := "original"
				if mode == "mutate" {
					want = "changed"
				}
				if result.StopReason != "stop" || result.Content.Blocks.Len() != 1 || result.Content.Blocks.Get(0).Text != want {
					t.Fatalf("wrong result %#v", result)
				}
			}
		})
	}
}

func TestPiMessagesModelBindingAndControls(t *testing.T) {
	tier := ModelTier{TierConfig: TierConfig{Provider: "radius", ProviderID: "gateway-row", Model: "test", BaseURL: "https://gateway.example/v1", APIKey: "stale"}}
	cfg, _, err := tier.BindPi(NativeAgentConfig{}, PiModelOptions{ToolChoice: agentcore.ToolChoice{Mode: agentcore.ToolChoiceNamed, Name: "write"}, RefreshKey: func(context.Context, string) (string, error) { return "fresh", nil }})
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		InitialState struct{ Model json.RawMessage }
	}
	_ = json.Unmarshal(cfg.Options, &wire)
	if !strings.Contains(string(wire.InitialState.Model), `"api":"pi-messages"`) {
		t.Fatal("legacy wire selected")
	}
	key, err := cfg.Callback(context.Background(), "getApiKey", json.RawMessage(`"radius"`), nil)
	if err != nil || string(key) != `"fresh"` {
		t.Fatalf("bound key %s %v", key, err)
	}
	if _, err = cfg.Callback(context.Background(), "getApiKey", json.RawMessage(`"other"`), nil); err == nil {
		t.Fatal("cross-provider credential admitted")
	}
	for _, mode := range []string{"named", "removed", "missing", "no-tools"} {
		t.Run(mode, func(t *testing.T) {
			payload := json.RawMessage(`{"model":"test","context":{"messages":[{"role":"system","toolsAdded":[{"name":"write","parameters":{"type":"object"}}]}]},"options":{"reasoning":"high","toolChoice":"required"},"extension":9007199254740993}`)
			if mode == "removed" {
				payload = json.RawMessage(`{"context":{"messages":[{"role":"system","toolsAdded":[{"name":"write"}]},{"role":"system","toolsRemoved":[{"name":"write"}]}]},"options":{"toolChoice":"required"}}`)
			}
			if mode == "no-tools" {
				payload = json.RawMessage(`{"context":{"messages":[]},"options":{"toolChoice":"required"}}`)
			}
			if mode == "missing" {
				payload = json.RawMessage(`{"context":{"messages":[{"role":"system","toolsAdded":[{"name":"other"}]}]},"options":{}}`)
			}
			result, err := cfg.Callback(context.Background(), "onPayload", piModelJSON(map[string]json.RawMessage{"model": wire.InitialState.Model, "payload": payload}), nil)
			if mode == "missing" {
				if err == nil {
					t.Fatal("unavailable named tool admitted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if mode == "named" {
				if !strings.Contains(string(result), `"toolChoice":{"function":{"name":"write"},"type":"function"}`) || !strings.Contains(string(result), `"extension":9007199254740993`) {
					t.Fatalf("gateway fields lost: %s", result)
				}
			} else if strings.Contains(string(result), "toolChoice") {
				t.Fatalf("tool-free request retained forced choice: %s", result)
			}
		})
	}
	parallel := false
	for _, options := range []PiModelOptions{{ParallelToolCalls: &parallel}, {OutputSchema: &agentcore.OutputSchema{Schema: map[string]any{"type": "object"}}}} {
		if _, _, err := tier.BindPi(NativeAgentConfig{}, options); err == nil {
			t.Fatal("unsupported control silently admitted")
		}
	}
	for _, endpoint := range []string{"", "file:///tmp/gateway", "https://user:pass@gateway.example"} {
		bad := tier
		bad.BaseURL = endpoint
		if _, _, err := bad.BindPi(NativeAgentConfig{}, PiModelOptions{}); err == nil {
			t.Fatal("invalid route admitted")
		}
	}
	failure := errors.New("refresh failed")
	cfg, _, err = tier.BindPi(NativeAgentConfig{}, PiModelOptions{RefreshKey: func(context.Context, string) (string, error) { return "", failure }})
	if err != nil {
		t.Fatal(err)
	}
	if key, err = cfg.Callback(context.Background(), "getApiKey", json.RawMessage(`"radius"`), nil); len(key) > 0 || !errors.Is(err, failure) {
		t.Fatal("failed refresh used stale key")
	}
}

func TestNativePiMessagesTruncatedStreamHasNoEffect(t *testing.T) {
	testNativeStreamFailureHasNoEffect(t, false, false, true)
}

func TestPiMessagesSiblingRowsRequireBoundRefresh(t *testing.T) {
	for _, vendor := range []string{"radius", ai.VendorPiMessages} {
		t.Run(vendor, func(t *testing.T) {
			tier := ModelTier{TierConfig: TierConfig{Provider: vendor, ProviderID: "a", Model: "primary", BaseURL: "https://a.example/v1", Fallback: &TierConfig{Provider: vendor, ProviderID: "b", Model: "secondary", BaseURL: "https://b.example/v1"}}}
			options := PiModelOptions{RefreshKey: func(context.Context, string) (string, error) { return "ambiguous", nil }}
			if _, err := (BuildParams{}).nativeLadderOptions(tier, options); err == nil {
				t.Fatal("vendor-only refresh admitted for sibling rows")
			}
			params := BuildParams{RefreshProviderKey: func(_ context.Context, row, provider, endpoint string) (string, error) {
				if provider != vendor || endpoint != "https://"+row+".example/v1" {
					return "", errors.New("wrong row route")
				}
				return row + "-key", nil
			}}
			resolve, err := params.nativeLadderOptions(tier, options)
			if err != nil {
				t.Fatal(err)
			}
			for _, rung := range tier.resolvedRungs() {
				settings, err := resolve(rung.tier)
				if err != nil {
					t.Fatal(err)
				}
				key, err := settings.RefreshKey(context.Background(), vendor)
				if err != nil || key != rung.tier.ProviderID+"-key" {
					t.Fatalf("credential route lost: %s %v", key, err)
				}
			}
		})
	}
}
