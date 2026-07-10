package agentcore

import "testing"

// TestNewProviderResolvesVendors checks the registry maps config→provider with
// zero agent.go edits: built-in vendors resolve, OpenAI-compatible vendors route
// through the OpenAI provider when given a base_url + compat, and an unknown
// vendor with neither is a hard error (§12 AC).
func TestNewProviderResolvesVendors(t *testing.T) {
	cases := []struct {
		name     string
		spec     ProviderSpec
		wantName string
		wantErr  bool
	}{
		{"default empty -> openai", ProviderSpec{}, "openai", false},
		{"explicit openai", ProviderSpec{Name: "openai", APIKey: "k"}, "openai", false},
		{"case-insensitive", ProviderSpec{Name: "OpenAI"}, "openai", false},
		{"anthropic", ProviderSpec{Name: "anthropic", APIKey: "k"}, "anthropic", false},
		{
			"compatible vendor via base_url+compat",
			ProviderSpec{Name: "groq", BaseURL: "https://api.groq.com/openai/v1", Compat: Compat{MaxTokensField: "max_tokens"}},
			"openai", false,
		},
		{"unknown vendor, no compat", ProviderSpec{Name: "mystery"}, "", true},
		{"unknown vendor, compat but no base_url", ProviderSpec{Name: "mystery", Compat: Compat{MaxTokensField: "max_tokens"}}, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, err := NewProvider(tc.spec)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error for %+v", tc.spec)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if p.Name() != tc.wantName {
				t.Errorf("provider name = %q, want %q", p.Name(), tc.wantName)
			}
		})
	}
}

// TestCompactCollapsesOldToolResults verifies compaction elides bulky old tool
// results, preserves the system prompt and recent tail verbatim, and inserts a
// single breadcrumb after the system message.
func TestCompactCollapsesOldToolResults(t *testing.T) {
	big := make([]byte, 512)
	for i := range big {
		big[i] = 'x'
	}
	msgs := []Message{
		{Role: RoleSystem, Content: "you are an analyst"},
		{Role: RoleUser, Content: "find anomalies"},
		{Role: RoleTool, Name: "run_sql", ToolCallID: "1", Content: string(big)},
		{Role: RoleTool, Name: "run_sql", ToolCallID: "2", Content: string(big)},
		{Role: RoleAssistant, Content: "thinking"},
		{Role: RoleTool, Name: "run_sql", ToolCallID: "3", Content: string(big)},
		{Role: RoleAssistant, Content: "recent enough"},
	}
	out := compact(msgs, 2)

	if out[0].Role != RoleSystem {
		t.Fatalf("system prompt must stay first, got %v", out[0].Role)
	}
	if out[1].Role != RoleSystem {
		t.Fatalf("breadcrumb must follow system prompt, got %v", out[1].Role)
	}
	// The recent tail (last 2) must be preserved verbatim.
	last := out[len(out)-1]
	if last.Content != "recent enough" {
		t.Errorf("recent tail mutated: %q", last.Content)
	}
	// At least one older bulky tool result must be elided.
	elided := 0
	for _, m := range out {
		if m.Role == RoleTool && m.Content == "[older tool result elided to fit context]" {
			elided++
		}
	}
	if elided == 0 {
		t.Error("expected at least one elided tool result")
	}
}

// TestShouldCompactBudget checks the heuristic boundary and the zero-budget
// fallback to the default ceiling.
func TestShouldCompactBudget(t *testing.T) {
	small := []Message{{Role: RoleUser, Content: "hi"}}
	if shouldCompact(small, 1000) {
		t.Error("tiny context must not trigger compaction")
	}
	body := make([]byte, 4096)
	msgs := []Message{{Role: RoleUser, Content: string(body)}}
	if !shouldCompact(msgs, 100) { // ~1024 est tokens > 100
		t.Error("large context must trigger compaction at low budget")
	}
	if shouldCompact(msgs, 0) { // 0 -> default 96k budget, 1024 est < that
		t.Error("zero budget must fall back to the default ceiling")
	}
}
