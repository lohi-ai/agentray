package agentruntime

import (
	"context"
	"errors"
	"strings"

	"github.com/lohi-ai/agentray/ai"
)

func (p BuildParams) nativeModelOptions(rung ModelTier, options PiModelOptions) PiModelOptions {
	if p.RefreshProviderKey != nil {
		options.RefreshKey = func(ctx context.Context, _ string) (string, error) {
			return p.RefreshProviderKey(ctx, rung.ProviderID, rung.Provider, rung.BaseURL)
		}
	}
	return options
}

func (p BuildParams) nativeLadderOptions(tier ModelTier, options PiModelOptions) (func(ModelTier) (PiModelOptions, error), error) {
	if options.RefreshKey != nil && p.RefreshProviderKey == nil {
		type identity struct{ row, vendor, endpoint string }
		seen := map[string]identity{}
		for _, resolved := range tier.resolvedRungs() {
			rung := resolved.tier
			if ai.NormalizeOAuthVendor(rung.Provider) == ai.VendorOpenAICodex && rung.TokenSource != nil {
				continue
			}
			provider, err := rung.RawProvider()
			if err != nil {
				return nil, err
			}
			bound := identity{rung.ProviderID, ai.NormalizeVendor(rung.Provider), strings.TrimRight(strings.TrimSpace(rung.BaseURL), "/")}
			if previous, found := seen[provider.Name()]; found && previous != bound {
				return nil, errors.New("native fallback requires provider-row credential refresh for distinct rows sharing a vendor")
			}
			seen[provider.Name()] = bound
		}
	}
	return func(rung ModelTier) (PiModelOptions, error) { return p.nativeModelOptions(rung, options), nil }, nil
}

// Unlike the legacy vendor lookup, native refresh keeps the credential tied to
// the admitted provider row/endpoint. A mid-run edit cannot redirect another
// row's key to the endpoint retained by the active request binding.
func (r *Runner) nativeKeyRefresher(projectID string) func(context.Context, string, string, string) (string, error) {
	if !r.KeyRefresh {
		return nil
	}
	return func(ctx context.Context, providerID, provider, baseURL string) (string, error) {
		if r.Store == nil {
			return "", errors.New("native credential refresh requires a store")
		}
		workspace, err := r.Store.WorkspaceIDForProject(ctx, projectID)
		if err != nil {
			return "", err
		}
		var candidates []TierConfig
		if providerID != "" {
			book, err := r.Store.LoadWorkspaceBook(ctx, workspace, true)
			if err != nil {
				return "", err
			}
			for _, row := range book.Providers {
				candidates = append(candidates, TierConfig{ProviderID: row.ID, Provider: row.Vendor, BaseURL: row.BaseURL, APIKey: row.APIKey})
			}
		} else {
			// Host defaults have no row. Resolve them through the same store path
			// which supplied the run, then demand an unambiguous matching route.
			cfg, keys, err := r.Store.WorkspaceTiersForRun(ctx, workspace)
			if err != nil {
				return "", err
			}
			tiers := TierSetFromWorkspace(cfg, keys, nil)
			for _, tier := range []Tier{TierFlash, TierLite, TierPro} {
				for _, rung := range (ModelTier{TierConfig: tiers.resolve(tier)}).resolvedRungs() {
					candidates = append(candidates, rung.tier.TierConfig)
				}
			}
		}
		return nativeRefreshedKey(providerID, provider, baseURL, candidates)
	}
}

func nativeRefreshedKey(providerID, provider, baseURL string, candidates []TierConfig) (string, error) {
	endpoint := func(value string) string { return strings.TrimRight(strings.TrimSpace(value), "/") }
	wantVendor, wantEndpoint := ai.NormalizeVendor(provider), endpoint(baseURL)
	key := ""
	for _, candidate := range candidates {
		if candidate.ProviderID != providerID {
			continue
		}
		if ai.NormalizeVendor(candidate.Provider) != wantVendor || endpoint(candidate.BaseURL) != wantEndpoint {
			if providerID != "" {
				return "", errors.New("native provider route changed; explicit migration required")
			}
			continue
		}
		if strings.TrimSpace(candidate.APIKey) == "" {
			return "", errors.New("native provider credential is empty")
		}
		if key != "" && key != candidate.APIKey {
			return "", errors.New("native provider credential is ambiguous")
		}
		key = candidate.APIKey
	}
	if key == "" {
		return "", errors.New("native provider credential binding no longer exists")
	}
	return key, nil
}
