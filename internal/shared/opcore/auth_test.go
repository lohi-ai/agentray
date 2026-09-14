package opcore

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"
)

// The authorizer is the credential boundary for every network adapter. These
// tests pin the matrix: capture denied, legacy frozen, scopes exact, session
// roles gated, unknown denied — before any handler runs.

func authRegistry() *Registry {
	r := NewRegistry()
	Register(r, echoOp()) // Access: analytics:read
	Register(r, Operation[echoIn, echoOut]{
		Name:    "probe_source",
		Summary: "probe a source credential",
		Access:  AccessSourcesRead,
		// A sources:read management key may probe; a member session may not.
		MinSessionRole: "admin",
		Handler: func(_ context.Context, _ CallContext, in echoIn) (echoOut, error) {
			return echoOut{Echoed: in.Text}, nil
		},
	})
	Register(r, Operation[echoIn, echoOut]{
		Name:    "source_status",
		Summary: "read source status",
		Access:  AccessSourcesRead,
		Handler: func(_ context.Context, _ CallContext, in echoIn) (echoOut, error) {
			return echoOut{Echoed: in.Text}, nil
		},
	})
	Register(r, Operation[echoIn, echoOut]{
		Name:    "write_dash",
		Summary: "write a dashboard",
		Access:  AccessDashboardsWrite,
		Handler: func(_ context.Context, _ CallContext, in echoIn) (echoOut, error) {
			return echoOut{Echoed: in.Text}, nil
		},
	})
	Register(r, Operation[echoIn, echoOut]{
		Name:    "unclassed",
		Summary: "no access class — must be unreachable remotely",
		Handler: func(_ context.Context, _ CallContext, in echoIn) (echoOut, error) {
			return echoOut{Echoed: in.Text}, nil
		},
	})
	r.SetLegacyAllowlist([]string{"echo"})
	return r
}

