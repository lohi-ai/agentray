package app

import (
	"context"
	"net/http"
	"strconv"
	"strings"

	"github.com/labstack/echo/v4"
	"github.com/lohi-ai/agentray/internal/dataplane/store"
)

// The write guard: one choke point in front of every mutating route.
//
// WHY IT IS A MIDDLEWARE AND NOT A HELPER. The shared demo (store/demo.go) puts
// every signed-up visitor inside a workspace that belongs to someone else, as a
// 'viewer'. Until now that role was decorative almost everywhere — a handful of
// `role IN ('owner','admin')` checks in store/auth.go and nothing else — so a
// visitor could rename the demo's projects, delete its dashboards, rotate its
// keys, and hire agents on the operator's model key. Closing that with a helper
// each handler remembers to call would close it exactly once: the route someone
// adds next year would be open, and nothing would say so. Mounted as
// middleware, the decision is made where the request context is built, before
// any handler runs, and a new route is covered by existing.
//
// FAIL CLOSED. classifyWrite's default arm is writeGuarded — "prove a writing
// role or be refused". A path is exempt only by being named below with a reason.
// Scope resolution fails closed too: a mutating request whose target workspace
// cannot be resolved is refused rather than allowed on the assumption that it
// was harmless.
//
// WHAT IT DOES NOT DO. When the instance has no demo (the default, and every
// `docker compose up`), the guard returns immediately and nothing about the API
// changes. The demo is what makes an untrusted member possible; without one,
// every member of a workspace was invited by its owner.

// mutatingMethods are the verbs that may change state. GET/HEAD/OPTIONS read,
// and reading is exactly what a demo viewer is here to do.
//
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
	// least-privilege ClickHouse connection (store.RunSQL → scopedReadonlySQL),
	// so they are the analytics surface, not a mutation. ---
	"/api/sql/run":                     writeReadOnly,
	"/api/saved-queries/:query_id/run": writeReadOnly,

	// --- asking the agent. Allowed for a viewer BY DESIGN, and metered.
	// The two message edit/regenerate routes are NOT here: a conversation in
	// the demo is project-scoped, so it is shared by every visitor, and
	// rewriting a turn rewrites someone else's thread. Adding a message to it
	// only appends. ---
	"/api/agent/chat":                       writeAgentAsk,
	"/api/agent/conversations/:id/messages": writeAgentAsk,

	// --- controlling a run that is already going. Costs no quota because it
	// starts nothing; refusing it would leave a viewer unable to stop a run
	// they are being billed for. ---
	"/api/agent/chat/steer":    writeAgentControl,
	"/api/agent/chat/cancel":   writeAgentControl,
	"/api/agent/conversations": writeAgentControl,
}

// classifyWrite answers what a matched route is. The default arm is the
// security property: an unrecognised mutating path must prove a writing role.
func classifyWrite(path string) writeClass {
	if class, ok := writeClasses[path]; ok {
		return class
	}
	return writeGuarded
}

// writeGuardStore is what the guard needs from storage. It is an interface so
// the authorization matrix can be tested exhaustively without a database — this
// is authorization code, and a rule that is only exercised against live
// Postgres is a rule that is exercised on almost no CI run.
type writeGuardStore interface {
	DemoWorkspaceID() string
	DemoProjectID() string
	UserBySessionToken(ctx context.Context, token string) (storage.User, storage.UserSession, error)
	ProjectByAPIKey(ctx context.Context, apiKey string) (storage.Project, error)
	ProjectByIDForUser(ctx context.Context, userID string, projectID string) (storage.Project, error)
	DefaultProjectForUser(ctx context.Context, userID string) (storage.Project, error)
	WorkspaceRoleForUser(ctx context.Context, userID string, workspaceID string) (string, error)
	ConsumeDemoAgentRun(ctx context.Context, userID string, limit int) (storage.DemoRunQuota, error)
	RefundDemoAgentRun(ctx context.Context, userID string) error
}

