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
	s := openRequiredAppTestStore(t)
	t.Setenv("AGENT_KEY_ENC_SECRET", "query-security-test-secret")
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
		plansWrite      bool
		growthWrite     bool
		sourcesManage   bool
	}
	rows := []matrixRow{
		{"investigator", []string{"analytics:read", "sources:read"}, true, true, false, false, false, false},
		{"sources reader", []string{"sources:read"}, false, true, false, false, false, false},
		{"analytics reader", []string{"analytics:read"}, true, false, false, false, false, false},
		{"board author", []string{"analytics:read", "dashboards:write"}, true, false, true, false, false, false},
		{"finding author", []string{"plans:write"}, false, false, false, true, false, false},
		{"growth writer", []string{"growth:write"}, false, false, false, false, true, false},
		{"source operator", []string{"sources:manage"}, false, true, false, false, false, true},
	}
	sourceCredential, err := s.CreateSourceCredential(ctx, owner.User.ID, owner.Project.ID, "query-security-source", "postgres://user:password@example.invalid/db")
	if err != nil {
		t.Fatal(err)
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
				"create_dashboard": row.dashboardsWrite, "submit_recommendation": row.plansWrite,
				"remember": row.growthWrite, "create_source": row.sourcesManage,
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

			// Every write profile performs its permitted mutation on both
			// network adapters, while every other write class is invoked
			// directly and must be typed as denied (not merely omitted from
			// tools/list).
			writes := []struct {
				name    string
				allowed bool
				args    func(adapter string) string
			}{
				{"create_dashboard", row.dashboardsWrite, func(adapter string) string {
					return fmt.Sprintf(`{"name":"security-%s-%s"}`, strings.ReplaceAll(row.name, " ", "-"), adapter)
				}},
				{"submit_recommendation", row.plansWrite, func(adapter string) string {
					return fmt.Sprintf(`{"title":"security finding %s %s","idempotency_key":"%d-%s-%s-finding"}`, row.name, adapter, stamp, strings.ReplaceAll(row.name, " ", "-"), adapter)
				}},
				{"remember", row.growthWrite, func(adapter string) string {
					return fmt.Sprintf(`{"content":"security memory %s %s"}`, row.name, adapter)
				}},
				{"create_source", row.sourcesManage, func(adapter string) string {
					return fmt.Sprintf(`{"name":"security source %s %s","kind":"postgres","credential_id":%q,"idempotency_key":"%d-%s-%s-source"}`, row.name, adapter, sourceCredential.ID, stamp, strings.ReplaceAll(row.name, " ", "-"), adapter)
				}},
			}
			for _, write := range writes {
				for adapter, invoke := range map[string]opInvoker{"rest": restInvoker(e, secret), "mcp": mcpInvoker(e, secret)} {
					out := invoke(t, write.name, write.args(adapter))
					if write.allowed && out.class != "ok" {
						t.Errorf("%s permitted %s = %s", adapter, write.name, out.class)
					}
					if !write.allowed && out.class != "denied" {
						t.Errorf("%s cross-class %s = %s, want denied", adapter, write.name, out.class)
					}
				}
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
			for _, path := range []string{"/api/op/run_sql", "/api/sql/run"} {
				rec := postJSON(t, e, path, `{"sql":"SELECT count(*) FROM events"}`, headers)
				if rec.Code == http.StatusOK {
					t.Fatalf("%s credential unexpectedly ran SQL on %s: %s", name, path, rec.Body.String())
				}
			}
			mcpRec := postJSON(t, e, "/mcp", mcpCall("run_sql", `{"sql":"SELECT count(*) FROM events"}`), headers)
			if strings.Contains(mcpRec.Body.String(), `"structuredContent"`) {
				t.Fatalf("credential unexpectedly ran SQL over MCP: %s", mcpRec.Body.String())
			}
		})
	}
}

func TestQuerySecurityIsolationAndExternalIODenials(t *testing.T) {
	s := openRequiredAppTestStore(t)
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
	registerMcpRoutes(e, s, nil, nil)

	// A credential's project is server-owned. Forging the query parameter must
	// still return the caller's four rows. A forged project_id in a typed body
	// is either rejected or ignored, but can never retarget the call.
	for _, path := range []string{
		"/api/sql/run?project_id=" + foreign.Project.ID,
		"/api/op/run_sql?project_id=" + foreign.Project.ID,
	} {
		rec := postJSON(t, e, path, `{"sql":"SELECT count(*) AS n FROM events"}`, map[string]string{"Authorization": "Bearer " + secret})
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"n":4`) {
			t.Fatalf("forged project query escaped caller scope on %s: %d %s", path, rec.Code, rec.Body.String())
		}
	}
	legacyForgedBody := fmt.Sprintf(`{"sql":"SELECT count(*) AS n FROM events","project_id":%q}`, foreign.Project.ID)
	legacyRec := postJSON(t, e, "/api/sql/run?project_id="+foreign.Project.ID, legacyForgedBody, map[string]string{"Authorization": "Bearer " + secret})
	if legacyRec.Code != http.StatusOK || !strings.Contains(legacyRec.Body.String(), `"n":4`) {
		t.Fatalf("legacy forged body project_id escaped caller scope: %d %s", legacyRec.Code, legacyRec.Body.String())
	}
	for adapter, invoke := range map[string]opInvoker{"rest": restInvoker(e, secret), "mcp": mcpInvoker(e, secret)} {
		forged := fmt.Sprintf(`{"sql":"SELECT count(*) AS n FROM events","project_id":%q}`, foreign.Project.ID)
		out := invoke(t, "run_sql", forged)
		if out.class == "ok" && !strings.Contains(rowsJSON(t, out.raw), `"n":4`) {
			t.Errorf("%s forged body project_id escaped caller scope: %s", adapter, out.raw)
		} else if out.class != "ok" && out.class != "invalid" {
			t.Errorf("%s forged body project_id = %s, want scoped success or typed invalid", adapter, out.class)
		}
	}

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
		for _, path := range []string{"/api/op/run_sql", "/api/sql/run"} {
			rec := postJSON(t, e, path, string(body), map[string]string{"Authorization": "Bearer " + secret})
			if rec.Code == http.StatusOK {
				t.Errorf("external I/O SQL succeeded on %s: %s", path, sql)
			}
			if strings.Contains(rec.Body.String(), "secret@example") || strings.Contains(rec.Body.String(), "query-access-secret") {
				t.Errorf("%s denial echoed a credential/path: %s", path, rec.Body.String())
			}
		}
		if out := mcpInvoker(e, secret)(t, "run_sql", string(body)); out.class == "ok" {
			t.Errorf("external I/O SQL succeeded over MCP: %s", sql)
		}
		mcpRec := postJSON(t, e, "/mcp", mcpCall("run_sql", string(body)), map[string]string{"Authorization": "Bearer " + secret})
		if strings.Contains(mcpRec.Body.String(), "secret@example") || strings.Contains(mcpRec.Body.String(), "query-access-secret") {
			t.Errorf("MCP denial echoed a credential/path: %s", mcpRec.Body.String())
		}
	}
}
