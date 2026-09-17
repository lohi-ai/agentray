package oauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/lohi-ai/agentray/ai"
	storage "github.com/lohi-ai/agentray/internal/dataplane/store"
)

// pendingTTL bounds how long a started login stays completable.
const pendingTTL = 15 * time.Minute

// PendingLogin is one in-flight login attempt. Browser flows key the map by
// the OAuth state; the Codex device flow keys it by a random pending id and
// carries DeviceAuthID/UserCode instead of a verifier.
type PendingLogin struct {
	Vendor       string
	WorkspaceID  string
	ProviderID   string
	Verifier     string
	State        string
	DeviceAuthID string
	UserCode     string
	CreatedAt    time.Time
}

// LoginStart is StartLogin's result: the URL to open plus the state handle the
// caller echoes back to CompleteLogin.
type LoginStart struct {
	AuthURL      string `json:"auth_url"`
	State        string `json:"state"`
	Instructions string `json:"instructions"`
}

// TokenResult is the normalized outcome of a code exchange or refresh.
type TokenResult struct {
	AccessToken  string
	RefreshToken string
	ExpiresAt    time.Time
	IDToken      string // codex: profile claims may live here

	AccountID string
	Email     string
	OrgID     string
	OrgName   string
	Plan      string
	ProjectID string
}

// Manager owns pending logins and drives exchange/enrichment against the
// account store. The zero-value http client and descriptor set are production
// defaults; tests construct a Manager literally and point descriptors at
// httptest servers.
type Manager struct {
	store       *storage.Store
	client      httpClient
	descriptors map[string]*providerDescriptor

	mu      sync.Mutex
	pending map[string]PendingLogin

	refresh *refresher
}

// NewManager builds a Manager with production endpoints and the default
// HTTP client.
func NewManager(store *storage.Store) *Manager {
	m := &Manager{
		store:       store,
		client:      &http.Client{Timeout: 30 * time.Second},
		descriptors: defaultDescriptors(),
		pending:     map[string]PendingLogin{},
	}
	m.refresh = &refresher{
		descriptors:  m.descriptors,
		client:       m.client,
		updateTokens: store.UpdateProviderAccountTokens,
	}
	return m
}

// --- PKCE + state -----------------------------------------------------------

func randomURLSafe(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := io.ReadFull(rand.Reader, buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// generatePKCE returns (verifier, S256 challenge), matching oh-my-pi's
// 96-byte verifier.
func generatePKCE() (verifier, challenge string, err error) {
	verifier, err = randomURLSafe(96)
	if err != nil {
		return "", "", err
	}
	sum := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(sum[:]), nil
}

// --- pending map ------------------------------------------------------------

// sweep drops expired pendings; called under mu on every access.
func (m *Manager) sweepLocked(now time.Time) {
	for k, p := range m.pending {
		if now.Sub(p.CreatedAt) > pendingTTL {
			delete(m.pending, k)
		}
	}
}

func (m *Manager) putPending(key string, p PendingLogin) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sweepLocked(time.Now())
	m.pending[key] = p
}

func (m *Manager) getPending(key string) (PendingLogin, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sweepLocked(time.Now())
	p, ok := m.pending[key]
	return p, ok
}

func (m *Manager) deletePending(key string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.pending, key)
}

// --- browser login ----------------------------------------------------------

// providerVendor resolves the provider row's vendor for a workspace member.
// ListWorkspaceProviders enforces membership; a missing row is pgx.ErrNoRows
// so the HTTP layer maps it to 404.
func (m *Manager) providerVendor(ctx context.Context, userID, workspaceID, providerID string) (string, error) {
	providers, err := m.store.ListWorkspaceProviders(ctx, userID, workspaceID)
	if err != nil {
		return "", err
	}
	for _, p := range providers {
		if p.ID == providerID {
			return p.Vendor, nil
		}
	}
	return "", pgx.ErrNoRows
}

