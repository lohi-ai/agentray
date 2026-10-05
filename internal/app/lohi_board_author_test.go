package app

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/lohi-ai/agentray/agentcore"
	storage "github.com/lohi-ai/agentray/internal/dataplane/store"
	runtime "github.com/lohi-ai/agentray/internal/runtime"
	"github.com/lohi-ai/agentray/internal/shared/opcore"
	"github.com/lohi-ai/agentray/internal/workloads"
)

// TestInstalledDataAnalystBoardAuthorJourney is the product-level seam that the
// Lohi evidence-pack acceptance originally missed. It installs the marketplace
// preset, proves the browser session retains its board-author grant, and then
// creates a board and chart through the same MCP adapter an external client
// uses. The two negative cases keep least privilege explicit: an investigator
// credential and an investigate-only runtime never receive authoring tools.
func TestInstalledDataAnalystBoardAuthorJourney(t *testing.T) {
	s := openRequiredAppTestStore(t)
	ctx := context.Background()
	storage.SetPackCatalog(marketplacePresets, marketplacePresetBySlug)
	t.Cleanup(func() { storage.SetPackCatalog(marketplacePresets, marketplacePresetBySlug) })

	stamp := time.Now().UnixNano()
	boot, err := s.CreateAccount(ctx, fmt.Sprintf("lohi-board-author-%d@test.local", stamp), "Board Author", "password-123", "ws", "lohi")
	if err != nil {
		t.Fatal(err)
	}
	agent, err := s.InstallAgentPreset(ctx, boot.User.ID, boot.Project.ID, "data-analyst")
	if err != nil {
		t.Fatalf("install Data Analyst: %v", err)
	}
	skills, err := s.ListAgentSkills(ctx, boot.User.ID, boot.Project.ID, agent.ID)
	if err != nil {
		t.Fatalf("list installed skills: %v", err)
	}
	var lohiSkill storage.AgentSkill
	for _, skill := range skills {
		if skill.Name == workloads.LohiEvidenceVersion {
			lohiSkill = skill
		}
	}
	if lohiSkill.ID == "" || lohiSkill.Body != workloads.LohiEvidenceSkill().Body {
		t.Fatalf("portable skill was not installed verbatim: id=%q body_equal=%v", lohiSkill.ID, lohiSkill.Body == workloads.LohiEvidenceSkill().Body)
	}
	lohiSkill.Body += "\n\nOperator note: preserve this local edit."
	if _, err := s.UpsertAgentSkill(ctx, boot.User.ID, boot.Project.ID, agent.ID, lohiSkill); err != nil {
		t.Fatalf("edit installed skill: %v", err)
	}
	reinstalled, err := s.InstallAgentPreset(ctx, boot.User.ID, boot.Project.ID, "data-analyst")
	if err != nil {
		t.Fatalf("reinstall Data Analyst: %v", err)
	}
	if reinstalled.ID != agent.ID {
		t.Fatalf("reinstall created agent %s, want existing %s", reinstalled.ID, agent.ID)
	}
	skills, err = s.ListAgentSkills(ctx, boot.User.ID, boot.Project.ID, agent.ID)
	if err != nil {
		t.Fatalf("list reinstalled skills: %v", err)
	}
	for _, skill := range skills {
		if skill.Name == workloads.LohiEvidenceVersion && !strings.Contains(skill.Body, "preserve this local edit") {
			t.Fatal("reinstall overwrote the operator-edited Lohi skill")
		}
	}
	caps, err := s.AgentCapabilitiesForRun(ctx, boot.Project.ID, agent.ID)
	if err != nil {
		t.Fatalf("resolve installed capabilities: %v", err)
	}
	installedTools := runtime.ScopeToolNames(runtime.ScopesFromMap(caps))
	slices.Sort(installedTools)
	for _, name := range []string{runtime.ToolRunSQL, runtime.ToolCreateDashboard, runtime.ToolCreateChart} {
		if !slices.Contains(installedTools, name) {
			t.Fatalf("installed Data Analyst lacks %q: %v", name, installedTools)
		}
	}
	t.Logf("installed preset agent=%s tools include run_sql=%v create_dashboard=%v create_chart=%v",
		agent.ID, slices.Contains(installedTools, runtime.ToolRunSQL), slices.Contains(installedTools, runtime.ToolCreateDashboard), slices.Contains(installedTools, runtime.ToolCreateChart))

	_, sessionToken, err := s.CreateUserSession(ctx, boot.User.ID, time.Hour)
	if err != nil {
		t.Fatalf("create board-author session: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/agent/chat?project_id="+boot.Project.ID, nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sessionToken})
	chatContext := echo.New().NewContext(req, httptest.NewRecorder())
	_, authorizedProject, err := authProject(chatContext, s)
	if err != nil {
		t.Fatalf("authorize chat session: %v", err)
	}
	if authorizedProject.Role != "owner" || !sessionAllowsWrite(authorizedProject) {
		t.Fatalf("board-author chat session resolved role=%q demo=%v write=%v; want owner write authorization",
			authorizedProject.Role, authorizedProject.IsDemo, sessionAllowsWrite(authorizedProject))
	}
	t.Logf("chat session role=%s demo=%v board_authorized=%v", authorizedProject.Role, authorizedProject.IsDemo, sessionAllowsWrite(authorizedProject))

	_, investigatorSecret, err := s.CreateProjectCredential(ctx, boot.User.ID, boot.Project.ID, "investigator", []string{"analytics:read", "sources:read"})
	if err != nil {
		t.Fatal(err)
	}
	_, authorSecret, err := s.CreateProjectCredential(ctx, boot.User.ID, boot.Project.ID, "board-author", []string{"analytics:read", "sources:read", "dashboards:write"})
	if err != nil {
		t.Fatal(err)
	}
	e := mountServerRoutes(t, s)
	registerMcpRoutes(e, s, nil, nil)
	mixedHeaders := func(secret string) map[string]string {
		return map[string]string{
			"Authorization": "Bearer " + secret,
			"Cookie":        sessionCookieName + "=" + sessionToken,
		}
	}

	investigatorTools := listedMCPTools(t, e, investigatorSecret)
	if investigatorTools[runtime.ToolCreateDashboard] || investigatorTools[runtime.ToolCreateChart] {
		t.Fatalf("investigator credential received authoring tools: %v", investigatorTools)
	}
	dashboardsBeforeRestricted, err := s.ListDashboards(ctx, boot.Project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if out := mcpInvokerWithHeaders(e, mixedHeaders(investigatorSecret))(t, runtime.ToolCreateDashboard, `{"name":"must not exist"}`); out.class != "denied" {
		t.Fatalf("investigator create_dashboard = %s %s, want denied", out.class, out.raw)
	} else {
		dashboards, err := s.ListDashboards(ctx, boot.Project.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(dashboards) != len(dashboardsBeforeRestricted) || slices.ContainsFunc(dashboards, func(d storage.Dashboard) bool { return d.Name == "must not exist" }) {
			t.Fatalf("restricted mixed credential persisted dashboards: %+v", dashboards)
		}
		t.Logf("owner cookie + restricted Bearer scopes=analytics:read,sources:read advertised create_dashboard=%v create_chart=%v direct_create=%s persisted_dashboard_delta=%d",
			investigatorTools[runtime.ToolCreateDashboard], investigatorTools[runtime.ToolCreateChart], out.class, len(dashboards)-len(dashboardsBeforeRestricted))
	}

	authorTools := listedMCPTools(t, e, authorSecret)
	for _, name := range []string{runtime.ToolRunSQL, runtime.ToolListSources, runtime.ToolCreateDashboard, runtime.ToolCreateChart} {
		if !authorTools[name] {
			t.Fatalf("board-author MCP session lacks %q: %v", name, authorTools)
		}
	}
	inv := mcpInvokerWithHeaders(e, mixedHeaders(authorSecret))
	dashboard := expectOp(t, "mcp", inv, runtime.ToolCreateDashboard,
		`{"name":"Lohi Revenue Post-mortem","description":"lohi-evidence-v1 governed figures"}`, "ok")
	dashboardID, _ := field(t, dashboard, "id").(string)
	if dashboardID == "" {
		t.Fatalf("create_dashboard returned no id: %s", dashboard.raw)
	}
	chart := expectOp(t, "mcp", inv, runtime.ToolCreateChart,
		fmt.Sprintf(`{"dashboard_id":%q,"name":"Daily Gross Topups (lohi-evidence-v1/R01)","kind":"line","sql":"SELECT '2026-10-02' AS date, 0 AS value","x_field":"date","y_field":"value"}`, dashboardID), "ok")
	chartID, _ := field(t, chart, "id").(string)
	if chartID == "" {
		t.Fatalf("create_chart returned no id: %s", chart.raw)
	}
	persistedCharts, err := s.ListCharts(ctx, boot.Project.ID, dashboardID)
	if err != nil {
		t.Fatalf("list persisted charts: %v", err)
	}
	if len(persistedCharts) != 1 || persistedCharts[0].ID != chartID || persistedCharts[0].DashboardID != dashboardID {
		t.Fatalf("persisted charts = %+v, want chart %s on dashboard %s", persistedCharts, chartID, dashboardID)
	}
	persistedDashboards, err := s.ListDashboards(ctx, boot.Project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(persistedDashboards) != len(dashboardsBeforeRestricted)+1 || !slices.ContainsFunc(persistedDashboards, func(d storage.Dashboard) bool { return d.ID == dashboardID }) {
		t.Fatalf("persisted dashboards = %+v, want dashboard %s", persistedDashboards, dashboardID)
	}
	t.Logf("owner cookie + dashboards:write Bearer created and persisted dashboard=%s chart=%s", dashboardID, chartID)

	investigateOnly := runtime.ScopeToolNames(runtime.Scopes{DataQuality: true})
	slices.Sort(investigateOnly)
	if slices.Contains(investigateOnly, runtime.ToolCreateDashboard) || slices.Contains(investigateOnly, runtime.ToolCreateChart) {
		t.Fatalf("investigate-only runtime received authoring tools: %v", investigateOnly)
	}
	for _, name := range []string{runtime.ToolCreateDashboard, runtime.ToolCreateChart} {
		if decision := agentcore.NewAllowList(investigateOnly...).Allow(ctx, agentcore.ToolCall{Name: name}); decision.Allow {
			t.Fatalf("investigate-only policy allowed %s", name)
		}
	}
	t.Logf("investigate-only runtime advertised create_dashboard=%v create_chart=%v tools=%v",
		slices.Contains(investigateOnly, runtime.ToolCreateDashboard), slices.Contains(investigateOnly, runtime.ToolCreateChart), investigateOnly)
}

// TestAgentRunCredentialPrecedenceOverOwnerSession pins both directions of the
// chat authority intersection. The cookie owns conversation history, while an
// explicitly supplied Bearer remains the selected authority for runtime tools.
func TestAgentRunCredentialPrecedenceOverOwnerSession(t *testing.T) {
	s := openRequiredAppTestStore(t)
	ctx := context.Background()
	stamp := time.Now().UnixNano()
	boot, err := s.CreateAccount(ctx, fmt.Sprintf("chat-credential-precedence-%d@test.local", stamp), "Owner", "password-123", "ws", "project")
	if err != nil {
		t.Fatal(err)
	}
	_, sessionToken, err := s.CreateUserSession(ctx, boot.User.ID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	_, investigatorSecret, err := s.CreateProjectCredential(ctx, boot.User.ID, boot.Project.ID, "investigator", []string{"analytics:read", "sources:read"})
	if err != nil {
		t.Fatal(err)
	}
	_, authorSecret, err := s.CreateProjectCredential(ctx, boot.User.ID, boot.Project.ID, "board-author", []string{"analytics:read", "sources:read", "dashboards:write"})
	if err != nil {
		t.Fatal(err)
	}

	for _, guarded := range []bool{false, true} {
		e := echo.New()
		if guarded {
			e.Use(demoWriteGuard(s, nil))
		}
		var gotGuardReadOnly bool
		var gotRunReadOnly bool
		var gotProject storage.Project
		var gotPrincipal opcore.Principal
		e.POST("/api/agent/chat", func(c echo.Context) error {
			auth, project, err := authProject(c, s)
			if err != nil {
				return err
			}
			gotProject = project
			gotPrincipal = auth.Principal
			gotGuardReadOnly = readOnlyCaller(c)
			gotRunReadOnly = agentRunReadOnly(c, auth)
			return c.NoContent(http.StatusNoContent)
		})

		for _, tc := range []struct {
			name          string
			bearer        string
			wantGuardOnly bool
			wantReadOnly  bool
		}{
			{name: "owner_session", wantReadOnly: false},
			{name: "owner_cookie_plus_investigator_bearer", bearer: investigatorSecret, wantGuardOnly: true, wantReadOnly: true},
			{name: "owner_cookie_plus_dashboard_author_bearer", bearer: authorSecret, wantReadOnly: false},
		} {
			t.Run(fmt.Sprintf("guarded=%v/%s", guarded, tc.name), func(t *testing.T) {
				gotGuardReadOnly = false
				gotRunReadOnly = false
				gotProject = storage.Project{}
				req := httptest.NewRequest(http.MethodPost, "/api/agent/chat?project_id="+boot.Project.ID, nil)
				req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sessionToken})
				if tc.bearer != "" {
					req.Header.Set("Authorization", "Bearer "+tc.bearer)
				}
				rec := httptest.NewRecorder()
				e.ServeHTTP(rec, req)
				if rec.Code != http.StatusNoContent {
					t.Fatalf("chat auth status=%d body=%s", rec.Code, rec.Body.String())
				}
				if gotProject.Role != "owner" {
					t.Fatalf("history owner role=%q, want owner", gotProject.Role)
				}
				if gotGuardReadOnly != (guarded && tc.wantGuardOnly) {
					t.Fatalf("write guard read-only=%v, want %v", gotGuardReadOnly, guarded && tc.wantGuardOnly)
				}
				if gotRunReadOnly != tc.wantReadOnly {
					t.Fatalf("agent run ReadOnly=%v, want %v", gotRunReadOnly, tc.wantReadOnly)
				}
				wantKind := opcore.CredSession
				if tc.bearer != "" {
					wantKind = opcore.CredManagement
					if gotProject.APIKey != "" {
						t.Fatal("membership reload exposed capture key to management credential")
					}
				}
				if gotPrincipal.Kind != wantKind {
					t.Fatalf("selected principal=%s, want %s", gotPrincipal.Kind, wantKind)
				}
				names := runtime.ScopeToolNames(runtime.Scopes{AnalyzeBuild: true})
				if gotRunReadOnly {
					names = runtime.ReadOnlyToolNames(names)
				}
				for _, name := range []string{runtime.ToolCreateDashboard, runtime.ToolCreateChart} {
					if allowed := agentcore.NewAllowList(names...).Allow(ctx, agentcore.ToolCall{Name: name}).Allow; allowed == tc.wantReadOnly {
						t.Fatalf("runtime gate %s allowed=%v, ReadOnly=%v", name, allowed, tc.wantReadOnly)
					}
				}
				t.Logf("history_role=%s selected_principal=%s guard_read_only=%v agent_run_read_only=%v writes_allowed=%v", gotProject.Role, gotPrincipal.Kind, gotGuardReadOnly, gotRunReadOnly, !gotRunReadOnly)
			})
		}
	}
}

// The chat, parked-answer, and durable-conversation paths must all use the
// same credential-aware decision. This source seam prevents one path from
// quietly reverting to the ambient membership check while the matrix above
// continues to exercise another.
func TestEveryAgentTurnUsesCredentialAwareReadOnly(t *testing.T) {
	src, err := os.ReadFile("agent_routes.go")
	if err != nil {
		t.Fatal(err)
	}
	credentialAware := regexp.MustCompile(`ReadOnly:\s+agentRunReadOnly\(c, (auth|ctx)\)`)
	if got := len(credentialAware.FindAll(src, -1)); got != 3 {
		t.Fatalf("credential-aware ReadOnly wiring count=%d, want 3 (chat, answer, conversation)", got)
	}
	if regexp.MustCompile(`ReadOnly:\s+!sessionAllowsWrite\(project\)`).Match(src) {
		t.Fatal("an agent turn still derives authority only from ambient session membership")
	}
	controlAuthority := regexp.MustCompile(`LiveAuthority\{CanWrite:\s*!agentRunReadOnly\(c, (auth|ctx)\)\}`)
	if got := len(controlAuthority.FindAll(src, -1)); got != 2 {
		t.Fatalf("live chat and conversation control authority count=%d, want 2", got)
	}
	if got := strings.Count(string(src), `control == agentruntime.LiveControlDenied`); got != 2 {
		t.Fatalf("live chat and conversation denial checks=%d, want 2", got)
	}
}

func TestAgentRunHonorsReadOnlyGuardWithAuthorGrant(t *testing.T) {
	c := echo.New().NewContext(httptest.NewRequest(http.MethodPost, "/api/agent/chat", nil), httptest.NewRecorder())
	auth := authContext{Principal: opcore.Principal{
		Kind: opcore.CredManagement, Grants: []opcore.Access{opcore.AccessDashboardsWrite},
	}}
	if agentRunReadOnly(c, auth) {
		t.Fatal("explicit author grant was withheld")
	}
	c.Set(demoReadOnlyCallerKey, true)
	if !agentRunReadOnly(c, auth) {
		t.Fatal("explicit author grant overrode the read-only guard")
	}
}
