package credential

import (
	"context"
	"strings"
	"testing"
)

func TestResolveSubstitutesKnownCredential(t *testing.T) {
	v := NewVault()
	if err := v.Put("API_KEY", "sk-123"); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, err := v.Resolve(context.Background(), `{"auth":"{{cred:API_KEY}}"}`)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if want := `{"auth":"sk-123"}`; got != want {
		t.Fatalf("want %q, got %q", want, got)
	}
}

func TestResolveMultipleAndWhitespaceTolerant(t *testing.T) {
	v := NewVault()
	_ = v.Put("A", "1")
	_ = v.Put("B", "2")
	got, err := v.Resolve(context.Background(), `{{cred:A}} and {{ cred:B }} and {{cred:A}}`)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if want := "1 and 2 and 1"; got != want {
		t.Fatalf("want %q, got %q", want, got)
	}
}

func TestResolveUnknownFailsClosed(t *testing.T) {
	v := NewVault()
	_, err := v.Resolve(context.Background(), `{"auth":"{{cred:MISSING}}"}`)
	if err == nil {
		t.Fatal("expected an error for an unknown credential")
	}
	if !strings.Contains(err.Error(), "MISSING") {
		t.Fatalf("error should name the missing credential, got %v", err)
	}
}

// A partially-resolvable arg must fail atomically: the known credential's value
// must not leak into the returned (error) string.
func TestResolveUnknownIsAtomic(t *testing.T) {
	v := NewVault()
	_ = v.Put("KNOWN", "secret-value")
	out, err := v.Resolve(context.Background(), `{{cred:KNOWN}}-{{cred:UNKNOWN}}`)
	if err == nil {
		t.Fatal("expected error")
	}
	if out != "" {
		t.Fatalf("on error the output must be empty, got %q", out)
	}
	if strings.Contains(err.Error(), "secret-value") {
		t.Fatal("known secret value leaked into the error")
	}
}

func TestResolveNoPlaceholderUnchanged(t *testing.T) {
	v := NewVault()
	in := `{"sql":"select 1"}`
	got, err := v.Resolve(context.Background(), in)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got != in {
		t.Fatalf("want unchanged %q, got %q", in, got)
	}
}

func TestPutRejectsInvalidNameAndEmptyValue(t *testing.T) {
	v := NewVault()
	if err := v.Put("bad name!", "x"); err == nil {
		t.Fatal("expected invalid-name error")
	}
	if err := v.Put("OK", ""); err == nil {
		t.Fatal("expected empty-value error")
	}
	if v.Len() != 0 {
		t.Fatalf("nothing valid was stored, want Len 0 got %d", v.Len())
	}
}

func TestValidName(t *testing.T) {
	for _, ok := range []string{"API_KEY", "novel.api-key", "a", strings.Repeat("x", 128)} {
		if !ValidName(ok) {
			t.Errorf("ValidName(%q) = false, want true", ok)
		}
	}
	for _, bad := range []string{"", "has space", "semi;colon", "{{cred}}", strings.Repeat("x", 129)} {
		if ValidName(bad) {
			t.Errorf("ValidName(%q) = true, want false", bad)
		}
	}
}

func TestFromMapBuildsResolvableVault(t *testing.T) {
	v, err := FromMap(map[string]string{"API_KEY": "sk-1", "DSN": "postgres://x"})
	if err != nil {
		t.Fatalf("FromMap: %v", err)
	}
	if v.Len() != 2 {
		t.Fatalf("Len = %d, want 2", v.Len())
	}
	got, err := v.Resolve(context.Background(), `{"k":"{{cred:API_KEY}}","d":"{{cred:DSN}}"}`)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if want := `{"k":"sk-1","d":"postgres://x"}`; got != want {
		t.Fatalf("want %q, got %q", want, got)
	}
}

func TestFromMapFailsClosedOnBadEntry(t *testing.T) {
	if _, err := FromMap(map[string]string{"bad name": "v"}); err == nil {
		t.Error("expected error for invalid name")
	}
	if _, err := FromMap(map[string]string{"OK": ""}); err == nil {
		t.Error("expected error for empty value")
	}
}

func TestLoadFromEnviron(t *testing.T) {
	env := []string{
		"PATH=/usr/bin",
		"AGENTRAY_CRED_NOVEL_API_KEY=sk-novel",
		"AGENTRAY_CRED_DB_DSN=postgres://x",
		"AGENTRAY_SANDBOX_ENABLED=true", // wrong prefix, ignored
		"AGENTRAY_CRED_=ignored-empty-name",
		"malformed-no-eq",
	}
	v := LoadFromEnviron(env)
	if v.Len() != 2 {
		t.Fatalf("want 2 credentials loaded, got %d (%v)", v.Len(), v.Names())
	}
	got, err := v.Resolve(context.Background(), "{{cred:NOVEL_API_KEY}}")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got != "sk-novel" {
		t.Fatalf("want sk-novel, got %q", got)
	}
}
