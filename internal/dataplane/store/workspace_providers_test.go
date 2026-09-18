package storage

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/lohi-ai/agentray/agentcore"
	"github.com/lohi-ai/agentray/ai"
)

// Save two providers, list models from their stubs, save flash = A/model1 and
// lite = B/model2, read the same selection back, and resolve a run so lite
// uses B's key/base URL. Drives the shipped book + collection constructors.
func TestWorkspaceBookPersistListResolve(t *testing.T) {
	srvA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/models") && r.Method == http.MethodGet {
			_, _ = w.Write([]byte(`{"data":[{"id":"model-from-a"},{"id":"other-a"}]}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer srvA.Close()
	srvB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			_, _ = w.Write([]byte(`{"data":[{"id":"model-from-b"},{"id":"other-b"}]}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer srvB.Close()

	book := &WorkspaceProviderBook{WorkspaceID: "ws-1"}
	a, err := NewWorkspaceProviderRecord("", "ws-1", WorkspaceProviderInput{
		Vendor: "openai", Name: "Primary", APIKey: "key-a", BaseURL: srvA.URL,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewWorkspaceProviderRecord("", "ws-1", WorkspaceProviderInput{
		Vendor: "anthropic", Name: "Backup", APIKey: "key-b", BaseURL: srvB.URL,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	book.UpsertProvider(a)
	book.UpsertProvider(b)
	if len(book.Providers) != 2 {
		t.Fatalf("saved %d providers, want 2", len(book.Providers))
	}

	col, err := ai.CollectionFromSpecs([]ai.Spec{
		{ID: a.ID, Vendor: a.Vendor, Name: a.Name, APIKey: a.APIKey, BaseURL: a.BaseURL, HTTP: srvA.Client()},
		{ID: b.ID, Vendor: b.Vendor, Name: b.Name, APIKey: b.APIKey, BaseURL: b.BaseURL, HTTP: srvB.Client()},
	})
	if err != nil {
		t.Fatal(err)
	}
	listed, err := col.ListModels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(listed.Errors) != 0 {
		t.Fatalf("list errors: %+v", listed.Errors)
	}
	var modelA, modelB string
	for _, m := range listed.Models {
		switch m.ProviderID {
		case a.ID:
			if modelA == "" {
				modelA = m.ID
			}
		case b.ID:
			if modelB == "" {
				modelB = m.ID
			}
		default:
			t.Errorf("listed model from unknown provider %q", m.ProviderID)
		}
	}
	if modelA == "" || modelB == "" {
		t.Fatalf("missing listed models: %+v", listed.Models)
	}

	sel := WorkspaceTierSelection{
		FlashProviderID: a.ID, FlashModel: modelA,
		LiteProviderID: b.ID, LiteModel: modelB,
		ModelFallback: true,
	}
	if err := book.SetTiers(sel); err != nil {
		t.Fatal(err)
	}
	// Read back the same selection (persist).
	if book.Sel.FlashProviderID != a.ID || book.Sel.FlashModel != modelA {
		t.Fatalf("flash selection = %+v", book.Sel)
	}
	if book.Sel.LiteProviderID != b.ID || book.Sel.LiteModel != modelB {
		t.Fatalf("lite selection = %+v", book.Sel)
	}

	cfg, keys := book.Resolve()
	if cfg.FlashProviderID != a.ID || cfg.Model != modelA {
		t.Fatalf("resolved flash = %+v", cfg)
	}
	if cfg.LiteProviderID != b.ID || cfg.LiteModel != modelB {
		t.Fatalf("resolved lite = %+v", cfg)
	}
	if keys["lite"] != "key-b" {
		t.Fatalf("lite key = %q, want key-b", keys["lite"])
	}
	if cfg.LiteBaseURL != srvB.URL {
		t.Fatalf("lite base URL = %q, want %q", cfg.LiteBaseURL, srvB.URL)
	}
	if cfg.LiteProvider != "anthropic" {
		t.Fatalf("lite vendor = %q, want anthropic", cfg.LiteProvider)
	}
	if keys["flash"] != "key-a" || cfg.BaseURL != srvA.URL {
		t.Fatalf("flash creds leaked: keys=%v cfg=%+v", keys, cfg)
	}
}

// A pre-upgrade one-row-per-workspace tier record still resolves flash
// (keys and chosen models are not wiped).
func TestLegacyTierRowStillResolvesFlash(t *testing.T) {
	book, err := ApplyLegacyProviders("ws-legacy", LegacyWorkspaceTiers{
		Provider: "openai", Model: "kept-flash-model", BaseURL: "https://legacy.example/v1",
		FlashKey:      "sk-legacy-flash",
		ModelFallback: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	cfg, keys := book.Resolve()
	if cfg.Model != "kept-flash-model" {
		t.Fatalf("flash model = %q, want kept-flash-model", cfg.Model)
	}
	if keys["flash"] != "sk-legacy-flash" {
		t.Fatalf("flash key lost: %q", keys["flash"])
	}
	if cfg.BaseURL != "https://legacy.example/v1" || cfg.Provider != "openai" {
		t.Fatalf("flash vendor/url lost: %+v", cfg)
	}
	if cfg.FlashProviderID == "" || len(book.Providers) != 1 {
		t.Fatalf("legacy row should synthesize one flash provider, got %+v", book.Providers)
	}
}

// Distinct lite credentials become their own provider; a model-only override
// reuses flash.
func TestLegacyLiteOverrideBecomesOwnProvider(t *testing.T) {
	own, err := ApplyLegacyProviders("ws", LegacyWorkspaceTiers{
		Provider: "openai", Model: "flash-m", FlashKey: "fk",
		LiteProvider: "anthropic", LiteModel: "lite-m", LiteKey: "lk",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(own.Providers) != 2 {
		t.Fatalf("want 2 providers, got %d", len(own.Providers))
	}
	cfg, keys := own.Resolve()
	if keys["lite"] != "lk" || cfg.LiteProvider != "anthropic" || cfg.LiteModel != "lite-m" {
		t.Fatalf("lite own creds lost: cfg=%+v keys=%v", cfg, keys)
	}

	share, err := ApplyLegacyProviders("ws", LegacyWorkspaceTiers{
		Provider: "openai", Model: "flash-m", FlashKey: "fk",
		LiteModel: "lite-only-model",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(share.Providers) != 1 {
		t.Fatalf("model-only lite should reuse flash provider, got %d", len(share.Providers))
	}
	if share.Sel.LiteProviderID != share.Sel.FlashProviderID || share.Sel.LiteModel != "lite-only-model" {
		t.Fatalf("lite inherit pointer = %+v", share.Sel)
	}
}

func TestBlankLiteProInheritFlashOnResolve(t *testing.T) {
	book := &WorkspaceProviderBook{WorkspaceID: "ws"}
	flash, err := NewWorkspaceProviderRecord("p1", "ws", WorkspaceProviderInput{
		Vendor: "openai", APIKey: "fk", BaseURL: "https://flash.example",
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	book.UpsertProvider(flash)
	if err := book.SetTiers(WorkspaceTierSelection{FlashProviderID: "p1", FlashModel: "m-flash"}); err != nil {
		t.Fatal(err)
	}
	cfg, keys := book.Resolve()
	ts := map[string]string{"flash": keys["flash"], "lite": keys["lite"], "pro": keys["pro"]}
	if ts["lite"] != "" || ts["pro"] != "" {
		t.Fatalf("blank lite/pro must not invent keys: %v", ts)
	}
	// Runtime inheritance is still owned by TierSet.resolve — blank fields
	// mean inherit. Confirm the resolved cfg leaves them blank.
	if cfg.LiteModel != "" || cfg.ProModel != "" || cfg.LiteProvider != "" {
		t.Fatalf("blank lite/pro should stay blank for inherit: %+v", cfg)
	}
}

func TestSetTiersRejectsUnknownProvider(t *testing.T) {
	book := &WorkspaceProviderBook{}
	if err := book.SetTiers(WorkspaceTierSelection{FlashProviderID: "missing"}); err == nil {
		t.Fatal("expected error for unknown provider")
	}
}

// A fallback that names a provider row resolves to that row's vendor, base
// URL, and key — the cross-provider rung carries its own credentials, never
// the tier's. A model-only fallback keeps the same-provider meaning.
func TestResolveCrossProviderFallback(t *testing.T) {
	book := &WorkspaceProviderBook{WorkspaceID: "ws"}
	a := WorkspaceProviderRecord{ID: "pa", WorkspaceID: "ws", Vendor: "openai", BaseURL: "https://a.example/v1", APIKey: "key-a", HasKey: true}
	b := WorkspaceProviderRecord{ID: "pb", WorkspaceID: "ws", Vendor: "anthropic", BaseURL: "https://b.example", APIKey: "key-b", HasKey: true}
	book.UpsertProvider(a)
	book.UpsertProvider(b)
	if err := book.SetTiers(WorkspaceTierSelection{
		FlashProviderID: a.ID, FlashModel: "gpt-5",
		FlashFallbackProviderID: b.ID, FlashFallbackModel: "claude-sonnet-4-5",
		FlashFallbackCapabilities: agentcore.ModelCapabilities{Tools: agentcore.CapabilityUnsupported},
		LiteProviderID:            a.ID, LiteModel: "gpt-5-mini",
		LiteFallbackModel: "gpt-5-nano", // same-provider fallback: no provider id
		ModelFallback:     true,
	}); err != nil {
		t.Fatal(err)
	}
	cfg, keys := book.Resolve()
	if cfg.FallbackProvider != "anthropic" || cfg.FallbackBaseURL != "https://b.example" || !cfg.FallbackHasKey {
		t.Fatalf("cross-provider fallback did not resolve to row b: %+v", cfg)
	}
	if cfg.FallbackCapabilities.Tools != agentcore.CapabilityUnsupported {
		t.Fatalf("fallback capability snapshot was lost: %+v", cfg.FallbackCapabilities)
	}
	if keys["flash_fallback"] != "key-b" {
		t.Fatalf("fallback key slot = %q, want key-b", keys["flash_fallback"])
	}
	if cfg.LiteFallbackProvider != "" || cfg.LiteFallbackProviderID != "" {
		t.Fatalf("same-provider fallback must not resolve a provider: %+v", cfg)
	}
	if _, ok := keys["lite_fallback"]; ok {
		t.Fatal("same-provider fallback must not get its own key slot")
	}
}

// A fallback provider id without its model is half a rung — rejected, and an
// unknown fallback provider id is rejected like an unknown primary.
func TestSetTiersRejectsBadFallback(t *testing.T) {
	book := &WorkspaceProviderBook{WorkspaceID: "ws"}
	book.UpsertProvider(WorkspaceProviderRecord{ID: "pa", WorkspaceID: "ws", Vendor: "openai", APIKey: "k", HasKey: true})
	if err := book.SetTiers(WorkspaceTierSelection{FlashFallbackProviderID: "missing", FlashFallbackModel: "m"}); err == nil {
		t.Fatal("expected error for unknown fallback provider")
	}
	if err := book.SetTiers(WorkspaceTierSelection{FlashFallbackProviderID: "pa"}); err == nil {
		t.Fatal("expected error for fallback provider without a model")
	}
	// Deleting the provider clears the fallback pointer too.
	if err := book.SetTiers(WorkspaceTierSelection{FlashProviderID: "pa", FlashModel: "m", FlashFallbackProviderID: "pa", FlashFallbackModel: "m2"}); err != nil {
		t.Fatal(err)
	}
	book.DeleteProvider("pa")
	if book.Sel.FlashFallbackProviderID != "" {
		t.Fatal("DeleteProvider left a dangling fallback provider pointer")
	}
}

func TestCompatVendorRequiresBaseURL(t *testing.T) {
	if _, err := NewWorkspaceProviderRecord("", "ws", WorkspaceProviderInput{Vendor: "groq", APIKey: "k"}, nil); err == nil {
		t.Fatal("expected error for compat vendor without base URL")
	}
}

func TestOpenAIResponsesProviderUsesOfficialEndpointWithoutBaseURL(t *testing.T) {
	rec, err := NewWorkspaceProviderRecord("", "ws", WorkspaceProviderInput{
		Vendor: ai.VendorOpenAIResponses, APIKey: "sk-test",
	}, nil)
	if err != nil {
		t.Fatalf("NewWorkspaceProviderRecord: %v", err)
	}
	if rec.Vendor != ai.VendorOpenAIResponses || rec.BaseURL != "" || !rec.HasKey {
		t.Fatalf("record = %+v", rec)
	}
}

// Integration: drive the shipped Store persist + resolve path against Postgres
// when one is reachable (same skip convention as conversation tests).
func openProviderTestStore(t *testing.T) *Store {
	t.Helper()
	s := openConvTestStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// migratePostgres returns before migrateAgent (the analytics engine lives in a
	// different function). The conversation helper only runs the PG core, so
	// apply the agent schema here — including workspace_providers.
	if err := s.migrateAgent(ctx); err != nil {
		t.Fatalf("migrateAgent: %v", err)
	}
	return s
}

func TestStorePersistProvidersAndResolve(t *testing.T) {
	s := openProviderTestStore(t)
	t.Setenv("AGENT_KEY_ENC_SECRET", "workspace-provider-test-secret")
	ctx := context.Background()
	userID, projectID := seedConvProject(t, s)
	var wsID string
	if err := s.pg.QueryRow(ctx, `SELECT workspace_id::text FROM projects WHERE id = $1`, projectID).Scan(&wsID); err != nil {
		t.Fatal(err)
	}

	srvA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"store-a-1"}]}`))
	}))
	defer srvA.Close()
	srvB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"store-b-1"}]}`))
	}))
	defer srvB.Close()

	a, err := s.CreateWorkspaceProvider(ctx, userID, wsID, WorkspaceProviderInput{
		Vendor: "openai", Name: "A", APIKey: "key-store-a", BaseURL: srvA.URL,
	})
	if err != nil {
		t.Fatalf("create A: %v", err)
	}
	b, err := s.CreateWorkspaceProvider(ctx, userID, wsID, WorkspaceProviderInput{
		Vendor: "anthropic", Name: "B", APIKey: "key-store-b", BaseURL: srvB.URL,
	})
	if err != nil {
		t.Fatalf("create B: %v", err)
	}

	book, err := s.LoadWorkspaceBook(ctx, wsID, true)
	if err != nil {
		t.Fatal(err)
	}
	col, err := ai.CollectionFromSpecs([]ai.Spec{
		{ID: a.ID, Vendor: "openai", Name: "A", APIKey: book.byID()[a.ID].APIKey, BaseURL: srvA.URL, HTTP: srvA.Client()},
		{ID: b.ID, Vendor: "anthropic", Name: "B", APIKey: book.byID()[b.ID].APIKey, BaseURL: srvB.URL, HTTP: srvB.Client()},
	})
	if err != nil {
		t.Fatal(err)
	}
	listed, err := col.ListModels(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var modelA, modelB string
	for _, m := range listed.Models {
		if m.ProviderID == a.ID {
			modelA = m.ID
		}
		if m.ProviderID == b.ID {
			modelB = m.ID
		}
	}
	if modelA == "" || modelB == "" {
		t.Fatalf("listed models missing: %+v", listed)
	}

	got, err := s.SaveWorkspaceTierSelection(ctx, userID, wsID, WorkspaceTierSelection{
		FlashProviderID: a.ID, FlashModel: modelA,
		LiteProviderID: b.ID, LiteModel: modelB,
		FlashCapabilities: agentcore.ModelCapabilities{Tools: agentcore.CapabilityUnsupported},
		LiteCapabilities:  agentcore.ModelCapabilities{Tools: agentcore.CapabilitySupported},
		ModelFallback:     true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.FlashProviderID != a.ID || got.Model != modelA || got.LiteProviderID != b.ID || got.LiteModel != modelB {
		t.Fatalf("read-back selection = %+v", got)
	}
	if got.Capabilities.Tools != agentcore.CapabilityUnsupported || got.LiteCapabilities.Tools != agentcore.CapabilitySupported {
		t.Fatalf("read-back capabilities = flash %+v lite %+v", got.Capabilities, got.LiteCapabilities)
	}

	cfg, keys, err := s.WorkspaceTiersForRun(ctx, wsID)
	if err != nil {
		t.Fatal(err)
	}
	if keys["lite"] != "key-store-b" || cfg.LiteBaseURL != srvB.URL {
		t.Fatalf("run resolve lite used wrong creds: keys=%v cfg=%+v", keys, cfg)
	}
	if keys["flash"] != "key-store-a" {
		t.Fatalf("run resolve flash key = %q", keys["flash"])
	}
	if cfg.Capabilities.Tools != agentcore.CapabilityUnsupported || cfg.LiteCapabilities.Tools != agentcore.CapabilitySupported {
		t.Fatalf("run resolve lost capability snapshots: flash %+v lite %+v", cfg.Capabilities, cfg.LiteCapabilities)
	}
}

func TestSetTiersRejectsInvalidCapabilityState(t *testing.T) {
	book := &WorkspaceProviderBook{}
	err := book.SetTiers(WorkspaceTierSelection{
		FlashCapabilities: agentcore.ModelCapabilities{Tools: agentcore.CapabilitySupport("maybe")},
	})
	if err == nil || !strings.Contains(err.Error(), "invalid model capabilities") {
		t.Fatalf("invalid capability error = %v", err)
	}
}

// Load a pre-upgrade one-row-per-workspace tier record through the shipped
// Store dual-read and still resolve flash.
func TestStoreLegacyRowResolvesFlash(t *testing.T) {
	s := openProviderTestStore(t)
	t.Setenv("AGENT_KEY_ENC_SECRET", "workspace-provider-test-secret")
	ctx := context.Background()
	userID, projectID := seedConvProject(t, s)
	var wsID string
	if err := s.pg.QueryRow(ctx, `SELECT workspace_id::text FROM projects WHERE id = $1`, projectID).Scan(&wsID); err != nil {
		t.Fatal(err)
	}
	cipher, err := encryptAgentKey("sk-pre-upgrade")
	if err != nil {
		t.Fatal(err)
	}
	// Insert only the legacy columns — then delete any auto-backfill providers
	// so the dual-read path is what WorkspaceTiersForRun walks.
	if _, err := s.pg.Exec(ctx, `
INSERT INTO workspace_model_tiers (workspace_id, provider, model, base_url, api_key_ciphertext, model_fallback)
VALUES ($1, 'openai', 'pre-upgrade-flash', 'https://legacy.example/v1', $2, true)
ON CONFLICT (workspace_id) DO UPDATE SET
	provider = EXCLUDED.provider, model = EXCLUDED.model, base_url = EXCLUDED.base_url,
	api_key_ciphertext = EXCLUDED.api_key_ciphertext`,
		wsID, cipher); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pg.Exec(ctx, `DELETE FROM workspace_providers WHERE workspace_id = $1`, wsID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pg.Exec(ctx, `
UPDATE workspace_model_tiers SET flash_provider_id = NULL, lite_provider_id = NULL, pro_provider_id = NULL
WHERE workspace_id = $1`, wsID); err != nil {
		t.Fatal(err)
	}

	cfg, keys, err := s.WorkspaceTiersForRun(ctx, wsID)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Model != "pre-upgrade-flash" || keys["flash"] != "sk-pre-upgrade" {
		t.Fatalf("legacy dual-read lost flash: cfg=%+v keys=%v", cfg, keys)
	}
	_ = userID
}

// GET /api/workspace/models loads the book without decrypting. A BYOK
// provider (HasKey from ciphertext, empty APIKey) must not be treated as an
// empty pool: host lite/pro model IDs must not appear, or Settings Save
// would persist them as overrides and break blank-lite/pro inherit.
func TestGetWorkspaceModelTiers_BYOKSkipsHostLitePro(t *testing.T) {
	s := openProviderTestStore(t)
	t.Setenv("AGENT_KEY_ENC_SECRET", "workspace-provider-test-secret")
	hostLite, hostPro := "hosted-lite-id", "hosted-pro-id"
	s.hostModel = HostModelDefaults{
		Provider: "openai",
		BaseURL:  "https://host.example/v1",
		APIKey:   "sk-host-must-not-leak",
		Flash:    "hosted-flash-id",
		Lite:     hostLite,
		Pro:      hostPro,
	}
	ctx := context.Background()
	userID, projectID := seedConvProject(t, s)
	var wsID string
	if err := s.pg.QueryRow(ctx, `SELECT workspace_id::text FROM projects WHERE id = $1`, projectID).Scan(&wsID); err != nil {
		t.Fatal(err)
	}

	p, err := s.CreateWorkspaceProvider(ctx, userID, wsID, WorkspaceProviderInput{
		Vendor: "openai", Name: "BYOK", APIKey: "sk-tenant", BaseURL: "https://byok.example/v1",
	})
	if err != nil {
		t.Fatalf("create provider: %v", err)
	}
	if _, err := s.SaveWorkspaceTierSelection(ctx, userID, wsID, WorkspaceTierSelection{
		FlashProviderID: p.ID, FlashModel: "tenant-flash",
		ModelFallback: true,
	}); err != nil {
		t.Fatal(err)
	}

	cfg, err := s.GetWorkspaceModelTiers(ctx, userID, wsID)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.HasKey {
		t.Fatalf("BYOK GET must report has_key, got %+v", cfg)
	}
	if cfg.HostedDefault {
		t.Fatalf("BYOK workspace must not be marked hosted_default: %+v", cfg)
	}
	if cfg.LiteModel == hostLite || cfg.ProModel == hostPro || cfg.Model == "hosted-flash-id" {
		t.Fatalf("host model IDs leaked into GET config: %+v", cfg)
	}
	if cfg.LiteModel != "" || cfg.ProModel != "" || cfg.LiteProvider != "" || cfg.ProProvider != "" {
		t.Fatalf("blank lite/pro must stay blank for inherit, got %+v", cfg)
	}
	if cfg.Model != "tenant-flash" || cfg.FlashProviderID != p.ID {
		t.Fatalf("flash selection lost: %+v", cfg)
	}
}

// A BYOK provider with no tier selection yet still blocks host lite/pro
// injection — otherwise the first Save from Settings would persist them.
func TestGetWorkspaceModelTiers_BYOKNoSelectionSkipsHost(t *testing.T) {
	s := openProviderTestStore(t)
	t.Setenv("AGENT_KEY_ENC_SECRET", "workspace-provider-test-secret")
	s.hostModel = HostModelDefaults{
		Provider: "openai", APIKey: "sk-host-must-not-leak",
		Flash: "hosted-flash-id", Lite: "hosted-lite-id", Pro: "hosted-pro-id",
	}
	ctx := context.Background()
	userID, projectID := seedConvProject(t, s)
	var wsID string
	if err := s.pg.QueryRow(ctx, `SELECT workspace_id::text FROM projects WHERE id = $1`, projectID).Scan(&wsID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateWorkspaceProvider(ctx, userID, wsID, WorkspaceProviderInput{
		Vendor: "anthropic", Name: "BYOK", APIKey: "sk-tenant",
	}); err != nil {
		t.Fatal(err)
	}
	cfg, err := s.GetWorkspaceModelTiers(ctx, userID, wsID)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.LiteModel == s.hostModel.Lite || cfg.ProModel == s.hostModel.Pro || cfg.Model == s.hostModel.Flash {
		t.Fatalf("host model IDs leaked before any tier was chosen: %+v", cfg)
	}
	if cfg.HostedDefault {
		t.Fatal("BYOK workspace must not be hosted_default")
	}
}

// Resolve on a redacted book (HasKey, no APIKey) is the GET path. HasKey
// must survive so applyHostModelFallback does not treat the workspace as empty.
func TestResolveHonorsRedactedHasKey(t *testing.T) {
	book := &WorkspaceProviderBook{
		Providers: []WorkspaceProviderRecord{
			{ID: "p1", Vendor: "openai", BaseURL: "https://byok.example/v1", HasKey: true},
		},
		Sel: WorkspaceTierSelection{FlashProviderID: "p1", FlashModel: "tenant-m"},
	}
	cfg, keys := book.Resolve()
	if !cfg.HasKey {
		t.Fatalf("redacted BYOK must still report HasKey, cfg=%+v keys=%v", cfg, keys)
	}
	if keys["flash"] != "" {
		t.Fatalf("GET path must not expose the key, got %q", keys["flash"])
	}
}

func TestResolveTreatsExplicitLocalEngineAsReadyWithoutKey(t *testing.T) {
	book := &WorkspaceProviderBook{
		Providers: []WorkspaceProviderRecord{{
			ID: "local", Vendor: "ollama", Name: "Laptop Ollama",
			BaseURL: "http://127.0.0.1:11434/v1",
		}},
		Sel: WorkspaceTierSelection{FlashProviderID: "local", FlashModel: "qwen3-coder"},
	}
	cfg, keys := book.Resolve()
	if !cfg.HasKey {
		t.Fatalf("keyless local engine must be run-ready: %+v", cfg)
	}
	if keys["flash"] != "" {
		t.Fatalf("keyless local engine invented a credential: %q", keys["flash"])
	}
	pub := book.Public()
	if len(pub) != 1 || pub[0].AuthType != "optional" || pub[0].HasKey {
		t.Fatalf("public provider = %+v, want optional auth with no stored key", pub)
	}
	if !providerBookHasKey(book) {
		t.Fatal("host fallback would overwrite an explicitly configured local engine")
	}

	cloud := &WorkspaceProviderBook{
		Providers: []WorkspaceProviderRecord{{ID: "cloud", Vendor: "openai"}},
		Sel:       WorkspaceTierSelection{FlashProviderID: "cloud", FlashModel: "gpt"},
	}
	cloudCfg, _ := cloud.Resolve()
	if cloudCfg.HasKey || providerBookHasKey(cloud) {
		t.Fatal("keyless OpenAI must remain unready")
	}
}

func TestDeleteProviderClearsTierRefs(t *testing.T) {
	book := &WorkspaceProviderBook{}
	book.UpsertProvider(WorkspaceProviderRecord{ID: "p1", Vendor: "openai", APIKey: "k"})
	book.UpsertProvider(WorkspaceProviderRecord{ID: "p2", Vendor: "anthropic", APIKey: "k2"})
	_ = book.SetTiers(WorkspaceTierSelection{
		FlashProviderID: "p1", FlashModel: "m",
		FlashFallbackProviderID: "p2", FlashFallbackModel: "fb",
		LiteFallbackProviderID: "p2", LiteFallbackModel: "fb2",
	})
	book.DeleteProvider("p2")
	if book.Sel.FlashFallbackProviderID != "" || book.Sel.LiteFallbackProviderID != "" {
		t.Fatalf("delete left a dangling fallback pointer: %+v", book.Sel)
	}
	book.DeleteProvider("p1")
	if book.Sel.FlashProviderID != "" || len(book.Providers) != 0 {
		t.Fatalf("delete did not clear: book=%+v", book)
	}
}

// An OAuth provider with a live account pool resolves as configured: the tier
// key is the pool sentinel (never a real key — the wire client pulls a token
// from its TokenSource) and HasKey is true so the run gate treats it as ready.
func TestResolveOAuthProviderWithAccounts(t *testing.T) {
	book := &WorkspaceProviderBook{
		Providers: []WorkspaceProviderRecord{
			{ID: "p1", Vendor: ai.VendorClaudeCode, AuthType: "oauth", AccountCount: 2},
		},
		Sel: WorkspaceTierSelection{FlashProviderID: "p1", FlashModel: "claude-sonnet-4-5"},
	}
	cfg, keys := book.Resolve()
	if !cfg.HasKey {
		t.Fatalf("oauth provider with accounts must report HasKey, cfg=%+v", cfg)
	}
	if keys["flash"] != ai.OAuthPoolKey {
		t.Fatalf("expected pool sentinel key, got %q", keys["flash"])
	}
	pub := book.Public()
	if len(pub) != 1 || pub[0].AuthType != "oauth" || pub[0].AccountCount != 2 || !pub[0].HasKey {
		t.Fatalf("public view must carry auth_type/account_count: %+v", pub)
	}
}

// An OAuth provider with an empty pool resolves as unconfigured — no sentinel
// key, HasKey false — so the run path surfaces "sign in an account" rather
// than dispatching a request with no credential.
func TestResolveOAuthProviderNoAccounts(t *testing.T) {
	book := &WorkspaceProviderBook{
		Providers: []WorkspaceProviderRecord{
			{ID: "p1", Vendor: ai.VendorOpenAICodex, AuthType: "oauth"},
		},
		Sel: WorkspaceTierSelection{FlashProviderID: "p1", FlashModel: "gpt-5-codex"},
	}
	cfg, keys := book.Resolve()
	if cfg.HasKey || keys["flash"] != "" {
		t.Fatalf("empty pool must not resolve as configured: cfg=%+v keys=%v", cfg, keys)
	}
	pub := book.Public()
	if pub[0].HasKey {
		t.Fatalf("public view must not report has_key for an empty pool: %+v", pub[0])
	}
}

// OAuth vendors need no base URL and never store a key. A caller that passes
// one is told so — silently dropping it would leave a custom-endpoint user
// stranded on a provider that can never authenticate.
func TestOAuthProviderRecordSkipsKeyAndBaseURL(t *testing.T) {
	rec, err := NewWorkspaceProviderRecord("", "ws", WorkspaceProviderInput{
		Vendor: "claude-code",
	}, nil)
	if err != nil {
		t.Fatalf("oauth vendor must not require a base URL: %v", err)
	}
	if rec.AuthType != "oauth" || rec.APIKey != "" || rec.HasKey {
		t.Fatalf("oauth record must carry no key: %+v", rec)
	}
	if _, err := NewWorkspaceProviderRecord("", "ws", WorkspaceProviderInput{
		Vendor: "claude-code", APIKey: "sk-should-not-stick",
	}, nil); err == nil {
		t.Fatal("an API key on an OAuth vendor must be rejected, not dropped")
	}
}
