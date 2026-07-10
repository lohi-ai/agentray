package agentruntime

import (
	"context"
	"strings"
	"testing"

	"github.com/lohi-ai/agentray/agentcore"
)

// stubResolver is a sentinel CredentialResolver used to assert the global path
// is returned unchanged when a project has no secrets of its own.
type stubResolver struct{ tag string }

func (s stubResolver) Resolve(_ context.Context, args string) (string, error) {
	return args + s.tag, nil
}

func TestRunCredentialsNoSecretsUsesGlobal(t *testing.T) {
	global := stubResolver{tag: "-global"}
	got, err := runCredentials(global, nil)
	if err != nil {
		t.Fatalf("runCredentials: %v", err)
	}
	if got == nil {
		t.Fatal("expected the global resolver, got nil")
	}
	out, _ := got.Resolve(context.Background(), "x")
	if out != "x-global" {
		t.Fatalf("expected global resolver to be used unchanged, got %q", out)
	}
}

func TestRunCredentialsNilGlobalNoSecrets(t *testing.T) {
	got, err := runCredentials(nil, map[string]string{})
	if err != nil {
		t.Fatalf("runCredentials: %v", err)
	}
	if got != nil {
		t.Fatal("expected nil resolver when no global and no secrets (feature off)")
	}
}

func TestRunCredentialsSecretsTakePrecedence(t *testing.T) {
	global := stubResolver{tag: "-global"}
	got, err := runCredentials(global, map[string]string{"API_KEY": "sk-real"})
	if err != nil {
		t.Fatalf("runCredentials: %v", err)
	}
	resolved, err := got.Resolve(context.Background(), `{"auth":"{{cred:API_KEY}}"}`)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if resolved != `{"auth":"sk-real"}` {
		t.Fatalf("per-agent vault not used; got %q", resolved)
	}
	// And it must be the vault, not the global stub (no -global tag appended).
	if strings.Contains(resolved, "-global") {
		t.Fatal("global resolver leaked through despite per-agent secrets")
	}
}

func TestRunCredentialsFailsClosedOnBadSecret(t *testing.T) {
	if _, err := runCredentials(nil, map[string]string{"bad name": "v"}); err == nil {
		t.Fatal("expected fail-closed error for an invalid stored secret name")
	}
}

var _ agentcore.CredentialResolver = stubResolver{}
