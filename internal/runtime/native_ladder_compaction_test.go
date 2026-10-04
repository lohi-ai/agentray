package agentruntime

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/lohi-ai/agentray/agentcore"
	nativehost "github.com/lohi-ai/agentray/agentcore/host"
)

func TestNativeLadderCompactionUsesRequestWindow(t *testing.T) {
	// Bind each fixture independently so explicit context windows survive the
	// tier resolver's legacy fallback-window policy.
	ladder := &nativeModelLadder{}
	for i, window := range []int{100000, 2000} {
		tier := ModelTier{TierConfig: TierConfig{Provider: "openai", Model: []string{"large", "small"}[i], APIKey: "test", ContextWindow: window}}
		bound, err := newNativeModelLadder(tier, NativeAgentConfig{}, func(ModelTier) (PiModelOptions, error) { return PiModelOptions{}, nil })
		if err != nil {
			t.Fatal(err)
		}
		ladder.rungs = append(ladder.rungs, bound.rungs[0])
	}
	binding, _, stream := ladder.sessionBinding()
	summaries := 0
	policy := nativehost.CompactionPolicy{Budget: 90000, KeepRecent: 500, Summarize: func(_ context.Context, raw json.RawMessage, _ string) (string, agentcore.Usage, error) {
		summaries++
		if err := nativehost.ValidateMessages(raw); err != nil {
			t.Fatal("summary split native tool history", err)
		}
		return "saved evidence", agentcore.Usage{InputTokens: 7}, nil
	}}
	cfg := PiRunConfig{Compaction: &policy, Session: PiSessionConfig{Pi: binding, NativeStream: stream, nativeLadder: ladder}}
	projection := &piRunProjection{}
	initialize, err := bindPiRequestCompaction(&cfg, projection)
	if err != nil {
		t.Fatal(err)
	}
	session, err := NewPiSession(context.Background(), cfg.Session)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	if err = initialize(session); err != nil {
		t.Fatal(err)
	}
	raw := piRequestJSON(piLongRequest())
	check := func(ctx context.Context, input json.RawMessage, compact bool) {
		t.Helper()
		view, err := session.callback(ctx, "transformContext", input, nil)
		if err != nil {
			t.Fatal(err)
		}
		if nativehost.SameJSON(view, input) == compact || strings.Contains(string(view), "Earlier work summary") != compact {
			t.Fatalf("wrong request view for compact=%v", compact)
		}
	}
	check(session.ctx, raw, false)
	candidate, err := ladder.attemptContext(session.ctx, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	check(candidate, raw, true)
	if summaries != 1 || projection.result.Usage.InputTokens != 7 || ladder.selection().Rung != 0 {
		t.Fatal("candidate compaction changed selection or lost usage")
	}
	// An exact saved prefix is reusable even by the larger committed model.
	check(session.ctx, raw, true)
	// A different prefix must be evaluated using the large model again, rather
	// than inheriting the candidate's small limit or its prior summary.
	changed := json.RawMessage(strings.ReplaceAll(string(raw), "opaque", "changed"))
	check(session.ctx, changed, false)
	if summaries != 1 || policy.Budget != 90000 {
		t.Fatal("candidate permanently shrank host policy")
	}
	if err = session.selectNativeRung(session.ctx, 0, 1); err != nil {
		t.Fatal(err)
	}
	check(session.ctx, changed, true)
	if summaries != 2 || projection.result.Usage.InputTokens != 14 {
		t.Fatal("committed small model did not compact independently")
	}
	if _, err = session.callback(candidate, "transformContext", raw, nil); err == nil {
		t.Fatal("stale attempt compacted a request")
	}
	foreign, err := ladder.fork().attemptContext(session.ctx, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = session.callback(foreign, "transformContext", raw, nil); err == nil {
		t.Fatal("foreign attempt compacted a request")
	}
	if summaries != 2 {
		t.Fatal("invalid request paid for a summary")
	}
}
