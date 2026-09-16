package storage

import (
	"context"
	"regexp"
	"strings"
)

// funnel_steps.go — the activation-funnel step heuristic, server side.
//
// This is a port of web/lib/ia.ts's FUNNEL_STAGES / stageOfName /
// matchedFunnelSteps / funnelStepNames, and it is the source of truth for a
// funnel board tile that declares no steps: the tile's steps are resolved at
// read time from the project's event catalog, so agents, MCP and the web board
// all see the same default. ia.ts keeps its own copy for weakestLink,
// retentionAnchorEvent and the first-run opinion until those move — the two
// must not drift, so any change here is a change there.
//
// The catalog arrives volume-descending, so the first event matching a stage
// is the busiest one — the same event the web heuristic picks.

// funnelStage is one recognised activation stage. match is the same
// case-insensitive pattern the web heuristic uses.
type funnelStage struct {
	id    string
	label string
	match *regexp.Regexp
}

var funnelStages = []funnelStage{
	{id: "visit", label: "visit", match: regexp.MustCompile(`(?i)page_?view|visit|session_start|app_open|^loaded$`)},
	{id: "signup", label: "signup", match: regexp.MustCompile(`(?i)sign[_\s-]?up|register|account_created|signed_up`)},
	{id: "activation", label: "activation", match: regexp.MustCompile(`(?i)activat|onboard|first_value|aha|setup_complete|completed_setup|first_(action|event|project)|value_realized`)},
	{id: "retention", label: "return", match: regexp.MustCompile(`(?i)return|repeat|day_?[27]|habit|engaged`)},
	{id: "revenue", label: "purchase", match: regexp.MustCompile(`(?i)purchase|paid|checkout|subscri|upgrade|payment|invoice|conversion`)},
}

// defaultFunnelSteps is the contract served when the catalog matches fewer
// than two stages — the same fallback the web heuristic returns.
var defaultFunnelSteps = []string{"user.pageview", "user.signup", "user.conversion"}

// funnelStageOfName returns the index of the stage an event name belongs to,
// or -1. An explicit activationEvent claims the activation stage by exact
// match and disables the regex for that stage — only the configured event
// qualifies. Otherwise the last matching stage wins, mirroring the web's
// `claimed` loop.
func funnelStageOfName(name, activationEvent string) int {
	act := strings.TrimSpace(activationEvent)
	if act != "" && name == act {
		for i, s := range funnelStages {
			if s.id == "activation" {
				return i
			}
		}
	}
	claimed := -1
	for order, stage := range funnelStages {
		if stage.id == "activation" && act != "" {
			continue
		}
		if stage.match.MatchString(name) {
			claimed = order
		}
	}
	return claimed
}

// funnelStepNames is the honest activation funnel: the busiest event of each
// matched stage in stage order, or the default contract when the catalog has
// fewer than two. onboarding_verified is a verification receipt, not a product
// signal — the web catalogEvents() excludes it for every consumer, so it is
// excluded here too.
func funnelStepNames(catalog []EventCatalogEntry, activationEvent string) []string {
	events := make([]EventCatalogEntry, 0, len(catalog))
	for _, e := range catalog {
		if e.EventName != "" && e.EventName != "onboarding_verified" {
			events = append(events, e)
		}
	}
	steps := make([]string, 0, len(funnelStages))
	for order := range funnelStages {
		for _, e := range events {
			if funnelStageOfName(e.EventName, activationEvent) == order {
				steps = append(steps, e.EventName)
				break
			}
		}
	}
	if len(steps) >= 2 {
		return steps
	}
	return append([]string{}, defaultFunnelSteps...)
}

// resolveFunnelSteps fills the steps of every funnel tile that declared none,
// from the project's event catalog and configured activation event. The
// resolved names are served, never stored — a stored copy would freeze the
// catalog the tile is supposed to track.
//
// A resolution failure is a warning, not a failed read: the board's metric
// and chart tiles do not depend on the event catalog, and the reader of the
// board is the one who needs to know a funnel tile could not derive its
// steps.
func (s *Store) resolveFunnelSteps(ctx context.Context, projectID string, content *BoardContent) {
	needs := false
	for _, section := range content.Definition.Sections {
		for _, tile := range section.Tiles {
			if tile.Kind == TileKindFunnel && tile.Steps == nil {
				needs = true
			}
		}
	}
	if !needs {
		return
	}
	project, err := s.ProjectByID(ctx, projectID)
	if err != nil {
		content.Warnings = append(content.Warnings, "funnel steps could not be derived: project lookup failed: "+err.Error())
		return
	}
	catalog, err := s.EventNames(ctx, projectID, 0)
	if err != nil {
		content.Warnings = append(content.Warnings, "funnel steps could not be derived: event catalog unavailable: "+err.Error())
		return
	}
	steps := funnelStepNames(catalog, project.ActivationEvent)
	for si := range content.Definition.Sections {
		for ti := range content.Definition.Sections[si].Tiles {
			tile := &content.Definition.Sections[si].Tiles[ti]
			if tile.Kind == TileKindFunnel && tile.Steps == nil {
				tile.Steps = steps
			}
		}
	}
}
