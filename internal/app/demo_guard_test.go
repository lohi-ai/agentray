package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/lohi-ai/agentray/internal/dataplane/store"
)

// The write guard is authorization code, so it is tested against the real
// route table (every mutating path this app registers) and a fake store, rather
// than against a handful of hand-picked paths. Two failures are what these
// tests exist to catch:
//
//   - a viewer of the shared demo being able to change someone else's site, and
//   - a viewer of the demo losing write access to their OWN workspace, which
//     would lock every signed-up user out of their account.
//
// Both are one wrong branch away from each other, which is why they are always
// asserted together.

// --- fake store -----------------------------------------------------------

const (
	demoWS      = "11111111-1111-1111-1111-111111111111"
	demoProject = "22222222-2222-2222-2222-222222222222"
	demoSibling = "22222222-2222-2222-2222-22222222bbbb"
	homeWS      = "33333333-3333-3333-3333-333333333333"
	homeProject = "44444444-4444-4444-4444-444444444444"

	visitorID    = "55555555-5555-5555-5555-555555555555"
	operatorID   = "66666666-6666-6666-6666-666666666666"
	visitorToken = "session-visitor"
	operatorTok  = "session-operator"
	demoKey      = "agentray_demo_public_key"
)

type fakeGuardStore struct {
	demoWorkspace string
	demoProjectID string
	users         map[string]storage.User      // session token -> user
	projects      map[string]storage.Project   // project id -> project (role filled per caller)
	members       map[string]map[string]string // workspace id -> user id -> role
	apiKeys       map[string]string            // api key -> project id
	defaultProj   map[string]string            // user id -> project id
	used          map[string]int               // user id -> demo runs started today
}

func newFakeGuardStore() *fakeGuardStore {
	return &fakeGuardStore{
		demoWorkspace: demoWS,
		demoProjectID: demoProject,
		users: map[string]storage.User{
			visitorToken: {ID: visitorID, Email: "visitor@example.com"},
			operatorTok:  {ID: operatorID, Email: "operator@example.com"},
		},
		projects: map[string]storage.Project{
			demoProject: {ID: demoProject, WorkspaceID: demoWS, Name: "Demo site"},
			demoSibling: {ID: demoSibling, WorkspaceID: demoWS, Name: "Demo sibling"},
			homeProject: {ID: homeProject, WorkspaceID: homeWS, Name: "My project"},
		},
		members: map[string]map[string]string{
			// The visitor is a viewer of the demo and the OWNER of their own
			// workspace. That pairing is the whole point of the fixture.
			demoWS: {visitorID: storage.DemoViewerRole, operatorID: "owner"},
			homeWS: {visitorID: "owner"},
		},
		apiKeys:     map[string]string{demoKey: demoProject},
		defaultProj: map[string]string{visitorID: homeProject, operatorID: demoProject},
		used:        map[string]int{},
	}
}

func (f *fakeGuardStore) DemoWorkspaceID() string { return f.demoWorkspace }
func (f *fakeGuardStore) DemoProjectID() string   { return f.demoProjectID }

func (f *fakeGuardStore) UserBySessionToken(_ context.Context, token string) (storage.User, storage.UserSession, error) {
	user, ok := f.users[token]
	if !ok {
		return storage.User{}, storage.UserSession{}, fmt.Errorf("no session")
	}
	return user, storage.UserSession{UserID: user.ID}, nil
}

func (f *fakeGuardStore) ProjectByAPIKey(_ context.Context, key string) (storage.Project, error) {
	id, ok := f.apiKeys[key]
	if !ok {
		return storage.Project{}, fmt.Errorf("invalid api key")
	}
	return f.projects[id], nil
}

func (f *fakeGuardStore) ProjectByIDForUser(_ context.Context, userID, projectID string) (storage.Project, error) {
	project, ok := f.projects[projectID]
	if !ok {
		return storage.Project{}, fmt.Errorf("no project")
	}
	role := f.members[project.WorkspaceID][userID]
	if role == "" {
		return storage.Project{}, fmt.Errorf("not a member")
	}
	project.Role = role
	return project, nil
}

func (f *fakeGuardStore) DefaultProjectForUser(_ context.Context, userID string) (storage.Project, error) {
	id, ok := f.defaultProj[userID]
	if !ok {
		return storage.Project{}, fmt.Errorf("no project")
	}
	return f.ProjectByIDForUser(context.Background(), userID, id)
}

func (f *fakeGuardStore) WorkspaceRoleForUser(_ context.Context, userID, workspaceID string) (string, error) {
	return f.members[workspaceID][userID], nil
}

// ConsumeDemoAgentRun mirrors the store's contract: it counts at the START and
// answers Allowed=false once the ceiling is reached.
func (f *fakeGuardStore) ConsumeDemoAgentRun(_ context.Context, userID string, limit int) (storage.DemoRunQuota, error) {
	quota := storage.DemoRunQuota{Limit: limit, ResetsAt: time.Now().UTC().Add(time.Hour)}
	if limit <= 0 || f.used[userID] >= limit {
		quota.Used = limit
		return quota, nil
	}
	f.used[userID]++
	quota.Used, quota.Allowed = f.used[userID], true
	return quota, nil
}

