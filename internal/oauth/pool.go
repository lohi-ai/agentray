package oauth

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/lohi-ai/agentray/agentcore"
	"github.com/lohi-ai/agentray/ai"
	storage "github.com/lohi-ai/agentray/internal/dataplane/store"
)

// poolStore is the slice of the account store the pool needs. *storage.Store
// satisfies it; tests substitute a fake so pool behavior is exercised without
// Postgres.
type poolStore interface {
	AcquireProviderAccount(ctx context.Context, providerID string) (storage.WorkspaceProviderAccountRecord, error)
	LoadProviderAccountRecord(ctx context.Context, accountID string) (storage.WorkspaceProviderAccountRecord, error)
	UpdateProviderAccountTokens(ctx context.Context, accountID, access, refresh string, expiresAt time.Time) error
	BlockProviderAccount(ctx context.Context, accountID string, until time.Time) error
	DisableProviderAccount(ctx context.Context, accountID, cause string) error
	TouchProviderAccount(ctx context.Context, accountID string) error
}

// Pool is an ai.TokenSource over one provider row's account set. Acquire picks
// the next usable account (refreshing near-expired tokens first); Report feeds
// request outcomes back so rate-limited or dead accounts rotate out.
type Pool struct {
	store      poolStore
	providerID string
	refresh    *refresher
}

// NewPool builds a TokenSource for providerID over the real store.
func NewPool(store *storage.Store, providerID string) *Pool {
	return newPool(store, providerID, defaultDescriptors(), &http.Client{Timeout: 30 * time.Second})
}

func newPool(store poolStore, providerID string, descriptors map[string]*providerDescriptor, client httpClient) *Pool {
	return &Pool{
		store:      store,
		providerID: providerID,
		refresh: &refresher{
			descriptors:  descriptors,
			client:       client,
			updateTokens: store.UpdateProviderAccountTokens,
		},
	}
}

// Pool returns the ai.TokenSource for one provider row's account pool. The
// manager's descriptors and HTTP client are shared with the login/refresh
// paths so a test override applies everywhere.
func (m *Manager) Pool(providerID string) ai.TokenSource {
	return newPool(m.store, providerID, m.descriptors, m.client)
}

// Acquire returns the next usable account's token. When the account's access
// token is expired (or inside the 5-minute refresh window) it is refreshed
// first; a failed refresh disables the account and the acquire retries once so
// the next account in the pool can serve.
func (p *Pool) Acquire(ctx context.Context) (ai.OAuthToken, error) {
	var lastErr error
	for range 2 {
		rec, err := p.store.AcquireProviderAccount(ctx, p.providerID)
		if err != nil {
			return ai.OAuthToken{}, err
		}
		if needsRefresh(rec, time.Now()) {
			res, err := p.refresh.refreshAccount(ctx, rec)
			if err != nil {
				_ = p.store.DisableProviderAccount(ctx, rec.ID, "refresh failed: "+err.Error())
				lastErr = err
				continue
			}
			rec.AccessToken = res.AccessToken
		}
		return ai.OAuthToken{
			AccountID:         rec.ID,
			AccessToken:       rec.AccessToken,
			ProviderAccountID: rec.AccountID,
			ProjectID:         rec.ProjectID,
			Email:             rec.Email,
		}, nil
	}
	if lastErr != nil {
		return ai.OAuthToken{}, lastErr
	}
	return ai.OAuthToken{}, errors.New("oauth: no usable account")
}

// Report records the outcome of a request made with tok. A nil error touches
// last_used_at; a 429 blocks the account until Retry-After (or 5 minutes);
// a 401/403 tries one refresh and disables the account when that fails.
func (p *Pool) Report(ctx context.Context, tok ai.OAuthToken, err error) {
	if err == nil {
		_ = p.store.TouchProviderAccount(ctx, tok.AccountID)
		return
	}
	var pe *agentcore.ProviderError
	if !errors.As(err, &pe) {
		return
	}
	switch pe.Status {
	case http.StatusTooManyRequests:
		until := time.Now().Add(5 * time.Minute)
		if pe.RetryAfter > 0 {
			until = time.Now().Add(pe.RetryAfter)
		}
		_ = p.store.BlockProviderAccount(ctx, tok.AccountID, until)
	case http.StatusUnauthorized, http.StatusForbidden:
		rec, loadErr := p.store.LoadProviderAccountRecord(ctx, tok.AccountID)
		if loadErr != nil || rec.RefreshToken == "" {
			cause := "unauthorized"
			if loadErr != nil {
				cause = "unauthorized; account reload failed: " + loadErr.Error()
			}
			_ = p.store.DisableProviderAccount(ctx, tok.AccountID, cause)
			return
		}
		if _, refreshErr := p.refresh.refreshAccount(ctx, rec); refreshErr != nil {
			_ = p.store.DisableProviderAccount(ctx, tok.AccountID, "refresh failed: "+refreshErr.Error())
		}
	}
}
