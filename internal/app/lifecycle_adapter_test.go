package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/lohi-ai/agentray/internal/dataplane/connector"
	storage "github.com/lohi-ai/agentray/internal/dataplane/store"
)

// Legacy REST ↔ shared-operation adapter tests: the legacy URLs keep their
// envelopes while the mutation runs through the registry — revision fencing,
// idempotent replay, reversible archive, and typed error mapping. Needs the
// compose Postgres + DuckDB; skips without them.

// storeRunner is the minimal SourceRunner for tests: it delegates to the real
// store enqueue path so the archived-source admission guard is exercised.
type storeRunner struct{ s *storage.Store }

func (r storeRunner) EnqueueRun(ctx context.Context, projectID, syncID, idemKey string) (connector.Run, bool, error) {
	return r.s.EnqueueConnectorRun(ctx, projectID, syncID, idemKey)
}

func (r storeRunner) CancelRun(runID string) {}

func newLifecycleEcho(t *testing.T, s *storage.Store) *echo.Echo {
	t.Helper()
	e := echo.New()
	ops := newOpAdapter(s, nil, storeRunner{s})
	registerConnectorRoutes(e, s, ops, nil)
	// The dashboard/chart block lives inside registerRoutes, which needs the
	// full server wiring; mount just those routes here instead.
	mountDashboardLifecycle(e, s, ops)
	return e
}

func sessionCookieFor(t *testing.T, s *storage.Store, userID string) *http.Cookie {
	t.Helper()
	session, token, err := s.CreateUserSession(t.Context(), userID, sessionTTL)
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	return &http.Cookie{Name: sessionCookieName, Value: token, Expires: session.ExpiresAt}
}

