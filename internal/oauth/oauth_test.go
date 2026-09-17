package oauth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lohi-ai/agentray/agentcore"
	"github.com/lohi-ai/agentray/ai"
	storage "github.com/lohi-ai/agentray/internal/dataplane/store"
)

// testDescriptors returns the shipped descriptor set with every endpoint
// pointed at the httptest server.
func testDescriptors(base string) map[string]*providerDescriptor {
	d := defaultDescriptors()
	for _, desc := range d {
		desc.authorizeURL = base + "/authorize"
		desc.tokenURL = base + "/token"
		desc.bootstrapURL = base + "/bootstrap"
		desc.userinfoURL = base + "/userinfo"
		desc.cloudCodeEndpoint = base
		desc.usageURL = base + "/usage"
		desc.deviceUsercodeURL = base + "/device/usercode"
		desc.deviceTokenURL = base + "/device/token"
		desc.deviceVerifyURL = base + "/device"
	}
	// Antigravity's usageURL is a path suffix on cloudCodeEndpoint in prod;
	// under test it is a full URL on the same server.
	d[ai.VendorGoogleAntigravity].usageURL = "/usage"
	d[ai.VendorGoogleAntigravity].cloudCodeEndpoint = base
	return d
}

func testManager(t *testing.T, handler http.HandlerFunc) (*Manager, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	m := &Manager{
		client:      srv.Client(),
		descriptors: testDescriptors(srv.URL),
		pending:     map[string]PendingLogin{},
	}
	m.refresh = &refresher{
		descriptors:  m.descriptors,
		client:       m.client,
		updateTokens: func(context.Context, string, string, string, time.Time) error { return nil },
	}
	return m, srv
}

