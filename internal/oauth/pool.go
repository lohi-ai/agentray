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
}

// Pool is an ai.TokenSource over one provider row's account set. Acquire picks
// the next usable account (refreshing near-expired tokens first); Report feeds
// request outcomes back so rate-limited or dead accounts rotate out.
type Pool struct {
	store      poolStore
	providerID string
	refresh    *refresher
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
// manager's descriptors, HTTP client, and refresher are shared with the
// login/refresh paths so a test override applies everywhere — and so the
// refresher's singleflight actually dedupes: a per-Pool refresher would let
// two concurrent runs refresh the same account twice, and on a vendor that
// rotates refresh tokens the losing grant invalidates the winner's.
//
// A nil manager yields a TokenSource that errors on Acquire rather than
// panicking — tests and embeddings that mount the routes without a manager
// must not take the process down when a tier points at an OAuth provider.
func (m *Manager) Pool(providerID string) ai.TokenSource {
	if m == nil {
		return errTokenSource{err: errors.New("oauth: no manager configured")}
	}
	p := newPool(m.store, providerID, m.descriptors, m.client)
	p.refresh = m.refresh
	return p
}

// errTokenSource is the nil-manager TokenSource: every call fails with the
// same error so the run surfaces "not configured" instead of crashing.
type errTokenSource struct{ err error }

func (e errTokenSource) Acquire(context.Context) (ai.OAuthToken, error) {
	return ai.OAuthToken{}, e.err
}
func (e errTokenSource) Report(context.Context, ai.OAuthToken, error) {}

// Acquire returns the next usable account's token. When the account's access
// token is expired (or inside the 5-minute refresh window) it is refreshed
// first; a failed refresh disables the account and the acquire moves on to the
// next account. The loop is bounded by the pool's depth so every account gets
// a turn before Acquire gives up.
func (p *Pool) Acquire(ctx context.Context) (ai.OAuthToken, error) {
	var lastErr error
	for range maxAcquireAttempts {
		rec, err := p.store.AcquireProviderAccount(ctx, p.providerID)
		if err != nil {
			return ai.OAuthToken{}, err
		}
		if rec.RefreshToken == "" && !rec.ExpiresAt.IsZero() && !rec.ExpiresAt.After(time.Now()) {
			// Expired access token and nothing to refresh it with — serving it
			// is a guaranteed 401, so retire the account instead.
			_ = p.store.DisableProviderAccount(ctx, rec.ID, "access token expired; no refresh token")
			lastErr = errors.New("oauth: account access token expired with no refresh token")
			continue
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

// maxAcquireAttempts bounds the refresh-and-retry loop: an 8-account pool is
// already generous for one provider row, and a larger pool still terminates.
const maxAcquireAttempts = 8

// Report records the outcome of a request made with tok. The LRU cursor moved
// at Acquire (the pick stamps last_used_at), so Report only handles the marks
// an acquire cannot know: a 429 blocks the account until Retry-After (or 5
// minutes); a 401/403 tries one refresh and disables the account when that
// fails. Other failures need no mark — the account already spent its turn.
func (p *Pool) Report(ctx context.Context, tok ai.OAuthToken, err error) {
	if err == nil {
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
