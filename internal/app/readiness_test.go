package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/lohi-ai/agentray/internal/dataplane/ingest"
)

// stubProbe is a readinessProbe whose answer the test chooses.
type stubProbe struct {
	verdict ingestion.ReplayVerdict
	err     error
}

func (s stubProbe) ReplayStatus(context.Context) (ingestion.ReplayVerdict, error) {
	return s.verdict, s.err
}

// /readyz is the deploy gate's input: 200 only for a colour that has applied
// everything it can receive, 503 (with the diagnosis) otherwise — including when
// the broker cannot be read, because a colour whose coherence is unknown must
// not take traffic. /healthz keeps its static-200 liveness answer alongside it.
func TestReadyz(t *testing.T) {
	tests := []struct {
		name       string
		probe      readinessProbe
		wantStatus int
		wantBody   map[string]any
	}{
		{
			name:       "no durable pipeline is coherent by construction",
			probe:      nil,
			wantStatus: http.StatusOK,
			wantBody:   map[string]any{"ready": true, "pipeline": "core-nats"},
		},
		{
			name:       "caught up",
			probe:      stubProbe{verdict: ingestion.ReplayVerdict{Ready: true, Reason: ingestion.ReplayCaughtUp, Applied: 12, Head: 12, First: 1}},
			wantStatus: http.StatusOK,
			wantBody:   map[string]any{"ready": true, "pipeline": "jetstream", "reason": ingestion.ReplayCaughtUp, "applied": 12.0, "head": 12.0, "first": 1.0},
		},
		{
			name:       "still replaying refuses the switch",
			probe:      stubProbe{verdict: ingestion.ReplayVerdict{Reason: ingestion.ReplayBehind, Applied: 4, Head: 30, First: 1, Pending: 26}},
			wantStatus: http.StatusServiceUnavailable,
			wantBody:   map[string]any{"ready": false, "reason": ingestion.ReplayBehind, "applied": 4.0, "head": 30.0, "pending": 26.0},
		},
		{
			name:       "purged gap is reported, never ready",
			probe:      stubProbe{verdict: ingestion.ReplayVerdict{Reason: ingestion.ReplayPurgedGap, Applied: 4, Head: 90, First: 50, Missing: 45, Pending: 41}},
			wantStatus: http.StatusServiceUnavailable,
			wantBody:   map[string]any{"ready": false, "reason": ingestion.ReplayPurgedGap, "missing": 45.0},
		},
		{
			name:       "broker unreadable fails closed",
			probe:      stubProbe{err: errors.New("consumer info: nats: timeout")},
			wantStatus: http.StatusServiceUnavailable,
			wantBody:   map[string]any{"ready": false, "reason": "unavailable", "error": "consumer info: nats: timeout"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := echo.New()
			e.GET("/readyz", readyzHandler(tt.probe))
			req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
			rec := httptest.NewRecorder()
			e.ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d (body %s)", rec.Code, tt.wantStatus, rec.Body.String())
			}
			var got map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatalf("decode body %q: %v", rec.Body.String(), err)
			}
			for key, want := range tt.wantBody {
				if got[key] != want {
					t.Fatalf("body[%q] = %v, want %v (body %s)", key, got[key], want, rec.Body.String())
				}
			}
		})
	}
}

// The compose healthcheck is `wget --spider`, which sends HEAD. A GET-only route
// answers that 405, so the probe exits non-zero for every colour and the
// blue-green gate can never see a healthy container — the deploy fails whether
// or not the colour is ready. Pinned through the REAL registration, because a
// test that mounts its own routes cannot catch this.
func TestHealthRoutesAnswerHead(t *testing.T) {
	tests := []struct {
		name   string
		ready  readinessProbe
		path   string
		status int
	}{
		{
			name:   "caught up: the probe that decides the switch is OK",
			ready:  stubProbe{verdict: ingestion.ReplayVerdict{Ready: true, Reason: ingestion.ReplayCaughtUp}},
			path:   "/readyz",
			status: http.StatusOK,
		},
		{
			name:   "behind: the probe refuses, and still answers",
			ready:  stubProbe{verdict: ingestion.ReplayVerdict{Reason: ingestion.ReplayBehind, Pending: 3}},
			path:   "/readyz",
			status: http.StatusServiceUnavailable,
		},
		{
			name:   "liveness is unaffected",
			ready:  stubProbe{verdict: ingestion.ReplayVerdict{Reason: ingestion.ReplayBehind}},
			path:   "/healthz",
			status: http.StatusOK,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := echo.New()
			registerHealthRoutes(e, tt.ready)
			req := httptest.NewRequest(http.MethodHead, tt.path, nil)
			rec := httptest.NewRecorder()
			e.ServeHTTP(rec, req)
			if rec.Code == http.StatusMethodNotAllowed {
				t.Fatalf("HEAD %s answered 405: `wget --spider` would fail for every colour", tt.path)
			}
			if rec.Code != tt.status {
				t.Fatalf("HEAD %s = %d, want %d", tt.path, rec.Code, tt.status)
			}
		})
	}
}
