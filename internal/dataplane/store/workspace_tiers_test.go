package storage

import "testing"

func TestApplyHostModelFallback_FillsEmptyPool(t *testing.T) {
	host := HostModelDefaults{
		Provider: "openai",
		BaseURL:  "https://ai.example.com/v1",
		APIKey:   "sk-host",
		Flash:    "flash",
		Lite:     "plus",
		Pro:      "pro",
	}
	cfg, keys := applyHostModelFallback(WorkspaceModelTiers{Provider: "openai", ModelFallback: true}, nil, host)
	if !cfg.HasKey || !cfg.HostedDefault {
		t.Fatalf("expected hosted has_key, got %+v", cfg)
	}
	if cfg.Model != "flash" || cfg.LiteModel != "plus" || cfg.ProModel != "pro" {
		t.Fatalf("unexpected models: %+v", cfg)
	}
	if cfg.BaseURL != host.BaseURL || keys["flash"] != "sk-host" {
		t.Fatalf("expected host key+url, keys=%v cfg=%+v", keys, cfg)
	}
}

func TestApplyHostModelFallback_BYOKWins(t *testing.T) {
	host := HostModelDefaults{APIKey: "sk-host", Flash: "flash", BaseURL: "https://host"}
	cfg, keys := applyHostModelFallback(
		WorkspaceModelTiers{Provider: "openai", Model: "gpt-4o", BaseURL: "https://byok", HasKey: true},
		map[string]string{"flash": "sk-tenant"},
		host,
	)
	if cfg.HostedDefault || keys["flash"] != "sk-tenant" || cfg.BaseURL != "https://byok" {
		t.Fatalf("BYOK should win, got cfg=%+v keys=%v", cfg, keys)
	}
}

// A workspace can set base_url without a key (owner/admin editable). The host
// key must never be shipped to that tenant-chosen endpoint.
func TestApplyHostModelFallback_NeverSendsHostKeyToTenantURL(t *testing.T) {
	host := HostModelDefaults{Provider: "openai", BaseURL: "https://ai.example.com/v1", APIKey: "sk-host", Flash: "flash", Lite: "plus", Pro: "pro"}
	cfg, keys := applyHostModelFallback(
		WorkspaceModelTiers{
			Provider: "openai", BaseURL: "https://exfil.tenant.test/v1",
			LiteBaseURL: "https://exfil.tenant.test/v1",
			ProBaseURL:  "https://exfil.tenant.test/v1",
		},
		nil,
		host,
	)
	for tier, url := range map[string]string{"flash": cfg.BaseURL, "lite": cfg.LiteBaseURL, "pro": cfg.ProBaseURL} {
		if keys[tier] == host.APIKey && url != host.BaseURL {
			t.Fatalf("%s tier got the host key pointed at %q", tier, url)
		}
	}
}

func TestApplyHostModelFallback_NoHostKey(t *testing.T) {
	cfg, keys := applyHostModelFallback(WorkspaceModelTiers{Provider: "openai"}, nil, HostModelDefaults{})
	if cfg.HasKey || cfg.HostedDefault || keys["flash"] != "" {
		t.Fatalf("empty host must not invent a key, got cfg=%+v keys=%v", cfg, keys)
	}
}
