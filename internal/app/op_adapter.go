package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/labstack/echo/v4"
	"github.com/lohi-ai/agentray/internal/dataplane/store"
	"github.com/lohi-ai/agentray/internal/dataplane/usecase"
	"github.com/lohi-ai/agentray/internal/runtime"
	"github.com/lohi-ai/agentray/internal/shared/opcore"
)

// opAdapter lets the legacy REST surface run the shared operation registry
// in-process: same opcore.Operation -> usecase -> store path /api/op, MCP, and
// the agent tools execute, wrapped in the legacy URL + envelope contract.
// The registry is built once per server; deps is the same bundle MountHTTP
// hands to /api/op handlers.
type opAdapter struct {
	reg  *opcore.Registry
	deps *usecase.Deps
}

func newOpAdapter(store *storage.Store, notifier usecase.Notifier, runner usecase.SourceRunner) *opAdapter {
	return &opAdapter{
		reg: usecase.Registry(),
		deps: &usecase.Deps{
			Repo:     store,
			Memory:   agentruntime.NewPgMemory(store, false),
			Notifier: notifier,
			Runner:   runner,
			Audit:    store,
		},
	}
}

// invoke runs one registered operation under the resolved principal, and is
// where the legacy REST surface makes the access-class decision.
//
// It is the choke point on purpose: every opAdapter route that executes an
// operation reaches the registry through this function, so a route added
// tomorrow inherits the check instead of having to remember it — which is
// exactly how the nine dashboard/chart routes came to run as any credential
// that could reach the project. The hand-rolled adapters — MountHTTP on
// /api/op, MountMCP on /mcp, and overview_routes.go — call spec.OpInvoke
// themselves and run their own Registry.Authorize first. The check is the same
// Registry.Authorize MountHTTP and MCP run, and the refusal is the same one
// they return, so a credential refused on /api/op is refused here.
//
// Admission — whether this credential may address the project at all — stays
// with the caller's resolver (projectFromRequest / principalAndProject).
func (a *opAdapter) invoke(c echo.Context, principal opcore.Principal, opName string, input any) (json.RawMessage, error) {
	spec, ok := a.reg.Get(opName)
	if !ok {
		return nil, echo.NewHTTPError(http.StatusInternalServerError, "operation "+opName+" not registered")
	}
	if err := a.authorize(principal, opName); err != nil {
		return nil, err
	}
	body, err := json.Marshal(input)
	if err != nil {
		return nil, err
	}
	cc := opcore.CallContext{ProjectID: principal.ProjectID, Deps: a.deps, Principal: principal}
	out, err := spec.OpInvoke(c.Request().Context(), cc, string(body))
	if err != nil {
		return nil, usecase.MapOpError(err)
	}
	return json.RawMessage(out), nil
}

// authorize is the legacy REST surface's access-class decision — the same
// Registry.Authorize call MountHTTP makes on /api/op — and it answers with the
// same refusal those adapters return, so the two cannot drift apart.
func (a *opAdapter) authorize(principal opcore.Principal, opName string) error {
	if !a.reg.Authorize(principal, opName) {
		return echo.NewHTTPError(http.StatusForbidden, "credential may not invoke "+opName)
	}
	return nil
}

// authorizeAccess is authorize for the legacy routes that have no registered
// operation to name — cohort audiences, saved queries, template
// instantiation, the subscription mapping. The work is real and it writes, but
// there is no operation to ask about, so the route states the class its work
// belongs to instead of naming an operation. The decision is the registry's
// (Allow), not a second implementation of it here: a management credential
// without the class is refused exactly as /api/op refuses it, and a viewer's
// session is refused on every write for the same reason /api/op refuses one.
func (a *opAdapter) authorizeAccess(principal opcore.Principal, req opcore.Requirement) error {
	if a.reg.Allow(principal, req) {
		return nil
	}
	// A session refused on a write hears why: the class is not the missing
	// piece, the membership is. It is the sentence the demo write guard
	// answers a viewer with, for the same situation. A read requirement carries
	// no floor (legacyRead leaves MinSessionRole empty), so a session refused a
	// read is told which class it lacks instead of being told its role is
	// read-only — which would be the wrong sentence for a read.
	if principal.Kind == opcore.CredSession && req.MinSessionRole != "" && !storage.RoleMayWrite(principal.Role) {
		return echo.NewHTTPError(http.StatusForbidden, "your role in this workspace is read-only")
	}
	return echo.NewHTTPError(http.StatusForbidden, "credential may not perform this action (requires "+string(req.Access)+")")
}