// RefundDemoAgentRun mirrors the store's floor-at-zero, so a test that refunds
// more than it claimed cannot mint free asks the real ledger would not.
func (f *fakeGuardStore) RefundDemoAgentRun(_ context.Context, userID string) error {
	if f.used[userID] > 0 {
		f.used[userID]--
	}
	return nil
}

// --- the route table under test -------------------------------------------

// mutatingRoutes is every mutating route this app registers, as (method,
// pattern). It is written out rather than derived so the guard is tested
// against a list a person has read; TestTheRouteTableMatchesTheSource then
// scans the package's own source and fails if a route was added to the server
// and not to this list.
var mutatingRoutes = [][2]string{
	{http.MethodPost, "/api/auth/signup"},
	{http.MethodPost, "/api/auth/login"},
	{http.MethodPost, "/api/auth/logout"},
	{http.MethodPut, "/api/users/me"},
	{http.MethodPost, "/api/workspaces"},
	{http.MethodPut, "/api/workspaces/:workspace_id"},
	{http.MethodPost, "/api/workspaces/:workspace_id/upgrade-request"},
	{http.MethodPost, "/api/workspaces/:workspace_id/members"},
	{http.MethodPut, "/api/workspaces/:workspace_id/members/:user_id"},
	{http.MethodDelete, "/api/workspaces/:workspace_id/members/:user_id"},
	{http.MethodPost, "/api/workspaces/:workspace_id/projects"},
	{http.MethodPost, "/api/projects"},
	{http.MethodPut, "/api/projects/:project_id"},
	{http.MethodPost, "/api/projects/:project_id/rotate-key"},
	{http.MethodPost, "/api/templates/:template_id/apply"},
	{http.MethodPost, "/api/templates/:template_id/charts/:chart_id/clone"},
	{http.MethodPost, "/api/cohorts/audiences"},
	{http.MethodPut, "/api/cohorts/audiences/:audience_id"},
	{http.MethodDelete, "/api/cohorts/audiences/:audience_id"},
	{http.MethodPut, "/api/subscription/mapping"},
	{http.MethodPost, "/api/saved-queries"},
	{http.MethodPost, "/api/saved-queries/:query_id/run"},
	{http.MethodPatch, "/api/saved-queries/:query_id"},
	{http.MethodDelete, "/api/saved-queries/:query_id"},
	{http.MethodPost, "/api/sql/run"},
	{http.MethodPost, "/api/dashboards"},
	{http.MethodPut, "/api/dashboards/:dashboard_id"},
	{http.MethodDelete, "/api/dashboards/:dashboard_id"},
	{http.MethodPost, "/api/dashboards/:dashboard_id/charts"},
	{http.MethodPut, "/api/dashboards/:dashboard_id/charts/order"},
	{http.MethodPut, "/api/charts/:chart_id"},
	{http.MethodDelete, "/api/charts/:chart_id"},
	{http.MethodPost, "/api/alerts/rules"},
	{http.MethodPut, "/api/alerts/rules/:rule_id"},
	{http.MethodDelete, "/api/alerts/rules/:rule_id"},
	{http.MethodPost, "/api/alerts/channels"},
	{http.MethodDelete, "/api/alerts/channels/:channel_id"},
	{http.MethodPost, "/api/connectors"},
	{http.MethodDelete, "/api/connectors/:connector_id"},
	{http.MethodPost, "/api/connectors/:connector_id/test"},
	{http.MethodPost, "/api/connectors/:connector_id/syncs"},
	{http.MethodPost, "/api/connectors/:connector_id/syncs/draft"},
	{http.MethodPut, "/api/connector-syncs/:sync_id"},
	{http.MethodDelete, "/api/connector-syncs/:sync_id"},
	{http.MethodPost, "/api/connector-syncs/:sync_id/run"},
	{http.MethodPost, "/api/teams"},
	{http.MethodPut, "/api/teams/:team_id"},
	{http.MethodDelete, "/api/teams/:team_id"},
	{http.MethodPut, "/api/teams/:team_id/members/:agent_id"},
	{http.MethodDelete, "/api/teams/:team_id/members/:agent_id"},
	{http.MethodPost, "/api/teams/:team_id/cards"},
	{http.MethodPut, "/api/teams/:team_id/cards/:card_id"},
	{http.MethodDelete, "/api/teams/:team_id/cards/:card_id"},
	{http.MethodPost, "/api/validation/tests"},
	{http.MethodPost, "/api/validation/tests/:id/commit"},
	{http.MethodPost, "/api/validation/tests/:id/decide"},
	{http.MethodDelete, "/api/validation/waitlist/:id"},
	{http.MethodPost, "/api/operations/:id/run"},
	{http.MethodPut, "/api/agent/config"},
	{http.MethodPut, "/api/workspace/models"},
	{http.MethodPost, "/api/workspace/models/test"},
	{http.MethodPost, "/api/workspace/providers"},
	{http.MethodPut, "/api/workspace/providers/:id"},
	{http.MethodDelete, "/api/workspace/providers/:id"},
	{http.MethodPut, "/api/agent/capabilities"},
	{http.MethodPut, "/api/agent/task-tiers"},
	{http.MethodPut, "/api/agent/definition"},
	{http.MethodPost, "/api/agent/definition/generate"},
	{http.MethodPost, "/api/agent/skills"},
	{http.MethodPut, "/api/agent/skills/:id"},
	{http.MethodDelete, "/api/agent/skills/:id"},
	{http.MethodPost, "/api/agent/skills/:id/approve"},
	{http.MethodPut, "/api/agent/secrets/:name"},
	{http.MethodDelete, "/api/agent/secrets/:name"},
	{http.MethodPost, "/api/agent/agents"},
	{http.MethodPost, "/api/marketplace/agents/:slug/install"},
	{http.MethodPut, "/api/agent/agents/:id"},
	{http.MethodDelete, "/api/agent/agents/:id"},
	{http.MethodPost, "/api/agent/agents/:id/grant"},
	{http.MethodDelete, "/api/agent/agents/:id/grant"},
	{http.MethodPut, "/api/agent/tools/:name"},
	{http.MethodDelete, "/api/agent/tools/:name"},
	{http.MethodPut, "/api/agent/delegates/:id"},
	{http.MethodDelete, "/api/agent/delegates/:id"},
	{http.MethodPut, "/api/agent/budgets"},
	{http.MethodDelete, "/api/agent/budgets/:period"},
	{http.MethodDelete, "/api/agent/memory/:id"},
	{http.MethodPost, "/api/agent/recommendations/:id/ack"},
	{http.MethodPost, "/api/agent/chat"},
	{http.MethodPost, "/api/agent/chat/steer"},
	{http.MethodPost, "/api/agent/chat/cancel"},
	{http.MethodPost, "/api/agent/conversations"},
	{http.MethodPost, "/api/agent/conversations/:id/messages"},
	{http.MethodPost, "/api/agent/conversations/:id/messages/:entry_id/edit"},
	{http.MethodPost, "/api/agent/conversations/:id/messages/:entry_id/regenerate"},
	{http.MethodPost, "/api/agent/run/:run_id/resume"},
	{http.MethodPost, "/api/agent/run"},
	{http.MethodPost, "/api/agent/triggers"},
	{http.MethodPut, "/api/agent/triggers/:id"},
	{http.MethodDelete, "/api/agent/triggers/:id"},
	{http.MethodPost, "/api/agent/hook/:token"},
	{http.MethodPost, "/api/agent/lab/cases"},
	{http.MethodDelete, "/api/agent/lab/cases/:id"},
	{http.MethodPost, "/api/agent/lab/cases/:id/verdict"},
	{http.MethodPost, "/api/agent/lab/test"},
	{http.MethodPost, "/api/agent/lab/explain"},
	{http.MethodPost, "/api/agent/lab/explain/:run_id/advance"},
	{http.MethodPost, "/api/agent/lab/explain/:run_id/stop"},
	{http.MethodPost, "/api/agent/lab/explain/:run_id/steer"},
	{http.MethodPost, "/api/op/create_chart"},
	{http.MethodPost, "/api/op/run_sql"},
	{http.MethodPost, "/mcp"},
	{http.MethodPost, "/waitlist"},
	{http.MethodPost, "/waitlist/unsubscribe"},
	{http.MethodPost, "/capture"},
	{http.MethodPost, "/batch"},
	{http.MethodPost, "/identify"},
	{http.MethodPost, "/alias"},
	{http.MethodPost, "/e"},
	{http.MethodPost, "/e/"},
	{http.MethodPost, "/i/v0/e"},
	{http.MethodPost, "/i/v0/e/"},
}

