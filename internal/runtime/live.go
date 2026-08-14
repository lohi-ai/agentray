package agentruntime

import (
	"context"
	"sync"

	"github.com/lohi-ai/agentray/agentcore"
)

// LiveRegistry tracks in-flight runs so a sibling request can drive one without
// starting a second run: mid-run steering (inject a correction honored on the
// next turn) and follow-up (continue the same bounded run after it answers). It
// is the consumer-side wiring for agentcore's steering/follow-up queues, which
// the loop drains but never sources.
//
// One process-wide instance is shared (built once in app wiring, threaded into
// the Runner via WithLiveRegistry and handed to the steer endpoints), exactly
// like the LabService's in-memory explain-session map: each HTTP request builds
// its own Runner, so the registry — not the Runner — must own the cross-request
// state. A run registers a session at start and unregisters on exit; a steer/
// follow-up request finds the session by id and pushes a message into it.
type LiveRegistry struct {
	mu       sync.Mutex
	sessions map[string]*liveRun
}

// liveRun is the live control surface of one in-flight run. projectID scopes
// steering to the run's own project so a member of another project can't drive
// it (mirroring explainSession). The channels are buffered so a push never blocks
// the requesting goroutine; the loop drains them at each turn boundary.
type liveRun struct {
	projectID string
	steer     chan agentcore.Message
	followup  chan agentcore.Message
}

// liveQueueDepth bounds how many un-drained steer/follow-up messages a session
// holds. Generous for human typing speed; a push past it is dropped rather than
// blocking the run (steering is best-effort by design — the loop guarantees only
// next-turn delivery, not durability).
const liveQueueDepth = 32

// NewLiveRegistry builds an empty registry. Safe for concurrent use.
func NewLiveRegistry() *LiveRegistry {
	return &LiveRegistry{sessions: map[string]*liveRun{}}
}

// register opens a live session for sessionID and returns its handle. A second
// register for the same id supersedes the first (one live run per session); the
// returned handle's drain closures are wired into the agentcore Config. Returns
// nil only when sessionID is empty, so callers can treat a nil registry/handle as
// "no live control" uniformly.
func (r *LiveRegistry) register(sessionID, projectID string) *liveRun {
	if r == nil || sessionID == "" {
		return nil
	}
	lr := &liveRun{
		projectID: projectID,
		steer:     make(chan agentcore.Message, liveQueueDepth),
		followup:  make(chan agentcore.Message, liveQueueDepth),
	}
	r.mu.Lock()
	r.sessions[sessionID] = lr
	r.mu.Unlock()
	return lr
}

// unregister closes out a session once its run exits. Idempotent.
func (r *LiveRegistry) unregister(sessionID string) {
	if r == nil || sessionID == "" {
		return
	}
	r.mu.Lock()
	delete(r.sessions, sessionID)
	r.mu.Unlock()
}

// lookup returns the live run for a session when one is in flight and its project
// matches, so steering is both presence- and ownership-checked.
func (r *LiveRegistry) lookup(projectID, sessionID string) (*liveRun, bool) {
	if r == nil || sessionID == "" {
		return nil, false
	}
	r.mu.Lock()
	lr, ok := r.sessions[sessionID]
	r.mu.Unlock()
	if !ok || lr.projectID != projectID {
		return nil, false
	}
	return lr, true
}

// Steer injects a user message into an in-flight run, to be threaded in before
// the model reasons on its next turn (agentcore's steering queue). Returns false
// when no run is live for the session (the caller then starts a normal turn) or
// the project doesn't match. A full queue drops the message rather than blocking.
func (r *LiveRegistry) Steer(projectID, sessionID, message string) bool {
	lr, ok := r.lookup(projectID, sessionID)
	if !ok {
		return false
	}
	select {
	case lr.steer <- agentcore.Message{Role: agentcore.RoleUser, Content: message}:
		return true
	default:
		return true // queue full: next-turn delivery isn't guaranteed, treat as accepted
	}
}

// FollowUp queues a message that continues the run after it produces its next
// final answer, instead of ending it (agentcore's follow-up queue). Returns false
// when no run is live for the session or the project doesn't match.
func (r *LiveRegistry) FollowUp(projectID, sessionID, message string) bool {
	lr, ok := r.lookup(projectID, sessionID)
	if !ok {
		return false
	}
	select {
	case lr.followup <- agentcore.Message{Role: agentcore.RoleUser, Content: message}:
		return true
	default:
		return true
	}
}

// steeringSource returns the agentcore callback that drains queued steer messages
// at a turn boundary. nil handle yields nil, so a run with no live session leaves
// Config.GetSteeringMessages unset (the production default).
func (lr *liveRun) steeringSource() func(context.Context) []agentcore.Message {
	if lr == nil {
		return nil
	}
	return func(context.Context) []agentcore.Message { return drainMessages(lr.steer) }
}

// followUpSource returns the agentcore callback that drains queued follow-up
// messages when the run would otherwise stop. nil handle yields nil.
func (lr *liveRun) followUpSource() func(context.Context) []agentcore.Message {
	if lr == nil {
		return nil
	}
	return func(context.Context) []agentcore.Message { return drainMessages(lr.followup) }
}

// drainMessages reads every currently-queued message without blocking, so a turn
// boundary picks up exactly what has arrived since the last drain.
func drainMessages(ch <-chan agentcore.Message) []agentcore.Message {
	var out []agentcore.Message
	for {
		select {
		case m := <-ch:
			out = append(out, m)
		default:
			return out
		}
	}
}
