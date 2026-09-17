package app

import (
	"context"
	"net/http"
	"strconv"
	"strings"

	"github.com/labstack/echo/v4"
	"github.com/lohi-ai/agentray/internal/dataplane/store"
	"github.com/lohi-ai/agentray/internal/shared/opcore"
)

// The write floor: one choke point in front of every mutating route.
//
// It is a second *invocation* of opcore.Allow, not a second rule. The modern
// authProject surface has no per-route class (the F1 fence names that surface
// as admission-only on purpose), so a mutating request that would otherwise
// skip Allow is asked here: legacyWrite(AccessDashboardsWrite). Demo
// non-owners fail because sessionGrants, given the project, withheld the
// write class — Allow itself stays demo-blind.
//
// FAIL CLOSED. classifyWrite's default arm is writeGuarded. A path is exempt
// only by being named below with a reason — including writeClassed, the routes
// that ask the registry their own Allow question and would be preempted by the
// floor's generic one. Scope resolution fails closed too.

// TRACE and CONNECT are absent deliberately: Echo never routes them to a
// handler in this app, and if that changed they would fall into the mutating
// set by default rather than out of it.
func mutatingMethod(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return false
	}
	return true
}

// writeClass says what a mutating route is, for the purpose of the guard.
type writeClass int

const (
	// writeGuarded is the default and the point of the whole file: the caller
	// must hold a writing role in the workspace the request resolves to.
	writeGuarded writeClass = iota
	// writeUnscoped touches no workspace's data — session lifecycle, the
	// caller's own account, creating something new that is theirs, or a public
	// collection endpoint that authenticates by project key. There is no
	// membership to check because there is no existing workspace being changed.
	writeUnscoped
	// writeReadOnly is a read that happens to be a POST because it carries a
	// body. It is allowed for a viewer for the same reason GET is.
	writeReadOnly
	// writeAgentAsk is asking the agent a question. Deliberately allowed for a
	// demo viewer — it is the strongest moment in the product and the reason
	// the demo exists — and metered, because the answer bills the instance
	// owner's model key.
	writeAgentAsk
	// writeAgentControl is the rest of the agent surface a viewer may touch:
	// opening a thread, steering an in-flight run, stopping one. Same
	// permission as asking, but none of it can START a run, so none of it costs
	// quota — and refusing the stop would leave a viewer unable to end a run
	// the instance owner is paying for.
	writeAgentControl
	// writeClassed is a mutating route that asks the registry its own Allow
	// question — authorizedProject / authProjectForWrite / the op adapters
	// declare the access class the work belongs to. The floor's generic
	// dashboards:write question must not run in front of them: it would deny
	// a plans:write credential on a plans:write route, and its refusal would
	// cite a class the route never asked about. The route's own authorizer
	// answers instead, so the floor passes these through untouched.
	writeClassed
)

