package usecase

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/labstack/echo/v4"
	"github.com/lohi-ai/agentray/internal/dataplane/connector"
	storage "github.com/lohi-ai/agentray/internal/dataplane/store"
	"github.com/lohi-ai/agentray/internal/shared/opcore"
)

// Consumer-parity evidence (slice 3): the typed error contract — not_found /
// conflict / retryable — must survive every adapter identically, and the
// connector-run receipt must read and cancel through the shared operations.

// parityRepo is a fakeRepo that also answers the source-lifecycle reads the
// run/status/cancel operations need.
type parityRepo struct {
	fakeRepo
	runErr     error
	syncErr    error
	cancelErr  error
	verifyRows []storage.Event
	linked     bool
	// connectorErr is the connector-existence answer source_status reads
	// before it lists syncs: pgx.ErrNoRows makes the id unknown.
	connectorErr error
}

func (f *parityRepo) DataConnectorForProject(context.Context, string, string) (storage.DataConnector, error) {
	if f.connectorErr != nil {
		return storage.DataConnector{}, f.connectorErr
	}
	return storage.DataConnector{ID: "connector-1"}, nil
}

func (f *parityRepo) ConnectorRunForProject(context.Context, string, string) (storage.ConnectorRun, error) {
	return storage.ConnectorRun{}, f.runErr
}

func (f *parityRepo) ConnectorSyncForProject(context.Context, string, string) (storage.ConnectorSync, error) {
	return storage.ConnectorSync{}, f.syncErr
}

func (f *parityRepo) CancelConnectorRun(context.Context, string, string) (storage.ConnectorRun, error) {
	return storage.ConnectorRun{ID: "run-1", Status: "cancelled"}, f.cancelErr
}

func (f *parityRepo) RecentEventsForVerification(context.Context, string, int, time.Time) ([]storage.Event, error) {
	return f.verifyRows, nil
}

func (f *parityRepo) DistinctIDLinked(context.Context, string, string) (bool, error) {
	return f.linked, nil
}

func (f *parityRepo) LatestConnectorRunsForProject(context.Context, string, []string) (map[string]storage.ConnectorRun, error) {
	return map[string]storage.ConnectorRun{}, nil
}

// batchStatusRepo proves source_status asks the repository once for a
// connector's latest receipts rather than issuing one LatestConnectorRun read
// for every sync on every web poll.
type batchStatusRepo struct {
	fakeRepo
	syncs      []storage.ConnectorSync
	runs       map[string]storage.ConnectorRun
	batchCalls [][]string
}

// DataConnectorForProject answers the existence read source_status performs
// before listing; the connector exists unless a test says otherwise.
func (f *batchStatusRepo) DataConnectorForProject(context.Context, string, string) (storage.DataConnector, error) {
	return storage.DataConnector{ID: "connector-1"}, nil
}

func (f *batchStatusRepo) ListConnectorSyncsForProject(context.Context, string, string) ([]storage.ConnectorSync, error) {
	return f.syncs, nil
}

func (f *batchStatusRepo) LatestConnectorRunsForProject(_ context.Context, _ string, ids []string) (map[string]storage.ConnectorRun, error) {
	f.batchCalls = append(f.batchCalls, append([]string(nil), ids...))
	return f.runs, nil
}

// fakeRunner records enqueue/cancel and can be set to fail with the engine's
// sentinels.
type fakeRunner struct {
	enqueueErr error
	cancelled  []string
}

func (f *fakeRunner) EnqueueRun(context.Context, string, string, string) (connector.Run, bool, error) {
	if f.enqueueErr != nil {
		return connector.Run{}, false, f.enqueueErr
	}
	return connector.Run{ID: "run-1", Status: "queued"}, true, nil
}

func (f *fakeRunner) CancelRun(runID string) { f.cancelled = append(f.cancelled, runID) }

// parityAdapters mounts the real registry over fake deps on both network
// adapters, resolving every request to a session admin — the credential
// contract is covered elsewhere; this suite isolates the error taxonomy.
func parityAdapters(t *testing.T, deps *Deps) *echo.Echo {
	t.Helper()
	reg := Registry()
	e := echo.New()
	resolve := func(echo.Context) (opcore.Principal, error) {
		return opcore.Principal{ProjectID: "p1", Kind: opcore.CredSession, Role: "admin",
			Grants: []opcore.Access{opcore.AccessAnalyticsRead, opcore.AccessDashboardsWrite, opcore.AccessSourcesRead, opcore.AccessSourcesManage}}, nil
	}
	opcore.MountHTTP(e.Group("/api/op"), reg, deps, resolve)
	opcore.MountMCP(e.Group("/mcp"), reg, deps, resolve)
	return e
}