func makeJWT(t *testing.T, claims map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`)) + "." +
		base64.RawURLEncoding.EncodeToString(raw) + ".sig"
}

// --- authorize URLs ----------------------------------------------------------

func TestStartLoginAuthURL(t *testing.T) {
	m, _ := testManager(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request: %s", r.URL)
	})

	tests := []struct {
		vendor     string
		wantParams map[string]string
		wantScope  []string
		wantPKCE   bool
	}{
		{
			vendor: ai.VendorClaudeCode,
			wantParams: map[string]string{
				"client_id":     "9d1c250a-e61b-44d9-88ed-5944d1962f5e",
				"redirect_uri":  "http://localhost:54545/callback",
				"response_type": "code",
				"code":          "true",
			},
			wantScope: []string{"org:create_api_key", "user:profile", "user:inference",
				"user:sessions:claude_code", "user:mcp_servers", "user:file_upload"},
			wantPKCE: true,
		},
		{
			vendor: ai.VendorOpenAICodex,
			wantParams: map[string]string{
				"client_id":                  "app_EMoamEEZ73f0CkXaXp7hrann",
				"redirect_uri":               "http://localhost:1455/auth/callback",
				"response_type":              "code",
				"id_token_add_organizations": "true",
				"codex_cli_simplified_flow":  "true",
				"originator":                 "omp",
			},
			wantScope: []string{"openid", "profile", "email", "offline_access",
				"api.connectors.read", "api.connectors.invoke"},
			wantPKCE: true,
		},
		{
			vendor: ai.VendorGoogleAntigravity,
			wantParams: map[string]string{
				"client_id":    m.descriptors[ai.VendorGoogleAntigravity].clientID,
				"redirect_uri": "http://127.0.0.1:51121/oauth-callback",
				"access_type":  "offline",
				"prompt":       "consent",
			},
			wantScope: []string{
				"https://www.googleapis.com/auth/cloud-platform",
				"https://www.googleapis.com/auth/userinfo.email",
				"https://www.googleapis.com/auth/userinfo.profile",
				"https://www.googleapis.com/auth/cclog",
				"https://www.googleapis.com/auth/experimentsandconfigs",
			},
			wantPKCE: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.vendor, func(t *testing.T) {
			start, err := m.startLogin(tc.vendor, "ws1", "prov1")
			if err != nil {
				t.Fatal(err)
			}
			u, err := url.Parse(start.AuthURL)
			if err != nil {
				t.Fatal(err)
			}
			q := u.Query()
			for k, want := range tc.wantParams {
				if got := q.Get(k); got != want {
					t.Errorf("param %s = %q, want %q", k, got, want)
				}
			}
			if got := strings.Split(q.Get("scope"), " "); strings.Join(got, " ") != strings.Join(tc.wantScope, " ") {
				t.Errorf("scope = %v, want %v", got, tc.wantScope)
			}
			if tc.wantPKCE {
				if q.Get("code_challenge") == "" || q.Get("code_challenge_method") != "S256" {
					t.Error("missing PKCE challenge")
				}
			} else if q.Get("code_challenge") != "" {
				t.Error("unexpected PKCE challenge")
			}
			if q.Get("state") == "" || q.Get("state") != start.State {
				t.Error("state missing or not echoed")
			}
			if _, ok := m.getPending(start.State); !ok {
				t.Error("pending login not recorded")
			}
		})
	}
}

// --- code exchange shapes ----------------------------------------------------

func TestClaudeExchangeJSON(t *testing.T) {
	var gotBody map[string]string
	var gotCT string
	var bootstrapHit atomic.Bool
	m, _ := testManager(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			gotCT = r.Header.Get("Content-Type")
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &gotBody)
			w.Header().Set("Content-Type", "application/json")
			// Omit account/org so the bootstrap fill runs.
			_, _ = w.Write([]byte(`{"access_token":"at-1","refresh_token":"rt-1","expires_in":3600}`))
		case "/bootstrap":
			bootstrapHit.Store(true)
			if got := r.Header.Get("User-Agent"); got != claudeBootstrapUA {
				t.Errorf("bootstrap UA = %q", got)
			}
			if got := r.Header.Get("anthropic-beta"); got != claudeBetaOAuth {
				t.Errorf("bootstrap beta = %q", got)
			}
			if got := r.Header.Get("Authorization"); got != "Bearer at-1" {
				t.Errorf("bootstrap auth = %q", got)
			}
			_, _ = w.Write([]byte(`{"oauth_account":{"account_uuid":"acc-9","account_email":"me@x.com","organization_uuid":"org-7","organization_name":"My Org"}}`))
		default:
			http.NotFound(w, r)
		}
	})

	p := PendingLogin{Vendor: ai.VendorClaudeCode, Verifier: "ver-1", State: "st-1"}
	res, err := m.exchangeLogin(context.Background(), p, "code-1")
	if err != nil {
		t.Fatal(err)
	}
	if gotCT != "application/json" {
		t.Errorf("content-type = %q, want JSON", gotCT)
	}
	for k, want := range map[string]string{
		"grant_type": "authorization_code", "client_id": "9d1c250a-e61b-44d9-88ed-5944d1962f5e",
		"code": "code-1", "redirect_uri": "http://localhost:54545/callback",
		"code_verifier": "ver-1", "state": "st-1",
	} {
		if gotBody[k] != want {
			t.Errorf("body[%s] = %q, want %q", k, gotBody[k], want)
		}
	}
	if !bootstrapHit.Load() {
		t.Error("bootstrap fill did not run")
	}
	if res.AccessToken != "at-1" || res.RefreshToken != "rt-1" {
		t.Errorf("tokens = %q/%q", res.AccessToken, res.RefreshToken)
	}
	if res.AccountID != "acc-9" || res.Email != "me@x.com" || res.OrgID != "org-7" || res.OrgName != "My Org" {
		t.Errorf("identity = %+v", res)
	}
	// 3600s minus the 300s skew.
	if d := time.Until(res.ExpiresAt); d < 50*time.Minute || d > 56*time.Minute {
		t.Errorf("expiresAt skew off: %v", d)
	}
}

func TestCodexExchangeForm(t *testing.T) {
	jwt := makeJWT(t, map[string]any{
		codexJWTAuthClaim:    map[string]any{"chatgpt_account_id": "acct-42", "chatgpt_plan_type": "PRO"},
		codexJWTProfileClaim: map[string]any{"email": "  User@Example.COM "},
	})
	var gotForm url.Values
	var gotCT string
	m, _ := testManager(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/token" {
			http.NotFound(w, r)
			return
		}
		gotCT = r.Header.Get("Content-Type")
		raw, _ := io.ReadAll(r.Body)
		gotForm, _ = url.ParseQuery(string(raw))
		w.Header().Set("Content-Type", "application/json")
		resp := map[string]any{"access_token": jwt, "refresh_token": "rt-9", "expires_in": 3600}
		_ = json.NewEncoder(w).Encode(resp)
	})

	p := PendingLogin{Vendor: ai.VendorOpenAICodex, Verifier: "ver-2", State: "st-2"}
	res, err := m.exchangeLogin(context.Background(), p, "code-2")
	if err != nil {
		t.Fatal(err)
	}
	if gotCT != "application/x-www-form-urlencoded" {
		t.Errorf("content-type = %q, want form", gotCT)
	}
	for k, want := range map[string]string{
		"grant_type": "authorization_code", "client_id": "app_EMoamEEZ73f0CkXaXp7hrann",
		"code": "code-2", "redirect_uri": "http://localhost:1455/auth/callback",
		"code_verifier": "ver-2",
	} {
		if gotForm.Get(k) != want {
			t.Errorf("form[%s] = %q, want %q", k, gotForm.Get(k), want)
		}
	}
	if gotForm.Get("state") != "" {
		t.Error("codex form must not carry state")
	}
	if res.AccountID != "acct-42" || res.OrgID != "acct-42" {
		t.Errorf("account/org = %q/%q", res.AccountID, res.OrgID)
	}
	if res.Email != "user@example.com" {
		t.Errorf("email = %q, want normalized lowercase", res.Email)
	}
	if res.Plan != "pro" || res.OrgName != "pro" {
		t.Errorf("plan = %q orgName = %q", res.Plan, res.OrgName)
	}
}

func TestCodexExchangeMissingIdentityFails(t *testing.T) {
	m, _ := testManager(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Opaque access token: no JWT claims to extract.
		_, _ = w.Write([]byte(`{"access_token":"opaque","refresh_token":"rt","expires_in":60}`))
	})
	p := PendingLogin{Vendor: ai.VendorOpenAICodex, Verifier: "v", State: "s"}
	if _, err := m.exchangeLogin(context.Background(), p, "c"); err == nil {
		t.Fatal("expected identity extraction failure")
	}
}

func TestAntigravityExchangeForm(t *testing.T) {
	var gotForm url.Values
	var loadAssistHits int
	m, _ := testManager(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/token":
			raw, _ := io.ReadAll(r.Body)
			gotForm, _ = url.ParseQuery(string(raw))
			_, _ = w.Write([]byte(`{"access_token":"at-g","refresh_token":"rt-g","expires_in":3600}`))
		case r.URL.Path == "/userinfo":
			if r.Header.Get("Authorization") != "Bearer at-g" {
				t.Errorf("userinfo auth = %q", r.Header.Get("Authorization"))
			}
			_, _ = w.Write([]byte(`{"email":"g@example.com"}`))
		case r.URL.Path == "/v1internal:loadCodeAssist":
			loadAssistHits++
			var body map[string]any
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &body)
			if meta, _ := body["metadata"].(map[string]any); meta["ideType"] != "ANTIGRAVITY" {
				t.Errorf("loadCodeAssist metadata = %v", body["metadata"])
			}
			_, _ = w.Write([]byte(`{"cloudaicompanionProject":"proj-1","currentTier":{"id":"free-tier"},"paidTier":{"id":"paid"}}`))
		default:
			http.NotFound(w, r)
		}
	})

	p := PendingLogin{Vendor: ai.VendorGoogleAntigravity, State: "st-3"}
	res, err := m.exchangeLogin(context.Background(), p, "code-3")
	if err != nil {
		t.Fatal(err)
	}
	if gotForm.Get("client_secret") != "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" {
		t.Errorf("client_secret = %q", gotForm.Get("client_secret"))
	}
	if gotForm.Get("code_verifier") != "" {
		t.Error("antigravity has no PKCE")
	}
	if res.Email != "g@example.com" || res.ProjectID != "proj-1" {
		t.Errorf("email/project = %q/%q", res.Email, res.ProjectID)
	}
	if loadAssistHits != 1 {
		t.Errorf("loadCodeAssist hits = %d, want 1 (project+currentTier on the first response → no reload, no onboard)", loadAssistHits)
	}
}

func TestAntigravityOnboardFlow(t *testing.T) {
	var onboarded, opPolled bool
	m, _ := testManager(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1internal:loadCodeAssist":
			if onboarded {
				_, _ = w.Write([]byte(`{"cloudaicompanionProject":{"id":"proj-9"},"currentTier":{"id":"free-tier"}}`))
			} else {
				// No tier, no project, free tier allowed → onboard.
				_, _ = w.Write([]byte(`{"allowedTiers":[{"id":"free-tier"}]}`))
			}
		case "/v1internal:onboardUser":
			onboarded = true
			_, _ = w.Write([]byte(`{"name":"operations/abc","done":false}`))
		case "/v1internal/operations/abc":
			opPolled = true
			_, _ = w.Write([]byte(`{"done":true,"response":{"cloudaicompanionProject":"proj-9"}}`))
		default:
			http.NotFound(w, r)
		}
	})
	d := m.descriptors[ai.VendorGoogleAntigravity]
	project, err := m.discoverAntigravityProject(context.Background(), d, "at-g")
	if err != nil {
		t.Fatal(err)
	}
	if project != "proj-9" {
		t.Errorf("project = %q", project)
	}
	if !onboarded || !opPolled {
		t.Error("onboard LRO was not driven to completion")
	}
}

// --- device flow -------------------------------------------------------------

func TestCodexDeviceFlow(t *testing.T) {
	jwt := makeJWT(t, map[string]any{
		codexJWTAuthClaim:    map[string]any{"chatgpt_account_id": "acct-d"},
		codexJWTProfileClaim: map[string]any{"email": "d@x.com"},
	})
	var pollCount atomic.Int32
	var exchangeRedirect string
	m, _ := testManager(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/device/usercode":
			var body map[string]string
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &body)
			if body["client_id"] != "app_EMoamEEZ73f0CkXaXp7hrann" {
				t.Errorf("usercode client_id = %q", body["client_id"])
			}
			_, _ = w.Write([]byte(`{"device_auth_id":"da-1","user_code":"ABCD-EFGH","interval":5}`))
		case "/device/token":
			if pollCount.Add(1) == 1 {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			_, _ = w.Write([]byte(`{"authorization_code":"dev-code","code_verifier":"dev-ver"}`))
		case "/token":
			raw, _ := io.ReadAll(r.Body)
			form, _ := url.ParseQuery(string(raw))
			exchangeRedirect = form.Get("redirect_uri")
			if form.Get("code_verifier") != "dev-ver" || form.Get("code") != "dev-code" {
				t.Errorf("device exchange form = %v", form)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token": jwt, "refresh_token": "rt-d", "expires_in": 3600,
			})
		default:
			http.NotFound(w, r)
		}
	})

	start, err := m.startDeviceLogin(context.Background(), m.descriptors[ai.VendorOpenAICodex], "ws1", "prov1")
	if err != nil {
		t.Fatal(err)
	}
	if start.UserCode != "ABCD-EFGH" || start.VerificationURL == "" || start.PendingID == "" {
		t.Fatalf("bad device start: %+v", start)
	}

	// First poll: 403 → pending.
	res, done, err := m.pollDeviceOnce(context.Background(), m.descriptors[ai.VendorOpenAICodex],
		PendingLogin{Vendor: ai.VendorOpenAICodex, DeviceAuthID: "da-1", UserCode: "ABCD-EFGH"})
	if err != nil || done {
		t.Fatalf("first poll: done=%v err=%v", done, err)
	}
	// Second poll: code + verifier → exchange with the device redirect URI.
	res, done, err = m.pollDeviceOnce(context.Background(), m.descriptors[ai.VendorOpenAICodex],
		PendingLogin{Vendor: ai.VendorOpenAICodex, DeviceAuthID: "da-1", UserCode: "ABCD-EFGH"})
	if err != nil || !done {
		t.Fatalf("second poll: done=%v err=%v", done, err)
	}
	if exchangeRedirect != "https://auth.openai.com/deviceauth/callback" {
		t.Errorf("device exchange redirect_uri = %q", exchangeRedirect)
	}
	if res.AccountID != "acct-d" || res.Email != "d@x.com" {
		t.Errorf("device result = %+v", res)
	}
}

// --- refresh -----------------------------------------------------------------

func TestRefreshShapes(t *testing.T) {
	type observed struct {
		ct      string
		headers http.Header
		body    string
	}
	var last observed
	m, _ := testManager(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		last = observed{ct: r.Header.Get("Content-Type"), headers: r.Header.Clone(), body: string(raw)}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"new-at","refresh_token":"new-rt","expires_in":3600}`))
	})

	var updated atomic.Int32
	var gotAccess, gotRefresh string
	m.refresh.updateTokens = func(_ context.Context, id, access, refresh string, exp time.Time) error {
		updated.Add(1)
		gotAccess, gotRefresh = access, refresh
		return nil
	}

	// claude: JSON body + beta/SDK headers.
	_, err := m.refresh.refreshAccount(context.Background(), storage.WorkspaceProviderAccountRecord{
		ID: "a1", Vendor: ai.VendorClaudeCode, RefreshToken: "rt-old",
	})
	if err != nil {
		t.Fatal(err)
	}
	if last.ct != "application/json" {
		t.Errorf("claude refresh ct = %q", last.ct)
	}
	var jb map[string]string
	_ = json.Unmarshal([]byte(last.body), &jb)
	if jb["grant_type"] != "refresh_token" || jb["refresh_token"] != "rt-old" || jb["client_id"] == "" {
		t.Errorf("claude refresh body = %v", jb)
	}
	if last.headers.Get("anthropic-beta") != "oauth-2025-04-20" {
		t.Error("claude refresh missing anthropic-beta")
	}
	if !strings.Contains(last.headers.Get("User-Agent"), "userOAuthProvider") {
		t.Errorf("claude refresh UA = %q", last.headers.Get("User-Agent"))
	}
	if gotAccess != "new-at" || gotRefresh != "new-rt" {
		t.Errorf("stored tokens = %q/%q", gotAccess, gotRefresh)
	}

	// codex: form body, no extra headers.
	_, err = m.refresh.refreshAccount(context.Background(), storage.WorkspaceProviderAccountRecord{
		ID: "a2", Vendor: ai.VendorOpenAICodex, RefreshToken: "rt-c",
	})
	if err != nil {
		t.Fatal(err)
	}
	form, _ := url.ParseQuery(last.body)
	if last.ct != "application/x-www-form-urlencoded" ||
		form.Get("grant_type") != "refresh_token" || form.Get("refresh_token") != "rt-c" {
		t.Errorf("codex refresh = %q ct=%q", last.body, last.ct)
	}

	// antigravity: form body + client_secret.
	_, err = m.refresh.refreshAccount(context.Background(), storage.WorkspaceProviderAccountRecord{
		ID: "a3", Vendor: ai.VendorGoogleAntigravity, RefreshToken: "rt-g",
	})
	if err != nil {
		t.Fatal(err)
	}
	form, _ = url.ParseQuery(last.body)
	if form.Get("client_secret") != "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" {
		t.Errorf("antigravity refresh missing client_secret: %q", last.body)
	}
	if updated.Load() != 3 {
		t.Errorf("updateTokens calls = %d", updated.Load())
	}
}