func doJSON(t *testing.T, e *echo.Echo, method, path, body string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	var reader *strings.Reader
	if body == "" {
		reader = strings.NewReader("")
	} else {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("Content-Type", "application/json")
	for _, ck := range cookies {
		req.AddCookie(ck)
	}
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	return rec
}

func TestLegacyDashboardLifecycleThroughOps(t *testing.T) {
	s := openAppTestStore(t)
	e := newLifecycleEcho(t, s)
	ctx := t.Context()

	boot, err := s.CreateAccount(ctx, fmt.Sprintf("lc-%d@example.com", time.Now().UnixNano()), "LC", "pw-12345", "ws", "proj")
	if err != nil {
		t.Fatalf("account: %v", err)
	}
	projectID := boot.Project.ID
	key := boot.Project.APIKey
	cookie := sessionCookieFor(t, s, boot.User.ID)

	// The project's API key is capture-only (new projects split at creation):
	// the legacy management surface refuses it, exactly as before the cutover.
	if rec := doJSON(t, e, http.MethodPost, "/api/dashboards?api_key="+key, `{"name":"x"}`); rec.Code != http.StatusForbidden {
		t.Fatalf("capture key on legacy route: %d, want 403", rec.Code)
	}

	// Create via the legacy route (session — the web client's credential).
	rec := doJSON(t, e, http.MethodPost, "/api/dashboards?project_id="+projectID, `{"name":"Ops","description":"d"}`, cookie)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create dashboard: %d %s", rec.Code, rec.Body.String())
	}
	var created struct {
		Dashboard storage.Dashboard `json:"dashboard"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	dashID := created.Dashboard.ID
	if created.Dashboard.Revision != 1 {
		t.Fatalf("new dashboard revision = %d, want 1", created.Dashboard.Revision)
	}

	// Update without a revision (legacy caller) — adapter resolves current.
	rec = doJSON(t, e, http.MethodPut, "/api/dashboards/"+dashID+"?project_id="+projectID, `{"name":"Ops 2","description":"d2"}`, cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("update dashboard: %d %s", rec.Code, rec.Body.String())
	}
	var updated struct {
		Dashboard storage.Dashboard `json:"dashboard"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &updated); err != nil {
		t.Fatal(err)
	}
	if updated.Dashboard.Name != "Ops 2" || updated.Dashboard.Revision != 2 {
		t.Fatalf("update result: %+v", updated.Dashboard)
	}

	// Stale revision → 409.
	rec = doJSON(t, e, http.MethodPut, "/api/dashboards/"+dashID+"?project_id="+projectID, `{"name":"Ops 3","description":"d3","revision":1}`, cookie)
	if rec.Code != http.StatusConflict {
		t.Fatalf("stale revision: %d %s", rec.Code, rec.Body.String())
	}

	// Idempotent replay: same key + same payload returns the first result.
	body := fmt.Sprintf(`{"name":"Ops 4","description":"d4","revision":%d,"idempotency_key":"k1"}`, updated.Dashboard.Revision)
	rec = doJSON(t, e, http.MethodPut, "/api/dashboards/"+dashID+"?project_id="+projectID, body, cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("update with key: %d %s", rec.Code, rec.Body.String())
	}
	var first struct {
		Dashboard storage.Dashboard `json:"dashboard"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &first)
	rec = doJSON(t, e, http.MethodPut, "/api/dashboards/"+dashID+"?project_id="+projectID, body, cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("replay: %d %s", rec.Code, rec.Body.String())
	}
	var replayed struct {
		Dashboard storage.Dashboard `json:"dashboard"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &replayed)
	if replayed.Dashboard.Revision != first.Dashboard.Revision {
		t.Fatalf("replay bumped revision: %d → %d", first.Dashboard.Revision, replayed.Dashboard.Revision)
	}

	// Same key, different payload → 409.
	rec = doJSON(t, e, http.MethodPut, "/api/dashboards/"+dashID+"?project_id="+projectID,
		fmt.Sprintf(`{"name":"Other","description":"x","revision":%d,"idempotency_key":"k1"}`, replayed.Dashboard.Revision), cookie)
	if rec.Code != http.StatusConflict {
		t.Fatalf("key reuse with different payload: %d %s", rec.Code, rec.Body.String())
	}

	// Chart create/update/delete through the same adapter.
	rec = doJSON(t, e, http.MethodPost, "/api/dashboards/"+dashID+"/charts?project_id="+projectID,
		`{"name":"C","kind":"bar","metric":"events","event_type":"agent","col_span":2}`, cookie)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create chart: %d %s", rec.Code, rec.Body.String())
	}
	var chartRes struct {
		Chart storage.Chart `json:"chart"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &chartRes); err != nil {
		t.Fatal(err)
	}
	if chartRes.Chart.EventType != "agent" || chartRes.Chart.ColSpan != 2 {
		t.Fatalf("chart fields dropped: %+v", chartRes.Chart)
	}

	rec = doJSON(t, e, http.MethodDelete, "/api/charts/"+chartRes.Chart.ID+"?project_id="+projectID, "", cookie)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete chart: %d %s", rec.Code, rec.Body.String())
	}
	// Reversible: the row is archived, not deleted.
	archived, err := s.ChartForProject(ctx, projectID, chartRes.Chart.ID)
	if err != nil {
		t.Fatalf("archived chart read: %v", err)
	}
	if archived.ArchivedAt == nil {
		t.Fatal("chart was hard-deleted, want archived")
	}
	// …and it leaves the active list.
	rec = doJSON(t, e, http.MethodGet, "/api/dashboards/"+dashID+"/charts?project_id="+projectID, "", cookie)
	var charts struct {
		Charts []storage.Chart `json:"charts"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &charts)
	if len(charts.Charts) != 0 {
		t.Fatalf("archived chart still listed: %+v", charts.Charts)
	}

	// Dashboard delete archives; the row survives.
	rec = doJSON(t, e, http.MethodDelete, "/api/dashboards/"+dashID+"?project_id="+projectID, "", cookie)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete dashboard: %d %s", rec.Code, rec.Body.String())
	}
	dash, err := s.DashboardForProject(ctx, projectID, dashID)
	if err != nil || dash.ArchivedAt == nil {
		t.Fatalf("dashboard not archived: %+v %v", dash, err)
	}
	rec = doJSON(t, e, http.MethodGet, "/api/dashboards?project_id="+projectID, "", cookie)
	var boards struct {
		Dashboards []storage.Dashboard `json:"dashboards"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &boards)
	for _, d := range boards.Dashboards {
		if d.ID == dashID {
			t.Fatal("archived dashboard still listed")
		}
	}

	// Missing id → 404 (typed not-found), foreign project → 404.
	rec = doJSON(t, e, http.MethodPut, "/api/dashboards/00000000-0000-0000-0000-000000000000?project_id="+projectID, `{"name":"x","description":"y"}`, cookie)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing dashboard: %d %s", rec.Code, rec.Body.String())
	}

	// Session caller gets the same surface.
	rec = doJSON(t, e, http.MethodGet, "/api/dashboards?project_id="+projectID, "", cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("session list: %d %s", rec.Code, rec.Body.String())
	}
}

func TestLegacyConnectorLifecycleThroughOps(t *testing.T) {
	t.Setenv("AGENT_KEY_ENC_SECRET", "lifecycle-adapter-test-secret")
	s := openAppTestStore(t)
	e := newLifecycleEcho(t, s)
	ctx := t.Context()

	boot, err := s.CreateAccount(ctx, fmt.Sprintf("lc2-%d@example.com", time.Now().UnixNano()), "LC2", "pw-12345", "ws", "proj")
	if err != nil {
		t.Fatalf("account: %v", err)
	}
	projectID := boot.Project.ID
	cookie := sessionCookieFor(t, s, boot.User.ID)

	cred, err := s.CreateSourceCredential(ctx, boot.User.ID, projectID, "pg", "postgres://u:p@h:5432/d")
	if err != nil {
		t.Fatalf("credential: %v", err)
	}

	rec := doJSON(t, e, http.MethodPost, "/api/connectors?project_id="+projectID,
		fmt.Sprintf(`{"name":"pg","kind":"postgres","credential_id":%q,"idempotency_key":"ck1"}`, cred.ID), cookie)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create connector: %d %s", rec.Code, rec.Body.String())
	}
	var created struct {
		Connector storage.DataConnector `json:"connector"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	connID := created.Connector.ID

	// Same key + same payload replays the receipt — no second connector.
	rec = doJSON(t, e, http.MethodPost, "/api/connectors?project_id="+projectID,
		fmt.Sprintf(`{"name":"pg","kind":"postgres","credential_id":%q,"idempotency_key":"ck1"}`, cred.ID), cookie)
	if rec.Code != http.StatusCreated {
		t.Fatalf("replay create: %d %s", rec.Code, rec.Body.String())
	}
	var replayed struct {
		Connector storage.DataConnector `json:"connector"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &replayed)
	if replayed.Connector.ID != connID {
		t.Fatalf("replay created a second connector: %s vs %s", replayed.Connector.ID, connID)
	}

	// Sync create + list through the adapter.
	rec = doJSON(t, e, http.MethodPost, "/api/connectors/"+connID+"/syncs?project_id="+projectID,
		`{"source_table":"orders","key_column":"id","cursor_column":"updated_at","schedule_cron":"*/5 * * * *","enabled":true,"join_key":"","deletion_mode":"none","soft_delete_column":"","soft_delete_semantics":""}`, cookie)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create sync: %d %s", rec.Code, rec.Body.String())
	}
	var syncRes struct {
		Sync storage.ConnectorSync `json:"sync"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &syncRes)

	rec = doJSON(t, e, http.MethodGet, "/api/connectors/"+connID+"/syncs?project_id="+projectID, "", cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("list syncs: %d %s", rec.Code, rec.Body.String())
	}
	var syncs struct {
		Syncs []storage.ConnectorSync `json:"syncs"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &syncs)
	if len(syncs.Syncs) != 1 || syncs.Syncs[0].ID != syncRes.Sync.ID {
		t.Fatalf("syncs: %+v", syncs.Syncs)
	}

	// DELETE archives the connector and pauses its sync — nothing is deleted.
	rec = doJSON(t, e, http.MethodDelete, "/api/connectors/"+connID+"?project_id="+projectID, "", cookie)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("archive connector: %d %s", rec.Code, rec.Body.String())
	}
	conn, err := s.DataConnectorForProject(ctx, projectID, connID)
	if err != nil || conn.ArchivedAt == nil {
		t.Fatalf("connector not archived: %+v %v", conn, err)
	}
	sync, err := s.ConnectorSyncForProject(ctx, projectID, syncRes.Sync.ID)
	if err != nil {
		t.Fatalf("sync read: %v", err)
	}
	if sync.Enabled || !sync.DisabledByArchive {
		t.Fatalf("sync not paused by archive: %+v", sync)
	}

	// The archived connector leaves the active list.
	rec = doJSON(t, e, http.MethodGet, "/api/connectors?project_id="+projectID, "", cookie)
	var list struct {
		Connectors []storage.DataConnector `json:"connectors"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &list)
	for _, cn := range list.Connectors {
		if cn.ID == connID {
			t.Fatal("archived connector still listed")
		}
	}

	// Run-now against an archived source fails closed in the legacy envelope.
	rec = doJSON(t, e, http.MethodPost, "/api/connector-syncs/"+syncRes.Sync.ID+"/run?project_id="+projectID, `{}`, cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("run archived sync: %d %s", rec.Code, rec.Body.String())
	}
	var runRes struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &runRes)
	if runRes.OK || !strings.Contains(runRes.Error, "archived") {
		t.Fatalf("run under archive should fail closed: %+v", runRes)
	}

	// Unknown sync → legacy 404.
	rec = doJSON(t, e, http.MethodPost, "/api/connector-syncs/00000000-0000-0000-0000-000000000000/run?project_id="+projectID, `{}`, cookie)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("run missing sync: %d %s", rec.Code, rec.Body.String())
	}
}
