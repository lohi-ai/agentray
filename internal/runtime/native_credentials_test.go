package agentruntime

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lohi-ai/agentray/agentcore"
	storage "github.com/lohi-ai/agentray/internal/dataplane/store"
)

func TestNativeRefreshedKeyRequiresExactRowAndRoute(t *testing.T) {
	a := TierConfig{ProviderID: "a", Provider: "openai", BaseURL: "https://same.test/v1", APIKey: "a-current"}
	b := a
	b.ProviderID, b.APIKey = "b", "b-current"
	for _, target := range []TierConfig{a, b} {
		key, err := nativeRefreshedKey(target.ProviderID, "OpenAI", " https://same.test/v1/ ", []TierConfig{b, a})
		if err != nil || key != target.APIKey {
			t.Fatal("row identity did not select the current credential", err)
		}
	}
	for _, mode := range []string{"missing", "vendor", "endpoint", "empty", "conflicting", "rowless"} {
		t.Run(mode, func(t *testing.T) {
			changed := a
			switch mode {
			case "missing":
				changed.ProviderID = "removed"
			case "vendor":
				changed.Provider = "anthropic"
			case "endpoint":
				changed.BaseURL = "https://changed.test/v1"
			case "empty":
				changed.APIKey = ""
			case "conflicting":
				changed.APIKey = "different"
			case "rowless":
				changed.ProviderID = ""
			}
			candidates := []TierConfig{b, changed}
			if mode == "conflicting" {
				candidates = append(candidates, a)
			}
			key, err := nativeRefreshedKey("a", a.Provider, a.BaseURL, candidates)
			if err == nil || key != "" || strings.Contains(err.Error(), "a-current") || strings.Contains(err.Error(), "b-current") {
				t.Fatal("invalid row reused/exposed a credential")
			}
		})
	}
}

func TestNativeRefreshedKeyWithoutRowRequiresUnambiguousRoute(t *testing.T) {
	bound := TierConfig{Provider: "openai", APIKey: "host-current"}
	if key, err := nativeRefreshedKey("", "", "", []TierConfig{bound, bound}); err != nil || key != bound.APIKey {
		t.Fatal("inherited host key not resolved", err)
	}
	for _, changed := range []TierConfig{
		{Provider: "openai", APIKey: "another-key"},
		{Provider: "openai", APIKey: ""},
	} {
		if key, err := nativeRefreshedKey("", "openai", "", []TierConfig{bound, changed}); err == nil || key != "" {
			t.Fatal("ambiguous/revoked rowless key accepted")
		}
	}
	for _, changed := range []TierConfig{
		{ProviderID: "workspace-row", Provider: "openai", APIKey: "workspace-key"},
		{Provider: "openai", BaseURL: "https://different.test/v1", APIKey: "endpoint-key"},
	} {
		if key, err := nativeRefreshedKey("", "openai", "", []TierConfig{changed}); err == nil || key != "" {
			t.Fatal("rowless binding migrated implicitly")
		}
	}
}

func TestNativeHostedCredentialDoesNotOwnWorkspaceRow(t *testing.T) {
	cfg := storage.WorkspaceModelTiers{HostedDefault: true, Provider: "openai", BaseURL: "https://host.test/v1", Model: "host-model", FlashProviderID: "empty-workspace-row", LiteProviderID: "another-empty-row", ProProviderID: "third-empty-row", ModelFallback: true, FallbackModel: "fallback", FallbackProviderID: "configured-fallback", FallbackProvider: "openai", FallbackBaseURL: "https://fallback.test/v1"}
	tiers := TierSetFromWorkspace(cfg, map[string]string{"flash": "host-current", "lite": "host-current", "pro": "host-current"}, nil)
	for _, name := range []Tier{TierFlash, TierLite, TierPro} {
		tier := tiers.For(name)
		if tier.ProviderID != "" || tier.APIKey != "host-current" {
			t.Fatal("host credential inherited an unrelated workspace row")
		}
		if tier.Fallback == nil || tier.Fallback.ProviderID != "configured-fallback" {
			t.Fatal("explicit fallback provider row was replaced by host defaults")
		}
	}
	if cfg.FlashProviderID != "empty-workspace-row" {
		t.Fatal("resolution rewrote persisted workspace selection")
	}
}