// StartLogin generates PKCE + state, records the pending attempt, and returns
// the authorize URL for the user to open. The vendor comes from the provider
func (m *Manager) StartLogin(ctx context.Context, userID, workspaceID, providerID string) (LoginStart, error) {
	vendor, err := m.providerVendor(ctx, userID, workspaceID, providerID)
	if err != nil {
		return LoginStart{}, err
	}
	if !ai.IsOAuthVendor(vendor) {
		return LoginStart{}, &Error{Kind: "validation",
			Message: "provider " + vendor + " is not an OAuth provider"}
	}
	return m.startLogin(vendor, workspaceID, providerID)
}

// startLogin builds the authorize URL for a resolved vendor. Split from
// StartLogin so tests exercise URL construction without a store.
func (m *Manager) startLogin(vendor, workspaceID, providerID string) (LoginStart, error) {
	d, err := m.descriptorFor(vendor)
	if err != nil {
		return LoginStart{}, err
	}
	state, err := randomURLSafe(32)
	if err != nil {
		return LoginStart{}, err
	}
	var verifier, challenge string
	if d.pkce {
		verifier, challenge, err = generatePKCE()
		if err != nil {
			return LoginStart{}, err
		}
	}

	q := url.Values{
		"response_type": {"code"},
		"client_id":     {d.clientID},
		"redirect_uri":  {d.redirectURI},
		"scope":         {strings.Join(d.scopes, " ")},
		"state":         {state},
	}
	if d.pkce {
		q.Set("code_challenge", challenge)
		q.Set("code_challenge_method", "S256")
	}
	for k, v := range d.extraAuthParams {
		q.Set(k, v)
	}

	m.putPending(state, PendingLogin{
		Vendor:      normalizeVendor(vendor),
		WorkspaceID: workspaceID,
		ProviderID:  providerID,
		Verifier:    verifier,
		State:       state,
		CreatedAt:   time.Now(),
	})
	return LoginStart{
		AuthURL:      d.authorizeURL + "?" + q.Encode(),
		State:        state,
		Instructions: d.instructions,
	}, nil
}

// parsePastedCode extracts (code, state) from whatever the user pasted: a full
// redirect URL, a bare `code#state` pair, or a bare code (state empty → the
// pending state is implied).
func parsePastedCode(pasted string) (code, state string, err error) {
	pasted = strings.TrimSpace(pasted)
	if pasted == "" {
		return "", "", &Error{Kind: "validation", Message: "missing authorization code"}
	}
	if u, parseErr := url.Parse(pasted); parseErr == nil && u.Scheme != "" && u.Host != "" {
		q := u.Query()
		code = q.Get("code")
		state = q.Get("state")
		if code == "" {
			return "", "", &Error{Kind: "validation", Message: "missing authorization code in redirect URL"}
		}
		return code, state, nil
	}
	return pasted, "", nil
}

// CompleteLogin finishes a browser login: parses the pasted redirect, checks
// the state against the pending attempt, exchanges the code, enriches the
// result (bootstrap/JWT/project discovery), and stores the account.
func (m *Manager) CompleteLogin(ctx context.Context, userID, workspaceID, providerID, state, pasted string) (storage.WorkspaceProviderAccount, error) {
	p, ok := m.getPending(state)
	if !ok {
		return storage.WorkspaceProviderAccount{}, &Error{Kind: "validation", Message: "unknown or expired login state"}
	}
	if p.WorkspaceID != workspaceID || p.ProviderID != providerID {
		return storage.WorkspaceProviderAccount{}, &Error{Kind: "validation", Message: "state mismatch: login belongs to a different workspace or provider"}
	}
	code, pastedState, err := parsePastedCode(pasted)
	if err != nil {
		return storage.WorkspaceProviderAccount{}, err
	}
	if pastedState != "" && pastedState != p.State {
		return storage.WorkspaceProviderAccount{}, &Error{Kind: "validation", Message: "state mismatch in pasted redirect"}
	}

	res, err := m.exchangeLogin(ctx, p, code)
	if err != nil {
		return storage.WorkspaceProviderAccount{}, err
	}
	m.deletePending(state)

	return m.store.CreateProviderAccount(ctx, userID, workspaceID, p.ProviderID, storage.ProviderAccountInput{
		Email:        res.Email,
		AccountID:    res.AccountID,
		OrgID:        res.OrgID,
		OrgName:      res.OrgName,
		ProjectID:    res.ProjectID,
		Plan:         res.Plan,
		AccessToken:  res.AccessToken,
		RefreshToken: res.RefreshToken,
		ExpiresAt:    res.ExpiresAt,
	})
}

