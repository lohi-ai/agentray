package agentruntime

import (
	"context"
	"errors"
	"testing"

	"github.com/2found/2ai/agentcore"
)

// TestLiveRegistrySteerRoundTrip verifies a registered run's steer queue receives
// a pushed message and the steering source drains exactly what arrived.
func TestLiveRegistrySteerRoundTrip(t *testing.T) {
	reg := NewLiveRegistry()
	lr := reg.register("sess-1", "proj-1", LiveAuthority{CanWrite: true}, nil)
	if lr == nil {
		t.Fatal("register returned nil for a non-empty session id")
	}

	if reg.Steer("proj-1", "sess-1", "use last 7 days", LiveAuthority{CanWrite: true}) != LiveControlDelivered {
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
	lr := reg.register("sess-1", "proj-1", LiveAuthority{CanWrite: true}, nil)
	if reg.FollowUp("proj-1", "sess-1", "now break it down by country", LiveAuthority{CanWrite: true}) != LiveControlDelivered {
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
	reg.register("sess-1", "proj-1", LiveAuthority{CanWrite: true}, nil)

	if reg.Steer("proj-2", "sess-1", "x", LiveAuthority{CanWrite: true}) != LiveControlNotFound {
		t.Fatal("Steer must return false when the project does not own the session")
	}
	if reg.Steer("proj-1", "missing", "x", LiveAuthority{CanWrite: true}) != LiveControlNotFound {
		t.Fatal("Steer must return false for an unknown session")
	}
}

// TestLiveRegistryUnregister verifies a run that exited is no longer steerable.
func TestLiveRegistryUnregister(t *testing.T) {
	reg := NewLiveRegistry()
	reg.register("sess-1", "proj-1", LiveAuthority{CanWrite: true}, nil)
	reg.unregister("sess-1")
	if reg.Steer("proj-1", "sess-1", "x", LiveAuthority{CanWrite: true}) != LiveControlNotFound {
		t.Fatal("Steer must return false after the run unregisters")
	}
}

// TestLiveRegistryEmptySessionIsNoLiveControl verifies an empty session id yields
// a nil handle whose drain sources are nil, so a plain run leaves the loop's
// defaults untouched.
func TestLiveRegistryEmptySessionIsNoLiveControl(t *testing.T) {
	reg := NewLiveRegistry()
	lr := reg.register("", "proj-1", LiveAuthority{}, nil)
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
	if reg.register("s", "p", LiveAuthority{}, nil) != nil {
		t.Fatal("nil registry register must return nil")
	}
	reg.unregister("s") // must not panic
	if reg.Steer("p", "s", "x", LiveAuthority{}) != LiveControlNotFound {
		t.Fatal("nil registry Steer must return false")
	}
	if reg.Cancel("p", "s") {
		t.Fatal("nil registry Cancel must return false")
	}
}

// TestLiveRegistryCancelStopsTheRun verifies Cancel unwinds the registered run's
// context with ErrRunStopped as the cause — the fact the run path keys on to
// persist a `stopped` terminal status instead of an `error` one.
func TestLiveRegistryCancelStopsTheRun(t *testing.T) {
	reg := NewLiveRegistry()
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	reg.register("sess-1", "proj-1", LiveAuthority{}, cancel)

	if !reg.Cancel("proj-1", "sess-1") {
		t.Fatal("Cancel returned false for a live, project-matched session")
	}
	select {
	case <-ctx.Done():
	default:
		t.Fatal("Cancel did not cancel the run context")
	}
	if !errors.Is(context.Cause(ctx), ErrRunStopped) {
		t.Fatalf("cause = %v, want ErrRunStopped", context.Cause(ctx))
	}
	// Idempotent: a double-click must not panic or change the cause.
	reg.Cancel("proj-1", "sess-1")
	if !errors.Is(context.Cause(ctx), ErrRunStopped) {
		t.Fatalf("cause after second Cancel = %v, want ErrRunStopped", context.Cause(ctx))
	}
}

// TestLiveRegistryCancelScoping verifies Cancel is presence- and ownership-checked
// exactly like Steer: another project can't stop a run, and a run that already
// exited reports nothing to stop.
func TestLiveRegistryCancelScoping(t *testing.T) {
	reg := NewLiveRegistry()
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	reg.register("sess-1", "proj-1", LiveAuthority{}, cancel)

	if reg.Cancel("proj-2", "sess-1") {
		t.Fatal("Cancel must return false when the project does not own the session")
	}
	if reg.Cancel("proj-1", "missing") {
		t.Fatal("Cancel must return false for an unknown session")
	}
	select {
	case <-ctx.Done():
		t.Fatal("a refused Cancel must not have cancelled the run")
	default:
	}

	reg.unregister("sess-1")
	if reg.Cancel("proj-1", "sess-1") {
		t.Fatal("Cancel must return false after the run unregisters")
	}
}

func TestLiveRegistryEnforcesCurrentRequestAuthority(t *testing.T) {
	reg := NewLiveRegistry()
	lr := reg.register("write-run", "project", LiveAuthority{CanWrite: true}, nil)
	deniedPersist := false
	if result := reg.Steer("project", "write-run", "restricted steer", LiveAuthority{}); result != LiveControlDenied {
		t.Fatalf("restricted steer result=%v, want denied", result)
	}
	if result := reg.FollowUp("project", "write-run", "restricted followup", LiveAuthority{}); result != LiveControlDenied {
		t.Fatalf("restricted follow-up result=%v, want denied", result)
	}
	if result, err := reg.QueueInput("project", "write-run", false, LiveAuthority{}, func() (agentcore.Message, error) {
		deniedPersist = true
		return agentcore.Message{Content: "restricted conversation input"}, nil
	}); err != nil || result != LiveControlDenied {
		t.Fatalf("restricted conversation input result=%v err=%v, want denied", result, err)
	}
	if deniedPersist || len(lr.steeringSource()(context.Background())) != 0 || len(lr.followUpSource()(context.Background())) != 0 {
		t.Fatal("denied control persisted or reached the live run")
	}

	for _, control := range []LiveControlResult{
		reg.Steer("project", "write-run", "owner steer", LiveAuthority{CanWrite: true}),
		reg.FollowUp("project", "write-run", "owner follow-up", LiveAuthority{CanWrite: true}),
	} {
		if control != LiveControlDelivered {
			t.Fatalf("authorized control result=%v, want delivered", control)
		}
	}
	if result, err := reg.QueueInput("project", "write-run", false, LiveAuthority{CanWrite: true}, func() (agentcore.Message, error) {
		return agentcore.Message{Role: agentcore.RoleUser, Content: "authorized conversation input"}, nil
	}); err != nil || result != LiveControlDelivered {
		t.Fatalf("authorized conversation input result=%v err=%v", result, err)
	}
	if got := lr.steeringSource()(context.Background()); len(got) != 2 {
		t.Fatalf("authorized steer queue=%d, want 2", len(got))
	}
	if got := lr.followUpSource()(context.Background()); len(got) != 1 {
		t.Fatalf("authorized follow-up queue=%d, want 1", len(got))
	}

	readRun := reg.register("read-run", "project", LiveAuthority{}, nil)
	if result := reg.Steer("project", "read-run", "safe read-only steer", LiveAuthority{}); result != LiveControlDelivered {
		t.Fatalf("read-only run control result=%v, want delivered", result)
	}
	if got := readRun.steeringSource()(context.Background()); len(got) != 1 {
		t.Fatal("read-only request could not steer a read-only run")
	}
}
