package dataplane

import (
	"github.com/lohi-ai/agentray/internal/dataplane/connector"
)

// Source is the plugin contract for an external data system. Re-exported so
// callers can depend on the dataplane API without reaching into connector.
type Source = connector.Source

// SourceKinds lists registered connector plugin names, sorted.
func SourceKinds() []string { return connector.Kinds() }
