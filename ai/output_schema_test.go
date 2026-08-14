package ai

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/lohi-ai/agentray/agentcore"
)

func verdictSchema() *agentcore.OutputSchema {
	return &agentcore.OutputSchema{
		Name: "verdict",
		Schema: map[string]any{
			"type":                 "object",
			"properties":           map[string]any{"verdict": map[string]any{"type": "string"}},
			"required":             []any{"verdict"},
			"additionalProperties": false,
		},
	}
}

// TestOpenAIEncodesOutputSchema pins the OpenAI wire mapping: agentcore.OutputSchema →
// response_format json_schema — best-effort (strict:false) by default so a
// draft-07 schema outside OpenAI's strict subset soft-degrades instead of
// 400ing, strict:true only on caller opt-in, and absent entirely when unset so
// strict compat servers never see the field.
func TestOpenAIEncodesOutputSchema(t *testing.T) {
	p := &OpenAIProvider{}
	raw, err := json.Marshal(p.encode(agentcore.ChatRequest{Model: "m", OutputSchema: verdictSchema()}))
	if err != nil {
		t.Fatal(err)
	}
	body := string(raw)
	for _, want := range []string{`"response_format"`, `"type":"json_schema"`, `"name":"verdict"`, `"strict":false`, `"verdict"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("payload missing %s: %s", want, body)
		}
	}

	// Strict is opt-in: only a schema the caller vouches fits OpenAI's strict
	// subset asks for grammar-constrained decoding.
	strict := verdictSchema()
	strict.Strict = true
	raw, _ = json.Marshal(p.encode(agentcore.ChatRequest{Model: "m", OutputSchema: strict}))
	if !strings.Contains(string(raw), `"strict":true`) {
		t.Fatalf("opt-in schema must emit strict:true: %s", raw)
	}

	raw, _ = json.Marshal(p.encode(agentcore.ChatRequest{Model: "m"}))
	if strings.Contains(string(raw), "response_format") {
		t.Fatalf("unset schema must not emit response_format: %s", raw)
	}

	// A nameless schema still satisfies OpenAI's required name.
	s := verdictSchema()
	s.Name = ""
	raw, _ = json.Marshal(p.encode(agentcore.ChatRequest{Model: "m", OutputSchema: s}))
	if !strings.Contains(string(raw), `"name":"output"`) {
		t.Fatalf("empty name must default to output: %s", raw)
	}
}

// TestAnthropicEncodesOutputSchema pins the Anthropic wire mapping: agentcore.OutputSchema
// → output_format json_schema plus the structured-outputs beta header.
func TestAnthropicEncodesOutputSchema(t *testing.T) {
	p := &AnthropicProvider{}
	req := agentcore.ChatRequest{Model: "m", OutputSchema: verdictSchema()}
	raw, err := json.Marshal(p.encode(req))
	if err != nil {
		t.Fatal(err)
	}
	body := string(raw)
	for _, want := range []string{`"output_format"`, `"type":"json_schema"`, `"schema"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("payload missing %s: %s", want, body)
		}
	}
	if got := antBetaHeader(req); got != anthropicStructuredOutputBeta {
		t.Fatalf("beta header = %q", got)
	}
	// Betas compose: extended cache + structured outputs join with a comma.
	req.CacheKey, req.CacheRetention = "k", "long"
	if got := antBetaHeader(req); got != anthropicExtendedCacheBeta+","+anthropicStructuredOutputBeta {
		t.Fatalf("combined beta header = %q", got)
	}

	raw, _ = json.Marshal(p.encode(agentcore.ChatRequest{Model: "m"}))
	if strings.Contains(string(raw), "output_format") {
		t.Fatalf("unset schema must not emit output_format: %s", raw)
	}
	if got := antBetaHeader(agentcore.ChatRequest{Model: "m"}); got != "" {
		t.Fatalf("no betas must mean no header, got %q", got)
	}
}

// TestLoopThreadsOutputSchema verifies agentcore.Config.OutputSchema reaches every
// provider request the loop issues.
func TestLoopThreadsOutputSchema(t *testing.T) {
	provider := agentcore.NewFauxProvider(agentcore.AssistantText(`{"verdict":"ok"}`))
	agent, err := agentcore.New(agentcore.Config{
		Provider:     provider,
		Model:        "test",
		OutputSchema: verdictSchema(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := agent.Prompt(context.Background(), "judge this"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if len(provider.Recorded) != 1 || provider.Recorded[0].OutputSchema == nil {
		t.Fatalf("request missing OutputSchema: %+v", provider.Recorded)
	}
	if provider.Recorded[0].OutputSchema.Name != "verdict" {
		t.Fatalf("schema name = %q", provider.Recorded[0].OutputSchema.Name)
	}
}