// legacyWrite is what a legacy mutating route requires: the access class its
// work belongs to, at the session floor every write class in the registry
// carries. A viewer's session holds no write class, so the floor is not the
// only thing standing in its way — but stating both keeps this requirement and
// an operation's Spec the same shape.
func legacyWrite(access opcore.Access) opcore.Requirement {
	return opcore.Requirement{Access: access, MinSessionRole: "member"}
}

// legacyRead is what a legacy route that only reads requires. Both of today's
// callers are POSTs (they carry SQL in a body), and both are analytics:read —
// the class /api/op's run_sql carries, so a viewer may run SQL here for the
// same reason it may run it there.
func legacyRead(access opcore.Access) opcore.Requirement {
	return opcore.Requirement{Access: access}
}

// authorizedProject resolves the caller of a legacy route — the same admission
// and the same key redaction projectFromRequest applies to a read — and then
// makes the route's access-class decision through the registry, the same Allow
// question /api/op asks. That pair is the whole of the resolver split:
// projectFromRequest answers "may this credential address the project", which is
// the question for the addressing itself and never the question for reading or
// writing the project's data, so a route that resolved through it ran for any
// credential that could reach the project — a reader-scoped management key, a
// viewer's session, a pre-split project key.
//
// The name says nothing about reads or writes on purpose: the requirement
// decides the class, so the class cannot disagree with the verb the way
// projectForRead/projectForWrite could. TestNoRouteResolvesThroughTheReadResolver
// is the fence — the only route allowed to skip this is the one whose work IS
// the addressing, GET /api/projects.
func authorizedProject(c echo.Context, store *storage.Store, ops *opAdapter, req opcore.Requirement) (storage.Project, error) {
	_, project, err := authorizedPrincipalAndProject(c, store, ops, req)
	return project, err
}

// authorizedPrincipalAndProject is authorizedProject plus the caller it
// resolved, for the routes that have to decide something else about the caller
// as well: running a saved query is an analytics read, but caching its result is
// an UPDATE to the owner's row, so that handler needs the principal and not only
// the project.
func authorizedPrincipalAndProject(c echo.Context, store *storage.Store, ops *opAdapter, req opcore.Requirement) (opcore.Principal, storage.Project, error) {
	principal, project, err := principalAndProject(c, store)
	if err != nil {
		return opcore.Principal{}, storage.Project{}, err
	}
	if err := ops.authorizeAccess(principal, req); err != nil {
		return opcore.Principal{}, storage.Project{}, err
	}
	return principal, project, nil
}

// optionalMutationBody decodes the extra fields a legacy mutation may carry —
// the caller's expected revision and its idempotency key — without disturbing
// the operation-specific payload the handler binds separately. An empty body
// (legacy DELETE callers send none) decodes to the zero value.
type optionalMutationBody struct {
	Revision       int64  `json:"revision"`
	IdempotencyKey string `json:"idempotency_key"`
}

func readOptionalMutationBody(c echo.Context) (optionalMutationBody, error) {
	var out optionalMutationBody
	raw, err := io.ReadAll(c.Request().Body)
	if err != nil {
		return out, echo.NewHTTPError(http.StatusBadRequest, "unreadable request body")
	}
	if len(raw) == 0 {
		return out, nil
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return out, echo.NewHTTPError(http.StatusBadRequest, "invalid json")
	}
	return out, nil
}

// principalAndProject resolves the caller and loads the project it names,
// refusing capture credentials — the same admission projectFromRequest has
// always applied — while keeping the principal for the operation call. The
// project comes back through projectForPrincipal, so a non-session caller never
// receives the capture key.
func principalAndProject(c echo.Context, store *storage.Store) (opcore.Principal, storage.Project, error) {
	principal, err := principalFromRequest(c, store)
	if err != nil {
		return opcore.Principal{}, storage.Project{}, err
	}
	if principal.Kind == opcore.CredCapture {
		return opcore.Principal{}, storage.Project{}, echo.NewHTTPError(http.StatusForbidden, "capture credential cannot access management routes")
	}
	project, err := store.ProjectByID(c.Request().Context(), principal.ProjectID)
	if err != nil {
		return opcore.Principal{}, storage.Project{}, err
	}
	return principal, projectForPrincipal(project, principal), nil
}

