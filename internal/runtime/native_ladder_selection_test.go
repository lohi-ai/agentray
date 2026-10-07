package agentruntime

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/2found/2ai/agentcore"
)

func testNativeLadder(t *testing.T) *nativeModelLadder {
	t.Helper()
	tier := ModelTier{TierConfig{Provider: "openai", ProviderID: "row-a", Model: "primary", BaseURL: "https://same.test/v1", APIKey: "secret-a", Fallback: &TierConfig{Provider: "openai", ProviderID: "row-b", Model: "fallback", BaseURL: "https://same.test/v1", APIKey: "secret-b", Capabilities: agentcore.ModelCapabilities{StatefulResponses: agentcore.CapabilitySupported}}}}
	ladder, err := newNativeModelLadder(tier, NativeAgentConfig{}, func(rung ModelTier) (PiModelOptions, error) {
		return PiModelOptions{RefreshKey: func(context.Context, string) (string, error) { return rung.APIKey + "-fresh", nil }}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return ladder
}
func TestNativeLadderSelectionCommitsBindingTogether(t *testing.T) {
	ladder := testNativeLadder(t)
	check := func(want string) {
		cfg, _, stream := ladder.binding()
		if stream == nil {
			t.Fatal("dispatcher missing")
		}
		raw, err := cfg.Callback(context.Background(), "getApiKey", json.RawMessage(`"openai"`), nil)
		if err != nil || string(raw) != want {
			t.Fatalf("wrong row credential: err=%v", err)
		}
		var config struct {
			InitialState struct{ Model json.RawMessage }
		}
		_ = json.Unmarshal(cfg.Options, &config)
		request, _ := json.Marshal(map[string]any{"model": config.InitialState.Model})
		if _, err = cfg.Callback(context.Background(), "prepareRequest", request, nil); err != nil {
			t.Fatal("callback/model binding drift", err)
		}
	}
	check(`"secret-a-fresh"`)
	var saved nativeLadderSelection
	err := ladder.selectRung(context.Background(), 0, 1, func(_ context.Context, selection nativeLadderSelection) error {
		saved = selection
		return errors.New("append failed")
	})
	if err == nil || ladder.selection().Rung != 0 {
		t.Fatal("failed durable write published selection")
	}
	check(`"secret-a-fresh"`)
	if err = ladder.selectRung(context.Background(), 0, 1, func(_ context.Context, selection nativeLadderSelection) error { saved = selection; return nil }); err != nil {
		t.Fatal(err)
	}
	check(`"secret-b-fresh"`)
	if saved.ProviderID != "row-b" || saved.Generation != 1 || saved.Rung != 1 {
		t.Fatal("durable identity missing")
	}
	raw, _ := json.Marshal(saved)
	if strings.Contains(string(raw), "secret-") {
		t.Fatal("credential entered durable selection")
	}
	restored := testNativeLadder(t)
	if err = restored.restore(saved); err != nil {
		t.Fatal(err)
	}
	cfg, _, _ := restored.binding()
	key, err := cfg.Callback(context.Background(), "getApiKey", json.RawMessage(`"openai"`), nil)
	if err != nil || string(key) != `"secret-b-fresh"` {
		t.Fatal("restore returned primary credential")
	}
	saved.Model[0] = '!'
	if !json.Valid(ladder.selection().Model) {
		t.Fatal("record aliases live binding")
	}
}
func TestNativeLadderSelectionStaleAndCancelledCommit(t *testing.T) {
	ladder := testNativeLadder(t)
	var commits atomic.Int32
	var successes atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if ladder.selectRung(context.Background(), 0, 1, func(context.Context, nativeLadderSelection) error { commits.Add(1); return nil }) == nil {
				successes.Add(1)
			}
		}()
	}
	wg.Wait()
	if commits.Load() != 1 || successes.Load() != 1 {
		t.Fatalf("duplicate selection commits=%d successes=%d", commits.Load(), successes.Load())
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := ladder.selectRung(ctx, 1, 0, func(context.Context, nativeLadderSelection) error {
		t.Error("cancelled selection persisted")
		return nil
	}); err == nil {
		t.Fatal("cancellation ignored")
	}
	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()
	if err := ladder.selectRung(ctx, 1, 0, func(context.Context, nativeLadderSelection) error { cancel(); return nil }); err != nil {
		t.Fatal(err)
	}
	if ladder.selection().Rung != 0 || ladder.selection().Generation != 2 {
		t.Fatal("successful durable commit lost in cancellation race")
	}
}
func TestNativeLadderRestoreRejectsChangedIdentity(t *testing.T) {
	ladder := testNativeLadder(t)
	base := ladder.selection()
	for _, change := range []func(*nativeLadderSelection){func(s *nativeLadderSelection) { s.ProviderID = "foreign" }, func(s *nativeLadderSelection) { s.Rung = 1 }, func(s *nativeLadderSelection) { s.Version = 2 }, func(s *nativeLadderSelection) {
		s.Model = json.RawMessage(`{"id":"primary","baseUrl":"https://foreign.test"}`)
	}} {
		invalid := base
		change(&invalid)
		if err := ladder.restore(invalid); err == nil {
			t.Fatal("accepted foreign durable binding")
		}
		if ladder.selection().Rung != 0 {
			t.Fatal("failed restore mutated selection")
		}
	}
}
func TestNativeLadderBindingFailsAsAWhole(t *testing.T) {
	tier := ModelTier{TierConfig{Provider: "openai", Model: "primary", Fallback: &TierConfig{Provider: "openai", Model: "fallback", BaseURL: "ftp://other.test"}}}
	ladder, err := newNativeModelLadder(tier, NativeAgentConfig{}, func(ModelTier) (PiModelOptions, error) { return PiModelOptions{}, nil })
	if err == nil || ladder != nil {
		t.Fatal("partially bound ladder escaped")
	}
}
