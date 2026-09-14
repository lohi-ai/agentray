package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	store "github.com/lohi-ai/agentray/internal/dataplane/store"
	"github.com/lohi-ai/agentray/internal/dataplane/usecase"
	"github.com/lohi-ai/agentray/internal/shared/opcore"
)

// board_parity_test.go — the declarative board surface on all four adapters.
//
// The point of the operation layer is that a capability is written once and
// behaves identically over REST, MCP, the CLI and the in-process agent tool.
// Board declaration is one transaction behind four transports, so the thing
// worth proving is not that it works (the store tests do that) but that the
// *class* of every outcome — declared, refused, conflicting, archived — is the
// same answer wherever the caller came from.

const boardDeclarationBody = `{
	"board_key":"parity-board",
	"name":"Parity board",
	"definition":{"version":1,"sections":[
		{"key":"overview","title":"Overview","tiles":[
			{"key":"people","metric":"active_users","display":"stat"},
			{"key":"trend","metric":"active_users_daily","display":"line","span":2}
		]}
	]}
}`

func TestBoardContentModelParityAcrossAdapters(t *testing.T) {
	t.Setenv("AGENT_KEY_ENC_SECRET", "board-parity-test-secret")
	s := openAppTestStore(t)
	ctx := context.Background()
	e := mountRealAdapters(t, s)
	srv := httptest.NewServer(e)
	t.Cleanup(srv.Close)

	scopes := []string{"analytics:read", "dashboards:write"}
	for _, adapter := range []string{"rest", "mcp", "cli", "runtime"} {
		t.Run(adapter, func(t *testing.T) {
			boot, err := s.CreateAccount(ctx, fmt.Sprintf("board-parity-%s-%d@test.local", adapter, time.Now().UnixNano()), "BP", "password-123", "ws", "proj")
			if err != nil {
				t.Fatalf("%s account: %v", adapter, err)
			}
			_, secret, err := s.CreateProjectCredential(ctx, boot.User.ID, boot.Project.ID, "parity", scopes)
			if err != nil {
				t.Fatalf("%s credential: %v", adapter, err)
			}
			deps := &usecase.Deps{Repo: s, Runner: storeRunner{s}, Audit: s}
			reg := usecase.Registry()
			var inv opInvoker
			switch adapter {
			case "rest":
				inv = restInvoker(e, secret)
			case "mcp":
				inv = mcpInvoker(e, secret)
			case "cli":
				inv = cliInvoker(srv.URL, secret)
			case "runtime":
				inv = runtimeInvoker(reg, opcore.CallContext{ProjectID: boot.Project.ID, Deps: deps})
			}

			// The catalog is the vocabulary a declaration is written against,
			// and it must arrive intact on every transport.
			catalog := expectOp(t, adapter, inv, "list_metrics", `{}`, "ok")
			var served struct {
				MetricVersion string                   `json:"metric_version"`
				Metrics       []store.MetricDefinition `json:"metrics"`
			}
			if err := json.Unmarshal(catalog.raw, &served); err != nil {
				t.Fatalf("%s list_metrics: %v (%s)", adapter, err, string(catalog.raw))
			}
			if served.MetricVersion != store.OverviewMetricVersion || len(served.Metrics) == 0 {
				t.Fatalf("%s catalog = %+v", adapter, served)
			}
			active := false
			for _, def := range served.Metrics {
				if def.Key == store.MetricActiveUsers {
					active = def.Kind == store.MetricKindValue && def.Unit == "people" && def.Definition != ""
				}
			}
			if !active {
				t.Fatalf("%s catalog does not carry active_users: %s", adapter, string(catalog.raw))
			}

			// A brand-new project reads its metric as a named empty state, not
			// as a zero — on every transport.
			reading := expectOp(t, adapter, inv, "read_metric", `{"metric":"active_users"}`, "ok")
			var read map[string]any
			if err := json.Unmarshal(reading.raw, &read); err != nil {
				t.Fatalf("%s read_metric: %v", adapter, err)
			}
			if read["state"] != store.OverviewStateNoData {
				t.Fatalf("%s read_metric state = %v, want no_data (%s)", adapter, read["state"], string(reading.raw))
			}
			if _, present := read["value"]; present {
				t.Fatalf("%s read_metric served a value with no events: %s", adapter, string(reading.raw))
			}

			// Declare the board in one call, then read it back.
			declared := expectOp(t, adapter, inv, "save_board", boardDeclarationBody, "ok")
			var content store.BoardContent
			if err := json.Unmarshal(declared.raw, &content); err != nil {
				t.Fatalf("%s save_board: %v (%s)", adapter, err, string(declared.raw))
			}
			if content.Board.BoardKey != "parity-board" || content.Board.Revision != 1 || !content.HasDefinition {
				t.Fatalf("%s declared board = %+v", adapter, content.Board)
			}
			if len(content.Metrics) != 2 || len(content.Warnings) != 0 {
				t.Fatalf("%s resolved = %d metrics, warnings %v", adapter, len(content.Metrics), content.Warnings)
			}
			back := expectOp(t, adapter, inv, "get_board", `{"board_key":"parity-board"}`, "ok")
			var reread store.BoardContent
			if err := json.Unmarshal(back.raw, &reread); err != nil {
				t.Fatalf("%s get_board: %v (%s)", adapter, err, string(back.raw))
			}
			if reread.Board.ID != content.Board.ID || !reflect.DeepEqual(reread.Definition, content.Definition) {
				t.Fatalf("%s get_board returned a different board:\n%s\n%s", adapter, string(back.raw), string(declared.raw))
			}

			// Refusals must classify the same way everywhere: a document the
			// catalog cannot satisfy is the author's to fix, an unfenced
			// re-declaration is a conflict, and a declaration onto an archived
			// board says so rather than looking like a failed edit.
			unknownMetric := strings.Replace(boardDeclarationBody, `"metric":"active_users"`, `"metric":"crashes"`, 1)
			expectOp(t, adapter, inv, "save_board", unknownMetric, "invalid")
			expectOp(t, adapter, inv, "save_board", boardDeclarationBody, "conflict")
			expectOp(t, adapter, inv, "save_board", `{"board_key":"unfenced"}`, "invalid")
			expectOp(t, adapter, inv, "read_metric", `{"metric":"crashes"}`, "invalid")

			expectOp(t, adapter, inv, "archive_dashboard",
				fmt.Sprintf(`{"dashboard_id":%q,"revision":1,"idempotency_key":"arch-1"}`, content.Board.ID), "ok")
			expectOp(t, adapter, inv, "save_board",
				fmt.Sprintf(`{"board_id":%q,"revision":2,"definition":{"version":1,"sections":[]}}`, content.Board.ID), "archived")
		})
	}
}
