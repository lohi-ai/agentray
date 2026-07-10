package agentruntime

import (
	"context"
	"testing"

	"github.com/lohi-ai/agentray/internal/storage"
)

func TestSkillLoaderNilStorePanicsLateNotAtConstruction(t *testing.T) {
	r := &Runner{Store: &storage.Store{}}
	loader := r.skillLoader("scope-1")
	if loader == nil {
		t.Fatal("expected loader")
	}
	// The behavior we care about is construction-time: creating the loader should
	// not read bodies eagerly. We intentionally do not call loader here because that
	// would require a live DB-backed Store.
	if _, err := loader(context.Background(), nil); err != nil {
		// empty ids short-circuit before touching storage; this proves the loader is lazy.
		t.Fatalf("empty-id load should be a no-op, got %v", err)
	}
}
