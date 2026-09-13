package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/labstack/echo/v4"
	"github.com/lohi-ai/agentray/agentcore"
	storage "github.com/lohi-ai/agentray/internal/dataplane/store"
	"github.com/lohi-ai/agentray/internal/dataplane/usecase"
	"github.com/lohi-ai/agentray/internal/shared/opcore"
)

// Adapter parity: the real registry behind the real MCP + /api/op mounts,
// authorized by the real principal resolver. Proves the credential contract
// end to end — capture keys denied everywhere, management scopes enforced per
// operation, and both adapters answer identically.

// mountRealAdapters wires the production registry + resolver onto a test echo.
// Deps carries the store-backed runner so run_source/cancel_source_run reach
// the same enqueue fencing the engine's adapter implements in production.
func mountRealAdapters(t *testing.T, s *storage.Store) *echo.Echo {
	t.Helper()
	e := echo.New()
	deps := &usecase.Deps{Repo: s, Runner: storeRunner{s}, Audit: s}
	reg := usecase.Registry()
	resolve := func(c echo.Context) (opcore.Principal, error) {
		return principalFromRequest(c, s)
	}
	opcore.MountMCP(e.Group("/mcp"), reg, deps, resolve)
	opcore.MountHTTP(e.Group("/api/op"), reg, deps, resolve)
	return e
}

func postJSON(t *testing.T, e *echo.Echo, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	return rec
}

func mcpCall(name, args string) string {
	return fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":%q,"arguments":%s}}`, name, args)
}

func TestAdaptersEnforceCredentialContract(t *testing.T) {
	s := openAppTestStore(t)
	ctx := context.Background()
	e := mountRealAdapters(t, s)

	boot, err := s.CreateAccount(ctx, fmt.Sprintf("parity-%d@test.local", time.Now().UnixNano()), "P", "password-123", "ws", "proj")
	if err != nil {
		t.Fatalf("account: %v", err)
	}
	project := boot.Project // born-split: its api key is capture-only

	// Capture key: denied on MCP and /api/op for a read op AND a write op.
	for _, op := range []string{"activity_summary", "verify_sdk", "update_dashboard", "run_source"} {
		rec := postJSON(t, e, "/mcp", mcpCall(op, `{}`), map[string]string{"X-API-Key": project.APIKey})
		if !strings.Contains(rec.Body.String(), "isError\":true") {
			t.Fatalf("MCP %s with capture key: %s", op, rec.Body.String())
		}
		rec = postJSON(t, e, "/api/op/"+op, `{}`, map[string]string{"X-API-Key": project.APIKey})
		if rec.Code == http.StatusOK {
			t.Fatalf("/api/op %s with capture key: %d %s", op, rec.Code, rec.Body.String())
		}
	}

	// analytics:read management key: reads pass, writes denied.
	_, readSecret, err := s.CreateProjectCredential(ctx, boot.User.ID, project.ID, "reader", []string{"analytics:read"})
	if err != nil {
		t.Fatalf("read cred: %v", err)
	}
	rec := postJSON(t, e, "/api/op/activity_summary", `{"hours":1}`, map[string]string{"Authorization": "Bearer " + readSecret})
	if rec.Code != http.StatusOK {
		t.Fatalf("analytics:read activity_summary: %d %s", rec.Code, rec.Body.String())
	}
	rec = postJSON(t, e, "/api/op/verify_sdk", `{"event_name":"onboarding_verified"}`, map[string]string{"Authorization": "Bearer " + readSecret})
	if rec.Code != http.StatusOK {
		t.Fatalf("analytics:read verify_sdk: %d %s", rec.Code, rec.Body.String())
	}
	rec = postJSON(t, e, "/api/op/update_dashboard", `{"dashboard_id":"x","revision":1}`, map[string]string{"Authorization": "Bearer " + readSecret})
	if rec.Code == http.StatusOK {
		t.Fatalf("analytics:read update_dashboard allowed: %s", rec.Body.String())
	}
	// sources:read may probe/status but not run.
	_, srcSecret, err := s.CreateProjectCredential(ctx, boot.User.ID, project.ID, "src", []string{"sources:read"})
	if err != nil {
		t.Fatalf("src cred: %v", err)
	}
	// sources:read may probe/status but not run. The connector id does not
	// exist, so the answer is the typed not-found — 404 proves the credential
	// was authorized (a refusal would be 403) AND that an unknown connector is
	// no longer an empty success.
	rec = postJSON(t, e, "/api/op/source_status", `{"connector_id":"00000000-0000-0000-0000-000000000000"}`, map[string]string{"Authorization": "Bearer " + srcSecret})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("sources:read source_status: %d %s", rec.Code, rec.Body.String())
	}
	rec = postJSON(t, e, "/api/op/run_source", `{"sync_id":"00000000-0000-0000-0000-000000000000"}`, map[string]string{"Authorization": "Bearer " + srcSecret})
	if rec.Code == http.StatusOK {
		t.Fatalf("sources:read run_source allowed: %s", rec.Body.String())
	}

	// MCP tools/list advertises only what the credential may call.
	rec = postJSON(t, e, "/mcp", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, map[string]string{"Authorization": "Bearer " + srcSecret})
	body := rec.Body.String()
	if !strings.Contains(body, "source_status") || strings.Contains(body, "run_source") {
		t.Fatalf("tools/list for sources:read leaked manage ops: %s", body)
	}
	rec = postJSON(t, e, "/mcp", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, map[string]string{"X-API-Key": project.APIKey})
	if strings.Contains(rec.Body.String(), `"name"`) {
		t.Fatalf("tools/list for capture key advertised tools: %s", rec.Body.String())
	}
}

