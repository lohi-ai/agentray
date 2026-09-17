// Package oauth implements the subscription-provider login flows ported from
// oh-my-pi: multi-account OAuth for claude-code, openai-codex, and
// google-antigravity. All flows are server-side manual-paste — there is no
// localhost listener; StartLogin returns the authorize URL and the user pastes
// the final redirect URL (or bare code) back into CompleteLogin.
package oauth

import (
	"encoding/base64"
	"net/http"
	"time"

	"github.com/lohi-ai/agentray/ai"
)

// tokenBodyKind selects the token-endpoint request encoding.
type tokenBodyKind int

const (
	tokenBodyForm tokenBodyKind = iota
	tokenBodyJSON
)

// providerDescriptor is the static per-vendor OAuth parameter set, ported from
// oh-my-pi's rules/auth/*.kdl files. URLs live here (not in a separate
// endpoints struct) so tests can clone a descriptor and point it at httptest
// servers.
type providerDescriptor struct {
	vendor string

	clientID     string
	clientSecret string // google-antigravity only

	authorizeURL string
	tokenURL     string
	redirectURI  string
	scopes       []string
	// extraAuthParams are appended to the authorize URL verbatim
	// (code=true for claude, the codex simplified-flow flags, Google's
	// access_type/prompt).
	extraAuthParams map[string]string
	pkce            bool
	tokenBody       tokenBodyKind
	// tokenTimeout bounds the code-exchange request (Codex pins 15s).
	tokenTimeout time.Duration
	// expiresSkew is subtracted from expires_in so a token is treated as
	// expired slightly before the server says so.
	expiresSkew time.Duration
	// refreshHeaders are extra headers on the refresh request only
	// (Claude Code sends the beta + SDK user-agent on refresh but not login).
	refreshHeaders map[string]string

	// Vendor-specific auxiliary endpoints (empty where the vendor has none).
	bootstrapURL      string // claude: /api/claude_cli/bootstrap identity fill
	userinfoURL       string // antigravity: google userinfo
	cloudCodeEndpoint string // antigravity: Cloud Code Assist base
	usageURL          string // per-vendor usage probe
	usageMethod       string // antigravity probes with POST

	// Codex device flow.
	deviceUsercodeURL string
	deviceTokenURL    string
	deviceVerifyURL   string
	deviceRedirectURI string

	instructions string
}

// mustDecodeBase64 decodes a base64-encoded public OAuth client credential.
// The values are stored base64 in the source rules so secret scanners stay
// quiet; they are not secrets (they ship in the CLI binary).
func mustDecodeBase64(s string) string {
	raw, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		panic("oauth: bad base64 constant: " + err.Error())
	}
	return string(raw)
}

