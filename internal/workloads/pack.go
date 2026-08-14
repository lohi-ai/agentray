package workloads

import (
	"fmt"
	"sort"
	"sync"
)

// Category groups packs in the marketplace.
type Category string

const (
	CategoryGrowth    Category = "growth"
	CategoryOperator  Category = "operator"
	CategorySupport   Category = "support"
	CategoryData      Category = "data"
	CategoryMarketing Category = "marketing"
)

// Skill is one starter skill shipped inside a pack. It maps onto an active
// AgentSkill at install time.
type Skill struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Body        string `json:"body"`
}

// Pack is a system-defined, installable agent blueprint. Category groups
// packs in the UI. SoulMD / AgentsMD are omitted from JSON (they are written
// into the agent definition at install, not shown in the catalog card).
type Pack struct {
	Slug        string          `json:"slug"`
	Name        string          `json:"name"`
	Tagline     string          `json:"tagline"`
	Description string          `json:"description"`
	Category    Category        `json:"category"`
	Icon        string          `json:"icon"`
	SoulMD      string          `json:"-"`
	AgentsMD    string          `json:"-"`
	Scopes      map[string]bool `json:"scopes"`
	Skills      []Skill         `json:"skills"`
}

// DefaultSlug is the pack seeded onto every new project.
const DefaultSlug = "growth-lead"

var (
	regMu sync.RWMutex
	reg   = map[string]Pack{}
	order []string
)

// Register adds a pack to the catalog. Duplicate slug panics.
func Register(p Pack) {
	if p.Slug == "" {
		panic("workloads: Register with empty slug")
	}
	if p.Category == "" {
		panic("workloads: pack " + p.Slug + " missing category")
	}
	regMu.Lock()
	defer regMu.Unlock()
	if _, dup := reg[p.Slug]; dup {
		panic("workloads: duplicate pack " + p.Slug)
	}
	reg[p.Slug] = p
	order = append(order, p.Slug)
}

// All returns registered packs in registration order.
func All() []Pack {
	regMu.RLock()
	defer regMu.RUnlock()
	out := make([]Pack, 0, len(order))
	for _, slug := range order {
		out = append(out, reg[slug])
	}
	return out
}

// BySlug looks up one pack.
func BySlug(slug string) (Pack, bool) {
	regMu.RLock()
	defer regMu.RUnlock()
	p, ok := reg[slug]
	return p, ok
}

// Default returns the pack seeded onto a new project.
func Default() (Pack, bool) { return BySlug(DefaultSlug) }

// ByCategory returns packs in one category, sorted by slug.
func ByCategory(c Category) []Pack {
	var out []Pack
	for _, p := range All() {
		if p.Category == c {
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Slug < out[j].Slug })
	return out
}

// MustBySlug is for seed paths that cannot proceed without a known pack.
func MustBySlug(slug string) Pack {
	p, ok := BySlug(slug)
	if !ok {
		panic(fmt.Sprintf("workloads: missing pack %q", slug))
	}
	return p
}
