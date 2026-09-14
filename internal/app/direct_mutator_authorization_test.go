package app

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/lohi-ai/agentray/internal/dataplane/ingest"
	"github.com/lohi-ai/agentray/internal/dataplane/store"
	"github.com/lohi-ai/agentray/internal/runtime"
)

// The legacy REST mutators that write through storage.Store without an
// operation to name — cohort audiences, saved queries, the subscription
// mapping, template instantiation. They resolve their caller with
// projectFromRequest, which answers "may this credential address the project"
// and nothing else: it was written for reads, and a management credential
// minted with analytics:read or a viewer's session therefore reached all of
// them while /api/op refused the same work for the same credential.
//
// These tests are the reproduction. The refused-caller matrix is what failed
// before the fix (each route answered 2xx and the row appeared), and the
// owner's column is what keeps the fix from being "deny everything".

// mountServerRoutes mounts the real route set over the test store — the same
// registerRoutes the server calls, with the same op adapter — so these tests
// exercise the shipped handlers and their resolvers rather than a stand-in
// handler per route.
func mountServerRoutes(t *testing.T, s *storage.Store) *echo.Echo {
	t.Helper()
	e := echo.New()
	e.HideBanner = true
	pass := func(next echo.HandlerFunc) echo.HandlerFunc { return next }
	registerRoutes(e, s, ingestion.EventQueue{}, pass, pass, nil, nil, agentruntime.ToolBuildContext{}, nil, false, publicCollectSet{}, newOpAdapter(s, nil, nil), nil)
	return e
}

// caller is one credential aimed at the routes under test. projectID is
// appended as ?project_id= for the session callers: a session's default
// project is its own, so a viewer's write aimed at the demo's project (exactly
// what the web client sends) must name it.
type caller struct {
	name    string
	headers map[string]string
	cookies []*http.Cookie
	// projectID is appended as ?project_id=: a session's default project is its
	// own, so a write aimed at another project must name it.
	projectID string
	// mayRunReads marks a caller whose grants cover the class the two
	// SQL-carrying routes need (analytics:read). Those routes are reads, so the
	// matrix asserts a refusal only for the callers that lack the class —
	// stated here rather than inferred from the caller's display name.
	mayRunReads bool
}