// guardedEcho mounts the real guard in front of a stand-in handler for every
// mutating route. The handler answers 299 — a status no refusal uses — so
// "reached the handler" is never confused with "the guard let a 200 through".
const reachedHandler = 299

func guardedEcho(t *testing.T, g writeGuardStore, runsPerDay int) *echo.Echo {
	t.Helper()
	e := echo.New()
	e.Use(demoWriteGuard(g, runsPerDay))
	ok := func(c echo.Context) error { return c.NoContent(reachedHandler) }
	for _, route := range mutatingRoutes {
		e.Add(route[0], route[1], ok)
	}
	// A read, to prove the guard never touches one.
	e.GET("/api/dashboards", ok)
	return e
}

func do(e *echo.Echo, method, target, token string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, strings.NewReader("{}"))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	if token != "" {
		req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: token})
	}
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	return rec
}

// --- the matrix -----------------------------------------------------------

// Every class of mutation the contract names, aimed at the demo project. A
// viewer must be refused all of them, and the demo's own operator must not be.
func TestViewerIsRefusedEveryClassOfDemoMutation(t *testing.T) {
	cases := []struct {
		name   string
		method string
		target string
	}{
		{"create a dashboard", http.MethodPost, "/api/dashboards"},
		{"delete a dashboard", http.MethodDelete, "/api/dashboards/d1"},
		{"edit a chart", http.MethodPut, "/api/charts/c1"},
		{"delete a chart", http.MethodDelete, "/api/charts/c1"},
		{"reorder charts", http.MethodPut, "/api/dashboards/d1/charts/order"},
		{"save a query", http.MethodPost, "/api/saved-queries"},
		{"delete a saved query", http.MethodDelete, "/api/saved-queries/q1"},
		{"create an alert rule", http.MethodPost, "/api/alerts/rules"},
		{"delete an alert rule", http.MethodDelete, "/api/alerts/rules/r1"},
		{"create an alert channel", http.MethodPost, "/api/alerts/channels"},
		{"create a cohort audience", http.MethodPost, "/api/cohorts/audiences"},
		{"delete a cohort audience", http.MethodDelete, "/api/cohorts/audiences/a1"},
		{"hire an agent", http.MethodPost, "/api/agent/agents"},
		{"install a marketplace agent", http.MethodPost, "/api/marketplace/agents/growth/install"},
		{"configure an agent", http.MethodPut, "/api/agent/config"},
		{"rewrite the agent definition", http.MethodPut, "/api/agent/definition"},
		{"delete an agent", http.MethodDelete, "/api/agent/agents/a1"},
		{"grant an agent a scope", http.MethodPost, "/api/agent/agents/a1/grant"},
		{"toggle an agent tool", http.MethodPut, "/api/agent/tools/run_shell"},
		{"add a skill", http.MethodPost, "/api/agent/skills"},
		{"approve a skill", http.MethodPost, "/api/agent/skills/s1/approve"},
		{"write an agent secret", http.MethodPut, "/api/agent/secrets/OPENAI"},
		{"raise an agent budget", http.MethodPut, "/api/agent/budgets"},
		{"forget an agent memory", http.MethodDelete, "/api/agent/memory/m1"},
		{"create a schedule", http.MethodPost, "/api/agent/triggers"},
		{"edit a schedule", http.MethodPut, "/api/agent/triggers/t1"},
		{"delete a schedule", http.MethodDelete, "/api/agent/triggers/t1"},
		{"start a scheduled-style run", http.MethodPost, "/api/agent/run"},
		{"resume a run", http.MethodPost, "/api/agent/run/r1/resume"},
		{"connect a data source", http.MethodPost, "/api/connectors"},
		{"test a data source", http.MethodPost, "/api/connectors/c1/test"},
		{"schedule a sync", http.MethodPost, "/api/connectors/c1/syncs"},
		{"run a sync", http.MethodPost, "/api/connector-syncs/s1/run"},
		{"change the model pool", http.MethodPut, "/api/workspace/models"},
		{"add a provider key", http.MethodPost, "/api/workspace/providers"},
		{"delete a provider key", http.MethodDelete, "/api/workspace/providers/p1"},
		{"apply a template", http.MethodPost, "/api/templates/t1/apply"},
		{"change the subscription mapping", http.MethodPut, "/api/subscription/mapping"},
		{"create a team", http.MethodPost, "/api/teams"},
		{"move a kanban card", http.MethodPut, "/api/teams/t1/cards/c1"},
		{"create a validation test", http.MethodPost, "/api/validation/tests"},
		{"delete a waitlist row", http.MethodDelete, "/api/validation/waitlist/w1"},
		{"run an operation", http.MethodPost, "/api/operations/o1/run"},
		{"call a write op directly", http.MethodPost, "/api/op/create_chart"},
		{"edit someone else's turn", http.MethodPost, "/api/agent/conversations/c1/messages/e1/edit"},
		{"regenerate someone else's turn", http.MethodPost, "/api/agent/conversations/c1/messages/e1/regenerate"},
		{"add a lab case", http.MethodPost, "/api/agent/lab/cases"},
		{"run the lab", http.MethodPost, "/api/agent/lab/test"},
		{"ack a recommendation", http.MethodPost, "/api/agent/recommendations/r1/ack"},
	}

	e := guardedEcho(t, newFakeGuardStore(), 5)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			target := tc.target + "?project_id=" + demoProject
			rec := do(e, tc.method, target, visitorToken)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("viewer %s in the demo: status %d, want %d", tc.name, rec.Code, http.StatusForbidden)
			}
			var body map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("refusal body is not JSON: %q", rec.Body.String())
			}
			if body["reason"] != "demo_read_only" {
				t.Errorf("refusal reason = %v, want demo_read_only", body["reason"])
			}
			// The web client reads `error` on the streaming path and `message`
			// on the plain one; both have to be the sentence.
			for _, key := range []string{"error", "message"} {
				if text, _ := body[key].(string); !strings.Contains(text, "demo") {
					t.Errorf("refusal %q = %q, want a sentence about the demo", key, text)
				}
			}

			// The demo's real operator is an owner there and must be unaffected.
			if rec := do(e, tc.method, target, operatorTok); rec.Code != reachedHandler {
				t.Errorf("operator (owner of the demo) %s: status %d, want the handler to be reached", tc.name, rec.Code)
			}
		})
	}
}