// exchangeLogin runs the token exchange plus the vendor's after-exchange
// enrichment. Split from CompleteLogin so tests exercise the HTTP shapes
// without a store.
func (m *Manager) exchangeLogin(ctx context.Context, p PendingLogin, code string) (TokenResult, error) {
	d, err := m.descriptorFor(p.Vendor)
	if err != nil {
		return TokenResult{}, err
	}
	res, err := m.exchangeCode(ctx, d, code, p.Verifier, d.redirectURI, p.State)
	if err != nil {
		return TokenResult{}, err
	}
	if err := m.enrichLogin(ctx, d, &res); err != nil {
		return TokenResult{}, err
	}
	return res, nil
}

// --- token endpoint ----------------------------------------------------------

// tokenResponse is the union of the three vendors' token-endpoint bodies.
type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token"`
	ExpiresIn    int64  `json:"expires_in"`

	Account *struct {
		UUID         string `json:"uuid"`
		EmailAddress string `json:"email_address"`
	} `json:"account"`
	Organization *struct {
		UUID string `json:"uuid"`
		Name string `json:"name"`
	} `json:"organization"`
}

// exchangeCode POSTs the authorization code to the vendor token endpoint.
// state is sent only where the vendor expects it (Claude echoes it back in the
// JSON body).
func (m *Manager) exchangeCode(ctx context.Context, d *providerDescriptor, code, verifier, redirectURI, state string) (TokenResult, error) {
	var body io.Reader
	if d.tokenBody == tokenBodyJSON {
		payload := map[string]string{
			"grant_type":    "authorization_code",
			"client_id":     d.clientID,
			"code":          code,
			"redirect_uri":  redirectURI,
			"code_verifier": verifier,
			"state":         state,
		}
		raw, err := json.Marshal(payload)
		if err != nil {
			return TokenResult{}, err
		}
		body = strings.NewReader(string(raw))
	} else {
		form := url.Values{
			"grant_type":   {"authorization_code"},
			"client_id":    {d.clientID},
			"code":         {code},
			"redirect_uri": {redirectURI},
		}
		if verifier != "" {
			form.Set("code_verifier", verifier)
		}
		if d.clientSecret != "" {
			form.Set("client_secret", d.clientSecret)
		}
		body = strings.NewReader(form.Encode())
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.tokenURL, body)
	if err != nil {
		return TokenResult{}, err
	}
	if d.tokenBody == tokenBodyJSON {
		req.Header.Set("Content-Type", "application/json")
	} else {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}

	client := m.client
	if d.tokenTimeout > 0 {
		// Per-request timeout via context so the shared client stays usable.
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, d.tokenTimeout)
		defer cancel()
		req = req.WithContext(ctx)
	}
	resp, err := client.Do(req)
	if err != nil {
		return TokenResult{}, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return TokenResult{}, err
	}
	if resp.StatusCode/100 != 2 {
		return TokenResult{}, &Error{
			Kind:    "token-exchange",
			Status:  resp.StatusCode,
			Message: fmt.Sprintf("token exchange failed: %d %s", resp.StatusCode, truncate(string(raw), 300)),
		}
	}
	var tr tokenResponse
	if err := json.Unmarshal(raw, &tr); err != nil {
		return TokenResult{}, &Error{Kind: "validation", Message: "token endpoint returned invalid JSON: " + truncate(string(raw), 300)}
	}
	if tr.AccessToken == "" {
		return TokenResult{}, &Error{Kind: "validation", Message: "token response missing access_token"}
	}

	res := TokenResult{
		AccessToken:  tr.AccessToken,
		RefreshToken: tr.RefreshToken,
		IDToken:      tr.IDToken,
		ExpiresAt:    time.Now().Add(time.Duration(tr.ExpiresIn)*time.Second - d.expiresSkew),
	}
	if tr.Account != nil {
		res.AccountID = tr.Account.UUID
		res.Email = tr.Account.EmailAddress
	}
	if tr.Organization != nil {
		res.OrgID = tr.Organization.UUID
		res.OrgName = tr.Organization.Name
	}
	return res, nil
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

