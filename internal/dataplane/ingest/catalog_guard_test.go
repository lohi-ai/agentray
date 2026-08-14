package ingestion

import (
	"context"
	"testing"
	"time"
)

type fakeCatalog struct {
	names []string
	calls int
}

func (f *fakeCatalog) EventNameSet(_ context.Context, _ string) (map[string]struct{}, error) {
	f.calls++
	out := make(map[string]struct{}, len(f.names))
	for _, n := range f.names {
		out[n] = struct{}{}
	}
	return out, nil
}

// A name outside an established catalog is unplanned; a known name is not.
func TestCatalogGuardFlagsUnknownNames(t *testing.T) {
	g := newCatalogGuard(&fakeCatalog{names: []string{"purchase", "signup"}}, time.Hour)
	ctx := context.Background()
	if g.isUnplanned(ctx, "p1", "purchase") {
		t.Fatal("known name must not be flagged")
	}
	if !g.isUnplanned(ctx, "p1", "puchase") { // typo
		t.Fatal("typo'd name must be flagged")
	}
}

// A brand-new project (empty catalog) flags nothing — it is still bootstrapping.
func TestCatalogGuardBootstrappingFlagsNothing(t *testing.T) {
	g := newCatalogGuard(&fakeCatalog{names: nil}, time.Hour)
	if g.isUnplanned(context.Background(), "p1", "anything") {
		t.Fatal("bootstrapping project must not flag")
	}
}

// Platform-defined names are never subject to the tracking plan.
func TestCatalogGuardIgnoresPlatformNames(t *testing.T) {
	g := newCatalogGuard(&fakeCatalog{names: []string{"purchase"}}, time.Hour)
	ctx := context.Background()
	for _, name := range []string{"$identify", "$pageview", "system.tracking.unplanned_event", "agent.turn"} {
		if g.isUnplanned(ctx, "p1", name) {
			t.Fatalf("%s must never be flagged", name)
		}
	}
}

// The catalog snapshot is cached within its TTL: repeated tagging does not hit
// the store per event.
func TestCatalogGuardCachesWithinTTL(t *testing.T) {
	f := &fakeCatalog{names: []string{"purchase"}}
	g := newCatalogGuard(f, time.Hour)
	ctx := context.Background()
	for i := 0; i < 10; i++ {
		g.isUnplanned(ctx, "p1", "purchase")
	}
	if f.calls != 1 {
		t.Fatalf("want 1 catalog load within TTL, got %d", f.calls)
	}
}

// A nil guard (handler built without WithCatalogGuard) tags nothing and never panics.
func TestNilCatalogGuardSafe(t *testing.T) {
	var g *catalogGuard
	if g.isUnplanned(context.Background(), "p1", "whatever") {
		t.Fatal("nil guard must not flag")
	}
}