func (c caller) request(t *testing.T, e *echo.Echo, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	if c.projectID != "" {
		separator := "?"
		if strings.Contains(path, "?") {
			separator = "&"
		}
		path += separator + "project_id=" + c.projectID
	}
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	for k, v := range c.headers {
		req.Header.Set(k, v)
	}
	for _, cookie := range c.cookies {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	return rec
}

// writeRoute mutates project state; readRoute is a POST that only runs SQL
// (the analytics read the matrix allows a viewer, and the reason these cannot
// be guarded by HTTP method).
type mutatorKind int

const (
	writeRoute mutatorKind = iota
	readRoute
)

type directMutatorCase struct {
	name   string
	kind   mutatorKind
	method string
	path   func(f directMutatorFixture) string
	body   func(f directMutatorFixture) string
	// accepted is the status the owner's session sees. Every refusal below is
	// asserted against a route that provably works for the owner in the same
	// run, so a route broken by a typo cannot masquerade as a closed hole.
	accepted int
}

type directMutatorFixture struct {
	dashboardID     string
	audienceID      string
	renameQueryID   string
	deleteQueryID   string
	templateID      string
	templateChartID string
}

const countEventsSQL = `SELECT count(*) AS n FROM events`

func seedDirectMutatorFixture(t *testing.T, s *storage.Store, projectID string) directMutatorFixture {
	t.Helper()
	ctx := context.Background()
	f := directMutatorFixture{}

	dashboard, err := s.CreateDashboard(ctx, projectID, "Ops", "")
	if err != nil {
		t.Fatalf("seed dashboard: %v", err)
	}
	f.dashboardID = dashboard.ID

	audience, err := s.CreateProjectAudience(ctx, projectID, "Beta testers", "paid", nil)
	if err != nil {
		t.Fatalf("seed audience: %v", err)
	}
	f.audienceID = audience.ID

	rename, err := s.CreateSavedQuery(ctx, projectID, "Count events", countEventsSQL, true)
	if err != nil {
		t.Fatalf("seed saved query: %v", err)
	}
	f.renameQueryID = rename.ID

	remove, err := s.CreateSavedQuery(ctx, projectID, "Doomed query", countEventsSQL, true)
	if err != nil {
		t.Fatalf("seed saved query: %v", err)
	}
	f.deleteQueryID = remove.ID

	templates, err := s.ListTemplates(ctx, projectID)
	if err != nil {
		t.Fatalf("list templates: %v", err)
	}
	for _, template := range templates {
		if len(template.Charts) > 0 {
			f.templateID, f.templateChartID = template.ID, template.Charts[0].ID
			break
		}
	}
	if f.templateID == "" {
		t.Fatalf("no seeded system template with charts — the apply/clone routes cannot be exercised")
	}
	return f
}

// directMutatorState is everything these routes can change, read back the way
// a client would see it.
type directMutatorState struct {
	audiences    []storage.ProjectAudience
	savedQueries []storage.SavedQuery
	dashboards   []storage.Dashboard
	mapping      storage.SubscriptionMapping
}

func snapshotDirectMutatorState(t *testing.T, s *storage.Store, projectID string) directMutatorState {
	t.Helper()
	ctx := context.Background()
	var state directMutatorState
	var err error
	if state.audiences, err = s.ListProjectAudiences(ctx, projectID); err != nil {
		t.Fatalf("list audiences: %v", err)
	}
	if state.savedQueries, err = s.ListSavedQueries(ctx, projectID); err != nil {
		t.Fatalf("list saved queries: %v", err)
	}
	if state.dashboards, err = s.ListDashboards(ctx, projectID); err != nil {
		t.Fatalf("list dashboards: %v", err)
	}
	if state.mapping, err = s.GetSubscriptionMapping(ctx, projectID); err != nil {
		t.Fatalf("get mapping: %v", err)
	}
	return state
}

func directMutatorCases() []directMutatorCase {
	return []directMutatorCase{
		{
			name:   "apply a template",
			kind:   writeRoute,
			method: http.MethodPost,
			path:   func(f directMutatorFixture) string { return "/api/templates/" + f.templateID + "/apply" },
			body:   func(directMutatorFixture) string { return `{}` },
			// 201 Created — the clone creates the dashboard and its charts.
			accepted: http.StatusCreated,
		},
		{
			name:   "clone a template chart",
			kind:   writeRoute,
			method: http.MethodPost,
			path: func(f directMutatorFixture) string {
				return "/api/templates/" + f.templateID + "/charts/" + f.templateChartID + "/clone"
			},
			body:     func(f directMutatorFixture) string { return `{"dashboard_id":"` + f.dashboardID + `"}` },
			accepted: http.StatusCreated,
		},
		{
			name:     "create a cohort audience",
			kind:     writeRoute,
			method:   http.MethodPost,
			path:     func(directMutatorFixture) string { return "/api/cohorts/audiences" },
			body:     func(directMutatorFixture) string { return `{"label":"Payers","kind":"paid","plans":[]}` },
			accepted: http.StatusCreated,
		},
		{
			name:     "update a cohort audience",
			kind:     writeRoute,
			method:   http.MethodPut,
			path:     func(f directMutatorFixture) string { return "/api/cohorts/audiences/" + f.audienceID },
			body:     func(directMutatorFixture) string { return `{"label":"Beta testers","kind":"paid","plans":[]}` },
			accepted: http.StatusOK,
		},
		{
			name:     "delete a cohort audience",
			kind:     writeRoute,
			method:   http.MethodDelete,
			path:     func(f directMutatorFixture) string { return "/api/cohorts/audiences/" + f.audienceID },
			body:     func(directMutatorFixture) string { return "" },
			accepted: http.StatusNoContent,
		},
		{
			name:   "save the subscription mapping",
			kind:   writeRoute,
			method: http.MethodPut,
			path:   func(directMutatorFixture) string { return "/api/subscription/mapping" },
			body: func(directMutatorFixture) string {
				return `{"start_event":"subscription_started","period_end_prop":"current_period_end","plan_prop":"plan","grace_days":3}`
			},
			accepted: http.StatusOK,
		},
		{
			name:   "create a saved query",
			kind:   writeRoute,
			method: http.MethodPost,
			path:   func(directMutatorFixture) string { return "/api/saved-queries" },
			body: func(directMutatorFixture) string {
				return `{"natural_language":"Count events","generated_sql":"` + countEventsSQL + `","verified":true}`
			},
			accepted: http.StatusCreated,
		},
		{
			name:     "rename a saved query",
			kind:     writeRoute,
			method:   http.MethodPatch,
			path:     func(f directMutatorFixture) string { return "/api/saved-queries/" + f.renameQueryID },
			body:     func(directMutatorFixture) string { return `{"natural_language":"Renamed"}` },
			accepted: http.StatusOK,
		},
		{
			name:     "delete a saved query",
			kind:     writeRoute,
			method:   http.MethodDelete,
			path:     func(f directMutatorFixture) string { return "/api/saved-queries/" + f.deleteQueryID },
			body:     func(directMutatorFixture) string { return "" },
			accepted: http.StatusNoContent,
		},
		{
			name:     "run a saved query",
			kind:     readRoute,
			method:   http.MethodPost,
			path:     func(f directMutatorFixture) string { return "/api/saved-queries/" + f.renameQueryID + "/run" },
			body:     func(directMutatorFixture) string { return `{}` },
			accepted: http.StatusOK,
		},
		{
			name:     "run ad-hoc SQL",
			kind:     readRoute,
			method:   http.MethodPost,
			path:     func(directMutatorFixture) string { return "/api/sql/run" },
			body:     func(directMutatorFixture) string { return `{"sql":"` + countEventsSQL + `"}` },
			accepted: http.StatusOK,
		},
	}
}

func TestDirectMutatorsRefuseCredentialsThatMayNotWrite(t *testing.T) {
	s := openAppTestStore(t)
	ctx := context.Background()
	e := mountServerRoutes(t, s)

	stamp := time.Now().UnixNano()
	owner, err := s.CreateAccount(ctx, fmt.Sprintf("direct-mutator-%d@test.local", stamp), "Owner", "password-123", "ws", "proj")
	if err != nil {
		t.Fatalf("create account: %v", err)
	}
	project := owner.Project
	_, ownerToken, err := s.CreateUserSession(ctx, owner.User.ID, time.Hour)
	if err != nil {
		t.Fatalf("owner session: %v", err)
	}

	// The two under-privileged machine credentials the audit proved live: a
	// reader that may not write anything, and a credential whose only grant is
	// the source-probe read.
	_, readerSecret, err := s.CreateProjectCredential(ctx, owner.User.ID, project.ID, "reader", []string{"analytics:read"})
	if err != nil {
		t.Fatalf("reader credential: %v", err)
	}
	_, sourceSecret, err := s.CreateProjectCredential(ctx, owner.User.ID, project.ID, "sources", []string{"sources:read"})
	if err != nil {
		t.Fatalf("sources credential: %v", err)
	}

	// A viewer of the same workspace — a real member, admitted to the project,
	// read-only by role.
	viewer, err := s.CreateAccount(ctx, fmt.Sprintf("direct-mutator-viewer-%d@test.local", stamp), "Viewer", "password-123", "viewer-ws", "viewer-proj")
	if err != nil {
		t.Fatalf("create viewer: %v", err)
	}
	if _, err := s.AddWorkspaceMemberByEmail(ctx, owner.User.ID, owner.Workspace.ID, viewer.User.Email, "viewer"); err != nil {
		t.Fatalf("add viewer: %v", err)
	}
	_, viewerToken, err := s.CreateUserSession(ctx, viewer.User.ID, time.Hour)
	if err != nil {
		t.Fatalf("viewer session: %v", err)
	}

	fixture := seedDirectMutatorFixture(t, s, project.ID)

	reader := caller{name: "analytics:read credential", headers: map[string]string{"Authorization": "Bearer " + readerSecret}, mayRunReads: true}
	sources := caller{name: "sources:read credential", headers: map[string]string{"Authorization": "Bearer " + sourceSecret}}
	viewerCaller := caller{
		name:        "viewer session",
		cookies:     []*http.Cookie{{Name: sessionCookieName, Value: viewerToken}},
		projectID:   project.ID,
		mayRunReads: true,
	}

	// Every credential that may not change this project is refused by every
	// route that changes it, and the refusal is the whole request: the state
	// below is compared afterwards.
	before := snapshotDirectMutatorState(t, s, project.ID)
	for _, tc := range directMutatorCases() {
		for _, who := range []caller{reader, sources, viewerCaller} {
			if tc.kind == readRoute && who.mayRunReads {
				// Running SQL is analytics:read — the class /api/op's run_sql
				// requires and the call the demo lets a viewer make — so the
				// only refusal to assert here is the caller that lacks it.
				continue
			}
			rec := who.request(t, e, tc.method, tc.path(fixture), tc.body(fixture))
			if rec.Code != http.StatusForbidden {
				t.Errorf("%s %s as %s = %d %s, want 403", tc.method, tc.path(fixture), who.name, rec.Code, rec.Body.String())
			}
		}
	}
	after := snapshotDirectMutatorState(t, s, project.ID)
	if !reflect.DeepEqual(before, after) {
		t.Errorf("a refused caller changed the project:\nbefore: %+v\nafter:  %+v", before, after)
	}

	// The owner is not collateral damage: the same request succeeds for the
	// only credential here that may write.
	ownerCaller := caller{
		name:      "owner session",
		cookies:   []*http.Cookie{{Name: sessionCookieName, Value: ownerToken}},
		projectID: project.ID,
	}
	for _, tc := range directMutatorCases() {
		rec := ownerCaller.request(t, e, tc.method, tc.path(fixture), tc.body(fixture))
		if rec.Code != tc.accepted {
			t.Errorf("owner %s %s = %d %s, want %d", tc.method, tc.path(fixture), rec.Code, rec.Body.String(), tc.accepted)
		}
	}
}

// TestNoMutatingRouteResolvesThroughTheReadResolver pins the property rather
// than the eleven instances of it: projectFromRequest answers admission, never
// an access class, so a mutating handler that resolves through it is open to
// whatever credential may address the project. The scan is over the package's
// own source because that is the only place a route that does not exist yet
// can be caught.
func TestNoMutatingRouteResolvesThroughTheReadResolver(t *testing.T) {
	// A registration runs from its line to the next registration of ANY method
	// or the next package-level function — whichever comes first — so the slice
	// is that handler's body and nothing else. (Bounding on mutating
	// registrations alone would run a read handler's body into the next
	// mutating route's slice and report its resolver.) Both registration forms
	// the package uses are scanned — `receiver.METHOD("path"` and
	// `receiver.Add(http.MethodX, "path"` — because a route that registers the
	// second way is exactly as capable of resolving through the read resolver
	// as one that registers the first.
	registration := regexp.MustCompile(`(?m)^\s*\w+\.(GET|POST|PUT|PATCH|DELETE|HEAD|OPTIONS)\("([^"]+)"`)
	added := regexp.MustCompile(`(?m)^\s*\w+\.Add\(http\.(Method\w+),\s*"([^"]+)"`)
	topLevelFunc := regexp.MustCompile(`(?m)^func `)

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	type registered struct {
		start        int
		method, path string
	}
	examined := 0
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		source := string(src)
		var hits []registered
		for _, m := range registration.FindAllStringSubmatchIndex(source, -1) {
			hits = append(hits, registered{m[0], source[m[2]:m[3]], source[m[4]:m[5]]})
		}
		for _, m := range added.FindAllStringSubmatchIndex(source, -1) {
			hits = append(hits, registered{m[0], strings.TrimPrefix(source[m[2]:m[3]], "Method"), source[m[4]:m[5]]})
		}
		sort.Slice(hits, func(i, j int) bool { return hits[i].start < hits[j].start })
		for i, h := range hits {
			// mutatingMethod is the guard's own deny-by-default verb rule, so a
			// verb this scan does not know is treated as a mutation by both.
			if !mutatingMethod(h.method) {
				continue
			}
			end := len(source)
			if i+1 < len(hits) {
				end = hits[i+1].start
			}
			if fn := topLevelFunc.FindStringIndex(source[h.start:end]); fn != nil {
				end = h.start + fn[0]
			}
			examined++
			if strings.Contains(source[h.start:end], "projectFromRequest(") {
				t.Errorf("%s registers %s %s through projectFromRequest — the read resolver decides admission, not access; resolve with projectForWrite (declaring the class) or principalAndProject",
					name, h.method, h.path)
			}
		}
	}
	// A regex that matches nothing would make this test pass forever.
	if examined < 80 {
		t.Fatalf("only %d mutating registrations found in the package source; the scan is broken", examined)
	}
}

