package app

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
	storage "github.com/lohi-ai/agentray/internal/dataplane/store"
	"github.com/lohi-ai/agentray/internal/dataplane/usecase"
	"github.com/lohi-ai/agentray/internal/shared/opcore"
)

// Adapter parity: the real registry behind the real MCP + /api/op mounts,
// authorized by the real principal resolver. Proves the credential contract
// end to end — capture keys denied everywhere, management scopes enforced per
// operation, and both adapters answer identically.

// mountRealAdapters wires the production registry + resolver onto a test echo.
func mountRealAdapters(t *testing.T, s *storage.Store) *echo.Echo {
	t.Helper()
	e := echo.New()
	deps := &usecase.Deps{Repo: s}
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
	rec = postJSON(t, e, "/api/op/source_status", `{"connector_id":"00000000-0000-0000-0000-000000000000"}`, map[string]string{"Authorization": "Bearer " + srcSecret})
	if rec.Code != http.StatusOK {
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