// The failure that would be worse than the hole: a viewer of the demo is still
// an OWNER of their own workspace, and every one of those mutations must pass.
// Note the ?project_id=<demo> variants — the web client appends the ACTIVE
// project to every call, so a user who is looking at the demo sends its id
// alongside a write aimed at their own account.
func TestDemoViewerKeepsFullWriteAccessAtHome(t *testing.T) {
	cases := []struct {
		name   string
		method string
		target string
	}{
		{"create a dashboard", http.MethodPost, "/api/dashboards?project_id=" + homeProject},
		{"delete a chart", http.MethodDelete, "/api/charts/c1?project_id=" + homeProject},
		{"create an alert rule", http.MethodPost, "/api/alerts/rules?project_id=" + homeProject},
		{"hire an agent", http.MethodPost, "/api/agent/agents?project_id=" + homeProject},
		{"create a schedule", http.MethodPost, "/api/agent/triggers?project_id=" + homeProject},
		{"rotate their own api key", http.MethodPost, "/api/projects/" + homeProject + "/rotate-key"},
		{"rename their own workspace", http.MethodPut, "/api/workspaces/" + homeWS},
		{"invite a member", http.MethodPost, "/api/workspaces/" + homeWS + "/members"},
		{"remove a member", http.MethodDelete, "/api/workspaces/" + homeWS + "/members/u2"},
		{"create a project in their workspace", http.MethodPost, "/api/workspaces/" + homeWS + "/projects"},
		{"ask for an upgrade", http.MethodPost, "/api/workspaces/" + homeWS + "/upgrade-request"},
		// The demo is the active project in the UI, but the write is theirs.
		{"invite while viewing the demo", http.MethodPost, "/api/workspaces/" + homeWS + "/members?project_id=" + demoProject},
		{"rename themselves while viewing the demo", http.MethodPut, "/api/users/me?project_id=" + demoProject},
		{"create a workspace while viewing the demo", http.MethodPost, "/api/workspaces?project_id=" + demoProject},
		{"create a project while viewing the demo", http.MethodPost, "/api/projects?project_id=" + demoProject},
		// No project named at all: the default is their own, never the demo.
		{"write with no explicit scope", http.MethodPost, "/api/dashboards"},
	}

	e := guardedEcho(t, newFakeGuardStore(), 5)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if rec := do(e, tc.method, tc.target, visitorToken); rec.Code != reachedHandler {
				t.Fatalf("%s: status %d, want the handler to be reached (body %q)", tc.name, rec.Code, rec.Body.String())
			}
		})
	}
}