// TestAViewerCannotCommitOrDecideWithoutADemo is F2's other half — the
// session-only family. The routes above reach the registry; the validation
// writes resolve through authProject and the store then proves MEMBERSHIP only
// (CommitValidationTest and DecideValidationTest call ProjectByIDForUser and no
// role check at all), so a viewer's session could commit the threshold and
// then declare the test passed. That is the finding the audit pinned as F2, and
// it bites instances with NO demo configured — so nothing here mounts
// demoWriteGuard: registerRoutes is wired with pass-through middleware, which is
// strictly less than a no-op guard, and the refusals below therefore come from
// the route's own access decision rather than from the middleware the finding
// said could not cover this case.
//
// The row's own status is the assertion that matters: not "the handler said no"
// but "the store was never reached".
func TestAViewerCannotCommitOrDecideWithoutADemo(t *testing.T) {
	s := openAppTestStore(t)
	ctx := context.Background()
	e := mountServerRoutes(t, s)

	stamp := time.Now().UnixNano()
	owner, err := s.CreateAccount(ctx, fmt.Sprintf("validation-write-%d@test.local", stamp), "Owner", "password-123", "ws", "proj")
	if err != nil {
		t.Fatalf("create owner: %v", err)
	}
	viewer, err := s.CreateAccount(ctx, fmt.Sprintf("validation-viewer-%d@test.local", stamp), "Viewer", "password-123", "vws", "vproj")
	if err != nil {
		t.Fatalf("create viewer: %v", err)
	}
	member, err := s.CreateAccount(ctx, fmt.Sprintf("validation-member-%d@test.local", stamp), "Member", "password-123", "mws", "mproj")
	if err != nil {
		t.Fatalf("create member: %v", err)
	}
	for _, m := range []struct {
		boot storage.AccountBootstrap
		role string
	}{{viewer, "viewer"}, {member, "member"}} {
		if _, err := s.AddWorkspaceMemberByEmail(ctx, owner.User.ID, owner.Workspace.ID, m.boot.User.Email, m.role); err != nil {
			t.Fatalf("add %s: %v", m.role, err)
		}
	}

	cookie := func(userID string) []*http.Cookie {
		_, token, err := s.CreateUserSession(ctx, userID, time.Hour)
		if err != nil {
			t.Fatalf("session: %v", err)
		}
		return []*http.Cookie{{Name: sessionCookieName, Value: token}}
	}
	viewerCaller := caller{name: "viewer session", cookies: cookie(viewer.User.ID), projectID: owner.Project.ID}
	memberCaller := caller{name: "member session", cookies: cookie(member.User.ID), projectID: owner.Project.ID}

	testID, err := s.CreateValidationTest(ctx, storage.ValidationTest{
		ProjectID:   owner.Project.ID,
		Hypothesis:  "a viewer may not commit the threshold",
		MetricEvent: "waitlist.joined",
		TargetCount: 10,
		WindowDays:  7,
		Status:      storage.TestProposed,
	})
	if err != nil {
		t.Fatalf("seed validation test: %v", err)
	}
	status := func() string {
		test, err := s.ValidationTestByID(ctx, owner.User.ID, owner.Project.ID, testID)
		if err != nil {
			t.Fatalf("read validation test: %v", err)
		}
		return test.Status
	}
	base := "/api/validation/tests/" + testID

	// The viewer is refused both writes. Decide is aimed at a row that is still
	// proposed — the state the store answers 400 for — so a handler that
	// validated first would answer 400 here and the assertion below is the
	// authorization answer and nothing else.
	for _, step := range []struct{ name, path, body string }{
		{"commit", base + "/commit", ""},
		{"decide", base + "/decide", `{"status":"passed","note":"viewer"}`},
	} {
		rec := viewerCaller.request(t, e, http.MethodPost, step.path, step.body)
		if rec.Code != http.StatusForbidden {
			t.Errorf("viewer %s = %d %s, want 403", step.name, rec.Code, rec.Body.String())
		}
		if got := status(); got != storage.TestProposed {
			t.Fatalf("a refused viewer %s changed the row: status %q, want %q", step.name, got, storage.TestProposed)
		}
	}

	// The member keeps the flow: the cutover ends a viewer's writes, not the
	// workspace's.
	if rec := memberCaller.request(t, e, http.MethodPost, base+"/commit", ""); rec.Code != http.StatusOK {
		t.Fatalf("member commit = %d %s, want 200", rec.Code, rec.Body.String())
	}
	if got := status(); got != storage.TestCommitted {
		t.Fatalf("member commit left status %q, want %q", got, storage.TestCommitted)
	}
	if rec := viewerCaller.request(t, e, http.MethodPost, base+"/decide", `{"status":"passed","note":"viewer"}`); rec.Code != http.StatusForbidden {
		t.Errorf("viewer decide on a committed row = %d %s, want 403", rec.Code, rec.Body.String())
	}
	if got := status(); got != storage.TestCommitted {
		t.Fatalf("a refused viewer decide changed the row: status %q, want %q", got, storage.TestCommitted)
	}
	if rec := memberCaller.request(t, e, http.MethodPost, base+"/decide", `{"status":"passed","note":"member"}`); rec.Code != http.StatusOK {
		t.Fatalf("member decide = %d %s, want 200", rec.Code, rec.Body.String())
	}
	if got := status(); got != storage.TestPassed {
		t.Fatalf("member decide left status %q, want %q", got, storage.TestPassed)
	}

	// Admission is unchanged: a Bearer that is present and does not resolve is
	// the 401 principalFromRequest answers it with, never a fall-through to the
	// session riding beside it. Without this the route would commit as the
	// member while the caller believed it had presented a credential.
	secondID, err := s.CreateValidationTest(ctx, storage.ValidationTest{
		ProjectID:   owner.Project.ID,
		Hypothesis:  "an unresolvable bearer is a denial, not an absence",
		MetricEvent: "waitlist.joined",
		TargetCount: 10,
		WindowDays:  7,
		Status:      storage.TestProposed,
	})
	if err != nil {
		t.Fatalf("seed second validation test: %v", err)
	}
	deniedBearer := caller{
		name:      "member session behind an unresolvable bearer",
		headers:   map[string]string{"Authorization": "Bearer not-a-management-credential"},
		cookies:   memberCaller.cookies,
		projectID: owner.Project.ID,
	}
	rec := deniedBearer.request(t, e, http.MethodPost, "/api/validation/tests/"+secondID+"/commit", "")
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("commit behind an unresolvable bearer = %d %s, want 401", rec.Code, rec.Body.String())
	}
	if test, err := s.ValidationTestByID(ctx, owner.User.ID, owner.Project.ID, secondID); err != nil || test.Status != storage.TestProposed {
		t.Errorf("a refused bearer committed the row: status %q, err %v", test.Status, err)
	}
}

