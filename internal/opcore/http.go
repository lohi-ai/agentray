package opcore

import (
	"io"
	"net/http"

	"github.com/labstack/echo/v4"
)

// ProjectResolver extracts the acting project id from an HTTP request. Auth —
// session cookie, API key, project membership — stays in the app layer; opcore
// only needs the resolved id to scope the CallContext.
type ProjectResolver func(c echo.Context) (projectID string, err error)

// MountHTTP registers every operation as POST <group>/<name>. The request body is
// the operation's input JSON; the response body is its output JSON. deps is the
// concrete dependency bundle (the same one the agent tools use) handed to every
// handler, so a web client and the agent run the identical usecase code.
func MountHTTP(g *echo.Group, r *Registry, deps any, resolve ProjectResolver) {
	for _, s := range r.Specs() {
		spec := s // capture per iteration
		g.POST("/"+spec.OpName(), func(c echo.Context) error {
			projectID, err := resolve(c)
			if err != nil {
				return err
			}
			body, err := io.ReadAll(c.Request().Body)
			if err != nil {
				return echo.NewHTTPError(http.StatusBadRequest, "unreadable request body")
			}
			cc := CallContext{ProjectID: projectID, Deps: deps}
			out, err := spec.OpInvoke(c.Request().Context(), cc, string(body))
			if err != nil {
				if he, ok := err.(*echo.HTTPError); ok {
					return he
				}
				return echo.NewHTTPError(http.StatusBadRequest, err.Error())
			}
			return c.Blob(http.StatusOK, echo.MIMEApplicationJSON, []byte(out))
		})
	}
}
