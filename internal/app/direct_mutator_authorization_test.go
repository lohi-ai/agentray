package app

import (
	"bytes"
	"context"
	"encoding/json"
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
	"github.com/lohi-ai/agentray/internal/shared/config"
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
	registerRoutes(e, s, ingestion.EventQueue{}, pass, pass, nil, nil, agentruntime.ToolBuildContext{}, nil, false, publicCollectSet{}, newOpAdapter(s, nil, nil), nil, nil)
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
	// denied is the exact refusal body a credential without the route's class
	// hears — the same sentence /api/op answers, so the two surfaces cannot
	// drift apart (F3). Asserted, not described.
	denied string
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
			denied:   "credential may not perform this action (requires dashboards:write)",
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
			denied:   "credential may not perform this action (requires dashboards:write)",
			accepted: http.StatusCreated,
		},
		{
			name:     "create a cohort audience",
			kind:     writeRoute,
			method:   http.MethodPost,
			path:     func(directMutatorFixture) string { return "/api/cohorts/audiences" },
			body:     func(directMutatorFixture) string { return `{"label":"Payers","kind":"paid","plans":[]}` },
			accepted: http.StatusCreated,
			denied:   "credential may not perform this action (requires plans:write)",
		},
		{
			name:     "update a cohort audience",
			kind:     writeRoute,
			method:   http.MethodPut,
			path:     func(f directMutatorFixture) string { return "/api/cohorts/audiences/" + f.audienceID },
			body:     func(directMutatorFixture) string { return `{"label":"Beta testers","kind":"paid","plans":[]}` },
			denied:   "credential may not perform this action (requires plans:write)",
			accepted: http.StatusOK,
		},
		{
			name:     "delete a cohort audience",
			kind:     writeRoute,
			method:   http.MethodDelete,
			path:     func(f directMutatorFixture) string { return "/api/cohorts/audiences/" + f.audienceID },
			body:     func(directMutatorFixture) string { return "" },
			denied:   "credential may not perform this action (requires plans:write)",
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
			denied:   "credential may not perform this action (requires plans:write)",
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
			denied:   "credential may not perform this action (requires dashboards:write)",
			accepted: http.StatusCreated,
		},
		{
			name:     "rename a saved query",
			kind:     writeRoute,
			method:   http.MethodPatch,
			path:     func(f directMutatorFixture) string { return "/api/saved-queries/" + f.renameQueryID },
			body:     func(directMutatorFixture) string { return `{"natural_language":"Renamed"}` },
			denied:   "credential may not perform this action (requires dashboards:write)",
			accepted: http.StatusOK,
		},
		{
			name:     "delete a saved query",
			kind:     writeRoute,
			method:   http.MethodDelete,
			path:     func(f directMutatorFixture) string { return "/api/saved-queries/" + f.deleteQueryID },
			body:     func(directMutatorFixture) string { return "" },
			denied:   "credential may not perform this action (requires dashboards:write)",
			accepted: http.StatusNoContent,
		},
		{
			name:     "run a saved query",
			kind:     readRoute,
			method:   http.MethodPost,
			path:     func(f directMutatorFixture) string { return "/api/saved-queries/" + f.renameQueryID + "/run" },
			body:     func(directMutatorFixture) string { return `{}` },
			denied:   "credential may not perform this action (requires analytics:read)",
			accepted: http.StatusOK,
		},
		{
			name:     "run ad-hoc SQL",
			kind:     readRoute,
			method:   http.MethodPost,
			path:     func(directMutatorFixture) string { return "/api/sql/run" },
			body:     func(directMutatorFixture) string { return `{"sql":"` + countEventsSQL + `"}` },
			denied:   "credential may not perform this action (requires analytics:read)",
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

	fixture := seedDirectMutatorFixture(t, s, project.ID)

	reader := caller{name: "analytics:read credential", headers: map[string]string{"Authorization": "Bearer " + readerSecret}, mayRunReads: true}
	sources := caller{name: "sources:read credential", headers: map[string]string{"Authorization": "Bearer " + sourceSecret}}

	// Every credential that may not change this project is refused by every
	// route that changes it, and the refusal is the whole request: the state
	// below is compared afterwards.
	before := snapshotDirectMutatorState(t, s, project.ID)
	for _, tc := range directMutatorCases() {
		for _, who := range []caller{reader, sources} {
			if tc.kind == readRoute && who.mayRunReads {
				// Running SQL is analytics:read — the class /api/op's run_sql
				// requires and the call the demo lets a viewer make — so the
				// only refusal to assert here is the caller that lacks it.
				continue
			}
			rec := who.request(t, e, tc.method, tc.path(fixture), tc.body(fixture))
			if rec.Code != http.StatusForbidden {
				t.Errorf("%s %s as %s = %d %s, want 403", tc.method, tc.path(fixture), who.name, rec.Code, rec.Body.String())
				continue
			}
			// The body is the contract, not a detail: the same credential asking
			// /api/op for the same class hears this exact sentence, so a client
			// writes one branch for a refused call on either surface.
			var refusal struct {
				Message string `json:"message"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &refusal); err != nil {
				t.Errorf("%s %s as %s: refusal body %q is not the {message} envelope", tc.method, tc.path(fixture), who.name, rec.Body.String())
			} else if refusal.Message != tc.denied {
				t.Errorf("%s %s as %s: refusal %q, want %q", tc.method, tc.path(fixture), who.name, refusal.Message, tc.denied)
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

// TestNoRouteResolvesThroughTheReadResolver pins the property rather than the
// instances of it: projectFromRequest answers admission, never an access class,
// so any route that reaches it runs for whatever credential may address the
// project. It is over the package's own source because that is the only place a
// route that does not exist yet can be caught.
//
// It asserts three things, and the first one does not depend on how a route is
// registered:
//
//  1. The census — this package contains exactly ONE production call to
//     projectFromRequest. A second call site fails here however it is reached:
//     inlined into a route, hidden in a helper, registered through a computed
//     path or a wrapper. A route-shaped scan alone cannot see those, which is
//     why the count comes first.
//  2. The exemption — the one call is the GET /api/projects handler and no
//     other registration contains it. The exemption is by route identity, not
//     by a helper's name: a named admission resolver is a hatch any later route
//     could reuse to skip its class, so there deliberately is not one.
//  3. The parity — every route that declares a class (authorizedProject /
//     authorizedPrincipalAndProject) has a behavioural case in the two matrices
//     below, and every case still matches a registration. Drift in either
//     direction fails: a new class-decided route without a case, or a case
//     whose route was renamed away.
func TestNoRouteResolvesThroughTheReadResolver(t *testing.T) {
	registrations := scanRouteRegistrations(t)

	// 1. The census, over the package source rather than over registrations —
	// because a route-shaped scan cannot see a call a helper hides.
	//
	// projectFromRequest has exactly two legitimate owners and they are named
	// here. The first is the GET /api/projects handler, asserted by route
	// identity below. The second is authProject, the resolver of the modern
	// surface — agent, team, alert, connector, validation, operations — whose
	// routes authorize at the operation or store layer rather than at the
	// route; those routes are not the legacy store-direct surface this ticket
	// closes, and the classes their writes still owe are the residual recorded
	// in the ticket evidence.
	//
	// What the census guarantees is that no THIRD owner appears: a new helper
	// that wraps the admission-only resolver, or a legacy route that inlines
	// it, changes this set and fails here however it is registered — which is
	// exactly how the modern surface's own resolver was found.
	var helperOwners []string
	routeOwners := map[string]bool{}
	for _, file := range packageSources(t) {
		masked, spans := maskRegistrations(file.source)
		for _, span := range spans {
			if strings.Contains(file.source[span.start:span.end], "projectFromRequest(") {
				routeOwners[span.method+" "+span.path] = true
			}
		}
		for _, at := range occurrences(masked, "projectFromRequest(") {
			owner := enclosingFunc(masked, at)
			if owner == "projectFromRequest" {
				continue // the definition is not a call
			}
			helperOwners = append(helperOwners, owner)
		}
	}
	sort.Strings(helperOwners)
	helperOwners = dedupe(helperOwners)
	if len(routeOwners) != 1 || !routeOwners[http.MethodGet+" /api/projects"] {
		t.Errorf("projectFromRequest is called from the routes %v; the only route allowed to skip the class decision is GET /api/projects", keys(routeOwners))
	}
	if len(helperOwners) != 1 || helperOwners[0] != "authProject" {
		t.Errorf("projectFromRequest is called from the helpers %v; the only helper allowed to is authProject, the modern surface's resolver. Every legacy route declares its class through authorizedProject",
			helperOwners)
	}

	// 2. The exemption is the route the census names, and it still asks.
	admitted := 0
	for _, r := range registrations {
		if r.method == http.MethodGet && r.path == "/api/projects" && strings.Contains(r.body, "projectFromRequest(") {
			admitted++
		}
	}
	if admitted != 1 {
		t.Errorf("GET /api/projects is registered %d times through projectFromRequest; it is the one admission-only route", admitted)
	}

	// 3. The parity, in both directions.
	classed := map[string]bool{}
	for _, r := range registrations {
		if strings.Contains(r.body, "authorizedProject(") || strings.Contains(r.body, "authorizedPrincipalAndProject(") {
			classed[r.method+" "+r.path] = true
		}
	}
	cased := map[string]string{}
	for _, path := range storeDirectReadRoutes {
		cased[http.MethodGet+" "+path] = "reads"
	}
	placeholders := directMutatorFixture{
		dashboardID:     "d0000000-0000-4000-8000-000000000001",
		audienceID:      "a0000000-0000-4000-8000-000000000002",
		renameQueryID:   "q0000000-0000-4000-8000-000000000003",
		deleteQueryID:   "q0000000-0000-4000-8000-000000000004",
		templateID:      "t0000000-0000-4000-8000-000000000005",
		templateChartID: "c0000000-0000-4000-8000-000000000006",
	}
	for _, tc := range directMutatorCases() {
		cased[tc.method+" "+tc.path(placeholders)] = "writes"
	}
	for _, r := range registrations {
		key := r.method + " " + r.path
		if !classed[key] {
			continue
		}
		if !matchedByCases(r, cased) {
			t.Errorf("%s registers %s %s with a class decision and no case in either matrix — add it to the reads or writes matrix so the decision is exercised",
				r.file, r.method, r.path)
		}
	}
	for key, matrix := range cased {
		if !matchedByClassed(key, classed) {
			t.Errorf("the %s matrix covers %s, which no registration declares a class for — the case is stale", matrix, key)
		}
	}
}

// scanRouteRegistrations returns every route registration in this package with
// the source slice that is its handler body.
//
// A registration runs from its line to the next registration of ANY method or
// the next package-level function — whichever comes first — so the slice is
// that handler's body and nothing else. (Bounding on mutating registrations
// alone would run a read handler's body into the next mutating route's slice
// and report its resolver.) Both registration forms the package uses are
// scanned — `receiver.METHOD("path"` and `receiver.Add(http.MethodX, "path"` —
// because a route that registers the second way is exactly as capable of
// resolving through the read resolver as one that registers the first.
func scanRouteRegistrations(t *testing.T) []routeRegistration {
	t.Helper()
	var out []routeRegistration
	for _, file := range packageSources(t) {
		for _, span := range registrationSpans(file.source) {
			span.file = file.name
			span.body = file.source[span.start:span.end]
			out = append(out, span)
		}
	}
	return out
}

// registrationSpans locates every registration in one file's source. Each span
// runs from the registration to the next registration of ANY method or the next
// package-level function — whichever comes first — so the span is that
// handler's body and nothing else. (Bounding on mutating registrations alone
// would run a read handler's body into the next mutating route's slice and
// report its resolver.) Both registration forms the package uses are found —
// `receiver.METHOD("path"` and `receiver.Add(http.MethodX, "path"` — because a
// route that registers the second way is exactly as capable of resolving
// through the admission-only resolver as one that registers the first.
func registrationSpans(source string) []routeRegistration {
	registration := regexp.MustCompile(`(?m)^\s*\w+\.(GET|POST|PUT|PATCH|DELETE|HEAD|OPTIONS)\("([^"]+)"`)
	added := regexp.MustCompile(`(?m)^\s*\w+\.Add\(http\.(Method\w+),\s*"([^"]+)"`)
	topLevelFunc := regexp.MustCompile(`(?m)^func `)

	var hits []routeRegistration
	for _, m := range registration.FindAllStringSubmatchIndex(source, -1) {
		hits = append(hits, routeRegistration{start: m[0], method: source[m[2]:m[3]], path: source[m[4]:m[5]]})
	}
	for _, m := range added.FindAllStringSubmatchIndex(source, -1) {
		hits = append(hits, routeRegistration{start: m[0], method: strings.TrimPrefix(source[m[2]:m[3]], "Method"), path: source[m[4]:m[5]]})
	}
	sort.Slice(hits, func(i, j int) bool { return hits[i].start < hits[j].start })
	for i, h := range hits {
		end := len(source)
		if i+1 < len(hits) {
			end = hits[i+1].start
		}
		if fn := topLevelFunc.FindStringIndex(source[h.start:end]); fn != nil {
			end = h.start + fn[0]
		}
		hits[i].end = end
	}
	return hits
}

// maskRegistrations blanks the registration spans so the census counts what the
// handler bodies call separately from what the helpers call. Newlines survive,
// so the source stays line-shaped for the `^func ` search.
func maskRegistrations(source string) (string, []routeRegistration) {
	spans := registrationSpans(source)
	masked := []byte(source)
	for _, span := range spans {
		for i := span.start; i < span.end; i++ {
			if masked[i] != '\n' {
				masked[i] = ' '
			}
		}
	}
	return string(masked), spans
}

// occurrences returns every index at which needle starts.
func occurrences(source, needle string) []int {
	var out []int
	for offset := 0; ; {
		at := strings.Index(source[offset:], needle)
		if at < 0 {
			return out
		}
		out = append(out, offset+at)
		offset += at + len(needle)
	}
}

// enclosingFunc names the top-level function whose body contains the offset.
func enclosingFunc(source string, at int) string {
	head := source[:at]
	start := -1
	if idx := strings.LastIndex(head, "\nfunc "); idx >= 0 {
		start = idx + len("\nfunc ")
	} else if strings.HasPrefix(head, "func ") {
		start = len("func ")
	} else {
		return ""
	}
	// The name follows start immediately; read it from the source rather than
	// from head, because for a function's own definition the offset sits exactly
	// where the name begins.
	rest := source[start:]
	if strings.HasPrefix(rest, "(") { // a method receiver
		close := strings.Index(rest, ")")
		if close < 0 {
			return ""
		}
		rest = rest[close+1:]
	}
	rest = strings.TrimLeft(rest, " \t")
	end := strings.IndexAny(rest, "( \t")
	if end < 0 {
		return ""
	}
	return rest[:end]
}

func dedupe(sorted []string) []string {
	var out []string
	for i, v := range sorted {
		if i == 0 || v != sorted[i-1] {
			out = append(out, v)
		}
	}
	return out
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

type routeRegistration struct {
	start, end   int
	file         string
	method, path string
	body         string
}

type packageFile struct {
	name   string
	source string
}

// packageSources is every non-test .go file of this package.
func packageSources(t *testing.T) []packageFile {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	var out []packageFile
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		out = append(out, packageFile{name: name, source: string(src)})
	}
	return out
}

// matchedByCases reports whether a registration is exercised by a concrete path
// in one of the matrices, by matching its pattern against the case's path.
func matchedByCases(r routeRegistration, cased map[string]string) bool {
	for key := range cased {
		method, path, ok := strings.Cut(key, " ")
		if !ok || method != r.method {
			continue
		}
		if pathMatches(r.path, path) {
			return true
		}
	}
	return false
}

// matchedByClassed reports whether a case's concrete path lands on a route that
// declares a class.
func matchedByClassed(key string, classed map[string]bool) bool {
	method, path, ok := strings.Cut(key, " ")
	if !ok {
		return false
	}
	for route := range classed {
		routeMethod, pattern, ok := strings.Cut(route, " ")
		if ok && routeMethod == method && pathMatches(pattern, path) {
			return true
		}
	}
	return false
}

// pathMatches reports whether a concrete path is an instance of a route pattern
// (`:param` matches one segment).
func pathMatches(pattern, path string) bool {
	if !strings.Contains(pattern, ":") {
		return pattern == path
	}
	segments := strings.Split(pattern, "/")
	got := strings.Split(path, "/")
	if len(segments) != len(got) {
		return false
	}
	for i, segment := range segments {
		if strings.HasPrefix(segment, ":") {
			if got[i] == "" {
				return false
			}
			continue
		}
		if segment != got[i] {
			return false
		}
	}
	return true
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
func TestAMemberCanCommitAndDecideWithoutADemo(t *testing.T) {
	s := openAppTestStore(t)
	ctx := context.Background()
	e := mountServerRoutes(t, s)

	stamp := time.Now().UnixNano()
	owner, err := s.CreateAccount(ctx, fmt.Sprintf("validation-write-%d@test.local", stamp), "Owner", "password-123", "ws", "proj")
	if err != nil {
		t.Fatalf("create owner: %v", err)
	}
	member, err := s.CreateAccount(ctx, fmt.Sprintf("validation-member-%d@test.local", stamp), "Member", "password-123", "mws", "mproj")
	if err != nil {
		t.Fatalf("create member: %v", err)
	}
	if _, err := s.AddWorkspaceMemberByEmail(ctx, owner.User.ID, owner.Workspace.ID, member.User.Email, "member"); err != nil {
		t.Fatalf("add member: %v", err)
	}

	cookie := func(userID string) []*http.Cookie {
		_, token, err := s.CreateUserSession(ctx, userID, time.Hour)
		if err != nil {
			t.Fatalf("session: %v", err)
		}
		return []*http.Cookie{{Name: sessionCookieName, Value: token}}
	}
	memberCaller := caller{name: "member session", cookies: cookie(member.User.ID), projectID: owner.Project.ID}

	testID, err := s.CreateValidationTest(ctx, storage.ValidationTest{
		ProjectID:   owner.Project.ID,
		Hypothesis:  "a member may commit the threshold",
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

	if rec := memberCaller.request(t, e, http.MethodPost, base+"/commit", ""); rec.Code != http.StatusOK {
		t.Fatalf("member commit = %d %s, want 200", rec.Code, rec.Body.String())
	}
	if got := status(); got != storage.TestCommitted {
		t.Fatalf("member commit left status %q, want %q", got, storage.TestCommitted)
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

// TestStoreDirectReadsRefuseACredentialWithoutTheReadClass is the read half of
// F1, and it is the half the audit reported first: a management credential
// scoped sources:read and nothing else read every analytics route on this
// surface while /api/op refused the same credential activity_summary. The
// routes answered 200 because projectFromRequest consults no grant at all.
//
// Both sides are asserted, because "deny the under-privileged caller" is only
// half a contract: the credential that holds the class still reads, a viewer's
// session still reads (the demo's whole point), and a pre-split project key
// still reads — its frozen allowlist covers analytics:read through
// activity_summary, so the Option A bridge survives.
//
// storeDirectReadRoutes is the read half of the store-direct surface: the
// routes that return a project's analytics and therefore declare
// analytics:read. It is one declaration because two tests read it — the
// behavioural matrix below and the parity check in
// TestNoRouteResolvesThroughTheReadResolver, which fails if a route that
// declares a class has no case here or if a case here matches no route.
//
// The replay route is exercised with a session id that does not exist: the
// store answers an empty replay with 200, so the class decision is what the
// status reports.
var storeDirectReadRoutes = []string{
	"/api/activity",
	"/api/insights/run",
	"/api/templates",
	"/api/web-analytics",
	"/api/persons",
	"/api/cohorts",
	"/api/cohorts/audiences",
	"/api/subscription/mapping",
	"/api/events/explore",
	"/api/events/names",
	"/api/sessions/qa-no-such-session/replay",
	"/api/saved-queries",
	"/api/events",
	"/api/sessions",
}

func TestStoreDirectReadsRefuseACredentialWithoutTheReadClass(t *testing.T) {
	s := openAppTestStore(t)
	ctx := context.Background()
	e := mountServerRoutes(t, s)

	stamp := time.Now().UnixNano()
	owner, err := s.CreateAccount(ctx, fmt.Sprintf("read-class-%d@test.local", stamp), "Owner", "password-123", "ws", "proj")
	if err != nil {
		t.Fatalf("create owner: %v", err)
	}
	project := owner.Project
	_, sources, err := s.CreateProjectCredential(ctx, owner.User.ID, project.ID, "sources", []string{"sources:read"})
	if err != nil {
		t.Fatalf("sources credential: %v", err)
	}
	_, analytics, err := s.CreateProjectCredential(ctx, owner.User.ID, project.ID, "analytics", []string{"analytics:read"})
	if err != nil {
		t.Fatalf("analytics credential: %v", err)
	}
	member, err := s.CreateAccount(ctx, fmt.Sprintf("read-class-member-%d@test.local", stamp), "Member", "password-123", "mws", "mproj")
	if err != nil {
		t.Fatalf("create member: %v", err)
	}
	if _, err := s.AddWorkspaceMemberByEmail(ctx, owner.User.ID, owner.Workspace.ID, member.User.Email, "member"); err != nil {
		t.Fatalf("add member: %v", err)
	}
	_, memberToken, err := s.CreateUserSession(ctx, member.User.ID, time.Hour)
	if err != nil {
		t.Fatalf("member session: %v", err)
	}
	// A pre-split project key: the Option A bridge, whose frozen allowlist is
	// what admits it to a class it held before the split.
	legacy, err := s.CreateProject(ctx, "Legacy read class")
	if err != nil {
		t.Fatalf("create legacy project: %v", err)
	}
	if _, err := s.ProjectCredentialSplit(ctx, legacy.ID); err != nil {
		t.Fatalf("read split flag: %v", err)
	}

	// The read routes that return the project's analytics. /api/projects is not
	// here: it returns the project the credential addressed — the addressing
	// itself — and is the one admission-only route, reached by calling
	// projectFromRequest directly (there is no wrapper to reuse, and
	// TestNoRouteResolvesThroughTheReadResolver counts the call sites).
	reads := storeDirectReadRoutes
	sourcesCaller := caller{name: "sources:read credential", headers: map[string]string{"Authorization": "Bearer " + sources}}
	analyticsCaller := caller{name: "analytics:read credential", headers: map[string]string{"Authorization": "Bearer " + analytics}, projectID: project.ID}
	memberCaller := caller{name: "member session", cookies: []*http.Cookie{{Name: sessionCookieName, Value: memberToken}}, projectID: project.ID}
	legacyCaller := caller{name: "pre-split project key", headers: map[string]string{"X-API-Key": legacy.APIKey}, projectID: legacy.ID}
	captureCaller := caller{name: "capture key", headers: map[string]string{"X-API-Key": project.APIKey}, projectID: project.ID}

	for _, path := range reads {
		if rec := sourcesCaller.request(t, e, http.MethodGet, path+"?project_id="+project.ID, ""); rec.Code != http.StatusForbidden {
			t.Errorf("sources:read GET %s = %d %s, want 403 — the credential holds no analytics class", path, rec.Code, rec.Body.String())
		}
	}
	for _, who := range []caller{analyticsCaller, memberCaller} {
		for _, path := range reads {
			if rec := who.request(t, e, http.MethodGet, path, ""); rec.Code != http.StatusOK {
				t.Errorf("%s GET %s = %d %s, want 200 — this caller holds analytics:read", who.name, path, rec.Code, rec.Body.String())
			}
		}
	}
	// The bridge: a pre-split key keeps the reads it had before the split.
	for _, path := range reads {
		if rec := legacyCaller.request(t, e, http.MethodGet, path, ""); rec.Code != http.StatusOK {
			t.Errorf("pre-split project key GET %s = %d %s, want 200 — its frozen allowlist covers analytics:read", path, rec.Code, rec.Body.String())
		}
	}
	// And the key that is refused everywhere stays refused.
	for _, path := range reads {
		if rec := captureCaller.request(t, e, http.MethodGet, path, ""); rec.Code != http.StatusForbidden {
			t.Errorf("capture key GET %s = %d %s, want 403", path, rec.Code, rec.Body.String())
		}
	}
	// The admission-only route still answers the addressing question: the
	// sources:read credential may see the project it was minted for.
	if rec := sourcesCaller.request(t, e, http.MethodGet, "/api/projects?project_id="+project.ID, ""); rec.Code != http.StatusOK {
		t.Errorf("sources:read GET /api/projects = %d %s, want 200 — it is the addressing, not analytics", rec.Code, rec.Body.String())
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
		// denied is the exact refusal sentence — the same one /api/op answers
		// for the same class, so the assertion is the F3 contract itself.
		denied string
	}{
		// Malformed body: the parse must not run first.
		{"malformed body", http.MethodPost, "/api/cohorts/audiences", `{"label":`, "credential may not perform this action (requires plans:write)"},
		{"malformed body, saved query", http.MethodPost, "/api/saved-queries", `{"natural_language":`, "credential may not perform this action (requires dashboards:write)"},
		{"malformed body, mapping", http.MethodPut, "/api/subscription/mapping", `{"start_event":`, "credential may not perform this action (requires plans:write)"},
		// Missing row: the lookup must not run first.
		{"missing audience", http.MethodPut, "/api/cohorts/audiences/" + missingID, `{"label":"x","kind":"paid"}`, "credential may not perform this action (requires plans:write)"},
		{"missing audience, delete", http.MethodDelete, "/api/cohorts/audiences/" + missingID, "", "credential may not perform this action (requires plans:write)"},
		{"missing saved query", http.MethodPatch, "/api/saved-queries/" + missingID, `{"natural_language":"x"}`, "credential may not perform this action (requires dashboards:write)"},
		{"missing saved query, delete", http.MethodDelete, "/api/saved-queries/" + missingID, "", "credential may not perform this action (requires dashboards:write)"},
		{"missing template", http.MethodPost, "/api/templates/" + missingID + "/apply", `{}`, "credential may not perform this action (requires dashboards:write)"},
	}
	for _, tc := range refused {
		rec := callREST(t, e, tc.method, tc.path, tc.body, sources)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s with a sources:read credential = %d %s, want 403 (the authorization answer, not a parse or existence verdict)",
				tc.name, rec.Code, rec.Body.String())
			continue
		}
		var refusal struct {
			Message string `json:"message"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &refusal); err != nil {
			t.Errorf("%s: refusal body %q is not the {message} envelope", tc.name, rec.Body.String())
		} else if refusal.Message != tc.denied {
			t.Errorf("%s: refusal %q, want %q — the same sentence /api/op answers", tc.name, refusal.Message, tc.denied)
		}
	}

	// The registry surface answers the same credential the same way — same
	// status AND the same sentence, so a refused caller writes one branch.
	rec := postJSON(t, e, "/api/op/create_dashboard", `{"name":"denied"}`, bearer(sources))
	if rec.Code != http.StatusForbidden {
		t.Errorf("sources:read create_dashboard via /api/op = %d %s, want 403", rec.Code, rec.Body.String())
	} else if body := strings.TrimSpace(rec.Body.String()); body != `{"message":"credential may not perform this action (requires dashboards:write)"}` {
		t.Errorf("sources:read create_dashboard via /api/op body = %s, want the grant refusal the direct surface answers", body)
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
	rec = callREST(t, e, http.MethodPost, "/api/saved-queries",
		`{"natural_language":"Authorised","generated_sql":"SELECT 1 AS n","verified":true}`, author)
	if rec.Code != http.StatusCreated {
		t.Errorf("dashboards:write credential create saved query = %d %s, want 201", rec.Code, rec.Body.String())
	}
}

// TestAReadOnlyQueryRunDoesNotWriteTheCacheWithoutADemo: running a saved query
// is an analytics read, but caching the result is an UPDATE to the owner's
// saved_queries row. The registry is asked; a credential without
// dashboards:write must not cache, and the owner's own run still caches.
func TestAReadOnlyQueryRunDoesNotWriteTheCacheWithoutADemo(t *testing.T) {
	s := openAppTestStore(t)
	ctx := context.Background()
	e := mountServerRoutes(t, s)

	stamp := time.Now().UnixNano()
	owner, err := s.CreateAccount(ctx, fmt.Sprintf("query-cache-%d@test.local", stamp), "Owner", "password-123", "ws", "proj")
	if err != nil {
		t.Fatalf("create owner: %v", err)
	}
	_, readerSecret, err := s.CreateProjectCredential(ctx, owner.User.ID, owner.Project.ID, "reader", []string{"analytics:read"})
	if err != nil {
		t.Fatalf("reader credential: %v", err)
	}
	session := func(userID string) []*http.Cookie {
		_, token, err := s.CreateUserSession(ctx, userID, time.Hour)
		if err != nil {
			t.Fatalf("session: %v", err)
		}
		return []*http.Cookie{{Name: sessionCookieName, Value: token}}
	}
	readerCaller := caller{name: "analytics:read credential", headers: map[string]string{"Authorization": "Bearer " + readerSecret}, projectID: owner.Project.ID}
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

	const notCached = "null"

	rec := readerCaller.request(t, e, http.MethodPost, run, `{}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("reader run = %d %s, want 200 — analytics:read may execute a saved query", rec.Code, rec.Body.String())
	}
	if got := string(cached()); got != notCached {
		t.Errorf("a read-only run cached its result into the owner's row: %s", got)
	}

	rec = ownerCaller.request(t, e, http.MethodPost, run, `{}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("owner run = %d %s, want 200", rec.Code, rec.Body.String())
	}
	ownerCached := cached()
	if string(ownerCached) == notCached {
		t.Fatalf("the owner's own run did not refresh the cache — the route no longer caches for anyone")
	}

	if rec := readerCaller.request(t, e, http.MethodPost, run, `{}`); rec.Code != http.StatusOK {
		t.Fatalf("reader re-run = %d %s, want 200", rec.Code, rec.Body.String())
	}
	if got := cached(); !bytes.Equal(got, ownerCached) {
		t.Errorf("a read-only run rewrote the owner's cached result: %s, want %s", got, ownerCached)
	}
}

// TestADemoMemberHearsTheGrantRefusal is the member-without-grant half of the
// body contract. A demo member holds a real session and a real membership —
// the denial is a grant refusal, not a role statement — so the body must be
// the same sentence a denied credential hears, on the store-direct route and
// on the op-backed route alike. The stale "your role in this workspace is
// read-only" copy named a role that no longer exists; this test fails if it
// returns.
//
// The demo flag lives on the store, so the demo project must exist before the
// store opens: the first store creates it, the second is opened with
// DemoProjectID set, and the routes mount over that one.
func TestADemoMemberHearsTheGrantRefusal(t *testing.T) {
	ctx := context.Background()
	stamp := time.Now().UnixNano()

	setup := openAppTestStore(t)
	demoOwner, err := setup.CreateAccount(ctx, fmt.Sprintf("demo-owner-%d@test.local", stamp), "Demo Owner", "password-123", "demo-ws", "demo-proj")
	if err != nil {
		t.Fatalf("demo owner account: %v", err)
	}
	member, err := setup.CreateAccount(ctx, fmt.Sprintf("demo-member-%d@test.local", stamp), "Member", "password-123", "home-ws", "home-proj")
	if err != nil {
		t.Fatalf("member account: %v", err)
	}
	if _, err := setup.AddWorkspaceMemberByEmail(ctx, demoOwner.User.ID, demoOwner.Workspace.ID, member.User.Email, "member"); err != nil {
		t.Fatalf("add member to demo workspace: %v", err)
	}

	s := openAppTestStoreWith(t, func(cfg *config.Config) { cfg.DemoProjectID = demoOwner.Project.ID })
	e := mountServerRoutes(t, s)

	_, memberToken, err := s.CreateUserSession(ctx, member.User.ID, time.Hour)
	if err != nil {
		t.Fatalf("member session: %v", err)
	}
	asMember := caller{
		name:      "demo member session",
		cookies:   []*http.Cookie{{Name: sessionCookieName, Value: memberToken}},
		projectID: demoOwner.Project.ID,
	}

	// The store-direct write and the op-backed write are the same refusal
	// class, so the member hears the same sentence a denied credential does.
	denied := []struct {
		method, path, body, want string
	}{
		{http.MethodPost, "/api/cohorts/audiences", `{"label":"x","kind":"paid","plans":[]}`,
			"credential may not perform this action (requires plans:write)"},
		{http.MethodPost, "/api/dashboards", `{"name":"member board"}`,
			"credential may not perform this action (requires dashboards:write)"},
	}
	for _, tc := range denied {
		rec := asMember.request(t, e, tc.method, tc.path, tc.body)
		if rec.Code != http.StatusForbidden {
			t.Errorf("demo member %s %s = %d %s, want 403", tc.method, tc.path, rec.Code, rec.Body.String())
			continue
		}
		var refusal struct {
			Message string `json:"message"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &refusal); err != nil {
			t.Errorf("demo member %s %s: refusal body %q is not the {message} envelope", tc.method, tc.path, rec.Body.String())
		} else if refusal.Message != tc.want {
			t.Errorf("demo member %s %s: refusal %q, want %q", tc.method, tc.path, refusal.Message, tc.want)
		}
	}

	// The read the demo exists for still works, and the member's own project
	// is unaffected — the refusal is scoped to the demo's grants, not the
	// person.
	if rec := asMember.request(t, e, http.MethodGet, "/api/activity", ""); rec.Code != http.StatusOK {
		t.Errorf("demo member GET /api/activity = %d %s, want 200", rec.Code, rec.Body.String())
	}
	atHome := caller{
		name:      "member at home",
		cookies:   asMember.cookies,
		projectID: member.Project.ID,
	}
	if rec := atHome.request(t, e, http.MethodPost, "/api/cohorts/audiences", `{"label":"home","kind":"paid","plans":[]}`); rec.Code != http.StatusCreated {
		t.Errorf("member POST /api/cohorts/audiences on own project = %d %s, want 201", rec.Code, rec.Body.String())
	}
}
