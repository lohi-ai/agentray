package httptool_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lohi-ai/agentray/agentcore"
	"github.com/lohi-ai/agentray/internal/credential"
	"github.com/lohi-ai/agentray/internal/httptool"
)

// TestEndToEndCredentialReachesServerNotTrace is the worked-consumer proof: the
// model emits {{cred:API_KEY}} in an Authorization header, the vault resolves it
// at the trust boundary, the http_request tool sends the *real* bearer to the
// destination — and the persisted trace still shows only the placeholder.
func TestEndToEndCredentialReachesServerNotTrace(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	vault := credential.NewVault()
	if err := vault.Put("API_KEY", "sk-real-secret"); err != nil {
		t.Fatalf("Put: %v", err)
	}

	tool := httptool.New(
		httptool.WithAllowHosts([]string{"127.0.0.1"}),
		httptool.WithAllowPlainHTTP(true), // httptest serves plain http
	)
	tool.AllowAllIPsForTest() // httptest is on loopback, normally refused

	args, _ := json.Marshal(map[string]any{
		"method":  "GET",
		"url":     srv.URL,
		"headers": map[string]string{"Authorization": "Bearer {{cred:API_KEY}}"},
	})

	env := agentcore.DefaultEnv()
	env.Credentials = vault
	faux := agentcore.NewFauxProvider(
		agentcore.AssistantToolCall("c1", httptool.ToolHTTPRequest, string(args)),
		agentcore.AssistantText("done"),
	)
	agent, err := agentcore.New(agentcore.Config{
		Provider: faux,
		Model:    "test",
		Tools:    agentcore.NewToolSet(tool),
		Policy:   agentcore.NewAllowList(httptool.ToolHTTPRequest),
		Env:      &env,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	res, err := agent.Prompt(context.Background(), "fetch it")
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}

	// The destination received the resolved secret.
	if gotAuth != "Bearer sk-real-secret" {
		t.Fatalf("server Authorization = %q, want resolved bearer", gotAuth)
	}
	// The trace kept the placeholder — the secret never persisted.
	if len(res.Tools) != 1 {
		t.Fatalf("expected 1 trace, got %d", len(res.Tools))
	}
	if !strings.Contains(res.Tools[0].Args, "{{cred:API_KEY}}") {
		t.Fatalf("trace should keep the placeholder, got %q", res.Tools[0].Args)
	}
	if strings.Contains(res.Tools[0].Args, "sk-real-secret") {
		t.Fatal("secret leaked into the tool trace")
	}
}

// TestEndToEndBlocksNonAllowlistedHost confirms the tool refuses an off-allowlist
// host and feeds the reason back to the model rather than making the call.
func TestEndToEndBlocksNonAllowlistedHost(t *testing.T) {
	tool := httptool.New(httptool.WithAllowHosts([]string{"api.allowed.com"}))
	args, _ := json.Marshal(map[string]any{"url": "https://evil.example.com/steal"})
	faux := agentcore.NewFauxProvider(
		agentcore.AssistantToolCall("c1", httptool.ToolHTTPRequest, string(args)),
		agentcore.AssistantText("understood"),
	)
	agent, err := agentcore.New(agentcore.Config{
		Provider: faux,
		Model:    "test",
		Tools:    agentcore.NewToolSet(tool),
		Policy:   agentcore.NewAllowList(httptool.ToolHTTPRequest),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	res, err := agent.Prompt(context.Background(), "go")
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	var sawErr bool
	for _, m := range res.Messages {
		if m.Role == agentcore.RoleTool && strings.Contains(m.Content, "allowlist") {
			sawErr = true
		}
	}
	if !sawErr {
		t.Fatal("expected the allowlist refusal to be returned to the model")
	}
}
