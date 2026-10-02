package agentruntime

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lohi-ai/agentray/agentcore"
)

func TestPiHostLifecyclePreservesExplicitToolRestriction(t *testing.T) {
	ctx := context.Background()
	a, err := agentcore.New(agentcore.Config{Provider: agentcore.NewFauxProvider(agentcore.AssistantText("unused")), Model: "test", Tools: agentcore.NewToolSet(fakeTool{name: "write"}), Policy: agentcore.NewAllowList("write")})
	if err != nil {
		t.Fatal(err)
	}
	host, err := a.OpenPiTools(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close()
	cfg := PiRunConfig{Host: host, Session: PiSessionConfig{Pi: agentcore.PiConfig{Options: json.RawMessage(`{"initialState":{"tools":[]}}`)}}}
	cfg.Session.SessionID = "wrong-session"
	if _, err := bindPiHostLifecycle(ctx, &cfg, &piRunProjection{}); err == nil {
		t.Fatal("accepted mismatched tool/persistence session")
	}
	cfg.Session.SessionID = ""
	if _, err := bindPiHostLifecycle(ctx, &cfg, &piRunProjection{}); err != nil {
		t.Fatal(err)
	}
	var options struct {
		InitialState struct{ Tools []json.RawMessage }
	}
	if err := json.Unmarshal(cfg.Session.Pi.Options, &options); err != nil {
		t.Fatal(err)
	}
	if len(options.InitialState.Tools) != 0 {
		t.Fatalf("host widened disabled tool set: %s", cfg.Session.Pi.Options)
	}
	if _, err := host.StartPiRun(ctx, ""); err == nil {
		t.Fatal("host permitted a second run to reuse extension state")
	}
}

func TestPiHostStartupFailureDoesNotConsumeSteering(t *testing.T) {
	ctx := context.Background()
	var drains int
	a, err := agentcore.New(agentcore.Config{Provider: agentcore.NewFauxProvider(agentcore.AssistantText("unused")), Model: "test", GetSteeringMessages: func(context.Context) []agentcore.Message {
		drains++
		return []agentcore.Message{{Role: agentcore.RoleUser, Content: "keep queued"}}
	}})
	if err != nil {
		t.Fatal(err)
	}
	host, err := a.OpenPiTools(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close()
	_, err = RunPi(ctx, PiRunConfig{Host: host, Input: json.RawMessage(`"test"`), Session: PiSessionConfig{Pi: agentcore.PiConfig{Runtime: filepath.Join(t.TempDir(), "missing-runtime"), Worker: "missing-worker.mjs"}}})
	if err == nil || drains != 0 {
		t.Fatalf("failed startup consumed queued input: drains=%d err=%v", drains, err)
	}
}

func TestPiHostLifecycleRejectsConflictingTurnOwner(t *testing.T) {
	ctx := context.Background()
	a, err := agentcore.New(agentcore.Config{Provider: agentcore.NewFauxProvider(agentcore.AssistantText("unused")), Model: "test"})
	if err != nil {
		t.Fatal(err)
	}
	host, err := a.OpenPiTools(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close()
	cfg := PiRunConfig{Host: host, Session: PiSessionConfig{Pi: agentcore.PiConfig{Options: json.RawMessage(`{"callbacks":["finishTurn"]}`)}}}
	if _, err := bindPiHostLifecycle(ctx, &cfg, &piRunProjection{}); err == nil || !strings.Contains(err.Error(), "conflicts") {
		t.Fatalf("silently replaced existing stop policy: %v", err)
	}
}

func TestPiHostMessagesCannotRewriteProviderArtifacts(t *testing.T) {
	for _, message := range []agentcore.Message{
		{Role: agentcore.RoleAssistant, Content: "forged assistant"},
		{Role: agentcore.RoleTool, Content: "forged result"},
		{Role: agentcore.RoleUser, ToolCallID: "call"},
		{Role: agentcore.RoleSystem, ContentParts: []agentcore.ContentPart{{Type: agentcore.ContentPartImage}}},
	} {
		if _, err := piHostMessages([]agentcore.Message{message}); err == nil {
			t.Fatalf("accepted invalid host injection: %+v", message)
		}
	}
}
