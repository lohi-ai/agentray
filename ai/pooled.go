package ai

import (
	"context"
	"fmt"

	"github.com/lohi-ai/agentray/agentcore"
)

// oauthTokenApplier is the private seam between pooledProvider and the wire
// clients that can take a per-request OAuth credential. Each subscription
// provider (claude-code, openai-codex, google-antigravity) implements it; the
// pooled wrapper refuses to wrap anything else so a token can never be dropped
// silently and the request sent with a stale static key.
type oauthTokenApplier interface {
	applyOAuthToken(tok OAuthToken)
}

// pooledProvider is an agentcore.LLMProvider that draws a live OAuth access
// token from a TokenSource for every request instead of holding a static key.
// It exists because the subscription vendors authenticate a pool of accounts,
// not one credential: Acquire picks the next usable account (refreshing it when
// near expiry) and Report feeds the outcome back so a rate-limited or dead
// account rotates out.
//
// It deliberately does NOT implement agentcore.KeyUpdater: the run loop's
// per-turn key refresh would overwrite the freshly acquired token with the
// provider row's sentinel key (OAuthPoolKey), which is never a real credential.
type pooledProvider struct {
	vendor string
	inner  agentcore.LLMProvider
	src    TokenSource
}

// newPooledProvider wraps inner so each Chat/Stream call acquires an account
// token from src. Both arguments are required: a nil source has nothing to
// draw from, and an inner client that cannot apply a token would send the
// request unauthenticated.
func newPooledProvider(vendor string, inner agentcore.LLMProvider, src TokenSource) (*pooledProvider, error) {
	if src == nil {
		return nil, fmt.Errorf("ai: provider %q requires a TokenSource (OAuth account pool)", vendor)
	}
	if _, ok := inner.(oauthTokenApplier); !ok {
		return nil, fmt.Errorf("ai: provider %q wire client cannot apply OAuth tokens", vendor)
	}
	return &pooledProvider{vendor: vendor, inner: inner, src: src}, nil
}

// acquire pulls the next usable account token and installs it on the inner
// wire client. An empty pool or an exhausted pool surfaces as a provider error
// so the loop can escalate rather than retrying a request that has no
// credential at all.
func (p *pooledProvider) acquire(ctx context.Context) (OAuthToken, error) {
	tok, err := p.src.Acquire(ctx)
	if err != nil {
		return OAuthToken{}, agentcore.NewProviderError(p.vendor, nil, err.Error())
	}
	p.inner.(oauthTokenApplier).applyOAuthToken(tok)
	return tok, nil
}

func (p *pooledProvider) Name() string        { return p.vendor }
func (p *pooledProvider) SupportsTools() bool { return p.inner.SupportsTools() }

func (p *pooledProvider) Chat(ctx context.Context, req agentcore.ChatRequest) (agentcore.ChatResponse, error) {
	tok, err := p.acquire(ctx)
	if err != nil {
		return agentcore.ChatResponse{}, err
	}
	resp, callErr := p.inner.Chat(ctx, req)
	p.src.Report(ctx, tok, callErr)
	return resp, callErr
}

func (p *pooledProvider) Stream(ctx context.Context, req agentcore.ChatRequest) (<-chan agentcore.ChatDelta, error) {
	tok, err := p.acquire(ctx)
	if err != nil {
		return nil, err
	}
	ch, callErr := p.inner.Stream(ctx, req)
	p.src.Report(ctx, tok, callErr)
	if callErr != nil {
		return nil, callErr
	}
	return p.reportStream(ctx, tok, ch), nil
}

// reportStream wraps the inner delta channel so the account that served the
// request is marked when the stream fails mid-flight: the first delta carrying
// Err is reported (once — later deltas may repeat the same failure), and a
// clean close reports success so last_used_at advances. A caller-cancelled
// context is not the account's fault, so it reports nothing.
func (p *pooledProvider) reportStream(ctx context.Context, tok OAuthToken, ch <-chan agentcore.ChatDelta) <-chan agentcore.ChatDelta {
	out := make(chan agentcore.ChatDelta, 16)
	go func() {
		defer close(out)
		reported := false
		for d := range ch {
			if d.Err != nil && !reported {
				reported = true
				p.src.Report(ctx, tok, d.Err)
			}
			out <- d
		}
		if !reported && ctx.Err() == nil {
			p.src.Report(ctx, tok, nil)
		}
	}()
	return out
}
