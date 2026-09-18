package app

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/labstack/echo/v4"
	"github.com/lohi-ai/agentray/agentcore"
	"github.com/lohi-ai/agentray/ai"
	"github.com/lohi-ai/agentray/internal/dataplane/store"
	"github.com/lohi-ai/agentray/internal/oauth"
	"github.com/lohi-ai/agentray/internal/runtime"
)

// collectionFromBook builds the shipped provider collection from a workspace
// book for live model discovery and provider-scoped calls.
// OAuth vendors get their account pool attached as the TokenSource — a
// list-models call then authenticates with a live account token, the same
// credential a run would draw.
func collectionFromBook(book *storage.WorkspaceProviderBook, mgr *oauth.Manager) (*ai.Collection, error) {
	specs := make([]ai.Spec, 0, len(book.Providers))
	for _, p := range book.Providers {
		specs = append(specs, oauthProviderSpec(mgr, p))
	}
	return ai.CollectionFromSpecs(specs)
}

// testBookConnections pings each configured tier — and each tier's fallback
// rung — through the owning provider and model wire (same credentials and
// persisted capabilities a run uses).
// Fallback results land under "<tier>_fallback" keys so the settings page can
// render the ladder it just verified.
func testBookConnections(ctx context.Context, book *storage.WorkspaceProviderBook, mgr *oauth.Manager) (bool, map[string]any) {
	cfg, keys := book.Resolve()
	results := make(map[string]any, 3)
	allOK := true
	test := func(name, providerID, provider, model, baseURL, key string, capabilities agentcore.ModelCapabilities) {
		provider = firstNonEmpty(provider, cfg.Provider)
		model = firstNonEmpty(model, cfg.Model)
		baseURL = firstNonEmpty(baseURL, cfg.BaseURL)
		key = firstNonEmpty(key, keys["flash"])
		var res map[string]any
		if providerID != "" {
			res = testOwnedProvider(ctx, book, providerID, model, capabilities, mgr)
		} else {
			res = testTierProviderCtx(ctx, provider, baseURL, model, key, tierTokenSource(mgr, provider, cfg.FlashProviderID), capabilities)
		}
		results[name] = res
		if ok, _ := res["ok"].(bool); !ok {
			allOK = false
		}
	}
	test("flash", cfg.FlashProviderID, cfg.Provider, cfg.Model, cfg.BaseURL, keys["flash"], cfg.Capabilities)
	if cfg.LiteProviderID != "" || cfg.LiteProvider != "" || cfg.LiteModel != "" || keys["lite"] != "" {
		test("lite", cfg.LiteProviderID, cfg.LiteProvider, cfg.LiteModel, cfg.LiteBaseURL, keys["lite"], cfg.LiteCapabilities)
	}
	if cfg.ProProviderID != "" || cfg.ProProvider != "" || cfg.ProModel != "" || keys["pro"] != "" {
		test("pro", cfg.ProProviderID, cfg.ProProvider, cfg.ProModel, cfg.ProBaseURL, keys["pro"], cfg.ProCapabilities)
	}

	// Fallback rungs. A cross-provider fallback tests through its own provider
	// row; a same-provider one tests the fallback model on the tier's provider.
	testFallback := func(name, fbProviderID, fbProvider, fbModel, fbBaseURL, fbKey, tierProviderID, tierProvider, tierBaseURL, tierKey string, capabilities agentcore.ModelCapabilities) {
		if fbModel == "" {
			return
		}
		var res map[string]any
		switch {
		case fbProviderID != "":
			res = testOwnedProvider(ctx, book, fbProviderID, fbModel, capabilities, mgr)
		case tierProviderID != "":
			res = testOwnedProvider(ctx, book, tierProviderID, fbModel, capabilities, mgr)
		default:
			provider := firstNonEmpty(fbProvider, tierProvider, cfg.Provider)
			baseURL := firstNonEmpty(fbBaseURL, tierBaseURL, cfg.BaseURL)
			key := firstNonEmpty(fbKey, tierKey, keys["flash"])
			res = testTierProviderCtx(ctx, provider, baseURL, fbModel, key, tierTokenSource(mgr, provider, cfg.FlashProviderID), capabilities)
		}
		results[name] = res
		if ok, _ := res["ok"].(bool); !ok {
			allOK = false
		}
	}
	testFallback("flash_fallback", cfg.FallbackProviderID, cfg.FallbackProvider, cfg.FallbackModel, cfg.FallbackBaseURL, keys["flash_fallback"],
		cfg.FlashProviderID, cfg.Provider, cfg.BaseURL, keys["flash"], cfg.FallbackCapabilities)
	testFallback("lite_fallback", cfg.LiteFallbackProviderID, cfg.LiteFallbackProvider, cfg.LiteFallbackModel, cfg.LiteFallbackBaseURL, keys["lite_fallback"],
		cfg.LiteProviderID, cfg.LiteProvider, cfg.LiteBaseURL, keys["lite"], cfg.LiteFallbackCapabilities)
	testFallback("pro_fallback", cfg.ProFallbackProviderID, cfg.ProFallbackProvider, cfg.ProFallbackModel, cfg.ProFallbackBaseURL, keys["pro_fallback"],
		cfg.ProProviderID, cfg.ProProvider, cfg.ProBaseURL, keys["pro"], cfg.ProFallbackCapabilities)
	return allOK, results
}