// --- after-exchange enrichment ----------------------------------------------

// enrichLogin fills identity/project fields the token response omits, per
// vendor. Errors are fatal for codex (identity is required) and antigravity
// (project is required); claude's bootstrap fill is best-effort.
func (m *Manager) enrichLogin(ctx context.Context, d *providerDescriptor, res *TokenResult) error {
	switch d.vendor {
	case ai.VendorClaudeCode:
		if res.AccountID != "" && res.Email != "" && res.OrgID != "" {
			return nil
		}
		id, err := m.fetchClaudeBootstrap(ctx, d, res.AccessToken)
		if err != nil {
			return nil // best-effort, matching the TS hook's catch
		}
		if res.AccountID == "" {
			res.AccountID = id.accountID
		}
		if res.Email == "" {
			res.Email = id.email
		}
		if res.OrgID == "" {
			res.OrgID = id.orgID
		}
		if res.OrgName == "" {
			res.OrgName = id.orgName
		}
		return nil

	case ai.VendorOpenAICodex:
		prof := codexTokenProfile(res.AccessToken, res.IDToken)
		if prof.accountID == "" && prof.email == "" {
			return &Error{Kind: "validation", Message: "failed to extract account identity from token"}
		}
		if prof.accountID != "" {
			res.AccountID = prof.accountID
			res.OrgID = prof.accountID // the ChatGPT workspace is the org
		}
		if prof.email != "" {
			res.Email = prof.email
		}
		if prof.planType != "" {
			res.OrgName = prof.planType
			res.Plan = prof.planType
		}
		return nil

	case ai.VendorGoogleAntigravity:
		if res.RefreshToken == "" {
			return &Error{Kind: "validation", Message: "no refresh token received; please try again"}
		}
		if res.Email == "" {
			if email, err := m.fetchGoogleUserEmail(ctx, d, res.AccessToken); err == nil {
				res.Email = email
			}
		}
		projectID, err := m.discoverAntigravityProject(ctx, d, res.AccessToken)
		if err != nil {
			return err
		}
		res.ProjectID = projectID
		return nil
	}
	return nil
}

// --- claude bootstrap --------------------------------------------------------

const (
	claudeBootstrapUA = "claude-cli/2.1.257 (external, cli)"
	claudeBetaOAuth   = "oauth-2025-04-20"
)

type claudeIdentity struct {
	accountID, email, orgID, orgName string
}

