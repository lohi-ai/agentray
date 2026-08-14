package ai

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// New is the one construction path shared by a run and a list-models call, so
// identity resolution has to be right: an empty ID or Name falls back to the
// normalized vendor, and aliases collapse before anything downstream sees them.
func TestNew_ResolvesIdentityFromSpec(t *testing.T) {
	cases := []struct {
		name                  string
		spec                  Spec
		wantID, wantVendor    string
		wantName, wantBaseURL string
	}{
		{
			name:   "empty id and name fall back to the vendor",
			spec:   Spec{Vendor: "openai"},
			wantID: "openai", wantVendor: "openai", wantName: "openai", wantBaseURL: "",
		},
		{
			name:   "gemini is an alias for google",
			spec:   Spec{Vendor: "gemini", ID: "row-7", Name: "My Gemini"},
			wantID: "row-7", wantVendor: "google", wantName: "My Gemini", wantBaseURL: "",
		},
		{
			name:   "base URL is trimmed of its trailing slash",
			spec:   Spec{Vendor: "anthropic", BaseURL: "https://gw.example/  "},
			wantID: "anthropic", wantVendor: "anthropic", wantName: "anthropic",
			wantBaseURL: "https://gw.example",
		},
		{
			name:   "an unknown vendor stays itself once it has a base URL",
			spec:   Spec{Vendor: "9router", BaseURL: "https://9router.example/v1"},
			wantID: "9router", wantVendor: "9router", wantName: "9router",
			wantBaseURL: "https://9router.example/v1",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p, err := New(c.spec)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			if p.ID() != c.wantID || p.Vendor() != c.wantVendor ||
				p.DisplayName() != c.wantName || p.BaseURL() != c.wantBaseURL {
				t.Fatalf("id=%q vendor=%q name=%q base=%q; want %q/%q/%q/%q",
					p.ID(), p.Vendor(), p.DisplayName(), p.BaseURL(),
					c.wantID, c.wantVendor, c.wantName, c.wantBaseURL)
			}
			if !p.SupportsTools() {
				t.Error("SupportsTools() = false; every built-in wire supports tools")
			}
		})
	}
}

// An unknown vendor with no base URL has nowhere to send its traffic. Failing
// at construction beats discovering it on the first turn of a run.
func TestNew_UnknownVendorWithoutBaseURLFails(t *testing.T) {
	_, err := New(Spec{Vendor: "mystery-gateway"})
	if err == nil {
		t.Fatal("unknown vendor without a base URL constructed successfully")
	}
	if !strings.Contains(err.Error(), "base URL") {
		t.Fatalf("err = %v, want it to name the missing base URL", err)
	}
}

// The injected client must reach the *wire* provider, not just be held by the
// wrapper — otherwise a test server or proxy configured by the caller applies
// to list-models but silently not to Chat.
func TestNew_InjectsHTTPClientIntoTheWireProvider(t *testing.T) {
	client := &http.Client{Timeout: 3 * time.Second}

	for _, vendor := range []string{"openai", "anthropic"} {
		p, err := New(Spec{Vendor: vendor, BaseURL: "https://gw.example", HTTP: client})
		if err != nil {
			t.Fatalf("New(%s): %v", vendor, err)
		}
		inner := p.(*wired).inner
		var got *http.Client
		switch w := inner.(type) {
		case *OpenAIProvider:
			got = w.HTTP
		case *AnthropicProvider:
			got = w.HTTP
		default:
			t.Fatalf("%s: unexpected wire type %T", vendor, inner)
		}
		if got != client {
			t.Fatalf("%s: wire provider kept its own client, injection did not reach it", vendor)
		}
	}
}

// A refreshed BYO token has to reach the wire provider too; updating only the
// wrapper leaves every subsequent request authenticating with the dead key.
func TestWired_UpdateAPIKeyReachesTheWireProvider(t *testing.T) {
	p, err := New(Spec{Vendor: "openai", APIKey: "old"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	w := p.(*wired)
	w.UpdateAPIKey("new")
	if w.APIKey() != "new" {
		t.Fatalf("wrapper key = %q, want new", w.APIKey())
	}
	if inner := w.inner.(*OpenAIProvider); inner.APIKey != "new" {
		t.Fatalf("wire key = %q, want new", inner.APIKey)
	}
	// An empty key is a no-op, not a way to erase the credential.
	w.UpdateAPIKey("")
	if w.APIKey() != "new" {
		t.Fatalf("empty update erased the key: %q", w.APIKey())
	}
}

// With no explicit base URL each vendor must fall back to its own default;
// sharing one fallback would send Anthropic traffic to OpenAI's host.
func TestWired_EffectiveBaseURLPerVendor(t *testing.T) {
	want := map[string]string{
		"openai":    defaultOpenAIBaseURL,
		"anthropic": defaultAnthropicBaseURL,
		"google":    defaultGoogleBaseURL,
	}
	for vendor, url := range want {
		p, err := New(Spec{Vendor: vendor})
		if err != nil {
			t.Fatalf("New(%s): %v", vendor, err)
		}
		if got := p.(*wired).effectiveBaseURL(); got != url {
			t.Fatalf("%s effective base = %q, want %q", vendor, got, url)
		}
	}
	// An explicit base URL always wins over the vendor default.
	p, _ := New(Spec{Vendor: "openai", BaseURL: "https://gw.example/v1"})
	if got := p.(*wired).effectiveBaseURL(); got != "https://gw.example/v1" {
		t.Fatalf("explicit base URL was ignored: %q", got)
	}
}

// ListModels stamps provider identity onto every model so a collection can route
// a model id back to its owner. An unstamped row is unroutable.
func TestWired_ListModelsStampsProviderIdentity(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer sk-test" {
			t.Errorf("Authorization = %q, want the spec's key", got)
		}
		w.Write([]byte(`{"data":[{"id":"gpt-5"},{"id":"gpt-5-mini"}]}`))
	}))
	defer srv.Close()

	p, err := New(Spec{ID: "row-1", Vendor: "openai", Name: "House GW",
		APIKey: "sk-test", BaseURL: srv.URL, HTTP: srv.Client()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	models, err := p.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if len(models) != 2 {
		t.Fatalf("got %d models, want 2", len(models))
	}
	m := models[0]
	if m.ProviderID != "row-1" || m.ProviderVendor != "openai" ||
		m.ProviderName != "House GW" || m.ID != "gpt-5" {
		t.Fatalf("model = %+v, want it stamped with the provider identity", m)
	}
}

// CollectionFromSpecs is the shared construction path for the workspace API, so
// a bad spec must fail the whole build rather than yield a half-registered
// collection that looks fine until the missing provider is routed to.
func TestCollectionFromSpecs_RegistersAllOrFails(t *testing.T) {
	col, err := CollectionFromSpecs([]Spec{
		{ID: "a", Vendor: "openai"},
		{ID: "b", Vendor: "anthropic"},
	})
	if err != nil {
		t.Fatalf("CollectionFromSpecs: %v", err)
	}
	if got := len(col.Providers()); got != 2 {
		t.Fatalf("registered %d providers, want 2", got)
	}
	if _, ok := col.Get("b"); !ok {
		t.Fatal("provider b was not registered under its spec ID")
	}

	if _, err := CollectionFromSpecs([]Spec{
		{ID: "a", Vendor: "openai"},
		{ID: "bad", Vendor: "mystery-gateway"}, // no base URL
	}); err == nil {
		t.Fatal("a spec that cannot construct did not fail the collection")
	}
}
