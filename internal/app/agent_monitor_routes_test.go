package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
	storage "github.com/lohi-ai/agentray/internal/dataplane/store"
)

func TestInstalledAgentMonitorRoutesResolveSetupAndMonitoring(t *testing.T) {
	s := openRequiredAppTestStore(t)
	ctx := context.Background()
	storage.SetPackCatalog(marketplacePresets, marketplacePresetBySlug)

	boot, err := s.CreateAccount(ctx,
		fmt.Sprintf("agent-monitor-%d@test.local", time.Now().UnixNano()),
		"Agent monitor", "password-123", "Monitor workspace", "Monitor project",
	)
	if err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}
	agent, err := s.InstallAgentPreset(ctx, boot.User.ID, boot.Project.ID, "insight-digest")
	if err != nil {
		t.Fatalf("InstallAgentPreset: %v", err)
	}
	_, sessionToken, err := s.CreateUserSession(ctx, boot.User.ID, time.Hour)
	if err != nil {
		t.Fatalf("CreateUserSession: %v", err)
	}

	e := echo.New()
	registerAgentMonitorRoutes(e, s)
	get := func(path string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, path+"?project_id="+boot.Project.ID, nil)
		req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sessionToken})
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)
		return rec
	}

	overview := get("/api/agents/monitor")
	detail := get("/api/agents/" + agent.ID + "/monitor")
	t.Logf("GET /api/agents/monitor = %d %s", overview.Code, overview.Body.String())
	t.Logf("GET /api/agents/%s/monitor (setup dependency) = %d %s", agent.ID, detail.Code, detail.Body.String())
	if detail.Code != http.StatusOK {
		t.Errorf("GET /api/agents/:id/monitor = %d %s", detail.Code, detail.Body.String())
	}
	if overview.Code != http.StatusOK {
		t.Fatalf("GET /api/agents/monitor = %d %s", overview.Code, overview.Body.String())
	}
	var list struct {
		Agents []storage.AgentMonitorRow `json:"agents"`
	}
	if err := json.Unmarshal(overview.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode monitor overview: %v", err)
	}
	found := false
	for _, row := range list.Agents {
		if row.ID == agent.ID && row.PresetSlug == "insight-digest" {
			found = true
		}
	}
	if !found {
		t.Fatalf("installed agent %s absent from monitor overview: %+v", agent.ID, list.Agents)
	}

	if detail.Code != http.StatusOK {
		t.Fatalf("GET /api/agents/:id/monitor = %d %s", detail.Code, detail.Body.String())
	}
	var got struct {
		Agent storage.AgentMonitorRow `json:"agent"`
	}
	if err := json.Unmarshal(detail.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode monitor detail: %v", err)
	}
	if got.Agent.ID != agent.ID || got.Agent.Name != agent.Name {
		t.Fatalf("monitor detail agent = %+v, want installed agent %+v", got.Agent.Agent, agent)
	}
	// A settled provider failure must appear in both rollups, with a useful
	// explanation and without borrowing the default agent's runs.
	failedID, err := s.CreateAgentRun(ctx, boot.Project.ID, agent.ID, "scheduled", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.FinishAgentRun(ctx, failedID, "error", "provider deadline exceeded", 12, 3, 0.02, false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateAgentRun(ctx, boot.Project.ID, agent.ID, "scheduled", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateAgentRun(ctx, boot.Project.ID, boot.Project.ID, "scheduled", ""); err != nil {
		t.Fatal(err)
	}
	detail = get("/api/agents/" + agent.ID + "/monitor")
	if detail.Code != http.StatusOK {
		t.Fatalf("monitor with failed run = %d %s", detail.Code, detail.Body.String())
	}
	if err := json.Unmarshal(detail.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	m := got.Agent
	if m.RunCount != 2 || m.RunningCount != 1 || m.ErrorCount != 1 || m.TokenInput != 12 || m.TokenOutput != 3 || m.CostUSD != 0.02 || m.LastError != "provider deadline exceeded" || m.LastRunAt == nil {
		t.Fatalf("incorrect failed-run monitor aggregate: %+v", m)
	}
	overview = get("/api/agents/monitor")
	if overview.Code != http.StatusOK {
		t.Fatalf("overview with failed run = %d %s", overview.Code, overview.Body.String())
	}
	if err := json.Unmarshal(overview.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	for _, row := range list.Agents {
		if row.ID == agent.ID && (row.RunCount != m.RunCount || row.ErrorCount != m.ErrorCount || row.LastError != m.LastError) {
			t.Fatalf("overview/detail disagree: %+v / %+v", row, m)
		}
	}
	t.Logf("monitor after provider failure: status=%d runs=%d running=%d errors=%d last_error=%q", detail.Code, m.RunCount, m.RunningCount, m.ErrorCount, m.LastError)
	missing := get("/api/agents/00000000-0000-4000-8000-000000000000/monitor")
	if missing.Code != http.StatusNotFound {
		t.Fatalf("missing agent = %d %s, want 404", missing.Code, missing.Body.String())
	}
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/agents/monitor?project_id="+boot.Project.ID, nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated monitor = %d, want 401", rec.Code)
	}
	t.Logf("nonhappy monitoring: missing=%d unauthenticated=%d", missing.Code, rec.Code)
}