func TestNativeCredentialRefreshAcrossHTTPRetryFallbackAndResume(t *testing.T) {
	for _, mutation := range []string{"rotate", "route-change", "revoke"} {
		t.Run(mutation, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			var mu sync.Mutex
			var records []TierConfig
			var admitted []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body struct{ Model string }
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				mu.Lock()
				defer mu.Unlock()
				admitted = append(admitted, r.Header.Get("Authorization"))
				if body.Model == "primary" {
					if len(admitted) == 1 {
						switch mutation {
						case "rotate":
							records[0].APIKey = "a2"
						case "route-change":
							records[0].BaseURL = "https://changed.test/v1"
						case "revoke":
							records[0].APIKey = ""
						}
					}
					w.WriteHeader(503)
					_, _ = w.Write([]byte(`{"error":{"message":"temporarily unavailable"}}`))
					return
				}
				if body.Model != "fallback" {
					t.Error("unexpected model")
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"answer\"},\"finish_reason\":null}]}\n\ndata: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":2,\"completion_tokens\":3}}\n\ndata: [DONE]\n\n")
			}))
			defer server.Close()
			records = []TierConfig{{ProviderID: "a", Provider: "openai", BaseURL: server.URL, APIKey: "a1"}, {ProviderID: "b", Provider: "openai", BaseURL: server.URL, APIKey: "b1"}}
			params := BuildParams{RefreshKey: func(context.Context, string) (string, error) {
				t.Error("used ambiguous vendor-only refresh")
				return "", nil
			}, RefreshProviderKey: func(_ context.Context, id, vendor, endpoint string) (string, error) {
				mu.Lock()
				defer mu.Unlock()
				return nativeRefreshedKey(id, vendor, endpoint, records)
			}}
			tier := ModelTier{TierConfig: records[0]}
			tier.Model = "primary"
			fallback := records[1]
			fallback.Model = "fallback"
			tier.Fallback = &fallback
			ladder, err := newNativeModelLadder(tier, agentcore.PiConfig{}, func(rung ModelTier) (PiModelOptions, error) {
				return params.nativeModelOptions(rung, PiModelOptions{RefreshKey: params.RefreshKey}), nil
			})
			if err != nil {
				t.Fatal(err)
			}
			binding, _, stream := ladder.sessionBinding()
			cfg := PiRunConfig{Input: json.RawMessage(`"answer"`), Session: PiSessionConfig{NativeGo: true, Pi: binding, NativeStream: stream, nativeLadder: ladder, nativeAttempts: &agentcore.RetryPolicy{MaxAttempts: 2, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond}, Store: agentcore.NewMemorySessionStore(), SessionID: "row-refresh"}}
			result, err := RunPi(ctx, cfg)
			if mutation != "rotate" {
				mu.Lock()
				defer mu.Unlock()
				if err == nil || len(admitted) != 1 || ladder.selection().Rung != 0 {
					t.Fatal("changed credential retried or escalated", err, len(admitted))
				}
				return
			}
			if err != nil || result.Projection.Final != "answer" || ladder.selection().Rung != 1 {
				t.Fatal("fallback did not complete", err)
			}
			mu.Lock()
			records[1].APIKey = "b2"
			mu.Unlock()
			cfg.Session.Resume = true
			result, err = RunPi(ctx, cfg)
			if err != nil || result.Projection.Final != "answer" {
				t.Fatal("resumed fallback failed", err)
			}
			mu.Lock()
			defer mu.Unlock()
			if !reflect.DeepEqual(admitted, []string{"Bearer a1", "Bearer a2", "Bearer b1", "Bearer b2"}) {
				t.Fatal("HTTP requests used incorrect credential rows/versions")
			}
		})
	}
}
