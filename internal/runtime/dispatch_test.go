package agentruntime

import "testing"

func TestDispatchRequestRejectsEmptyProject(t *testing.T) {
	s := &Scheduler{}
	err := s.Dispatch(t.Context(), DispatchRequest{Body: "hello"})
	if err == nil {
		t.Fatal("expected error for empty project_id")
	}
}