// --- Lifecycle parity: one case matrix, every adapter ---

// opOutcome is the adapter-neutral view of one operation call: the typed class
// (ok/conflict/not_found/archived/denied/invalid) plus the raw result payload
// for receipt comparison. Adapters surface the same outcome differently —
// HTTP status, MCP isError, a CLI transport error, an in-process tool error —
// so parity is asserted on the class and the receipt, never on the transport
// framing.
type opOutcome struct {
	class string
	raw   json.RawMessage
}

// opInvoker invokes one registered operation with raw JSON args and reports
// the adapter-neutral outcome. One invoker is bound to one credential/project.
type opInvoker func(t *testing.T, name, args string) opOutcome

// classifyOpError maps the operation layer's typed errors onto the class every
// adapter must agree on. The message text is the contract: handlers return
// sentinel errors (revision conflict, idempotency key reuse, archived source)
// or fixed "not found" strings, and each adapter carries that text to its
// caller unchanged.
func classifyOpError(msg string) string {
	switch {
	case strings.Contains(msg, "revision conflict"),
		strings.Contains(msg, "idempotency key"):
		return "conflict"
	case strings.Contains(msg, "not found"),
		strings.Contains(msg, "no rows in result set"):
		return "not_found"
	case strings.Contains(msg, "archived"):
		return "archived"
	case strings.Contains(msg, "may not invoke"),
		strings.Contains(msg, "authentication failed"),
		strings.Contains(msg, "invalid credential"),
		strings.Contains(msg, "invalid api key"):
		return "denied"
	default:
		return "invalid"
	}
}

