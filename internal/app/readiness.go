package app

import (
	"context"
	"net/http"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/lohi-ai/agentray/internal/dataplane/ingest"
)

// readinessProbe is the data-coherence signal behind /readyz. It answers one
// question: has this process applied everything that was accepted onto the
// durable stream? The blue-green deploy gate reads it through the container
// healthcheck, so a colour that is still replaying is never switched to.
//
// /healthz stays what it always was — a static 200 proving the process is up.
// The two are deliberately different questions; existing consumers of /healthz
// keep the answer they had.
type readinessProbe interface {
	ReplayStatus(ctx context.Context) (ingestion.ReplayVerdict, error)
}

// readyzTimeout bounds one probe. A hung broker must surface as "not ready"
// rather than holding the healthcheck open until its own timeout.
const readyzTimeout = 5 * time.Second

// readyzBody is the response of both outcomes. It EMBEDS ingestion.ReplayVerdict
// rather than re-declaring its fields: a diagnostic added to the verdict must
// reach /readyz by construction, because this is the surface an operator reads
// when the gate refuses and a field-by-field copy is exactly what goes stale.
// Zero values are omitted, so a caught-up colour answers
// {"ready":true,"pipeline":"jetstream"} and the sequence numbers appear exactly
// when they are the diagnosis.
type readyzBody struct {
	ingestion.ReplayVerdict
	Pipeline string `json:"pipeline"`
	Error    string `json:"error,omitempty"`
}

// readyzHandler answers 200 only for a colour that has applied the whole stream,
// and 503 otherwise — including when the answer cannot be read at all, because a
// colour whose coherence cannot be established must not take traffic.
func readyzHandler(probe readinessProbe) echo.HandlerFunc {
	return func(c echo.Context) error {
		if probe == nil {
			// No durable pipeline (INGEST_JETSTREAM=false): nothing replays, so
			// there is no lag to wait for. The pipeline name says which mode is
			// answering rather than pretending the check ran.
			return c.JSON(http.StatusOK, readyzBody{
				ReplayVerdict: ingestion.ReplayVerdict{Ready: true},
				Pipeline:      "core-nats",
			})
		}

		ctx, cancel := context.WithTimeout(c.Request().Context(), readyzTimeout)
		defer cancel()
		verdict, err := probe.ReplayStatus(ctx)
		if err != nil {
			return c.JSON(http.StatusServiceUnavailable, readyzBody{
				ReplayVerdict: ingestion.ReplayVerdict{Reason: "unavailable"},
				Pipeline:      "jetstream",
				Error:         err.Error(),
			})
		}

		body := readyzBody{ReplayVerdict: verdict, Pipeline: "jetstream"}
		if !verdict.Ready {
			return c.JSON(http.StatusServiceUnavailable, body)
		}
		return c.JSON(http.StatusOK, body)
	}
}
