package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
)

func listedMCPTools(t *testing.T, e *echo.Echo, secret string) map[string]bool {
	t.Helper()
	rec := postJSON(t, e, "/mcp", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, map[string]string{"Authorization": "Bearer " + secret})
	var response struct {
		Result struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode tools/list: %v; body=%s", err, rec.Body.String())
	}
	out := map[string]bool{}
	for _, tool := range response.Result.Tools {
		out[tool.Name] = true
	}
	return out
}

func TestQuerySecurityCredentialMatrix(t *testing.T) {
	s := openAppTestStore(t)
	ctx := context.Background()
	stamp := time.Now().UnixNano()
	owner, err := s.CreateAccount(ctx, fmt.Sprintf("query-security-%d@test.local", stamp), "Owner", "password-123", "ws", "query")
	if err != nil {
		t.Fatal(err)
	}
	foreign, err := s.CreateAccount(ctx, fmt.Sprintf("query-security-foreign-%d@test.local", stamp), "Other", "password-123", "ws", "foreign")
	if err != nil {
		t.Fatal(err)
	}
	seedAppQueryFixture(t, s, owner.Project.ID, foreign.Project.ID)
	e := mountServerRoutes(t, s)
	registerOpRoutes(e, s, nil, nil)
	registerMcpRoutes(e, s, nil, nil)

	type matrixRow struct {
		name            string
		scopes          []string
		analytics       bool
		sourcesRead     bool
		dashboardsWrite bool
		sourcesManage   bool
	}
	rows := []matrixRow{
		{"investigator", []string{"analytics:read", "sources:read"}, true, true, false, false},
		{"sources reader", []string{"sources:read"}, false, true, false, false},
		{"analytics reader", []string{"analytics:read"}, true, false, false, false},
		{"board author", []string{"analytics:read", "dashboards:write"}, true, false, true, false},
		{"finding author", []string{"plans:write"}, false, false, false, false},
		{"growth writer", []string{"growth:write"}, false, false, false, false},
		{"source operator", []string{"sources:manage"}, false, true, false, true},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			_, secret, err := s.CreateProjectCredential(ctx, owner.User.ID, owner.Project.ID, row.name, row.scopes)
			if err != nil {
				t.Fatal(err)
			}
			tools := listedMCPTools(t, e, secret)
			for name, want := range map[string]bool{
				"run_sql": row.analytics, "list_sources": row.sourcesRead,
				"create_dashboard": row.dashboardsWrite, "create_source": row.sourcesManage,
			} {
				if tools[name] != want {
					t.Errorf("tools/list %s = %v, want %v", name, tools[name], want)
				}
			}

			body := `{"sql":"SELECT count(*) AS n FROM events"}`
			rec := postJSON(t, e, "/api/op/run_sql", body, map[string]string{"Authorization": "Bearer " + secret})
			if row.analytics && rec.Code != http.StatusOK {
				t.Errorf("authorized run_sql = %d %s", rec.Code, rec.Body.String())
			}
			if !row.analytics && rec.Code != http.StatusForbidden {
				t.Errorf("unauthorized run_sql = %d %s, want 403", rec.Code, rec.Body.String())
			}
			mcp := mcpInvoker(e, secret)(t, "run_sql", `{"sql":"SELECT count(*) AS n FROM events"}`)
			if (mcp.class == "ok") != row.analytics {
				t.Errorf("MCP direct run_sql class=%s, analytics=%v", mcp.class, row.analytics)
			}
		})
	}

	// Capture, missing, invalid, and revoked credentials are all denied before
	// SQL validation, so none can use an error oracle to inspect the project.
	revoked, revokedSecret, err := s.CreateProjectCredential(ctx, owner.User.ID, owner.Project.ID, "revoked", []string{"analytics:read"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RevokeProjectCredential(ctx, owner.User.ID, owner.Project.ID, revoked.ID); err != nil {
		t.Fatal(err)
	}
	for name, headers := range map[string]map[string]string{
		"capture": {"X-API-Key": owner.Project.APIKey},
		"missing": {},
		"invalid": {"Authorization": "Bearer agm_invalid"},
		"revoked": {"Authorization": "Bearer " + revokedSecret},
	} {
		t.Run(name, func(t *testing.T) {
			rec := postJSON(t, e, "/api/op/run_sql", `{"sql":"SELECT count(*) FROM events"}`, headers)
			if rec.Code == http.StatusOK {
				t.Fatalf("credential unexpectedly ran SQL: %s", rec.Body.String())
			}
		})
	}
}

func TestQuerySecurityIsolationAndExternalIODenials(t *testing.T) {
	s := openAppTestStore(t)
	ctx := context.Background()
	stamp := time.Now().UnixNano()
	owner, err := s.CreateAccount(ctx, fmt.Sprintf("query-isolation-%d@test.local", stamp), "Owner", "password-123", "ws", "query")
	if err != nil {
		t.Fatal(err)
	}
	foreign, err := s.CreateAccount(ctx, fmt.Sprintf("query-isolation-foreign-%d@test.local", stamp), "Other", "password-123", "ws", "foreign")
	if err != nil {
		t.Fatal(err)
	}
	seedAppQueryFixture(t, s, owner.Project.ID, foreign.Project.ID)
	_, secret, err := s.CreateProjectCredential(ctx, owner.User.ID, owner.Project.ID, "reader", []string{"analytics:read"})
	if err != nil {
		t.Fatal(err)
	}
	e := mountServerRoutes(t, s)
	registerOpRoutes(e, s, nil, nil)

	foreignPredicate := fmt.Sprintf(`{"sql":"SELECT count(*) AS n FROM events WHERE project_id = '%s'"}`, foreign.Project.ID)
	rec := postJSON(t, e, "/api/op/run_sql", foreignPredicate, map[string]string{"Authorization": "Bearer " + secret})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"n":0`) {
		t.Fatalf("server project fence did not isolate foreign id: %d %s", rec.Code, rec.Body.String())
	}

	foreignSaved, err := s.CreateSavedQuery(ctx, foreign.Project.ID, "FOREIGN_LABEL_MUST_NOT_LEAK", "SELECT 'FOREIGN_SQL_MUST_NOT_LEAK' FROM events", true)
	if err != nil {
		t.Fatal(err)
	}
	rec = postJSON(t, e, "/api/saved-queries/"+foreignSaved.ID+"/run?project_id="+owner.Project.ID, `{}`, map[string]string{"Authorization": "Bearer " + secret})
	if rec.Code == http.StatusOK || strings.Contains(rec.Body.String(), "FOREIGN_") {
		t.Fatalf("foreign saved query disclosed or executed: %d %s", rec.Code, rec.Body.String())
	}

	for _, sql := range []string{
		`SELECT * FROM read_csv('/tmp/query-access-secret.csv'), events`,
		`SELECT * FROM postgres_scan('postgres://secret@example.invalid/db', 'public', 'users'), events`,
		`WITH x AS (ATTACH '/tmp/foreign.duckdb') SELECT * FROM events`,
		`SELECT * FROM 'https://example.invalid/private.csv', events`,
	} {
		body, _ := json.Marshal(map[string]string{"sql": sql})
		rec := postJSON(t, e, "/api/op/run_sql", string(body), map[string]string{"Authorization": "Bearer " + secret})
		if rec.Code == http.StatusOK {
			t.Errorf("external I/O SQL succeeded: %s", sql)
		}
		if strings.Contains(rec.Body.String(), "secret@example") || strings.Contains(rec.Body.String(), "query-access-secret") {
			t.Errorf("denial echoed a credential/path: %s", rec.Body.String())
		}
	}
}
