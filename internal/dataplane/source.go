package dataplane

import (
	"context"

	"github.com/lohi-ai/agentray/internal/dataplane/connector"
)

// Source is the plugin contract for an external data system. Re-exported so
// callers can depend on the dataplane API without reaching into connector.
type Source = connector.Source

// OpenFunc constructs a Source from an operator-supplied DSN.
type OpenFunc = connector.OpenFunc

// RegisterSource adds a connector plugin under its kind name.
// Duplicate registration panics — it is always a programming error.
func RegisterSource(kind string, open OpenFunc) {
	connector.Register(kind, open)
}

// SourceKinds lists registered connector plugin names, sorted.
func SourceKinds() []string { return connector.Kinds() }

// OpenSource constructs a Source of the given kind from a DSN.
func OpenSource(ctx context.Context, kind, dsn string) (Source, error) {
	return connector.Open(ctx, kind, dsn)
}
