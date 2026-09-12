package opcore

import (
	"errors"
	"io"
	"net/http"

	"github.com/labstack/echo/v4"
)

// MountHTTP registers every operation as POST <group>/<name>. The request body is
// the operation's input JSON; the response body is its output JSON. deps is the
// concrete dependency bundle (the same one the agent tools use) handed to every
// handler, so a web client and the agent run the identical usecase code.
//
// Every call resolves a Principal and is authorized against the operation's
// Access class before the handler runs — a credential that authenticates but
// lacks the class is refused, and a capture credential is refused outright.
func MountHTTP(g *echo.Group, r *Registry, deps any, resolve PrincipalResolver) {
	for _, s := range r.Specs() {
		spec := s // capture per iteration
		g.POST("/"+spec.OpName(), func(c echo.Context) error {
			principal, err := resolve(c)
			if err != nil {
				return err
			}
			if !r.Authorize(principal, spec.OpName()) {
				return echo.NewHTTPError(http.StatusForbidden, "credential may not invoke "+spec.OpName())
			}
			body, err := io.ReadAll(c.Request().Body)
			if err != nil {
				return echo.NewHTTPError(http.StatusBadRequest, "unreadable request body")
			}
			cc := CallContext{ProjectID: principal.ProjectID, Deps: deps, Principal: principal}
			out, err := spec.OpInvoke(c.Request().Context(), cc, string(body))
			if err != nil {
				err = r.classifyError(err)
				var oe *OpError
				if errors.As(err, &oe) {
					return c.JSON(statusForKind(oe.Kind), opErrorBody{Error: oe.Message, Code: string(oe.Kind)})
				}
				if he, ok := r.MapError(err).(*echo.HTTPError); ok {
					return he
				}
				return c.JSON(http.StatusBadRequest, opErrorBody{Error: err.Error()})
			}
			return c.Blob(http.StatusOK, echo.MIMEApplicationJSON, []byte(out))
		})
	}
}
