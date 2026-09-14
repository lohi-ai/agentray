package storage

import (
	"context"
	"encoding/json"
	"fmt"
)

// Default analysis boards. Each is a 007 declaration addressed by board_key,
// seeded onto every project so Acquisition / Monetization / Usage are real
// destinations rather than Coming soon affordances.
//
// Tile titles follow App Store Connect analytics labels (decision 2026-09-14).
// Values stay AgentRay's: the tile names a catalog metric the overview read
// already computes. Apple-shaped metrics with no AgentRay implementation are
// not declared here — the catalog refuses a metric nobody computes — and the
// analysis pages render those as named empty states instead of inventing a
// figure.
const (
	BoardKeyAcquisition  = "acquisition"
	BoardKeyMonetization = "monetization"
	BoardKeyUsage        = "usage"
)

// DefaultAnalysisBoard is one seeded analysis destination: the stable key the
// menu addresses it by, the name shown in list_dashboards, and the document
// get_board serves.
type DefaultAnalysisBoard struct {
	Key         string
	Name        string
	Description string
	Definition  BoardDefinition
}

func defaultTile(key, metric, display, title string, span int) BoardTile {
	return BoardTile{
		Key:     key,
		Kind:    TileKindMetric,
		Metric:  metric,
		Display: display,
		Title:   title,
		Span:    span,
	}
}

// DefaultAnalysisBoards is the declaration the seed writes. Order is the
// analysis menu order: Acquisition, Monetization, Usage.
func DefaultAnalysisBoards() []DefaultAnalysisBoard {
	return []DefaultAnalysisBoard{
		{
			Key:         BoardKeyAcquisition,
			Name:        "Acquisition",
			Description: "How people arrive. Labels follow App Store Connect; values are AgentRay first-observed people and attributed pageviews — not store downloads or impressions.",
			Definition: BoardDefinition{
				Version: BoardDefinitionVersion,
				Sections: []BoardSection{{
					Key:         "kpis",
					Title:       "Acquisition",
					Description: "First-time downloads is AgentRay new people (first observed qualifying activity). Ranked pages and sources stay visible, including Direct / unknown.",
					Tiles: []BoardTile{
						defaultTile("first-time-downloads", MetricNewUsers, DisplayStat, "First-time downloads", 1),
						defaultTile("top-pages", MetricTopPages, DisplayBar, "Top pages", 2),
						defaultTile("top-sources", MetricTopSources, DisplayBar, "Top acquisition sources", 2),
					},
				}},
			},
		},
		{
			Key:         BoardKeyMonetization,
			Name:        "Monetization",
			Description: "Proceeds is AgentRay net revenue from a trusted billing source. Paying users, in-app purchases and download→paid cohorts are Not available until those metrics exist.",
			Definition: BoardDefinition{
				Version: BoardDefinitionVersion,
				Sections: []BoardSection{{
					Key:         "kpis",
					Title:       "Monetization",
					Description: "Proceeds uses AgentRay's deduplicated net-money read. Unlike currencies are never summed.",
					Tiles: []BoardTile{
						defaultTile("proceeds", MetricRevenue, DisplayStat, "Proceeds", 1),
					},
				}},
			},
		},
		{
			Key:         BoardKeyUsage,
			Name:        "Usage",
			Description: "Active devices, sessions and retention from AgentRay qualifying activity. Average retention D14 and crashes are Not available — AgentRay serves D1/D7/D30 and has no crash contract.",
			Definition: BoardDefinition{
				Version: BoardDefinitionVersion,
				Sections: []BoardSection{
					{
						Key:         "kpis",
						Title:       "Usage",
						Description: "Active devices is AgentRay active people. Retention points are mature lifetime first-activity cohorts; immature cohorts render Not ready, never 0%.",
						Tiles: []BoardTile{
							defaultTile("active-devices", MetricActiveUsers, DisplayStat, "Active devices", 1),
							defaultTile("sessions", MetricSessions, DisplayStat, "Sessions", 1),
							defaultTile("retention-d1", MetricRetentionD1, DisplayStat, "Average retention D1", 1),
							defaultTile("retention-d7", MetricRetentionD7, DisplayStat, "Average retention D7", 1),
							defaultTile("retention-d30", MetricRetentionD30, DisplayStat, "Average retention D30", 1),
						},
					},
					{
						Key:         "trend",
						Title:       "Active devices per day",
						Description: "Daily distinct people. Daily counts are never summed into the period total.",
						Tiles: []BoardTile{
							defaultTile("active-devices-daily", MetricActiveUsersDaily, DisplayArea, "Active devices per day", 3),
						},
					},
				},
			},
		},
	}
}

// EnsureDefaultBoards writes the three analysis boards onto a project when
// their keys are absent. An existing board with the same key is left alone —
// a later declaration by the owner or an agent must not be overwritten by
// boot. The insert is set-level (one statement per key) so a busy test
// database cannot turn three declarations into a per-project timeout.
func (s *Store) EnsureDefaultBoards(ctx context.Context, projectID string) error {
	return seedDefaultBoards(ctx, s.pg, projectID)
}

// EnsureDefaultBoardsForAll seeds the analysis boards onto every project that
// does not already have them. Called at boot after the board columns exist, so
// projects created before this slice gain the destinations without a second
// signup.
func (s *Store) EnsureDefaultBoardsForAll(ctx context.Context) error {
	return seedDefaultBoards(ctx, s.pg, "")
}

func seedDefaultBoards(ctx context.Context, q pgQuerier, projectID string) error {
	for _, board := range DefaultAnalysisBoards() {
		normalized, err := normalizeBoardDefinition(board.Definition)
		if err != nil {
			return fmt.Errorf("seed %s board: %w", board.Key, err)
		}
		payload, err := json.Marshal(normalized)
		if err != nil {
			return fmt.Errorf("seed %s board: %w", board.Key, err)
		}
		if projectID != "" {
			_, err = q.Exec(ctx, `
INSERT INTO dashboards (project_id, name, description, board_key, definition, definition_updated_at)
SELECT p.id, $1::text, $2::text, $3::varchar, $4::jsonb, now()
FROM projects p
WHERE p.id = $5::uuid
  AND NOT EXISTS (
    SELECT 1 FROM dashboards d WHERE d.project_id = p.id AND d.board_key = $3::varchar
  )
ON CONFLICT (project_id, board_key) WHERE board_key <> '' DO NOTHING`, board.Name, board.Description, board.Key, payload, projectID)
		} else {
			_, err = q.Exec(ctx, `
INSERT INTO dashboards (project_id, name, description, board_key, definition, definition_updated_at)
SELECT p.id, $1::text, $2::text, $3::varchar, $4::jsonb, now()
FROM projects p
WHERE NOT EXISTS (
  SELECT 1 FROM dashboards d WHERE d.project_id = p.id AND d.board_key = $3::varchar
)
ON CONFLICT (project_id, board_key) WHERE board_key <> '' DO NOTHING`, board.Name, board.Description, board.Key, payload)
		}
		if err != nil {
			return fmt.Errorf("seed %s board: %w", board.Key, err)
		}
	}
	return nil
}

// DefaultBoardKeys is the set the analysis menu addresses, in menu order.
func DefaultBoardKeys() []string {
	boards := DefaultAnalysisBoards()
	keys := make([]string, len(boards))
	for i, board := range boards {
		keys[i] = board.Key
	}
	return keys
}