// sessionPrincipal builds the session principal for the session-only
// connector surface: the workspace role becomes the grant set, exactly as
// principalFromRequest's session branch resolves it. The project comes from
// ProjectByIDForUser so the role is populated and membership is proven.
func sessionPrincipal(c echo.Context, store *storage.Store, userID, projectID string) (opcore.Principal, storage.Project, error) {
	project, err := store.ProjectByIDForUser(c.Request().Context(), userID, projectID)
	if err != nil {
		return opcore.Principal{}, storage.Project{}, echo.NewHTTPError(http.StatusForbidden, "project not available")
	}
	return opcore.Principal{
		ProjectID: project.ID,
		Kind:      opcore.CredSession,
		Role:      project.Role,
		UserID:    userID,
		Grants:    sessionGrants(project.Role),
	}, project, nil
}

// sessionProjectID picks the project the session-scoped connector surface
// acts on: an explicit ?project_id= wins, then the caller's default project —
// the same selection projectFromRequest applies, minus credential principals
// (these routes never admitted them).
func sessionProjectID(c echo.Context, store *storage.Store, userID string) (string, error) {
	if projectID := firstNonEmpty(c.QueryParam("project_id"), c.Param("project_id")); projectID != "" {
		return projectID, nil
	}
	project, err := store.DefaultProjectForUser(c.Request().Context(), userID)
	if err != nil {
		return "", echo.NewHTTPError(http.StatusNotFound, "project not found")
	}
	return project.ID, nil
}

// sessionCaller resolves a session-only route's caller in one step: the cookie,
// the project it names (membership proven), and the opcore principal the
// registry decides on. It is the admission half of the connector family's
// resolver, shared so a second session-only family reaches its decision through
// the same principal rather than a second reading of the same request.
//
// It is deliberately not principalAndProject: a session-only route never
// admitted a project key or a management credential, and widening it here would
// hand a machine key access the surface was built without.
func sessionCaller(c echo.Context, store *storage.Store) (authContext, opcore.Principal, storage.Project, error) {
	auth, err := authFromRequest(c, store)
	if err != nil {
		return authContext{}, opcore.Principal{}, storage.Project{}, err
	}
	projectID, err := sessionProjectID(c, store, auth.User.ID)
	if err != nil {
		return authContext{}, opcore.Principal{}, storage.Project{}, err
	}
	principal, project, err := sessionPrincipal(c, store, auth.User.ID, projectID)
	if err != nil {
		return authContext{}, opcore.Principal{}, storage.Project{}, err
	}
	return auth, principal, project, nil
}

// authProjectForWrite is authProject plus the route's access-class decision,
// for the session-only families whose store methods prove membership and stop
// there. Admission stays authProject's, unchanged — including credential
// precedence, so a Bearer that is present and does not resolve is still the 401
// principalFromRequest answers it with rather than a fall-through to the cookie
// riding beside it. The decision is the registry's Allow, so a viewer is refused
// here for the reason /api/op already refuses it propose_test, not by a rule
// this file invented.
//
// authProject's own project cannot answer the decision: it comes from
// store.ProjectByID, which is role-blind, so the role comes from the membership
// row — the same lookup principalFromRequest's session branch makes.
func authProjectForWrite(c echo.Context, store *storage.Store, ops *opAdapter, req opcore.Requirement) (authContext, storage.Project, error) {
	auth, project, err := authProject(c, store)
	if err != nil {
		return authContext{}, storage.Project{}, err
	}
	principal, _, err := sessionPrincipal(c, store, auth.User.ID, project.ID)
	if err != nil {
		return authContext{}, storage.Project{}, err
	}
	if err := ops.authorizeAccess(principal, req); err != nil {
		return authContext{}, storage.Project{}, err
	}
	return auth, project, nil
}

// revisionFor resolves the expected revision a legacy mutation carries: the
// client's own value when it sent one, otherwise the row's current revision so
// pre-revision clients keep working. A missing row is not-found either way.
func revisionFor(c echo.Context, sent int64, current func(ctx context.Context) (int64, error)) (int64, error) {
	if sent > 0 {
		return sent, nil
	}
	rev, err := current(c.Request().Context())
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, echo.NewHTTPError(http.StatusNotFound, "not found")
		}
		return 0, err
	}
	return rev, nil
}