func testOwnedProvider(ctx context.Context, book *storage.WorkspaceProviderBook, providerID, model string, capabilities agentcore.ModelCapabilities, mgr *oauth.Manager) map[string]any {
	var rec *storage.WorkspaceProviderRecord
	for i := range book.Providers {
		if book.Providers[i].ID == providerID {
			rec = &book.Providers[i]
			break
		}
	}
	if rec == nil {
		return map[string]any{"ok": false, "error": "unknown provider " + providerID}
	}
	spec := oauthProviderSpec(mgr, *rec)
	p, err := agentruntime.NewTierProviderForModelWithSource(rec.Vendor, rec.BaseURL, rec.APIKey, spec.TokenSource, capabilities)
	if err != nil {
		return map[string]any{"ok": false, "error": err.Error()}
	}
	_, err = p.Chat(ctx, agentcore.ChatRequest{
		Model:     model,
		Messages:  []agentcore.Message{{Role: agentcore.RoleUser, Content: "ping"}},
		MaxTokens: 1,
	})
	if err != nil {
		return map[string]any{"ok": false, "error": err.Error()}
	}
	return map[string]any{"ok": true}
}

// tierTokenSource resolves the account pool a legacy (provider-string-only)
// OAuth tier draws from: the tier inherits flash's provider row, so flash's
// providerID keys the pool. Nil for key vendors and when no pool exists.
func tierTokenSource(mgr *oauth.Manager, provider, flashProviderID string) ai.TokenSource {
	if mgr == nil || flashProviderID == "" || !ai.IsOAuthVendor(provider) {
		return nil
	}
	return mgr.Pool(flashProviderID)
}

func testTierProviderCtx(ctx context.Context, provider, baseURL, model, key string, src ai.TokenSource, capabilities agentcore.ModelCapabilities) map[string]any {
	p, err := agentruntime.NewTierProviderForModelWithSource(provider, baseURL, key, src, capabilities)
	if err != nil {
		return map[string]any{"ok": false, "error": err.Error()}
	}
	_, callErr := p.Chat(ctx, agentcore.ChatRequest{
		Model:     model,
		Messages:  []agentcore.Message{{Role: agentcore.RoleUser, Content: "ping"}},
		MaxTokens: 1,
	})
	if callErr != nil {
		return map[string]any{"ok": false, "error": callErr.Error()}
	}
	return map[string]any{"ok": true}
}