// writeScope is the target a mutating request resolved to.
type writeScope struct {
	// user is the session's user id, and empty when the request carried no
	// usable session. The quota ledger bills it.
	user        string
	workspaceID string
	projectID   string
	role        string
	// byAPIKey marks the SDK/MCP path, which authenticates with the project's
	// own key instead of a membership and therefore has no role at all.
	byAPIKey bool
	// badKey marks an api_key that was supplied and did not resolve, so the
	// refusal can say that instead of asking for a login the caller never
	// intended to use.
	badKey bool
}

// demoWriteGuard refuses a mutating request that cannot prove the caller may
// write to the workspace it targets.
//
// demoRunsPerDay is config.DemoAgentRunsPerUserPerDay: the per-user daily
// ceiling on agent runs started from inside the demo project.
func demoWriteGuard(g writeGuardStore, demoRunsPerDay int) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			if !mutatingMethod(c.Request().Method) {
				return next(c)
			}
			// No demo, no guard. An instance without one has no untrusted
			// members: everyone in a workspace was invited into it by its
			// owner, and that is the contract this instance shipped with.
			demoWorkspace := g.DemoWorkspaceID()
			if demoWorkspace == "" {
				return next(c)
			}
			// An unmatched path has no handler to reach, so there is nothing to
			// guard; Echo answers 404/405 on its own.
			if c.Path() == "" {
				return next(c)
			}
			class := classifyWrite(c.Path())
			if class == writeUnscoped {
				return next(c)
			}

			ctx := c.Request().Context()
			scope, err := resolveWriteScope(c, g)
			if err != nil {
				return err
			}
			if scope.workspaceID == "" {
				// Could not prove where this write lands. That is what the
				// fail-closed default is for: an unauthenticated caller hears
				// 401 (the honest answer, and the same one the handler would
				// have given), and an authenticated one whose scope will not
				// resolve hears a refusal rather than being let through on the
				// assumption it was harmless.
				if scope.badKey {
					return echo.NewHTTPError(http.StatusUnauthorized, "invalid api key")
				}
				if scope.user == "" {
					return echo.NewHTTPError(http.StatusUnauthorized, "login required")
				}
				return echo.NewHTTPError(http.StatusForbidden, "this request does not name a workspace it may write to")
			}

			isDemo := scope.workspaceID == demoWorkspace
			if !isDemo {
				// Outside the demo, reads-with-a-body and agent questions are
				// open to any member, and everything else needs a writing role.
				if class == writeReadOnly || class == writeAgentAsk || class == writeAgentControl {
					return next(c)
				}
				if scope.byAPIKey || storage.RoleMayWrite(scope.role) {
					return next(c)
				}
				return echo.NewHTTPError(http.StatusForbidden, "your role in this workspace is read-only")
			}

			// --- inside the demo ---
			//
			// The API key stops being a credential here. It is a PUBLIC
			// write-only key — it ships in the script tag on the demo site's
			// own pages — so anyone who views the demo can read it. It may
			// still feed events (those paths are writeUnscoped above and never
			// reach this line); it may not be used to drive the rest of the API
			// against someone else's workspace.
			if scope.byAPIKey {
				return demoRefusal(c, "The demo project's API key is a public write key for sending events. It cannot be used to change the demo.")
			}
			switch class {
			case writeReadOnly:
				// The read itself is fine. Anything the handler does BESIDE
				// reading — a result cache written back to the owner's row —
				// is not, so it is told who it is serving.
				if !storage.RoleMayWrite(scope.role) {
					c.Set(demoReadOnlyCallerKey, true)
				}
				return next(c)
			case writeAgentControl:
				return next(c)
			case writeAgentAsk:
				// The operator of the demo site pays for their own runs and is
				// not a visitor; everyone else spends the instance owner's key
				// and is metered.
				if storage.RoleMayWrite(scope.role) {
					return next(c)
				}
				if scope.projectID != g.DemoProjectID() {
					// A sibling project inside the demo workspace is the
					// operator's too, and the budget is defined on the demo
					// project alone — so an unmetered run here would be exactly
					// the unbounded bill the cap exists to prevent.
					return demoRefusal(c, "The agent answers questions in the demo project. Connect your own project to ask it about your data.")
				}
				quota, err := g.ConsumeDemoAgentRun(ctx, scope.user, demoRunsPerDay)
				if err != nil {
					return err
				}
				if !quota.Allowed {
					return demoQuotaRefusal(c, quota)
				}
				// The run this request is about to start must not be able to do
				// what the caller may not: the agent holds create_dashboard and
				// create_chart, and "delete the funnel dashboard" is a sentence.
				c.Set(demoReadOnlyCallerKey, true)
				// A claim that buys nothing is given back. The handler is the
				// only thing that knows whether the provider was ever reached,
				// so it is handed a closure rather than the guard guessing from
				// a status code — this path answers 200 with an error in the
				// body, so there is no status to read.
				user := scope.user
				c.Set(demoRefundKey, func() {
					if err := g.RefundDemoAgentRun(ctx, user); err != nil {
						c.Logger().Warnf("demo quota refund for %s: %v", user, err)
					}
				})
				return next(c)
			default:
				if storage.RoleMayWrite(scope.role) {
					return next(c)
				}
				return demoRefusal(c, demoReadOnlyMessage)
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

	// The SDK/MCP path: the project's own key, no session, no membership.
	if key := firstNonEmpty(c.QueryParam("api_key"), c.QueryParam("token"), c.Request().Header.Get("X-API-Key")); key != "" {
		project, err := g.ProjectByAPIKey(ctx, key)
		if err != nil {
			// An invalid key proves nothing.
			return writeScope{badKey: true}, nil
		}
		return writeScope{workspaceID: project.WorkspaceID, projectID: project.ID, byAPIKey: true}, nil
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
		return scope, nil
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
		return scope, nil
	}

	// No explicit target: the same default the handlers take.
	project, err := g.DefaultProjectForUser(ctx, user.ID)
	if err != nil {
		return scope, nil
	}
	scope.workspaceID, scope.projectID, scope.role = project.WorkspaceID, project.ID, project.Role
	return scope, nil
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

// demoRefundKey carries the closure that gives back a demo agent-run claim.
// Present only on a metered demo question; absent everywhere else, which is why
// refundDemoRun is a no-op rather than an error when nothing set it.
const demoRefundKey = "agentray.demo_refund_run"

// refundDemoRun returns a demo agent-run claim that bought nothing. Call it
// ONLY when the run reported zero token usage — see Store.RefundDemoAgentRun
// for why a run that spent tokens stays spent.
func refundDemoRun(c echo.Context) {
	if refund, ok := c.Get(demoRefundKey).(func()); ok && refund != nil {
		refund()
	}
}

// demoReadOnlyMessage is what a visitor is told when they try to change the
// demo. It is written to be read by a person, not parsed by a client: it says
// what happened, why, and what to do instead.
const demoReadOnlyMessage = "This is a live demo of someone else's site — you can read everything here and ask the agent anything, but changes are switched off. Connect your own project to build dashboards, alerts and agents."

// demoRefusal answers a write the demo will not accept.
//
// 403 is right: the caller is authenticated and the request is well formed;
// they simply may not do this here. Both `message` and `error` carry the same
// sentence because the web client reads whichever it finds first depending on
// the call site, and a refusal that renders as "AgentRay API returned 403" in
// one surface and as a sentence in another is a refusal people report as a bug.
func demoRefusal(c echo.Context, message string) error {
	return c.JSON(http.StatusForbidden, map[string]any{
		"error":   message,
		"message": message,
		"reason":  "demo_read_only",
		"demo":    true,
	})
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
