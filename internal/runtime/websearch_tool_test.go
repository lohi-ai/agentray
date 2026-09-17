package agentruntime

import (
	"context"
	"strings"
	"testing"

	"github.com/lohi-ai/agentray/internal/shared/credential"
	"github.com/lohi-ai/agentray/sandbox"
)

// Absent config is a decline, not an error: the selection stays stored but the
// run gets no tool and a note explaining why.
func TestWebSearchAbsentConfigDeclines(t *testing.T) {
	for _, cfg := range []string{"", "{}", `{"provider":"  "}`} {
		tools, notes, err := BuildToolsWithContext(context.Background(), ToolBuildContext{}, sandbox.ToolWebSearch, cfg)
		if err != nil {
			t.Fatalf("config %q: want decline, got error %v", cfg, err)
		}
		if len(tools) != 0 {
			t.Fatalf("config %q: expected no tools, got %d", cfg, len(tools))
		}
		if len(notes) == 0 || !strings.Contains(notes[0], "no provider") {
			t.Fatalf("config %q: expected a decline note, got %v", cfg, notes)
		}
	}
}

func TestWebSearchBuildsDuckDuckGo(t *testing.T) {
	tools, notes, err := BuildToolsWithContext(context.Background(), ToolBuildContext{}, sandbox.ToolWebSearch, `{"provider":"duckduckgo"}`)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if len(notes) != 0 {
		t.Fatalf("unexpected notes: %v", notes)
	}
	if len(tools) != 1 || tools[0].Name() != sandbox.ToolWebSearch {
		t.Fatalf("want one web_search tool, got %v", tools)
	}
}

// A named-but-unknown provider is a malformed selection: fail closed, never
// silently drop a capability the operator believes is on.
func TestWebSearchUnknownProviderFailsClosed(t *testing.T) {
	if _, _, err := BuildToolsWithContext(context.Background(), ToolBuildContext{}, sandbox.ToolWebSearch, `{"provider":"altavista"}`); err == nil {
		t.Fatal("expected error for unknown provider")
	}
	if _, _, err := BuildToolsWithContext(context.Background(), ToolBuildContext{}, sandbox.ToolWebSearch, `{"provider":`); err == nil {
		t.Fatal("expected error for malformed config")
	}
}

func TestValidateWebSearchConfig(t *testing.T) {
	// Empty config is savable — the tool declines at run time.
	for _, cfg := range []string{"", "{}", `{"provider":"duckduckgo"}`, `{"provider":"duckduckgo","api_key":"{{cred:SEARCH_KEY}}"}`} {
		if err := ValidateToolConfig(ToolBuildContext{}, sandbox.ToolWebSearch, cfg); err != nil {
			t.Fatalf("config %q rejected at write time: %v", cfg, err)
		}
	}
	if err := ValidateToolConfig(ToolBuildContext{}, sandbox.ToolWebSearch, `{"provider":"altavista"}`); err == nil {
		t.Fatal("unknown provider accepted at write time")
	}
}

// An api_key placeholder resolves against the run vault at build time; with no
// vault the build fails closed rather than shipping a literal placeholder.
func TestWebSearchAPIKeyResolution(t *testing.T) {
	vault, err := credential.FromMap(map[string]string{"SEARCH_KEY": "s3cr3t"})
	if err != nil {
		t.Fatalf("FromMap: %v", err)
	}
	cfg := `{"provider":"duckduckgo","api_key":"{{cred:SEARCH_KEY}}"}`
	if _, _, err := BuildToolsWithContext(context.Background(), ToolBuildContext{Credentials: vault}, sandbox.ToolWebSearch, cfg); err != nil {
		t.Fatalf("resolvable key rejected: %v", err)
	}
	if _, _, err := BuildToolsWithContext(context.Background(), ToolBuildContext{}, sandbox.ToolWebSearch, cfg); err == nil {
		t.Fatal("placeholder without a vault must fail closed")
	}
}

func TestWebSearchInCatalog(t *testing.T) {
	if !IsRegisteredTool(sandbox.ToolWebSearch) {
		t.Fatal("web_search not registered")
	}
	found := false
	for _, e := range ToolCatalog() {
		if e.Name == sandbox.ToolWebSearch {
			found = true
			if !e.Configurable {
				t.Fatal("web_search should be configurable")
			}
		}
	}
	if !found {
		t.Fatal("web_search missing from catalog")
	}
}
