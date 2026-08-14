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
	"github.com/lohi-ai/agentray/internal/runtime"
)

// collectionFromBook builds the shipped provider collection from a workspace
// book so list-models and connection tests share construction with a run.
func collectionFromBook(book *storage.WorkspaceProviderBook) (*ai.Collection, error) {
	specs := make([]ai.Spec, 0, len(book.Providers))
	for _, p := range book.Providers {
		specs = append(specs, ai.Spec{
			ID: p.ID, Vendor: p.Vendor, Name: p.Name, APIKey: p.APIKey, BaseURL: p.BaseURL,
		})
	}
	return ai.CollectionFromSpecs(specs)
}

// testBookConnections pings each configured tier through the owning provider
// (same credentials a run would use).
func testBookConnections(ctx context.Context, book *storage.WorkspaceProviderBook) (bool, map[string]any) {
	cfg, keys := book.Resolve()
	results := make(map[string]any, 3)
	allOK := true
	test := func(name, providerID, provider, model, baseURL, key string) {
		provider = firstNonEmpty(provider, cfg.Provider)
		model = firstNonEmpty(model, cfg.Model)
		baseURL = firstNonEmpty(baseURL, cfg.BaseURL)
		key = firstNonEmpty(key, keys["flash"])
		var res map[string]any
		if providerID != "" {
			res = testOwnedProvider(ctx, book, providerID, model)
		} else {
			res = testTierProviderCtx(ctx, provider, baseURL, model, key)
		}
		results[name] = res
		if ok, _ := res["ok"].(bool); !ok {
			allOK = false
		}
	}
	test("flash", cfg.FlashProviderID, cfg.Provider, cfg.Model, cfg.BaseURL, keys["flash"])
	if cfg.LiteProviderID != "" || cfg.LiteProvider != "" || cfg.LiteModel != "" || keys["lite"] != "" {
		test("lite", cfg.LiteProviderID, cfg.LiteProvider, cfg.LiteModel, cfg.LiteBaseURL, keys["lite"])
	}
	if cfg.ProProviderID != "" || cfg.ProProvider != "" || cfg.ProModel != "" || keys["pro"] != "" {
		test("pro", cfg.ProProviderID, cfg.ProProvider, cfg.ProModel, cfg.ProBaseURL, keys["pro"])
	}
	return allOK, results
}

func testOwnedProvider(ctx context.Context, book *storage.WorkspaceProviderBook, providerID, model string) map[string]any {
	col, err := collectionFromBook(book)
	if err != nil {
		return map[string]any{"ok": false, "error": err.Error()}
	}
	_, err = col.ChatOn(ctx, providerID, agentcore.ChatRequest{
		Model:     model,
		Messages:  []agentcore.Message{{Role: agentcore.RoleUser, Content: "ping"}},
		MaxTokens: 1,
	})
	if err != nil {
		return map[string]any{"ok": false, "error": err.Error()}
	}
	return map[string]any{"ok": true}
}

func testTierProviderCtx(ctx context.Context, provider, baseURL, model, key string) map[string]any {
	p, err := agentruntime.NewTierProvider(provider, baseURL, key)
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

func registerWorkspaceProviderRoutes(e *echo.Echo, store *storage.Store) {
	e.GET("/api/workspace/providers", func(c echo.Context) error {
		ctx, project, err := authProject(c, store)
		if err != nil {
			return err
		}
		list, err := store.ListWorkspaceProviders(c.Request().Context(), ctx.User.ID, project.WorkspaceID)
		if err != nil {
			return echo.NewHTTPError(http.StatusForbidden, err.Error())
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
			return echo.NewHTTPError(http.StatusForbidden, err.Error())
		}
		book, err := store.LoadWorkspaceBook(c.Request().Context(), project.WorkspaceID, true)
		if err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, err.Error())
		}
		col, err := collectionFromBook(book)
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