// writeClasses is the exemption list, keyed by Echo's matched route path (the
// pattern, not the concrete URL — `e.Use` middleware runs after routing, so
// c.Path() is the registered pattern and cannot be spoofed by a path segment).
//
// Every entry is an exemption from "prove a writing role", so every entry
// carries the reason it is safe. Anything not listed is writeGuarded.
var writeClasses = map[string]writeClass{
	// --- session lifecycle: no session yet, or ending the one there is. ---
	"/api/auth/signup": writeUnscoped,
	"/api/auth/login":  writeUnscoped,
	"/api/auth/logout": writeUnscoped,

	// --- the caller's own account and their own new containers. ---
	// The web client appends ?project_id=<active project> to nearly every call,
	// so while someone is LOOKING at the demo these carry the demo's id. They
	// change nothing inside it: a user's display name is their own, a new
	// workspace is created owned by them, and POST /api/projects takes its
	// target workspace from the body where CreateWorkspaceProject already
	// gates it with userCanManageWorkspace.
	"/api/users/me":   writeUnscoped,
	"/api/workspaces": writeUnscoped,
	"/api/projects":   writeUnscoped,

	// --- public collection: authenticated by the project's write key, called
	// from the customer's own site, and for the demo project this is the real
	// site feeding it. Guarding these would stop the demo's data. ---
	"/waitlist":             writeUnscoped,
	"/waitlist/unsubscribe": writeUnscoped,
	"/capture":              writeUnscoped,
	"/batch":                writeUnscoped,
	"/identify":             writeUnscoped,
	"/alias":                writeUnscoped,
	"/e":                    writeUnscoped,
	"/e/":                   writeUnscoped,
	"/i/v0/e":               writeUnscoped,
	"/i/v0/e/":              writeUnscoped,

	// --- webhook ingress: the unguessable per-trigger token in the URL is the
	// credential and there is no session to read a role from. The trigger it
	// fires was created by someone who held a writing role at the time. ---
	"/api/agent/hook/:token": writeUnscoped,

	// --- reads that carry a body. Both run SELECT-only SQL on the
	// least-privilege DuckDB connection (store.RunSQL → scopedReadonlySQL),
	// so they are the analytics surface, not a mutation. ---
	"/api/sql/run":                     writeReadOnly,
	"/api/saved-queries/:query_id/run": writeReadOnly,

	// --- asking the agent. Allowed for a viewer BY DESIGN, and metered.
	// The two message edit/regenerate routes are NOT here: a conversation in
	// the demo is project-scoped, so it is shared by every visitor, and
	// rewriting a turn rewrites someone else's thread. Adding a message to it
	// only appends. ---
	"/api/agent/chat":                       writeAgentAsk,
	// Answering a parked ask question resumes the run — same spend class as
	// asking, so it is metered the same way.
	"/api/agent/chat/answer":                writeAgentAsk,
	"/api/agent/conversations/:id/messages": writeAgentAsk,

	// --- controlling a run that is already going. Costs no quota because it
	// starts nothing; refusing it would leave a viewer unable to stop a run
	// they are being billed for. ---
	"/api/agent/chat/cancel":   writeAgentControl,
	"/api/agent/conversations": writeAgentControl,

	// --- the shared operation surface. Every call is a POST, but the op name
	// lives in the body and the opcore authorizer enforces per-operation
	// access classes — a viewer's write op is denied there, so guarding the
	// transport as a write would reject reads the matrix allows. Classified
	// read-only so the request reaches the real authorizer; inside the demo
	// the read-only caller flag still suppresses any cache write-back. ---

	// --- routes that ask the registry their own Allow question. The floor's
	// generic write question would preempt the class the route declares: a
	// plans:write credential was refused on the plans:write audience routes
	// because the floor asked about dashboards:write. The route's authorizer
	// is the decision; the floor stays out of its way. ---
	"/api/templates/:template_id/apply":                  writeClassed,
	"/api/templates/:template_id/charts/:chart_id/clone": writeClassed,
	"/api/cohorts/audiences":                             writeClassed,
	"/api/cohorts/audiences/:audience_id":                writeClassed,
	"/api/subscription/mapping":                          writeClassed,
	"/api/saved-queries":                                 writeClassed,
	"/api/saved-queries/:query_id":                       writeClassed,
	"/api/validation/tests/:id/commit":                   writeClassed,
	"/api/validation/tests/:id/decide":                   writeClassed,
	"/api/dashboards":                                    writeClassed,
	"/api/dashboards/:dashboard_id":                      writeClassed,
	"/api/dashboards/:dashboard_id/charts":               writeClassed,
	"/api/dashboards/:dashboard_id/charts/order":         writeClassed,
	"/api/charts/:chart_id":                              writeClassed,
	"/api/connectors":                                    writeClassed,
	"/api/connectors/:connector_id":                      writeClassed,
	"/api/connectors/:connector_id/test":                 writeClassed,
	"/api/connector-syncs/:sync_id/run":                  writeClassed,
}

// classifyWrite answers what a matched route is. The default arm is the
// security property: an unrecognised mutating path must prove a writing role.
func classifyWrite(path string) writeClass {
	// The shared operation surface mounts per-operation paths
	// (/api/op/<operation>) and /mcp — every call is a POST, but the op name
	// lives in the body/path and the opcore authorizer enforces per-operation
	// access classes. A viewer's write op is denied there, so guarding the
	// transport as a write would reject reads the matrix allows.
	if path == "/mcp" || path == "/mcp/" || strings.HasPrefix(path, "/api/op/") {
		return writeReadOnly
	}
	if class, ok := writeClasses[path]; ok {
		return class
	}
	return writeGuarded
}

