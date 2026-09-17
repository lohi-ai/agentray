package oauth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/sync/singleflight"

	storage "github.com/lohi-ai/agentray/internal/dataplane/store"
)

// refresher posts a vendor refresh grant and persists the rotated tokens.
// Singleflight per account id so concurrent Acquires on an expired account
// share one refresh round-trip.
type refresher struct {
	descriptors  map[string]*providerDescriptor
	client       httpClient
	updateTokens func(ctx context.Context, accountID, access, refresh string, expiresAt time.Time) error
	sf           singleflight.Group
}

// refreshAccount performs the refresh once (singleflighted) and updates the
// store. Returns the fresh access token + expiry.
func (r *refresher) refreshAccount(ctx context.Context, rec storage.WorkspaceProviderAccountRecord) (TokenResult, error) {
	v, err, _ := r.sf.Do(rec.ID, func() (any, error) {
		return r.doRefresh(ctx, rec)
	})
	if err != nil {
		return TokenResult{}, err
	}
	return v.(TokenResult), nil
}

func (r *refresher) doRefresh(ctx context.Context, rec storage.WorkspaceProviderAccountRecord) (TokenResult, error) {
	d, ok := r.descriptors[normalizeVendor(rec.Vendor)]
	if !ok {
		return TokenResult{}, &Error{Kind: "validation", Message: "unsupported OAuth vendor: " + rec.Vendor}
	}
	if rec.RefreshToken == "" {
		return TokenResult{}, &Error{Kind: "validation", Message: "account has no refresh token"}
	}

	var body io.Reader
	if d.tokenBody == tokenBodyJSON {
		raw, err := json.Marshal(map[string]string{
			"grant_type":    "refresh_token",
			"refresh_token": rec.RefreshToken,
			"client_id":     d.clientID,
		})
		if err != nil {
			return TokenResult{}, err
		}
		body = strings.NewReader(string(raw))
	} else {
		form := url.Values{
			"grant_type":    {"refresh_token"},
			"refresh_token": {rec.RefreshToken},
			"client_id":     {d.clientID},
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
	for k, v := range d.refreshHeaders {
		req.Header.Set(k, v)
	}
	if d.tokenTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, d.tokenTimeout)
		defer cancel()
		req = req.WithContext(ctx)
	}

	resp, err := r.client.Do(req)
	if err != nil {
		return TokenResult{}, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return TokenResult{}, err
	}
	if resp.StatusCode/100 != 2 {
		return TokenResult{}, &Error{Kind: "token-exchange", Status: resp.StatusCode,
			Message: fmt.Sprintf("token refresh failed: %d %s", resp.StatusCode, truncate(string(raw), 300))}
	}
	var tr tokenResponse
	if err := json.Unmarshal(raw, &tr); err != nil {
		return TokenResult{}, &Error{Kind: "validation", Message: "refresh response was not JSON"}
	}
	if tr.AccessToken == "" {
		return TokenResult{}, &Error{Kind: "validation", Message: "refresh response missing access_token"}
	}

	res := TokenResult{
		AccessToken:  tr.AccessToken,
		RefreshToken: tr.RefreshToken,
		IDToken:      tr.IDToken,
		ExpiresAt:    time.Now().Add(time.Duration(tr.ExpiresIn)*time.Second - d.expiresSkew),
	}
	// Vendors that rotate the refresh token send a new one; keep the stored
	// token when the response omits it.
	if res.RefreshToken == "" {
		res.RefreshToken = rec.RefreshToken
	}
	if err := r.updateTokens(ctx, rec.ID, res.AccessToken, res.RefreshToken, res.ExpiresAt); err != nil {
		return TokenResult{}, err
	}
	return res, nil
}

// RefreshAccount refreshes one stored account's tokens and persists them.
// Called by the pool on near-expiry and by Report on 401/403.
func (m *Manager) RefreshAccount(ctx context.Context, rec storage.WorkspaceProviderAccountRecord) (TokenResult, error) {
	return m.refresh.refreshAccount(ctx, rec)
}

// needsRefresh reports whether the account's access token is expired or within
// the 5-minute refresh window. A zero ExpiresAt (never recorded) is treated as
// needing refresh when a refresh token exists.
func needsRefresh(rec storage.WorkspaceProviderAccountRecord, now time.Time) bool {
	if rec.RefreshToken == "" {
		return false
	}
	if rec.ExpiresAt.IsZero() {
		return true
	}
	return !rec.ExpiresAt.After(now.Add(5 * time.Minute))
}
