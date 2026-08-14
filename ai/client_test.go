package ai

import "testing"

// TestNewClientResolvesVendors checks NewClient maps config→wire client with
// zero loop edits: built-in vendors resolve, OpenAI-compatible vendors route
// through the OpenAI provider when given a base_url + compat, and an unknown
// vendor with neither is a hard error (§12 AC).
func TestNewClientResolvesVendors(t *testing.T) {
	cases := []struct {
		name     string
		spec     ClientSpec
		wantName string
		wantErr  bool
	}{
		{"default empty -> openai", ClientSpec{}, "openai", false},
		{"explicit openai", ClientSpec{Name: "openai", APIKey: "k"}, "openai", false},
		{"case-insensitive", ClientSpec{Name: "OpenAI"}, "openai", false},
		{"anthropic", ClientSpec{Name: "anthropic", APIKey: "k"}, "anthropic", false},
		{"google", ClientSpec{Name: "google", APIKey: "k"}, "google", false},
		{"gemini alias", ClientSpec{Name: "Gemini", APIKey: "k"}, "google", false},
		{
			// Compat vendors keep their own identity so traces and per-turn key
			// refresh attribute to the vendor's tier, not "openai".
			"compatible vendor via base_url+compat",
			ClientSpec{Name: "groq", BaseURL: "https://api.groq.com/openai/v1", Compat: Compat{MaxTokensField: "max_tokens"}},
			"groq", false,
		},
		{"unknown vendor, no compat", ClientSpec{Name: "mystery"}, "", true},
		{"unknown vendor, compat but no base_url", ClientSpec{Name: "mystery", Compat: Compat{MaxTokensField: "max_tokens"}}, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, err := NewClient(tc.spec)
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