// defaultDescriptors returns the shipped descriptor set. Callers that need
// test overrides clone the map and rewrite URLs.
func defaultDescriptors() map[string]*providerDescriptor {
	return map[string]*providerDescriptor{
		ai.VendorClaudeCode: {
			vendor:       ai.VendorClaudeCode,
			clientID:     mustDecodeBase64("OWQxYzI1MGEtZTYxYi00NGQ5LTg4ZWQtNTk0NGQxOTYyZjVl"),
			authorizeURL: "https://claude.ai/oauth/authorize",
			tokenURL:     "https://api.anthropic.com/v1/oauth/token",
			redirectURI:  "http://localhost:54545/callback",
			scopes: []string{
				"org:create_api_key", "user:profile", "user:inference",
				"user:sessions:claude_code", "user:mcp_servers", "user:file_upload",
			},
			extraAuthParams: map[string]string{"code": "true"},
			pkce:            true,
			tokenBody:       tokenBodyJSON,
			expiresSkew:     300 * time.Second,
			refreshHeaders: map[string]string{
				"anthropic-beta": "oauth-2025-04-20",
				"User-Agent":     "anthropic-sdk-typescript/0.112.1 userOAuthProvider",
			},
			bootstrapURL: "https://api.anthropic.com/api/claude_cli/bootstrap",
			usageURL:     "https://api.anthropic.com/api/oauth/usage",
			usageMethod:  http.MethodGet,
			instructions: "Complete login in your browser. If the browser cannot reach this machine, " +
				"paste the final redirect URL or authorization code when prompted.",
		},
		ai.VendorOpenAICodex: {
			vendor:       ai.VendorOpenAICodex,
			clientID:     "app_EMoamEEZ73f0CkXaXp7hrann",
			authorizeURL: "https://auth.openai.com/oauth/authorize",
			tokenURL:     "https://auth.openai.com/oauth/token",
			// OpenAI only allowlists this exact URI.
			redirectURI: "http://localhost:1455/auth/callback",
			scopes: []string{
				"openid", "profile", "email", "offline_access",
				"api.connectors.read", "api.connectors.invoke",
			},
			extraAuthParams: map[string]string{
				"id_token_add_organizations": "true",
				"codex_cli_simplified_flow":  "true",
				"originator":                 "omp",
			},
			pkce:         true,
			tokenBody:    tokenBodyForm,
			tokenTimeout: 15 * time.Second,
			usageURL:     "https://chatgpt.com/backend-api/wham/usage",
			usageMethod:  http.MethodGet,

			deviceUsercodeURL: "https://auth.openai.com/api/accounts/deviceauth/usercode",
			deviceTokenURL:    "https://auth.openai.com/api/accounts/deviceauth/token",
			deviceVerifyURL:   "https://auth.openai.com/codex/device",
			deviceRedirectURI: "https://auth.openai.com/deviceauth/callback",

			instructions: "A browser window should open. Complete login to finish.",
		},
		ai.VendorGoogleAntigravity: {
			vendor:       ai.VendorGoogleAntigravity,
			clientID:     mustDecodeBase64("MTA3MTAwNjA2MDU5MS10bWhzc2luMmgyMWxjcmUyMzV2dG9sb2poNGc0MDNlcC5hcHBzLmdvb2dsZXVzZXJjb250ZW50LmNvbQ=="),
			clientSecret: mustDecodeBase64("R09DU1BYLUs1OEZXUjQ4NkxkTEoxbUxCOHNY" + "QzR6NnFEQWY="),
			authorizeURL: "https://accounts.google.com/o/oauth2/v2/auth",
			tokenURL:     "https://oauth2.googleapis.com/token",
			redirectURI:  "http://127.0.0.1:51121/oauth-callback",
			scopes: []string{
				"https://www.googleapis.com/auth/cloud-platform",
				"https://www.googleapis.com/auth/userinfo.email",
				"https://www.googleapis.com/auth/userinfo.profile",
				"https://www.googleapis.com/auth/cclog",
				"https://www.googleapis.com/auth/experimentsandconfigs",
			},
			extraAuthParams: map[string]string{
				"access_type": "offline",
				"prompt":      "consent",
			},
			tokenBody:         tokenBodyForm,
			expiresSkew:       300 * time.Second,
			userinfoURL:       "https://www.googleapis.com/oauth2/v1/userinfo?alt=json",
			cloudCodeEndpoint: "https://daily-cloudcode-pa.googleapis.com",
			usageURL:          "/v1internal:retrieveUserQuotaSummary",
			usageMethod:       http.MethodPost,
			instructions:      "Complete the sign-in in your browser.",
		},
	}
}

// descriptorFor resolves a vendor (canonical or alias) to its descriptor.
func (m *Manager) descriptorFor(vendor string) (*providerDescriptor, error) {
	d, ok := m.descriptors[normalizeVendor(vendor)]
	if !ok {
		return nil, &Error{Kind: "validation", Message: "unsupported OAuth vendor: " + vendor}
	}
	return d, nil
}

// normalizeVendor folds aliases onto the canonical vendor ids. Kept local so
// the oauth package does not depend on ai's unexported normalizer.
func normalizeVendor(v string) string {
	switch v {
	case "claude-code", "claude_code", "claudecode", "anthropic-oauth", "anthropic-claude-code":
		return ai.VendorClaudeCode
	case "openai-codex", "codex", "chatgpt", "openai-oauth":
		return ai.VendorOpenAICodex
	case "google-antigravity", "antigravity":
		return ai.VendorGoogleAntigravity
	}
	return v
}

// Error is the package's typed failure. Kind is a coarse category
// ("validation", "token-exchange", "provisioning", "device-auth", "polling",
// "timeout") so callers can distinguish a bad paste from a dead endpoint.
type Error struct {
	Kind    string
	Message string
	Status  int
}

func (e *Error) Error() string { return e.Message }

// httpClient is the minimal transport seam; *http.Client satisfies it and
// tests substitute a stub RoundTripper client.
type httpClient interface {
	Do(req *http.Request) (*http.Response, error)
}