// TestDeniedCallersGetTheAuthorizationAnswerBeforeValidation is F3: a denied
// caller must not be able to tell a decision about AUTHORIZATION from one about
// the body or the row, and must never be answered as if it had not
// authenticated. /api/op authorizes before it reads the body; the legacy
// surface used to bind JSON and look the row up first, so the same credential
// learned "your body is malformed" (400) where /api/op said "not you" (403) and
// "that row does not exist" (404) where /api/op said the same 403.
//
// The refused calls below are aimed at a malformed body and at ids that do not
// exist, so each one fails if the decision moves back behind a parse or a
// lookup.
func TestDeniedCallersGetTheAuthorizationAnswerBeforeValidation(t *testing.T) {
	s := openAppTestStore(t)
	ctx := context.Background()
	e := mountServerRoutes(t, s)
	registerOpRoutes(e, s, nil, nil)

	stamp := time.Now().UnixNano()
	boot, err := s.CreateAccount(ctx, fmt.Sprintf("refusal-drift-%d@test.local", stamp), "Owner", "password-123", "ws", "proj")
	if err != nil {
		t.Fatalf("create account: %v", err)
	}
	project := boot.Project
	captureKey := project.APIKey
	_, sources, err := s.CreateProjectCredential(ctx, boot.User.ID, project.ID, "sources", []string{"sources:read"})
	if err != nil {
		t.Fatalf("sources credential: %v", err)
	}
	_, author, err := s.CreateProjectCredential(ctx, boot.User.ID, project.ID, "author", []string{"dashboards:write"})
	if err != nil {
		t.Fatalf("author credential: %v", err)
	}
	seedDirectMutatorFixture(t, s, project.ID)

	const missingID = "00000000-0000-0000-0000-000000000000"
	refused := []struct {
		name         string
		method, path string
		body         string
	}{
		// Malformed body: the parse must not run first.
		{"malformed body", http.MethodPost, "/api/cohorts/audiences", `{"label":`},
		{"malformed body, saved query", http.MethodPost, "/api/saved-queries", `{"natural_language":`},
		{"malformed body, mapping", http.MethodPut, "/api/subscription/mapping", `{"start_event":`},
		// Missing row: the lookup must not run first.
		{"missing audience", http.MethodPut, "/api/cohorts/audiences/" + missingID, `{"label":"x","kind":"paid"}`},
		{"missing audience, delete", http.MethodDelete, "/api/cohorts/audiences/" + missingID, ""},
		{"missing saved query", http.MethodPatch, "/api/saved-queries/" + missingID, `{"natural_language":"x"}`},
		{"missing saved query, delete", http.MethodDelete, "/api/saved-queries/" + missingID, ""},
		{"missing template", http.MethodPost, "/api/templates/" + missingID + "/apply", `{}`},
	}
	for _, tc := range refused {
		rec := callREST(t, e, tc.method, tc.path, tc.body, sources)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s with a sources:read credential = %d %s, want 403 (the authorization answer, not a parse or existence verdict)",
				tc.name, rec.Code, rec.Body.String())
		}
	}

	// The registry surface answers the same credential the same way, so the two
	// surfaces agree on the status of a refused caller.
	if rec := postJSON(t, e, "/api/op/create_dashboard", `{"name":"denied"}`, bearer(sources)); rec.Code != http.StatusForbidden {
		t.Errorf("sources:read create_dashboard via /api/op = %d %s, want 403", rec.Code, rec.Body.String())
	}

	// 403 is the refusal of a caller that authenticated. 401 stays reserved for
	// a request carrying no usable credential of its own.
	if rec := callREST(t, e, http.MethodGet, "/api/activity?project_id="+project.ID+"&api_key="+captureKey, "", ""); rec.Code != http.StatusForbidden {
		t.Errorf("capture credential on a management read = %d %s, want 403 — never 401 for a credential that authenticated", rec.Code, rec.Body.String())
	}
	if rec := callREST(t, e, http.MethodGet, "/api/activity?project_id="+project.ID, "", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("anonymous management read = %d %s, want 401", rec.Code, rec.Body.String())
	}

	// A management credential that HOLDS the class keeps its access: the
	// cutover ends undocumented machine-key writes, not scoped ones.
	rec := callREST(t, e, http.MethodPost, "/api/saved-queries",
		`{"natural_language":"Authorised","generated_sql":"SELECT 1 AS n","verified":true}`, author)
	if rec.Code != http.StatusCreated {
		t.Errorf("dashboards:write credential create saved query = %d %s, want 201", rec.Code, rec.Body.String())
	}
}