func opPost(t *testing.T, e *echo.Echo, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	return rec
}

func mcpToolCall(t *testing.T, e *echo.Echo, name, args string) map[string]any {
	t.Helper()
	rec := opPost(t, e, "/mcp", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":`+quote(name)+`,"arguments":`+args+`}}`)
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("mcp response: %v — %s", err, rec.Body.String())
	}
	result, _ := resp["result"].(map[string]any)
	if result == nil {
		t.Fatalf("mcp call %s: no result: %s", name, rec.Body.String())
	}
	return result
}

func quote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// restError decodes the {error, code} failure body.
func restError(t *testing.T, rec *httptest.ResponseRecorder) (code, msg string) {
	t.Helper()
	var body struct {
		Error string `json:"error"`
		Code  string `json:"code"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("error body: %v — %s", err, rec.Body.String())
	}
	return body.Code, body.Error
}

func mcpErrorCode(result map[string]any) string {
	meta, _ := result["_meta"].(map[string]any)
	code, _ := meta["error_code"].(string)
	return code
}

func mcpErrorText(result map[string]any) string {
	content, _ := result["content"].([]any)
	if len(content) == 0 {
		return ""
	}
	text, _ := content[0].(map[string]any)["text"].(string)
	return text
}

// The same not-found must surface as 404+code over REST, isError+error_code
// over MCP, and a "not_found:"-prefixed error through the in-process tool.
func TestNotFoundParityAcrossAdapters(t *testing.T) {
	deps := &Deps{Repo: &parityRepo{runErr: pgx.ErrNoRows}, Runner: &fakeRunner{}}
	e := parityAdapters(t, deps)

	rec := opPost(t, e, "/api/op/source_status", `{"run_id":"missing"}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("REST status = %d, want 404: %s", rec.Code, rec.Body.String())
	}
	code, msg := restError(t, rec)
	if code != "not_found" || !strings.Contains(msg, "run not found") {
		t.Fatalf("REST body code=%q msg=%q", code, msg)
	}

	result := mcpToolCall(t, e, "source_status", `{"run_id":"missing"}`)
	if result["isError"] != true {
		t.Fatalf("MCP isError missing: %v", result)
	}
	if got := mcpErrorCode(result); got != "not_found" {
		t.Fatalf("MCP error_code = %q", got)
	}
	if !strings.Contains(mcpErrorText(result), "not_found: run not found") {
		t.Fatalf("MCP text = %q", mcpErrorText(result))
	}

	// In-process tool: the classified error reaches the model as text.
	reg := Registry()
	tool := opcore.Tools(reg, opcore.CallContext{ProjectID: "p1", Deps: deps})
	var statusTool interface {
		Run(context.Context, string) (string, error)
	}
	for _, tl := range tool {
		if tl.Name() == "source_status" {
			statusTool = tl
		}
	}
	_, err := statusTool.Run(context.Background(), `{"run_id":"missing"}`)
	var oe *opcore.OpError
	if !errors.As(err, &oe) || oe.Kind != opcore.ErrNotFound {
		t.Fatalf("tool error = %v, want not_found OpError", err)
	}
	if !strings.Contains(err.Error(), "not_found: run not found") {
		t.Fatalf("tool error text = %q", err)
	}

	// CLI transport: the typed body decodes back into an OpError.
	srv := httptest.NewServer(e)
	defer srv.Close()
	_, err = opcore.NewClient(srv.URL, "").Call(context.Background(), "source_status", []byte(`{"run_id":"missing"}`))
	oe = nil
	if !errors.As(err, &oe) || oe.Kind != opcore.ErrNotFound {
		t.Fatalf("client error = %v, want not_found OpError", err)
	}
}

