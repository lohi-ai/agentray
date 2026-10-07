package agentruntime

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/2found/2ai/agentcore"
	"github.com/2found/2ai/ai"
)

type nativeAntigravityAccountSource struct{ acquired *atomic.Int32 }

func (s *nativeAntigravityAccountSource) Acquire(context.Context) (ai.OAuthToken, error) {
	return ai.OAuthToken{AccountID: "account", AccessToken: fmt.Sprintf("refreshed-%d", s.acquired.Add(1)), ProjectID: "native-project"}, nil
}
func (*nativeAntigravityAccountSource) Report(context.Context, ai.OAuthToken, error) {}
func writeNativeAntigravity(w http.ResponseWriter, parts []map[string]any, input, output int) {
	w.Header().Set("Content-Type", "text/event-stream")
	fmt.Fprintf(w, "data: %s\n\n", passiveNativeJSON(map[string]any{"response": map[string]any{"candidates": []any{map[string]any{"content": map[string]any{"parts": parts}, "finishReason": "STOP"}}, "usageMetadata": map[string]int{"promptTokenCount": input, "candidatesTokenCount": output}}}))
}
func TestNativeAntigravityChildrenAndSummary(t *testing.T) {
	testNativeHTTPProviderInheritedByChildrenAndSummary(t, false, false, false, false, false, ai.VendorGoogleAntigravity)
}
func TestNativeAntigravityToolsAndDurability(t *testing.T) {
	var requests, effects, acquisitions atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := requests.Add(1)
		var body struct {
			Project string
			Request struct {
				Contents         json.RawMessage
				ToolConfig       struct{ FunctionCallingConfig struct{ Mode string } }
				GenerationConfig struct{ ThinkingConfig struct{ ThinkingBudget int } }
			}
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			return
		}
		if body.Project != "native-project" || r.Header.Get("Authorization") != fmt.Sprintf("Bearer refreshed-%d", n) {
			t.Error("lost account/project")
		}
		if body.Request.GenerationConfig.ThinkingConfig.ThinkingBudget != 24575 {
			t.Error("lost reasoning control")
		}
		if n == 1 {
			if body.Request.ToolConfig.FunctionCallingConfig.Mode != "ANY" {
				t.Error("lost required tool choice")
			}
			writeNativeAntigravity(w, []map[string]any{{"text": "private reasoning", "thought": true, "thoughtSignature": "c2ln"}, {"functionCall": map[string]any{"id": "call", "name": "write", "args": map[string]any{}}, "thoughtSignature": "dG9vbA=="}}, 3, 1)
		} else {
			if body.Request.ToolConfig.FunctionCallingConfig.Mode != "" {
				t.Error("forced choice remained at ceiling")
			}
			if !strings.Contains(string(body.Request.Contents), `"thoughtSignature":"dG9vbA=="`) || !strings.Contains(string(body.Request.Contents), `"functionResponse"`) {
				t.Errorf("lost replay: %s", body.Request.Contents)
			}
			writeNativeAntigravity(w, []map[string]any{{"text": "done"}}, 9, 2)
		}
	}))
	defer server.Close()
	p := representativeBuildParams()
	p.Sandbox, p.HTTPTool, p.Subagents = nil, nil, nil
	p.Goal, p.PrepareNextTurn, p.RefreshKey = "", nil, nil
	p.Tools = []agentcore.Tool{nativeChildWrite{&effects}}
	p.MaxTurns = 1
	p.ToolChoice = agentcore.ToolChoice{Mode: agentcore.ToolChoiceRequired}
	tier := ModelTier{TierConfig: TierConfig{Provider: ai.VendorGoogleAntigravity, Model: "native-http", BaseURL: server.URL, TokenSource: &nativeAntigravityAccountSource{&acquisitions}}}
	runner := NewRunner(nil, WithPiRuntime(PiRuntimeConfig{}))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result, err := runner.runModelLoop(ctx, p, RunOptions{Prompt: "write", ReasoningEffort: "xhigh"}, tier, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Final != "done" || effects.Load() != 1 || requests.Load() != 2 || acquisitions.Load() != 2 || result.Usage.InputTokens != 12 || result.Usage.OutputTokens != 3 {
		t.Fatalf("result=%+v requests=%d effects=%d acquisitions=%d", result, requests.Load(), effects.Load(), acquisitions.Load())
	}
	if len(result.NativeTelemetry) == 0 || strings.Contains(string(result.NativeState)+string(result.NativeTelemetry), "refreshed-") {
		t.Fatal("bad native artifacts")
	}
	entries, err := p.Session.Log(ctx, p.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	starts, done := 0, 0
	for _, entry := range entries {
		if entry.Kind == piEffectStart {
			starts++
		}
		if entry.Kind == piEffectDone {
			done++
		}
	}
	if starts != 1 || done != 1 {
		t.Fatalf("effect journal %d/%d", starts, done)
	}
	if _, err := recoverPiState(entries); err != nil {
		t.Fatal(err)
	}
}
