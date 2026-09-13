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

// invoke runs one registered operation under the resolved principal. The
// caller decides admission first (legacy routes keep their own auth contract);
// invoke itself only scopes and executes, exactly like MountHTTP's handler
// after its Authorize step.
func (a *opAdapter) invoke(c echo.Context, principal opcore.Principal, opName string, input any) (json.RawMessage, error) {
	spec, ok := a.reg.Get(opName)
	if !ok {
		return nil, echo.NewHTTPError(http.StatusInternalServerError, "operation "+opName+" not registered")
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

// authorize mirrors MountHTTP's access-class check for adapters that adopt it.
func (a *opAdapter) authorize(principal opcore.Principal, opName string) error {
	if !a.reg.Authorize(principal, opName) {
		return echo.NewHTTPError(http.StatusForbidden, "credential may not invoke "+opName)
	}
	return nil
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
// always applied — while keeping the principal for the operation call.
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
	return principal, project, nil
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