// Reading the demo is the entire point of it.
func TestViewerCanReadAndAskInsideTheDemo(t *testing.T) {
	e := guardedEcho(t, newFakeGuardStore(), 5)
	cases := []struct {
		name   string
		method string
		target string
	}{
		{"read a dashboard list", http.MethodGet, "/api/dashboards?project_id=" + demoProject},
		{"run read-only SQL", http.MethodPost, "/api/sql/run?project_id=" + demoProject},
		{"run a saved query", http.MethodPost, "/api/saved-queries/q1/run?project_id=" + demoProject},
		{"open a conversation", http.MethodPost, "/api/agent/conversations?project_id=" + demoProject},
		{"ask the agent", http.MethodPost, "/api/agent/chat?project_id=" + demoProject},
		{"send a conversation message", http.MethodPost, "/api/agent/conversations/c1/messages?project_id=" + demoProject},
		{"steer the run", http.MethodPost, "/api/agent/chat/steer?project_id=" + demoProject},
		{"stop the run", http.MethodPost, "/api/agent/chat/cancel?project_id=" + demoProject},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if rec := do(e, tc.method, tc.target, visitorToken); rec.Code != reachedHandler {
				t.Fatalf("%s: status %d, want the handler to be reached (body %q)", tc.name, rec.Code, rec.Body.String())
			}
		})
	}
}

// --- the agent budget -----------------------------------------------------

func TestDemoAgentRunsAreCappedPerUserPerDay(t *testing.T) {
	const limit = 3
	fake := newFakeGuardStore()
	e := guardedEcho(t, fake, limit)
	target := "/api/agent/chat?project_id=" + demoProject

	for i := 1; i <= limit; i++ {
		if rec := do(e, http.MethodPost, target, visitorToken); rec.Code != reachedHandler {
			t.Fatalf("question %d of %d: status %d, want it allowed", i, limit, rec.Code)
		}
	}

	rec := do(e, http.MethodPost, target, visitorToken)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("question %d: status %d, want %d", limit+1, rec.Code, http.StatusTooManyRequests)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("refusal body is not JSON: %q", rec.Body.String())
	}
	if body["reason"] != "demo_agent_quota" {
		t.Errorf("reason = %v, want demo_agent_quota", body["reason"])
	}
	message, _ := body["message"].(string)
	// It is a conversion prompt, not a rate-limit error: it has to say what ran
	// out, why there is a limit at all, when it comes back, and what to do now.
	for _, want := range []string{"3 questions", "reset", "Connect your own project"} {
		if !strings.Contains(message, want) {
			t.Errorf("refusal message %q is missing %q", message, want)
		}
	}
	if strings.Contains(strings.ToLower(message), "forbidden") || strings.Contains(message, "429") {
		t.Errorf("refusal message reads like an error code: %q", message)
	}
	if body["limit"] != float64(limit) {
		t.Errorf("limit = %v, want %d", body["limit"], limit)
	}

	// Per user: a second person still has their own full budget.
	fake.defaultProj[operatorID] = homeProject
	fake.members[demoWS][operatorID] = storage.DemoViewerRole
	if rec := do(e, http.MethodPost, target, operatorTok); rec.Code != reachedHandler {
		t.Fatalf("a second viewer's first question: status %d, want it allowed", rec.Code)
	}
}