// writeGuardStore is what the guard needs from storage. It is an interface so
// the authorization matrix can be tested exhaustively without a database.
type writeGuardStore interface {
	DemoWorkspaceID() string
	DemoProjectID() string
	UserBySessionToken(ctx context.Context, token string) (storage.User, storage.UserSession, error)
	ProjectByAPIKey(ctx context.Context, apiKey string) (storage.Project, error)
	ProjectByID(ctx context.Context, projectID string) (storage.Project, error)
	CredentialBySecret(ctx context.Context, secret string) (storage.ResolvedCredential, error)
	ProjectByIDForUser(ctx context.Context, userID string, projectID string) (storage.Project, error)
	DefaultProjectForUser(ctx context.Context, userID string) (storage.Project, error)
	WorkspaceRoleForUser(ctx context.Context, userID string, workspaceID string) (string, error)
	ProjectCredentialSplit(ctx context.Context, projectID string) (bool, error)
}

// writeScope is the target a mutating request resolved to.
type writeScope struct {
	user            string
	workspaceID     string
	projectID       string
	role            string
	isDemo          bool
	grants          []opcore.Access
	split           bool
	byAPIKey        bool
	byManagementKey bool
	badKey          bool
	badBearer       bool
}

func (s writeScope) membership() storage.Project {
	return storage.Project{ID: s.projectID, Role: s.role, IsDemo: s.isDemo}
}

func (s writeScope) principal() opcore.Principal {
	switch {
	case s.byManagementKey:
		return opcore.Principal{ProjectID: s.projectID, Kind: opcore.CredManagement, Grants: s.grants}
	case s.byAPIKey:
		kind := opcore.CredLegacy
		if s.split {
			kind = opcore.CredCapture
		}
		return opcore.Principal{ProjectID: s.projectID, Kind: kind}
	default:
		project := s.membership()
		return opcore.Principal{
			ProjectID: s.projectID,
			Kind:      opcore.CredSession,
			Role:      s.role,
			UserID:    s.user,
			Grants:    sessionGrants(project),
		}
	}
}

// writeFloorReg is used when the server did not pass a registry. CredSession
// Allow does not consult registered operations; CredLegacy then denies.
var writeFloorReg = &opcore.Registry{}

func scopeAllowsWrite(reg *opcore.Registry, scope writeScope) bool {
	if reg == nil {
		reg = writeFloorReg
	}
	return reg.Allow(scope.principal(), legacyWrite(opcore.AccessDashboardsWrite))
}

// demoWriteGuard is the mutating floor. It asks Allow; it does not branch on
// whether a demo is configured.
func demoWriteGuard(g writeGuardStore, reg *opcore.Registry) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			if !mutatingMethod(c.Request().Method) {
				return next(c)
			}
			if c.Path() == "" {
				return next(c)
			}
			class := classifyWrite(c.Path())
			if class == writeUnscoped || class == writeClassed {
				return next(c)
			}

			scope, err := resolveWriteScope(c, g)
			if err != nil {
				return err
			}
			if scope.workspaceID == "" {
				if scope.badBearer {
					return echo.NewHTTPError(http.StatusUnauthorized, "invalid credential")
				}
				if scope.badKey {
					return echo.NewHTTPError(http.StatusUnauthorized, "invalid api key")
				}
				if scope.user == "" {
					return echo.NewHTTPError(http.StatusUnauthorized, "login required")
				}
				return echo.NewHTTPError(http.StatusForbidden, "this request does not name a workspace it may write to")
			}

			mayWrite := scopeAllowsWrite(reg, scope)
			switch class {
			case writeReadOnly, writeAgentAsk, writeAgentControl:
				if !mayWrite {
					c.Set(demoReadOnlyCallerKey, true)
				}
				return next(c)
			default:
				if mayWrite {
					return next(c)
				}
				return echo.NewHTTPError(http.StatusForbidden, opcore.RefusalMessage(opcore.AccessDashboardsWrite))
			}
		}
	}
}

