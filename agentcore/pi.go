package agentcore

import (
	"context"
	"encoding/json"
)

// PiCallback serves an upstream provider, tool, or optional hook. Params and
// results use Pi's native JSON vocabulary without Go Message conversions.
// For stream callbacks, emit accepts AssistantMessageEvent and the return value
// is the final AssistantMessage. For tools, emit accepts AgentToolResult updates.
// Native onPayload receives {payload, model}; return nil to preserve the payload.
// Callbacks may run concurrently and must honor ctx cancellation.
type PiCallback func(ctx context.Context, method string, params json.RawMessage, emit func(json.RawMessage) error) (json.RawMessage, error)

// PiConfig supplies native agent options and host callbacks.
type PiConfig struct {
	// Options carries native AgentOptions, callback names in a callbacks array,
	// and streamMode ("callback", the default, or "native" for Pi's provider APIs).
	Options  json.RawMessage
	Callback PiCallback
	// OnEvent is awaited by Pi before it proceeds. It may call State, Steer,
	// FollowUp or Abort, but must not wait for the run's own completion.
	OnEvent func(context.Context, json.RawMessage) error
	// OnTrace receives an original native request/response and settled Pi spans.
	// It is observational: panics are isolated and no result controls the loop.
	OnTrace func(context.Context, json.RawMessage)
}

// PiError is an error returned by the upstream runtime or a host callback.
type PiError struct {
	Name    string `json:"name"`
	Message string `json:"message"`
}

func (e *PiError) Error() string { return e.Name + ": " + e.Message }