// The cap is a DEMO control. The same user asking the same agent the same
// question in their own project is not metered by it at all.
// A question the provider never accepted costs the instance owner nothing, so
// it must not cost the visitor one of their few daily asks. Without this, an
// instance with a bad model alias burns a visitor's whole budget on errors and
// locks them out for the day having never seen an answer.
func TestAQuestionThatNeverReachedTheProviderIsRefunded(t *testing.T) {
	const limit = 2
	fake := newFakeGuardStore()
	e := echo.New()
	e.Use(demoWriteGuard(fake, limit))
	// A handler that fails the way a rejected model alias does: no tokens spent,
	// so it hands the claim back before answering.
	e.POST("/api/agent/chat", func(c echo.Context) error {
		refundDemoRun(c)
		return c.JSON(http.StatusBadGateway, map[string]any{"error": "provider chat (turn 1): model not supported"})
	})
	target := "/api/agent/chat?project_id=" + demoProject

	// Every attempt fails at the provider, so the budget never moves and the
	// visitor is never locked out.
	for i := 1; i <= limit*3; i++ {
		if rec := do(e, http.MethodPost, target, visitorToken); rec.Code != http.StatusBadGateway {
			t.Fatalf("attempt %d: status %d, want %d — the cap counted a run that bought nothing",
				i, rec.Code, http.StatusBadGateway)
		}
	}
	if used := fake.used[visitorID]; used != 0 {
		t.Errorf("used = %d after %d failed questions, want 0", used, limit*3)
	}

	// And the budget is still whole: a handler that DOES answer still spends it.
	e2 := echo.New()
	e2.Use(demoWriteGuard(fake, limit))
	e2.POST("/api/agent/chat", func(c echo.Context) error { return c.NoContent(reachedHandler) })
	for i := 1; i <= limit; i++ {
		if rec := do(e2, http.MethodPost, target, visitorToken); rec.Code != reachedHandler {
			t.Fatalf("answered question %d: status %d, want it allowed", i, rec.Code)
		}
	}
	if rec := do(e2, http.MethodPost, target, visitorToken); rec.Code != http.StatusTooManyRequests {
		t.Errorf("status %d after %d answers, want %d — the refund must not raise the cap",
			rec.Code, limit, http.StatusTooManyRequests)
	}
}

func TestTheCapAppliesOnlyInsideTheDemoProject(t *testing.T) {
	fake := newFakeGuardStore()
	e := guardedEcho(t, fake, 1)

	// Spend the demo budget.
	if rec := do(e, http.MethodPost, "/api/agent/chat?project_id="+demoProject, visitorToken); rec.Code != reachedHandler {
		t.Fatalf("first demo question: status %d", rec.Code)
	}
	if rec := do(e, http.MethodPost, "/api/agent/chat?project_id="+demoProject, visitorToken); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("second demo question: status %d, want it capped", rec.Code)
	}

	// Their own project is untouched by it, however many times they ask.
	for i := 0; i < 5; i++ {
		if rec := do(e, http.MethodPost, "/api/agent/chat?project_id="+homeProject, visitorToken); rec.Code != reachedHandler {
			t.Fatalf("question %d in their own project: status %d, want it allowed", i+1, rec.Code)
		}
	}
	if used := fake.used[visitorID]; used != 1 {
		t.Errorf("demo ledger counted %d runs, want only the 1 that was actually in the demo", used)
	}
}

// A sibling project inside the demo workspace is the operator's too, and the
// budget is defined on the demo project — so an unmetered agent run there would
// be exactly the unbounded bill the cap exists to prevent.
func TestAgentIsRefusedInDemoWorkspaceSiblingProjects(t *testing.T) {
	e := guardedEcho(t, newFakeGuardStore(), 5)
	rec := do(e, http.MethodPost, "/api/agent/chat?project_id="+demoSibling, visitorToken)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status %d, want %d", rec.Code, http.StatusForbidden)
	}
}

// A limit of zero is an operator saying "no agent for visitors", not "no
// limit". Reading it the other way would be an unbounded bill produced by a
// setting that looks like a lockdown.
func TestZeroCapRefusesEveryDemoQuestion(t *testing.T) {
	e := guardedEcho(t, newFakeGuardStore(), 0)
	rec := do(e, http.MethodPost, "/api/agent/chat?project_id="+demoProject, visitorToken)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status %d, want %d", rec.Code, http.StatusTooManyRequests)
	}
	if body := rec.Body.String(); !strings.Contains(body, "switched off") {
		t.Errorf("a zero cap should say the agent is off, not that questions ran out: %q", body)
	}
}

// The demo's own operator writes to their own site and pays for their own runs;
// the visitor budget is not theirs.
func TestTheDemoOperatorIsNotMetered(t *testing.T) {
	fake := newFakeGuardStore()
	e := guardedEcho(t, fake, 1)
	for i := 0; i < 4; i++ {
		if rec := do(e, http.MethodPost, "/api/agent/chat?project_id="+demoProject, operatorTok); rec.Code != reachedHandler {
			t.Fatalf("operator question %d: status %d, want it allowed", i+1, rec.Code)
		}
	}
	if used := fake.used[operatorID]; used != 0 {
		t.Errorf("the operator spent %d of the visitor budget, want 0", used)
	}
}

// --- fail-closed properties ------------------------------------------------