// The legacy REST adapter invokes MapOpError directly, after a handler has
// already returned an OpError. Keep those explicit classifications instead of
// downgrading them to the mapper's generic 400 fallback.
func TestMapOpErrorPreservesExplicitOperationKinds(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want int
	}{
		{opcore.NotFound("gone"), http.StatusNotFound},
		{opcore.Conflict("stale"), http.StatusConflict},
		{opcore.Retryable("busy"), http.StatusServiceUnavailable},
	} {
		mapped := MapOpError(tc.err)
		var he *echo.HTTPError
		if !errors.As(mapped, &he) || he.Code != tc.want {
			t.Fatalf("MapOpError(%v) = %#v, want HTTP %d", tc.err, mapped, tc.want)
		}
	}
}

// Paused syncs conflict; a saturated engine is retryable — and the kinds must
// agree across REST and MCP.
func TestConflictAndRetryableParity(t *testing.T) {
	deps := &Deps{Repo: &parityRepo{}, Runner: &fakeRunner{enqueueErr: storage.ErrSyncPaused}}
	e := parityAdapters(t, deps)

	rec := opPost(t, e, "/api/op/run_source", `{"sync_id":"s1"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("paused run_source status = %d, want 409: %s", rec.Code, rec.Body.String())
	}
	if code, _ := restError(t, rec); code != "conflict" {
		t.Fatalf("paused code = %q", code)
	}
	result := mcpToolCall(t, e, "run_source", `{"sync_id":"s1"}`)
	if got := mcpErrorCode(result); got != "conflict" {
		t.Fatalf("MCP paused error_code = %q", got)
	}

	deps.Runner = &fakeRunner{enqueueErr: connector.ErrEngineBusy}
	rec = opPost(t, e, "/api/op/run_source", `{"sync_id":"s1"}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("busy run_source status = %d, want 503: %s", rec.Code, rec.Body.String())
	}
	if code, _ := restError(t, rec); code != "retryable" {
		t.Fatalf("busy code = %q", code)
	}
	result = mcpToolCall(t, e, "run_source", `{"sync_id":"s1"}`)
	if got := mcpErrorCode(result); got != "retryable" {
		t.Fatalf("MCP busy error_code = %q", got)
	}
}

// run_source returns the durable receipt; cancel_source_run flags it and
// pokes the in-process worker — the web/CLI/agent all drive this one path.
func TestRunReceiptAndCancelThroughOps(t *testing.T) {
	runner := &fakeRunner{}
	deps := &Deps{Repo: &parityRepo{}, Runner: runner}
	e := parityAdapters(t, deps)

	rec := opPost(t, e, "/api/op/run_source", `{"sync_id":"s1","idempotency_key":"k1"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("run_source: %d %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Run      connector.Run `json:"run"`
		Enqueued bool          `json:"enqueued"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("run_source body: %v", err)
	}
	if out.Run.ID != "run-1" || out.Run.Status != "queued" || !out.Enqueued {
		t.Fatalf("receipt = %+v", out)
	}

	rec = opPost(t, e, "/api/op/cancel_source_run", `{"run_id":"run-1"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("cancel_source_run: %d %s", rec.Code, rec.Body.String())
	}
	var run connector.Run
	if err := json.Unmarshal(rec.Body.Bytes(), &run); err != nil {
		t.Fatalf("cancel body: %v", err)
	}
	if run.Status != "cancelled" {
		t.Fatalf("cancelled run status = %q", run.Status)
	}
	if len(runner.cancelled) != 1 || runner.cancelled[0] != "run-1" {
		t.Fatalf("engine cancel not signalled: %v", runner.cancelled)
	}

	// Cancelling an unknown run is a typed not-found, not a 400.
	deps.Repo = &parityRepo{cancelErr: pgx.ErrNoRows}
	rec = opPost(t, e, "/api/op/cancel_source_run", `{"run_id":"gone"}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("cancel unknown run: %d %s", rec.Code, rec.Body.String())
	}
}