func TestRefreshKeepsStoredTokenWhenOmitted(t *testing.T) {
	m, _ := testManager(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"access_token":"new-at","expires_in":3600}`))
	})
	var gotRefresh string
	m.refresh.updateTokens = func(_ context.Context, id, access, refresh string, exp time.Time) error {
		gotRefresh = refresh
		return nil
	}
	_, err := m.refresh.refreshAccount(context.Background(), storage.WorkspaceProviderAccountRecord{
		ID: "a1", Vendor: ai.VendorOpenAICodex, RefreshToken: "keep-me",
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotRefresh != "keep-me" {
		t.Errorf("refresh token = %q, want stored token preserved", gotRefresh)
	}
}

// --- pool --------------------------------------------------------------------

type fakePoolStore struct {
	accounts []storage.WorkspaceProviderAccountRecord
	acquires int

	touched  []string
	blocked  map[string]time.Time
	disabled map[string]string
}

func (f *fakePoolStore) AcquireProviderAccount(ctx context.Context, providerID string) (storage.WorkspaceProviderAccountRecord, error) {
	f.acquires++
	if len(f.accounts) == 0 {
		return storage.WorkspaceProviderAccountRecord{}, &Error{Kind: "validation", Message: "no accounts"}
	}
	rec := f.accounts[0]
	f.accounts = f.accounts[1:]
	return rec, nil
}

func (f *fakePoolStore) LoadProviderAccountRecord(ctx context.Context, accountID string) (storage.WorkspaceProviderAccountRecord, error) {
	for _, r := range f.accounts {
		if r.ID == accountID {
			return r, nil
		}
	}
	return storage.WorkspaceProviderAccountRecord{}, &Error{Kind: "validation", Message: "unknown account"}
}

func (f *fakePoolStore) UpdateProviderAccountTokens(ctx context.Context, accountID, access, refresh string, expiresAt time.Time) error {
	return nil
}

func (f *fakePoolStore) BlockProviderAccount(ctx context.Context, accountID string, until time.Time) error {
	if f.blocked == nil {
		f.blocked = map[string]time.Time{}
	}
	f.blocked[accountID] = until
	return nil
}

func (f *fakePoolStore) DisableProviderAccount(ctx context.Context, accountID, cause string) error {
	if f.disabled == nil {
		f.disabled = map[string]string{}
	}
	f.disabled[accountID] = cause
	return nil
}

func (f *fakePoolStore) TouchProviderAccount(ctx context.Context, accountID string) error {
	f.touched = append(f.touched, accountID)
	return nil
}

func TestPoolAcquireFreshToken(t *testing.T) {
	store := &fakePoolStore{accounts: []storage.WorkspaceProviderAccountRecord{{
		ID: "a1", Vendor: ai.VendorClaudeCode, AccessToken: "live-token",
		AccountID: "acct-1", Email: "e@x.com",
		ExpiresAt: time.Now().Add(time.Hour),
	}}}
	pool := newPool(store, "prov1", testDescriptors("http://unused"), &http.Client{})
	tok, err := pool.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if tok.AccessToken != "live-token" || tok.AccountID != "a1" || tok.ProviderAccountID != "acct-1" {
		t.Errorf("token = %+v", tok)
	}
}

func TestPoolAcquireRefreshesExpired(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"access_token":"fresh-at","refresh_token":"fresh-rt","expires_in":3600}`))
	}))
	defer srv.Close()
	store := &fakePoolStore{accounts: []storage.WorkspaceProviderAccountRecord{{
		ID: "a1", Vendor: ai.VendorClaudeCode, AccessToken: "stale",
		RefreshToken: "rt", ExpiresAt: time.Now().Add(-time.Minute),
	}}}
	pool := newPool(store, "prov1", testDescriptors(srv.URL), srv.Client())
	tok, err := pool.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if tok.AccessToken != "fresh-at" {
		t.Errorf("access token = %q, want refreshed", tok.AccessToken)
	}
}

func TestPoolAcquireDisablesOnRefreshFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	store := &fakePoolStore{accounts: []storage.WorkspaceProviderAccountRecord{
		{ID: "bad", Vendor: ai.VendorClaudeCode, AccessToken: "x",
			RefreshToken: "rt", ExpiresAt: time.Now().Add(-time.Minute)},
		{ID: "good", Vendor: ai.VendorClaudeCode, AccessToken: "good-at",
			ExpiresAt: time.Now().Add(time.Hour)},
	}}
	pool := newPool(store, "prov1", testDescriptors(srv.URL), srv.Client())
	tok, err := pool.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if tok.AccessToken != "good-at" {
		t.Errorf("token = %q, want second account", tok.AccessToken)
	}
	if _, ok := store.disabled["bad"]; !ok {
		t.Error("failed account was not disabled")
	}
	if store.acquires != 2 {
		t.Errorf("acquires = %d, want 2", store.acquires)
	}
}

func TestPoolReport(t *testing.T) {
	ctx := context.Background()

	// nil error → no mark (the acquire already stamped last_used_at).
	store := &fakePoolStore{}
	pool := newPool(store, "p", testDescriptors("http://unused"), &http.Client{})
	pool.Report(ctx, ai.OAuthToken{AccountID: "a1"}, nil)
	if len(store.touched) != 0 {
		t.Errorf("touched = %v, want none", store.touched)
	}

	// 429 with Retry-After → blocked until now+RetryAfter.
	pool.Report(ctx, ai.OAuthToken{AccountID: "a2"},
		&agentcore.ProviderError{Status: http.StatusTooManyRequests, RetryAfter: 42 * time.Second})
	until, ok := store.blocked["a2"]
	if !ok {
		t.Fatal("a2 not blocked")
	}
	if d := time.Until(until); d < 40*time.Second || d > 44*time.Second {
		t.Errorf("blocked until = %v out", d)
	}

	// 429 without Retry-After → ~5min block.
	pool.Report(ctx, ai.OAuthToken{AccountID: "a3"},
		&agentcore.ProviderError{Status: http.StatusTooManyRequests})
	if d := time.Until(store.blocked["a3"]); d < 4*time.Minute || d > 6*time.Minute {
		t.Errorf("default block = %v", d)
	}

	// 401 with a working refresh → not disabled.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"access_token":"new","expires_in":3600}`))
	}))
	defer srv.Close()
	store2 := &fakePoolStore{accounts: []storage.WorkspaceProviderAccountRecord{{
		ID: "a4", Vendor: ai.VendorClaudeCode, RefreshToken: "rt",
	}}}
	pool2 := newPool(store2, "p", testDescriptors(srv.URL), srv.Client())
	pool2.Report(ctx, ai.OAuthToken{AccountID: "a4"},
		&agentcore.ProviderError{Status: http.StatusUnauthorized})
	if _, ok := store2.disabled["a4"]; ok {
		t.Error("a4 disabled despite successful refresh")
	}

	// 401 with a failing refresh → disabled.
	badSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer badSrv.Close()
	store3 := &fakePoolStore{accounts: []storage.WorkspaceProviderAccountRecord{{
		ID: "a5", Vendor: ai.VendorClaudeCode, RefreshToken: "rt",
	}}}
	pool3 := newPool(store3, "p", testDescriptors(badSrv.URL), badSrv.Client())
	pool3.Report(ctx, ai.OAuthToken{AccountID: "a5"},
		&agentcore.ProviderError{Status: http.StatusForbidden})
	if _, ok := store3.disabled["a5"]; !ok {
		t.Error("a5 not disabled after failed refresh")
	}
}
