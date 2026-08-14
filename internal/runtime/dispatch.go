package agentruntime

import (
	"context"
	"fmt"
	"strings"
)

// DispatchRequest is the channel-agnostic unit of work the runtime accepts.
// Channels convert their native payload into this and call Dispatcher.
// PersonID is reserved for a future support channel that stitches identity;
// the runtime ignores it today.
type DispatchRequest struct {
	// Channel is a channels.Kind value (chat, mcp, schedule, webhook, lab).
	Channel   string
	ProjectID string
	AgentID   string
	PersonID  string
	Body      string
	Meta      map[string]string
}

// Dispatcher enqueues or runs one request. Scheduler implements it for the
// async path (schedule, webhook, and any future push channel). Chat and Lab
// call Runner directly because they need a streaming response.
type Dispatcher interface {
	Dispatch(ctx context.Context, req DispatchRequest) error
}

// Dispatch enqueues an autonomous run on the shared NATS subject. Trigger is
// the channel kind so traces and the Lab run tree show how the work arrived.
func (s *Scheduler) Dispatch(_ context.Context, req DispatchRequest) error {
	if strings.TrimSpace(req.ProjectID) == "" {
		return fmt.Errorf("runtime: dispatch requires project_id")
	}
	trigger := strings.TrimSpace(req.Channel)
	if trigger == "" {
		trigger = "manual"
	}
	return s.publishRun(runMessage{
		ProjectID: req.ProjectID,
		AgentID:   req.AgentID,
		Trigger:   trigger,
		Prompt:    req.Body,
	})
}
