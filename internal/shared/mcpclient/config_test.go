package mcpclient

import (
	"context"
	"strings"
	"testing"

	"github.com/lohi-ai/agentray/internal/shared/credential"
)

func TestParseConfigRejectsMalformed(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", "at least one server"},
		{"no servers", `{"servers":[]}`, "at least one server"},
		{"bad json", `{"servers":`, "invalid mcp config"},
		{"nameless", `{"servers":[{"url":"https://x.test/mcp"}]}`, "has no name"},
		{"urlless", `{"servers":[{"name":"a"}]}`, "has no url"},
		{"duplicate", `{"servers":[{"name":"a","url":"https://x.test/mcp"},{"name":"a","url":"https://y.test/mcp"}]}`, "configured twice"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseConfig(tc.in)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want error containing %q, got %v", tc.want, err)
			}
		})
	}
}

func TestParseConfigAcceptsValid(t *testing.T) {
	cfg, err := ParseConfig(`{"servers":[
		{"name":"billing","url":"https://mcp.example.test/mcp","headers":{"Authorization":"Bearer x"},"allow_tools":["lookup"],"required":true,"timeout_seconds":5}
	]}`)
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	if len(cfg.Servers) != 1 {
		t.Fatalf("want 1 server, got %d", len(cfg.Servers))
	}
	s := cfg.Servers[0]
	if s.Name != "billing" || !s.Required || s.TimeoutSeconds != 5 || len(s.AllowTools) != 1 {
		t.Fatalf("config not decoded: %+v", s)
	}
}

// The whole point of resolving here: the real token exists only in the header
// map handed to the transport, never in the stored config.
func TestConnectResolvesCredentialPlaceholders(t *testing.T) {
	vault, err := credential.FromMap(map[string]string{"BILLING_TOKEN": "s3cr3t"})
	if err != nil {
		t.Fatalf("FromMap: %v", err)
	}
	spec := ServerSpec{
		Name:    "billing",
		URL:     "https://mcp.example.test/mcp",
		Headers: map[string]string{"Authorization": "Bearer {{cred:BILLING_TOKEN}}"},
	}
	c, err := spec.Connect(context.Background(), vault)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if got := c.cfg.Headers["Authorization"]; got != "Bearer s3cr3t" {
		t.Fatalf("placeholder not resolved, header is %q", got)
	}
}

// Fail closed: a placeholder naming a secret the agent does not have must not
// silently ship a literal "{{cred:…}}" string as an Authorization header.
func TestConnectFailsOnUnknownSecret(t *testing.T) {
	vault, err := credential.FromMap(map[string]string{"OTHER": "x"})
	if err != nil {
		t.Fatalf("FromMap: %v", err)
	}
	spec := ServerSpec{
		Name:    "billing",
		URL:     "https://mcp.example.test/mcp",
		Headers: map[string]string{"Authorization": "Bearer {{cred:MISSING}}"},
	}
	if _, err := spec.Connect(context.Background(), vault); err == nil {
		t.Fatal("want an error for an unresolvable placeholder")
	}
}

func TestConnectRejectsPlaceholderWithoutVault(t *testing.T) {
	spec := ServerSpec{
		Name:    "billing",
		URL:     "https://mcp.example.test/mcp",
		Headers: map[string]string{"Authorization": "Bearer {{cred:TOKEN}}"},
	}
	_, err := spec.Connect(context.Background(), nil)
	if err == nil || !strings.Contains(err.Error(), "no secrets configured") {
		t.Fatalf("want a fail-closed error, got %v", err)
	}
}

func TestConnectWithoutSecretsPassesPlainHeaders(t *testing.T) {
	spec := ServerSpec{
		Name:    "public",
		URL:     "https://mcp.example.test/mcp",
		Headers: map[string]string{"X-Trace": "on"},
	}
	c, err := spec.Connect(context.Background(), nil)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if c.cfg.Headers["X-Trace"] != "on" {
		t.Fatalf("plain header lost: %+v", c.cfg.Headers)
	}
}