// fetchClaudeBootstrap reads /api/claude_cli/bootstrap for account/org
// identity when the token response omits it (anthropic-identity hook).
func (m *Manager) fetchClaudeBootstrap(ctx context.Context, d *providerDescriptor, accessToken string) (claudeIdentity, error) {
	u := d.bootstrapURL + "?entrypoint=cli&model=claude-opus-4-8"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return claudeIdentity{}, err
	}
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", claudeBootstrapUA)
	req.Header.Set("anthropic-beta", claudeBetaOAuth)

	resp, err := m.client.Do(req)
	if err != nil {
		return claudeIdentity{}, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return claudeIdentity{}, err
	}
	if resp.StatusCode/100 != 2 {
		return claudeIdentity{}, &Error{Kind: "provisioning", Status: resp.StatusCode,
			Message: fmt.Sprintf("claude bootstrap failed: %d %s", resp.StatusCode, truncate(string(raw), 200))}
	}
	var body struct {
		OAuthAccount *struct {
			AccountUUID      string `json:"account_uuid"`
			AccountEmail     string `json:"account_email"`
			OrganizationUUID string `json:"organization_uuid"`
			OrganizationName string `json:"organization_name"`
		} `json:"oauth_account"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return claudeIdentity{}, err
	}
	var id claudeIdentity
	if body.OAuthAccount != nil {
		id.accountID = body.OAuthAccount.AccountUUID
		id.email = body.OAuthAccount.AccountEmail
		id.orgID = body.OAuthAccount.OrganizationUUID
		id.orgName = body.OAuthAccount.OrganizationName
	}
	return id, nil
}

// --- codex JWT profile --------------------------------------------------------

const (
	codexJWTAuthClaim    = "https://api.openai.com/auth"
	codexJWTProfileClaim = "https://api.openai.com/profile"
)

type codexProfile struct {
	accountID, email, planType string
}

// decodeJWTPayload decodes the payload segment of a JWT without verifying the
// signature — the token came straight from the vendor's token endpoint over
// TLS, so claims are trustworthy for identity display.
func decodeJWTPayload(token string) map[string]any {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		// Some issuers pad; retry with padding tolerated.
		raw, err = base64.URLEncoding.DecodeString(parts[1])
		if err != nil {
			return nil
		}
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil
	}
	return payload
}

func claimString(payload map[string]any, claim, key string) string {
	sub, _ := payload[claim].(map[string]any)
	v, _ := sub[key].(string)
	return v
}

// codexTokenProfile extracts chatgpt_account_id / email / plan_type from the
// access token, falling back to the id token (getTokenProfile in the TS).
func codexTokenProfile(accessToken, idToken string) codexProfile {
	access := decodeJWTPayload(accessToken)
	var id map[string]any
	if idToken != "" {
		id = decodeJWTPayload(idToken)
	}
	pick := func(claim, key string) string {
		if v := claimString(access, claim, key); v != "" {
			return v
		}
		return claimString(id, claim, key)
	}
	return codexProfile{
		accountID: pick(codexJWTAuthClaim, "chatgpt_account_id"),
		email:     strings.ToLower(strings.TrimSpace(pick(codexJWTProfileClaim, "email"))),
		planType:  strings.ToLower(strings.TrimSpace(pick(codexJWTAuthClaim, "chatgpt_plan_type"))),
	}
}

// --- google userinfo + project discovery --------------------------------------

const (
	antigravityFreeTierID  = "free-tier"
	antigravityOnboardCap  = 30 * time.Second
	antigravityOnboardPoll = 1 * time.Second
	antigravityUserAgent   = "antigravity/hub/2.8.0 (aidev_client; os_type=darwin; arch=arm64; cl=963137146)"
)

func (m *Manager) fetchGoogleUserEmail(ctx context.Context, d *providerDescriptor, accessToken string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.userinfoURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	resp, err := m.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode/100 != 2 {
		return "", &Error{Kind: "provisioning", Status: resp.StatusCode,
			Message: fmt.Sprintf("google userinfo failed: %d", resp.StatusCode)}
	}
	var body struct {
		Email string `json:"email"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return "", err
	}
	return body.Email, nil
}

// cloudCodeRequest POSTs (or GETs) a Cloud Code Assist internal endpoint.
func (m *Manager) cloudCodeRequest(ctx context.Context, method, url, accessToken string, body any) (map[string]any, error) {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = strings.NewReader(string(raw))
	}
	req, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", antigravityUserAgent)
	resp, err := m.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, &Error{Kind: "provisioning", Status: resp.StatusCode,
			Message: fmt.Sprintf("%s %s failed: %d %s", method, url, resp.StatusCode, truncate(string(raw), 300))}
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, &Error{Kind: "provisioning", Message: "cloud code assist returned invalid JSON"}
	}
	return payload, nil
}

var antigravityMetadata = map[string]any{"ideType": "ANTIGRAVITY"}

// loadCodeAssist POSTs :loadCodeAssist and, when the response carries a
// project but no paidTier, re-posts with the project attached (the TS does the
// same to force tier resolution).
func (m *Manager) loadCodeAssist(ctx context.Context, d *providerDescriptor, accessToken string) (map[string]any, error) {
	payload, err := m.cloudCodeRequest(ctx, http.MethodPost,
		d.cloudCodeEndpoint+"/v1internal:loadCodeAssist", accessToken,
		map[string]any{"metadata": antigravityMetadata})
	if err != nil {
		return nil, err
	}
	project := extractAntigravityProject(payload)
	if _, hasPaid := payload["paidTier"]; !hasPaid && project != "" {
		payload, err = m.cloudCodeRequest(ctx, http.MethodPost,
			d.cloudCodeEndpoint+"/v1internal:loadCodeAssist", accessToken,
			map[string]any{"cloudaicompanionProject": project, "metadata": antigravityMetadata})
		if err != nil {
			return nil, err
		}
	}
	return payload, nil
}

