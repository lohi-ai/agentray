package storage

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

func TestRenderPrompt(t *testing.T) {
	// A template substitutes {{body}}; multiple occurrences are all replaced.
	got := renderPrompt("event: {{body}} (repeat {{body}})", "hi")
	if got != "event: hi (repeat hi)" {
		t.Errorf("renderPrompt template = %q", got)
	}
	// An empty template falls back to a default that still carries the payload.
	got = renderPrompt("   ", "payload")
	if !strings.Contains(got, "payload") {
		t.Errorf("renderPrompt empty-template = %q, want it to contain the body", got)
	}
}

func TestVerifyWebhookHMAC(t *testing.T) {
	body := `{"event":"new_request"}`
	mac := hmac.New(sha256.New, []byte("topsecret"))
	mac.Write([]byte(body))
	sig := hex.EncodeToString(mac.Sum(nil))

	if !verifyWebhookHMAC("topsecret", body, sig) {
		t.Error("valid signature must verify")
	}
	if !verifyWebhookHMAC("topsecret", body, " "+sig+" ") {
		t.Error("a signature with surrounding whitespace must still verify")
	}
	if verifyWebhookHMAC("topsecret", body, "deadbeef") {
		t.Error("a wrong signature must not verify")
	}
	if verifyWebhookHMAC("topsecret", body, "") {
		t.Error("an empty signature must not verify when a secret is required")
	}
	// No secret configured => the unguessable token is the only credential; accept.
	if !verifyWebhookHMAC("", body, "") {
		t.Error("an unconfigured secret must accept (token-only auth)")
	}
}

func TestNewWebhookToken(t *testing.T) {
	a, err := newWebhookToken()
	if err != nil {
		t.Fatalf("newWebhookToken error: %v", err)
	}
	if len(a) != 64 { // 32 random bytes, hex-encoded
		t.Errorf("token length = %d, want 64", len(a))
	}
	b, _ := newWebhookToken()
	if a == b {
		t.Error("two tokens must not collide")
	}
}
