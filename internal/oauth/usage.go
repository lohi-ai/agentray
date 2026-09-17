package oauth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/lohi-ai/agentray/ai"
	storage "github.com/lohi-ai/agentray/internal/dataplane/store"
)

// claudeUsageBeta is the anthropic-beta list the CLI sends on usage reads
// (CLAUDE_HEADERS in usage/claude.ts).
const claudeUsageBeta = "claude-code-20250219,oauth-2025-04-20,interleaved-thinking-2025-05-14," +
	"redact-thinking-2026-02-12,context-management-2025-06-27,prompt-caching-scope-2026-01-05," +
	"mid-conversation-system-2026-04-07,advanced-tool-use-2025-11-20,effort-2025-11-24," +
	"extended-cache-ttl-2025-04-11"

// ProbeAccountUsage fetches the account's subscription usage from the vendor
// and stores the parsed summary on the account row. The stored map is the
// vendor's own usage document (five_hour/seven_day buckets for claude,
// plan_type + rate_limit windows for codex, quotaInfos for antigravity).
func (m *Manager) ProbeAccountUsage(ctx context.Context, userID, workspaceID, providerID, accountID string) (map[string]any, error) {
	// Membership gate: providerVendor lists the workspace's providers, which
	// rejects non-members before any vendor traffic.
	if _, err := m.providerVendor(ctx, userID, workspaceID, providerID); err != nil {
		return nil, err
	}
	rec, err := m.store.LoadProviderAccountRecord(ctx, accountID)
	if err != nil {
		return nil, err
	}
	if rec.ProviderID != providerID || rec.WorkspaceID != workspaceID {
		return nil, &Error{Kind: "validation", Message: "account does not belong to provider " + providerID}
	}
	if needsRefresh(rec, time.Now()) {
		res, err := m.refresh.refreshAccount(ctx, rec)
		if err != nil {
			return nil, err
		}
		rec.AccessToken = res.AccessToken
	}

	d, err := m.descriptorFor(rec.Vendor)
	if err != nil {
		return nil, err
	}
	var usage map[string]any
	switch d.vendor {
	case ai.VendorClaudeCode:
		usage, err = m.probeClaudeUsage(ctx, d, rec)
	case ai.VendorOpenAICodex:
		usage, err = m.probeCodexUsage(ctx, d, rec)
	case ai.VendorGoogleAntigravity:
		usage, err = m.probeAntigravityUsage(ctx, d, rec)
	}
	if err != nil {
		return nil, err
	}
	if err := m.store.UpdateProviderAccountUsage(ctx, accountID, usage); err != nil {
		return nil, err
	}
	return usage, nil
}

// getJSON issues an authenticated GET and decodes the body into a map.
func (m *Manager) getJSON(ctx context.Context, url string, headers map[string]string) (map[string]any, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := m.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		return nil, &Error{Kind: "usage", Status: resp.StatusCode,
			Message: fmt.Sprintf("usage fetch failed: %d %s", resp.StatusCode, truncate(string(raw), 200))}
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, &Error{Kind: "validation", Message: "usage endpoint returned invalid JSON"}
	}
	return payload, nil
}
func (m *Manager) probeClaudeUsage(ctx context.Context, d *providerDescriptor, rec storage.WorkspaceProviderAccountRecord) (map[string]any, error) {
	return m.getJSON(ctx, d.usageURL, map[string]string{
		"Accept":         "application/json, text/plain, */*",
		"Authorization":  "Bearer " + rec.AccessToken,
		"Content-Type":   "application/json",
		"User-Agent":     claudeBootstrapUA,
		"anthropic-beta": claudeUsageBeta,
	})
}

func (m *Manager) probeCodexUsage(ctx context.Context, d *providerDescriptor, rec storage.WorkspaceProviderAccountRecord) (map[string]any, error) {
	headers := map[string]string{
		"Authorization": "Bearer " + rec.AccessToken,
		"User-Agent":    "agentray/1",
	}
	if rec.AccountID != "" {
		headers["ChatGPT-Account-Id"] = rec.AccountID
	}
	return m.getJSON(ctx, d.usageURL, headers)
}

func (m *Manager) probeAntigravityUsage(ctx context.Context, d *providerDescriptor, rec storage.WorkspaceProviderAccountRecord) (map[string]any, error) {
	if rec.ProjectID == "" {
		return nil, &Error{Kind: "validation", Message: "antigravity account has no project id"}
	}
	return m.cloudCodeRequest(ctx, d.usageMethod,
		d.cloudCodeEndpoint+d.usageURL,
		rec.AccessToken, map[string]any{"project": rec.ProjectID})
}
