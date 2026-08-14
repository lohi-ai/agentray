package mcpclient

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Config is the per-agent stored configuration for the `mcp` catalog entry: the
// list of remote servers this agent may reach. It is the whole extension surface
// a tenant gets — data in a config column, never code in this process.
//
//	{
//	  "servers": [
//	    {
//	      "name": "billing",
//	      "url": "https://mcp.example.com/mcp",
//	      "headers": {"Authorization": "Bearer {{cred:BILLING_MCP_TOKEN}}"},
//	      "allow_tools": ["lookup_invoice"],
//	      "required": true
//	    }
//	  ]
//	}
type Config struct {
	Servers []ServerSpec `json:"servers"`
}

// ServerSpec is one configured server. Headers may carry {{cred:NAME}}
// placeholders; they are resolved from the agent's vault when the tools are
// built, so the literal secret is never stored here and never reaches the model.
type ServerSpec struct {
	Name       string            `json:"name"`
	URL        string            `json:"url"`
	Headers    map[string]string `json:"headers,omitempty"`
	AllowTools []string          `json:"allow_tools,omitempty"`
	AllowHTTP  bool              `json:"allow_http,omitempty"`
	// TimeoutSeconds bounds one call to this server; 0 uses the package default.
	TimeoutSeconds int `json:"timeout_seconds,omitempty"`
	// Required makes an unreachable server fail the run instead of being skipped.
	// Default (false) is deliberate: a third-party server being down should not
	// take an unrelated question with it, and the skip is reported, not silent.
	// Turn it on for a server whose absence would make the agent's answers wrong
	// rather than merely narrower.
	Required bool `json:"required,omitempty"`
}

// SecretResolver substitutes {{cred:NAME}} placeholders. It is the same contract
// as agentcore.CredentialResolver, restated here so this package stays free of a
// dependency direction it does not need.
type SecretResolver interface {
	Resolve(ctx context.Context, args string) (string, error)
}

// ParseConfig decodes and fully validates the stored config: names are present
// and unique, and every URL is one we would be willing to dial. It performs no
// I/O and resolves no secrets, so the control plane can accept a config carrying
// {{cred:NAME}} headers for an agent whose vault it is not holding, and a remote
// server being down never blocks saving a correct config. It rejects a malformed
// or empty configuration rather than leaving an agent with a capability its
// operator believes is switched on.
func ParseConfig(configJSON string) (Config, error) {
	var cfg Config
	s := strings.TrimSpace(configJSON)
	if s == "" {
		return cfg, fmt.Errorf("mcp requires at least one server in its config")
	}
	if err := json.Unmarshal([]byte(s), &cfg); err != nil {
		return cfg, fmt.Errorf("invalid mcp config: %w", err)
	}
	if len(cfg.Servers) == 0 {
		return cfg, fmt.Errorf("mcp requires at least one server in its config")
	}
	seen := make(map[string]bool, len(cfg.Servers))
	for i, s := range cfg.Servers {
		name := strings.TrimSpace(s.Name)
		if name == "" {
			return cfg, fmt.Errorf("mcp server #%d has no name", i+1)
		}
		if seen[name] {
			return cfg, fmt.Errorf("mcp server %q is configured twice", name)
		}
		seen[name] = true
		if strings.TrimSpace(s.URL) == "" {
			return cfg, fmt.Errorf("mcp server %q has no url", name)
		}
		// Reuse the constructor's URL rules (scheme, host, plain-http refusal)
		// rather than restating them: what validates here is exactly what dials
		// later. Headers are left out — they may still hold placeholders.
		if _, err := New(ServerConfig{Name: name, URL: s.URL, AllowHTTP: s.AllowHTTP}); err != nil {
			return cfg, err
		}
	}
	return cfg, nil
}

// Connect builds a validated client for one server, resolving any {{cred:NAME}}
// placeholder in its headers through the supplied resolver. Resolution happens
// HERE, on the host side at build time, and not in the tool loop: an MCP auth
// header is operator configuration, never a model-supplied argument, so it must
// never pass through anything the model can see or influence.
func (s ServerSpec) Connect(ctx context.Context, secrets SecretResolver, opts ...Option) (*Client, error) {
	headers, err := resolveHeaders(ctx, secrets, s.Headers)
	if err != nil {
		return nil, fmt.Errorf("mcp server %q: %w", s.Name, err)
	}
	return New(ServerConfig{
		Name:       strings.TrimSpace(s.Name),
		URL:        strings.TrimSpace(s.URL),
		Headers:    headers,
		Timeout:    time.Duration(s.TimeoutSeconds) * time.Second,
		AllowTools: s.AllowTools,
		AllowHTTP:  s.AllowHTTP,
	}, opts...)
}

// resolveHeaders substitutes placeholders in header values. It round-trips the
// map through the resolver's JSON contract, so a header value is resolved
// exactly the way a tool argument would be. A nil resolver leaves values as
// written — correct for a header carrying no placeholder, and a fail-closed
// error below for one that does.
func resolveHeaders(ctx context.Context, secrets SecretResolver, in map[string]string) (map[string]string, error) {
	if len(in) == 0 {
		return nil, nil
	}
	if secrets == nil {
		for k, v := range in {
			if strings.Contains(v, "{{cred:") {
				return nil, fmt.Errorf("header %q uses a {{cred:…}} placeholder but this agent has no secrets configured", k)
			}
		}
		out := make(map[string]string, len(in))
		for k, v := range in {
			out[k] = v
		}
		return out, nil
	}
	encoded, err := json.Marshal(in)
	if err != nil {
		return nil, err
	}
	resolved, err := secrets.Resolve(ctx, string(encoded))
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	if err := json.Unmarshal([]byte(resolved), &out); err != nil {
		return nil, fmt.Errorf("resolving headers: %w", err)
	}
	return out, nil
}
