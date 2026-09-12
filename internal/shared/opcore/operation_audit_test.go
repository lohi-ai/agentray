package opcore

import (
	"context"
	"errors"
	"testing"
)

type auditRecorder struct {
	operations []string
	principal  Principal
}

func (r *auditRecorder) RecordOperationAudit(_ context.Context, principal Principal, operation string) {
	r.principal = principal
	r.operations = append(r.operations, operation)
}

func TestOperationAuditsOnlySuccessfulNetworkMutations(t *testing.T) {
	principal := Principal{ProjectID: "project-1", Kind: CredManagement, CredentialID: "credential-1"}
	recorder := &auditRecorder{}
	mutate := Operation[struct{}, struct{}]{
		Name:   "mutate",
		Access: AccessDashboardsWrite,
		Handler: func(context.Context, CallContext, struct{}) (struct{}, error) {
			return struct{}{}, nil
		},
	}
	if _, err := mutate.OpInvoke(context.Background(), CallContext{Deps: recorder, Principal: principal}, `{}`); err != nil {
		t.Fatalf("mutating operation: %v", err)
	}
	if len(recorder.operations) != 1 || recorder.operations[0] != "mutate" {
		t.Fatalf("audit operations = %v", recorder.operations)
	}
	if recorder.principal.ProjectID != principal.ProjectID || recorder.principal.Kind != principal.Kind || recorder.principal.CredentialID != principal.CredentialID {
		t.Fatalf("audit principal = %+v, want %+v", recorder.principal, principal)
	}

	read := mutate
	read.Name = "read"
	read.Access = AccessAnalyticsRead
	if _, err := read.OpInvoke(context.Background(), CallContext{Deps: recorder, Principal: principal}, `{}`); err != nil {
		t.Fatalf("read operation: %v", err)
	}
	failed := mutate
	failed.Name = "failed"
	failed.Handler = func(context.Context, CallContext, struct{}) (struct{}, error) {
		return struct{}{}, errors.New("boom")
	}
	if _, err := failed.OpInvoke(context.Background(), CallContext{Deps: recorder, Principal: principal}, `{}`); err == nil {
		t.Fatal("failed operation unexpectedly succeeded")
	}
	if len(recorder.operations) != 1 {
		t.Fatalf("read or failed operation was audited: %v", recorder.operations)
	}
}
