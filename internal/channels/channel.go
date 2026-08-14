package channels

import (
	"fmt"
	"sort"
	"strings"
	"sync"
)

// Kind is a stable identifier for how work arrives.
type Kind string

const (
	// KindChat is the in-app streaming conversation.
	KindChat Kind = "chat"
	// KindMCP is the Model Context Protocol tool surface (POST /mcp).
	KindMCP Kind = "mcp"
	// KindSchedule is a cron trigger on an agent.
	KindSchedule Kind = "schedule"
	// KindWebhook is an authenticated inbound HTTP hook on an agent.
	KindWebhook Kind = "webhook"
	// KindLab is a manual / Lab run.
	KindLab Kind = "lab"
	// KindSupportWidget and KindVoice are reserved. Register a real adapter
	// before mounting ingress; do not treat reservation as a shipped channel.
	KindSupportWidget Kind = "support_widget"
	KindVoice         Kind = "voice"
)

// Mode says how the channel reaches the runtime.
type Mode string

const (
	// ModeSync streams a response on the same request (chat, lab).
	ModeSync Mode = "sync"
	// ModeAsync enqueues a run on the shared NATS subject (schedule, webhook).
	ModeAsync Mode = "async"
	// ModeTools projects opcore operations, not an agent run (mcp).
	ModeTools Mode = "tools"
)

// Info is the catalog entry for one channel. Adapters register one at boot.
type Info struct {
	Kind        Kind
	Description string
	Mode        Mode
	// Reserved marks a kind that exists so callers can plan against it, but
	// that has no adapter yet. Dispatchers must refuse reserved kinds.
	Reserved bool
}

// Envelope is the channel-agnostic payload. Every adapter converts into this
// before the composition root maps it onto runtime.DispatchRequest.
type Envelope struct {
	Kind      Kind
	ProjectID string
	AgentID   string
	// PersonID stitches a future support/voice turn onto the product identity
	// graph. Empty for operator/growth channels today.
	PersonID string
	Body     string
	Meta     map[string]string
}

var (
	regMu sync.RWMutex
	reg   = map[Kind]Info{}
)

// Register adds a channel to the catalog. Duplicate kind panics — it is
// always a programming error. Reserved kinds may be registered so the catalog
// lists them; they still cannot be dispatched.
func Register(info Info) {
	if info.Kind == "" {
		panic("channels: Register with empty Kind")
	}
	regMu.Lock()
	defer regMu.Unlock()
	if _, dup := reg[info.Kind]; dup {
		panic("channels: duplicate kind " + string(info.Kind))
	}
	reg[info.Kind] = info
}

// Lookup returns a registered channel.
func Lookup(k Kind) (Info, bool) {
	regMu.RLock()
	defer regMu.RUnlock()
	info, ok := reg[k]
	return info, ok
}

// All returns registered channels sorted by kind.
func All() []Info {
	regMu.RLock()
	defer regMu.RUnlock()
	out := make([]Info, 0, len(reg))
	for _, info := range reg {
		out = append(out, info)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Kind < out[j].Kind })
	return out
}

// Kinds returns registered kind names, sorted.
func Kinds() []string {
	all := All()
	out := make([]string, len(all))
	for i, info := range all {
		out[i] = string(info.Kind)
	}
	return out
}

// NewEnvelope builds an envelope after checking the kind is registered and
// not reserved. The composition root should call this instead of constructing
// Envelope by hand so a typo fails closed.
func NewEnvelope(kind Kind, projectID, agentID, body string) (Envelope, error) {
	info, ok := Lookup(kind)
	if !ok {
		return Envelope{}, fmt.Errorf("channels: unknown kind %q (registered: %s)", kind, strings.Join(Kinds(), ", "))
	}
	if info.Reserved {
		return Envelope{}, fmt.Errorf("channels: kind %q is reserved; register an adapter before using it", kind)
	}
	if strings.TrimSpace(projectID) == "" && kind != KindMCP {
		// MCP authenticates per request inside its own adapter; other kinds
		// need a project before they can run.
		return Envelope{}, fmt.Errorf("channels: %s envelope requires project_id", kind)
	}
	return Envelope{Kind: kind, ProjectID: projectID, AgentID: agentID, Body: body}, nil
}
