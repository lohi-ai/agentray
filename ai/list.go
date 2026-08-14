package ai

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	defaultGoogleBaseURL = "https://generativelanguage.googleapis.com"
)

// HTTPDoer is the injected HTTP seam so list-models and Chat share a client
// and tests can point at httptest servers.
type HTTPDoer interface {
	Do(*http.Request) (*http.Response, error)
}

func defaultHTTP() HTTPDoer {
	return &http.Client{Timeout: 30 * time.Second}
}

func doJSON(ctx context.Context, client HTTPDoer, req *http.Request) ([]byte, int, error) {
	if client == nil {
		client = defaultHTTP()
	}
	resp, err := client.Do(req.WithContext(ctx))
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return data, resp.StatusCode, nil
}

// listOpenAIModels calls GET {base}/models (OpenAI and OpenAI-compatible).
func listOpenAIModels(ctx context.Context, client HTTPDoer, baseURL, apiKey string) ([]string, error) {
	base := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if base == "" {
		base = defaultOpenAIBaseURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/models", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Accept", "application/json")
	data, status, err := doJSON(ctx, client, req)
	if err != nil {
		return nil, err
	}
	if status >= 400 {
		return nil, fmt.Errorf("list models: status %d: %s", status, strings.TrimSpace(string(data)))
	}
	var decoded struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &decoded); err != nil {
		return nil, fmt.Errorf("list models: decode: %w", err)
	}
	out := make([]string, 0, len(decoded.Data))
	for _, m := range decoded.Data {
		if id := strings.TrimSpace(m.ID); id != "" {
			out = append(out, id)
		}
	}
	return out, nil
}

// listAnthropicModels calls GET {base}/v1/models.
func listAnthropicModels(ctx context.Context, client HTTPDoer, baseURL, apiKey string) ([]string, error) {
	base := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if base == "" {
		base = defaultAnthropicBaseURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/v1/models", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("x-api-key", apiKey)
	req.Header.Set("anthropic-version", anthropicVersion)
	req.Header.Set("Accept", "application/json")
	data, status, err := doJSON(ctx, client, req)
	if err != nil {
		return nil, err
	}
	if status >= 400 {
		return nil, fmt.Errorf("list models: status %d: %s", status, strings.TrimSpace(string(data)))
	}
	var decoded struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &decoded); err != nil {
		return nil, fmt.Errorf("list models: decode: %w", err)
	}
	out := make([]string, 0, len(decoded.Data))
	for _, m := range decoded.Data {
		if id := strings.TrimSpace(m.ID); id != "" {
			out = append(out, id)
		}
	}
	return out, nil
}

// listGoogleModels calls the Gemini models.list API. A base URL that already
// speaks the OpenAI-compatible surface (path contains /openai) uses GET /models
// instead.
func listGoogleModels(ctx context.Context, client HTTPDoer, baseURL, apiKey string) ([]string, error) {
	base := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if strings.Contains(base, "/openai") {
		return listOpenAIModels(ctx, client, base, apiKey)
	}
	if base == "" {
		base = defaultGoogleBaseURL
	}
	endpoint := base
	switch {
	case strings.HasSuffix(endpoint, "/models"):
		// already a list-models URL
	case strings.Contains(endpoint, "/v1beta"):
		endpoint += "/models"
	default:
		endpoint += "/v1beta/models"
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, err
	}
	q := u.Query()
	if apiKey != "" {
		q.Set("key", apiKey)
	}
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	if apiKey != "" {
		req.Header.Set("x-goog-api-key", apiKey)
	}
	data, status, err := doJSON(ctx, client, req)
	if err != nil {
		return nil, err
	}
	if status >= 400 {
		return nil, fmt.Errorf("list models: status %d: %s", status, strings.TrimSpace(string(data)))
	}
	var decoded struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if err := json.Unmarshal(data, &decoded); err != nil {
		return nil, fmt.Errorf("list models: decode: %w", err)
	}
	out := make([]string, 0, len(decoded.Models))
	for _, m := range decoded.Models {
		id := strings.TrimSpace(m.Name)
		id = strings.TrimPrefix(id, "models/")
		if id != "" {
			out = append(out, id)
		}
	}
	return out, nil
}

func listModelsForVendor(ctx context.Context, client HTTPDoer, vendor, baseURL, apiKey string) ([]string, error) {
	switch NormalizeVendor(vendor) {
	case "anthropic":
		return listAnthropicModels(ctx, client, baseURL, apiKey)
	case "google":
		return listGoogleModels(ctx, client, baseURL, apiKey)
	default:
		// openai, openai-compat, and any other OpenAI-compatible vendor
		return listOpenAIModels(ctx, client, baseURL, apiKey)
	}
}
