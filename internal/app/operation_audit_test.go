package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestMutatingOperationsRecordCredentialOrSessionAttribution(t *testing.T) {
	s := openAppTestStore(t)
	ctx := context.Background()
	e := mountRealAdapters(t, s)
	boot, err := s.CreateAccount(ctx, fmt.Sprintf("operation-audit-%d@test.local", time.Now().UnixNano()), "P", "password-123", "ws", "proj")
	if err != nil {
		t.Fatalf("account: %v", err)
	}
	credential, secret, err := s.CreateProjectCredential(ctx, boot.User.ID, boot.Project.ID, "dashboard writer", []string{"dashboards:write"})
	if err != nil {
		t.Fatalf("management credential: %v", err)
	}

	rec := postJSON(t, e, "/api/op/create_dashboard", `{"name":"credential audit"}`, map[string]string{"Authorization": "Bearer " + secret})
	if rec.Code != http.StatusOK {
		t.Fatalf("management mutation: %d %s", rec.Code, rec.Body.String())
	}

	_, sessionToken, err := s.CreateUserSession(ctx, boot.User.ID, time.Hour)
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/op/create_dashboard", strings.NewReader(`{"name":"session audit"}`))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sessionToken})
	rec = httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("session mutation: %d %s", rec.Code, rec.Body.String())
	}

	logs, err := s.ListWorkspaceAuditLogs(ctx, boot.User.ID, boot.Project.WorkspaceID, 50)
	if err != nil {
		t.Fatalf("list audits: %v", err)
	}
	var credentialAudit, sessionAudit bool
	for _, log := range logs {
		if log.Action != "operation.create_dashboard" {
			continue
		}
		metadata := map[string]string{}
		if err := json.Unmarshal([]byte(log.Metadata), &metadata); err != nil {
			t.Fatalf("audit metadata %q: %v", log.Metadata, err)
		}
		if metadata["credential_id"] == credential.ID && metadata["credential_kind"] == "management" {
			credentialAudit = true
		}
		if log.ActorID == boot.User.ID && metadata["credential_kind"] == "session" {
			sessionAudit = true
		}
	}
	if !credentialAudit || !sessionAudit {
		t.Fatalf("missing operation attribution: credential=%t session=%t logs=%+v", credentialAudit, sessionAudit, logs)
	}
}