// With no demo configured — the default, and every `docker compose up` — the
// guard must be completely inert. An instance without a demo has no untrusted
// members, and a self-hosted deployment must not have its API quietly narrowed.
func TestWithNoDemoNothingChanges(t *testing.T) {
	fake := newFakeGuardStore()
	fake.demoWorkspace = ""
	fake.demoProjectID = ""
	e := guardedEcho(t, fake, 5)

	for _, route := range mutatingRoutes {
		target := strings.NewReplacer(
			":workspace_id", homeWS, ":project_id", homeProject, ":user_id", "u1",
			":dashboard_id", "d1", ":chart_id", "c1", ":query_id", "q1", ":rule_id", "r1",
			":channel_id", "ch1", ":audience_id", "a1", ":connector_id", "c1", ":sync_id", "s1",
			":team_id", "t1", ":card_id", "cd1", ":agent_id", "ag1", ":id", "x1", ":name", "n1",
			":period", "daily", ":slug", "growth", ":run_id", "r1", ":entry_id", "e1",
			":template_id", "t1", ":token", "tok",
		).Replace(route[1])
		// No session at all, which is the strictest case: without a demo the
		// guard must still not be the thing that answers.
		if rec := do(e, route[0], target, ""); rec.Code != reachedHandler {
			t.Errorf("%s %s with no demo configured: status %d, want the handler to be reached", route[0], target, rec.Code)
		}
	}
}

// An unauthenticated mutating request must not sail past the guard into a
// handler on the strength of not being attributable to anyone.
func TestUnauthenticatedWritesAreRefused(t *testing.T) {
	e := guardedEcho(t, newFakeGuardStore(), 5)
	for _, target := range []string{"/api/dashboards", "/api/dashboards?project_id=" + demoProject, "/api/agent/chat"} {
		if rec := do(e, http.MethodPost, target, ""); rec.Code != http.StatusUnauthorized {
			t.Errorf("POST %s with no session: status %d, want %d", target, rec.Code, http.StatusUnauthorized)
		}
	}
	// The public collection endpoints have no session by design and must stay
	// open — they are how the demo site feeds the demo.
	for _, target := range []string{"/capture", "/batch", "/identify", "/alias", "/waitlist", "/api/auth/login", "/api/auth/signup"} {
		if rec := do(e, http.MethodPost, target, ""); rec.Code != reachedHandler {
			t.Errorf("POST %s: status %d, want the handler to be reached", target, rec.Code)
		}
	}
}

// A logged-in caller naming a project they are not a member of proves nothing
// about where the write lands, so it is refused rather than falling back to a
// workspace they DO belong to.
func TestUnresolvableScopeIsRefused(t *testing.T) {
	e := guardedEcho(t, newFakeGuardStore(), 5)
	for _, target := range []string{
		"/api/dashboards?project_id=99999999-9999-9999-9999-999999999999",
		"/api/workspaces/99999999-9999-9999-9999-999999999999/members",
	} {
		if rec := do(e, http.MethodPost, target, visitorToken); rec.Code != http.StatusForbidden {
			t.Errorf("POST %s: status %d, want %d", target, rec.Code, http.StatusForbidden)
		}
	}
}

// The demo project's API key is PUBLIC — it ships in the script tag on the demo
// site's own pages — so it authenticates event collection and nothing else.
func TestTheDemoAPIKeyCannotDriveTheAPI(t *testing.T) {
	e := guardedEcho(t, newFakeGuardStore(), 5)
	for _, target := range []string{
		"/api/dashboards?api_key=" + demoKey,
		"/api/agent/chat?api_key=" + demoKey,
		"/mcp?api_key=" + demoKey,
		"/api/op/create_chart?api_key=" + demoKey,
	} {
		if rec := do(e, http.MethodPost, target, ""); rec.Code != http.StatusForbidden {
			t.Errorf("POST %s: status %d, want %d", target, rec.Code, http.StatusForbidden)
		}
	}
	// Collection keeps working: that IS the demo's data feed.
	for _, target := range []string{"/capture?api_key=" + demoKey, "/batch?api_key=" + demoKey} {
		if rec := do(e, http.MethodPost, target, ""); rec.Code != reachedHandler {
			t.Errorf("POST %s: status %d, want the handler to be reached", target, rec.Code)
		}
	}
}

// The guard's whole security property is its default arm. A route nobody has
// classified must be denied, not allowed.
func TestAnUnclassifiedRouteIsDeniedByDefault(t *testing.T) {
	if got := classifyWrite("/api/some/route/nobody/has/thought/about"); got != writeGuarded {
		t.Fatalf("classifyWrite(unknown) = %v, want writeGuarded", got)
	}
	fake := newFakeGuardStore()
	e := echo.New()
	e.Use(demoWriteGuard(fake, 5))
	e.POST("/api/future/thing", func(c echo.Context) error { return c.NoContent(reachedHandler) })
	if rec := do(e, http.MethodPost, "/api/future/thing?project_id="+demoProject, visitorToken); rec.Code != http.StatusForbidden {
		t.Fatalf("a route added later: status %d, want it denied for a demo viewer", rec.Code)
	}
}

// Reads are never touched, whatever the role or the scope.
func TestReadsAreNeverGuarded(t *testing.T) {
	e := guardedEcho(t, newFakeGuardStore(), 0)
	for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodOptions} {
		if mutatingMethod(method) {
			t.Errorf("mutatingMethod(%s) = true, want false", method)
		}
	}
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		if !mutatingMethod(method) {
			t.Errorf("mutatingMethod(%s) = false, want true — an unclassified verb must be treated as a write", method)
		}
	}
	if rec := do(e, http.MethodGet, "/api/dashboards?project_id="+demoProject, visitorToken); rec.Code != reachedHandler {
		t.Fatalf("GET in the demo: status %d, want the handler to be reached", rec.Code)
	}
}