// extractAntigravityProject reads cloudaicompanionProject as either a bare
// string or an {id: ...} object — both shapes appear in the wild.
func extractAntigravityProject(payload map[string]any) string {
	switch v := payload["cloudaicompanionProject"].(type) {
	case string:
		return v
	case map[string]any:
		s, _ := v["id"].(string)
		return s
	}
	return ""
}

// discoverAntigravityProject resolves the Cloud Code Assist project after
// login: loadCodeAssist → optional free-tier onboardUser LRO → reload.
func (m *Manager) discoverAntigravityProject(ctx context.Context, d *providerDescriptor, accessToken string) (string, error) {
	initial, err := m.loadCodeAssist(ctx, d, accessToken)
	if err != nil {
		return "", err
	}
	if err := assertFreeTierEligible(initial); err != nil {
		return "", err
	}
	if _, hasTier := initial["currentTier"]; !hasTier {
		if err := m.onboardAntigravityUser(ctx, d, accessToken); err != nil {
			return "", err
		}
	}
	refreshed, err := m.loadCodeAssist(ctx, d, accessToken)
	if err != nil {
		return "", err
	}
	if project := extractAntigravityProject(refreshed); project != "" {
		return project, nil
	}
	return "", &Error{Kind: "provisioning", Message: "loadCodeAssist did not return a cloudaicompanionProject"}
}

// assertFreeTierEligible mirrors the TS check: when the free tier is neither
// allowed nor explicitly ineligible, onboarding is still attempted; an
// ineligible entry with a reason aborts login with that reason.
func assertFreeTierEligible(payload map[string]any) error {
	allowed := false
	if tiers, ok := payload["allowedTiers"].([]any); ok {
		for _, t := range tiers {
			if m, _ := t.(map[string]any); m != nil && m["id"] == antigravityFreeTierID {
				allowed = true
			}
		}
	}
	if allowed {
		return nil
	}
	if tiers, ok := payload["ineligibleTiers"].([]any); ok {
		for _, t := range tiers {
			m, _ := t.(map[string]any)
			if m == nil || m["tierId"] != antigravityFreeTierID {
				continue
			}
			reason, _ := m["reasonMessage"].(string)
			if reason == "" {
				return nil
			}
			if vurl, _ := m["validationUrl"].(string); vurl != "" {
				reason += "\n" + vurl
			}
			return &Error{Kind: "provisioning", Message: reason}
		}
	}
	return nil
}

// onboardAntigravityUser starts the free-tier LRO and polls the returned
// operation until done (30s cap, 1s interval).
func (m *Manager) onboardAntigravityUser(ctx context.Context, d *providerDescriptor, accessToken string) error {
	deadline := time.Now().Add(antigravityOnboardCap)
	op, err := m.cloudCodeRequest(ctx, http.MethodPost,
		d.cloudCodeEndpoint+"/v1internal:onboardUser", accessToken,
		map[string]any{"tierId": antigravityFreeTierID, "metadata": antigravityMetadata})
	if err != nil {
		return err
	}
	for {
		if done, _ := op["done"].(bool); done {
			if opErr, _ := op["error"].(map[string]any); opErr != nil {
				msg, _ := opErr["message"].(string)
				return &Error{Kind: "provisioning", Message: "onboardUser operation failed: " + msg}
			}
			if op["response"] == nil {
				return &Error{Kind: "provisioning", Message: "onboardUser finished without a response"}
			}
			return nil
		}
		name, _ := op["name"].(string)
		if name == "" {
			return &Error{Kind: "provisioning", Message: "onboardUser returned an operation without a name"}
		}
		if time.Now().Add(antigravityOnboardPoll).After(deadline) {
			return &Error{Kind: "timeout", Message: "onboardUser timed out after 30s"}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(antigravityOnboardPoll):
		}
		op, err = m.cloudCodeRequest(ctx, http.MethodPost,
			d.cloudCodeEndpoint+"/v1internal/"+name, accessToken, nil)
		if err != nil {
			return err
		}
	}
}