func registerWorkspaceProviderRoutes(e *echo.Echo, store *storage.Store, mgr *oauth.Manager) {
	registerWorkspaceOAuthRoutes(e, store, mgr)
	e.GET("/api/workspace/providers", func(c echo.Context) error {
		ctx, project, err := authProject(c, store)
		if err != nil {
			return err
		}
		list, err := store.ListWorkspaceProviders(c.Request().Context(), ctx.User.ID, project.WorkspaceID)
		if err != nil {
			if errors.Is(err, storage.ErrAgentForbidden) {
				return echo.NewHTTPError(http.StatusForbidden, err.Error())
			}
			return echo.NewHTTPError(http.StatusInternalServerError, "could not load providers")
		}
		if list == nil {
			list = []storage.WorkspaceProvider{}
		}
		return c.JSON(http.StatusOK, map[string]any{"providers": list})
	})

	e.POST("/api/workspace/providers", func(c echo.Context) error {
		ctx, project, err := authProject(c, store)
		if err != nil {
			return err
		}
		var payload struct {
			Vendor  string `json:"vendor"`
			Name    string `json:"name"`
			BaseURL string `json:"base_url"`
			APIKey  string `json:"api_key"`
		}
		if err := c.Bind(&payload); err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, "invalid json")
		}
		p, err := store.CreateWorkspaceProvider(c.Request().Context(), ctx.User.ID, project.WorkspaceID, storage.WorkspaceProviderInput{
			Vendor: payload.Vendor, Name: payload.Name, BaseURL: payload.BaseURL, APIKey: payload.APIKey,
		})
		if err != nil {
			if strings.Contains(err.Error(), "requires a base URL") {
				return echo.NewHTTPError(http.StatusBadRequest, err.Error())
			}
			return echo.NewHTTPError(http.StatusForbidden, err.Error())
		}
		return c.JSON(http.StatusOK, map[string]any{"provider": p})
	})

	e.PUT("/api/workspace/providers/:id", func(c echo.Context) error {
		ctx, project, err := authProject(c, store)
		if err != nil {
			return err
		}
		var payload struct {
			Vendor  string `json:"vendor"`
			Name    string `json:"name"`
			BaseURL string `json:"base_url"`
			APIKey  string `json:"api_key"`
		}
		if err := c.Bind(&payload); err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, "invalid json")
		}
		p, err := store.UpdateWorkspaceProvider(c.Request().Context(), ctx.User.ID, project.WorkspaceID, c.Param("id"), storage.WorkspaceProviderInput{
			Vendor: payload.Vendor, Name: payload.Name, BaseURL: payload.BaseURL, APIKey: payload.APIKey,
		})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return echo.NewHTTPError(http.StatusNotFound, "provider not found")
			}
			if strings.Contains(err.Error(), "requires a base URL") {
				return echo.NewHTTPError(http.StatusBadRequest, err.Error())
			}
			return echo.NewHTTPError(http.StatusForbidden, err.Error())
		}
		return c.JSON(http.StatusOK, map[string]any{"provider": p})
	})

	e.DELETE("/api/workspace/providers/:id", func(c echo.Context) error {
		ctx, project, err := authProject(c, store)
		if err != nil {
			return err
		}
		if err := store.DeleteWorkspaceProvider(c.Request().Context(), ctx.User.ID, project.WorkspaceID, c.Param("id")); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return echo.NewHTTPError(http.StatusNotFound, "provider not found")
			}
			return echo.NewHTTPError(http.StatusForbidden, err.Error())
		}
		return c.JSON(http.StatusOK, map[string]any{"ok": true})
	})

	e.GET("/api/workspace/models/listed", func(c echo.Context) error {
		ctx, project, err := authProject(c, store)
		if err != nil {
			return err
		}
		if _, err := store.GetWorkspaceModelTiers(c.Request().Context(), ctx.User.ID, project.WorkspaceID); err != nil {
			if errors.Is(err, storage.ErrAgentForbidden) {
				return echo.NewHTTPError(http.StatusForbidden, err.Error())
			}
			return echo.NewHTTPError(http.StatusInternalServerError, "could not load model pool")
		}
		book, err := store.LoadWorkspaceBook(c.Request().Context(), project.WorkspaceID, true)
		if err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, err.Error())
		}
		col, err := collectionFromBook(book, mgr)
		if err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, err.Error())
		}
		listed, err := col.ListModels(c.Request().Context())
		if err != nil {
			return echo.NewHTTPError(http.StatusBadGateway, err.Error())
		}
		return c.JSON(http.StatusOK, listed)
	})
}
