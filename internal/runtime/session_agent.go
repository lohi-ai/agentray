package agentruntime

import (
	"context"
	"encoding/json"
)

// piSessionAgent is a temporary concrete binding of the session operations.
// Both implementations use the same journal/policy callbacks during migration;
// remove the worker binding when the remaining Go provider/trace wiring lands.
type piSessionAgent struct {
	Call              func(context.Context, string, json.RawMessage) (json.RawMessage, error)
	State             func(context.Context) (json.RawMessage, error)
	Prompt            func(context.Context, json.RawMessage) error
	Continue          func(context.Context) error
	Close             func() error
	UpstreamCommit    func() string
	ObserveDelegation func(context.Context, string, func(context.Context) (json.RawMessage, error)) (json.RawMessage, error)
}
