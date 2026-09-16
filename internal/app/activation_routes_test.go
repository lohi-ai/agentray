package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// The activation-candidates convenience route is an adapter over the shared
// operation, so the test asserts the adapter contract, not the ranking: the
// session reaches the op's payload, a capture key is refused, and a management
// credential whose project is not the path's project gets not-found — never
// the other project's candidates.
func TestActivationCandidatesRouteAdapterContract(t *testing.T) {
	s := openAppTestStore(t)
	ctx := context.Background()
	e := mountRealAdapters(t, s)
	registerActivationRoutes(e, s, newOpAdapter(s, nil, storeRunner{s}))

	boot, err := s.CreateAccount(ctx, fmt.Sprintf("actcand-%d@test.local", time.Now().UnixNano()), "P", "password-123", "ws", "proj")
	if err != nil {
		t.Fatalf("account: %v", err)
	}
	other, err := s.CreateAccount(ctx, fmt.Sprintf("actcand-b-%d@test.local", time.Now().UnixNano()), "PB", "password-123", "ws-b", "proj-b")
	if err != nil {
		t.Fatalf("foreign account: %v", err)
	}
	_, sessionToken, err := s.CreateUserSession(ctx, boot.User.ID, time.Hour)
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	_, foreignSecret, err := s.CreateProjectCredential(ctx, other.User.ID, other.Project.ID, "reader", []string{"analytics:read"})
	if err != nil {
		t.Fatalf("foreign credential: %v", err)
	}

	// Session on its own project: the op answers (not_ready — no events).
	req := httptest.NewRequest(http.MethodGet, "/api/projects/"+boot.Project.ID+"/activation-candidates", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sessionToken})
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("session GET: %d %s", rec.Code, rec.Body.String())
	}
	var body struct {
		State      string `json:"state"`
		Candidates []any  `json:"candidates"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v; body=%s", err, rec.Body.String())
	}
	if body.State != "not_ready" {
		t.Fatalf("state = %q, want not_ready on an empty project", body.State)
	}

	// Capture key: refused before the op runs.
	req = httptest.NewRequest(http.MethodGet, "/api/projects/"+boot.Project.ID+"/activation-candidates", nil)
	req.Header.Set("X-API-Key", boot.Project.APIKey)
	rec = httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code == http.StatusOK {
		t.Fatalf("capture key reached activation-candidates: %s", rec.Body.String())
	}

	// A management credential for another project cannot read this project's
	// candidates through the path — the mismatch is a not-found.
	req = httptest.NewRequest(http.MethodGet, "/api/projects/"+boot.Project.ID+"/activation-candidates", nil)
	req.Header.Set("Authorization", "Bearer "+foreignSecret)
	rec = httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("foreign credential on path project = %d %s, want 404", rec.Code, rec.Body.String())
	}

	// The same operation answers identically over /api/op — one contract.
	opRec := postJSON(t, e, "/api/op/activation_candidates", `{}`, map[string]string{"Authorization": "Bearer " + foreignSecret})
	if opRec.Code != http.StatusOK {
		t.Fatalf("/api/op activation_candidates: %d %s", opRec.Code, opRec.Body.String())
	}
	var opBody struct {
		State string `json:"state"`
	}
	if err := json.Unmarshal(opRec.Body.Bytes(), &opBody); err != nil {
		t.Fatalf("decode op: %v; body=%s", err, opRec.Body.String())
	}
	if opBody.State != "not_ready" {
		t.Fatalf("op state = %q, want not_ready", opBody.State)
	}
}