// resolveWriteScope answers "which workspace does this request change, and what
// is the caller's role in it".
//
// The order mirrors how the handlers themselves resolve a target, most specific
// first: a path parameter is the thing the handler acts on, a query parameter
// is context. That ordering matters because the web client appends
// ?project_id=<active project> to nearly every call, so a request to
// /api/workspaces/<mine>/members carries the demo's project id whenever someone
// happens to be looking at the demo. Resolving the query first would refuse a
// write to the caller's OWN workspace — the exact "locks everyone out of their
// own account" failure this guard must not have.
func resolveWriteScope(c echo.Context, g writeGuardStore) (writeScope, error) {
	ctx := c.Request().Context()

	// Management credentials arrive as Bearer agm_… and take precedence over
	// every other transport — the same order principalFromRequest mandates,
	// so a stale ?api_key or X-API-Key riding alongside cannot mis-scope or
	// reject the request before the principal resolver sees the Bearer. They
	// are private scoped credentials, not the public capture key, so the
	// demo's public-key refusal does not apply.
	//
	// A Bearer that is present but is NOT an agm_ credential is denied, not
	// absent: principalFromRequest answers it 401 rather than falling through
	// to the cookie, and if this resolver fell through instead it would approve
	// a write on a session the handler is about to refuse. Two resolvers over
	// one request must reach one answer, so the denial is reported here and
	// resolves to the same 401.
	if tok, present := bearerToken(c); present {
		if tok == "" {
			return writeScope{badBearer: true}, nil
		}
		cred, err := g.CredentialBySecret(ctx, tok)
		if err != nil {
			return writeScope{badBearer: true}, nil
		}
		project, err := g.ProjectByID(ctx, cred.ProjectID)
		if err != nil {
			return writeScope{badBearer: true}, nil
		}
		return withDemo(g, writeScope{
			workspaceID:     project.WorkspaceID,
			projectID:       project.ID,
			byManagementKey: true,
			grants:          managementGrants(cred.Scopes),
		}), nil
	}

	// The SDK/MCP path: the project's own key, no session, no membership.
	if key := firstNonEmpty(c.QueryParam("api_key"), c.QueryParam("token"), c.Request().Header.Get("X-API-Key")); key != "" {
		project, err := g.ProjectByAPIKey(ctx, key)
		if err != nil {
			return writeScope{badKey: true}, nil
		}
		split, err := g.ProjectCredentialSplit(ctx, project.ID)
		if err != nil {
			return writeScope{badKey: true}, nil
		}
		return withDemo(g, writeScope{
			workspaceID: project.WorkspaceID,
			projectID:   project.ID,
			byAPIKey:    true,
			split:       split,
		}), nil
	}

	cookie, err := c.Cookie(sessionCookieName)
	if err != nil || cookie.Value == "" {
		return writeScope{}, nil
	}
	user, _, err := g.UserBySessionToken(ctx, cookie.Value)
	if err != nil {
		return writeScope{}, nil
	}
	scope := writeScope{user: user.ID}

	if workspaceID := strings.TrimSpace(c.Param("workspace_id")); workspaceID != "" {
		role, err := g.WorkspaceRoleForUser(ctx, user.ID, workspaceID)
		if err != nil {
			return scope, err
		}
		if role == "" {
			// Not a member. The handler will refuse too, but the guard must not
			// fall through to a workspace the caller DOES belong to and approve
			// the write against that one instead.
			return scope, nil
		}
		scope.workspaceID, scope.role = workspaceID, role
		return withDemo(g, scope), nil
	}

	projectID := firstNonEmpty(strings.TrimSpace(c.Param("project_id")), strings.TrimSpace(c.QueryParam("project_id")))
	if projectID != "" {
		project, err := g.ProjectByIDForUser(ctx, user.ID, projectID)
		if err != nil {
			// Either the project does not exist or the caller is not a member
			// of its workspace; both mean no proven scope.
			return scope, nil
		}
		scope.workspaceID, scope.projectID, scope.role = project.WorkspaceID, project.ID, project.Role
		return withDemo(g, scope), nil
	}

	// No explicit target: the same default the handlers take.
	project, err := g.DefaultProjectForUser(ctx, user.ID)
	if err != nil {
		return scope, nil
	}
	scope.workspaceID, scope.projectID, scope.role = project.WorkspaceID, project.ID, project.Role
	return withDemo(g, scope), nil
}

