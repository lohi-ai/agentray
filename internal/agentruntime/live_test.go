package agentruntime

import (
	"context"
	"testing"

	"github.com/lohi-ai/agentray/agentcore"
)

// TestLiveRegistrySteerRoundTrip verifies a registered run's steer queue receives
// a pushed message and the steering source drains exactly what arrived.
func TestLiveRegistrySteerRoundTrip(t *testing.T) {
	reg := NewLiveRegistry()
	lr := reg.register("sess-1", "proj-1")
	if lr == nil {
		t.Fatal("register returned nil for a non-empty session id")
	}

	if !reg.Steer("proj-1", "sess-1", "use last 7 days") {
		t.Fatal("Steer returned false for a live, project-matched session")
	}
	got := lr.steeringSource()(context.Background())
	if len(got) != 1 || got[0].Role != agentcore.RoleUser || got[0].Content != "use last 7 days" {
		t.Fatalf("drained steer = %+v, want one user message", got)
	}
	// A second drain sees nothing — the queue was emptied.
	if rest := lr.steeringSource()(context.Background()); len(rest) != 0 {
		t.Fatalf("second drain = %+v, want empty", rest)
	}
}

// TestLiveRegistryFollowUpRoundTrip verifies the follow-up queue is independent
// of the steer queue.
func TestLiveRegistryFollowUpRoundTrip(t *testing.T) {
	reg := NewLiveRegistry()
	lr := reg.register("sess-1", "proj-1")
	if !reg.FollowUp("proj-1", "sess-1", "now break it down by country") {
		t.Fatal("FollowUp returned false for a live session")
	}
	if got := lr.steeringSource()(context.Background()); len(got) != 0 {
		t.Fatalf("follow-up leaked into the steer queue: %+v", got)
	}
	got := lr.followUpSource()(context.Background())
	if len(got) != 1 || got[0].Content != "now break it down by country" {
		t.Fatalf("drained follow-up = %+v, want one message", got)
	}
}

// TestLiveRegistryProjectScoping verifies a member of another project can't steer
// a run, and an unknown session is reported as not live.
func TestLiveRegistryProjectScoping(t *testing.T) {
	reg := NewLiveRegistry()
	reg.register("sess-1", "proj-1")

	if reg.Steer("proj-2", "sess-1", "x") {
		t.Fatal("Steer must return false when the project does not own the session")
	}
	if reg.Steer("proj-1", "missing", "x") {
		t.Fatal("Steer must return false for an unknown session")
	}
}

// TestLiveRegistryUnregister verifies a run that exited is no longer steerable.
func TestLiveRegistryUnregister(t *testing.T) {
	reg := NewLiveRegistry()
	reg.register("sess-1", "proj-1")
	reg.unregister("sess-1")
	if reg.Steer("proj-1", "sess-1", "x") {
		t.Fatal("Steer must return false after the run unregisters")
	}
}

// TestLiveRegistryEmptySessionIsNoLiveControl verifies an empty session id yields
// a nil handle whose drain sources are nil, so a plain run leaves the loop's
// defaults untouched.
func TestLiveRegistryEmptySessionIsNoLiveControl(t *testing.T) {
	reg := NewLiveRegistry()
	lr := reg.register("", "proj-1")
	if lr != nil {
		t.Fatalf("register(\"\") = %v, want nil", lr)
	}
	if lr.steeringSource() != nil || lr.followUpSource() != nil {
		t.Fatal("a nil handle must yield nil drain sources")
	}
}

// TestLiveRegistryNilSafe verifies a nil registry (live control disabled) is safe
// to call, mirroring how a Runner with no LiveRegistry behaves.
func TestLiveRegistryNilSafe(t *testing.T) {
	var reg *LiveRegistry
	if reg.register("s", "p") != nil {
		t.Fatal("nil registry register must return nil")
	}
	reg.unregister("s") // must not panic
	if reg.Steer("p", "s", "x") {
		t.Fatal("nil registry Steer must return false")
	}
}