// verify_sdk answers found/not-found over the bounded arrival-time read —
// registry presence alone never proved the handler's contract.
func TestVerifySDKFoundAndNotFound(t *testing.T) {
	now := time.Now().UTC()
	deps := &Deps{Repo: &parityRepo{verifyRows: []storage.Event{
		{EventName: "page_view", DistinctID: "anon-1", Platform: "web", InsertedAt: &now},
	}}, Runner: &fakeRunner{}}
	reg := Registry()
	cc := opcore.CallContext{ProjectID: "p1", Deps: deps}
	spec, _ := reg.Get("verify_sdk")

	// Named event found: found=true, arrival receipt, platform, searched count.
	out, err := spec.OpInvoke(context.Background(), cc, `{"event_name":"page_view"}`)
	if err != nil {
		t.Fatalf("verify_sdk: %v", err)
	}
	var res struct {
		Found          bool   `json:"found"`
		EventName      string `json:"event_name"`
		ReceivedAt     string `json:"received_at"`
		Platform       string `json:"platform"`
		IdentityLinked bool   `json:"identity_linked"`
		Searched       int    `json:"searched"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("verify_sdk output: %v", err)
	}
	if !res.Found || res.EventName != "page_view" || res.Platform != "web" || res.ReceivedAt == "" || res.Searched != 1 {
		t.Fatalf("verify_sdk found = %+v", res)
	}

	// A different name is not-found — honest searched count, no fabrication.
	out, err = spec.OpInvoke(context.Background(), cc, `{"event_name":"signed_up"}`)
	if err != nil {
		t.Fatalf("verify_sdk miss: %v", err)
	}
	res.Found = true
	res.Searched = 0
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("verify_sdk miss output: %v", err)
	}
	if res.Found || res.Searched != 1 {
		t.Fatalf("verify_sdk miss = %+v", res)
	}

	// Empty window: found=false with the disclosure warning.
	deps.Repo = &parityRepo{}
	out, err = spec.OpInvoke(context.Background(), cc, `{}`)
	if err != nil {
		t.Fatalf("verify_sdk empty: %v", err)
	}
	if !strings.Contains(out, `"found":false`) || !strings.Contains(out, `"searched":0`) {
		t.Fatalf("verify_sdk empty = %s", out)
	}
}

func TestSourceStatusBatchesLatestRuns(t *testing.T) {
	repo := &batchStatusRepo{
		syncs: []storage.ConnectorSync{
			{ID: "sync-1", ConnectorID: "connector-1"},
			{ID: "sync-2", ConnectorID: "connector-1"},
			{ID: "sync-3", ConnectorID: "connector-1"},
		},
		runs: map[string]storage.ConnectorRun{
			"sync-2": {ID: "run-2", SyncID: "sync-2", Status: "running"},
		},
	}
	reg := Registry()
	spec, ok := reg.Get("source_status")
	if !ok {
		t.Fatal("source_status not registered")
	}
	out, err := spec.OpInvoke(context.Background(), opcore.CallContext{
		ProjectID: "p1",
		Deps:      &Deps{Repo: repo},
	}, `{"connector_id":"connector-1"}`)
	if err != nil {
		t.Fatalf("source_status: %v", err)
	}
	var result sourceStatusOutput
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("source_status output: %v", err)
	}
	if len(repo.batchCalls) != 1 || strings.Join(repo.batchCalls[0], ",") != "sync-1,sync-2,sync-3" {
		t.Fatalf("latest runs batch calls = %#v", repo.batchCalls)
	}
	if len(result.Syncs) != 3 || result.Syncs[1].LatestRun == nil || result.Syncs[1].LatestRun.ID != "run-2" {
		t.Fatalf("source_status result = %+v", result)
	}
}

// An unknown connector id is not-found on every adapter. "This connector has
// no syncs" and "there is no such connector" are different answers, and only
// the connector existence read can tell them apart — before this, a typo
// returned an empty success the agent reported as a healthy source.
func TestSourceStatusUnknownConnectorIsNotFound(t *testing.T) {
	deps := &Deps{Repo: &parityRepo{connectorErr: pgx.ErrNoRows}, Runner: &fakeRunner{}}
	e := parityAdapters(t, deps)

	rec := opPost(t, e, "/api/op/source_status", `{"connector_id":"00000000-0000-0000-0000-000000000000"}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("REST status = %d, want 404: %s", rec.Code, rec.Body.String())
	}
	if code, msg := restError(t, rec); code != "not_found" || !strings.Contains(msg, "connector not found") {
		t.Fatalf("REST body code=%q msg=%q", code, msg)
	}

	result := mcpToolCall(t, e, "source_status", `{"connector_id":"00000000-0000-0000-0000-000000000000"}`)
	if result["isError"] != true {
		t.Fatalf("MCP isError missing: %v", result)
	}
	if got := mcpErrorCode(result); got != "not_found" {
		t.Fatalf("MCP error_code = %q", got)
	}
	if !strings.Contains(mcpErrorText(result), "not_found: connector not found") {
		t.Fatalf("MCP text = %q", mcpErrorText(result))
	}
}