func withDemo(g writeGuardStore, scope writeScope) writeScope {
	demo := g.DemoWorkspaceID()
	scope.isDemo = demo != "" && scope.workspaceID == demo
	return scope
}

// demoReadOnlyCallerKey marks a request the guard let through even though the
// caller may not write to the scope it targets — a demo viewer asking a
// question, or running a query whose handler would otherwise write through a
// cache. The guard is the only writer of this key, so the decision is still
// made in exactly one place; a handler that has a side effect beyond its main
// job just has to ask.
const demoReadOnlyCallerKey = "agentray.demo_read_only_run"

// readOnlyRun reports whether this request's agent run must be read-only.
// Absent key means false, which is correct: the guard sets it only for the demo,
// and every other run is the caller's own project.
func readOnlyCaller(c echo.Context) bool {
	value, _ := c.Get(demoReadOnlyCallerKey).(bool)
	return value
}

// demoAskLimit is config.DemoAgentRunsPerUserPerDay, set at server start.
var demoAskLimit int

const demoRefundKey = "agentray.demo_refund_run"

func refundDemoRun(c echo.Context) {
	if refund, ok := c.Get(demoRefundKey).(func()); ok && refund != nil {
		refund()
	}
}

type demoMeter interface {
	DemoProjectID() string
	ConsumeDemoAgentRun(ctx context.Context, userID string, limit int) (storage.DemoRunQuota, error)
	RefundDemoAgentRun(ctx context.Context, userID string) error
}

// meterDemoAsk claims one demo visitor question. Owners/admins of the demo
// are not metered. A budget, not a permission.
func meterDemoAsk(c echo.Context, g demoMeter, project storage.Project, userID string) error {
	if !project.IsDemo || sessionAllowsWrite(project) {
		return nil
	}
	if project.ID != g.DemoProjectID() {
		return echo.NewHTTPError(http.StatusForbidden, "The agent answers questions in the demo project. Connect your own project to ask it about your data.")
	}
	quota, err := g.ConsumeDemoAgentRun(c.Request().Context(), userID, demoAskLimit)
	if err != nil {
		return err
	}
	if !quota.Allowed {
		return demoQuotaRefusal(c, quota)
	}
	user := userID
	ctx := c.Request().Context()
	c.Set(demoRefundKey, func() {
		if err := g.RefundDemoAgentRun(ctx, user); err != nil {
			c.Logger().Warnf("demo quota refund for %s: %v", user, err)
		}
	})
	return nil
}

// demoQuotaRefusal answers the question after the last one the budget allowed.
//
// 429 rather than 403: nothing is wrong with the caller or the request, they
// have simply used today's allowance — a distinction a client can act on
// (retry tomorrow) and a person can understand. The wording is a conversion
// prompt on purpose; this is the moment someone has just discovered the agent
// is worth using.
func demoQuotaRefusal(c echo.Context, quota storage.DemoRunQuota) error {
	message := demoQuotaMessage(quota)
	return c.JSON(http.StatusTooManyRequests, map[string]any{
		"error":     message,
		"message":   message,
		"reason":    "demo_agent_quota",
		"demo":      true,
		"limit":     quota.Limit,
		"used":      quota.Used,
		"resets_at": quota.ResetsAt,
	})
}

// demoQuotaMessage is separated from the HTTP answer so the wording is testable
// and so the two ways of running out — a budget spent, and a demo whose agent
// budget is zero — do not end up telling people the same wrong thing.
func demoQuotaMessage(quota storage.DemoRunQuota) string {
	if quota.Limit <= 0 {
		return "The agent is switched off in this shared demo. Connect your own project to ask it questions about your data."
	}
	return "You've used all " + plural(quota.Limit, "question") + " the shared demo gives each person per day — the answers run on the demo owner's account, which is why there's a limit. Your questions reset tomorrow. Connect your own project and the agent answers about YOUR data, with no cap."
}

// plural renders "1 question" / "5 questions" so the refusal reads like a
// sentence at every limit an operator might set.
func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return strconv.Itoa(n) + " " + noun + "s"
}
