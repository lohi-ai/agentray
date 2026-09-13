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

// readyzBody is the response of both outcomes. Zero values are omitted, so a
// caught-up colour answers {"ready":true,"pipeline":"jetstream"} and the
// sequence numbers appear exactly when they are the diagnosis.
type readyzBody struct {
	Ready      bool   `json:"ready"`
	Pipeline   string `json:"pipeline"`
	Reason     string `json:"reason,omitempty"`
	Applied    uint64 `json:"applied,omitempty"`
	Head       uint64 `json:"head,omitempty"`
	First      uint64 `json:"first,omitempty"`
	Missing    uint64 `json:"missing,omitempty"`
	Pending    uint64 `json:"pending,omitempty"`
	AckPending int    `json:"ack_pending,omitempty"`
	Error      string `json:"error,omitempty"`
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
			return c.JSON(http.StatusOK, readyzBody{Ready: true, Pipeline: "core-nats"})
		}

		ctx, cancel := context.WithTimeout(c.Request().Context(), readyzTimeout)
		defer cancel()
		verdict, err := probe.ReplayStatus(ctx)
		if err != nil {
			return c.JSON(http.StatusServiceUnavailable, readyzBody{
				Ready:    false,
				Pipeline: "jetstream",
				Reason:   "unavailable",
				Error:    err.Error(),
			})
		}

		body := readyzBody{
			Ready:      verdict.Ready,
			Pipeline:   "jetstream",
			Reason:     verdict.Reason,
			Applied:    verdict.Applied,
			Head:       verdict.Head,
			First:      verdict.First,
			Missing:    verdict.Missing,
			Pending:    verdict.Pending,
			AckPending: verdict.AckPending,
		}
		if !verdict.Ready {
			return c.JSON(http.StatusServiceUnavailable, body)
		}
		return c.JSON(http.StatusOK, body)
	}
}