// TestAViewersQueryRunDoesNotWriteTheCacheWithoutADemo is the other half of the
// run route's contract, and the half that has no demo guard behind it: running a
// saved query is an analytics read a viewer is entitled to, but caching the
// result is an UPDATE to the owner's saved_queries row. The route used to ask
// the demo guard's read-only marker, which only exists when a demo is
// configured — so on an instance with none, a viewer's run cached its result
// into the owner's row. The registry is asked instead, and the owner's own run
// still caches, so the fence is not "never cache".
func TestAViewersQueryRunDoesNotWriteTheCacheWithoutADemo(t *testing.T) {
	s := openAppTestStore(t)
	ctx := context.Background()
	e := mountServerRoutes(t, s)

	stamp := time.Now().UnixNano()
	owner, err := s.CreateAccount(ctx, fmt.Sprintf("query-cache-%d@test.local", stamp), "Owner", "password-123", "ws", "proj")
	if err != nil {
		t.Fatalf("create owner: %v", err)
	}
	viewer, err := s.CreateAccount(ctx, fmt.Sprintf("query-cache-viewer-%d@test.local", stamp), "Viewer", "password-123", "vws", "vproj")
	if err != nil {
		t.Fatalf("create viewer: %v", err)
	}
	if _, err := s.AddWorkspaceMemberByEmail(ctx, owner.User.ID, owner.Workspace.ID, viewer.User.Email, "viewer"); err != nil {
		t.Fatalf("add viewer: %v", err)
	}
	session := func(userID string) []*http.Cookie {
		_, token, err := s.CreateUserSession(ctx, userID, time.Hour)
		if err != nil {
			t.Fatalf("session: %v", err)
		}
		return []*http.Cookie{{Name: sessionCookieName, Value: token}}
	}
	viewerCaller := caller{name: "viewer session", cookies: session(viewer.User.ID), projectID: owner.Project.ID}
	ownerCaller := caller{name: "owner session", cookies: session(owner.User.ID), projectID: owner.Project.ID}

	query, err := s.CreateSavedQuery(ctx, owner.Project.ID, "Cache probe", countEventsSQL, true)
	if err != nil {
		t.Fatalf("seed saved query: %v", err)
	}
	cached := func() []byte {
		queries, err := s.ListSavedQueries(ctx, owner.Project.ID)
		if err != nil {
			t.Fatalf("list saved queries: %v", err)
		}
		for _, q := range queries {
			if q.ID == query.ID {
				return q.ResultCache
			}
		}
		t.Fatalf("saved query %s is gone", query.ID)
		return nil
	}
	run := "/api/saved-queries/" + query.ID + "/run"

	// The store reads a never-cached row back as the JSON literal null, so that
	// — not an empty slice — is what "nothing was written" looks like.
	const notCached = "null"

	rec := viewerCaller.request(t, e, http.MethodPost, run, `{}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("viewer run = %d %s, want 200 — a viewer may execute a saved query", rec.Code, rec.Body.String())
	}
	if got := string(cached()); got != notCached {
		t.Errorf("a viewer's run cached its result into the owner's row: %s", got)
	}

	rec = ownerCaller.request(t, e, http.MethodPost, run, `{}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("owner run = %d %s, want 200", rec.Code, rec.Body.String())
	}
	ownerCached := cached()
	if string(ownerCached) == notCached {
		t.Fatalf("the owner's own run did not refresh the cache — the route no longer caches for anyone")
	}

	if rec := viewerCaller.request(t, e, http.MethodPost, run, `{}`); rec.Code != http.StatusOK {
		t.Fatalf("viewer re-run = %d %s, want 200", rec.Code, rec.Body.String())
	}
	if got := cached(); !bytes.Equal(got, ownerCached) {
		t.Errorf("a viewer's run rewrote the owner's cached result: %s, want %s", got, ownerCached)
	}
}