func TestAuthorizeMatrix(t *testing.T) {
	r := authRegistry()
	cases := []struct {
		name string
		p    Principal
		op   string
		want bool
	}{
		// capture: denied everything, always
		{"capture denied read", Principal{Kind: CredCapture}, "echo", false},
		{"capture denied write", Principal{Kind: CredCapture}, "write_dash", false},
		// legacy: frozen allowlist only
		{"legacy allowed frozen op", Principal{Kind: CredLegacy}, "echo", true},
		{"legacy denied new op", Principal{Kind: CredLegacy}, "write_dash", false},
		{"legacy denied probe", Principal{Kind: CredLegacy}, "probe_source", false},
		// management: exact scope match; sources:manage implies sources:read
		{"mgmt analytics:read reads", Principal{Kind: CredManagement, Grants: []Access{AccessAnalyticsRead}}, "echo", true},
		{"mgmt analytics:read cannot write", Principal{Kind: CredManagement, Grants: []Access{AccessAnalyticsRead}}, "write_dash", false},
		{"mgmt sources:read probes", Principal{Kind: CredManagement, Grants: []Access{AccessSourcesRead}}, "probe_source", true},
		{"mgmt sources:read status", Principal{Kind: CredManagement, Grants: []Access{AccessSourcesRead}}, "source_status", true},
		{"mgmt sources:manage implies read", Principal{Kind: CredManagement, Grants: []Access{AccessSourcesManage, AccessSourcesRead}}, "probe_source", true},
		{"mgmt dashboards:write writes", Principal{Kind: CredManagement, Grants: []Access{AccessDashboardsWrite}}, "write_dash", true},
		// sessions: role gates on top of grants
		{"session member reads", Principal{Kind: CredSession, Role: "member", Grants: []Access{AccessAnalyticsRead, AccessSourcesRead}}, "echo", true},
		{"session member cannot probe", Principal{Kind: CredSession, Role: "member", Grants: []Access{AccessAnalyticsRead, AccessSourcesRead}}, "probe_source", false},
		{"session member reads status", Principal{Kind: CredSession, Role: "member", Grants: []Access{AccessAnalyticsRead, AccessSourcesRead}}, "source_status", true},
		{"session viewer reads status", Principal{Kind: CredSession, Role: "viewer", Grants: []Access{AccessAnalyticsRead, AccessSourcesRead}}, "source_status", true},
		{"session viewer cannot probe", Principal{Kind: CredSession, Role: "viewer", Grants: []Access{AccessAnalyticsRead, AccessSourcesRead}}, "probe_source", false},
		{"session admin probes", Principal{Kind: CredSession, Role: "admin", Grants: []Access{AccessSourcesRead, AccessSourcesManage}}, "probe_source", true},
		{"session unknown role denied", Principal{Kind: CredSession, Role: "superuser", Grants: []Access{AccessAnalyticsRead}}, "probe_source", false},
		// unclassed op: denied to everyone
		{"unclassed denied session", Principal{Kind: CredSession, Role: "owner", Grants: []Access{AccessAnalyticsRead, AccessDashboardsWrite, AccessSourcesRead, AccessSourcesManage, AccessGrowthWrite}}, "unclassed", false},
		{"unclassed denied legacy", Principal{Kind: CredLegacy}, "unclassed", false},
		// unknown op: denied
		{"unknown op denied", Principal{Kind: CredSession, Role: "owner", Grants: []Access{AccessAnalyticsRead}}, "nope", false},
	}
	for _, tc := range cases {
		if got := r.Authorize(tc.p, tc.op); got != tc.want {
			t.Errorf("%s: Authorize = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestAllowedSpecsFilters(t *testing.T) {
	r := authRegistry()
	capture := r.AllowedSpecs(Principal{Kind: CredCapture})
	if len(capture) != 0 {
		t.Fatalf("capture sees %d tools, want 0", len(capture))
	}
	legacy := r.AllowedSpecs(Principal{Kind: CredLegacy})
	if len(legacy) != 1 || legacy[0].OpName() != "echo" {
		t.Fatalf("legacy sees %v, want [echo]", toolNames(legacy))
	}
	viewer := r.AllowedSpecs(Principal{Kind: CredSession, Role: "viewer", Grants: []Access{AccessAnalyticsRead, AccessSourcesRead}})
	got := map[string]bool{}
	for _, s := range viewer {
		got[s.OpName()] = true
	}
	if !got["echo"] || !got["source_status"] || got["probe_source"] || got["write_dash"] || got["unclassed"] {
		t.Fatalf("viewer tools = %v", toolNames(viewer))
	}
}

func toolNames(specs []Spec) []string {
	out := make([]string, 0, len(specs))
	for _, s := range specs {
		out = append(out, s.OpName())
	}
	return out
}

// Allow is Authorize stated over an access class instead of an operation name,
// and the legacy REST routes that have no operation to name depend on the two
// agreeing. The matrix is Authorize's, plus the derivation the class form needs:
// a legacy project key holds the classes its frozen allowlist covers and no
// others.
func TestAllowMatrix(t *testing.T) {
	r := authRegistry()
	session := func(role string, grants ...Access) Principal {
		return Principal{Kind: CredSession, Role: role, Grants: grants}
	}
	management := func(grants ...Access) Principal {
		return Principal{Kind: CredManagement, Grants: grants}
	}
	cases := []struct {
		name string
		p    Principal
		req  Requirement
		want bool
	}{
		// capture: denied every class, the same way it is denied every op.
		{"capture denied a read", Principal{Kind: CredCapture}, Requirement{Access: AccessAnalyticsRead}, false},
		{"capture denied a write", Principal{Kind: CredCapture}, Requirement{Access: AccessDashboardsWrite, MinSessionRole: "member"}, false},
		// deny-by-default, as in Authorize.
		{"no class is unreachable", session("owner", AccessAnalyticsRead), Requirement{}, false},
		{"unknown kind denied", Principal{Kind: CredentialKind("robot")}, Requirement{Access: AccessAnalyticsRead}, false},

		// legacy: what the frozen allowlist covers, and nothing else — the
		// class is derived from the allowlist rather than listed a second time.
		{"legacy holds a frozen class", Principal{Kind: CredLegacy}, Requirement{Access: AccessAnalyticsRead}, true},
		{"legacy refused an unfrozen class", Principal{Kind: CredLegacy}, Requirement{Access: AccessDashboardsWrite, MinSessionRole: "member"}, false},
		{"legacy refused sources:manage", Principal{Kind: CredLegacy}, Requirement{Access: AccessSourcesManage, MinSessionRole: "admin"}, false},

		// management: the class must be granted — analytics:read alone never
		// reaches a write, which is the hole this closes.
		{"reader allowed a read", management(AccessAnalyticsRead), Requirement{Access: AccessAnalyticsRead}, true},
		{"reader refused a plan write", management(AccessAnalyticsRead), Requirement{Access: AccessPlansWrite, MinSessionRole: "member"}, false},
		{"writer allowed its class", management(AccessDashboardsWrite), Requirement{Access: AccessDashboardsWrite, MinSessionRole: "member"}, true},
		{"writer refused another class", management(AccessDashboardsWrite), Requirement{Access: AccessPlansWrite, MinSessionRole: "member"}, false},

		// sessions: the class and the role floor together. A viewer holds no
		// write class, and an unknown role writes nothing.
		{"viewer may read", session("viewer", AccessAnalyticsRead, AccessSourcesRead), Requirement{Access: AccessAnalyticsRead}, true},
		{"viewer may not write", session("viewer", AccessAnalyticsRead, AccessSourcesRead), Requirement{Access: AccessDashboardsWrite, MinSessionRole: "member"}, false},
		{"member may write", session("member", AccessAnalyticsRead, AccessDashboardsWrite, AccessPlansWrite), Requirement{Access: AccessDashboardsWrite, MinSessionRole: "member"}, true},
		{"member cannot take an admin floor", session("member", AccessAnalyticsRead, AccessSourcesRead, AccessSourcesManage), Requirement{Access: AccessSourcesManage, MinSessionRole: "admin"}, false},
		{"unknown role may read", session("superuser", AccessAnalyticsRead), Requirement{Access: AccessAnalyticsRead}, true},
		{"unknown role may not write", session("superuser", AccessAnalyticsRead, AccessDashboardsWrite), Requirement{Access: AccessDashboardsWrite, MinSessionRole: "member"}, false},
	}
	for _, tc := range cases {
		if got := r.Allow(tc.p, tc.req); got != tc.want {
			t.Errorf("%s: Allow = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// The class form must not be a second decision beside the operation form: for
// every registered operation and every credential kind that has no name-based
// rule of its own, Allow(access, floor) answers what Authorize(name) answers.
// Legacy is excluded because its rule IS the name — the freeze is a list.
func TestAllowAgreesWithAuthorize(t *testing.T) {
	r := authRegistry()
	principals := []Principal{
		{Kind: CredCapture},
		{Kind: CredManagement, Grants: []Access{AccessAnalyticsRead}},
		{Kind: CredManagement, Grants: []Access{AccessDashboardsWrite}},
		{Kind: CredManagement, Grants: []Access{AccessSourcesRead}},
		{Kind: CredManagement, Grants: []Access{AccessPlansWrite}},
		{Kind: CredSession, Role: "viewer", Grants: []Access{AccessAnalyticsRead, AccessSourcesRead}},
		{Kind: CredSession, Role: "member", Grants: []Access{AccessAnalyticsRead, AccessDashboardsWrite, AccessGrowthWrite, AccessPlansWrite, AccessSourcesRead}},
		{Kind: CredSession, Role: "admin", Grants: []Access{AccessAnalyticsRead, AccessDashboardsWrite, AccessGrowthWrite, AccessPlansWrite, AccessSourcesRead, AccessSourcesManage}},
		{Kind: CredentialKind("robot")},
	}
	for _, spec := range r.Specs() {
		req := Requirement{Access: spec.OpAccess(), MinSessionRole: spec.OpMinSessionRole()}
		for _, p := range principals {
			if got, want := r.Allow(p, req), r.Authorize(p, spec.OpName()); got != want {
				t.Errorf("%s for %+v: Allow = %v, Authorize = %v", spec.OpName(), p, got, want)
			}
		}
	}
}

// tools/call must re-authorize: a principal cannot invoke an operation
// tools/list never advertised.
func TestMCPCallReauthorizes(t *testing.T) {
	e := mcpServer(t)
	// capture key: tools/call denied even though the op exists
	resp := post(t, e, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo","arguments":{"text":"hi"}}}`, map[string]string{"X-Kind": "capture"})
	result := resp["result"].(map[string]any)
	if result["isError"] != true {
		t.Fatalf("capture tools/call not denied: %v", resp)
	}
	content := result["content"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(content, "may not invoke") {
		t.Fatalf("denial text = %q", content)
	}
}

// tools/list filters by principal: capture sees nothing, legacy sees only the
// frozen allowlist.
func TestMCPListFilteredByPrincipal(t *testing.T) {
	e := mcpServer(t)
	resp := post(t, e, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, map[string]string{"X-Kind": "capture"})
	tools := resp["result"].(map[string]any)["tools"].([]any)
	if len(tools) != 0 {
		t.Fatalf("capture tools/list = %v", tools)
	}
	resp = post(t, e, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`, map[string]string{"X-Kind": "legacy"})
	tools = resp["result"].(map[string]any)["tools"].([]any)
	if len(tools) != 1 || tools[0].(map[string]any)["name"] != "echo" {
		t.Fatalf("legacy tools/list = %v", tools)
	}
}

// REST adapter denies before the handler runs.
func TestHTTPAuthorizeDenies(t *testing.T) {
	reg := authRegistry()
	e := echo.New()
	MountHTTP(e.Group("/api/op"), reg, struct{}{}, func(c echo.Context) (Principal, error) {
		switch c.Request().Header.Get("X-Kind") {
		case "capture":
			return Principal{ProjectID: "p1", Kind: CredCapture}, nil
		case "legacy":
			return Principal{ProjectID: "p1", Kind: CredLegacy}, nil
		default:
			return Principal{ProjectID: "p1", Kind: CredSession, Role: "member",
				Grants: []Access{AccessAnalyticsRead, AccessSourcesRead}}, nil
		}
	})
	call := func(op, kind string) int {
		req := httptest.NewRequest(http.MethodPost, "/api/op/"+op, strings.NewReader(`{"text":"x"}`))
		req.Header.Set("Content-Type", "application/json")
		if kind != "" {
			req.Header.Set("X-Kind", kind)
		}
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)
		return rec.Code
	}
	if code := call("echo", "capture"); code != http.StatusForbidden {
		t.Fatalf("capture REST = %d, want 403", code)
	}
	if code := call("write_dash", "legacy"); code != http.StatusForbidden {
		t.Fatalf("legacy write REST = %d, want 403", code)
	}
	if code := call("probe_source", ""); code != http.StatusForbidden {
		t.Fatalf("member probe REST = %d, want 403", code)
	}
	if code := call("echo", ""); code != http.StatusOK {
		t.Fatalf("member read REST = %d, want 200", code)
	}
	if code := call("unclassed", ""); code != http.StatusForbidden {
		t.Fatalf("unclassed REST = %d, want 403", code)
	}
}