// Every exemption in writeClasses must correspond to a route this app actually
// registers. A stale entry is a hole aimed at a path that may be re-used later
// for something else entirely.
func TestEveryExemptionNamesARealRoute(t *testing.T) {
	registered := map[string]bool{}
	for _, route := range mutatingRoutes {
		registered[route[1]] = true
	}
	// The MCP group registers both spellings; only one is listed above.
	registered["/mcp/"] = true
	for path := range writeClasses {
		if !registered[path] {
			t.Errorf("writeClasses exempts %q, which is not a mutating route this app registers", path)
		}
	}
}

// The audit that keeps itself honest.
//
// The list above is only as good as its last update, and a security guard whose
// coverage is a hand-kept list is a guard that quietly stops covering things.
// This walks the package's own source for route registrations and fails when a
// mutating route exists that the table does not name — which is also the moment
// someone has to decide whether the new route is a write (the default) or an
// exemption.
//
// It cannot see routes mounted by another package into a group (the operation
// registry's /api/op/<op> and /mcp, which opcore mounts): those are covered by
// name in the table above and by TestTheDemoAPIKeyCannotDriveTheAPI.
func TestTheRouteTableMatchesTheSource(t *testing.T) {
	listed := map[string]bool{}
	for _, route := range mutatingRoutes {
		listed[route[1]] = true
	}
	direct := regexp.MustCompile(`\be\.(POST|PUT|PATCH|DELETE)\("([^"]+)"`)
	collect := regexp.MustCompile(`publicCollect\(e, http\.Method(?:Post|Put|Patch|Delete), "([^"]+)"`)

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	found := 0
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		for _, m := range direct.FindAllStringSubmatch(string(src), -1) {
			found++
			if !listed[m[2]] {
				t.Errorf("%s registers %s %s, which the guard's route table does not name — classify it in writeClasses (or leave it guarded) and add it to mutatingRoutes", name, m[1], m[2])
			}
		}
		for _, m := range collect.FindAllStringSubmatch(string(src), -1) {
			found++
			if !listed[m[1]] {
				t.Errorf("%s collects on %s, which the guard's route table does not name", name, m[1])
			}
		}
	}
	// A regex that matches nothing would make this test pass forever.
	if found < 80 {
		t.Fatalf("only %d mutating registrations found in the package source; the scan is broken", found)
	}
}

// Allowing the question is only safe if the run it starts cannot make the
// change the caller was just refused. The guard marks the request; the chat
// handlers carry the mark into ChatOptions.ReadOnly, which strips the agent's
// writing tools (internal/runtime/policy.go).
func TestADemoViewersQuestionRunsReadOnly(t *testing.T) {
	fake := newFakeGuardStore()
	e := echo.New()
	e.Use(demoWriteGuard(fake, 5))
	var sawReadOnly bool
	e.POST("/api/agent/chat", func(c echo.Context) error {
		sawReadOnly = readOnlyCaller(c)
		return c.NoContent(reachedHandler)
	})

	if rec := do(e, http.MethodPost, "/api/agent/chat?project_id="+demoProject, visitorToken); rec.Code != reachedHandler {
		t.Fatalf("status %d, want the question allowed", rec.Code)
	}
	if !sawReadOnly {
		t.Fatal("a demo viewer's question would have run with the agent's writing tools")
	}

	// Their own project is not read-only: the agent is supposed to build things
	// there, and a flag that leaked would quietly break the product.
	sawReadOnly = false
	if rec := do(e, http.MethodPost, "/api/agent/chat?project_id="+homeProject, visitorToken); rec.Code != reachedHandler {
		t.Fatalf("status %d, want the question allowed", rec.Code)
	}
	if sawReadOnly {
		t.Fatal("a question about the caller's OWN project was made read-only")
	}

	// Neither is the demo operator's own question about their own site.
	sawReadOnly = false
	if rec := do(e, http.MethodPost, "/api/agent/chat?project_id="+demoProject, operatorTok); rec.Code != reachedHandler {
		t.Fatalf("status %d, want the question allowed", rec.Code)
	}
	if sawReadOnly {
		t.Fatal("the demo owner's own question was made read-only")
	}
}

// Running a saved query is a read for the person doing it and a write to the
// owner's row (it refreshes a result cache). A demo viewer must get the rows
// and leave the cache alone, so the guard tells the handler who it is serving.
func TestAViewersQueryRunDoesNotWriteThroughTheCache(t *testing.T) {
	e := echo.New()
	e.Use(demoWriteGuard(newFakeGuardStore(), 5))
	var readOnly bool
	e.POST("/api/saved-queries/:query_id/run", func(c echo.Context) error {
		readOnly = readOnlyCaller(c)
		return c.NoContent(reachedHandler)
	})

	if rec := do(e, http.MethodPost, "/api/saved-queries/q1/run?project_id="+demoProject, visitorToken); rec.Code != reachedHandler {
		t.Fatalf("status %d, want the read allowed", rec.Code)
	}
	if !readOnly {
		t.Error("a demo viewer's query run would have written the owner's result cache")
	}

	readOnly = false
	if rec := do(e, http.MethodPost, "/api/saved-queries/q1/run?project_id="+homeProject, visitorToken); rec.Code != reachedHandler {
		t.Fatalf("status %d, want the read allowed", rec.Code)
	}
	if readOnly {
		t.Error("a query run in the caller's OWN project was marked read-only; the cache would never refresh")
	}
}
