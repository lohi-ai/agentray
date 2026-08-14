package ingestion

import (
	"context"
	"strings"
	"sync"
	"time"
)

// catalogStore is the read side the guard needs: the project's *complete*
// established event-name catalog. Satisfied by *storage.Store (EventNameSet). The
// guard uses the uncapped set — not a top-N — so a low-volume-but-established name
// is never misflagged as unplanned just because it falls outside a ranked slice.
type catalogStore interface {
	EventNameSet(ctx context.Context, projectID string) (map[string]struct{}, error)
}

// catalogGuard tags events whose name is absent from a project's established
// event catalog — the tracking-plan signal (P4 / W9). It is deliberately
// advisory: it never rejects, only flags, so a typo'd or untracked event name
// surfaces in the tracking.unplanned_event digest instead of silently creating a
// junk series.
//
// It keeps a per-project snapshot of known names, refreshed on a TTL, so tagging
// costs no per-event database hit on the ingest hot path. A project whose catalog
// is still empty (brand-new, bootstrapping) flags nothing — otherwise its first
// legitimate events would all read as unplanned.
type catalogGuard struct {
	store catalogStore
	ttl   time.Duration

	mu    sync.Mutex
	cache map[string]*projectCatalog
}

type projectCatalog struct {
	names    map[string]struct{}
	loadedAt time.Time
	// bootstrapping is true when the catalog was empty at load time: the project
	// has no established plan yet, so nothing is flagged until it does.
	bootstrapping bool
}

func newCatalogGuard(store catalogStore, ttl time.Duration) *catalogGuard {
	if ttl <= 0 {
		ttl = 6 * time.Hour
	}
	return &catalogGuard{store: store, ttl: ttl, cache: map[string]*projectCatalog{}}
}

// isUnplanned reports whether eventName is unknown to projectID's established
// catalog. Platform-defined names ($identify, system.*, agent.*, any $-prefixed
// name) are never flagged — only product/user event names are subject to the
// tracking plan. Any load error fails open (returns false): the tracking signal
// must never block or mistag ingestion.
func (g *catalogGuard) isUnplanned(ctx context.Context, projectID, eventName string) bool {
	if g == nil || g.store == nil || eventName == "" || !isProductEvent(eventName) {
		return false
	}
	cat := g.catalogFor(ctx, projectID)
	if cat == nil || cat.bootstrapping {
		return false
	}
	_, known := cat.names[eventName]
	return !known
}

func (g *catalogGuard) catalogFor(ctx context.Context, projectID string) *projectCatalog {
	g.mu.Lock()
	cat := g.cache[projectID]
	fresh := cat != nil && time.Since(cat.loadedAt) < g.ttl
	g.mu.Unlock()
	if fresh {
		return cat
	}

	names, err := g.store.EventNameSet(ctx, projectID)
	if err != nil {
		// Fail open: keep any stale snapshot, else a nil that flags nothing.
		return cat
	}
	loaded := &projectCatalog{names: names, loadedAt: time.Now(), bootstrapping: len(names) == 0}

	g.mu.Lock()
	g.cache[projectID] = loaded
	g.mu.Unlock()
	return loaded
}

// isProductEvent excludes platform-defined names from tracking-plan enforcement.
func isProductEvent(name string) bool {
	if strings.HasPrefix(name, "$") ||
		strings.HasPrefix(name, "system.") ||
		strings.HasPrefix(name, "agent.") {
		return false
	}
	return true
}