// restInvoker drives POST /api/op/<name> — the same surface the CLI transport
// calls, exercised directly so its status/envelope is observed.
func restInvoker(e *echo.Echo, secret string) opInvoker {
	return func(t *testing.T, name, args string) opOutcome {
		t.Helper()
		rec := postJSON(t, e, "/api/op/"+name, args, map[string]string{"Authorization": "Bearer " + secret})
		if rec.Code == http.StatusOK {
			return opOutcome{class: "ok", raw: rec.Body.Bytes()}
		}
		// A handler failure arrives as the typed envelope {error, code}; an
		// admission failure (auth, authorize) is still echo's {"message": …}.
		// Read both — the class vocabulary below distinguishes archived from
		// conflict, which the wire `code` alone collapses.
		var env struct {
			Error   string `json:"error"`
			Message string `json:"message"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &env)
		msg := env.Error
		if msg == "" {
			msg = env.Message
		}
		return opOutcome{class: classifyOpError(msg)}
	}
}

// mcpInvoker drives the JSON-RPC tools/call endpoint an external MCP client
// uses. Handler failures arrive as isError results whose content text is the
// operation's error message.
func mcpInvoker(e *echo.Echo, secret string) opInvoker {
	return func(t *testing.T, name, args string) opOutcome {
		t.Helper()
		rec := postJSON(t, e, "/mcp", mcpCall(name, args), map[string]string{"Authorization": "Bearer " + secret})
		var resp struct {
			Result struct {
				IsError           bool            `json:"isError"`
				StructuredContent json.RawMessage `json:"structuredContent"`
				Content           []struct {
					Text string `json:"text"`
				} `json:"content"`
			} `json:"result"`
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("mcp %s: response not JSON-RPC: %s", name, rec.Body.String())
		}
		if resp.Error != nil {
			return opOutcome{class: classifyOpError(resp.Error.Message)}
		}
		if resp.Result.IsError {
			msg := ""
			if len(resp.Result.Content) > 0 {
				msg = resp.Result.Content[0].Text
			}
			return opOutcome{class: classifyOpError(msg)}
		}
		raw := resp.Result.StructuredContent
		if len(raw) == 0 && len(resp.Result.Content) > 0 {
			raw = json.RawMessage(resp.Result.Content[0].Text)
		}
		return opOutcome{class: "ok", raw: raw}
	}
}

// cliInvoker drives the real CLI transport — opcore.NewClient over HTTP — the
// same code `agentray <op> '{...}'` runs.
func cliInvoker(baseURL, secret string) opInvoker {
	client := opcore.NewClient(baseURL, secret)
	return func(t *testing.T, name, args string) opOutcome {
		t.Helper()
		out, err := client.Call(context.Background(), name, []byte(args))
		if err != nil {
			return opOutcome{class: classifyOpError(err.Error())}
		}
		return opOutcome{class: "ok", raw: out}
	}
}

// runtimeInvoker drives the in-process agent adapter: opcore.Tools projects
// the registry into agentcore tools bound to one CallContext, exactly what
// buildToolsAndHooks hands the agent loop.
func runtimeInvoker(reg *opcore.Registry, cc opcore.CallContext) opInvoker {
	tools := map[string]agentcore.Tool{}
	for _, tool := range opcore.Tools(reg, cc) {
		tools[tool.Name()] = tool
	}
	return func(t *testing.T, name, args string) opOutcome {
		t.Helper()
		tool, ok := tools[name]
		if !ok {
			t.Fatalf("runtime adapter: operation %q not projected as a tool", name)
		}
		out, err := tool.Run(context.Background(), args)
		if err != nil {
			return opOutcome{class: classifyOpError(err.Error())}
		}
		return opOutcome{class: "ok", raw: json.RawMessage(out)}
	}
}

func field(t *testing.T, o opOutcome, key string) any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(o.raw, &m); err != nil {
		t.Fatalf("result not a JSON object: %s", string(o.raw))
	}
	return m[key]
}

func numField(t *testing.T, o opOutcome, key string) float64 {
	t.Helper()
	v, _ := field(t, o, key).(float64)
	return v
}

// expectOp asserts one case produced the same typed class on this adapter and
// returns the outcome for receipt checks.
func expectOp(t *testing.T, adapter string, inv opInvoker, name, args, want string) opOutcome {
	t.Helper()
	got := inv(t, name, args)
	if got.class != want {
		t.Fatalf("%s %s(%s): class %q, want %q", adapter, name, args, got.class, want)
	}
	return got
}

// dashboardJourney drives the full dashboard+chart lifecycle matrix through
// one adapter: happy path, stale revision, same-key replay, different-payload
// conflict, missing id, foreign id, missing required field, and reversible
// archive/restore. foreign invokes under a second project's credential.
func dashboardJourney(t *testing.T, adapter string, inv, foreign opInvoker) {
	t.Helper()

	dash := expectOp(t, adapter, inv, "create_dashboard", `{"name":"Parity board","description":"d"}`, "ok")
	dashID, _ := field(t, dash, "id").(string)
	if dashID == "" || numField(t, dash, "revision") != 1 {
		t.Fatalf("%s create_dashboard = %s", adapter, string(dash.raw))
	}

	upd := expectOp(t, adapter, inv, "update_dashboard",
		fmt.Sprintf(`{"dashboard_id":%q,"name":"Parity 2","description":"d2","revision":1,"idempotency_key":"%s-u1"}`, dashID, adapter), "ok")
	if numField(t, upd, "revision") != 2 {
		t.Fatalf("%s update_dashboard = %s", adapter, string(upd.raw))
	}

	// Same key + same payload replays the first receipt — the whole stored
	// result, not just id/revision, and no second bump.
	replay := expectOp(t, adapter, inv, "update_dashboard",
		fmt.Sprintf(`{"dashboard_id":%q,"name":"Parity 2","description":"d2","revision":1,"idempotency_key":"%s-u1"}`, dashID, adapter), "ok")
	var wantReceipt, gotReceipt map[string]any
	if err := json.Unmarshal(upd.raw, &wantReceipt); err != nil {
		t.Fatalf("%s first receipt not an object: %s", adapter, string(upd.raw))
	}
	if err := json.Unmarshal(replay.raw, &gotReceipt); err != nil {
		t.Fatalf("%s replay receipt not an object: %s", adapter, string(replay.raw))
	}
	if !reflect.DeepEqual(wantReceipt, gotReceipt) {
		t.Fatalf("%s replay diverged: %s vs %s", adapter, string(replay.raw), string(upd.raw))
	}

	// Same key + different payload conflicts.
	expectOp(t, adapter, inv, "update_dashboard",
		fmt.Sprintf(`{"dashboard_id":%q,"name":"Other","description":"x","revision":2,"idempotency_key":"%s-u1"}`, dashID, adapter), "conflict")

	// Stale revision conflicts.
	expectOp(t, adapter, inv, "update_dashboard",
		fmt.Sprintf(`{"dashboard_id":%q,"name":"Stale","description":"x","revision":1}`, dashID), "conflict")

	// Missing id and foreign id are both not-found — the project boundary
	// answers identically to a row that does not exist.
	expectOp(t, adapter, inv, "update_dashboard",
		`{"dashboard_id":"00000000-0000-0000-0000-000000000000","name":"x","description":"y","revision":1}`, "not_found")
	expectOp(t, adapter, foreign, "update_dashboard",
		fmt.Sprintf(`{"dashboard_id":%q,"name":"x","description":"y","revision":2}`, dashID), "not_found")

	// Missing required field is a uniform validation outcome.
	expectOp(t, adapter, inv, "update_dashboard", `{"name":"no-id"}`, "invalid")

	// Chart lifecycle under its own revision fence.
	chart := expectOp(t, adapter, inv, "create_chart",
		fmt.Sprintf(`{"dashboard_id":%q,"name":"c","metric":"events"}`, dashID), "ok")
	chartID, _ := field(t, chart, "id").(string)
	if chartID == "" {
		t.Fatalf("%s create_chart = %s", adapter, string(chart.raw))
	}
	expectOp(t, adapter, inv, "archive_chart",
		fmt.Sprintf(`{"chart_id":%q,"revision":1,"idempotency_key":"%s-ac"}`, chartID, adapter), "ok")
	listed := expectOp(t, adapter, inv, "list_charts", fmt.Sprintf(`{"dashboard_id":%q}`, dashID), "ok")
	if charts, _ := field(t, listed, "charts").([]any); len(charts) != 0 {
		t.Fatalf("%s archived chart still listed: %s", adapter, string(listed.raw))
	}
	un := expectOp(t, adapter, inv, "unarchive_chart",
		fmt.Sprintf(`{"chart_id":%q,"revision":2,"idempotency_key":"%s-uc"}`, chartID, adapter), "ok")
	if field(t, un, "archived_at") != nil {
		t.Fatalf("%s unarchive_chart = %s", adapter, string(un.raw))
	}
	expectOp(t, adapter, foreign, "archive_chart",
		fmt.Sprintf(`{"chart_id":%q,"revision":3}`, chartID), "not_found")

	// Dashboard archive/restore: leaves the active list, then returns.
	expectOp(t, adapter, inv, "archive_dashboard",
		fmt.Sprintf(`{"dashboard_id":%q,"revision":2,"idempotency_key":"%s-ad"}`, dashID, adapter), "ok")
	boards := expectOp(t, adapter, inv, "list_dashboards", `{}`, "ok")
	for _, d := range mustList(t, boards, "dashboards") {
		if m, _ := d.(map[string]any); m["id"] == dashID {
			t.Fatalf("%s archived dashboard still listed: %s", adapter, string(boards.raw))
		}
	}
	restored := expectOp(t, adapter, inv, "unarchive_dashboard",
		fmt.Sprintf(`{"dashboard_id":%q,"revision":3,"idempotency_key":"%s-ud"}`, dashID, adapter), "ok")
	if field(t, restored, "archived_at") != nil {
		t.Fatalf("%s unarchive_dashboard = %s", adapter, string(restored.raw))
	}
}

func mustList(t *testing.T, o opOutcome, key string) []any {
	t.Helper()
	l, _ := field(t, o, key).([]any)
	return l
}

// sourceJourney drives the connector lifecycle matrix through one adapter:
// create with replay, archive → sync paused + run admission fenced →
// unarchive → sync resumed, plus stale/foreign/not-found cases.
func sourceJourney(t *testing.T, adapter string, s *storage.Store, userID, projectID string, inv, foreign opInvoker) {
	t.Helper()
	ctx := context.Background()

	cred, err := s.CreateSourceCredential(ctx, userID, projectID, "parity-pg-"+adapter, "postgres://u:p@h:5432/db")
	if err != nil {
		t.Fatalf("%s credential: %v", adapter, err)
	}

	src := expectOp(t, adapter, inv, "create_source",
		fmt.Sprintf(`{"name":"warehouse","kind":"postgres","credential_id":%q,"idempotency_key":"%s-cs"}`, cred.ID, adapter), "ok")
	srcID, _ := field(t, src, "id").(string)
	if srcID == "" {
		t.Fatalf("%s create_source = %s", adapter, string(src.raw))
	}
	// Same key + same payload replays the receipt — no second connector.
	replay := expectOp(t, adapter, inv, "create_source",
		fmt.Sprintf(`{"name":"warehouse","kind":"postgres","credential_id":%q,"idempotency_key":"%s-cs"}`, cred.ID, adapter), "ok")
	if field(t, replay, "id") != srcID {
		t.Fatalf("%s create_source replay minted a second connector: %s", adapter, string(replay.raw))
	}
	// Same key + different payload conflicts.
	expectOp(t, adapter, inv, "create_source",
		fmt.Sprintf(`{"name":"other","kind":"postgres","credential_id":%q,"idempotency_key":"%s-cs"}`, cred.ID, adapter), "conflict")

	syncRow, err := s.CreateConnectorSync(ctx, userID, projectID, srcID, storage.ConnectorSyncInput{
		SourceTable: "orders", KeyColumn: "id", ScheduleCron: "*/5 * * * *", Enabled: true,
	})
	if err != nil {
		t.Fatalf("%s seed sync: %v", adapter, err)
	}

	// Archive pauses the sync transactionally.
	expectOp(t, adapter, inv, "archive_source",
		fmt.Sprintf(`{"connector_id":%q,"revision":1,"idempotency_key":"%s-as"}`, srcID, adapter), "ok")
	st := expectOp(t, adapter, inv, "source_status", fmt.Sprintf(`{"sync_id":%q}`, syncRow.ID), "ok")
	syncs, _ := field(t, st, "syncs").([]any)
	if len(syncs) != 1 {
		t.Fatalf("%s source_status = %s", adapter, string(st.raw))
	}
	syncMap, _ := syncs[0].(map[string]any)["sync"].(map[string]any)
	if syncMap["enabled"] != false || syncMap["disabled_by_archive"] != true {
		t.Fatalf("%s sync after archive = %v", adapter, syncMap)
	}

	// Admission fencing: run-now against the archived source fails closed on
	// every adapter, and resume-under-archive is rejected.
	expectOp(t, adapter, inv, "run_source", fmt.Sprintf(`{"sync_id":%q}`, syncRow.ID), "archived")
	expectOp(t, adapter, inv, "pause_source",
		fmt.Sprintf(`{"sync_id":%q,"paused":false,"revision":%v}`, syncRow.ID, syncMap["revision"]), "archived")

	// Restore resumes exactly the sync the archive paused.
	expectOp(t, adapter, inv, "unarchive_source",
		fmt.Sprintf(`{"connector_id":%q,"revision":2,"idempotency_key":"%s-us"}`, srcID, adapter), "ok")
	st = expectOp(t, adapter, inv, "source_status", fmt.Sprintf(`{"sync_id":%q}`, syncRow.ID), "ok")
	syncs, _ = field(t, st, "syncs").([]any)
	syncMap, _ = syncs[0].(map[string]any)["sync"].(map[string]any)
	if syncMap["enabled"] != true || syncMap["disabled_by_archive"] != false {
		t.Fatalf("%s sync after unarchive = %v", adapter, syncMap)
	}

	// Stale revision, missing id, foreign id.
	expectOp(t, adapter, inv, "archive_source",
		fmt.Sprintf(`{"connector_id":%q,"revision":1}`, srcID), "conflict")
	expectOp(t, adapter, inv, "archive_source",
		`{"connector_id":"00000000-0000-0000-0000-000000000000","revision":1}`, "not_found")
	expectOp(t, adapter, foreign, "archive_source",
		fmt.Sprintf(`{"connector_id":%q,"revision":3}`, srcID), "not_found")
}

// TestLifecycleParityAcrossAdapters proves the lifecycle contract is identical
// on every adapter over the shared registry: REST /api/op, MCP tools/call, the
// CLI transport (opcore.Client), and the in-process runtime tool set. Each
// adapter runs the same case matrix — happy path, stale revision, same-key
// replay, different-payload conflict, missing and foreign ids, reversible
// archive/restore, and archived-source run fencing — and must produce the same
// typed outcome class and the same receipts. Needs the compose Postgres +
// DuckDB; skips without them.
func TestLifecycleParityAcrossAdapters(t *testing.T) {
	t.Setenv("AGENT_KEY_ENC_SECRET", "lifecycle-parity-test-secret")
	s := openAppTestStore(t)
	ctx := context.Background()
	e := mountRealAdapters(t, s)
	srv := httptest.NewServer(e)
	t.Cleanup(srv.Close)

	scopes := []string{"analytics:read", "dashboards:write", "sources:read", "sources:manage"}
	newAdapterSet := func(t *testing.T, tag string) (inv, foreign opInvoker, userID, projectID string) {
		t.Helper()
		boot, err := s.CreateAccount(ctx, fmt.Sprintf("parity-%s-%d@test.local", tag, time.Now().UnixNano()), "P", "password-123", "ws", "proj")
		if err != nil {
			t.Fatalf("%s account: %v", tag, err)
		}
		other, err := s.CreateAccount(ctx, fmt.Sprintf("parity-%s-b-%d@test.local", tag, time.Now().UnixNano()), "PB", "password-123", "ws-b", "proj-b")
		if err != nil {
			t.Fatalf("%s foreign account: %v", tag, err)
		}
		_, secret, err := s.CreateProjectCredential(ctx, boot.User.ID, boot.Project.ID, "parity", scopes)
		if err != nil {
			t.Fatalf("%s credential: %v", tag, err)
		}
		_, foreignSecret, err := s.CreateProjectCredential(ctx, other.User.ID, other.Project.ID, "parity", scopes)
		if err != nil {
			t.Fatalf("%s foreign credential: %v", tag, err)
		}
		deps := &usecase.Deps{Repo: s, Runner: storeRunner{s}}
		reg := usecase.Registry()
		switch tag {
		case "rest":
			inv = restInvoker(e, secret)
			foreign = restInvoker(e, foreignSecret)
		case "mcp":
			inv = mcpInvoker(e, secret)
			foreign = mcpInvoker(e, foreignSecret)
		case "cli":
			inv = cliInvoker(srv.URL, secret)
			foreign = cliInvoker(srv.URL, foreignSecret)
		case "runtime":
			inv = runtimeInvoker(reg, opcore.CallContext{ProjectID: boot.Project.ID, Deps: deps})
			foreign = runtimeInvoker(reg, opcore.CallContext{ProjectID: other.Project.ID, Deps: deps})
		default:
			t.Fatalf("unknown adapter %q", tag)
		}
		return inv, foreign, boot.User.ID, boot.Project.ID
	}

	for _, adapter := range []string{"rest", "mcp", "cli", "runtime"} {
		t.Run(adapter, func(t *testing.T) {
			inv, foreign, userID, projectID := newAdapterSet(t, adapter)
			dashboardJourney(t, adapter, inv, foreign)
			sourceJourney(t, adapter, s, userID, projectID, inv, foreign)
		})
	}

	// Cross-adapter receipt: a key claimed through one adapter replays its
	// stored result through another — the receipt lives in the shared store,
	// not in any adapter.
	boot, err := s.CreateAccount(ctx, fmt.Sprintf("parity-x-%d@test.local", time.Now().UnixNano()), "PX", "password-123", "ws-x", "proj-x")
	if err != nil {
		t.Fatalf("cross account: %v", err)
	}
	_, secret, err := s.CreateProjectCredential(ctx, boot.User.ID, boot.Project.ID, "parity", scopes)
	if err != nil {
		t.Fatalf("cross credential: %v", err)
	}
	dash := expectOp(t, "rest", restInvoker(e, secret), "create_dashboard", `{"name":"Cross","description":"x"}`, "ok")
	dashID, _ := field(t, dash, "id").(string)
	args := fmt.Sprintf(`{"dashboard_id":%q,"name":"Cross 2","description":"y","revision":1,"idempotency_key":"cross-1"}`, dashID)
	first := expectOp(t, "rest", restInvoker(e, secret), "update_dashboard", args, "ok")
	for _, adapter := range []string{"mcp", "cli", "runtime"} {
		var inv opInvoker
		switch adapter {
		case "mcp":
			inv = mcpInvoker(e, secret)
		case "cli":
			inv = cliInvoker(srv.URL, secret)
		case "runtime":
			inv = runtimeInvoker(usecase.Registry(), opcore.CallContext{ProjectID: boot.Project.ID, Deps: &usecase.Deps{Repo: s, Runner: storeRunner{s}}})
		}
		got := expectOp(t, adapter, inv, "update_dashboard", args, "ok")
		// The receipt is the whole stored result — every field must be
		// identical, not just id/revision, or an adapter could drop or corrupt
		// part of the row and still look like a replay.
		var want, gotMap map[string]any
		if err := json.Unmarshal(first.raw, &want); err != nil {
			t.Fatalf("first receipt not an object: %s", string(first.raw))
		}
		if err := json.Unmarshal(got.raw, &gotMap); err != nil {
			t.Fatalf("%s receipt not an object: %s", adapter, string(got.raw))
		}
		if !reflect.DeepEqual(want, gotMap) {
			t.Fatalf("%s replayed a different receipt: %s vs %s", adapter, string(got.raw), string(first.raw))
		}
	}
	// And a different payload under that key conflicts on every adapter too.
	conflictArgs := fmt.Sprintf(`{"dashboard_id":%q,"name":"Cross 3","description":"z","revision":2,"idempotency_key":"cross-1"}`, dashID)
	expectOp(t, "mcp", mcpInvoker(e, secret), "update_dashboard", conflictArgs, "conflict")
	expectOp(t, "cli", cliInvoker(srv.URL, secret), "update_dashboard", conflictArgs, "conflict")
	expectOp(t, "runtime", runtimeInvoker(usecase.Registry(), opcore.CallContext{ProjectID: boot.Project.ID, Deps: &usecase.Deps{Repo: s, Runner: storeRunner{s}}}), "update_dashboard", conflictArgs, "conflict")
}

func TestOverviewAdaptersShareProjectTimezoneContract(t *testing.T) {
	s := openAppTestStore(t)
	ctx := context.Background()
	e := mountRealAdapters(t, s)
	registerOverviewRoutes(e, s, newOpAdapter(s, nil, storeRunner{s}))

	boot, err := s.CreateAccount(ctx, fmt.Sprintf("overview-parity-%d@test.local", time.Now().UnixNano()), "P", "password-123", "ws", "proj")
	if err != nil {
		t.Fatalf("account: %v", err)
	}
	zone := "Asia/Ho_Chi_Minh"
	if _, err := s.UpdateProjectForUser(ctx, boot.User.ID, boot.Project.ID, nil, &zone); err != nil {
		t.Fatalf("set timezone: %v", err)
	}
	_, secret, err := s.CreateProjectCredential(ctx, boot.User.ID, boot.Project.ID, "reader", []string{"analytics:read"})
	if err != nil {
		t.Fatalf("read credential: %v", err)
	}
	headers := map[string]string{"Authorization": "Bearer " + secret}

	type overviewContext struct {
		Timezone       string `json:"timezone"`
		TimezoneSource string `json:"timezone_source"`
		MetricVersion  string `json:"metric_version"`
	}
	decode := func(t *testing.T, body string) overviewContext {
		t.Helper()
		var out struct {
			Context overviewContext `json:"context"`
		}
		if err := json.Unmarshal([]byte(body), &out); err != nil {
			t.Fatalf("decode overview: %v; body=%s", err, body)
		}
		return out.Context
	}
	want := overviewContext{Timezone: zone, TimezoneSource: "project", MetricVersion: storage.OverviewMetricVersion}

	getReq := httptest.NewRequest(http.MethodGet, "/api/overview?period=today&platform=unknown", nil)
	getReq.Header.Set("Authorization", "Bearer "+secret)
	getRec := httptest.NewRecorder()
	e.ServeHTTP(getRec, getReq)
	if getRec.Code != http.StatusOK {
		t.Fatalf("GET overview: %d %s", getRec.Code, getRec.Body.String())
	}
	if got := decode(t, getRec.Body.String()); got != want {
		t.Fatalf("GET context = %+v, want %+v", got, want)
	}

	opRec := postJSON(t, e, "/api/op/overview", `{"period":"today","platform":"unknown"}`, headers)
	if opRec.Code != http.StatusOK {
		t.Fatalf("/api/op overview: %d %s", opRec.Code, opRec.Body.String())
	}
	if got := decode(t, opRec.Body.String()); got != want {
		t.Fatalf("/api/op context = %+v, want %+v", got, want)
	}

	mcpRec := postJSON(t, e, "/mcp", mcpCall("overview", `{"period":"today","platform":"unknown"}`), headers)
	if mcpRec.Code != http.StatusOK {
		t.Fatalf("MCP overview: %d %s", mcpRec.Code, mcpRec.Body.String())
	}
	var rpc struct {
		Result struct {
			StructuredContent struct {
				Context overviewContext `json:"context"`
			} `json:"structuredContent"`
		} `json:"result"`
	}
	if err := json.Unmarshal(mcpRec.Body.Bytes(), &rpc); err != nil {
		t.Fatalf("decode MCP overview: %v; body=%s", err, mcpRec.Body.String())
	}
	if got := rpc.Result.StructuredContent.Context; got != want {
		t.Fatalf("MCP context = %+v, want %+v", got, want)
	}
}

// overviewFailingRepo forces the one read the overview operation performs to
// fail with a given error; the embedded interface is never reached.
type overviewFailingRepo struct {
	usecase.Repo
	err error
}

func (r overviewFailingRepo) Overview(ctx context.Context, projectID, period, platform string, now time.Time) (storage.OverviewResult, error) {
	return storage.OverviewResult{}, r.err
}

// GET /api/overview must map an operation error through the same
// usecase.MapOpError as the mounted operation adapters: a typed not-found is a
// 404 here, not the blanket 400 the route used to send.
func TestOverviewAdaptersShareTypedErrorStatus(t *testing.T) {
	s := openAppTestStore(t)
	ctx := context.Background()
	boot, err := s.CreateAccount(ctx, fmt.Sprintf("overview-typed-%d@test.local", time.Now().UnixNano()), "P", "password-123", "ws", "proj")
	if err != nil {
		t.Fatalf("account: %v", err)
	}
	_, secret, err := s.CreateProjectCredential(ctx, boot.User.ID, boot.Project.ID, "reader", []string{"analytics:read"})
	if err != nil {
		t.Fatalf("read credential: %v", err)
	}

	e := echo.New()
	deps := &usecase.Deps{Repo: overviewFailingRepo{err: pgx.ErrNoRows}, Runner: storeRunner{s}, Audit: s}
	reg := usecase.Registry()
	resolve := func(c echo.Context) (opcore.Principal, error) { return principalFromRequest(c, s) }
	opcore.MountHTTP(e.Group("/api/op"), reg, deps, resolve)
	registerOverviewRoutes(e, s, &opAdapter{reg: reg, deps: deps})

	getReq := httptest.NewRequest(http.MethodGet, "/api/overview?period=7d", nil)
	getReq.Header.Set("Authorization", "Bearer "+secret)
	getRec := httptest.NewRecorder()
	e.ServeHTTP(getRec, getReq)
	opRec := postJSON(t, e, "/api/op/overview", `{"period":"7d"}`, map[string]string{"Authorization": "Bearer " + secret})

	if getRec.Code != http.StatusNotFound {
		t.Fatalf("GET overview on a not-found operation = %d %s, want 404", getRec.Code, getRec.Body.String())
	}
	if opRec.Code != getRec.Code {
		t.Fatalf("GET %d vs /api/op %d — the adapters disagree on a typed error", getRec.Code, opRec.Code)
	}
}

// fakeTierReader is the workspace-tier read the authoring helper depends on.
type fakeTierReader struct {
	cfg  storage.WorkspaceModelTiers
	keys map[string]string
	err  error
}

func (f fakeTierReader) WorkspaceTiersForRun(ctx context.Context, workspaceID string) (storage.WorkspaceModelTiers, map[string]string, error) {
	return f.cfg, f.keys, f.err
}

// Both authoring endpoints resolve their provider through one helper now, so
// the fallback chain has to be pinned here: an unconfigured pro tier inherits
// the flash default (including its key), a workspace with nothing configured is
// an ordinary configuration error — never the typed init one the definition
// route turns into a 502 — and a provider the factory rejects is typed.
func TestAuthoringProviderUsesFlashFallback(t *testing.T) {
	ctx := context.Background()

	reader := fakeTierReader{
		cfg:  storage.WorkspaceModelTiers{Provider: "openai", Model: "gpt-5-mini"},
		keys: map[string]string{"flash": "sk-flash"},
	}
	provider, model, err := authoringProvider(ctx, reader, "ws-1")
	if err != nil || provider == nil {
		t.Fatalf("flash fallback = provider %v model %q err %v", provider, model, err)
	}
	if model != "gpt-5-mini" {
		t.Fatalf("resolved model = %q, want the flash default", model)
	}

	_, _, err = authoringProvider(ctx, fakeTierReader{}, "ws-1")
	if err == nil {
		t.Fatal("an unconfigured workspace resolved a provider")
	}
	var initErr *authoringProviderInitError
	if errors.As(err, &initErr) {
		t.Fatalf("unconfigured tier classified as a provider-init failure: %v", err)
	}

	unbuildable := fakeTierReader{
		cfg:  storage.WorkspaceModelTiers{Provider: "mystery-router", Model: "m", BaseURL: ""},
		keys: map[string]string{"flash": "sk-flash"},
	}
	_, _, err = authoringProvider(ctx, unbuildable, "ws-1")
	if err == nil {
		t.Fatal("an unbuildable provider resolved without error")
	}
	if !errors.As(err, &initErr) {
		t.Fatalf("unbuildable provider err = %v, want the typed init error", err)
	}
}
